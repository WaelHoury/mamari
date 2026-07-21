package mamari

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

const (
	maxReviewChangedSymbols   = 40
	maxReviewCallersPerSymbol = 6
	maxReviewAlsoConsider     = 8
	maxReviewVisited          = 4000
	defaultReviewDepth        = 2
	// maxReviewWorkingSet bounds how many changed symbols get the expensive
	// per-symbol blast-radius/test-closure analysis. Far above any reviewable
	// PR, so real reviews are unaffected; only a pathological diff is bounded.
	maxReviewWorkingSet = 1500
)

// ReviewOptions configures Review.
type ReviewOptions struct {
	// Base is the git ref the working tree is diffed against. Empty defaults to
	// "HEAD" (review uncommitted changes). Pass a branch (e.g. "main") to review
	// a whole branch/PR against its base.
	Base string
	// Depth is how many caller hops to walk for blast radius (default 2).
	Depth int
	// Limit caps the number of changed symbols reported.
	Limit int
	// IncludeTests includes changed symbols that live in test files.
	IncludeTests bool
	// Callers includes the per-symbol proven/possible caller name lists. Off by
	// default: the rollup counts are the token-lean summary; the names are opt-in.
	Callers bool
	// CoveragePath points at an lcov report (e.g. coverage/lcov.info). When
	// set, the "untested" verdict is taken from what actually executed under
	// the test suite (authoritative) rather than static call-graph closure,
	// which overstates coverage when mocks or dead branches sever a path.
	CoveragePath string
}

// ReviewChangedSymbol is one symbol touched by the diff plus its blast radius,
// split by whether Mamari can stand behind the reachability.
type ReviewChangedSymbol struct {
	CGPSymbolSummary
	// ProvenCallers are exact/scoped-confidence callers reachable within Depth —
	// the blast radius Mamari proved. Populated only when ReviewOptions.Callers.
	ProvenCallers []CGPSymbolSummary `json:"provenCallers,omitempty"`
	// PossibleCallers are heuristic/unresolved-confidence callers — real
	// candidates to verify, never presented as certain. Opt-in like ProvenCallers.
	PossibleCallers []CGPSymbolSummary `json:"possibleCallers,omitempty"`
	ProvenCount     int                `json:"provenCount"`
	PossibleCount   int                `json:"possibleCount"`
	// ChangeKind classifies what the diff touched: "signature" (the
	// declaration line — params/name/return type, so callers may break) or
	// "body" (implementation only). Signature changes with callers are the
	// highest-value review signal, so they raise risk.
	ChangeKind string `json:"changeKind,omitempty"`
	// Untested is true when this symbol is not exercised by tests. With a
	// coverage report it means "did not execute under the suite" (authoritative);
	// otherwise it means "no resolved test-call path reaches it" (static).
	Untested bool `json:"untested,omitempty"`
	// UntestedBy records which signal set Untested: "coverage" (runtime) or
	// "static" (call-graph closure). Empty when the symbol is tested.
	UntestedBy  string   `json:"untestedBy,omitempty"`
	Risk        string   `json:"risk"` // high | medium | low
	RiskReasons []string `json:"riskReasons,omitempty"`
}

// ReviewResponse answers "what does this change affect, and how much of it can
// you actually stand behind?" for a git diff — the core PR-review flow.
type ReviewResponse struct {
	Status           string `json:"status"` // ok | no_changes | not_git | error
	Base             string `json:"base,omitempty"`
	ChangedFiles     int    `json:"changedFiles"`
	ChangedSymbols   int    `json:"changedSymbols"`
	Truncated        bool   `json:"truncated,omitempty"`
	ProvenAffected   int    `json:"provenAffected"`   // distinct exact/scoped callers of changed symbols
	PossibleAffected int    `json:"possibleAffected"` // distinct heuristic/unresolved callers to verify
	UntestedChanged  int    `json:"untestedChanged"`
	HighRisk         int    `json:"highRisk"`
	// CoverageApplied is true when a coverage report drove the untested
	// verdicts (making them runtime-authoritative rather than static).
	CoverageApplied bool                  `json:"coverageApplied,omitempty"`
	Symbols         []ReviewChangedSymbol `json:"symbols"`
	// AlsoConsider lists files historically changed together with the changed
	// files but absent from this diff — a "did you forget to update X?" signal.
	AlsoConsider []CoChangeEntry `json:"alsoConsider,omitempty"`
	Message      string          `json:"message,omitempty"`
	Warnings     []string        `json:"warnings,omitempty"`
}

type reviewLineRange struct{ start, end int }

// Review diffs the working tree against opts.Base, maps the changed lines to the
// symbols that own them, and for each reports its blast radius split into proven
// (exact/scoped) and possible (heuristic/unresolved) callers, whether a test
// reaches it, and a hot-path risk label. It never presents an unproven caller as
// certain — the whole point is a trustworthy "what does my change break?".
func Review(idx *Index, opts ReviewOptions) ReviewResponse {
	base := strings.TrimSpace(opts.Base)
	if base == "" {
		base = "HEAD"
	}
	root := idx.Repo.Root
	if root == "" {
		return ReviewResponse{Status: "not_git", Base: base, Symbols: []ReviewChangedSymbol{}, Message: "index has no repository root; re-index from inside a git working tree"}
	}
	changed, err := gitChangedRanges(root, base)
	if err != nil {
		return ReviewResponse{Status: "not_git", Base: base, Symbols: []ReviewChangedSymbol{}, Message: fmt.Sprintf("git diff against %q failed (not a git repo, or unknown ref): %v", base, err)}
	}
	// `git diff <base>` is blind to untracked files, but the review-before-push
	// workflow routinely includes brand-new files that were never `git add`ed —
	// and the index already has their symbols (indexing scans the filesystem,
	// not git). Fold every *indexed* untracked file in as fully-changed so its
	// symbols are reviewed; unindexed untracked paths (docs, junk) are ignored
	// and cannot flip a clean tree to "changed".
	for _, f := range gitUntrackedFiles(root) {
		if _, seen := changed[f]; seen {
			continue
		}
		if !idx.hasIndexedFile(f) {
			continue
		}
		changed[f] = []reviewLineRange{{start: 1, end: int(^uint32(0) >> 1)}}
	}
	if len(changed) == 0 {
		return ReviewResponse{Status: "no_changes", Base: base, Symbols: []ReviewChangedSymbol{}, Message: fmt.Sprintf("no changes vs %s", base)}
	}

	depth := opts.Depth
	if depth <= 0 {
		depth = defaultReviewDepth
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = maxReviewChangedSymbols
	}

	snap := idx.snapshot()
	reverse := buildReverseCallIndexSnapshot(snap)
	listOpts := ListSymbolsOptions{SourceOnly: true, IncludeTests: opts.IncludeTests}

	// Optional coverage report makes the untested verdict runtime-authoritative.
	var cov *coverageData
	var coverageWarnings []string
	if strings.TrimSpace(opts.CoveragePath) != "" {
		indexed := make(map[string]bool, len(snap.Symbols))
		for _, s := range snap.Symbols {
			if s.File != "" {
				indexed[s.File] = true
			}
		}
		if c, err := loadCoverage(opts.CoveragePath, root, indexed); err != nil {
			coverageWarnings = append(coverageWarnings, fmt.Sprintf("coverage report %q could not be read (%v); untested is static call-graph closure", opts.CoveragePath, err))
		} else if len(c.byFile) == 0 {
			coverageWarnings = append(coverageWarnings, fmt.Sprintf("coverage report %q matched no indexed files (path mismatch?); untested is static call-graph closure", opts.CoveragePath))
		} else {
			cov = c
		}
	}

	// Map changed line ranges -> the symbols that own them.
	var changedSyms []CGPSymbol
	changedFiles := map[string]bool{}
	for _, sym := range snap.Symbols {
		if sym.Kind == "file" {
			continue
		}
		ranges, ok := changed[sym.File]
		if !ok {
			continue
		}
		if !opts.IncludeTests && shouldExcludeNoisyFile(sym.File, listOpts) {
			continue
		}
		if reviewSymbolIntersects(sym, ranges) {
			changedSyms = append(changedSyms, sym)
			changedFiles[sym.File] = true
		}
	}

	// Classify each changed symbol as a signature change (its declaration
	// differs from the base — callers may break), a body-only change, or new.
	// This compares actual signature text against the base revision, so a
	// whole-file rewrite/add does not masquerade as an interface change.
	changeKinds := reviewClassifyChangeKinds(root, base, changedFiles, changedSyms)
	var classifyWarnings []string
	if len(changedFiles) > maxReviewClassifyFiles {
		classifyWarnings = append(classifyWarnings, fmt.Sprintf("%d changed files exceeds the change-classification cap (%d); changeKind is reported as \"body\" for all symbols — signature-change risk is not distinguished on a diff this large", len(changedFiles), maxReviewClassifyFiles))
	}

	// Prefer the innermost changed symbol: if a method changed, don't also
	// report its enclosing class as "changed".
	parentOfChanged := map[string]bool{}
	for _, s := range changedSyms {
		if s.ParentID != "" {
			parentOfChanged[s.ParentID] = true
		}
	}
	leaves := changedSyms[:0]
	for _, s := range changedSyms {
		if parentOfChanged[s.ID] {
			continue
		}
		leaves = append(leaves, s)
	}
	changedSyms = leaves

	// Bound worst-case work on a pathologically large diff: each changed
	// symbol drives a reverse-graph BFS (reviewCallers) and a test-closure
	// walk, so an enormous changed-set (thousands of symbols) is O(changed ×
	// graph). The cap is far above any reviewable PR, so realistic reviews are
	// unaffected (byte-identical); only a giant diff is bounded, and it is
	// bounded honestly — keep the highest-complexity/hot-path symbols (a cheap
	// proxy from the symbol's own fields), flag Truncated, and warn that
	// aggregates reflect the analyzed subset.
	changedSymbolTotal := len(changedSyms)
	workingSetCapped := false
	if len(changedSyms) > maxReviewWorkingSet {
		sort.SliceStable(changedSyms, func(i, j int) bool {
			ci := changedSyms[i].Complexity + changedSyms[i].TransitiveLoopDepth
			cj := changedSyms[j].Complexity + changedSyms[j].TransitiveLoopDepth
			if ci != cj {
				return ci > cj
			}
			if changedSyms[i].File != changedSyms[j].File {
				return changedSyms[i].File < changedSyms[j].File
			}
			return changedSyms[i].StartLine < changedSyms[j].StartLine
		})
		changedSyms = changedSyms[:maxReviewWorkingSet]
		workingSetCapped = true
	}

	provenSeen := map[string]bool{}
	possibleSeen := map[string]bool{}
	entries := make([]ReviewChangedSymbol, 0, len(changedSyms))
	for _, sym := range changedSyms {
		proven, possible := reviewCallers(snap, reverse, sym.ID, depth)
		untested := len(testCallersInReverseClosure(snap, reverse, sym.ID)) == 0
		untestedBy := ""
		if untested {
			untestedBy = "static"
		}
		// Coverage, when it can speak to this symbol, is authoritative: it
		// reflects what actually ran under the suite, overriding the static
		// closure in both directions (a statically-reachable symbol whose
		// lines never executed is untested; a symbol with no static test path
		// that a test still executed is tested).
		if cov != nil {
			if tested, known := cov.symbolTested(sym.File, sym.StartLine, sym.EndLine); known {
				untested = !tested
				if untested {
					untestedBy = "coverage"
				} else {
					untestedBy = ""
				}
			}
		}
		for _, c := range proven {
			provenSeen[c.ID] = true
		}
		for _, c := range possible {
			possibleSeen[c.ID] = true
		}
		changeKind := changeKinds[sym.ID]
		if changeKind == "" {
			changeKind = "body"
		}
		sc := changeKind == "signature"
		// The "may need updating" message must count DIRECT callers only: a
		// signature change forces edits at actual call sites, not in the
		// transitive closure. Quoting the depth-N blast radius here can
		// overstate the number of call sites that need edits. ProvenCount
		// stays the full-depth blast radius.
		directProven := 0
		if sc {
			direct, _ := reviewCallers(snap, reverse, sym.ID, 1)
			directProven = len(direct)
		}
		risk, reasons := reviewRisk(sym, len(proven), directProven, untested, sc)
		entry := ReviewChangedSymbol{
			CGPSymbolSummary: summarizeSymbol(sym),
			ProvenCount:      len(proven),
			PossibleCount:    len(possible),
			ChangeKind:       changeKind,
			Untested:         untested,
			UntestedBy:       untestedBy,
			Risk:             risk,
			RiskReasons:      reasons,
		}
		if opts.Callers {
			entry.ProvenCallers = reviewSummaries(proven, maxReviewCallersPerSymbol)
			entry.PossibleCallers = reviewSummaries(possible, maxReviewCallersPerSymbol)
		}
		entries = append(entries, entry)
	}

	sort.SliceStable(entries, func(i, j int) bool {
		if ri, rj := reviewRiskRank(entries[i].Risk), reviewRiskRank(entries[j].Risk); ri != rj {
			return ri > rj
		}
		if entries[i].ProvenCount != entries[j].ProvenCount {
			return entries[i].ProvenCount > entries[j].ProvenCount
		}
		if entries[i].File != entries[j].File {
			return entries[i].File < entries[j].File
		}
		return entries[i].StartLine < entries[j].StartLine
	})

	highRisk, untestedChanged := 0, 0
	for _, e := range entries {
		if e.Risk == "high" {
			highRisk++
		}
		if e.Untested {
			untestedChanged++
		}
	}
	truncated := workingSetCapped
	if len(entries) > limit {
		entries = entries[:limit]
		truncated = true
	}

	resp := ReviewResponse{
		Status:           "ok",
		Base:             base,
		ChangedFiles:     len(changed),
		ChangedSymbols:   changedSymbolTotal,
		Truncated:        truncated,
		ProvenAffected:   len(provenSeen),
		PossibleAffected: len(possibleSeen),
		UntestedChanged:  untestedChanged,
		HighRisk:         highRisk,
		CoverageApplied:  cov != nil,
		Symbols:          entries,
		AlsoConsider:     reviewAlsoConsider(idx, changed),
	}
	resp.Warnings = append(resp.Warnings, coverageWarnings...)
	resp.Warnings = append(resp.Warnings, classifyWarnings...)
	if workingSetCapped {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf("%d changed symbols exceeds the analysis budget (%d); blast-radius/untested/risk were computed for the %d highest-complexity symbols only — aggregate counts reflect that subset", changedSymbolTotal, maxReviewWorkingSet, maxReviewWorkingSet))
	}
	if resp.PossibleAffected > 0 {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf("%d possibly-affected caller(s) reached only through unresolved/heuristic edges — verify before trusting", resp.PossibleAffected))
	}
	return resp
}

// reviewCallers walks the reverse call graph up to depth hops and classifies
// each reached caller by the strongest path confidence: proven = the whole path
// is exact/scoped; possible = at least one hop is heuristic/unresolved.
func reviewCallers(snap indexSnapshot, reverse map[string][]CGPEdge, startID string, depth int) (proven, possible []CGPSymbol) {
	best := map[string]string{startID: ConfExact}
	frontier := []string{startID}
	for d := 0; d < depth && len(frontier) > 0 && len(best) < maxReviewVisited; d++ {
		var next []string
		for _, id := range frontier {
			parentConf := best[id]
			for _, e := range reverse[id] {
				if e.From == id || e.From == startID {
					continue
				}
				caller, ok := snap.Symbols[e.From]
				if !ok || caller.Kind == "file" {
					continue
				}
				pathConf, _ := weakerConfidence(parentConf, "", e.Confidence, e.UnresolvedReason)
				if prev, seen := best[e.From]; !seen || confidenceImproves(prev, pathConf) {
					best[e.From] = pathConf
					next = append(next, e.From)
				}
			}
		}
		frontier = next
	}
	for id, conf := range best {
		if id == startID {
			continue
		}
		sym, ok := snap.Symbols[id]
		if !ok {
			continue
		}
		if confidenceRank(conf) <= confidenceRank(ConfScoped) {
			proven = append(proven, sym)
		} else {
			possible = append(possible, sym)
		}
	}
	return proven, possible
}

func reviewSummaries(syms []CGPSymbol, limit int) []CGPSymbolSummary {
	sort.SliceStable(syms, func(i, j int) bool {
		if syms[i].File != syms[j].File {
			return syms[i].File < syms[j].File
		}
		return syms[i].StartLine < syms[j].StartLine
	})
	if len(syms) > limit {
		syms = syms[:limit]
	}
	out := make([]CGPSymbolSummary, 0, len(syms))
	for _, s := range syms {
		out = append(out, summarizeSymbolCandidate(s))
	}
	return out
}

func reviewRisk(sym CGPSymbol, provenCount, directProvenCount int, untested, signatureChanged bool) (string, []string) {
	var reasons []string
	score := 0
	// A changed signature can break every caller — the single most important
	// "look here" signal in a review — so it dominates when callers exist.
	// The count quoted is direct call sites (the ones that must pass the new
	// arguments), not the transitive blast radius.
	if signatureChanged && directProvenCount > 0 {
		reasons = append(reasons, fmt.Sprintf("signature changed — %d direct caller(s) may need updating", directProvenCount))
		score += 2
	}
	if sym.TransitiveLoopDepth >= 3 {
		reasons = append(reasons, fmt.Sprintf("hot-path (transitive loop depth %d)", sym.TransitiveLoopDepth))
		score += 2
	}
	if sym.LinearScanInLoop > 0 {
		reasons = append(reasons, "scan inside loop (possible O(n^2))")
		score += 2
	}
	switch {
	case sym.Complexity >= 15:
		reasons = append(reasons, fmt.Sprintf("high complexity (%d)", sym.Complexity))
		score += 2
	case sym.Complexity >= 8:
		reasons = append(reasons, fmt.Sprintf("complexity %d", sym.Complexity))
		score++
	}
	if untested {
		reasons = append(reasons, "no test reaches it")
		score++
	}
	switch {
	case provenCount >= 5:
		reasons = append(reasons, fmt.Sprintf("%d callers affected", provenCount))
		score += 2
	case provenCount >= 1:
		reasons = append(reasons, fmt.Sprintf("%d caller(s) affected", provenCount))
		score++
	}
	switch {
	case score >= 4:
		return "high", reasons
	case score >= 2:
		return "medium", reasons
	default:
		return "low", reasons
	}
}

func reviewRiskRank(r string) int {
	switch r {
	case "high":
		return 3
	case "medium":
		return 2
	default:
		return 1
	}
}

// reviewAlsoConsider aggregates the co-change partners of every changed file,
// drops files already in the diff, and returns the top few "you usually touch
// these together" hints.
func reviewAlsoConsider(idx *Index, changed map[string][]reviewLineRange) []CoChangeEntry {
	agg := map[string]int{}
	for file := range changed {
		for _, e := range idx.CoChangedFiles(file, maxReviewAlsoConsider) {
			if _, inDiff := changed[e.File]; inDiff {
				continue
			}
			agg[e.File] += e.Count
		}
	}
	if len(agg) == 0 {
		return nil
	}
	out := make([]CoChangeEntry, 0, len(agg))
	for f, c := range agg {
		out = append(out, CoChangeEntry{File: f, Count: c})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].File < out[j].File
	})
	if len(out) > maxReviewAlsoConsider {
		out = out[:maxReviewAlsoConsider]
	}
	return out
}

func reviewSymbolIntersects(sym CGPSymbol, ranges []reviewLineRange) bool {
	end := sym.EndLine
	if end < sym.StartLine {
		end = sym.StartLine
	}
	for _, r := range ranges {
		if sym.StartLine <= r.end && end >= r.start {
			return true
		}
	}
	return false
}

// maxReviewClassifyFiles caps how many changed files we fetch from the base
// revision to classify signature changes; beyond this the extra git calls
// aren't worth it and symbols default to "body".
const maxReviewClassifyFiles = 400

// reviewClassifyChangeKinds labels each changed symbol "signature" (its
// declaration differs from the base revision, so callers may break), "new"
// (the symbol/file did not exist at base), or "body" (declaration intact —
// implementation-only change). It compares the symbol's current declaration
// line(s) against the base revision of the file, so a whole-file rewrite or a
// brand-new file does not masquerade as an interface change (the flaw of a
// pure line-intersection heuristic). Files are fetched from the base once
// each; symbols default to "body" when the base cannot be read.
func reviewClassifyChangeKinds(root, base string, changedFiles map[string]bool, syms []CGPSymbol) map[string]string {
	kinds := map[string]string{}
	if len(changedFiles) == 0 || len(changedFiles) > maxReviewClassifyFiles {
		return kinds
	}
	type baseInfo struct {
		lines map[string]bool // trimmed non-empty lines present in the base file
		raw   string          // full base content, for a name-existence check
		isNew bool            // file absent at base
		cur   []string        // current working-tree lines
	}
	fileInfo := map[string]*baseInfo{}
	for f := range changedFiles {
		bi := &baseInfo{}
		out, err := exec.Command("git", "-C", root, "show", base+":"+f).Output()
		if err != nil {
			bi.isNew = true
		} else {
			bi.raw = string(out)
			bi.lines = map[string]bool{}
			for _, l := range strings.Split(bi.raw, "\n") {
				if t := normalizeDeclLine(l); t != "" {
					bi.lines[t] = true
				}
			}
		}
		if b, err := readRepoFile(root, f); err == nil {
			bi.cur = strings.Split(string(b), "\n")
		}
		fileInfo[f] = bi
	}

	for _, s := range syms {
		bi := fileInfo[s.File]
		if bi == nil || bi.isNew {
			kinds[s.ID] = "new"
			continue
		}
		// Compare the symbol's current declaration line(s) — StartLine across
		// the span its Signature occupies — against the base file's lines.
		span := strings.Count(s.Signature, "\n")
		allPresent, sawLine := true, false
		for ln := s.StartLine; ln <= s.StartLine+span && ln-1 < len(bi.cur) && ln >= 1; ln++ {
			t := normalizeDeclLine(bi.cur[ln-1])
			if t == "" {
				continue
			}
			sawLine = true
			if !bi.lines[t] {
				allPresent = false
				break
			}
		}
		if !sawLine {
			// Fall back to the recorded signature's first line.
			t := normalizeDeclLine(firstLineOf(s.Signature))
			if t != "" {
				sawLine = true
				allPresent = bi.lines[t]
			}
		}
		switch {
		case sawLine && allPresent:
			kinds[s.ID] = "body" // declaration intact — implementation change
		case reviewNameInBase(bi.raw, s.Name):
			kinds[s.ID] = "signature" // existed at base, declaration differs
		default:
			kinds[s.ID] = "new"
		}
	}
	return kinds
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// normalizeDeclLine trims a source line and collapses formatting-only
// whitespace so a declaration reformat does not read as a signature change:
// whitespace survives (as a single space) only between two identifier
// characters — `a b int` keeps its separator, while `foo( a,  b )` and
// `foo(a,b)`, or `key :` and `key:`, normalize identically. A pure
// re-indent/alignment change then classifies as "body", not "signature".
func normalizeDeclLine(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	isWord := func(r rune) bool {
		return r == '_' || r == '$' || unicode.IsLetter(r) || unicode.IsDigit(r)
	}
	runes := []rune(line)
	var b strings.Builder
	b.Grow(len(line))
	for i := 0; i < len(runes); i++ {
		if !unicode.IsSpace(runes[i]) {
			b.WriteRune(runes[i])
			continue
		}
		j := i
		for j < len(runes) && unicode.IsSpace(runes[j]) {
			j++
		}
		// TrimSpace guarantees a non-space rune exists on both sides.
		if isWord(runes[i-1]) && isWord(runes[j]) {
			b.WriteRune(' ')
		}
		i = j - 1
	}
	return b.String()
}

// reviewNameInBase reports whether name appears as a whole identifier in the
// base file content — a cheap "did this symbol exist before" check.
func reviewNameInBase(base, name string) bool {
	if name == "" {
		return false
	}
	off := 0
	for {
		i := strings.Index(base[off:], name)
		if i < 0 {
			return false
		}
		i += off
		before := byte(' ')
		if i > 0 {
			before = base[i-1]
		}
		after := byte(' ')
		if i+len(name) < len(base) {
			after = base[i+len(name)]
		}
		if !isIdentPart(before) && !isIdentPart(after) {
			return true
		}
		off = i + 1
	}
}

// gitChangedRanges runs `git diff <base> --unified=0` and returns, per current
// (post-image) file, the line ranges the diff added or modified. Deleted files
// are excluded (they have no current symbols to map to).
// hasIndexedFile reports whether rel (slash-separated, repo-relative) is a
// file this index scanned — a mutex-guarded point read.
func (idx *Index) hasIndexedFile(rel string) bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	_, ok := idx.Files[rel]
	return ok
}

// gitUntrackedFiles lists working-tree files git does not track (respecting
// .gitignore). Errors degrade to an empty list: the caller's diff already
// succeeded, so a failing ls-files must not fail the review.
func gitUntrackedFiles(root string) []string {
	out, err := exec.Command("git", "-C", root, "ls-files", "--others", "--exclude-standard").Output()
	if err != nil {
		return nil
	}
	var files []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			files = append(files, filepath.ToSlash(l))
		}
	}
	return files
}

func gitChangedRanges(root, base string) (map[string][]reviewLineRange, error) {
	cmd := exec.Command("git", "-C", root, "diff", base, "--unified=0", "--no-color", "--diff-filter=ACMR")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	changed := map[string][]reviewLineRange{}
	var cur string
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1024*1024), 32*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "+++ "):
			p := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
			p = strings.TrimPrefix(p, "b/")
			if p == "/dev/null" {
				cur = ""
			} else {
				cur = p
			}
		case strings.HasPrefix(line, "@@") && cur != "":
			if r, ok := parseHunkNewRange(line); ok {
				changed[cur] = append(changed[cur], r)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return changed, nil
}

// parseHunkNewRange extracts the post-image range from a hunk header like
// "@@ -12,3 +14,6 @@". A count of 0 (pure deletion) yields no current-line range.
func parseHunkNewRange(hunk string) (reviewLineRange, bool) {
	for _, f := range strings.Fields(hunk) {
		if !strings.HasPrefix(f, "+") {
			continue
		}
		spec := strings.TrimPrefix(f, "+")
		parts := strings.SplitN(spec, ",", 2)
		start, err := strconv.Atoi(parts[0])
		if err != nil {
			return reviewLineRange{}, false
		}
		count := 1
		if len(parts) == 2 {
			if count, err = strconv.Atoi(parts[1]); err != nil {
				return reviewLineRange{}, false
			}
		}
		if count <= 0 {
			return reviewLineRange{}, false
		}
		return reviewLineRange{start: start, end: start + count - 1}, true
	}
	return reviewLineRange{}, false
}

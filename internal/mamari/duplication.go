package mamari

import (
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
)

const (
	// A body must have at least this many normalized lines and characters to
	// be fingerprinted, so trivial getters/one-liners don't register as
	// clones. Normalization collapses each identifier/literal to a single
	// char, so the char floor is small by construction — the line floor is
	// the primary "is this a substantial body" gate.
	minShapeLines  = 6
	minShapeChars  = 40
	maxDupClusters = 200
)

// commonKeywords are control-flow/declaration keywords shared across the
// languages mamari indexes. They are preserved verbatim during shape
// normalization (instead of collapsing to the generic identifier token) so a
// fingerprint reflects real control-flow structure — an `if`-heavy function
// and a function-call-heavy function of the same length don't collide.
var commonKeywords = map[string]bool{
	"if": true, "else": true, "elif": true, "for": true, "while": true,
	"do": true, "switch": true, "case": true, "default": true, "break": true,
	"continue": true, "return": true, "yield": true, "await": true,
	"async": true, "function": true, "func": true, "def": true, "fn": true,
	"class": true, "struct": true, "interface": true, "enum": true,
	"try": true, "catch": true, "except": true, "finally": true, "throw": true,
	"raise": true, "const": true, "let": true, "var": true, "new": true,
	"import": true, "from": true, "export": true, "with": true, "in": true,
	"of": true, "match": true, "when": true, "map": true, "filter": true,
	"reduce": true, "forEach": true,
}

// shapeHashForRange builds a structural fingerprint for the symbol spanning
// [start,end] of maskedLines (source with strings/comments already masked).
// Returns "" when the body is too small to be a meaningful clone.
func shapeHashForRange(maskedLines []string, start, end int) string {
	if start < 1 {
		start = 1
	}
	if end > len(maskedLines) {
		end = len(maskedLines)
	}
	if end < start {
		return ""
	}
	var norm []string
	total := 0
	for i := start - 1; i < end; i++ {
		line := normalizeShapeLine(maskedLines[i])
		if line == "" {
			continue
		}
		norm = append(norm, line)
		total += len(line)
	}
	if len(norm) < minShapeLines || total < minShapeChars {
		return ""
	}
	h := fnv.New64a()
	for _, l := range norm {
		h.Write([]byte(l))
		h.Write([]byte{'\n'})
	}
	// Prefix with normalized line count so different-sized bodies never share
	// a hash even on a (astronomically unlikely) fnv collision.
	return strconv.Itoa(len(norm)) + ":" + strconv.FormatUint(h.Sum64(), 16)
}

// normalizeShapeLine collapses a source line to its structural skeleton:
// whitespace dropped, identifier runs → "I" (control keywords kept verbatim),
// number runs → "N". Masked strings/comments are already blanks, so string and
// comment content contributes nothing. Punctuation and operators are kept, so
// control-flow and call structure survive while names and literals do not.
func normalizeShapeLine(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case isIdentStart(c):
			j := i + 1
			for j < len(s) && isIdentPart(s[j]) {
				j++
			}
			word := s[i:j]
			if commonKeywords[word] {
				b.WriteString(word)
			} else {
				b.WriteByte('I')
			}
			i = j
		case c >= '0' && c <= '9':
			j := i + 1
			for j < len(s) && (isDigit(s[j]) || s[j] == '.' || s[j] == 'x' ||
				(s[j] >= 'a' && s[j] <= 'f') || (s[j] >= 'A' && s[j] <= 'F')) {
				j++
			}
			b.WriteByte('N')
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// annotateShapeHashLines computes and stores structural fingerprints for the
// function-like symbols in a file from shared, masked source lines.
func annotateShapeHashLines(idx *Index, file string, lines []string) {
	idx.ensureFileSymbolIndex()
	idx.mu.Lock()
	syms := append([]CGPSymbol(nil), idx.symbolsByFile[file]...)
	idx.mu.Unlock()
	if len(syms) == 0 {
		return
	}
	for _, sym := range syms {
		if !isComplexityKind(sym.Kind) {
			continue
		}
		hash := shapeHashForRange(lines, sym.StartLine, sym.EndLine)
		if hash == "" {
			continue
		}
		idx.mu.Lock()
		if existing, ok := idx.Symbols[sym.ID]; ok {
			existing.ShapeHash = hash
			idx.Symbols[sym.ID] = existing
		}
		idx.mu.Unlock()
	}
}

// DuplicationOptions configures Duplication.
type DuplicationOptions struct {
	// Limit caps the number of clone clusters reported (0 = default).
	Limit int
	// IncludeTests includes clones among test files (off by default — test
	// boilerplate is legitimately repetitive and rarely worth refactoring).
	IncludeTests bool
}

// DuplicationCluster is a set of symbols that share a structural fingerprint —
// the same code shape with names/literals changed.
type DuplicationCluster struct {
	Lines   int                `json:"lines"` // normalized body size (proxy for how much is duplicated)
	Count   int                `json:"count"`
	Members []CGPSymbolSummary `json:"members"`
}

// DuplicationResponse reports structural clone clusters, most-duplicated first.
type DuplicationResponse struct {
	Status        string               `json:"status"`
	TotalClusters int                  `json:"totalClusters"`
	Truncated     bool                 `json:"truncated,omitempty"`
	Clusters      []DuplicationCluster `json:"clusters"`
	Message       string               `json:"message,omitempty"`
}

// Duplication groups function/method symbols by their structural fingerprint
// and reports clusters of two or more — Type-2 clones (same structure, renamed
// identifiers/literals). It answers the "is this new code duplicating
// something we already have?" reusability question the review flow otherwise
// can only guess at. Fingerprints are computed at index time, so this is a
// cheap O(symbols) grouping, not a re-scan.
func Duplication(idx *Index, opts DuplicationOptions) DuplicationResponse {
	snap := idx.snapshot()
	listOpts := ListSymbolsOptions{SourceOnly: true, IncludeTests: opts.IncludeTests}

	groups := map[string][]CGPSymbol{}
	for _, sym := range snap.Symbols {
		if sym.ShapeHash == "" {
			continue
		}
		if !opts.IncludeTests && shouldExcludeNoisyFile(sym.File, listOpts) {
			continue
		}
		groups[sym.ShapeHash] = append(groups[sym.ShapeHash], sym)
	}

	var clusters []DuplicationCluster
	for hash, members := range groups {
		// Distinct symbol IDs only (a symbol never clones itself).
		seen := map[string]bool{}
		var uniq []CGPSymbol
		for _, m := range members {
			if seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			uniq = append(uniq, m)
		}
		if len(uniq) < 2 {
			continue
		}
		sort.SliceStable(uniq, func(i, j int) bool {
			if uniq[i].File != uniq[j].File {
				return uniq[i].File < uniq[j].File
			}
			return uniq[i].StartLine < uniq[j].StartLine
		})
		summaries := make([]CGPSymbolSummary, 0, len(uniq))
		for _, m := range uniq {
			summaries = append(summaries, summarizeSymbol(m))
		}
		lines := 0
		if colon := strings.IndexByte(hash, ':'); colon > 0 {
			lines, _ = strconv.Atoi(hash[:colon])
		}
		clusters = append(clusters, DuplicationCluster{Lines: lines, Count: len(uniq), Members: summaries})
	}

	// Most impactful first: more copies, then larger bodies.
	sort.SliceStable(clusters, func(i, j int) bool {
		if clusters[i].Count != clusters[j].Count {
			return clusters[i].Count > clusters[j].Count
		}
		if clusters[i].Lines != clusters[j].Lines {
			return clusters[i].Lines > clusters[j].Lines
		}
		if len(clusters[i].Members) > 0 && len(clusters[j].Members) > 0 {
			return clusters[i].Members[0].File < clusters[j].Members[0].File
		}
		return false
	})

	total := len(clusters)
	limit := opts.Limit
	if limit <= 0 {
		limit = maxDupClusters
	}
	truncated := false
	if len(clusters) > limit {
		clusters = clusters[:limit]
		truncated = true
	}
	if clusters == nil {
		clusters = []DuplicationCluster{}
	}
	return DuplicationResponse{
		Status:        "ok",
		TotalClusters: total,
		Truncated:     truncated,
		Clusters:      clusters,
	}
}

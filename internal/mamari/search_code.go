package mamari

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
)

const (
	defaultSearchCodeLimit        = 10
	defaultSearchCodeBudgetTokens = 1200
	defaultSearchCodeContextLines = 1
	maxSearchCodeCacheEntries     = 128
)

var searchTokenRe = regexp.MustCompile(`[A-Za-z0-9]+`)

// Exact-phrase signals lifted out of the natural-language query. A query that
// contains any of these patterns short-circuits the token-based ranker by
// adding a heavy score boost to lines whose source text contains the literal
// — so a query like "preview documents envelope previewEnvelopeDocuments" or
// "where is GET /signing/:id/preview" gets the right hit on the first pass
// instead of grinding through camel-split token bags. Intentionally compiled
// once at package init.
var (
	exactRouteRe          = regexp.MustCompile(`(/[A-Za-z][A-Za-z0-9_\-./:]+)`)
	exactMimeRe           = regexp.MustCompile(`(?i)\b(application|text|image|audio|video|font|multipart|model)/[A-Za-z0-9.+\-_]+`)
	exactQuotedDoubleRe   = regexp.MustCompile(`"([^"\n]{2,})"`)
	exactQuotedSingleRe   = regexp.MustCompile(`'([^'\n]{2,})'`)
	exactQuotedBacktickRe = regexp.MustCompile("`([^`\n]{2,})`")
	exactPredicateRe      = regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9_-]*):([A-Za-z][A-Za-z0-9_-]+)\b`)
	exactIdentifierRe     = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]+)\b`)
	exactKebabIdentRe     = regexp.MustCompile(`(?:^|[^A-Za-z0-9_./-])(\.?-?[A-Za-z_][A-Za-z0-9_]*-[A-Za-z0-9_-]*)\b`)
)

const (
	exactPhraseRouteWeight     = 1000
	exactPhraseMimeWeight      = 800
	exactPhraseQuotedWeight    = 900
	exactPhrasePredicateWeight = 850
	exactPhraseIdentWeight     = 700
	exactPhraseMinIdentLen     = 8
	exactPhraseMinIdentSegs    = 2
)

var searchStopWords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "by": true, "can": true, "does": true, "for": true,
	"from": true, "how": true, "i": true, "in": true, "inside": true,
	"is": true, "it": true, "of": true, "on": true, "or": true, "show": true,
	"shows": true, "the": true, "this": true, "to": true, "with": true,
	"should": true, "would": true, "could": true, "shall": true, "will": true,
	"nothing": true, "something": true, "anything": true, "everything": true,
	"someone": true, "anyone": true, "everyone": true, "anybody": true,
	"everybody": true, "nobody": true, "what": true, "which": true,
	"who": true, "whom": true, "when": true, "where": true, "why": true,
	"there": true, "here": true, "that": true, "these": true, "those": true,
}

var searchQueryExpansions = map[string][]string{
	"avoid":     {"guard", "skip", "block", "prevent"},
	"duplicate": {"double", "repeat", "repeated", "once", "idempotent"},
	"prevent":   {"guard", "skip", "block", "avoid"},
	"repeat":    {"duplicate", "repeated", "again"},
	"repeated":  {"duplicate", "repeat", "again"},
	"running":   {"run", "active", "started"},
}

var lifecycleQueryCueTerms = map[string]bool{
	"after":      true,
	"before":     true,
	"dispatch":   true,
	"exception":  true,
	"error":      true,
	"lifecycle":  true,
	"middleware": true,
	"request":    true,
	"response":   true,
	"teardown":   true,
}

var lifecycleExpansionTerms = []string{
	"context",
	"finalize",
	"full",
	"handle",
	"handler",
	"preprocess",
	"process",
	"response",
}

var lifecycleSymbolTerms = map[string]bool{
	"after":      true,
	"before":     true,
	"context":    true,
	"ctx":        true,
	"dispatch":   true,
	"exception":  true,
	"error":      true,
	"finalize":   true,
	"full":       true,
	"handle":     true,
	"handler":    true,
	"middleware": true,
	"preprocess": true,
	"process":    true,
	"pop":        true,
	"push":       true,
	"request":    true,
	"response":   true,
	"route":      true,
	"signal":     true,
	"teardown":   true,
	"view":       true,
}

var lifecyclePathTerms = map[string]bool{
	"app":         true,
	"application": true,
	"context":     true,
	"ctx":         true,
	"handler":     true,
	"middleware":  true,
	"router":      true,
	"routing":     true,
	"server":      true,
	"view":        true,
}

var commonCSSPropertyNames = map[string]bool{
	"align-items": true, "background-color": true, "border-radius": true,
	"box-shadow": true, "flex-direction": true, "font-family": true,
	"font-size": true, "font-weight": true, "grid-template-columns": true,
	"grid-template-rows": true, "justify-content": true, "letter-spacing": true,
	"line-height": true, "margin-bottom": true, "margin-left": true,
	"margin-right": true, "margin-top": true, "max-height": true,
	"max-width": true, "min-height": true, "min-width": true,
	"object-fit": true, "object-position": true, "overflow-x": true,
	"overflow-y": true, "padding-bottom": true, "padding-left": true,
	"padding-right": true, "padding-top": true, "text-align": true,
	"text-decoration": true, "text-overflow": true, "text-transform": true,
	"white-space": true, "z-index": true,
}

type exactPhrase struct {
	literal string
	kind    string
	weight  int
	// interiorBloom is the OR of searchTokenBloom over the literal's INTERIOR
	// tokens (all but the first and last). If a line's text contains the
	// literal as a substring, the same characters produce the same interior
	// token boundaries, so every interior token is guaranteed present in the
	// line's raw token set and therefore its bloom — the line bloom must be a
	// superset or strings.Contains cannot hit. The first/last tokens are
	// excluded because a substring match can start or end mid-token in the
	// line ("unreadAvailable" contains "readAvailable" but tokenizes to
	// unread+available, losing "read"). Zero (fewer than 3 tokens) disables
	// the gate for this phrase — never a false negative, only less pruning.
	interiorBloom uint64
}

// phraseInteriorBloom computes exactPhrase.interiorBloom for a literal.
func phraseInteriorBloom(literal string) uint64 {
	tokens := searchTokens(literal)
	if len(tokens) < 3 {
		return 0
	}
	var bloom uint64
	for _, tok := range tokens[1 : len(tokens)-1] {
		bloom |= searchTokenBloom(tok)
	}
	return bloom
}

type searchCodeCandidate struct {
	file         string
	line         int
	score        int
	matchedMask  uint64
	matchedExact []string
	definition   bool
}

type codeSearchFile struct {
	file       string
	language   string
	postingID  uint32
	pathTokens map[string]bool
	baseTokens map[string]bool
	// allTokens is a sorted NUL-delimited set. A []string kept a 16-byte
	// header per distinct token per file; large repositories retain millions
	// of those headers even though queries only need membership/iteration.
	allTokens compactTokenSet
	lines     []codeSearchLine
	source    string
	// Per-line symbol ownership is stored as spans into lineSymbolPointers.
	// Keeping a []*CGPSymbolSummary slice header on every source line cost 24
	// bytes even for the overwhelmingly common no-symbol line, plus one small
	// allocation for every populated line. The file-level table preserves the
	// same shared summaries with fixed-width spans in each line record.
	symbolSummaries   []*CGPSymbolSummary
	lineSymbolIndexes []uint32
	// tokenCount is the file's raw (non-deduped) token count across all
	// lines — the BM25 "document length" proxy used by bm25LengthNorm to
	// down-weight term-frequency evidence from unusually large files and
	// up-weight it from small, focused ones. See scoreSearchCodeLine.
	tokenCount int
}

type codeSearchLine struct {
	textStart   uint32
	textEnd     uint32
	tokenBloom  uint64
	symbolBloom uint64
	// lowerText was a second full copy of text removed in favor of
	// computing it on demand in scoreSearchCodeLine (the only consumer
	// that ever needed it, and only for the few lines per query that
	// already passed a token/symbol/exact-phrase match — see that
	// function's doc comment for why this is safe). Eagerly lowercasing
	// every line at build time duplicated the entire indexed text of a
	// repo a second time in memory for a benefit that scales with query
	// volume, not corpus size — matchExactPhrases just below already
	// lowercases its own input lazily for the same reason.
	tokens      compactTokenSet
	symbolText  compactTokenSet
	symbolStart uint32
	symbolCount uint8
}

func (file codeSearchFile) lineText(line codeSearchLine) string {
	start, end := int(line.textStart), int(line.textEnd)
	if start < 0 || end < start || end > len(file.source) {
		return ""
	}
	return file.source[start:end]
}

func (file codeSearchFile) lineSymbols(line codeSearchLine, dst []*CGPSymbolSummary) []*CGPSymbolSummary {
	start := int(line.symbolStart)
	end := start + int(line.symbolCount)
	if start < 0 || start > len(file.lineSymbolIndexes) || end < start || end > len(file.lineSymbolIndexes) {
		return nil
	}
	for _, index := range file.lineSymbolIndexes[start:end] {
		if int(index) < len(file.symbolSummaries) {
			dst = append(dst, file.symbolSummaries[index])
		}
	}
	return dst
}

func appendCodeSearchLineSymbols(file *codeSearchFile, symbols []*CGPSymbolSummary, indexes map[*CGPSymbolSummary]uint32) (uint32, uint8) {
	start := uint32(len(file.lineSymbolIndexes))
	if len(symbols) > 255 {
		symbols = symbols[:255]
	}
	for _, symbol := range symbols {
		index, ok := indexes[symbol]
		if !ok {
			index = uint32(len(file.symbolSummaries))
			indexes[symbol] = index
			file.symbolSummaries = append(file.symbolSummaries, symbol)
		}
		file.lineSymbolIndexes = append(file.lineSymbolIndexes, index)
	}
	return start, uint8(len(symbols))
}

type codeSearchPosting struct {
	fileID  uint32
	lineIdx uint32
}

type codeSearchPostingIndex map[string][]byte

// compactTokenSet stores an immutable sorted token set as NUL-delimited
// UTF-8. Tokens produced by searchTokens never contain NUL. Compared with a
// []string this keeps one string header per line instead of one header per
// token, while exact membership and iteration remain allocation-free.
type compactTokenSet string

func packCompactTokenSet(tokens []string) compactTokenSet {
	if len(tokens) == 0 {
		return ""
	}
	size := len(tokens) - 1
	for _, token := range tokens {
		size += len(token)
	}
	var b strings.Builder
	b.Grow(size)
	for i, token := range tokens {
		if i > 0 {
			b.WriteByte(0)
		}
		b.WriteString(token)
	}
	return compactTokenSet(b.String())
}

func (set compactTokenSet) contains(token string) bool {
	if token == "" || len(set) == 0 {
		return false
	}
	text := string(set)
	for offset := 0; offset+len(token) <= len(text); {
		match := strings.Index(text[offset:], token)
		if match < 0 {
			return false
		}
		start := offset + match
		end := start + len(token)
		if (start == 0 || text[start-1] == 0) && (end == len(text) || text[end] == 0) {
			return true
		}
		offset = start + 1
	}
	return false
}

func (set compactTokenSet) count() int {
	if len(set) == 0 {
		return 0
	}
	return strings.Count(string(set), "\x00") + 1
}

func (set compactTokenSet) forEach(visit func(string)) {
	text := string(set)
	for start := 0; start < len(text); {
		end := strings.IndexByte(text[start:], 0)
		if end < 0 {
			visit(text[start:])
			return
		}
		end += start
		visit(text[start:end])
		start = end + 1
	}
}

func (set compactTokenSet) strings() []string {
	if len(set) == 0 {
		return nil
	}
	return strings.Split(string(set), "\x00")
}

func searchTokenBloom(token string) uint64 {
	var hash uint64 = 1469598103934665603
	for i := 0; i < len(token); i++ {
		hash ^= uint64(token[i])
		hash *= 1099511628211
	}
	return uint64(1)<<(hash&63) | uint64(1)<<((hash>>6)&63)
}

func searchTokenBlooms(tokens []string) []uint64 {
	out := make([]uint64, len(tokens))
	for i, token := range tokens {
		out[i] = searchTokenBloom(token)
	}
	return out
}

func compactTokenBloom(tokens []string) uint64 {
	var bloom uint64
	for _, token := range tokens {
		bloom |= searchTokenBloom(token)
	}
	return bloom
}

type searchCodeFileScore struct {
	pathMatches []string
	fileMatches []string
	pathMask    uint64
	fileMask    uint64
	pathScore   int
	skip        bool
}

type searchCodeIntent struct {
	lifecycle bool
}

type searchCodeCacheKey struct {
	query             string
	mode              string
	limit             int
	budgetTokens      int
	contextLines      int
	sourceOnly        bool
	includeTests      bool
	includeStories    bool
	exactFirst        bool
	preferDefinitions bool
	preferUsages      bool
	symbolDetail      bool
}

type searchCodeCacheEntry struct {
	generation uint64
	terms      []string
	phrases    []exactPhrase
	response   SearchCodeResponse
}

// SearchCode returns a small ranked evidence packet for natural-language repo
// questions. It intentionally searches indexed source files only, then returns
// bounded line snippets rather than whole files. This gives MCP agents a cheap
// discovery step before they decide which symbols or file:line contexts to
// inspect in detail.
func SearchCode(idx *Index, query string, opts SearchCodeOptions) (resp SearchCodeResponse) {
	query = strings.TrimSpace(query)
	resp = SearchCodeResponse{
		Status:       "not_found",
		Query:        query,
		Hits:         []SearchCodeHit{},
		Limit:        opts.Limit,
		BudgetTokens: opts.BudgetTokens,
	}
	if query == "" {
		resp.Status = "invalid"
		resp.Warnings = append(resp.Warnings, "empty query")
		return resp
	}
	if opts.Limit <= 0 {
		opts.Limit = defaultSearchCodeLimit
	}
	if opts.BudgetTokens <= 0 {
		opts.BudgetTokens = defaultSearchCodeBudgetTokens
	}
	if opts.ContextLines < 0 {
		opts.ContextLines = 0
	}
	if opts.ContextLines == 0 {
		opts.ContextLines = defaultSearchCodeContextLines
	}
	resp.Limit = opts.Limit
	resp.BudgetTokens = opts.BudgetTokens
	mode := normalizeSearchCodeMode(opts.Mode)
	resp.Mode = mode

	snap := idx.published.Load()
	var prefixNames map[string]bool
	if snap != nil {
		prefixNames = snap.prefixNames
	} else {
		prefixNames = idx.prefixNamesSnapshot()
	}

	terms := searchQueryTerms(query)
	termBlooms := searchTokenBlooms(terms)
	intent := classifySearchCodeIntent(terms)
	queryLower := strings.ToLower(query)
	includeSupportFiles := queryMentionsSupportFiles(queryLower)
	phrases := extractExactPhrases(query, prefixNames)
	resp.ExactPhrases = exactPhraseSummaries(phrases)
	if len(terms) == 0 && len(phrases) == 0 {
		resp.Status = "invalid"
		resp.Warnings = append(resp.Warnings, "query has no searchable terms")
		return resp
	}

	cacheKey := newSearchCodeCacheKey(query, opts, mode)
	if snap != nil && !opts.BlastRadius {
		if cached, ok := idx.loadSearchCodeResult(cacheKey, snap.generation); ok {
			return cached
		}
		defer func() {
			idx.storeSearchCodeResult(cacheKey, snap, terms, phrases, resp)
		}()
	}

	var files []codeSearchFile
	var postings codeSearchPostingIndex
	var termDocFreq map[string]int
	if snap != nil {
		// A watcher has published at least one snapshot — read it directly,
		// no locking at all. May be one rebake-generation behind the very
		// latest edit, but never blocks on idx.mu regardless of how long a
		// concurrent rebake takes.
		files = snap.codeSearchFiles
		postings = snap.postings
		termDocFreq = snap.termDocFreq
	} else {
		// No watcher running (plain one-shot CLI), or a watcher hasn't
		// completed its first rebake/warm-up publish yet — fall back to the
		// lazy on-demand build. The cache maps are immutable after publication
		// (incremental refreshes clone and replace them), so retaining these
		// references after releasing idx.mu is consistent with the copied
		// file slice and lets ordinary non-watch MCP servers use postings too.
		files = idx.ensureCodeSearchIndex()
		idx.mu.Lock()
		postings = idx.codeSearchPostings
		termDocFreq = idx.codeSearchTermDocFreq
		idx.mu.Unlock()
	}
	var termWeights map[string]int
	if len(termDocFreq) > 0 {
		termWeights = searchTermWeightsFromDocFreq(terms, len(files), termDocFreq)
	} else {
		termWeights = searchTermWeights(terms, files)
	}
	avgDocLen := avgCodeSearchFileTokenCount(files)
	var candidates []searchCodeCandidate
	totalCandidates := 0
	// Low-signal gate aggregates, accumulated at append time (before any
	// mid-scan truncation) so they reflect the complete candidate set. The
	// gate reasons about the RAW user-typed terms only — expansion-added
	// synonyms would otherwise both fake the "gibberish present" signal
	// (df==0 synonyms on small corpora) and defeat the co-match test
	// (raw+synonym trivially co-match) — so the union mask and per-candidate
	// co-match count are restricted to raw-term bits.
	gateRawMask := searchRawTermMask(query, terms)
	var gateUnionMask uint64
	gateMaxTerms := 0
	if len(terms) > 0 && len(postings) > 0 {
		// Use every query term. Selecting only high-IDF terms is faster on
		// very broad queries but is not result-equivalent: the selected terms
		// can occur solely in a source-only-excluded support file while a core
		// implementation matches another term. The complete union is still
		// much smaller than scanning every source line and preserves the
		// linear scorer's exact candidate set.
		//
		// The postings path runs even when exact phrases were extracted (it
		// used to require len(phrases)==0, forcing a full-corpus line scan on
		// the commonest query shape — looking up a specific Camel/snake/route
		// identifier, which extractExactPhrases always turns into a phrase).
		// It is result-equivalent: a phrase is a substring of the query, so
		// its constituent tokens are a subset of `terms`; matchExactPhrases is
		// a substring test, and a hit implies those tokens are present on the
		// line, hence the line is indexed under them (same tokenizer builds
		// both query terms and line postings) and is therefore in the postings
		// union. No phrase is all-stopwords, so at least one non-stopword term
		// always anchors the line. The identical `phrases` slice and
		// `len(phrases)==0` flag are still passed to the scorers below, so all
		// phrase-conditioned scoring is preserved — only the visited line set
		// shrinks from every-line to the postings union.
		lineRefs := candidatePostingsForTerms(postings, terms)
		filesByID := make(map[uint32]codeSearchFile, len(files))
		for _, file := range files {
			filesByID[file.postingID] = file
		}
		pathCache := map[uint32]searchCodeFileScore{}
		for _, ref := range lineRefs {
			file, ok := filesByID[ref.fileID]
			if !ok {
				continue
			}
			lineIdx := int(ref.lineIdx)
			if lineIdx >= len(file.lines) {
				continue
			}
			cached, ok := pathCache[ref.fileID]
			if !ok {
				cached = searchCodeFilePathScore(file, terms, termWeights, includeSupportFiles, len(phrases) == 0, intent, opts)
				pathCache[ref.fileID] = cached
			}
			if cached.skip {
				continue
			}
			cand, ok := scoreSearchCodeLine(file, file.lines[lineIdx], lineIdx+1, terms, termBlooms, phrases, termWeights, queryLower, cached.pathMask, cached.fileMask, cached.pathScore, avgDocLen, intent, opts)
			if !ok {
				continue
			}
			candidates = append(candidates, cand)
			totalCandidates++
			gateUnionMask |= cand.matchedMask & gateRawMask
			if n := bits.OnesCount64(cand.matchedMask & gateRawMask); n > gateMaxTerms {
				gateMaxTerms = n
			}
			if !opts.ExactFirst && len(candidates) > 8192 {
				sortSearchCodeCandidates(candidates)
				candidates = candidates[:4096]
			}
		}
	} else {
		for _, file := range files {
			cached := searchCodeFilePathScore(file, terms, termWeights, includeSupportFiles, len(phrases) == 0, intent, opts)
			if cached.skip {
				continue
			}
			for lineIdx, line := range file.lines {
				cand, ok := scoreSearchCodeLine(file, line, lineIdx+1, terms, termBlooms, phrases, termWeights, queryLower, cached.pathMask, cached.fileMask, cached.pathScore, avgDocLen, intent, opts)
				if !ok {
					continue
				}
				candidates = append(candidates, cand)
				totalCandidates++
				gateUnionMask |= cand.matchedMask & gateRawMask
				if n := bits.OnesCount64(cand.matchedMask & gateRawMask); n > gateMaxTerms {
					gateMaxTerms = n
				}
				if !opts.ExactFirst && len(candidates) > 8192 {
					sortSearchCodeCandidates(candidates)
					candidates = candidates[:4096]
				}
			}
		}
	}
	if opts.ExactFirst && len(phrases) > 0 {
		filtered := candidates[:0]
		for _, cand := range candidates {
			if len(cand.matchedExact) > 0 {
				filtered = append(filtered, cand)
			}
		}
		if len(filtered) > 0 {
			candidates = filtered
			totalCandidates = len(candidates)
			resp.Warnings = append(resp.Warnings, "exact_first filtered search results to exact phrase matches")
		}
	}

	sortSearchCodeCandidates(candidates)
	if !opts.ExactFirst && len(candidates) > 4096 {
		candidates = candidates[:4096]
	}
	resp.Total = totalCandidates
	if len(candidates) == 0 {
		return resp
	}
	if gateLowSignalSearch(terms, phrases, termWeights, gateRawMask, gateUnionMask, gateMaxTerms) {
		// Every candidate matched only a single, ultra-common term while other
		// query terms don't exist in the corpus at all — the classic gibberish
		// query. Ranked junk anchored on words like "match"/"this" wastes the
		// caller's budget; an honest not_found with the unmatched terms named
		// is cheaper and more actionable.
		resp.Total = 0
		resp.Warnings = appendUniqueString(resp.Warnings, lowSignalSearchWarning(terms, gateRawMask, termWeights))
		return resp
	}
	if warning := unmatchedRawTermsWarning(terms, gateRawMask, termWeights); warning != "" {
		// Some user-typed terms don't exist in the corpus but the rest carried
		// real signal, so hits are returned — still say which terms matched
		// nothing, so the caller can tell a partial answer from a full one.
		resp.Warnings = appendUniqueString(resp.Warnings, warning)
		if lowConfidenceSearch(terms, phrases, gateRawMask, gateMaxTerms) {
			// The hard gate above catches single-common-term junk; this
			// catches its sibling shape that survives it — distinctive terms
			// absent from the corpus while even the best candidate co-matched
			// only a minority of the typed terms, so ranking is anchored on
			// the query's least-informative words, such as a gibberish query's
			// stopword-like tail. Hits are
			// still returned — they can be legitimate leads — but flagged so
			// a calling agent doesn't spend budget reading them as answers.
			resp.Confidence = "low"
			resp.Warnings = appendUniqueString(resp.Warnings, "low-confidence result: best hit matches only a minority of query terms")
		}
	}

	covered := map[string][][2]int{}
	perFile := map[string]int{}
	blastHits := 0
	// maxHitsPerFileFirstPass diversifies the top-N so no single large file
	// monopolizes the slots and crowds out smaller but equally relevant files whose best
	// line ranks slightly lower. Candidates over the cap are deferred, not
	// dropped, and fill any remaining slots in the same score order in pass two,
	// so a genuinely single-file-heavy result is never under-filled.
	const maxHitsPerFileFirstPass = 3
	var deferred []searchCodeCandidate

	tryEmit := func(cand searchCodeCandidate) {
		start := cand.line - opts.ContextLines
		if start < 1 {
			start = 1
		}
		end := cand.line + opts.ContextLines
		if overlapsCovered(covered[cand.file], start, end) {
			return
		}
		hit, ok := buildSearchCodeHit(files, cand, terms, start, end, opts.BudgetTokens-resp.EstimatedTokens)
		if !ok {
			resp.Truncated = true
			return
		}
		covered[cand.file] = append(covered[cand.file], [2]int{hit.StartLine, hit.EndLine})
		applySearchCodeMode(&hit, mode)
		if opts.BlastRadius && blastHits < 1 {
			// Must run before compactSearchCodeHitSymbols: it needs each
			// symbol's ID to trace callers/callees, which the compact
			// projection below deliberately drops.
			addSearchCodeBlastRadius(idx, &hit)
			if len(hit.BlastRadius) > 0 {
				blastHits++
			}
		}
		if !opts.SymbolDetail {
			compactSearchCodeHitSymbols(&hit)
		}
		resp.Hits = append(resp.Hits, hit)
		resp.EstimatedTokens += hit.EstimatedTokens
		perFile[cand.file]++
	}

	for _, cand := range candidates {
		if len(resp.Hits) >= opts.Limit || resp.EstimatedTokens >= opts.BudgetTokens {
			resp.Truncated = true
			break
		}
		if opts.DiversifyFiles && perFile[cand.file] >= maxHitsPerFileFirstPass {
			deferred = append(deferred, cand)
			continue
		}
		tryEmit(cand)
	}
	for _, cand := range deferred {
		if len(resp.Hits) >= opts.Limit || resp.EstimatedTokens >= opts.BudgetTokens {
			resp.Truncated = true
			break
		}
		tryEmit(cand)
	}
	if len(resp.Hits) == 0 {
		resp.Status = "budget_exceeded"
		return resp
	}
	resp.Status = "ok"
	if resp.Total > len(resp.Hits) {
		resp.Truncated = true
	}
	return resp
}

// FitSearchCodeResponse applies BudgetTokens to the complete JSON packet.
// Candidate construction uses source-text estimates so ranking can stay cheap,
// but paths, scores, matched terms, and symbol metadata are also context paid
// by CLI/MCP clients. The core SearchCode API deliberately retains its
// historical source-evidence budget; serialization boundaries call this
// shaper immediately before returning the packet.
func FitSearchCodeResponse(resp *SearchCodeResponse, budget int) {
	if resp == nil || resp.Status != "ok" || budget <= 0 {
		return
	}
	// A successful response omits the query echo: the caller already knows what
	// it asked, and every echoed byte is context the agent pays for. The error
	// paths (not the concern here) keep Query so a failed call is debuggable.
	resp.Query = ""
	serializedBudget := budget * 7 / 8
	if serializedBudget <= 0 {
		serializedBudget = budget
	}
	if searchCodeSerializedTokens(resp) <= serializedBudget {
		resp.EstimatedTokens = searchCodeSerializedTokens(resp)
		return
	}

	resp.Truncated = true
	resp.Warnings = appendUniqueString(resp.Warnings, "result shaped to fit total serialized token budget")
	for searchCodeSerializedTokens(resp) > serializedBudget && len(resp.Hits) > 1 {
		resp.Hits = resp.Hits[:len(resp.Hits)-1]
	}
	resp.EstimatedTokens = searchCodeSerializedTokens(resp)
}

func searchCodeSerializedTokens(resp *SearchCodeResponse) int {
	if resp == nil {
		return 0
	}
	copy := *resp
	copy.EstimatedTokens = 0
	data, err := json.Marshal(copy)
	if err != nil {
		return 0
	}
	estimate := EstimateTokens(string(data))
	copy.EstimatedTokens = estimate
	data, err = json.Marshal(copy)
	if err != nil {
		return estimate
	}
	return EstimateTokens(string(data))
}

func sortSearchCodeCandidates(candidates []searchCodeCandidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		if candidates[i].file != candidates[j].file {
			return candidates[i].file < candidates[j].file
		}
		return candidates[i].line < candidates[j].line
	})
}

func searchCodeFilePathScore(file codeSearchFile, terms []string, termWeights map[string]int, includeSupportFiles bool, applyWeights bool, intent searchCodeIntent, opts SearchCodeOptions) searchCodeFileScore {
	if isBackupOrDeadFile(file.file) {
		return searchCodeFileScore{skip: true}
	}
	if opts.SourceOnly && shouldExcludeNoisyFile(file.file, ListSymbolsOptions{
		IncludeTests:   opts.IncludeTests,
		IncludeStories: opts.IncludeStories,
	}) {
		return searchCodeFileScore{skip: true}
	}
	if opts.SourceOnly && !includeSupportFiles && isExampleOrDemoPath(file.file) {
		return searchCodeFileScore{skip: true}
	}
	pathMatches := matchedTermsInSet(terms, file.pathTokens)
	baseMatches := matchedTermsInSet(terms, file.baseTokens)
	fileMatches := matchedTermsInCompactSet(terms, file.allTokens)
	pathScore := len(pathMatches) * 18
	pathScore += len(baseMatches) * 28
	pathScore -= searchSupportFilePenalty(file.file, includeSupportFiles)
	if intent.lifecycle {
		pathScore += lifecyclePathBoost(file)
	}
	if applyWeights {
		pathScore += weightedSearchTermScore(pathMatches, termWeights) / 8
		pathScore += weightedSearchTermScore(baseMatches, termWeights) / 5
	}
	return searchCodeFileScore{
		pathMatches: pathMatches,
		fileMatches: fileMatches,
		pathMask:    termMaskFromTerms(terms, pathMatches),
		fileMask:    termMaskFromTerms(terms, fileMatches),
		pathScore:   pathScore,
	}
}

// bm25K1 and bm25B are the standard BM25 term-frequency-saturation and
// length-normalization constants (Robertson/Sparck-Jones defaults), applied
// here only to the IDF-weighted term-weight contribution in
// scoreSearchCodeLine — not to the exact-phrase, symbol, or structural
// boosts, which are independently benchmark-tuned and left untouched.
const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// avgCodeSearchFileTokenCount returns the corpus-wide average file token
// count, the BM25 "average document length" term. Computed fresh per query
// (O(files), same order as the path-score pass already done per query) so it
// always reflects the current file set without plumbing incremental upkeep
// through the watcher's add/remove paths.
func avgCodeSearchFileTokenCount(files []codeSearchFile) float64 {
	if len(files) == 0 {
		return 0
	}
	total := 0
	for _, f := range files {
		total += f.tokenCount
	}
	if total == 0 {
		return 0
	}
	return float64(total) / float64(len(files))
}

// bm25LengthNorm scales a term-weight contribution by how the file's length
// compares to the corpus average: shorter, more focused files score higher
// per matched term than long files matching the same terms incidentally.
// Returns 1.0 (no-op) when length data is unavailable.
func bm25LengthNorm(tokenCount int, avgDocLen float64) float64 {
	if avgDocLen <= 0 || tokenCount <= 0 {
		return 1.0
	}
	return (bm25K1 + 1) / (bm25K1*(1-bm25B+bm25B*(float64(tokenCount)/avgDocLen)) + 1)
}

func scoreSearchCodeLine(file codeSearchFile, line codeSearchLine, lineNumber int, terms []string, termBlooms []uint64, phrases []exactPhrase, termWeights map[string]int, queryLower string, pathMask, fileMask uint64, pathScore int, avgDocLen float64, intent searchCodeIntent, opts SearchCodeOptions) (searchCodeCandidate, bool) {
	var lineSymbolScratch [3]*CGPSymbolSummary
	lineSymbols := file.lineSymbols(line, lineSymbolScratch[:0])
	lineText := file.lineText(line)
	lineMask := matchedTermsMaskInCompactSet(terms, termBlooms, line.tokens, line.tokenBloom)
	symbolMask := matchedTermsMaskInCompactSet(terms, termBlooms, line.symbolText, line.symbolBloom)
	exactMatches, exactScore := matchExactPhrases(lineText, line.tokenBloom, phrases)
	if lineMask == 0 && symbolMask == 0 && len(exactMatches) == 0 {
		return searchCodeCandidate{}, false
	}
	matchMask := pathMask | lineMask | symbolMask
	matchCount := bits.OnesCount64(matchMask)
	lineMatchCount := bits.OnesCount64(lineMask)
	symbolMatchCount := bits.OnesCount64(symbolMask)
	symbolScore := symbolMatchCount * 34
	if len(lineSymbols) > 0 {
		symbolScore += 10
	}
	symbolScore += exactSymbolNameSearchBoost(lineSymbols, line.symbolText.count(), symbolMatchCount, queryLower, terms)
	score := pathScore + lineMatchCount*80 + symbolScore + matchCount*matchCount*14 + exactScore
	if len(phrases) == 0 {
		lengthNorm := bm25LengthNorm(file.tokenCount, avgDocLen)
		score += int(float64(weightedSearchTermScoreMask(matchMask, terms, termWeights)) * lengthNorm)
		score += rareTermClusterBoostMask(matchMask, terms, termWeights)
		score += searchFileCoverageBoostMask(fileMask, matchMask, terms, termWeights)
	}
	if len(terms) > 0 && containsAllTermsMask(matchMask, len(terms)) {
		score += 120
	}
	// The source line is lowercased here rather than once per line at build
	// time (see codeSearchLine's doc comment): scoreSearchCodeLine
	// only runs for lines that already passed the lineMask/symbolMask/
	// exactMatches check above, so this pays the allocation only for the
	// few per-query candidate lines, not every line in the repo.
	if strings.Contains(strings.ToLower(lineText), queryLower) {
		score += 160
	}
	if strings.Contains(lineText, "class=") || strings.Contains(lineText, "@click=") || strings.Contains(lineText, "v-if=") || strings.Contains(lineText, "v-model=") {
		score += 18
	}
	definition := isSearchDefinitionLine(file, line, lineNumber)
	if intent.lifecycle {
		score += lifecycleLineBoost(file, lineMask, symbolMask, terms, definition)
	}
	if definition && len(exactMatches) > 0 {
		score += 180
	}
	if opts.PreferDefinitions && definition {
		score += 420
	}
	if opts.PreferDefinitions && !definition {
		score -= 60
	}
	if opts.PreferUsages && !definition {
		score += 180
	}
	if opts.PreferUsages && definition {
		score -= 80
	}
	return searchCodeCandidate{
		file: file.file, line: lineNumber, score: score, matchedMask: matchMask, matchedExact: exactMatches, definition: definition,
	}, true
}

func buildCodeSearchPostings(files []codeSearchFile) codeSearchPostingIndex {
	raw := map[string][]codeSearchPosting{}
	for _, file := range files {
		for lineIdx, line := range file.lines {
			seen := map[string]bool{}
			line.tokens.forEach(func(tok string) {
				if tok == "" || seen[tok] {
					return
				}
				seen[tok] = true
				raw[tok] = append(raw[tok], codeSearchPosting{fileID: file.postingID, lineIdx: uint32(lineIdx)})
			})
			line.symbolText.forEach(func(tok string) {
				if tok == "" || seen[tok] {
					return
				}
				seen[tok] = true
				raw[tok] = append(raw[tok], codeSearchPosting{fileID: file.postingID, lineIdx: uint32(lineIdx)})
			})
		}
	}
	postings := make(codeSearchPostingIndex, len(raw))
	for token, refs := range raw {
		postings[token] = encodeCodeSearchPostings(refs)
	}
	return postings
}

func codeSearchPostingKey(ref codeSearchPosting) uint64 {
	return uint64(ref.fileID)<<32 | uint64(ref.lineIdx)
}

func encodeCodeSearchPostings(refs []codeSearchPosting) []byte {
	if len(refs) == 0 {
		return nil
	}
	sort.Slice(refs, func(i, j int) bool {
		return codeSearchPostingKey(refs[i]) < codeSearchPostingKey(refs[j])
	})
	encoded := make([]byte, 0, len(refs)*3)
	var previous uint64
	for _, ref := range refs {
		key := codeSearchPostingKey(ref)
		encoded = binary.AppendUvarint(encoded, key-previous)
		previous = key
	}
	return encoded
}

func decodeCodeSearchPostings(encoded []byte, dst []codeSearchPosting) []codeSearchPosting {
	var previous uint64
	for len(encoded) > 0 {
		delta, size := binary.Uvarint(encoded)
		if size <= 0 {
			break
		}
		previous += delta
		dst = append(dst, codeSearchPosting{
			fileID:  uint32(previous >> 32),
			lineIdx: uint32(previous),
		})
		encoded = encoded[size:]
	}
	return dst
}

func buildCodeSearchTermDocFreq(files []codeSearchFile) map[string]int {
	docFreq := map[string]int{}
	for _, file := range files {
		file.allTokens.forEach(func(token string) {
			docFreq[token]++
		})
	}
	return docFreq
}

func addCodeSearchFileToIndexes(postings codeSearchPostingIndex, docFreq map[string]int, file codeSearchFile) {
	file.allTokens.forEach(func(token string) {
		docFreq[token]++
	})
	additions := map[string][]codeSearchPosting{}
	for lineIdx, line := range file.lines {
		seen := map[string]bool{}
		line.tokens.forEach(func(tok string) {
			if tok == "" || seen[tok] {
				return
			}
			seen[tok] = true
			additions[tok] = append(additions[tok], codeSearchPosting{fileID: file.postingID, lineIdx: uint32(lineIdx)})
		})
		line.symbolText.forEach(func(tok string) {
			if tok == "" || seen[tok] {
				return
			}
			seen[tok] = true
			additions[tok] = append(additions[tok], codeSearchPosting{fileID: file.postingID, lineIdx: uint32(lineIdx)})
		})
	}
	for token, refs := range additions {
		list := decodeCodeSearchPostings(postings[token], nil)
		list = append(list, refs...)
		postings[token] = encodeCodeSearchPostings(list)
	}
}

func removeCodeSearchFileFromIndexes(postings codeSearchPostingIndex, docFreq map[string]int, file codeSearchFile) {
	file.allTokens.forEach(func(token string) {
		if docFreq[token] <= 1 {
			delete(docFreq, token)
		} else {
			docFreq[token]--
		}
	})
	tokens := map[string]bool{}
	for _, line := range file.lines {
		line.tokens.forEach(func(tok string) {
			if tok != "" {
				tokens[tok] = true
			}
		})
		line.symbolText.forEach(func(tok string) {
			if tok != "" {
				tokens[tok] = true
			}
		})
	}
	for token := range tokens {
		encoded := postings[token]
		if len(encoded) == 0 {
			continue
		}
		list := decodeCodeSearchPostings(encoded, nil)
		kept := make([]codeSearchPosting, 0, len(list))
		for _, ref := range list {
			if ref.fileID == file.postingID {
				continue
			}
			kept = append(kept, ref)
		}
		if len(kept) == 0 {
			delete(postings, token)
		} else {
			postings[token] = encodeCodeSearchPostings(kept)
		}
	}
}

func cloneCodeSearchPostingsMap(in codeSearchPostingIndex) codeSearchPostingIndex {
	if len(in) == 0 {
		return nil
	}
	out := make(codeSearchPostingIndex, len(in))
	for token, refs := range in {
		out[token] = refs
	}
	return out
}

func cloneIntMap(in map[string]int) map[string]int {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func candidatePostingsForTerms(postings codeSearchPostingIndex, terms []string) []codeSearchPosting {
	total := 0
	for _, term := range terms {
		total += len(postings[term]) / 2
	}
	refs := make([]codeSearchPosting, 0, total)
	for _, term := range terms {
		refs = decodeCodeSearchPostings(postings[term], refs)
	}
	// Sort via packed uint64 keys (fileID in the high half, lineIdx in the
	// low half — the same lexicographic order the struct comparator produced)
	// instead of a closure-based sort.Slice: on broad natural-language
	// queries the union routinely holds 100k+ refs, and eliminating the
	// per-comparison closure calls makes this the difference between the
	// postings path being a win or a wash on large repos. Order and the
	// deduped result set are provably identical.
	keys := make([]uint64, len(refs))
	for i, ref := range refs {
		keys[i] = uint64(ref.fileID)<<32 | uint64(ref.lineIdx)
	}
	slices.Sort(keys)
	out := refs[:0]
	var last uint64
	for i, key := range keys {
		if i > 0 && key == last {
			continue
		}
		last = key
		out = append(out, codeSearchPosting{fileID: uint32(key >> 32), lineIdx: uint32(key)})
	}
	return out
}

func addSearchCodeBlastRadius(idx *Index, hit *SearchCodeHit) {
	if len(hit.Symbols) == 0 {
		return
	}
	const (
		maxSymbols = 2
		maxRelated = 3
	)
	for _, sym := range hit.Symbols {
		if len(hit.BlastRadius) >= maxSymbols {
			break
		}
		if sym.ID == "" || !isBlastRadiusSymbolKind(sym.Kind) {
			continue
		}
		trace := TraceSymbolWithOptions(idx, sym.ID, TraceSymbolOptions{Sites: false, ExcludeTests: false})
		blast := SearchCodeBlast{Symbol: compactSearchRelatedSymbol(sym)}
		for _, caller := range trace.Callers {
			if caller.Kind == "test-callback-group" || isTestPath(caller.File) {
				blast.Tests = appendLimitedRelated(blast.Tests, caller, maxRelated)
				continue
			}
			blast.Callers = appendLimitedRelated(blast.Callers, caller, maxRelated)
		}
		for _, callee := range trace.Callees {
			blast.Callees = appendLimitedRelated(blast.Callees, callee, maxRelated)
		}
		hit.BlastRadius = append(hit.BlastRadius, blast)
	}
}

func isBlastRadiusSymbolKind(kind string) bool {
	if isTerraformSymbolKind(kind) {
		return true
	}
	switch kind {
	case "function", "method", "class", "component", "http-route", "http-endpoint":
		return true
	default:
		return false
	}
}

func appendLimitedRelated(out []SearchCodeRelatedSymbol, sym CGPSymbolSummary, limit int) []SearchCodeRelatedSymbol {
	if len(out) >= limit {
		if len(out) > 0 {
			out[len(out)-1].More = true
		}
		return out
	}
	return append(out, compactSearchRelatedSymbol(sym))
}

func compactSearchRelatedSymbol(sym CGPSymbolSummary) SearchCodeRelatedSymbol {
	return SearchCodeRelatedSymbol{
		Name:      sym.Name,
		Kind:      sym.Kind,
		File:      sym.File,
		StartLine: sym.StartLine,
		Count:     sym.Count,
		More:      sym.Truncated,
	}
}

func normalizeSearchCodeMode(mode string) string {
	switch mode {
	case ModeCompact, ModeEvidence, ModeContext:
		return mode
	default:
		return ModeContext
	}
}

func applySearchCodeMode(hit *SearchCodeHit, mode string) {
	switch mode {
	case ModeCompact:
		hit.Text = ""
		hit.EstimatedTokens = 0
	case ModeEvidence:
		hit.Text = focusedSearchLine(hit.Text, hit.StartLine, hit.FocusLine)
		hit.StartLine = hit.FocusLine
		hit.EndLine = hit.FocusLine
		hit.EstimatedTokens = EstimateTokens(hit.Text)
	}
}

// compactSearchCodeHitSymbols trims hit.Symbols (already value copies from
// dereferenceSymbolSummaries, so this never mutates the per-symbol summaries
// cached/shared by searchSymbolsByLine) down to name/kind/file/startLine —
// the same compact projection trace_symbol/impact use for caller/callee
// entries. Applied unconditionally unless SymbolDetail is set; relevance
// scoring already ran against the full summary before this point, so
// trimming the output here changes nothing about which hits were chosen.
func compactSearchCodeHitSymbols(hit *SearchCodeHit) {
	for i := range hit.Symbols {
		hit.Symbols[i] = CGPSymbolSummary{
			Name:      hit.Symbols[i].Name,
			Kind:      hit.Symbols[i].Kind,
			File:      hit.Symbols[i].File,
			StartLine: hit.Symbols[i].StartLine,
		}
	}
}

// MarshalJSON emits a token-lean SearchCodeHit: it never serializes the
// internal estimatedTokens accounting field, and omits endLine/focusLine
// whenever they carry no information beyond startLine (the evidence-mode
// common case, where a hit is a single focused line so all three are equal).
// The Go fields stay populated for budget math and in-process callers; only
// the wire shape shrinks. Nothing an agent acts on (file, line, score,
// matched terms, source text, symbols) is dropped.
func (h SearchCodeHit) MarshalJSON() ([]byte, error) {
	type wire struct {
		File         string            `json:"file"`
		StartLine    int               `json:"startLine"`
		EndLine      int               `json:"endLine,omitempty"`
		FocusLine    int               `json:"focusLine,omitempty"`
		Score        int               `json:"score"`
		MatchedTerms []string          `json:"matchedTerms,omitempty"`
		MatchedExact []string          `json:"matchedExact,omitempty"`
		Symbols      []json.RawMessage `json:"symbols,omitempty"`
		BlastRadius  []SearchCodeBlast `json:"blastRadius,omitempty"`
		Text         string            `json:"text,omitempty"`
	}
	w := wire{
		File:         h.File,
		StartLine:    h.StartLine,
		Score:        h.Score,
		MatchedTerms: h.MatchedTerms,
		MatchedExact: h.MatchedExact,
		BlastRadius:  h.BlastRadius,
		Text:         h.Text,
	}
	if h.EndLine != 0 && h.EndLine != h.StartLine {
		w.EndLine = h.EndLine
	}
	if h.FocusLine != 0 && h.FocusLine != h.StartLine {
		w.FocusLine = h.FocusLine
	}
	// A symbol with an empty ID is the compact projection (see
	// compactSearchCodeHitSymbols) applied whenever symbol_detail is off: it
	// carries only name/kind/file/startLine, and its file/startLine almost
	// always restate the hit's own. Drop those two when redundant, leaving
	// {name,kind}. A symbol_detail=true symbol keeps its ID/signature/etc., so
	// it is serialized in full and untouched.
	for _, s := range h.Symbols {
		if s.ID == "" {
			lean := struct {
				Name      string `json:"name"`
				Kind      string `json:"kind"`
				File      string `json:"file,omitempty"`
				StartLine int    `json:"startLine,omitempty"`
			}{Name: s.Name, Kind: s.Kind}
			if s.File != h.File {
				lean.File = s.File
			}
			if s.StartLine != h.StartLine {
				lean.StartLine = s.StartLine
			}
			b, err := json.Marshal(lean)
			if err != nil {
				return nil, err
			}
			w.Symbols = append(w.Symbols, b)
			continue
		}
		b, err := json.Marshal(s)
		if err != nil {
			return nil, err
		}
		w.Symbols = append(w.Symbols, b)
	}
	return json.Marshal(w)
}

func focusedSearchLine(text string, startLine, focusLine int) string {
	if text == "" {
		return ""
	}
	lines := strings.SplitAfter(text, "\n")
	idx := focusLine - startLine
	if idx >= 0 && idx < len(lines) {
		return lines[idx]
	}
	return firstLine(text)
}

func isSearchDefinitionLine(file codeSearchFile, line codeSearchLine, lineNumber int) bool {
	var scratch [3]*CGPSymbolSummary
	for _, sym := range file.lineSymbols(line, scratch[:0]) {
		if sym.StartLine != lineNumber {
			continue
		}
		switch sym.Kind {
		case "function", "method", "component", "class", "interface", "type", "constant", "getter", "setter", "ttl-shape", "ttl-term", "vue-prop", "vue-model", "vue-emit", "http-route", "http-endpoint":
			return true
		}
		if isTerraformSymbolKind(sym.Kind) {
			return true
		}
	}
	return false
}

func (idx *Index) ensureCodeSearchIndex() []codeSearchFile {
	// Watch mode prewarms this cache immediately after registering filesystem
	// watches, while the MCP server may already accept its first query. Without
	// single-flight serialization both goroutines can tokenize the entire repo
	// concurrently, roughly doubling cold latency and peak allocation.
	idx.codeSearchBuildMu.Lock()
	defer idx.codeSearchBuildMu.Unlock()

	graph := idx.symbolGraphSnapshot()
	idx.mu.Lock()
	if idx.codeSearchBuilt {
		out := append([]codeSearchFile(nil), idx.codeSearchFiles...)
		idx.mu.Unlock()
		return out
	}
	sidecarPath := idx.codeSearchSidecarPath
	idx.mu.Unlock()

	// Try the persisted sidecar before paying to retokenize every searchable
	// file from source. loadCodeSearchSidecar validates schema version and
	// per-file content hashes itself (see codeSearchSidecarMatches) and only
	// reports success once codeSearchFiles/Postings/TermDocFreq are fully
	// populated, so a stale or missing sidecar falls straight through to the
	// from-scratch build below with no special-casing needed here.
	if sidecarPath != "" && loadCodeSearchSidecar(idx, sidecarPath) {
		idx.mu.Lock()
		out := append([]codeSearchFile(nil), idx.codeSearchFiles...)
		idx.mu.Unlock()
		return out
	}

	idx.mu.Lock()
	fileMetas := make([]File, 0, len(idx.Files))
	for _, meta := range idx.Files {
		if searchableCodeLanguage(meta.Language) {
			fileMetas = append(fileMetas, meta)
		}
	}
	symbolsByFile := make(map[string][]CGPSymbol, len(fileMetas))
	for _, id := range graph.OrderedSymbolIDs {
		if symbol, ok := graph.Symbols[id]; ok {
			symbolsByFile[symbol.File] = append(symbolsByFile[symbol.File], symbol)
		}
	}
	for file, symbols := range symbolsByFile {
		sort.Slice(symbols, func(i, j int) bool {
			if symbols[i].StartLine != symbols[j].StartLine {
				return symbols[i].StartLine < symbols[j].StartLine
			}
			if symbols[i].EndLine != symbols[j].EndLine {
				return symbols[i].EndLine > symbols[j].EndLine
			}
			return symbols[i].ID < symbols[j].ID
		})
		symbolsByFile[file] = symbols
	}
	root := idx.Repo.Root
	idx.mu.Unlock()

	sort.Slice(fileMetas, func(i, j int) bool { return fileMetas[i].Path < fileMetas[j].Path })
	built := buildCodeSearchFiles(fileMetas, symbolsByFile, root)
	internCodeSearchFiles(built)
	postings := buildCodeSearchPostings(built)
	docFreq := buildCodeSearchTermDocFreq(built)

	idx.mu.Lock()
	idx.codeSearchFiles = built
	idx.codeSearchPostings = postings
	idx.codeSearchTermDocFreq = docFreq
	idx.codeSearchBuilt = true
	out := append([]codeSearchFile(nil), idx.codeSearchFiles...)
	idx.mu.Unlock()
	return out
}

func buildCodeSearchFiles(fileMetas []File, symbolsByFile map[string][]CGPSymbol, root string) []codeSearchFile {
	type result struct {
		file codeSearchFile
		ok   bool
	}
	if len(fileMetas) == 0 {
		return nil
	}
	results := make([]result, len(fileMetas))
	workers := runtime.GOMAXPROCS(0) * 2
	if workers < 1 {
		workers = 1
	}
	if workers > len(fileMetas) {
		workers = len(fileMetas)
	}
	jobs := make(chan int, len(fileMetas))
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := range jobs {
				meta := fileMetas[i]
				// Symbols spanning many lines (a class, a long function)
				// previously had searchTextTokenSet(name+kind+signature)
				// recomputed once per line in their span. Caching by symbol ID
				// keeps that O(unique symbols in this file). The cache is
				// intentionally per file so the parallel build does not trade
				// CPU time for lock contention.
				symbolTokenCache := map[string][]string{}
				cached, ok := buildCodeSearchFile(meta, symbolsByFile[meta.Path], root, symbolTokenCache)
				results[i] = result{file: cached, ok: ok}
			}
		}()
	}
	for i := range fileMetas {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	built := make([]codeSearchFile, 0, len(fileMetas))
	for _, result := range results {
		if result.ok {
			result.file.postingID = uint32(len(built) + 1)
			built = append(built, result.file)
		}
	}
	return built
}

// internCodeSearchFiles canonicalizes repeated token strings across the whole
// search cache. Source tokenization naturally creates a new string header and
// backing allocation for common words in every line where they occur; on
// large repositories those duplicate strings account for a substantial part
// of the warmed server heap. The inverted postings and scoring logic compare
// token values, so sharing one immutable string per distinct token changes no
// behavior while allowing the duplicates to be collected.
func internCodeSearchFiles(files []codeSearchFile) {
	interner := make(map[string]string, maxInt(len(files)*8, 64))
	for i := range files {
		internCodeSearchFile(&files[i], interner)
	}
}

func internCodeSearchFile(file *codeSearchFile, interner map[string]string) {
	intern := func(token string) string {
		if canonical, ok := interner[token]; ok {
			return canonical
		}
		interner[token] = token
		return token
	}
	internSet := func(tokens map[string]bool) map[string]bool {
		out := make(map[string]bool, len(tokens))
		for token := range tokens {
			out[intern(token)] = true
		}
		return out
	}
	file.pathTokens = internSet(file.pathTokens)
	file.baseTokens = internSet(file.baseTokens)
	allTokens := make(map[string]bool, file.allTokens.count())
	file.allTokens.forEach(func(token string) {
		allTokens[intern(token)] = true
	})
	mergeSearchTokens(allTokens, file.pathTokens)
	mergeSearchTokens(allTokens, file.baseTokens)
	for i := range file.lines {
		line := &file.lines[i]
		line.tokens = compactTokenSet(intern(string(line.tokens)))
		line.symbolText = compactTokenSet(intern(string(line.symbolText)))
		line.tokens.forEach(func(token string) {
			allTokens[intern(token)] = true
		})
		line.symbolText.forEach(func(token string) {
			allTokens[intern(token)] = true
		})
	}
	file.allTokens = packCompactTokenSet(mapKeys(allTokens))
}

// buildCodeSearchFile tokenizes a single file into a codeSearchFile entry.
// Shared by the full ensureCodeSearchIndex build and the incremental
// updateCodeSearchIndexForFiles path so a single-file edit re-tokenizes only
// that file, not the whole repo.
func buildCodeSearchFile(meta File, symbols []CGPSymbol, root string, symbolTokenCache map[string][]string) (codeSearchFile, bool) {
	if !searchableCodeLanguage(meta.Language) {
		return codeSearchFile{}, false
	}
	content, err := os.ReadFile(filepath.Join(root, meta.Path))
	if err != nil {
		return codeSearchFile{}, false
	}
	if uint64(len(content)) > math.MaxUint32 {
		return codeSearchFile{}, false
	}
	source := string(content)
	lines := strings.SplitAfter(source, "\n")
	symbolsByLine := searchSymbolsByLine(symbols)
	allTokens := map[string]bool{}
	cached := codeSearchFile{
		file:       meta.Path,
		language:   meta.Language,
		pathTokens: searchTextTokenSet(meta.Path),
		baseTokens: searchTextTokenSet(strings.TrimSuffix(filepath.Base(meta.Path), filepath.Ext(meta.Path))),
		lines:      make([]codeSearchLine, 0, len(lines)),
		source:     source,
	}
	mergeSearchTokens(allTokens, cached.pathTokens)
	mergeSearchTokens(allTokens, cached.baseTokens)
	summaryIndexes := map[*CGPSymbolSummary]uint32{}
	offset := 0
	for i, text := range lines {
		lineNo := i + 1
		syms := symbolsByLine[lineNo]
		lineTokens := searchTextTokenSlice(text)
		symbolText := searchSymbolTextSliceCached(syms, symbolTokenCache)
		mergeTokensFromSlice(allTokens, lineTokens)
		mergeTokensFromSlice(allTokens, symbolText)
		cached.tokenCount += len(lineTokens) + len(symbolText)
		symbolStart, symbolCount := appendCodeSearchLineSymbols(&cached, syms, summaryIndexes)
		cached.lines = append(cached.lines, codeSearchLine{
			textStart:   uint32(offset),
			textEnd:     uint32(offset + len(text)),
			tokenBloom:  compactTokenBloom(lineTokens),
			symbolBloom: compactTokenBloom(symbolText),
			tokens:      packCompactTokenSet(lineTokens),
			symbolText:  packCompactTokenSet(symbolText),
			symbolStart: symbolStart,
			symbolCount: symbolCount,
		})
		offset += len(text)
	}
	cached.allTokens = packCompactTokenSet(mapKeys(allTokens))
	return cached, true
}

// updateCodeSearchIndexForFiles incrementally refreshes the search cache for
// a set of changed/removed files instead of invalidating the whole repo's
// cache. Watch-mode rebakes touch one (or a handful of) files at a time —
// previously every such edit called invalidateCodeSearchIndex(), which threw
// away every other file's already-tokenized entry and forced the next
// search-code/inspect-flow/repo_map call to re-read and re-tokenize the
// entire repo from disk. In a long-running `mamari serve --watch` session
// with an agent editing files throughout, that meant paying the full
// repo-wide rebuild cost on every single edit-then-query cycle, not once per
// session. If the cache hasn't been built yet, this is a no-op — the next
// query builds it fresh in full, which is already correct and cheap relative
// to doing it here speculatively.
func (idx *Index) updateCodeSearchIndexForFiles(changed, removed []string) {
	idx.mu.Lock()
	if !idx.codeSearchBuilt {
		idx.mu.Unlock()
		return
	}
	root := idx.Repo.Root
	fileMetas := make(map[string]File, len(changed))
	symbolsByFile := make(map[string][]CGPSymbol, len(changed))
	for _, rel := range changed {
		if meta, ok := idx.Files[rel]; ok {
			fileMetas[rel] = meta
			symbolsByFile[rel] = append([]CGPSymbol(nil), idx.symbolsByFile[rel]...)
		}
	}
	existing := idx.codeSearchFiles
	idx.mu.Unlock()

	toRemove := make(map[string]bool, len(changed)+len(removed))
	for _, rel := range removed {
		toRemove[rel] = true
	}
	for _, rel := range changed {
		toRemove[rel] = true // dropped below and re-added if it still rebuilds successfully
	}

	rebuilt := make([]codeSearchFile, 0, len(existing)+len(changed))
	oldByPath := make(map[string]codeSearchFile, len(changed)+len(removed))
	var nextPostingID uint32
	for _, cf := range existing {
		if cf.postingID > nextPostingID {
			nextPostingID = cf.postingID
		}
		if toRemove[cf.file] {
			oldByPath[cf.file] = cf
		} else {
			rebuilt = append(rebuilt, cf)
		}
	}
	symbolTokenCache := map[string][]string{}
	var changedBuilt []codeSearchFile
	for _, rel := range changed {
		meta, ok := fileMetas[rel]
		if !ok {
			continue
		}
		cached, ok := buildCodeSearchFile(meta, symbolsByFile[rel], root, symbolTokenCache)
		if !ok {
			continue
		}
		internCodeSearchFile(&cached, map[string]string{})
		nextPostingID++
		cached.postingID = nextPostingID
		rebuilt = append(rebuilt, cached)
		changedBuilt = append(changedBuilt, cached)
	}
	sort.Slice(rebuilt, func(i, j int) bool { return rebuilt[i].file < rebuilt[j].file })

	idx.mu.Lock()
	// Only commit if nothing else rebuilt/invalidated the cache while we were
	// reading files off the lock (e.g. a concurrent full ensureCodeSearchIndex
	// triggered by a query racing this rebake) — otherwise we'd resurrect a
	// stale snapshot over a newer one.
	if idx.codeSearchBuilt {
		idx.codeSearchFiles = rebuilt
		if idx.codeSearchPostings == nil || idx.codeSearchTermDocFreq == nil {
			idx.codeSearchPostings = buildCodeSearchPostings(rebuilt)
			idx.codeSearchTermDocFreq = buildCodeSearchTermDocFreq(rebuilt)
		} else {
			postings := cloneCodeSearchPostingsMap(idx.codeSearchPostings)
			docFreq := cloneIntMap(idx.codeSearchTermDocFreq)
			for _, old := range oldByPath {
				removeCodeSearchFileFromIndexes(postings, docFreq, old)
			}
			for _, cached := range changedBuilt {
				addCodeSearchFileToIndexes(postings, docFreq, cached)
			}
			idx.codeSearchPostings = postings
			idx.codeSearchTermDocFreq = docFreq
		}
	}
	idx.mu.Unlock()
}

func (idx *Index) invalidateCodeSearchIndex() {
	idx.mu.Lock()
	idx.codeSearchFiles = nil
	idx.codeSearchPostings = nil
	idx.codeSearchTermDocFreq = nil
	idx.codeSearchBuilt = false
	idx.semanticIndex = nil
	idx.mu.Unlock()
}

// publishedQueryIndex is an immutable snapshot of exactly what SearchCode's
// hot path reads. Every field here must only ever be replaced wholesale
// (never mutated in place) by whatever builds it, since a published
// snapshot's slices/maps may be concurrently visible to readers that loaded
// it before a newer one replaced it.
type publishedQueryIndex struct {
	generation      uint64
	prefixNames     map[string]bool
	codeSearchFiles []codeSearchFile
	postings        codeSearchPostingIndex
	termDocFreq     map[string]int
}

// publishQuerySnapshot (re)builds the code-search cache from the current live
// state — paying whatever cost that takes on the CALLER's goroutine — then
// atomically publishes the result for
// lock-free reads via idx.published. Called by the watcher (see watch.go),
// never by a query path itself: that's what keeps the cost off any
// concurrent query's critical path. Safe to call from a single goroutine at
// a time (the watcher's rebake loop is already sequential; this does not
// add its own synchronization against concurrent callers of itself).
func (idx *Index) publishQuerySnapshot(changed []string) {
	files := idx.ensureCodeSearchIndex()

	idx.mu.Lock()
	prefixNames := prefixNamesFrom(idx.Prefixes, idx.Files)
	postings := idx.codeSearchPostings
	termDocFreq := idx.codeSearchTermDocFreq
	idx.mu.Unlock()

	previous := idx.published.Load()
	generation := uint64(1)
	if previous != nil {
		generation = previous.generation + 1
	}
	next := &publishedQueryIndex{
		generation:      generation,
		prefixNames:     prefixNames,
		codeSearchFiles: files,
		postings:        postings,
		termDocFreq:     termDocFreq,
	}

	// Keep the cache transition and pointer publication atomic with respect to
	// cache readers/storers. A query that started on the previous snapshot can
	// either publish before this section (and be validated here), or observe
	// the new pointer and decline to store its now-old result.
	idx.searchResultsMu.Lock()
	idx.advanceSearchCodeResults(previous, next, changed)
	idx.published.Store(next)
	idx.searchResultsMu.Unlock()
}

func newSearchCodeCacheKey(query string, opts SearchCodeOptions, mode string) searchCodeCacheKey {
	return searchCodeCacheKey{
		query: query, mode: mode, limit: opts.Limit, budgetTokens: opts.BudgetTokens,
		contextLines: opts.ContextLines, sourceOnly: opts.SourceOnly,
		includeTests: opts.IncludeTests, includeStories: opts.IncludeStories,
		exactFirst: opts.ExactFirst, preferDefinitions: opts.PreferDefinitions,
		preferUsages: opts.PreferUsages, symbolDetail: opts.SymbolDetail,
	}
}

func (idx *Index) loadSearchCodeResult(key searchCodeCacheKey, generation uint64) (SearchCodeResponse, bool) {
	idx.searchResultsMu.Lock()
	entry, ok := idx.searchResults[key]
	idx.searchResultsMu.Unlock()
	if !ok || entry.generation != generation {
		return SearchCodeResponse{}, false
	}
	return cloneSearchCodeResponse(entry.response), true
}

func (idx *Index) storeSearchCodeResult(key searchCodeCacheKey, snap *publishedQueryIndex, terms []string, phrases []exactPhrase, resp SearchCodeResponse) {
	idx.searchResultsMu.Lock()
	defer idx.searchResultsMu.Unlock()
	if idx.published.Load() != snap {
		return
	}
	if idx.searchResults == nil {
		idx.searchResults = make(map[searchCodeCacheKey]searchCodeCacheEntry)
	}
	if len(idx.searchResults) >= maxSearchCodeCacheEntries {
		clear(idx.searchResults)
	}
	idx.searchResults[key] = searchCodeCacheEntry{
		generation: snap.generation,
		terms:      append([]string(nil), terms...), phrases: append([]exactPhrase(nil), phrases...),
		response: cloneSearchCodeResponse(resp),
	}
}

func (idx *Index) advanceSearchCodeResults(previous, next *publishedQueryIndex, changed []string) {
	if len(idx.searchResults) == 0 {
		return
	}
	if previous == nil {
		clear(idx.searchResults)
		return
	}
	oldFiles := codeSearchFilesByPath(previous.codeSearchFiles, changed)
	newFiles := codeSearchFilesByPath(next.codeSearchFiles, changed)
	for key, entry := range idx.searchResults {
		if entry.generation != previous.generation || searchCodeEntryAffected(key, entry, changed, oldFiles, newFiles) {
			delete(idx.searchResults, key)
			continue
		}
		entry.generation = next.generation
		idx.searchResults[key] = entry
	}
}

func codeSearchFilesByPath(files []codeSearchFile, paths []string) map[string]codeSearchFile {
	wanted := make(map[string]bool, len(paths))
	for _, path := range paths {
		wanted[path] = true
	}
	out := make(map[string]codeSearchFile, len(paths))
	for _, file := range files {
		if wanted[file.file] {
			out[file.file] = file
		}
	}
	return out
}

func searchCodeEntryAffected(key searchCodeCacheKey, entry searchCodeCacheEntry, changed []string, oldFiles, newFiles map[string]codeSearchFile) bool {
	for _, path := range changed {
		if !searchCodeFilesEquivalentForEntry(oldFiles[path], newFiles[path], key, entry) {
			return true
		}
	}
	return false
}

type searchCodeFileInfluence struct {
	termPresence []bool
	pathMatches  []bool
	baseMatches  []bool
	candidates   []searchCodeLineInfluence
}

type searchCodeLineInfluence struct {
	number     int
	text       string
	symbols    []*CGPSymbolSummary
	symbolText compactTokenSet
	context    []string
}

func searchCodeFilesEquivalentForEntry(oldFile, newFile codeSearchFile, key searchCodeCacheKey, entry searchCodeCacheEntry) bool {
	oldInfluence := searchCodeInfluenceForFile(oldFile, key, entry)
	newInfluence := searchCodeInfluenceForFile(newFile, key, entry)
	return reflect.DeepEqual(oldInfluence, newInfluence)
}

func searchCodeInfluenceForFile(file codeSearchFile, key searchCodeCacheKey, entry searchCodeCacheEntry) searchCodeFileInfluence {
	if file.file == "" {
		return searchCodeFileInfluence{}
	}
	influence := searchCodeFileInfluence{
		termPresence: make([]bool, len(entry.terms)),
		pathMatches:  make([]bool, len(entry.terms)),
		baseMatches:  make([]bool, len(entry.terms)),
	}
	for i, term := range entry.terms {
		influence.termPresence[i] = file.allTokens.contains(term)
		influence.pathMatches[i] = file.pathTokens[term]
		influence.baseMatches[i] = file.baseTokens[term]
	}
	for lineIdx, line := range file.lines {
		matched := false
		for _, term := range entry.terms {
			if line.tokens.contains(term) || line.symbolText.contains(term) {
				matched = true
				break
			}
		}
		if !matched {
			if exact, _ := matchExactPhrases(file.lineText(line), line.tokenBloom, entry.phrases); len(exact) > 0 {
				matched = true
			}
		}
		if !matched {
			continue
		}
		start := lineIdx - key.contextLines
		if start < 0 {
			start = 0
		}
		end := lineIdx + key.contextLines + 1
		if end > len(file.lines) {
			end = len(file.lines)
		}
		context := make([]string, 0, end-start)
		for _, contextLine := range file.lines[start:end] {
			context = append(context, file.lineText(contextLine))
		}
		influence.candidates = append(influence.candidates, searchCodeLineInfluence{
			number: lineIdx + 1, text: file.lineText(line),
			symbols: file.lineSymbols(line, nil), symbolText: line.symbolText, context: context,
		})
	}
	return influence
}

func cloneSearchCodeResponse(in SearchCodeResponse) SearchCodeResponse {
	out := in
	out.ExactPhrases = append([]SearchCodeExactPhrase(nil), in.ExactPhrases...)
	out.Warnings = append([]string(nil), in.Warnings...)
	out.Hits = make([]SearchCodeHit, len(in.Hits))
	for i, hit := range in.Hits {
		out.Hits[i] = hit
		out.Hits[i].MatchedTerms = append([]string(nil), hit.MatchedTerms...)
		out.Hits[i].MatchedExact = append([]string(nil), hit.MatchedExact...)
		out.Hits[i].Symbols = cloneCGPSymbolSummaries(hit.Symbols)
		out.Hits[i].BlastRadius = append([]SearchCodeBlast(nil), hit.BlastRadius...)
	}
	return out
}

// dereferenceSymbolSummaries converts the internal, memory-shared
// []*CGPSymbolSummary representation (see codeSearchLine.symbols) back into
// the plain []CGPSymbolSummary value type every public response type
// (SearchCodeHit.Symbols, etc.) already uses — the pointer-sharing
// optimization is strictly internal to the long-lived search cache; only a
// handful of matched hits ever cross this boundary per query (not every
// line in the repo), so copying back to values here is cheap.
func dereferenceSymbolSummaries(in []*CGPSymbolSummary) []CGPSymbolSummary {
	if len(in) == 0 {
		return nil
	}
	out := make([]CGPSymbolSummary, len(in))
	for i, s := range in {
		out[i] = *s
	}
	return out
}

// internSymbolSummaries is dereferenceSymbolSummaries' inverse, used when
// loading the persisted search sidecar (search_persist.go): each line's
// symbols were saved as plain values (the on-disk format stays
// byte-compatible and simple), so loading naively would reallocate one
// fresh *CGPSymbolSummary per line again, exactly the duplication the
// pointer-sharing fix exists to avoid. cache is keyed by symbol ID and
// shared across every line in one file's load, so a symbol spanning many
// lines gets exactly one *CGPSymbolSummary regardless of how many lines'
// worth of (byte-identical) values were persisted for it.
func internSymbolSummaries(in []CGPSymbolSummary, cache map[string]*CGPSymbolSummary) []*CGPSymbolSummary {
	if len(in) == 0 {
		return nil
	}
	out := make([]*CGPSymbolSummary, len(in))
	for i, s := range in {
		if cached, ok := cache[s.ID]; ok {
			out[i] = cached
			continue
		}
		sCopy := s
		sCopy.searchNameTokens = packCompactTokenSet(searchTextTokenSlice(sCopy.Name))
		cache[s.ID] = &sCopy
		out[i] = &sCopy
	}
	return out
}

func cloneCGPSymbolSummaries(in []CGPSymbolSummary) []CGPSymbolSummary {
	out := make([]CGPSymbolSummary, len(in))
	for i, symbol := range in {
		out[i] = symbol
		out[i].ReturnTypes = append([]string(nil), symbol.ReturnTypes...)
		out[i].Lines = append([]int(nil), symbol.Lines...)
		out[i].NamesPreview = append([]string(nil), symbol.NamesPreview...)
	}
	return out
}

func searchableCodeLanguage(lang string) bool {
	switch lang {
	case "javascript", "typescript", "vue", "python", "ttl", "go", "java", "csharp", "rust", "ruby", "php", "kotlin", "swift", "r", "julia", "zig", "ocaml", "hcl", "yaml", "dockerfile":
		return true
	default:
		return false
	}
}

func buildSearchCodeHit(files []codeSearchFile, cand searchCodeCandidate, terms []string, start, end, remainingTokens int) (SearchCodeHit, bool) {
	var file *codeSearchFile
	for i := range files {
		if files[i].file == cand.file {
			file = &files[i]
			break
		}
	}
	if file == nil {
		return SearchCodeHit{}, false
	}
	lines := file.lines
	if start < 1 {
		start = 1
	}
	if end > len(lines) {
		end = len(lines)
	}
	if end < start {
		return SearchCodeHit{}, false
	}
	text := joinSearchLines(*file, lines[start-1:end])
	estimated := EstimateTokens(text)
	if estimated > remainingTokens {
		if cand.line < 1 || cand.line > len(lines) {
			return SearchCodeHit{}, false
		}
		start, end = cand.line, cand.line
		text = file.lineText(lines[cand.line-1])
		estimated = EstimateTokens(text)
		if estimated > remainingTokens {
			return SearchCodeHit{}, false
		}
	}
	return SearchCodeHit{
		File:            cand.file,
		StartLine:       start,
		EndLine:         end,
		FocusLine:       cand.line,
		Score:           cand.score,
		EstimatedTokens: estimated,
		MatchedTerms:    termsFromMask(cand.matchedMask, terms),
		MatchedExact:    cand.matchedExact,
		Symbols:         dereferenceSymbolSummaries(file.lineSymbols(lines[cand.line-1], nil)),
		Text:            text,
	}, true
}

func joinSearchLines(file codeSearchFile, lines []codeSearchLine) string {
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(file.lineText(line))
	}
	return b.String()
}

func searchSymbolsByLine(symbols []CGPSymbol) map[int][]*CGPSymbolSummary {
	out := map[int][]*CGPSymbolSummary{}
	// summarizeSymbol(sym) is identical for every line within sym's own
	// span — memoized by symbol ID (per file, like searchSymbolTextSliceCached
	// one level up) so it's computed once per symbol, not once per line,
	// and every covered line shares the one resulting pointer.
	summaryCache := make(map[string]*CGPSymbolSummary, len(symbols))
	for _, sym := range symbols {
		if sym.Kind == "file" {
			continue
		}
		end := sym.EndLine
		if end == 0 || end < sym.StartLine {
			end = sym.StartLine
		}
		summary := summaryCache[sym.ID]
		if summary == nil {
			s := summarizeSymbol(sym)
			s.searchNameTokens = packCompactTokenSet(searchTextTokenSlice(sym.Name))
			summary = &s
			summaryCache[sym.ID] = summary
		}
		for line := sym.StartLine; line <= end; line++ {
			if len(out[line]) >= 3 {
				continue
			}
			out[line] = append(out[line], summary)
		}
	}
	return out
}

// searchSymbolTextSliceCached is the per-line analogue of searchTextTokenSet
// for the (usually 0-3) symbols covering a line, with the per-symbol token
// set memoized in `cache` (keyed by symbol ID). A symbol spanning N lines (a
// class, a long function) would otherwise have its name/kind/signature
// retokenized N times — once per line it covers — for an identical result
// each time; on a several-hundred-line class that's hundreds of redundant
// regex/stemming passes plus a fresh map allocation each time. Returns a
// sorted, deduped slice rather than a map: these per-line sets are tiny (a
// handful of tokens) and built for every line in the repo, so avoiding the
// map's hash-table overhead matters more than O(log n) lookup elegance.
func searchSymbolTextSliceCached(symbols []*CGPSymbolSummary, cache map[string][]string) []string {
	if len(symbols) == 0 {
		return nil
	}
	if len(symbols) == 1 {
		return symbolTokensCached(symbols[0], cache)
	}
	var out []string
	for _, sym := range symbols {
		out = append(out, symbolTokensCached(sym, cache)...)
	}
	return sortDedupTokens(out)
}

func symbolTokensCached(sym *CGPSymbolSummary, cache map[string][]string) []string {
	if tokens, ok := cache[sym.ID]; ok {
		return tokens
	}
	tokens := searchTextTokenSlice(sym.Name + " " + sym.Kind + " " + sym.Signature)
	cache[sym.ID] = tokens
	return tokens
}

func mergeSearchTokens(dst, src map[string]bool) {
	for token := range src {
		dst[token] = true
	}
}

func mergeTokensFromSlice(dst map[string]bool, src []string) {
	for _, token := range src {
		dst[token] = true
	}
}

// sortDedupTokens sorts and deduplicates in place, returning the trimmed
// slice. Used instead of a map-based dedup to avoid trading one allocation
// problem for another on these small, hot, per-line token sets.
func sortDedupTokens(tokens []string) []string {
	if len(tokens) < 2 {
		return tokens
	}
	sort.Strings(tokens)
	j := 0
	for i := 1; i < len(tokens); i++ {
		if tokens[i] != tokens[j] {
			j++
			tokens[j] = tokens[i]
		}
	}
	return tokens[:j+1]
}

func searchTermWeights(terms []string, files []codeSearchFile) map[string]int {
	return searchTermWeightsFromDocFreq(terms, len(files), buildCodeSearchTermDocFreq(files))
}

func searchTermWeightsFromDocFreq(terms []string, fileCount int, docFreq map[string]int) map[string]int {
	out := map[string]int{}
	if len(terms) == 0 || fileCount == 0 {
		return out
	}
	total := float64(fileCount + 1)
	for _, term := range terms {
		df := docFreq[term]
		if df == 0 {
			continue
		}
		// IDF-style boost: rare query terms carry more ranking evidence than
		// boilerplate words that appear in many files. The cap keeps exact
		// phrase boosts and definition preferences dominant when they apply.
		weight := int(math.Log(total/float64(df+1)) * 90)
		if weight < 0 {
			weight = 0
		}
		if weight > 420 {
			weight = 420
		}
		out[term] = weight
	}
	return out
}

func weightedSearchTermScore(matches []string, weights map[string]int) int {
	score := 0
	for _, term := range matches {
		score += weights[term]
	}
	return score
}

func queryMentionsSupportFiles(query string) bool {
	for _, term := range []string{"benchmark", "bench", "script", "tool", "test", "spec", "fixture", "golden", "example", "examples", "demo", "sample"} {
		if strings.Contains(query, term) {
			return true
		}
	}
	return false
}

func classifySearchCodeIntent(terms []string) searchCodeIntent {
	cues := 0
	for _, term := range terms {
		if lifecycleQueryCueTerms[term] {
			cues++
		}
	}
	return searchCodeIntent{lifecycle: cues >= 2}
}

func lifecyclePathBoost(file codeSearchFile) int {
	matches := 0
	for term := range lifecyclePathTerms {
		if file.pathTokens[term] || file.baseTokens[term] {
			matches++
		}
	}
	switch {
	case matches >= 2:
		return 180
	case matches == 1:
		return 90
	default:
		return 0
	}
}

func lifecycleLineBoost(file codeSearchFile, lineMask, symbolMask uint64, terms []string, definition bool) int {
	lineSignals := lifecycleSignalCount(lineMask, terms)
	symbolSignals := lifecycleSignalCount(symbolMask, terms)
	boost := 0
	if symbolSignals >= 2 {
		boost += 420 + symbolSignals*120
		if definition {
			boost += 520
		}
	} else if definition && symbolSignals == 1 && lineSignals >= 2 {
		boost += 360
	}
	if definition && symbolSignals > 0 && lifecyclePathBoost(file) > 0 {
		boost += 160
	}
	if !definition && symbolSignals == 0 && lineSignals <= 2 {
		boost -= 180
	}
	if boost > 1400 {
		return 1400
	}
	return boost
}

func lifecycleSignalCount(mask uint64, terms []string) int {
	count := 0
	for i, term := range terms {
		if i >= 64 {
			break
		}
		if mask&(uint64(1)<<uint(i)) == 0 {
			continue
		}
		if lifecycleSymbolTerms[term] {
			count++
		}
	}
	return count
}

func searchSupportFilePenalty(file string, includeSupport bool) int {
	if includeSupport {
		return 0
	}
	file = filepath.ToSlash(strings.ToLower(file))
	switch {
	case strings.HasPrefix(file, "scripts/"):
		return 900
	case strings.Contains(file, "/scripts/"):
		return 800
	case isExampleOrDemoPath(file):
		return 3000
	case strings.Contains(file, "benchmark"), strings.Contains(file, "fixture"), strings.Contains(file, "testdata/"):
		return 650
	case isTestPath(file):
		return 500
	default:
		return 0
	}
}

func isExampleOrDemoPath(file string) bool {
	file = filepath.ToSlash(strings.ToLower(file))
	return strings.HasPrefix(file, "examples/") ||
		strings.Contains(file, "/examples/") ||
		strings.HasPrefix(file, "example/") ||
		strings.Contains(file, "/example/") ||
		strings.HasPrefix(file, "demo/") ||
		strings.Contains(file, "/demo/") ||
		strings.HasPrefix(file, "samples/") ||
		strings.Contains(file, "/samples/")
}

func exactSymbolNameSearchBoost(symbols []*CGPSymbolSummary, symbolTokenCount, symbolMatchCount int, queryLower string, queryTerms []string) int {
	boost := 0
	for _, sym := range symbols {
		if sym.Name == "" {
			continue
		}
		nameLower := strings.ToLower(sym.Name)
		if strings.Contains(queryLower, nameLower) {
			boost += 520
			continue
		}
		if sym.searchNameTokens.count() >= 2 && compactSetContainedInTerms(sym.searchNameTokens, queryTerms) {
			// Natural-language questions normally spell TraceSymbol as
			// "trace symbol" and full_dispatch_request as "full dispatch
			// request". Treat that as exact identity evidence without
			// granting the same boost to generic one-token names.
			boost += 520
		}
	}
	if symbolMatchCount > 0 {
		boost += symbolMatchCount * 70
		if symbolTokenCount > 0 && symbolMatchCount >= symbolTokenCount/2 && symbolMatchCount >= 2 {
			boost += 240
		}
	}
	if boost > 900 {
		return 900
	}
	return boost
}

func compactSetContainedInTerms(needles compactTokenSet, haystack []string) bool {
	contained := true
	needles.forEach(func(needle string) {
		if !contained {
			return
		}
		found := false
		for _, term := range haystack {
			if term == needle {
				found = true
				break
			}
		}
		if !found {
			contained = false
		}
	})
	return contained
}

func weightedSearchTermScoreMask(mask uint64, terms []string, weights map[string]int) int {
	score := 0
	for i, term := range terms {
		if i >= 64 {
			break
		}
		if mask&(uint64(1)<<uint(i)) != 0 {
			score += weights[term]
		}
	}
	return score
}

func searchFileCoverageBoostMask(fileMask, localMask uint64, terms []string, weights map[string]int) int {
	if fileMask == 0 {
		return 0
	}
	extraMask := fileMask &^ localMask
	boost := weightedSearchTermScoreMask(fileMask, terms, weights)/4 + weightedSearchTermScoreMask(extraMask, terms, weights)/2
	if boost > 500 {
		return 500
	}
	return boost
}

func rareTermClusterBoostMask(mask uint64, terms []string, weights map[string]int) int {
	rareCount := 0
	rareSum := 0
	for i, term := range terms {
		if i >= 64 {
			break
		}
		if mask&(uint64(1)<<uint(i)) == 0 {
			continue
		}
		weight := weights[term]
		if weight < 300 {
			continue
		}
		rareCount++
		rareSum += weight
	}
	if rareCount < 2 {
		return 0
	}
	boost := rareCount*rareCount*160 + rareSum/2
	if boost > 900 {
		return 900
	}
	return boost
}

func overlapsCovered(ranges [][2]int, start, end int) bool {
	for _, r := range ranges {
		if start <= r[1] && end >= r[0] {
			return true
		}
	}
	return false
}

func searchQueryTerms(query string) []string {
	raw := searchTokens(query)
	seen := map[string]bool{}
	var out []string
	rawSet := map[string]bool{}
	add := func(token string) {
		token = searchStem(token)
		if token == "" || searchStopWords[token] || seen[token] {
			return
		}
		seen[token] = true
		out = append(out, token)
	}
	for _, token := range raw {
		if searchStopWords[token] {
			continue
		}
		stemmed := searchStem(token)
		rawSet[stemmed] = true
		add(stemmed)
		for _, expanded := range searchQueryExpansions[stemmed] {
			add(expanded)
		}
	}
	if shouldExpandLifecycleQuery(rawSet) {
		for _, expanded := range lifecycleExpansionTerms {
			add(expanded)
		}
		if rawSet["exception"] || rawSet["error"] {
			add("error")
			add("exception")
		}
		if rawSet["dispatch"] {
			add("route")
			add("view")
		}
		if rawSet["teardown"] {
			add("cleanup")
			add("close")
			add("pop")
			add("push")
			add("signal")
		}
	}
	return out
}

func shouldExpandLifecycleQuery(rawSet map[string]bool) bool {
	cues := 0
	for term := range rawSet {
		if lifecycleQueryCueTerms[term] {
			cues++
		}
	}
	if cues < 2 {
		return false
	}
	return rawSet["lifecycle"] ||
		rawSet["request"] ||
		rawSet["response"] ||
		rawSet["teardown"] ||
		rawSet["before"] ||
		rawSet["after"]
}

func searchTextTokenSet(text string) map[string]bool {
	out := map[string]bool{}
	for _, token := range searchTokens(text) {
		if token == "" {
			continue
		}
		out[token] = true
		out[searchStem(token)] = true
	}
	return out
}

// searchTextTokenSlice is searchTextTokenSet's sorted-slice counterpart, used
// on the per-line hot path (see codeSearchLine) where a map's hash-table
// overhead dominates for sets this small, multiplied across every line in
// every indexed file.
func searchTextTokenSlice(text string) []string {
	raw := searchTokens(text)
	if len(raw) == 0 {
		return nil
	}
	out := make([]string, 0, len(raw)*2)
	for _, token := range raw {
		if token == "" {
			continue
		}
		out = append(out, token, searchStem(token))
	}
	return sortDedupTokens(out)
}

func searchTokens(text string) []string {
	text = splitCamelForSearch(text)
	raw := searchTokenRe.FindAllString(strings.ToLower(text), -1)
	var out []string
	for _, token := range raw {
		if token == "" {
			continue
		}
		out = append(out, token)
	}
	return out
}

func splitCamelForSearch(s string) string {
	var b strings.Builder
	var prev rune
	for i, r := range s {
		if i > 0 && ((isSearchLower(prev) && isSearchUpper(r)) || (isSearchLetter(prev) && isSearchDigit(r)) || (isSearchDigit(prev) && isSearchLetter(r))) {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
		prev = r
	}
	return b.String()
}

func isSearchLower(r rune) bool { return r >= 'a' && r <= 'z' }
func isSearchUpper(r rune) bool { return r >= 'A' && r <= 'Z' }
func isSearchDigit(r rune) bool { return r >= '0' && r <= '9' }
func isSearchLetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func searchStem(token string) string {
	if len(token) > 4 && strings.HasSuffix(token, "ies") {
		return strings.TrimSuffix(token, "ies") + "y"
	}
	if len(token) > 3 && strings.HasSuffix(token, "s") && !strings.HasSuffix(token, "ss") {
		return strings.TrimSuffix(token, "s")
	}
	return token
}

func matchedTermsInSet(terms []string, set map[string]bool) []string {
	var out []string
	for _, term := range terms {
		if set[term] {
			out = append(out, term)
		}
	}
	return out
}

func matchedTermsInCompactSet(terms []string, set compactTokenSet) []string {
	var out []string
	for _, term := range terms {
		if set.contains(term) {
			out = append(out, term)
		}
	}
	return out
}

func matchedTermsMaskInCompactSet(terms []string, termBlooms []uint64, set compactTokenSet, setBloom uint64) uint64 {
	if len(set) == 0 {
		return 0
	}
	var mask uint64
	for i, term := range terms {
		if i >= 64 {
			break
		}
		if i < len(termBlooms) && setBloom&termBlooms[i] != termBlooms[i] {
			continue
		}
		if set.contains(term) {
			mask |= uint64(1) << uint(i)
		}
	}
	return mask
}

func termMaskFromTerms(terms, matches []string) uint64 {
	if len(matches) == 0 {
		return 0
	}
	var mask uint64
	for i, term := range terms {
		if i >= 64 {
			break
		}
		if sliceContainsToken(matches, term) {
			mask |= uint64(1) << uint(i)
		}
	}
	return mask
}

// lowSignalSearchWeight is the IDF weight below which a matched term is too
// common to anchor a result set on its own — int(math.Log(4)*90) = 124, i.e.
// the term appears in roughly a quarter of indexed files or more. Corpus-size
// invariant because searchTermWeightsFromDocFreq is log(files/df)-shaped.
const lowSignalSearchWeight = 124

// searchRawTermMask returns the bitmask (over the expanded terms slice) of the
// terms the user actually typed — stemmed and stopword-filtered exactly like
// searchQueryTerms' raw pass, but without query expansions. The low-signal gate
// reasons only about these bits: expansion synonyms must neither count as
// "gibberish present" (they routinely have df==0 on small corpora) nor rescue a
// candidate's co-match count.
func searchRawTermMask(query string, terms []string) uint64 {
	rawSet := map[string]bool{}
	for _, token := range searchTokens(query) {
		if searchStopWords[token] {
			continue
		}
		if stemmed := searchStem(token); stemmed != "" && !searchStopWords[stemmed] {
			rawSet[stemmed] = true
		}
	}
	var mask uint64
	for i, term := range terms {
		if i >= 64 {
			break
		}
		if rawSet[term] {
			mask |= uint64(1) << uint(i)
		}
	}
	return mask
}

// gateLowSignalSearch reports whether the candidate set is gibberish-anchored
// junk: no exact-phrase evidence, at least one RAW query term that matched
// nothing in the corpus (df==0), no candidate that co-matched two or more
// distinct raw terms (path/symbol matches count, since those bits are folded
// into the match mask), and every raw term that did match being ultra-common.
// IDF already shapes *ranking*; this is the only place it gates *admission*,
// and only when all four conditions hold together — a single strong signal (a
// phrase, a raw co-match, a distinctive matched term) bypasses it.
func gateLowSignalSearch(terms []string, phrases []exactPhrase, termWeights map[string]int, rawMask, unionMask uint64, maxTermsPerCand int) bool {
	if len(phrases) > 0 || maxTermsPerCand > 1 {
		return false
	}
	missing := 0
	for _, term := range termsFromMask(rawMask, terms) {
		if _, ok := termWeights[term]; !ok {
			missing++
		}
	}
	if missing == 0 {
		return false
	}
	matched := termsFromMask(unionMask, terms)
	if len(matched) == 0 {
		return false
	}
	for _, term := range matched {
		if termWeights[term] >= lowSignalSearchWeight {
			return false
		}
	}
	return true
}

// lowConfidenceSearch reports whether a result set that passed the hard
// low-signal gate is still anchored on a minority of the typed query terms:
// at least one raw term is known-missing (the caller sees which via
// unmatchedRawTermsWarning) and even the best single candidate co-matched
// fewer than half of the raw terms. Exact phrases are structural evidence and
// exempt. Corpus-statistics-driven — no language-specific word lists.
func lowConfidenceSearch(terms []string, phrases []exactPhrase, rawMask uint64, maxTermsPerCand int) bool {
	if len(phrases) > 0 {
		return false
	}
	rawCount := bits.OnesCount64(rawMask)
	if rawCount < 2 {
		return false
	}
	return maxTermsPerCand*2 < rawCount
}

// lowSignalSearchWarning names the raw query terms that matched nothing so the
// caller can correct them instead of paging through junk.
func lowSignalSearchWarning(terms []string, rawMask uint64, termWeights map[string]int) string {
	var unmatched []string
	for _, term := range termsFromMask(rawMask, terms) {
		if _, ok := termWeights[term]; !ok {
			unmatched = append(unmatched, term)
		}
	}
	sort.Strings(unmatched)
	return "matched terms are too common to rank; unmatched terms: " + strings.Join(unmatched, ", ")
}

// unmatchedRawTermsWarning returns a warning naming user-typed terms that
// matched nothing in the corpus, or "" when every raw term matched. Used on
// the success path so a partial match is visibly partial.
func unmatchedRawTermsWarning(terms []string, rawMask uint64, termWeights map[string]int) string {
	var unmatched []string
	for _, term := range termsFromMask(rawMask, terms) {
		if _, ok := termWeights[term]; !ok {
			unmatched = append(unmatched, term)
		}
	}
	if len(unmatched) == 0 {
		return ""
	}
	sort.Strings(unmatched)
	return "terms with no match in this repo: " + strings.Join(unmatched, ", ")
}

func termsFromMask(mask uint64, terms []string) []string {
	if mask == 0 {
		return nil
	}
	out := make([]string, 0, bits.OnesCount64(mask))
	for i, term := range terms {
		if i >= 64 {
			break
		}
		if mask&(uint64(1)<<uint(i)) != 0 {
			out = append(out, term)
		}
	}
	sort.Strings(out)
	return out
}

func sliceContainsToken(set []string, token string) bool {
	i := sort.SearchStrings(set, token)
	return i < len(set) && set[i] == token
}

func mergeMatchedTerms(groups ...[]string) []string {
	var out []string
	for _, group := range groups {
		for _, term := range group {
			if term == "" {
				continue
			}
			out = append(out, term)
		}
	}
	return sortDedupTokens(out)
}

func containsAllTermsMask(mask uint64, termCount int) bool {
	if termCount == 0 || termCount > 64 {
		return false
	}
	want := uint64(1)<<uint(termCount) - 1
	return mask&want == want
}

// extractExactPhrases mines a natural-language query for substrings that
// should be matched verbatim against indexed source. Five signals are
// recognized, ordered roughly by how much they tell us the user already
// knows the exact name: (1) routes like /signing/:id/preview, (2) MIME
// types like application/pdf, (3) quoted phrases, (4) RDF predicate
// adjacencies like "sh in" -> "sh:in" or already-prefixed forms like
// "custom:hideIf", and (5) long camel/Pascal/snake identifiers. The
// returned phrases drive an additional scoring boost, not a replacement
// search — natural-language tokens still contribute so phrasing like
// "where is previewEnvelopeDocuments rendered" both finds the function
// and ranks render-site evidence high.
func extractExactPhrases(query string, prefixNames map[string]bool) []exactPhrase {
	if strings.TrimSpace(query) == "" {
		return nil
	}
	var out []exactPhrase
	seen := map[string]bool{}
	add := func(literal, kind string, weight int) {
		literal = strings.TrimSpace(literal)
		if literal == "" {
			return
		}
		key := kind + "|" + literal
		if seen[key] {
			return
		}
		seen[key] = true
		ph := exactPhrase{literal: literal, kind: kind, weight: weight}
		// The interior-token bloom gate is provably safe only for kinds
		// matched case-SENSITIVELY (identical bytes in the line reproduce the
		// literal's interior token boundaries exactly). predicate/mime kinds
		// match lowercased, where a differently-cased occurrence (NODESHAPE
		// vs NodeShape) can merge camel-split tokens and break the invariant,
		// so those keep interiorBloom 0 and are never bloom-skipped.
		if kind != "predicate" && kind != "mime" {
			ph.interiorBloom = phraseInteriorBloom(literal)
		}
		out = append(out, ph)
	}
	for _, m := range exactQuotedDoubleRe.FindAllStringSubmatch(query, -1) {
		add(m[1], "quoted", exactPhraseQuotedWeight)
	}
	for _, m := range exactQuotedSingleRe.FindAllStringSubmatch(query, -1) {
		add(m[1], "quoted", exactPhraseQuotedWeight)
	}
	for _, m := range exactQuotedBacktickRe.FindAllStringSubmatch(query, -1) {
		add(m[1], "quoted", exactPhraseQuotedWeight)
	}
	for _, idxs := range exactRouteRe.FindAllStringIndex(query, -1) {
		if len(idxs) != 2 {
			continue
		}
		m := query[idxs[0]:idxs[1]]
		if isLikelyRouteLiteralAt(query, idxs[0], m) {
			add(m, "route", exactPhraseRouteWeight)
		}
	}
	for _, m := range exactMimeRe.FindAllString(query, -1) {
		add(m, "mime", exactPhraseMimeWeight)
	}
	for _, phrase := range extractPredicatePhrases(query, prefixNames) {
		add(phrase, "predicate", exactPhrasePredicateWeight)
	}
	for _, m := range exactKebabIdentRe.FindAllStringSubmatch(query, -1) {
		if len(m) < 2 {
			continue
		}
		literal := normalizeExactClassLiteral(m[1])
		if commonCSSPropertyNames[strings.ToLower(literal)] {
			continue
		}
		if isLikelyExactKebabIdentifier(literal) {
			add(literal, "kebab-ident", exactPhraseIdentWeight)
		}
	}
	for _, m := range exactIdentifierRe.FindAllString(query, -1) {
		if isLikelyExactIdentifier(m) {
			add(m, "ident", exactPhraseIdentWeight)
		}
	}
	return out
}

// extractPredicatePhrases recovers RDF predicate hints both from explicit
// "prefix:local" forms and from natural-language adjacency patterns like
// "sh in", "dcterms identifier", "custom hideIf" that follow when a user
// types an RDF question without remembering the colon. The natural-language
// branch only fires when the leading token matches a known prefix in the
// repo's prefix table — otherwise we'd over-eagerly synthesize phrases for
// every two-word query. Local-name validity uses isLikelyLocalNameToken so
// adjacency to a stop word like "in", "is", "or" is still captured (those
// happen to be valid RDF locals like sh:in, sh:or).
func extractPredicatePhrases(query string, prefixNames map[string]bool) []string {
	if len(prefixNames) == 0 && !exactPredicateRe.MatchString(query) {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	push := func(literal string) {
		if literal == "" || seen[literal] {
			return
		}
		seen[literal] = true
		out = append(out, literal)
	}
	for _, m := range exactPredicateRe.FindAllStringSubmatch(query, -1) {
		prefix := m[1]
		local := m[2]
		if prefix == "" || local == "" {
			continue
		}
		if !looksLikePrefixToken(prefix) || !isLikelyLocalNameToken(local) {
			continue
		}
		push(prefix + ":" + local)
	}
	if len(prefixNames) == 0 {
		return out
	}
	tokens := splitQueryTokens(query)
	for i := 0; i+1 < len(tokens); i++ {
		raw := strings.Trim(tokens[i], "\"'`,.;:!?()[]{}")
		next := strings.Trim(tokens[i+1], "\"'`,.;:!?()[]{}")
		if raw == "" || next == "" {
			continue
		}
		if !prefixNames[strings.ToLower(raw)] {
			continue
		}
		if !isLikelyLocalNameToken(next) {
			continue
		}
		push(strings.ToLower(raw) + ":" + next)
	}
	return out
}

func splitQueryTokens(query string) []string {
	return strings.FieldsFunc(query, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', ',', ';', '!', '?', '(', ')', '[', ']', '{', '}':
			return true
		}
		return false
	})
}

func looksLikePrefixToken(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if !(isSearchLetter(r) || r == '_') {
				return false
			}
			continue
		}
		if !(isSearchLetter(r) || isSearchDigit(r) || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func isLikelyLocalNameToken(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if !(isSearchLetter(r) || r == '_') {
				return false
			}
			continue
		}
		if !(isSearchLetter(r) || isSearchDigit(r) || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func normalizeExactClassLiteral(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), ".")
}

func isLikelyExactKebabIdentifier(s string) bool {
	s = normalizeExactClassLiteral(s)
	if len(s) < 6 || !strings.Contains(s, "-") {
		return false
	}
	parts := strings.Split(s, "-")
	if len(parts) < 2 {
		return false
	}
	hasLetter := false
	for i, part := range parts {
		if part == "" {
			return false
		}
		for j, r := range part {
			switch {
			case isSearchLetter(r):
				hasLetter = true
			case isSearchDigit(r) || r == '_':
			default:
				return false
			}
			if i == 0 && j == 0 && !(isSearchLetter(r) || r == '_') {
				return false
			}
		}
	}
	return hasLetter
}

// isLikelyRouteLiteral filters out incidental slashes (file paths, URLs in
// thoughts) so only routes that look like "/foo", "/foo/bar", "/foo/:id"
// drive the route boost. We require at least one non-slash, non-dot
// character and reject single-segment slashes that could be pure division
// or word breaks.
func isLikelyRouteLiteral(s string) bool {
	return isLikelyRouteLiteralAt("", 0, s)
}

func isLikelyRouteLiteralAt(source string, start int, s string) bool {
	if !strings.HasPrefix(s, "/") || len(s) < 3 {
		return false
	}
	if start > 0 && start <= len(source) {
		prev := source[start-1]
		if (prev >= 'A' && prev <= 'Z') || (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9') {
			return false
		}
	}
	rest := s[1:]
	hasSegment := false
	for _, r := range rest {
		if r == '/' || r == '.' {
			continue
		}
		hasSegment = true
		break
	}
	return hasSegment
}

// isLikelyExactIdentifier promotes a token to an exact phrase when it looks
// like the kind of name a user types verbatim because they already know it:
// long enough to be unambiguous and either CamelCase, camelCase, or
// snake_case. Plain lowercase words are excluded — those go through normal
// token ranking.
func isLikelyExactIdentifier(s string) bool {
	if len(s) < exactPhraseMinIdentLen {
		return false
	}
	hasUpper, hasLower, hasDigit, hasUnderscore := false, false, false, false
	for _, r := range s {
		switch {
		case isSearchUpper(r):
			hasUpper = true
		case isSearchLower(r):
			hasLower = true
		case isSearchDigit(r):
			hasDigit = true
		case r == '_':
			hasUnderscore = true
		}
	}
	if hasUnderscore && (hasLower || hasUpper) && !strings.HasPrefix(s, "_") {
		return true
	}
	if !hasUpper {
		return false
	}
	segments := countCaseSegments(s)
	if segments < exactPhraseMinIdentSegs {
		return false
	}
	_ = hasDigit
	_ = hasLower
	return true
}

func countCaseSegments(s string) int {
	if s == "" {
		return 0
	}
	segments := 1
	var prev rune
	for i, r := range s {
		if i > 0 && isSearchUpper(r) && (isSearchLower(prev) || isSearchDigit(prev)) {
			segments++
		}
		prev = r
	}
	return segments
}

// matchExactPhrases scans a single line for verbatim occurrences of every
// extracted phrase and returns the matched literals plus the cumulative
// score boost. Predicate matches are case-sensitive on the local part to
// avoid conflating "sh:in" with "sh:IN" type tokens; the prefix is matched
// case-insensitively because users commonly type predicates in lowercase.
// Other phrase kinds use case-sensitive contains, since they are usually
// language-level identifiers, route paths, or MIME strings where casing is
// significant.
func matchExactPhrases(lineText string, lineBloom uint64, phrases []exactPhrase) ([]string, int) {
	if len(phrases) == 0 || lineText == "" {
		return nil, 0
	}
	var matched []string
	seen := map[string]bool{}
	score := 0
	lower := ""
	for _, ph := range phrases {
		// Bloom pre-filter: a substring hit requires every interior token of
		// the literal to appear in the line's token set (boundary tokens can
		// merge with neighboring characters in the line — see interiorBloom —
		// so they are excluded), meaning a line whose bloom lacks any interior
		// bit provably cannot Contains-match. This replaces the per-line
		// strings.Contains scan with two bit ops for most lines. interiorBloom
		// of 0 (short literals) skips the filter entirely.
		if ph.interiorBloom != 0 && lineBloom&ph.interiorBloom != ph.interiorBloom {
			continue
		}
		hit := false
		switch ph.kind {
		case "predicate":
			if lower == "" {
				lower = strings.ToLower(lineText)
			}
			if strings.Contains(lower, strings.ToLower(ph.literal)) {
				hit = true
			}
		case "mime":
			if lower == "" {
				lower = strings.ToLower(lineText)
			}
			if strings.Contains(lower, strings.ToLower(ph.literal)) {
				hit = true
			}
		default:
			if strings.Contains(lineText, ph.literal) {
				hit = true
			}
		}
		if !hit {
			continue
		}
		if !seen[ph.literal] {
			seen[ph.literal] = true
			matched = append(matched, ph.literal)
		}
		score += ph.weight
	}
	if len(matched) > 1 {
		score += len(matched) * len(matched) * 40
	}
	return matched, score
}

func exactPhraseSummaries(phrases []exactPhrase) []SearchCodeExactPhrase {
	if len(phrases) == 0 {
		return nil
	}
	out := make([]SearchCodeExactPhrase, 0, len(phrases))
	for _, ph := range phrases {
		out = append(out, SearchCodeExactPhrase{Literal: ph.literal, Kind: ph.kind})
	}
	return out
}

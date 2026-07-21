package mamari

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	defaultContextBudgetTokens = 1200
	defaultContextLines        = 8
)

type contextCandidate struct {
	file       string
	startLine  int
	endLine    int
	fullStart  int
	fullEnd    int
	focusLine  int
	focusLines []int
	kind       string
	reason     string
	priority   int
	order      int
}

func FetchContext(idx *Index, query string, opts FetchContextOptions) (FetchContextResponse, error) {
	if opts.BudgetTokens <= 0 {
		opts.BudgetTokens = defaultContextBudgetTokens
	}
	if opts.ContextLines <= 0 {
		opts.ContextLines = defaultContextLines
	}
	mode := normalizeMode(opts.Mode)
	if mode == ModeFull {
		// Full mode pulls in callers and callees automatically — that is the
		// point of full. Other modes preserve the caller's choice.
		opts.IncludeCallers = true
		opts.IncludeCallees = true
	}
	resp := FetchContextResponse{
		Status:       "not_found",
		Query:        query,
		BudgetTokens: opts.BudgetTokens,
		Slices:       []ContextSlice{},
	}
	query = strings.TrimSpace(query)
	if query == "" {
		resp.Status = "invalid"
		resp.Warnings = append(resp.Warnings, "empty query")
		return resp, nil
	}
	candidates, target := contextCandidates(idx, query, opts)
	if target != nil {
		summary := summarizeSymbol(*target)
		resp.Target = &summary
	}
	if len(candidates) == 0 {
		resp.Warnings = append(resp.Warnings, "no context candidates found")
		return resp, nil
	}
	if mode == ModeContext || mode == ModeFull {
		candidates = mergeOverlappingCandidates(candidates)
	}
	candidates = clampContextCandidateSpans(candidates, opts.MaxSymbolContextLines)

	// Item 5: reserve budget for the target slice before any other candidate
	// competes. Without this, a file with many imports (priority 1) would
	// drain the budget before the target's body is emitted at all.
	primary, others := splitPrimary(candidates)
	seen := map[string]bool{}
	if primary != nil {
		reserved := opts.BudgetTokens - reservedForOthers(opts.BudgetTokens, others)
		if reserved < 1 {
			reserved = opts.BudgetTokens
		}
		slice, ok, err := buildContextSlice(idx, *primary, reserved)
		if err != nil {
			resp.Warnings = append(resp.Warnings, err.Error())
		} else if ok {
			key := sliceKey(slice)
			if !seen[key] {
				seen[key] = true
				resp.Slices = append(resp.Slices, slice)
				resp.EstimatedTokens += slice.EstimatedTokens
				resp.Truncated = resp.Truncated || slice.Truncated
			}
		} else {
			resp.Truncated = true
		}
	}

	sort.SliceStable(others, func(i, j int) bool {
		if others[i].priority != others[j].priority {
			return others[i].priority < others[j].priority
		}
		if others[i].file != others[j].file {
			return others[i].file < others[j].file
		}
		return others[i].startLine < others[j].startLine
	})
	for _, cand := range others {
		slice, ok, err := buildContextSlice(idx, cand, opts.BudgetTokens-resp.EstimatedTokens)
		if err != nil {
			resp.Warnings = append(resp.Warnings, err.Error())
			continue
		}
		if !ok {
			resp.Truncated = true
			continue
		}
		key := sliceKey(slice)
		if seen[key] {
			continue
		}
		seen[key] = true
		resp.Slices = append(resp.Slices, slice)
		resp.EstimatedTokens += slice.EstimatedTokens
		resp.Truncated = resp.Truncated || slice.Truncated
	}
	if len(resp.Slices) == 0 {
		resp.Status = "budget_exceeded"
		return resp, nil
	}
	resp.Status = "ok"
	applyMode(&resp, mode)
	return resp, nil
}

// FetchContextMany resolves several symbols, file:line locations, or RDF terms
// into one source packet. Unlike repeated FetchContext calls, it merges
// overlapping ranges across all queries before reading source, which keeps
// flow-oriented tools from paying for the same class/function body multiple
// times.
func FetchContextMany(idx *Index, queries []string, opts FetchContextOptions) (FetchContextResponse, error) {
	return fetchContextManyWithSymbolGraph(idx, queries, opts, nil)
}

// fetchContextManyWithSymbolGraph is FetchContextMany's request-local shared
// generation path. All targets resolve against one immutable graph generation,
// so a watcher publication cannot mix versions within a response. InspectFlow
// passes the generation it already loaded for tracing.
func fetchContextManyWithSymbolGraph(idx *Index, queries []string, opts FetchContextOptions, shared *symbolGraphSnapshot) (FetchContextResponse, error) {
	if opts.BudgetTokens <= 0 {
		opts.BudgetTokens = defaultContextBudgetTokens
	}
	if opts.ContextLines <= 0 {
		opts.ContextLines = defaultContextLines
	}
	mode := normalizeMode(opts.Mode)
	if mode == ModeFull {
		opts.IncludeCallers = true
		opts.IncludeCallees = true
	}
	resp := FetchContextResponse{
		Status:       "not_found",
		Query:        strings.Join(cleanContextQueries(queries), " | "),
		BudgetTokens: opts.BudgetTokens,
		Slices:       []ContextSlice{},
	}
	queries = cleanContextQueries(queries)
	if len(queries) == 0 {
		resp.Status = "invalid"
		resp.Warnings = append(resp.Warnings, "empty query list")
		return resp, nil
	}

	if shared == nil && (opts.IncludeCallers || opts.IncludeCallees) {
		snap := idx.symbolGraphSnapshot()
		shared = &snap
	}
	var candidates []contextCandidate
	targetSeen := map[string]bool{}
	for queryOrder, query := range queries {
		resolved, target := contextCandidatesWithSymbolGraph(idx, query, opts, shared)
		for i := range resolved {
			resolved[i].order = queryOrder
		}
		if target != nil && !targetSeen[target.ID] {
			resp.Targets = append(resp.Targets, summarizeSymbol(*target))
			targetSeen[target.ID] = true
			if resp.Target == nil {
				summary := summarizeSymbol(*target)
				resp.Target = &summary
			}
		}
		if len(resolved) == 0 {
			resp.Warnings = append(resp.Warnings, "no context candidates found for "+query)
			continue
		}
		candidates = append(candidates, resolved...)
	}
	if len(candidates) == 0 {
		return resp, nil
	}
	if mode == ModeContext || mode == ModeFull {
		candidates = mergeContextCandidatesByRange(candidates)
	}
	candidates = clampContextCandidateSpans(candidates, opts.MaxSymbolContextLines)
	sortContextCandidates(candidates)

	seen := map[string]bool{}
	for _, cand := range candidates {
		slice, ok, err := buildContextSlice(idx, cand, opts.BudgetTokens-resp.EstimatedTokens)
		if err != nil {
			resp.Warnings = append(resp.Warnings, err.Error())
			continue
		}
		if !ok {
			resp.Truncated = true
			continue
		}
		key := sliceKey(slice)
		if seen[key] {
			continue
		}
		seen[key] = true
		resp.Slices = append(resp.Slices, slice)
		resp.EstimatedTokens += slice.EstimatedTokens
		resp.Truncated = resp.Truncated || slice.Truncated
	}
	if len(resp.Slices) == 0 {
		resp.Status = "budget_exceeded"
		return resp, nil
	}
	resp.Status = "ok"
	applyMode(&resp, mode)
	return resp, nil
}

func cleanContextQueries(queries []string) []string {
	out := make([]string, 0, len(queries))
	seen := map[string]bool{}
	for _, query := range queries {
		query = strings.TrimSpace(query)
		if query == "" || seen[query] {
			continue
		}
		seen[query] = true
		out = append(out, query)
	}
	return out
}

// normalizeMode resolves an empty mode to the legacy default and rejects
// unknown values silently (treated as default) so the API stays forgiving.
func normalizeMode(m string) string {
	switch m {
	case ModeCompact, ModeEvidence, ModeContext, ModeFull:
		return m
	default:
		return ModeContext
	}
}

// applyMode rewrites the response in-place to match the requested verbosity.
// The slice graph (file/startLine/endLine/kind/reason) is preserved across
// all modes so agents always know *where* the evidence is — modes only
// change how much source text travels back. Token counts are recomputed per
// mode: compact returns 0 (no text), evidence the per-line estimate.
func applyMode(resp *FetchContextResponse, mode string) {
	switch mode {
	case ModeCompact:
		for i := range resp.Slices {
			resp.Slices[i].Text = ""
			resp.Slices[i].EstimatedTokens = 0
		}
		resp.EstimatedTokens = 0
	case ModeEvidence:
		total := 0
		for i := range resp.Slices {
			s := &resp.Slices[i]
			line, lineNo := evidenceLine(s)
			s.Text = line
			s.StartLine = lineNo
			s.EndLine = lineNo
			s.EstimatedTokens = EstimateTokens(line)
			total += s.EstimatedTokens
		}
		resp.EstimatedTokens = total
	case ModeContext, ModeFull:
		// no-op
	}
}

func firstLine(s string) string {
	if s == "" {
		return ""
	}
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx+1]
	}
	return s
}

func evidenceLine(s *ContextSlice) (string, int) {
	lineNo := s.FocusLine
	if lineNo == 0 && len(s.FocusLines) > 0 {
		lineNo = s.FocusLines[0]
	}
	if lineNo < s.StartLine || lineNo > s.EndLine {
		lineNo = s.StartLine
	}
	lines := strings.SplitAfter(s.Text, "\n")
	idx := lineNo - s.StartLine
	if idx < 0 || idx >= len(lines) {
		return firstLine(s.Text), s.StartLine
	}
	return lines[idx], lineNo
}

func sliceKey(slice ContextSlice) string {
	return fmt.Sprintf("%s:%d:%d:%s", slice.File, slice.StartLine, slice.EndLine, slice.Reason)
}

// splitPrimary separates the user's primary evidence slice from the rest.
// Symbol queries use the full target symbol as primary; file:line queries
// use the requested line neighborhood as primary, even if that line is inside
// a giant Vue component or class. That keeps UI/debugging questions centered
// on the thing the caller pointed at instead of spending the budget from the
// top of the enclosing symbol.
func splitPrimary(candidates []contextCandidate) (*contextCandidate, []contextCandidate) {
	others := make([]contextCandidate, 0, len(candidates))
	var primary *contextCandidate
	for i := range candidates {
		if primary == nil && (candidates[i].reason == "target symbol" || candidates[i].reason == "requested line") {
			c := candidates[i]
			primary = &c
			continue
		}
		others = append(others, candidates[i])
	}
	return primary, others
}

// reservedForOthers caps the share of the budget the target may consume so
// imports / caller / callee signatures still fit. We give the target up to
// 70% of the budget; other candidates split the remaining 30%. If there are
// no other candidates, the target may use the full budget.
func reservedForOthers(budget int, others []contextCandidate) int {
	if len(others) == 0 {
		return 0
	}
	share := budget * 3 / 10
	if share < 60 {
		share = 60
	}
	if share >= budget {
		share = budget - 1
	}
	if share < 0 {
		share = 0
	}
	return share
}

func contextCandidates(idx *Index, query string, opts FetchContextOptions) ([]contextCandidate, *CGPSymbol) {
	return contextCandidatesWithSymbolGraph(idx, query, opts, nil)
}

func contextCandidatesWithSymbolGraph(idx *Index, query string, opts FetchContextOptions, shared *symbolGraphSnapshot) ([]contextCandidate, *CGPSymbol) {
	if file, line, ok := parseFileLineQuery(idx, query); ok {
		idx.ensureFileSymbolIndex()
		if sym := idx.containingSymbolFast(file, line); sym.ID != "" {
			return lineWithinSymbolContextCandidates(idx, sym, line, opts), &sym
		}
		return []contextCandidate{lineContextCandidate(idx, file, line, opts.ContextLines, "line", "requested line")}, nil
	}
	symbols := findSymbols(idx, query)
	if len(symbols) == 1 {
		return symbolContextCandidatesWithSymbolGraph(idx, symbols[0], opts, shared), &symbols[0]
	}
	if len(symbols) > 1 {
		return summarizeAmbiguousSymbolCandidates(symbols), nil
	}
	trace := TraceTerm(idx, query)
	if trace.Status == "found" {
		var out []contextCandidate
		for _, loc := range trace.TTLUsages {
			out = append(out, locationContextCandidate(idx, loc, opts.ContextLines, "ttl-evidence", "term TTL usage", 10))
		}
		for _, ref := range trace.CodeReferences {
			loc := Location{File: ref.File, StartLine: ref.StartLine, StartColumn: ref.StartColumn, EndLine: ref.EndLine, EndColumn: ref.EndColumn, Kind: ref.Kind}
			out = append(out, locationContextCandidate(idx, loc, opts.ContextLines, "code-evidence", "term code reference", 20))
		}
		return out, nil
	}
	return nil, nil
}

func lineWithinSymbolContextCandidates(idx *Index, sym CGPSymbol, line int, opts FetchContextOptions) []contextCandidate {
	var out []contextCandidate
	if !opts.SuppressImports && shouldIncludeImportsForSymbol(sym) {
		out = append(out, importCandidates(idx, sym.File)...)
	}
	lineCand := lineContextCandidate(idx, sym.File, line, opts.ContextLines, sym.Kind, "requested line")
	if shouldClampLineQueryToSymbol(sym) {
		lineCand.startLine = maxInt(lineCand.startLine, sym.StartLine)
		lineCand.endLine = minInt(lineCand.endLine, sym.EndLine)
	}
	lineCand = clampContextCandidateSpan(lineCand, line, opts.MaxSymbolContextLines)
	out = append(out, lineCand)
	if sym.StartLine > 0 && (line < sym.StartLine || line > sym.StartLine+opts.ContextLines) {
		out = append(out, signatureCandidate(sym, "enclosing symbol"))
	}
	return out
}

func shouldClampLineQueryToSymbol(sym CGPSymbol) bool {
	if sym.StartLine <= 0 || sym.EndLine < sym.StartLine {
		return false
	}
	switch sym.Kind {
	case "template-class", "css-class", "ttl-term":
		return false
	default:
		return true
	}
}

func symbolContextCandidatesWithSymbolGraph(idx *Index, sym CGPSymbol, opts FetchContextOptions, shared *symbolGraphSnapshot) []contextCandidate {
	var out []contextCandidate
	if !opts.SuppressImports && shouldIncludeImportsForSymbol(sym) {
		out = append(out, importCandidates(idx, sym.File)...)
	}
	start, end := symbolRange(idx, sym, opts.ContextLines)
	cand := contextCandidate{file: sym.File, startLine: start, endLine: end, focusLine: sym.StartLine, kind: sym.Kind, reason: "target symbol", priority: 10}
	cand = clampContextCandidateSpan(cand, sym.StartLine, opts.MaxSymbolContextLines)
	out = append(out, cand)
	if cand, ok := tailAnchorCandidate(idx, sym); ok {
		out = append(out, cand)
	}
	if opts.IncludeCallers || opts.IncludeCallees {
		var snap symbolGraphSnapshot
		if shared != nil {
			snap = *shared
		} else {
			snap = idx.symbolGraphSnapshot() // reads only Symbols/SymbolEdges
		}
		snap.forEachEdge(func(_ int, edge CGPEdge) bool {
			if opts.IncludeCallees && traceSymbolEdgeType(edge.Type) && edge.From == sym.ID {
				if callee, ok := snap.Symbols[edge.To]; ok {
					if callee.ID == sym.ID {
						return true
					}
					out = append(out, signatureCandidate(callee, "callee signature"))
				}
			}
			if opts.IncludeCallers && traceSymbolEdgeType(edge.Type) && edge.To == sym.ID {
				if caller, ok := snap.Symbols[edge.From]; ok {
					if caller.ID == sym.ID {
						return true
					}
					out = append(out, locationContextCandidate(idx, edge.Evidence, opts.ContextLines, "caller-site", "caller call site", 40))
					if caller.Kind != "component" {
						out = append(out, signatureCandidate(caller, "caller signature"))
					}
				}
			}
			return true
		})
	}
	return out
}

func clampContextCandidateSpan(cand contextCandidate, focusLine, maxLines int) contextCandidate {
	if maxLines <= 0 || cand.endLine < cand.startLine || cand.endLine-cand.startLine+1 <= maxLines {
		return cand
	}
	if cand.fullStart == 0 {
		cand.fullStart = cand.startLine
		cand.fullEnd = cand.endLine
	}
	if focusLine <= 0 {
		focusLine = cand.focusLine
	}
	if focusLine <= 0 {
		focusLine = cand.startLine
	}
	focusLine = maxInt(cand.startLine, minInt(cand.endLine, focusLine))
	before := maxLines / 2
	after := maxLines - before - 1
	start := focusLine - before
	end := focusLine + after
	if start < cand.startLine {
		end += cand.startLine - start
		start = cand.startLine
	}
	if end > cand.endLine {
		start -= end - cand.endLine
		end = cand.endLine
	}
	cand.startLine = maxInt(cand.startLine, start)
	cand.endLine = minInt(cand.endLine, end)
	return cand
}

func clampContextCandidateSpans(candidates []contextCandidate, maxLines int) []contextCandidate {
	if maxLines <= 0 {
		return candidates
	}
	for i := range candidates {
		focusLine := candidates[i].focusLine
		if focusLine <= 0 && len(candidates[i].focusLines) > 0 {
			focusLine = candidates[i].focusLines[0]
		}
		candidates[i] = clampContextCandidateSpan(candidates[i], focusLine, maxLines)
	}
	return candidates
}

func shouldIncludeImportsForSymbol(sym CGPSymbol) bool {
	switch sym.Kind {
	case "css-class", "template-class", "ttl-term", "ttl-shape":
		return false
	default:
		return true
	}
}

func importCandidates(idx *Index, file string) []contextCandidate {
	var out []contextCandidate
	snap := idx.symbolGraphSnapshot() // reads only SymbolEdges
	snap.forEachEdge(func(_ int, edge CGPEdge) bool {
		if edge.Type != "imports" || edge.Evidence.File != file {
			return true
		}
		out = append(out, contextCandidate{file: file, startLine: edge.Evidence.StartLine, endLine: edge.Evidence.StartLine, focusLine: edge.Evidence.StartLine, kind: "import", reason: "required import", priority: 1})
		return true
	})
	return mergeImportCandidates(out)
}

func mergeImportCandidates(in []contextCandidate) []contextCandidate {
	if len(in) == 0 {
		return nil
	}
	sort.Slice(in, func(i, j int) bool {
		if in[i].file != in[j].file {
			return in[i].file < in[j].file
		}
		return in[i].startLine < in[j].startLine
	})
	var out []contextCandidate
	cur := in[0]
	for _, cand := range in[1:] {
		if cand.file == cur.file && cand.startLine <= cur.endLine+1 {
			if cand.endLine > cur.endLine {
				cur.endLine = cand.endLine
			}
			continue
		}
		out = append(out, cur)
		cur = cand
	}
	out = append(out, cur)
	return out
}

func mergeOverlappingCandidates(in []contextCandidate) []contextCandidate {
	if len(in) == 0 {
		return nil
	}
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].file != in[j].file {
			return in[i].file < in[j].file
		}
		if in[i].kind != in[j].kind {
			return in[i].kind < in[j].kind
		}
		if in[i].reason != in[j].reason {
			return in[i].reason < in[j].reason
		}
		if in[i].priority != in[j].priority {
			return in[i].priority < in[j].priority
		}
		return in[i].startLine < in[j].startLine
	})
	out := make([]contextCandidate, 0, len(in))
	cur := in[0]
	for _, cand := range in[1:] {
		if cand.file == cur.file && cand.kind == cur.kind && cand.reason == cur.reason && cand.priority == cur.priority && cand.startLine <= cur.endLine+1 {
			if cand.endLine > cur.endLine {
				cur.endLine = cand.endLine
			}
			if cur.focusLine == 0 {
				cur.focusLine = cand.focusLine
			}
			continue
		}
		out = append(out, cur)
		cur = cand
	}
	out = append(out, cur)
	return out
}

func mergeContextCandidatesByRange(in []contextCandidate) []contextCandidate {
	if len(in) == 0 {
		return nil
	}
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].file != in[j].file {
			return in[i].file < in[j].file
		}
		if in[i].startLine != in[j].startLine {
			return in[i].startLine < in[j].startLine
		}
		return in[i].endLine < in[j].endLine
	})
	out := make([]contextCandidate, 0, len(in))
	cur := normalizeCandidateFocus(in[0])
	for _, raw := range in[1:] {
		cand := normalizeCandidateFocus(raw)
		if cand.file == cur.file && cand.startLine <= cur.endLine+1 {
			if cand.endLine > cur.endLine {
				cur.endLine = cand.endLine
			}
			// Priority and query order are one lexicographic rank. Taking
			// their minima independently can synthesize a rank no candidate
			// actually had: a later high-priority search slice supplies the
			// priority while an earlier low-priority callee supplies the order,
			// incorrectly outranking the caller's explicit primary target.
			if cand.priority < cur.priority || (cand.priority == cur.priority && cand.order < cur.order) {
				cur.priority = cand.priority
				cur.order = cand.order
			}
			cur.kind = joinUniqueLabel(cur.kind, cand.kind)
			cur.reason = joinUniqueLabel(cur.reason, cand.reason)
			cur.focusLines = mergeFocusLines(cur.focusLines, cand.focusLines)
			if cur.focusLine == 0 && len(cur.focusLines) > 0 {
				cur.focusLine = cur.focusLines[0]
			}
			continue
		}
		out = append(out, cur)
		cur = cand
	}
	out = append(out, cur)
	return out
}

func normalizeCandidateFocus(cand contextCandidate) contextCandidate {
	if cand.focusLine > 0 {
		cand.focusLines = mergeFocusLines(cand.focusLines, []int{cand.focusLine})
	}
	if cand.focusLine == 0 && len(cand.focusLines) > 0 {
		cand.focusLine = cand.focusLines[0]
	}
	return cand
}

func mergeFocusLines(a, b []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, line := range append(append([]int(nil), a...), b...) {
		if line <= 0 || seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	sort.Ints(out)
	return out
}

func joinUniqueLabel(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" || a == b {
		return a
	}
	parts := strings.Split(a, "+")
	for _, part := range parts {
		if part == b {
			return a
		}
	}
	return a + "+" + b
}

func sortContextCandidates(candidates []contextCandidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].priority != candidates[j].priority {
			return candidates[i].priority < candidates[j].priority
		}
		if candidates[i].order != candidates[j].order {
			return candidates[i].order < candidates[j].order
		}
		if candidates[i].file != candidates[j].file {
			return candidates[i].file < candidates[j].file
		}
		return candidates[i].startLine < candidates[j].startLine
	})
}

func signatureCandidate(sym CGPSymbol, reason string) contextCandidate {
	return contextCandidate{file: sym.File, startLine: sym.StartLine, endLine: sym.StartLine, focusLine: sym.StartLine, kind: sym.Kind, reason: reason, priority: 50}
}

func summarizeAmbiguousSymbolCandidates(symbols []CGPSymbol) []contextCandidate {
	var out []contextCandidate
	for _, sym := range symbols {
		out = append(out, signatureCandidate(sym, "ambiguous candidate"))
	}
	return out
}

func lineContextCandidate(idx *Index, file string, line, contextLines int, kind, reason string) contextCandidate {
	idx.mu.Lock()
	info := idx.Files[file]
	idx.mu.Unlock()
	start := maxInt(1, line-contextLines)
	end := minInt(info.LineCount, line+contextLines)
	return contextCandidate{file: file, startLine: start, endLine: end, focusLine: line, kind: kind, reason: reason, priority: 10}
}

func locationContextCandidate(idx *Index, loc Location, contextLines int, kind, reason string, priority int) contextCandidate {
	cand := lineContextCandidate(idx, loc.File, loc.StartLine, contextLines, kind, reason)
	cand.priority = priority
	return cand
}

func symbolRange(idx *Index, sym CGPSymbol, contextLines int) (int, int) {
	idx.mu.Lock()
	info := idx.Files[sym.File]
	idx.mu.Unlock()
	start := sym.StartLine
	end := sym.EndLine
	if start <= 0 {
		start = 1
	}
	if end < start {
		end = start
	}
	if sym.Kind == "ttl-term" || sym.Kind == "ttl-shape" {
		start = maxInt(1, start-contextLines)
		end = minInt(info.LineCount, end+contextLines)
	}
	if info.LineCount > 0 {
		end = minInt(info.LineCount, end)
	}
	return start, end
}

func buildContextSlice(idx *Index, cand contextCandidate, remainingTokens int) (ContextSlice, bool, error) {
	if remainingTokens <= 0 {
		return ContextSlice{}, false, nil
	}
	resp, err := FetchSource(idx, cand.file, cand.startLine, cand.endLine)
	if err != nil {
		return ContextSlice{}, false, err
	}
	text := resp.Text
	estimated := EstimateTokens(text)
	start := cand.startLine
	end := cand.endLine
	fullStart := cand.fullStart
	fullEnd := cand.fullEnd
	if fullStart == 0 {
		fullStart = start
		fullEnd = end
	}
	if estimated > remainingTokens {
		text, end, estimated = clampLinesToBudget(text, start, remainingTokens)
		if estimated == 0 {
			return ContextSlice{}, false, nil
		}
	}
	truncated := start > fullStart || end < fullEnd
	slice := ContextSlice{
		File:            cand.file,
		StartLine:       start,
		EndLine:         end,
		Truncated:       truncated,
		FocusLine:       cand.focusLine,
		FocusLines:      focusLinesInRange(cand.focusLines, start, end),
		Kind:            cand.kind,
		Reason:          cand.reason,
		EstimatedTokens: estimated,
		Text:            text,
	}
	if truncated {
		slice.FullStartLine = fullStart
		slice.FullEndLine = fullEnd
	}
	return slice, true, nil
}

func focusLinesInRange(lines []int, start, end int) []int {
	var out []int
	for _, line := range lines {
		if line >= start && line <= end {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func clampLinesToBudget(text string, startLine, budgetTokens int) (string, int, int) {
	if budgetTokens <= 0 {
		return "", startLine - 1, 0
	}
	lines := strings.SplitAfter(text, "\n")
	var b strings.Builder
	used := 0
	endLine := startLine - 1
	for _, line := range lines {
		lineTokens := EstimateTokens(line)
		if used > 0 && used+lineTokens > budgetTokens {
			break
		}
		if used == 0 && lineTokens > budgetTokens {
			runes := []rune(line)
			limit := minInt(len(runes), budgetTokens*4)
			b.WriteString(string(runes[:limit]))
			used = EstimateTokens(b.String())
			endLine = startLine
			break
		}
		b.WriteString(line)
		used += lineTokens
		endLine++
	}
	return b.String(), endLine, used
}

func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	runes := len([]rune(text))
	return (runes + 3) / 4
}

// MarshalWithRealTokenEstimate marshals v to JSON, then — if v (or *v) has a
// top-level int field named "EstimatedTokens" — overwrites that field with
// an estimate of the *entire serialized response*, not just the source-text
// excerpts that internal budget tracking accumulates into it during
// construction.
//
// Why: response builders (SearchCode, InspectFlow, InspectTerm, ...)
// accumulate EstimatedTokens incrementally as a *content budget* — it only
// counts the source-code excerpt text used to decide when to stop adding
// hits/slices, not the surrounding JSON (file paths, scores, matchedTerms,
// status, kind, etc.). That's the right thing for budget enforcement, but
// it makes the field actively misleading as a "how many tokens will this
// response cost the calling agent" signal: response audits found substantial
// under-reporting. An
// agent that trusts this field to plan its own context budget will
// under-provision. This function is the single place both the CLI's
// printJSON and the MCP server's jsonResult fix that up before the bytes
// actually go out, without touching the internal budget-accumulation logic.
func MarshalWithRealTokenEstimate(v any, indent, escapeHTML bool) ([]byte, error) {
	marshal := func(x any) ([]byte, error) {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(escapeHTML)
		if indent {
			enc.SetIndent("", "  ")
		}
		if err := enc.Encode(x); err != nil {
			return nil, err
		}
		// json.Encoder.Encode appends a trailing newline; trim it so callers
		// that want to add their own (or none) get the exact same bytes
		// json.Marshal/MarshalIndent would have produced.
		return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
	}

	rv := reflect.ValueOf(v)
	var target reflect.Value
	switch rv.Kind() {
	case reflect.Ptr:
		if rv.IsNil() {
			return marshal(v)
		}
		target = rv.Elem()
	case reflect.Struct:
		// Value types aren't addressable — operate on an addressable copy
		// so we can set the field without mutating the caller's value.
		cp := reflect.New(rv.Type()).Elem()
		cp.Set(rv)
		target = cp
	default:
		return marshal(v)
	}

	field := target.FieldByName("EstimatedTokens")
	if !field.IsValid() || field.Kind() != reflect.Int || !field.CanSet() {
		return marshal(v)
	}

	first, err := marshal(target.Interface())
	if err != nil {
		return nil, err
	}
	field.SetInt(int64(EstimateTokens(string(first))))
	return marshal(target.Interface())
}

func parseFileLineQuery(idx *Index, query string) (string, int, bool) {
	parts := strings.Split(query, ":")
	if len(parts) < 2 {
		return "", 0, false
	}
	linePart := parts[len(parts)-1]
	if len(parts) >= 3 && isInt(parts[len(parts)-1]) && isInt(parts[len(parts)-2]) {
		linePart = parts[len(parts)-2]
		parts = parts[:len(parts)-2]
	} else {
		parts = parts[:len(parts)-1]
	}
	line, err := strconv.Atoi(linePart)
	if err != nil || line < 1 {
		return "", 0, false
	}
	file := filepath.ToSlash(filepath.Clean(strings.Join(parts, ":")))
	idx.mu.Lock()
	_, ok := idx.Files[file]
	idx.mu.Unlock()
	if !ok {
		return "", 0, false
	}
	return file, line, true
}

func isInt(value string) bool {
	_, err := strconv.Atoi(value)
	return err == nil
}

func ReadIndexSourceLine(idx *Index, file string, line int) (string, error) {
	idx.mu.Lock()
	_, ok := idx.Files[file]
	root := idx.Repo.Root
	idx.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("file is not indexed: %s", file)
	}
	data, err := readRepoFile(root, file)
	if err != nil {
		return "", err
	}
	lines := strings.SplitAfter(string(data), "\n")
	if line < 1 || line > len(lines) {
		return "", fmt.Errorf("line out of range: %s:%d", file, line)
	}
	return lines[line-1], nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Phase 2 — tail evidence anchors. When a symbol body is long enough that the
// head clamp inside FetchContext would silently drop its tail, we emit a
// second slice anchored on the last evidence-bearing line (return / throw /
// .filter( / .sort( / .map( / closing brace). The pattern is borrowed from
// SCIP's definition-vs-occurrence split — the head shows the contract, the
// tail shows the result. The tail slice is *not* the primary, so it shares
// the "others" budget with imports/callers and never starves the body.
const (
	tailAnchorMinBody    = 60
	tailAnchorWindow     = 12
	tailAnchorScanLines  = 28
	tailAnchorMinHeadGap = 6
)

var tailAnchorPatternRe = regexp.MustCompile(`\b(return|throw|yield)\b|\.\s*(filter|sort|map|reduce|forEach|find|some|every|toArray|flatMap)\s*\(|^\s*}`)

// tailAnchorCandidate returns a small line range at the bottom of a long
// symbol body, focused on the last line that returns, throws, or applies a
// terminal collection method. The window is bounded so a runaway anchor (e.g.
// a trailing CSS block in a Vue file) cannot expand without limit. Returns
// false when the body is too short to justify a tail slice or when the
// anchor would overlap the head reservation.
func tailAnchorCandidate(idx *Index, sym CGPSymbol) (contextCandidate, bool) {
	if sym.EndLine-sym.StartLine < tailAnchorMinBody {
		return contextCandidate{}, false
	}
	idx.mu.Lock()
	info, ok := idx.Files[sym.File]
	idx.mu.Unlock()
	if !ok {
		return contextCandidate{}, false
	}
	scanStart := sym.EndLine - tailAnchorScanLines
	if scanStart <= sym.StartLine+tailAnchorWindow+tailAnchorMinHeadGap {
		return contextCandidate{}, false
	}
	scanEnd := sym.EndLine
	if info.LineCount > 0 && scanEnd > info.LineCount {
		scanEnd = info.LineCount
	}
	if scanEnd <= scanStart {
		return contextCandidate{}, false
	}
	src, err := FetchSource(idx, sym.File, scanStart, scanEnd)
	if err != nil {
		return contextCandidate{}, false
	}
	lines := strings.SplitAfter(src.Text, "\n")
	anchorLineNo := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if tailAnchorPatternRe.MatchString(lines[i]) {
			anchorLineNo = scanStart + i
			break
		}
	}
	if anchorLineNo == 0 {
		anchorLineNo = scanEnd
	}
	half := tailAnchorWindow / 2
	start := anchorLineNo - half
	end := anchorLineNo + half
	if start < sym.StartLine+tailAnchorWindow+tailAnchorMinHeadGap {
		start = sym.StartLine + tailAnchorWindow + tailAnchorMinHeadGap
	}
	if start >= sym.EndLine {
		return contextCandidate{}, false
	}
	if end > sym.EndLine {
		end = sym.EndLine
	}
	if end-start < 3 {
		return contextCandidate{}, false
	}
	if anchorLineNo < start {
		anchorLineNo = start
	}
	if anchorLineNo > end {
		anchorLineNo = end
	}
	return contextCandidate{
		file:      sym.File,
		startLine: start,
		endLine:   end,
		focusLine: anchorLineNo,
		kind:      sym.Kind,
		reason:    "tail anchor",
		priority:  12,
	}, true
}

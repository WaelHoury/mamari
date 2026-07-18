package mamari

import "fmt"

const (
	// maxTestCoverageDepth bounds how many reverse-call hops are walked when
	// looking for a test-file caller, keeping per-symbol cost roughly
	// constant regardless of call-graph depth.
	maxTestCoverageDepth = 6
	// maxTestCoverageVisited bounds the total number of symbols visited per
	// BFS, protecting against hub functions with huge caller fan-in.
	maxTestCoverageVisited = 300
	// maxTestsForResults caps the number of test symbols returned by TestsFor.
	maxTestsForResults = 50
	// maxUntestedSymbols caps the number of symbols returned by
	// UntestedSymbols, mirroring the other report-style tools' caps.
	maxUntestedSymbols = 500
)

var defaultUntestedKinds = map[string]bool{
	"function":  true,
	"method":    true,
	"class":     true,
	"component": true,
}

// testCallersInReverseClosure walks the reverse caller graph from startID and
// returns every visited symbol that looks like a test (isTestCaller), up to
// maxTestCoverageDepth hops and maxTestCoverageVisited visited nodes. The
// walk is breadth-first and dedupes revisits, so cyclic call graphs terminate.
func testCallersInReverseClosure(snap indexSnapshot, callers map[string][]CGPEdge, startID string) []CGPSymbol {
	var found []CGPSymbol
	visited := map[string]bool{startID: true}
	frontier := []string{startID}
	for d := 0; d < maxTestCoverageDepth && len(visited) < maxTestCoverageVisited; d++ {
		var next []string
		for _, id := range frontier {
			for _, edge := range callers[id] {
				if edge.From == startID {
					continue
				}
				caller, ok := snap.Symbols[edge.From]
				if !ok || caller.Kind == "file" {
					continue
				}
				if visited[caller.ID] {
					continue
				}
				visited[caller.ID] = true
				if isTestCaller(caller) {
					found = append(found, caller)
				}
				next = append(next, caller.ID)
				if len(visited) >= maxTestCoverageVisited {
					break
				}
			}
			if len(visited) >= maxTestCoverageVisited {
				break
			}
		}
		if len(next) == 0 {
			break
		}
		frontier = next
	}
	return found
}

// TestsFor returns the test files/functions that, transitively via the call
// graph (up to maxTestCoverageDepth hops), exercise the queried symbol. This
// is a best-effort heuristic based on static call edges: it cannot see
// dynamic dispatch, reflection, or end-to-end/black-box tests that never call
// the symbol directly.
func TestsFor(idx *Index, query string, depth int) TestsForResponse {
	resp := TestsForResponse{Status: "not_found", Query: query, Tests: []CGPSymbolSummary{}}
	matches := findSymbols(idx, query)
	if len(matches) > 1 {
		resp.Status = "ambiguous"
		resp.Candidates = summarizeSymbols(matches)
		return resp
	}
	if len(matches) == 0 {
		return resp
	}
	target := matches[0]
	summary := summarizeSymbol(target)
	resp.Symbol = &summary
	resp.Status = "found"

	snap := idx.snapshot()
	callers := buildReverseCallIndexSnapshot(snap)
	found := testCallersInReverseClosure(snap, callers, target.ID)

	out := make([]CGPSymbolSummary, 0, len(found))
	for _, sym := range found {
		out = append(out, summarizeSymbol(sym))
	}
	sortSymbolSummaries(out, "")
	resp.Total = len(out)
	if len(out) > maxTestsForResults {
		out = out[:maxTestsForResults]
		resp.Truncated = true
	}
	resp.Tests = out
	possibleSeen := map[string]bool{}
	for _, possible := range possibleUnresolvedCallersForIndex(snap, target) {
		if !isTestCaller(possible.caller) || possibleSeen[possible.caller.ID] {
			continue
		}
		possibleSeen[possible.caller.ID] = true
		resp.PossibleTests = append(resp.PossibleTests, summarizeSymbol(possible.caller))
	}
	sortSymbolSummaries(resp.PossibleTests, "")
	if len(resp.PossibleTests) > 0 {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf("%d unresolved same-name test caller(s) may exercise this symbol", len(resp.PossibleTests)))
	}
	return resp
}

// UntestedSymbols returns symbols whose reverse call closure (up to
// maxTestCoverageDepth hops) contains no test-file caller anywhere. Like
// TestsFor, this is a static-call-graph heuristic: symbols exercised only via
// dynamic dispatch, reflection, or black-box/integration tests will be
// reported as untested even if they are, in practice, covered.
func UntestedSymbols(idx *Index, opts UntestedSymbolsOptions) UntestedSymbolsResponse {
	snap := idx.snapshot()
	callers := buildReverseCallIndexSnapshot(snap)
	unresolvedTestNames := unresolvedCallNamesByLanguageIndex(snap, true)

	kindSet := defaultUntestedKinds
	if len(opts.Kinds) > 0 {
		kindSet = map[string]bool{}
		for _, k := range opts.Kinds {
			if k != "" {
				kindSet[k] = true
			}
		}
	}

	listOpts := ListSymbolsOptions{SourceOnly: true}

	var out []CGPSymbolSummary
	uncertainSkipped := 0
	for _, sym := range snap.Symbols {
		if !kindSet[sym.Kind] {
			continue
		}
		if shouldExcludeNoisyFile(sym.File, listOpts) {
			continue
		}
		if len(testCallersInReverseClosure(snap, callers, sym.ID)) > 0 {
			continue
		}
		if hasUnresolvedName(unresolvedTestNames, sym) {
			uncertainSkipped++
			continue
		}
		out = append(out, summarizeSymbol(sym))
	}
	sortSymbolSummaries(out, "")

	total := len(out)
	limit := opts.Limit
	if limit <= 0 {
		limit = maxUntestedSymbols
	}
	truncated := false
	if len(out) > limit {
		out = out[:limit]
		truncated = true
	}
	if out == nil {
		out = []CGPSymbolSummary{}
	}
	resp := UntestedSymbolsResponse{
		Status:           "ok",
		Total:            total,
		Limit:            limit,
		Truncated:        truncated,
		UncertainSkipped: uncertainSkipped,
		Symbols:          out,
	}
	if uncertainSkipped > 0 {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf("%d symbol(s) omitted because unresolved calls from tests may exercise them", uncertainSkipped))
	}
	return resp
}

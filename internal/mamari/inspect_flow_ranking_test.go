package mamari

import (
	"strings"
	"testing"
)

func TestLifecycleSupplementalRankingPrefersImplementationSymbol(t *testing.T) {
	query := "request dispatch before after teardown exceptions"
	raw := searchQueryRawTermSet(query)
	expanded := map[string]bool{}
	for _, term := range searchQueryTerms(query) {
		expanded[term] = true
	}
	implementation := CGPSymbol{
		ID: "app.full_dispatch_request", Name: "full_dispatch_request",
		Kind: "method", File: "src/framework/app.py", StartLine: 10,
		Signature: "def full_dispatch_request(self, ctx):",
	}
	wrapper := CGPSymbol{
		ID: "wrappers.Request", Name: "Request",
		Kind: "class", File: "src/framework/wrappers.py", StartLine: 1,
		Signature: "class Request:",
	}
	implementationScore := lifecycleSupplementalSymbolScore(implementation, raw, expanded, 3)
	wrapperScore := lifecycleSupplementalSymbolScore(wrapper, raw, expanded, 0)
	if implementationScore <= wrapperScore {
		t.Fatalf("implementation score %d must exceed wrapper score %d; raw=%v expanded=%v", implementationScore, wrapperScore, raw, expanded)
	}
}

func TestLifecycleSupplementalRankingPrefersRequestedTeardownBoundary(t *testing.T) {
	query := "request dispatch before after teardown exceptions"
	raw := searchQueryRawTermSet(query)
	expanded := map[string]bool{}
	for _, term := range searchQueryTerms(query) {
		expanded[term] = true
	}
	contextBoundary := CGPSymbol{
		ID: "ctx.RequestContext.pop", Name: "pop",
		Kind: "method", File: "src/framework/ctx.py", StartLine: 20,
		Signature: "def pop(self, exc=None):",
	}
	redundantDispatch := CGPSymbol{
		ID: "views.dispatch_request", Name: "dispatch_request",
		Kind: "method", File: "src/framework/views.py", StartLine: 40,
		Signature: "def dispatch_request(self):",
	}
	contextScore := lifecycleSupplementalSymbolScore(contextBoundary, raw, expanded, 1)
	dispatchScore := lifecycleSupplementalSymbolScore(redundantDispatch, raw, expanded, 1)
	if contextScore <= dispatchScore {
		t.Fatalf("requested teardown boundary score %d must exceed redundant dispatch score %d", contextScore, dispatchScore)
	}
}

func TestInspectFlowExplicitAnchorsPreferNamedCallableInUnrepresentedFile(t *testing.T) {
	class := CGPSymbol{ID: "class:QueryExecDataset", Name: "QueryExecDataset", Kind: "class", File: "src/QueryExecDataset.java", StartLine: 10}
	method := CGPSymbol{ID: "method:QueryExecDataset.getPlan", Name: "getPlan", Kind: "method", File: class.File, StartLine: 80, ParentID: class.ID}
	represented := CGPSymbol{ID: "interface:QueryEngineFactory", Name: "QueryEngineFactory", Kind: "interface", File: "src/QueryEngineFactory.java", StartLine: 5}
	snap := symbolGraphSnapshot{Symbols: map[string]CGPSymbol{
		class.ID: class, method.ID: method, represented.ID: represented,
	}}
	anchors := inspectFlowExplicitSymbolAnchors(
		"QueryExecDataset QueryEngineFactory Plan getPlan query execution",
		[]string{represented.ID},
		InspectFlowOptions{Limit: 8, SourceOnly: true},
		snap,
	)
	if len(anchors) != 1 || anchors[0].ID != method.ID {
		t.Fatalf("expected the named callable from the unrepresented file, got %#v", anchors)
	}
}

func TestMergedContextCandidateKeepsPriorityAndOrderFromSameSource(t *testing.T) {
	merged := mergeContextCandidatesByRange([]contextCandidate{
		{file: "src/service.ts", startLine: 10, endLine: 20, priority: 10, order: 8, reason: "later primary"},
		{file: "src/service.ts", startLine: 15, endLine: 25, priority: 50, order: 0, reason: "early callee"},
	})
	if len(merged) != 1 {
		t.Fatalf("expected one merged candidate, got %#v", merged)
	}
	if merged[0].priority != 10 || merged[0].order != 8 {
		t.Fatalf("merged rank combined fields from different candidates: %#v", merged[0])
	}
}

func TestInspectFlowTotalBudgetRemovesDuplicateContextBeforeFileDiversity(t *testing.T) {
	long := strings.Repeat("implementation evidence ", 80)
	resp := InspectFlowResponse{
		Status: "ok",
		Query:  "request lifecycle dispatch teardown",
		Search: SearchCodeResponse{
			Status: "ok", Query: "request lifecycle dispatch teardown", Limit: 2,
			Hits: []SearchCodeHit{
				{File: "src/app.py", StartLine: 10, EndLine: 10, FocusLine: 10, Text: "def dispatch_request():\n"},
				{File: "src/ctx.py", StartLine: 20, EndLine: 20, FocusLine: 20, Text: "def pop_context():\n"},
			},
		},
		Symbols: []CGPSymbolSummary{
			{ID: "dispatch", Name: "dispatch_request", Kind: "function", File: "src/app.py", StartLine: 10, Docstring: long},
			{ID: "pop", Name: "pop_context", Kind: "function", File: "src/ctx.py", StartLine: 20, Docstring: long},
		},
		Context: FetchContextResponse{
			Status: "ok",
			Slices: []ContextSlice{
				{File: "src/app.py", StartLine: 10, EndLine: 20, FocusLine: 10, Text: long, EstimatedTokens: EstimateTokens(long)},
				{File: "src/app.py", StartLine: 30, EndLine: 40, FocusLine: 30, Text: long, EstimatedTokens: EstimateTokens(long)},
				{File: "src/ctx.py", StartLine: 20, EndLine: 30, FocusLine: 20, Text: long, EstimatedTokens: EstimateTokens(long)},
				{File: "src/errors.py", StartLine: 50, EndLine: 60, FocusLine: 50, Text: long, EstimatedTokens: EstimateTokens(long)},
			},
		},
	}
	const budget = 900
	fitInspectFlowResponse(&resp, budget)
	if got := inspectFlowSerializedTokens(&resp); got > budget {
		t.Fatalf("serialized response exceeded budget: got %d want <= %d", got, budget)
	}
	if len(resp.Context.Slices) == 0 {
		t.Fatal("budget shaping removed every source slice")
	}
	seen := map[string]bool{}
	for _, slice := range resp.Context.Slices {
		if seen[slice.File] {
			t.Fatalf("duplicate file survived while packet was truncated: %#v", resp.Context.Slices)
		}
		seen[slice.File] = true
	}
	if !resp.Context.Truncated {
		t.Fatal("budget shaping did not report truncation")
	}
}

func TestInspectFlowBudgetTrimsMetadataBeforeDistinctSourceFiles(t *testing.T) {
	long := strings.Repeat("metadata detail ", 30)
	resp := InspectFlowResponse{
		Status: "ok",
		Search: SearchCodeResponse{Status: "ok", Hits: []SearchCodeHit{
			{File: "src/a.ts", Text: long}, {File: "src/b.ts", Text: long},
			{File: "src/c.ts", Text: long}, {File: "src/d.ts", Text: long},
			{File: "src/e.ts", Text: long}, {File: "src/f.ts", Text: long},
		}},
		Symbols: []CGPSymbolSummary{
			{Name: "A", File: "src/a.ts", Signature: long}, {Name: "B", File: "src/b.ts", Signature: long},
			{Name: "C", File: "src/c.ts", Signature: long}, {Name: "D", File: "src/d.ts", Signature: long},
			{Name: "E", File: "src/e.ts", Signature: long}, {Name: "F", File: "src/f.ts", Signature: long},
		},
		Context: FetchContextResponse{Status: "ok", Slices: []ContextSlice{
			{File: "src/primary.ts", Text: "primary evidence\n", EstimatedTokens: 4},
			{File: "src/secondary.ts", Text: "secondary evidence\n", EstimatedTokens: 4},
		}},
	}
	fitInspectFlowResponse(&resp, 800)
	if len(resp.Context.Slices) != 2 {
		t.Fatalf("distinct source evidence was removed before metadata tails: %#v", resp.Context.Slices)
	}
	if len(resp.Search.Hits) >= 6 || len(resp.Symbols) >= 6 {
		t.Fatalf("expected ranked metadata tails to be trimmed, got %d hits and %d symbols", len(resp.Search.Hits), len(resp.Symbols))
	}
}

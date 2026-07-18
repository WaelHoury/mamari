package mamari

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestRepoMapArchitectureFindsModulesAndOperationalSignals(t *testing.T) {
	idx := architectureFixtureIndex(t)
	resp := RepoMap(idx, RepoMapOptions{
		BudgetTokens:        1800,
		Limit:               20,
		SourceOnly:          true,
		IncludeArchitecture: true,
	})
	if resp.Status != "ok" || resp.Architecture == nil {
		t.Fatalf("expected architecture response, status=%s warnings=%v", resp.Status, resp.Warnings)
	}
	architecture := resp.Architecture
	if len(architecture.Languages) != 1 || architecture.Languages[0].Language != "javascript" || architecture.Languages[0].Files != 7 {
		t.Fatalf("unexpected language summary: %#v", architecture.Languages)
	}
	if !hasArchitecturePackage(architecture.Packages, "src/auth") || !hasArchitecturePackage(architecture.Packages, "src/billing") {
		t.Fatalf("expected auth and billing packages, got %#v", architecture.Packages)
	}
	if !hasArchitectureEntryPoint(architecture.EntryPoints, "main") {
		t.Fatalf("expected conventional main entry point, got %#v", architecture.EntryPoints)
	}
	if len(architecture.Routes) != 2 {
		t.Fatalf("expected two HTTP routes, got %#v", architecture.Routes)
	}
	if len(architecture.Hotspots) == 0 || architecture.Hotspots[0].File == "" {
		t.Fatalf("expected ranked hotspots, got %#v", architecture.Hotspots)
	}
	if len(architecture.Communities) < 2 {
		t.Fatalf("expected at least two graph communities, got %#v", architecture.Communities)
	}
	if len(architecture.Boundaries) == 0 {
		t.Fatalf("expected the cross-subsystem call to appear as a community boundary")
	}
	for _, community := range architecture.Communities {
		if community.Cohesion < 0 || community.Cohesion > 1 || len(community.Packages) == 0 || len(community.TopSymbols) == 0 || len(community.EdgeTypes) == 0 {
			t.Fatalf("community metadata is incomplete: %#v", community)
		}
		hasAuth, hasBilling := false, false
		for _, file := range community.Files {
			hasAuth = hasAuth || strings.HasPrefix(file, "src/auth/")
			hasBilling = hasBilling || strings.HasPrefix(file, "src/billing/")
		}
		if hasAuth && hasBilling {
			t.Fatalf("disconnected subsystems were mixed into one community: %#v", community)
		}
	}
	if !hasCommunityEdgeType(architecture.Communities, "calls") || !hasCommunityEdgeType(architecture.Communities, "imports") {
		t.Fatalf("expected typed community coupling, got %#v", architecture.Communities)
	}
	if architecture.Boundaries[0].Edges != 1 || !hasBoundaryEdgeType(architecture.Boundaries, "calls") {
		t.Fatalf("expected typed cross-community call boundary, got %#v", architecture.Boundaries)
	}
	if architecture.EstimatedTokens <= 0 || architecture.EstimatedTokens > 900 {
		t.Fatalf("architecture did not respect its budget: %#v", architecture)
	}

	second := RepoMap(idx, RepoMapOptions{
		BudgetTokens:        1800,
		Limit:               20,
		SourceOnly:          true,
		IncludeArchitecture: true,
	})
	// Compare the client-visible serialized output rather than the in-memory
	// structs: EstimatedTokens is an internal-only accounting field (json:"-")
	// that RepoMap's JSON-roundtrip cache clone intentionally does not carry, so
	// a fresh build and a cache hit can differ in that field while producing
	// byte-identical output. Determinism is a property of what the agent sees.
	firstJSON, _ := json.Marshal(resp.Architecture)
	secondJSON, _ := json.Marshal(second.Architecture)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("architecture output is not deterministic\nfirst:  %s\nsecond: %s", firstJSON, secondJSON)
	}
}

func TestRepoMapArchitectureCanBeDisabled(t *testing.T) {
	idx := architectureFixtureIndex(t)
	resp := RepoMap(idx, RepoMapOptions{BudgetTokens: 1200, Limit: 20})
	if resp.Architecture != nil {
		t.Fatalf("architecture should remain opt-in for direct Go callers: %#v", resp.Architecture)
	}
}

func TestRepoMapFileMentionMatchesAllGroupsInOnePass(t *testing.T) {
	symbols := map[string]CGPSymbol{
		"a": {ID: "a", Name: "compileDataset", Signature: "compileDataset()", File: "src/compiler.ts"},
		"b": {ID: "b", Name: "validateDataset", Signature: "validateDataset()", File: "src/compiler.ts"},
		"c": {ID: "c", Name: "renderView", Signature: "renderView()", File: "src/view.ts"},
	}
	mentions := repoMapMentions{"compile": 2, "dataset": 2, "render": 2}
	got := repoMapFileMentionMatchesAll(symbols, mentions)
	if !reflect.DeepEqual(got["src/compiler.ts"], []string{"compile", "dataset"}) {
		t.Fatalf("compiler matches=%v", got["src/compiler.ts"])
	}
	if !reflect.DeepEqual(got["src/view.ts"], []string{"render"}) {
		t.Fatalf("view matches=%v", got["src/view.ts"])
	}
	if legacy := repoMapFileMentionMatches(symbols, "src/compiler.ts", mentions); !reflect.DeepEqual(legacy, got["src/compiler.ts"]) {
		t.Fatalf("single-file wrapper=%v, grouped=%v", legacy, got["src/compiler.ts"])
	}
}

func TestRepoMapCacheClonesResponsesAndInvalidatesOnGraphMutation(t *testing.T) {
	idx := architectureFixtureIndex(t)
	opts := RepoMapOptions{
		BudgetTokens:        1200,
		Limit:               20,
		SourceOnly:          true,
		IncludeArchitecture: true,
		Query:               "auth billing architecture",
	}
	first := RepoMap(idx, opts)
	if first.Status != "ok" || len(first.Files) == 0 || first.Architecture == nil {
		t.Fatalf("expected cacheable repo map, got %#v", first)
	}
	idx.repoMapResultsMu.Lock()
	cachedEntries := len(idx.repoMapResults)
	idx.repoMapResultsMu.Unlock()
	if cachedEntries != 1 {
		t.Fatalf("repo map cache entries=%d, want 1", cachedEntries)
	}

	first.Files[0].File = "mutated-by-caller"
	first.Architecture.Languages = nil
	second := RepoMap(idx, opts)
	if second.Files[0].File == "mutated-by-caller" || len(second.Architecture.Languages) == 0 {
		t.Fatalf("caller mutation leaked into cached response: %#v", second)
	}

	var from CGPSymbol
	for _, symbol := range idx.Symbols {
		if symbol.File != "" {
			from = symbol
			break
		}
	}
	added := idx.AddCGPSymbol(CGPSymbol{
		ID:         "symbol:test:new-cache-file",
		Name:       "newCacheFile",
		Kind:       "function",
		Language:   "javascript",
		File:       "src/new-cache-file.js",
		StartLine:  1,
		EndLine:    1,
		Confidence: ConfExact,
	})
	idx.AddCGPEdge(from.ID, added.ID, "calls", ConfExact, Location{File: from.File, StartLine: from.StartLine})
	third := RepoMap(idx, opts)
	found := false
	for _, file := range third.Files {
		if file.File == added.File {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("graph mutation returned stale cached repo map: %#v", third.Files)
	}
}

func TestTrimRepoArchitecturePreservesCommunityBreadthBeforeDetails(t *testing.T) {
	architecture := RepoArchitecture{}
	for i := 1; i <= 6; i++ {
		architecture.Communities = append(architecture.Communities, RepoCommunity{
			ID: i, Name: "community", FileCount: 6,
			Files:      []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go"},
			Packages:   []string{"pkg/a", "pkg/b", "pkg/c"},
			TopSymbols: []string{"Alpha", "Beta", "Gamma"},
			EdgeTypes: []RepoCommunityEdgeType{
				{Type: "calls", InternalEdges: 8, ExternalEdges: 2, InternalWeight: 8, ExternalWeight: 2},
				{Type: "imports", InternalEdges: 4, ExternalEdges: 1, InternalWeight: 3.2, ExternalWeight: 0.8},
			},
			Cohesion: 0.8, InternalWeight: 12, ExternalWeight: 3,
		})
	}
	trimRepoArchitecture(&architecture, 600)
	if len(architecture.Communities) != 6 {
		t.Fatalf("expected detail trimming to preserve six communities, got %#v", architecture.Communities)
	}
	if !architecture.Truncated || architecture.EstimatedTokens > 600 {
		t.Fatalf("expected a bounded, explicitly truncated packet, got %#v", architecture)
	}
	if len(architecture.Communities[0].EdgeTypes) == 0 {
		t.Fatalf("expected highest-ranked community to retain typed coupling: %#v", architecture.Communities[0])
	}
	if len(architecture.Communities[5].EdgeTypes) != 0 {
		t.Fatalf("expected lower-ranked detail to trim before community rows: %#v", architecture.Communities[5])
	}
}

func TestRepoArchitectureRefinementSplitsDisconnectedCommunity(t *testing.T) {
	adj := []map[int]float64{
		{1: 1},
		{0: 1},
		{3: 1},
		{2: 1},
	}
	got := repoArchitectureRefineConnectedCommunities([]int{7, 7, 7, 7}, adj, []int{0, 1, 2, 3})
	if got[0] != got[1] || got[2] != got[3] || got[0] == got[2] {
		t.Fatalf("expected two connected communities, got %v", got)
	}
	want := []int{7, 7, 8, 8}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected deterministic component IDs %v, got %v", want, got)
	}
}

func TestRepoArchitectureConnectivityUsesVisibleSourceGraph(t *testing.T) {
	idx := &Index{
		SchemaVersion: SchemaVersion,
		Repo:          RepoInfo{Root: t.TempDir()},
		Files: map[string]File{
			"src/a.js":           {ID: "src/a.js", Path: "src/a.js", Language: "javascript", ParseStatus: ParseStatusOK},
			"src/bridge.test.js": {ID: "src/bridge.test.js", Path: "src/bridge.test.js", Language: "javascript", ParseStatus: ParseStatusOK},
			"src/b.js":           {ID: "src/b.js", Path: "src/b.js", Language: "javascript", ParseStatus: ParseStatusOK},
		},
		Prefixes: map[string]Prefix{}, Terms: map[string]Term{}, Shapes: map[string]Shape{},
		Symbols: map[string]CGPSymbol{
			"a":      {ID: "a", Name: "a", Kind: "function", File: "src/a.js", Language: "javascript", StartLine: 1, EndLine: 2},
			"bridge": {ID: "bridge", Name: "bridge", Kind: "function", File: "src/bridge.test.js", Language: "javascript", StartLine: 1, EndLine: 2},
			"b":      {ID: "b", Name: "b", Kind: "function", File: "src/b.js", Language: "javascript", StartLine: 1, EndLine: 2},
		},
		SymbolEdges: []CGPEdge{
			{ID: "a-bridge", From: "a", To: "bridge", Type: "calls", Confidence: ConfExact},
			{ID: "bridge-b", From: "bridge", To: "b", Type: "calls", Confidence: ConfExact},
		},
	}
	idx.mu.Lock()
	idx.initRuntimeLocked()
	idx.mu.Unlock()
	resp := RepoMap(idx, RepoMapOptions{BudgetTokens: 1200, Limit: 20, SourceOnly: true, IncludeArchitecture: true})
	if resp.Architecture == nil || len(resp.Architecture.Communities) != 2 {
		t.Fatalf("expected two visible production communities, got %#v", resp.Architecture)
	}
	for _, community := range resp.Architecture.Communities {
		for _, file := range community.Files {
			if strings.Contains(file, ".test.") {
				t.Fatalf("source-only architecture leaked test bridge: %#v", community)
			}
		}
		if community.FileCount != 1 {
			t.Fatalf("test-only bridge incorrectly connected production files: %#v", community)
		}
	}
}

func architectureFixtureIndex(t *testing.T) *Index {
	t.Helper()
	root := t.TempDir()
	files := []string{
		"src/auth/main.js", "src/auth/routes.js", "src/auth/service.js", "src/auth/store.js",
		"src/billing/routes.js", "src/billing/service.js", "src/billing/store.js",
	}
	idx := &Index{
		SchemaVersion: SchemaVersion,
		Repo:          RepoInfo{Root: root},
		Files:         map[string]File{},
		Prefixes:      map[string]Prefix{},
		Terms:         map[string]Term{},
		Shapes:        map[string]Shape{},
		Symbols:       map[string]CGPSymbol{},
	}
	for _, file := range files {
		idx.Files[file] = File{ID: file, Path: file, Language: "javascript", ParseStatus: ParseStatusOK}
	}
	add := func(id, name, kind, file string, complexity int) {
		idx.Symbols[id] = CGPSymbol{
			ID: id, Name: name, Kind: kind, Language: "javascript", File: file,
			StartLine: 1, EndLine: 3, Confidence: ConfExact, Complexity: complexity,
		}
	}
	add("auth-main", "main", "function", "src/auth/main.js", 2)
	add("auth-route", "POST /login", "http-route", "src/auth/routes.js", 0)
	add("auth-handler", "login", "function", "src/auth/routes.js", 4)
	add("auth-service", "issueToken", "function", "src/auth/service.js", 9)
	add("auth-store", "saveSession", "function", "src/auth/store.js", 6)
	add("billing-route", "POST /invoice", "http-route", "src/billing/routes.js", 0)
	add("billing-handler", "createInvoice", "function", "src/billing/routes.js", 3)
	add("billing-service", "chargeInvoice", "function", "src/billing/service.js", 8)
	add("billing-store", "saveInvoice", "function", "src/billing/store.js", 5)
	edge := func(id, from, to, kind string) {
		idx.SymbolEdges = append(idx.SymbolEdges, CGPEdge{ID: id, From: from, To: to, Type: kind, Confidence: ConfExact})
	}
	edge("a1", "auth-main", "auth-handler", "calls")
	edge("a2", "auth-route", "auth-handler", "handles-route")
	edge("a3", "auth-handler", "auth-service", "calls")
	edge("a4", "auth-service", "auth-store", "calls")
	edge("a5", "auth-main", "auth-store", "imports")
	edge("b1", "billing-route", "billing-handler", "handles-route")
	edge("b2", "billing-handler", "billing-service", "calls")
	edge("b3", "billing-service", "billing-store", "calls")
	edge("cross1", "auth-service", "billing-service", "calls")
	idx.mu.Lock()
	idx.initRuntimeLocked()
	idx.mu.Unlock()
	return idx
}

func hasArchitecturePackage(rows []RepoPackageSummary, want string) bool {
	for _, row := range rows {
		if row.Package == want {
			return true
		}
	}
	return false
}

func hasArchitectureEntryPoint(rows []RepoEntryPoint, want string) bool {
	for _, row := range rows {
		if row.Name == want {
			return true
		}
	}
	return false
}

func hasCommunityEdgeType(rows []RepoCommunity, want string) bool {
	for _, community := range rows {
		for _, edgeType := range community.EdgeTypes {
			if edgeType.Type == want && edgeType.InternalEdges+edgeType.ExternalEdges > 0 {
				return true
			}
		}
	}
	return false
}

func hasBoundaryEdgeType(rows []RepoBoundary, want string) bool {
	for _, boundary := range rows {
		for _, edgeType := range boundary.EdgeTypes {
			if edgeType.Type == want && edgeType.Edges > 0 {
				return true
			}
		}
	}
	return false
}

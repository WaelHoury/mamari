package mamari

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/pprof"
	"sort"
	"testing"
	"unsafe"
)

// TestLargeRepositoryHeapProfile is an opt-in real-repository workload for
// `go test -memprofile`. It stays skipped in ordinary CI; point
// MAMARI_PROFILE_INDEX at an existing index to reproduce the retained heap
// after warming both lexical search and source-rich flow exploration.
func TestLargeRepositoryHeapProfile(t *testing.T) {
	indexPath := os.Getenv("MAMARI_PROFILE_INDEX")
	if indexPath == "" {
		t.Skip("set MAMARI_PROFILE_INDEX to profile a real repository")
	}
	idx, err := LoadIndex(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	queries := []string{
		"request pipeline routing authentication sessions response errors",
		"server startup engine configuration plugins lifecycle shutdown",
		"websocket handshake frames sessions close errors",
		"client request pipeline redirects retries response validation",
		"routing resolution parameters middleware dispatch exceptions",
	}
	for round := 0; round < 6; round++ {
		for _, query := range queries {
			if _, err := InspectFlow(idx, query, InspectFlowOptions{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	runtime.GC()
	runtime.KeepAlive(idx)
	if outPath := os.Getenv("MAMARI_PROFILE_OUT"); outPath != "" {
		out, err := os.Create(outPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := pprof.WriteHeapProfile(out); err != nil {
			_ = out.Close()
			t.Fatal(err)
		}
		if err := out.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadedIndexCanonicalizesRepeatedGraphStrings(t *testing.T) {
	root := t.TempDir()
	write(t, root, "main.go", `package main
func target() {}
func first() { target() }
func second() { target() }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(root, ".mamari", "index.json")
	if err := SaveIndex(idx, indexPath); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadIndex(indexPath)
	if err != nil {
		t.Fatal(err)
	}

	var targetID string
	for id, symbol := range loaded.Symbols {
		if symbol.Name == "target" {
			targetID = id
			if unsafe.StringData(id) != unsafe.StringData(symbol.ID) {
				t.Fatal("symbol map key and symbol ID do not share canonical storage")
			}
			break
		}
	}
	if targetID == "" {
		t.Fatal("target symbol not indexed")
	}
	var targetPointers []uintptr
	for _, edge := range loaded.SymbolEdges {
		if edge.To == targetID {
			targetPointers = append(targetPointers, uintptr(unsafe.Pointer(unsafe.StringData(edge.To))))
		}
	}
	if len(targetPointers) != 2 {
		t.Fatalf("expected two calls to target, got %d", len(targetPointers))
	}
	want := uintptr(unsafe.Pointer(unsafe.StringData(targetID)))
	for _, pointer := range targetPointers {
		if pointer != want {
			t.Fatalf("edge target does not share canonical symbol ID storage: got %x want %x", pointer, want)
		}
	}
}

func TestCodeSearchLineTokensUseCompactExactSet(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/a.ts", "export function alpha() { return quasarlexeme }\n")
	write(t, root, "src/b.ts", "export function bravo() { return quasarlexeme }\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	SearchCode(idx, "quasarlexeme", SearchCodeOptions{})

	idx.mu.Lock()
	files := append([]codeSearchFile(nil), idx.codeSearchFiles...)
	idx.mu.Unlock()
	matches := 0
	for _, file := range files {
		for _, line := range file.lines {
			if line.tokens.contains("quasarlexeme") {
				matches++
			}
			if line.tokens.contains("quasar") {
				t.Fatal("compact token membership accepted a substring")
			}
		}
	}
	if matches != 2 {
		t.Fatalf("expected exact token in both files, got %d matches", matches)
	}
}

func TestCodeSearchPostingCompressionRoundTripsWithoutRangeLimits(t *testing.T) {
	refs := []codeSearchPosting{
		{fileID: 70000, lineIdx: 90000},
		{fileID: 1, lineIdx: 2},
		{fileID: 70000, lineIdx: 90001},
		{fileID: ^uint32(0), lineIdx: ^uint32(0)},
	}
	encoded := encodeCodeSearchPostings(append([]codeSearchPosting(nil), refs...))
	decoded := decodeCodeSearchPostings(encoded, nil)
	sort.Slice(refs, func(i, j int) bool {
		return codeSearchPostingKey(refs[i]) < codeSearchPostingKey(refs[j])
	})
	if !reflect.DeepEqual(decoded, refs) {
		t.Fatalf("posting compression changed values:\n got %#v\nwant %#v", decoded, refs)
	}

	dense := make([]codeSearchPosting, 1000)
	for i := range dense {
		dense[i] = codeSearchPosting{fileID: 42, lineIdx: uint32(i)}
	}
	if size := len(encodeCodeSearchPostings(dense)); size >= len(dense)*4 {
		t.Fatalf("dense posting list was not meaningfully compressed: %d bytes", size)
	}
}

func TestCodeSearchLineUsesFixedWidthSourceAndSymbolReferences(t *testing.T) {
	if got, wantMax := unsafe.Sizeof(codeSearchLine{}), uintptr(64); got > wantMax {
		t.Fatalf("codeSearchLine size=%d bytes, want <=%d", got, wantMax)
	}
}

func TestExactSymbolBoostRecognizesSpacedCompoundName(t *testing.T) {
	summary := &CGPSymbolSummary{
		Name:             "TraceSymbol",
		searchNameTokens: packCompactTokenSet(searchTextTokenSlice("TraceSymbol")),
	}
	boost := exactSymbolNameSearchBoost(
		[]*CGPSymbolSummary{summary},
		summary.searchNameTokens.count(),
		2,
		"trace symbol callers",
		[]string{"trace", "symbol", "caller"},
	)
	if boost < 520 {
		t.Fatalf("spaced compound symbol name did not receive exact boost: %d", boost)
	}
}

func TestIncrementalCodeSearchPostingIDsRemainUnique(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/a.ts", "export function alpha() { return 1 }\n")
	write(t, root, "src/b.ts", "export function bravo() { return 2 }\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	SearchCode(idx, "alpha bravo", SearchCodeOptions{})

	for _, name := range []string{"alphaSecond", "alphaThird"} {
		write(t, root, "src/a.ts", "export function "+name+"() { return 1 }\n")
		if _, _, err := rebakeChangedFiles(idx, root, []string{"src/a.ts"}, nil); err != nil {
			t.Fatal(err)
		}
	}

	idx.mu.Lock()
	ids := map[uint32]string{}
	for _, file := range idx.codeSearchFiles {
		if file.postingID == 0 {
			idx.mu.Unlock()
			t.Fatalf("zero posting id for %s", file.file)
		}
		if previous := ids[file.postingID]; previous != "" {
			idx.mu.Unlock()
			t.Fatalf("duplicate posting id %d for %s and %s", file.postingID, previous, file.file)
		}
		ids[file.postingID] = file.file
	}
	idx.mu.Unlock()

	for _, query := range []string{"alphaThird", "bravo"} {
		resp := SearchCode(idx, query, SearchCodeOptions{Limit: 4, BudgetTokens: 400})
		if resp.Status != "ok" || len(resp.Hits) == 0 {
			t.Fatalf("search %q failed after repeated incremental updates: %#v", query, resp)
		}
	}
}

func TestCodeSearchSidecarPreservesUniquePostingIDs(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/a.ts", "export function alpha() { return 1 }\n")
	write(t, root, "src/b.ts", "export function bravo() { return 2 }\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAMARI_PERSIST_SEARCH", "1")
	indexPath := filepath.Join(root, ".mamari", "index.json")
	if err := SaveIndex(idx, indexPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".mamari", "search.json")); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadIndex(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	SearchCode(loaded, "alpha bravo", SearchCodeOptions{})
	loaded.mu.Lock()
	defer loaded.mu.Unlock()
	ids := map[uint32]string{}
	for _, file := range loaded.codeSearchFiles {
		if file.postingID == 0 {
			t.Fatalf("zero posting id for %s", file.file)
		}
		if previous := ids[file.postingID]; previous != "" {
			t.Fatalf("duplicate posting id %d for %s and %s", file.postingID, previous, file.file)
		}
		ids[file.postingID] = file.file
	}
}

func TestNonWatchSearchPostingPathMatchesLinearFallback(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/router.ts", `export function dispatchRequest(route: Route) {
  return route.handle()
}
`)
	write(t, root, "src/errors.ts", `export function handleRequestError(error: Error) {
  return recover(error)
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	opts := SearchCodeOptions{Limit: 6, BudgetTokens: 800, ContextLines: 1, SourceOnly: true}
	withPostings := SearchCode(idx, "request route dispatch handle error", opts)

	idx.mu.Lock()
	postings := idx.codeSearchPostings
	idx.codeSearchPostings = nil
	idx.mu.Unlock()
	linear := SearchCode(idx, "request route dispatch handle error", opts)
	idx.mu.Lock()
	idx.codeSearchPostings = postings
	idx.mu.Unlock()

	if !reflect.DeepEqual(withPostings, linear) {
		t.Fatalf("posting and linear search paths differ:\npostings: %#v\nlinear:   %#v", withPostings, linear)
	}
}

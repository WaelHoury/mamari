package mamari

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"unsafe"
)

func TestCompactSymbolEdgesPreserveRecordsAndUseFixedWidthStorage(t *testing.T) {
	edges := completeV2SnapshotFixture().SymbolEdges
	store := newCompactSymbolEdgeStore(edges)
	if store == nil {
		t.Fatal("compact store rejected valid edges")
	}
	if got, want := unsafe.Sizeof(compactSymbolEdge{}), uintptr(48); got != want {
		t.Fatalf("compact edge size=%d, want %d", got, want)
	}
	if unsafe.Sizeof(compactSymbolEdge{}) >= unsafe.Sizeof(CGPEdge{})/2 {
		t.Fatalf("compact edge %d bytes is not less than half CGPEdge's %d bytes", unsafe.Sizeof(compactSymbolEdge{}), unsafe.Sizeof(CGPEdge{}))
	}
	if got := store.materialize(true); !reflect.DeepEqual(got, edges) {
		t.Fatalf("materialized compact edges differ\nwant %#v\n got %#v", edges, got)
	}
}

func TestCompactReadOnlyQueriesMatchExpandedGraph(t *testing.T) {
	root := t.TempDir()
	source := `
export function helper(value: string): string {
  return value.trim()
}
export function run(value: string): string {
  return helper(value)
}
`
	if err := os.WriteFile(filepath.Join(root, "main.ts"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".mamari", "index.json")
	if err := SaveIndex(idx, path); err != nil {
		t.Fatal(err)
	}
	expanded, err := LoadIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	compact, err := LoadIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	compact.compactReadOnlySymbolGraph()
	if compact.compactSymbolEdges == nil || len(compact.SymbolEdges) != 0 {
		t.Fatal("read-only compaction did not release the expanded edge slice")
	}

	type resultSet struct {
		Trace  TraceSymbolResponse
		Map    RepoMapResponse
		Dead   DeadCodeResponse
		Doctor DoctorReport
		Report ReportResponse
		Cypher QueryGraphLiteResponse
	}
	run := func(candidate *Index) resultSet {
		result := resultSet{
			Trace:  TraceSymbolWithOptions(candidate, "helper", TraceSymbolOptions{Compact: true, Sites: true}),
			Map:    RepoMap(candidate, RepoMapOptions{Limit: 20, BudgetTokens: 2000}),
			Dead:   DeadCode(candidate, DeadCodeOptions{}),
			Doctor: Doctor(candidate),
			Report: Report(candidate, ReportOptions{TopN: 10}),
			Cypher: QueryGraphLite(candidate, "MATCH (a)-[:CALLS]->(b) RETURN a.name, b.name LIMIT 20", QueryGraphLiteOptions{}),
		}
		// Doctor deliberately computes this from wall-clock time, so two
		// otherwise identical calls a few microseconds apart cannot match.
		result.Doctor.IndexAgeHours = 0
		return result
	}
	wantJSON, err := json.Marshal(run(expanded))
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, err := json.Marshal(run(compact))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("compact query results changed\nwant %s\n got %s", wantJSON, gotJSON)
	}
	if len(compact.SymbolEdges) != 0 || compact.compactSymbolEdges == nil {
		t.Fatal("read queries materialized the writable edge slice")
	}
	diff := DiffIndex(expanded, compact)
	if diff.Summary.EdgesAdded != 0 || diff.Summary.EdgesRemoved != 0 {
		t.Fatalf("expanded/compact diff reported edge changes: %#v", diff.Summary)
	}
}

func TestCompactGraphMaterializesOnlyOnFirstMutation(t *testing.T) {
	idx := publishedGraphTestIndex()
	original := append([]CGPEdge(nil), idx.SymbolEdges...)
	idx.compactReadOnlySymbolGraph()
	old := idx.symbolGraphSnapshot()
	if old.edgeCount() != len(original) || len(old.SymbolEdges) != 0 {
		t.Fatalf("compact published generation edges=%d expanded=%d", old.edgeCount(), len(old.SymbolEdges))
	}
	added := idx.AddCGPSymbol(CGPSymbol{ID: "symbol:new", Name: "new", Kind: "function", File: "new.go", StartLine: 1})
	idx.AddCGPEdge("symbol:root", "symbol:child", "references-symbol", ConfScoped, Location{File: "root.go", StartLine: 3})
	if idx.compactSymbolEdges != nil || len(idx.SymbolEdges) != len(original)+1 {
		t.Fatalf("first mutation did not materialize exactly once: compact=%v edges=%d", idx.compactSymbolEdges != nil, len(idx.SymbolEdges))
	}
	if old.edgeCount() != len(original) {
		t.Fatal("mutation changed the retained compact generation")
	}
	if _, ok := old.Symbols[added.ID]; ok {
		t.Fatal("mutation changed the retained compact symbol map")
	}
}

func TestSaveIndexFromCompactGraphPreservesEdges(t *testing.T) {
	idx := publishedGraphTestIndex()
	want := append([]CGPEdge(nil), idx.SymbolEdges...)
	idx.compactReadOnlySymbolGraph()
	path := filepath.Join(t.TempDir(), "index.bin")
	if err := SaveIndex(idx, path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.SymbolEdges, want) {
		t.Fatalf("saved compact edges changed\nwant %#v\n got %#v", want, loaded.SymbolEdges)
	}
}

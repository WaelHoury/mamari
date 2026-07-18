package mamari

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
)

var publishedGraphSnapshotSink int
var loadedGraphIndexSink *Index

func publishedGraphTestIndex() *Index {
	root := CGPSymbol{ID: "symbol:root", Name: "root", Kind: "function", File: "root.go", StartLine: 1}
	child := CGPSymbol{ID: "symbol:child", Name: "child", Kind: "function", File: "child.go", StartLine: 1}
	edge := CGPEdge{ID: "edge:root-child", From: root.ID, To: child.ID, Type: "calls", Confidence: ConfExact}
	idx := &Index{
		SchemaVersion: SchemaVersion,
		Files:         map[string]File{},
		Prefixes:      map[string]Prefix{},
		Terms:         map[string]Term{},
		Shapes:        map[string]Shape{},
		Symbols:       map[string]CGPSymbol{root.ID: root, child.ID: child},
		SymbolEdges:   []CGPEdge{edge},
	}
	idx.mu.Lock()
	idx.initRuntimeLocked()
	idx.mu.Unlock()
	idx.publishSymbolGraph()
	return idx
}

func TestPublishedSymbolGraphSnapshotDoesNotAllocate(t *testing.T) {
	idx := publishedGraphTestIndex()
	allocs := testing.AllocsPerRun(1000, func() {
		snap := idx.symbolGraphSnapshot()
		publishedGraphSnapshotSink = len(snap.Symbols) + snap.edgeCount()
	})
	if allocs != 0 {
		t.Fatalf("published snapshot allocated %.2f objects per read; want zero", allocs)
	}
}

func TestSymbolGraphMutationUsesCopyOnWriteGeneration(t *testing.T) {
	idx := publishedGraphTestIndex()
	old := idx.symbolGraphSnapshot()
	oldPointer := idx.publishedSymbolGraph.Load()

	idx.beginSymbolGraphMutation(true)
	added := idx.AddCGPSymbol(CGPSymbol{
		ID: "symbol:new", Name: "new", Kind: "function", File: "new.go", StartLine: 1,
	})
	idx.AddCGPEdge("symbol:root", added.ID, "calls", ConfExact, Location{File: "root.go", StartLine: 2})

	if got := idx.publishedSymbolGraph.Load(); got != oldPointer {
		t.Fatal("transaction replaced the published graph before commit")
	}
	visibleDuringMutation := idx.symbolGraphSnapshot()
	if _, ok := visibleDuringMutation.Symbols[added.ID]; ok {
		t.Fatal("reader observed an uncommitted symbol")
	}
	if _, ok := old.Symbols[added.ID]; ok || len(old.SymbolEdges) != 1 {
		t.Fatal("copy-on-write mutation changed the retained generation")
	}

	next := idx.publishSymbolGraph()
	if next.Generation <= old.Generation {
		t.Fatalf("generation did not advance: old=%d new=%d", old.Generation, next.Generation)
	}
	if _, ok := next.Symbols[added.ID]; !ok {
		t.Fatal("committed generation is missing the new symbol")
	}
	if len(next.SymbolEdges) != 2 {
		t.Fatalf("committed edge count=%d; want 2", len(next.SymbolEdges))
	}
	if &old.SymbolEdges[0] == &next.SymbolEdges[0] {
		t.Fatal("old and new generations share a mutable edge backing array")
	}
}

func TestStandaloneGraphMutationInvalidatesAndLazilyRepublishes(t *testing.T) {
	idx := publishedGraphTestIndex()
	old := idx.symbolGraphSnapshot()
	added := idx.AddCGPSymbol(CGPSymbol{
		ID: "symbol:standalone", Name: "standalone", Kind: "function", File: "standalone.go", StartLine: 1,
	})
	if idx.publishedSymbolGraph.Load() != nil {
		t.Fatal("standalone mutation left a stale generation published")
	}
	if got := findSymbols(idx, added.Name); len(got) != 1 || got[0].ID != added.ID {
		t.Fatalf("live lookup after mutation = %#v; want %s", got, added.ID)
	}
	next := idx.symbolGraphSnapshot()
	if _, ok := next.Symbols[added.ID]; !ok {
		t.Fatal("lazy publication omitted standalone mutation")
	}
	if _, ok := old.Symbols[added.ID]; ok {
		t.Fatal("standalone mutation changed retained generation")
	}
}

func TestConcurrentSymbolGraphReadersSeeCompleteGenerations(t *testing.T) {
	idx := publishedGraphTestIndex()
	var stop atomic.Bool
	errs := make(chan error, 1)
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for !stop.Load() {
				snap := idx.symbolGraphSnapshot()
				if !sort.StringsAreSorted(snap.OrderedSymbolIDs) {
					select {
					case errs <- fmt.Errorf("generation %d has unsorted IDs", snap.Generation):
					default:
					}
					return
				}
				for _, edge := range snap.SymbolEdges {
					if _, ok := snap.Symbols[edge.From]; !ok {
						select {
						case errs <- fmt.Errorf("generation %d edge has missing source %s", snap.Generation, edge.From):
						default:
						}
						return
					}
					if _, ok := snap.Symbols[edge.To]; !ok {
						select {
						case errs <- fmt.Errorf("generation %d edge has missing target %s", snap.Generation, edge.To):
						default:
						}
						return
					}
				}
			}
		}()
	}

	for generation := 0; generation < 100; generation++ {
		idx.beginSymbolGraphMutation(true)
		id := fmt.Sprintf("symbol:g%03d", generation)
		idx.AddCGPSymbol(CGPSymbol{ID: id, Name: id, Kind: "function", File: "generated.go", StartLine: generation + 1})
		idx.AddCGPEdge("symbol:root", id, "calls", ConfExact, Location{File: "root.go", StartLine: generation + 2})
		idx.publishSymbolGraph()
	}
	stop.Store(true)
	readers.Wait()
	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}
}

func TestLoadedReadOnlyIndexLazilyInitializesMutationState(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
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
	loaded, err := LoadIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.publishedSymbolGraph.Load() == nil {
		t.Fatal("loaded index did not publish its read-only graph")
	}
	if loaded.symbolSeen != nil || loaded.symbolEdgeSeen != nil || loaded.referenceSeen != nil || loaded.edgeSeen != nil || loaded.termLocationSeen != nil {
		t.Fatal("loaded read-only index eagerly initialized mutation-only dedup state")
	}

	beforeSymbols := len(loaded.Symbols)
	beforeEdges := len(loaded.SymbolEdges)
	var existing CGPSymbol
	for _, sym := range loaded.Symbols {
		existing = sym
		break
	}
	if existing.ID == "" {
		t.Fatal("fixture produced no symbols")
	}
	loaded.AddCGPSymbol(existing)
	if len(loaded.Symbols) != beforeSymbols {
		t.Fatal("lazy dedup initialization duplicated an existing symbol")
	}
	if beforeEdges > 0 {
		loaded.mu.Lock()
		edge := loaded.SymbolEdges[0]
		loaded.mu.Unlock()
		loaded.AddCGPEdgeWithReason(edge.From, edge.To, edge.Type, edge.Confidence, edge.UnresolvedReason, edge.Evidence)
		if len(loaded.SymbolEdges) != beforeEdges {
			t.Fatal("lazy dedup initialization duplicated an existing edge")
		}
	}
	if loaded.symbolSeen == nil || loaded.symbolEdgeSeen == nil {
		t.Fatal("first graph mutation did not initialize dedup state")
	}
}

func BenchmarkPublishedSymbolGraphSnapshot(b *testing.B) {
	idx := publishedGraphTestIndex()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snap := idx.symbolGraphSnapshot()
		publishedGraphSnapshotSink = len(snap.Symbols) + snap.edgeCount()
	}
}

// BenchmarkLoadedIndexGraphSnapshot is an opt-in production-corpus memory
// check. Point MAMARI_BENCH_INDEX at any persisted index; ordinary test runs
// skip it, while maintainers can combine it with -benchmem/-memprofile without
// baking a third-party repository or machine-specific threshold into CI.
func BenchmarkLoadedIndexGraphSnapshot(b *testing.B) {
	path := os.Getenv("MAMARI_BENCH_INDEX")
	if path == "" {
		b.Skip("set MAMARI_BENCH_INDEX to a persisted index")
	}
	idx, err := LoadIndex(path)
	if err != nil {
		b.Fatal(err)
	}
	idx.ReleaseUnusedMemory()
	loadedGraphIndexSink = idx
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snap := idx.symbolGraphSnapshot()
		publishedGraphSnapshotSink = len(snap.Symbols) + snap.edgeCount()
	}
	b.StopTimer()
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	b.ReportMetric(float64(memory.HeapAlloc)/(1024*1024), "heap_MiB")
	b.ReportMetric(float64(memory.HeapInuse)/(1024*1024), "heap_inuse_MiB")
}

// BenchmarkLoadedIndexWarmSearchMemory is the long-session companion to the
// graph-only benchmark above. It builds the lazy search cache once, forces a
// GC, and reports the retained heap independently from allocator/RSS slack.
func BenchmarkLoadedIndexWarmSearchMemory(b *testing.B) {
	path := os.Getenv("MAMARI_BENCH_INDEX")
	if path == "" {
		b.Skip("set MAMARI_BENCH_INDEX to a persisted index")
	}
	idx, err := LoadIndex(path)
	if err != nil {
		b.Fatal(err)
	}
	idx.ReleaseUnusedMemory()
	query := os.Getenv("MAMARI_BENCH_QUERY")
	if query == "" {
		query = "validation execution lifecycle"
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = SearchCode(idx, query, SearchCodeOptions{Limit: 8, BudgetTokens: 1800, ContextLines: 2, SourceOnly: true})
	}
	b.StopTimer()
	runtime.GC()
	loadedGraphIndexSink = idx
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	b.ReportMetric(float64(memory.HeapAlloc)/(1024*1024), "heap_MiB")
	b.ReportMetric(float64(memory.HeapInuse)/(1024*1024), "heap_inuse_MiB")
	b.ReportMetric(float64(len(idx.codeSearchFiles)), "search_files")
	lines := 0
	postingBytes := 0
	for _, file := range idx.codeSearchFiles {
		lines += len(file.lines)
	}
	for _, posting := range idx.codeSearchPostings {
		postingBytes += len(posting)
	}
	b.ReportMetric(float64(lines), "search_lines")
	b.ReportMetric(float64(postingBytes)/(1024*1024), "posting_MiB")
}

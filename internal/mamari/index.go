package mamari

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"
)

var indexBinaryMagicV1 = []byte("mamari-index-v1\n")

// BuildIndex walks the repository, runs the per-language scanners, and
// produces a CGP-ready Index. File scans are parallelized: independent files
// run on a worker pool sized to runtime.NumCPU(), with all writes to the
// shared Index serialized through Index.mu (each Add* method takes the lock
// internally).
func BuildIndex(repo string) (*Index, error) {
	root, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	idx := &Index{
		SchemaVersion: SchemaVersion,
		Repo: RepoInfo{
			Root:      root,
			IndexedAt: time.Now().UTC().Format(time.RFC3339),
			GitCommit: gitCommit(root),
		},
		Files:    map[string]File{},
		Prefixes: map[string]Prefix{},
		Terms:    map[string]Term{},
		Shapes:   map[string]Shape{},
		Symbols:  map[string]CGPSymbol{},
	}
	idx.literalsLoaded = true
	idx.mu.Lock()
	idx.initRuntimeLocked()
	idx.mu.Unlock()

	files, err := WalkRepo(root)
	if err != nil {
		return nil, err
	}

	// Phase 1: read every file once and commit File metadata.
	contents, err := readAllFiles(idx, root, files)
	if err != nil {
		return nil, err
	}

	// Phase 2 (sequential): TTL — prefixes and shapes have to be globally
	// available before code-reference resolution can run, and the TTL
	// scanner mutates shared term/prefix maps. Doing it serially is cheap
	// and keeps the cross-file resolution stable.
	for _, rel := range files {
		switch idx.languageFor(rel) {
		case "ttl":
			if err := ScanTTL(idx, rel, contents[rel]); err != nil {
				return nil, err
			}
		}
	}

	// Phase 3: collect per-file local namespaces and imports for code refs.
	codeFiles := filterByLanguage(files, idx, "typescript", "javascript", "vue")
	cgpFiles := filterByLanguage(files, idx, append([]string{"typescript", "javascript", "vue", "python", "java", "go", "csharp", "rust", "ruby", "php", "c", "cpp", "kotlin", "bash", "scala", "lua", "elixir", "dart", "haskell", "clojure", "swift", "r", "julia", "zig", "ocaml", "hcl", "yaml", "dockerfile"}, heuristicLanguages...)...)
	codeFileSet := make(map[string]bool, len(codeFiles))
	fileLocals := make(map[string]map[string]namespaceEntry, len(codeFiles))
	fileImports := make(map[string][]importStmt, len(codeFiles))
	for _, rel := range codeFiles {
		codeFileSet[rel] = true
		fileLocals[rel] = fileLocalNamespaces(rel, contents[rel])
		fileImports[rel] = collectImports(rel, contents[rel])
	}
	idx.setCodeNamespaceCache(fileLocals, fileImports)
	effective := resolveEffectiveNamespaces(fileLocals, fileImports, codeFileSet)

	// Phase 4 (parallel): code references and dynamic IRI calls.
	idx.beginCodeScanSnapshot()
	err = runParallel(codeFiles, func(rel string) error {
		ScanCode(idx, rel, contents[rel], effective[rel])
		dyn := ScanDynamicIRICalls(rel, contents[rel])
		if len(dyn) > 0 {
			idx.appendDynamicIRICalls(dyn)
		}
		return nil
	})
	idx.endCodeScanSnapshot()
	if err != nil {
		return nil, err
	}

	// Phase 5 (parallel): CGP symbol extraction.
	if err := runParallel(cgpFiles, func(rel string) error {
		ScanCGPSymbols(idx, rel, idx.languageFor(rel), contents[rel])
		return nil
	}); err != nil {
		return nil, err
	}
	AddCGPFromTTL(idx)
	idx.invalidateFileSymbolIndex()
	idx.invalidateCodeSearchIndex()
	idx.ensureFileSymbolIndex()
	// Must run after every file's Phase 5 symbol extraction above has
	// completed (so a method's receiver-type class is guaranteed to exist
	// in idx.Symbols if it exists anywhere in the repo) and before
	// anything below that depends on a method's ParentID being correct —
	// see resolveOutOfLineMethodParents' own doc comment for the C++
	// header/cpp out-of-line-definition case this fixes.
	resolveOutOfLineMethodParents(idx)
	linkAllTemplateClassUsages(idx)

	// Phase 5.5 (parallel): approximate cyclomatic-complexity and hot-path
	// (loop depth / linear-scan-in-loop / alloc-in-loop / recursion-in-loop)
	// scores per symbol, computed from the symbol ranges established in
	// Phase 5.
	if err := runParallel(cgpFiles, func(rel string) error {
		// Mask strings/comments once and share the lines across all three
		// annotators (they masked the same file independently before).
		lines := maskedSourceLines(idx, rel, contents[rel])
		annotateComplexityLines(idx, rel, lines)
		annotateHotPathSignalsLines(idx, rel, lines)
		annotateShapeHashLines(idx, rel, lines)
		return nil
	}); err != nil {
		return nil, err
	}

	// Detect import-path aliases (tsconfig/jsconfig paths, vite alias) before
	// resolving imports, so `@/…`/`~/…`/custom-prefix specs resolve to the
	// right source directory rather than failing.
	detectAliasRules(idx, contents)

	// Phase 6 (parallel): CGP edges (imports + calls).
	if err := runParallel(cgpFiles, func(rel string) error {
		ScanCGPRelations(idx, rel, idx.languageFor(rel), contents[rel])
		return nil
	}); err != nil {
		return nil, err
	}

	// Phase 7: framework facts. Route symbols must be registered before
	// frontend/backend HTTP call edges can resolve to them, so this runs as a
	// small two-step overlay after the base symbol/call graph is available.
	if err := runParallel(codeFiles, func(rel string) error {
		ScanFrameworkSymbols(idx, rel, idx.languageFor(rel), contents[rel])
		return nil
	}); err != nil {
		return nil, err
	}
	idx.invalidateFileSymbolIndex()
	idx.invalidateCodeSearchIndex()
	idx.ensureFileSymbolIndex()
	if err := runParallel(codeFiles, func(rel string) error {
		ScanFrameworkRelations(idx, rel, idx.languageFor(rel), contents[rel])
		return nil
	}); err != nil {
		return nil, err
	}

	idx.mu.Lock()
	sortReferences(idx.References)
	sortEdges(idx.Edges)
	sortDynamicIRICalls(idx.DynamicIRICalls)
	idx.mu.Unlock()
	sortCGP(idx)
	propagateTransitiveLoopDepth(idx)
	idx.publishSymbolGraph()
	return idx, nil
}

// readAllFiles reads every file once into memory and commits a File entry
// for each. File entries (path, language, sha256, lineCount) are computed on
// worker goroutines; commits to the Index are serialized through Index.mu.
func readAllFiles(idx *Index, root string, files []string) (map[string]string, error) {
	contents := make(map[string]string, len(files))
	type result struct {
		rel     string
		content string
		entry   File
	}
	jobs := make(chan string, len(files))
	out := make(chan result, len(files))
	errs := make(chan error, 1)

	workers := runtime.NumCPU()
	if workers < 2 {
		workers = 2
	}
	if workers > len(files) {
		workers = len(files)
	}
	if workers < 1 {
		workers = 1
	}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rel := range jobs {
				abs := filepath.Join(root, rel)
				data, err := os.ReadFile(abs)
				if err != nil {
					select {
					case errs <- err:
					default:
					}
					return
				}
				str := string(data)
				lang := languageForContent(rel, data)
				out <- result{
					rel:     rel,
					content: str,
					entry: File{
						ID:          "file:" + filepath.ToSlash(rel),
						Path:        filepath.ToSlash(rel),
						Language:    lang,
						SHA256:      hash(data),
						LineCount:   countLines(str),
						Parser:      parserFor(lang),
						ParseStatus: ParseStatusOK,
					},
				}
			}
		}()
	}
	go func() {
		for _, rel := range files {
			jobs <- rel
		}
		close(jobs)
		wg.Wait()
		close(out)
	}()
	for r := range out {
		contents[r.rel] = r.content
		idx.commitFile(r.entry)
	}
	select {
	case err := <-errs:
		return nil, err
	default:
	}
	return contents, nil
}

// runParallel runs work on every relative path with NumCPU workers. The first
// error short-circuits the rest.
func runParallel(rels []string, work func(rel string) error) error {
	if len(rels) == 0 {
		return nil
	}
	workers := runtime.NumCPU()
	if workers < 2 {
		workers = 2
	}
	if workers > len(rels) {
		workers = len(rels)
	}
	jobs := make(chan string, len(rels))
	for _, rel := range rels {
		jobs <- rel
	}
	close(jobs)

	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rel := range jobs {
				if err := work(rel); err != nil {
					select {
					case errs <- err:
					default:
					}
					return
				}
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-errs:
		return err
	default:
		return nil
	}
}

func filterByLanguage(files []string, idx *Index, langs ...string) []string {
	want := map[string]bool{}
	for _, l := range langs {
		want[l] = true
	}
	out := make([]string, 0, len(files))
	for _, rel := range files {
		if want[idx.languageFor(rel)] {
			out = append(out, rel)
		}
	}
	return out
}

// languageFor on Index commits the file entry on first observation. This lets
// the parallel reader register File entries lazily; we keep the side-effect
// here rather than in the read goroutine because Index.mu is held.
func (idx *Index) languageFor(rel string) string {
	idx.mu.Lock()
	if existing, ok := idx.Files[rel]; ok {
		idx.mu.Unlock()
		return existing.Language
	}
	idx.mu.Unlock()
	return languageFor(rel)
}

func (idx *Index) commitFile(entry File) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.Files[entry.Path] = entry
}

func (idx *Index) appendDynamicIRICalls(calls []DynamicIRICall) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.DynamicIRICalls = append(idx.DynamicIRICalls, calls...)
}

func (idx *Index) dynamicIRICallCount() int {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return len(idx.DynamicIRICalls)
}

func (idx *Index) prefixIRI(prefix string) (string, bool) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	p, ok := idx.Prefixes[prefix]
	return p.IRI, ok
}

// prefixNamesSnapshot returns a lowercase set of every registered RDF prefix
// (both repo-wide and any unique file-scoped prefix). Used by query
// normalization to detect adjacency patterns like "sh in" -> "sh:in".
func (idx *Index) prefixNamesSnapshot() map[string]bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return prefixNamesFrom(idx.Prefixes, idx.Files)
}

func prefixNamesFrom(prefixes map[string]Prefix, files map[string]File) map[string]bool {
	out := make(map[string]bool, len(prefixes))
	for name := range prefixes {
		if name == "" {
			continue
		}
		out[strings.ToLower(name)] = true
	}
	for _, file := range files {
		for name := range file.Prefixes {
			if name == "" {
				continue
			}
			out[strings.ToLower(name)] = true
		}
	}
	return out
}

func sortDynamicIRICalls(calls []DynamicIRICall) {
	sort.Slice(calls, func(i, j int) bool {
		if calls[i].File != calls[j].File {
			return calls[i].File < calls[j].File
		}
		if calls[i].Line != calls[j].Line {
			return calls[i].Line < calls[j].Line
		}
		return calls[i].Column < calls[j].Column
	})
}

func SaveIndex(idx *Index, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	sidecar := filepath.Join(filepath.Dir(path), "literals.jsonl")
	searchSidecar := filepath.Join(filepath.Dir(path), "search.json")
	semanticSidecar := filepath.Join(filepath.Dir(path), "semantic.gob")
	idx.mu.Lock()
	literals := append([]Literal(nil), idx.Literals...)
	snapshot := idx.persistenceSnapshotLocked()
	shouldWriteLiterals := idx.literalsLoaded || idx.literalSidecarPath == ""
	idx.mu.Unlock()
	if shouldWriteLiterals {
		if err := saveLiteralsSidecar(literals, sidecar); err != nil {
			return err
		}
	} else if idx.literalSidecarPath != sidecar {
		if err := copyFileIfExists(idx.literalSidecarPath, sidecar); err != nil {
			return err
		}
	}

	data, err := marshalBinaryIndexV2(snapshot)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	idx.mu.Lock()
	idx.semanticSidecarPath = semanticSidecar
	hasSemantic := idx.semanticIndex != nil
	idx.mu.Unlock()
	if hasSemantic {
		if err := saveSemanticSidecar(idx, semanticSidecar); err != nil {
			return err
		}
	}
	if os.Getenv("MAMARI_PERSIST_SEARCH") == "1" {
		return saveCodeSearchSidecar(idx, searchSidecar)
	}
	if err := os.Remove(searchSidecar); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// SaveIndexJSON writes idx as plain, deterministically-key-ordered (Go's
// json package sorts map keys) indented JSON, with no gob and no gzip —
// unlike SaveIndex's compact binary snapshot, meant to be committed into a
// project's own git history (see `mamari hooks install` / `index --commit`)
// where reviewability and stable diffs matter more than load speed or file
// size. The literals sidecar is written alongside it as plain JSONL, same
// as SaveIndex's default sidecar format. Loadable by LoadIndex exactly like
// any other plain-JSON index (see LoadIndex's gzip/binary-magic sniffing).
func SaveIndexJSON(idx *Index, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	idx.mu.Lock()
	literals := append([]Literal(nil), idx.Literals...)
	snapshot := idx.persistenceSnapshotLocked()
	idx.mu.Unlock()

	sidecar := filepath.Join(filepath.Dir(path), "literals.jsonl")
	if err := saveLiteralsSidecar(literals, sidecar); err != nil {
		return err
	}

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// indexSnapshot mirrors the JSON-serialized fields of Index without the
// sync.Mutex, so vet does not warn about copying a lock value when the
// snapshot is constructed.
type indexSnapshot struct {
	SchemaVersion      int                  `json:"schemaVersion"`
	Repo               RepoInfo             `json:"repo"`
	Files              map[string]File      `json:"files"`
	Prefixes           map[string]Prefix    `json:"prefixes"`
	Terms              map[string]Term      `json:"terms"`
	Shapes             map[string]Shape     `json:"shapes"`
	References         []Reference          `json:"references"`
	Edges              []Edge               `json:"edges"`
	DynamicIRICalls    []DynamicIRICall     `json:"dynamicIriCalls,omitempty"`
	Symbols            map[string]CGPSymbol `json:"symbols,omitempty"`
	SymbolEdges        []CGPEdge            `json:"symbolEdges,omitempty"`
	compactSymbolEdges *compactSymbolEdgeStore
}

func (idx *Index) snapshotLocked() indexSnapshot {
	files := make(map[string]File, len(idx.Files))
	for k, v := range idx.Files {
		if v.Prefixes != nil {
			prefixes := make(map[string]string, len(v.Prefixes))
			for pk, pv := range v.Prefixes {
				prefixes[pk] = pv
			}
			v.Prefixes = prefixes
		}
		files[k] = v
	}
	prefixes := make(map[string]Prefix, len(idx.Prefixes))
	for k, v := range idx.Prefixes {
		prefixes[k] = v
	}
	terms := make(map[string]Term, len(idx.Terms))
	for k, v := range idx.Terms {
		v.Locations = append([]Location(nil), v.Locations...)
		terms[k] = v
	}
	shapes := make(map[string]Shape, len(idx.Shapes))
	for k, v := range idx.Shapes {
		v.TargetClasses = append([]ShapeLink(nil), v.TargetClasses...)
		v.Paths = append([]ShapeLink(nil), v.Paths...)
		v.Nodes = append([]ShapeLink(nil), v.Nodes...)
		v.Predicates = append([]ShapeLink(nil), v.Predicates...)
		v.Branches = append([]Branch(nil), v.Branches...)
		v.Names = append([]Literal(nil), v.Names...)
		v.Unsupported = append([]Location(nil), v.Unsupported...)
		shapes[k] = v
	}
	symbols := make(map[string]CGPSymbol, len(idx.Symbols))
	for k, v := range idx.Symbols {
		symbols[k] = v
	}
	return indexSnapshot{
		SchemaVersion:      idx.SchemaVersion,
		Repo:               idx.Repo,
		Files:              files,
		Prefixes:           prefixes,
		Terms:              terms,
		Shapes:             shapes,
		References:         append([]Reference(nil), idx.References...),
		Edges:              append([]Edge(nil), idx.Edges...),
		DynamicIRICalls:    append([]DynamicIRICall(nil), idx.DynamicIRICalls...),
		Symbols:            symbols,
		SymbolEdges:        append([]CGPEdge(nil), idx.SymbolEdges...),
		compactSymbolEdges: idx.compactSymbolEdges,
	}
}

func (idx *Index) persistenceSnapshotLocked() indexSnapshot {
	snapshot := idx.snapshotLocked()
	if snapshot.compactSymbolEdges != nil {
		snapshot.SymbolEdges = snapshot.compactSymbolEdges.materialize(true)
		snapshot.compactSymbolEdges = nil
	}
	return snapshot
}

func (idx *Index) snapshot() indexSnapshot {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return idx.snapshotLocked()
}

// symbolGraphSnapshot is an immutable, atomically published graph generation.
// Maps and slices are shared by every reader of a generation and must never be
// mutated after publication. Writers call beginSymbolGraphMutation before
// changing the live graph, which detaches it once per mutation batch; readers
// can therefore retain an old generation without locks while a watcher builds
// the next one.
type symbolGraphSnapshot struct {
	Generation         uint64
	Symbols            map[string]CGPSymbol
	SymbolEdges        []CGPEdge
	CompactSymbolEdges *compactSymbolEdgeStore
	OrderedSymbolIDs   []string
	SymbolsByName      map[string][]string
}

func cloneSymbolGraphData(symbols map[string]CGPSymbol, edges []CGPEdge) (map[string]CGPSymbol, []CGPEdge) {
	cloned := make(map[string]CGPSymbol, len(symbols))
	for k, v := range symbols {
		cloned[k] = v
	}
	return cloned, append([]CGPEdge(nil), edges...)
}

// beginSymbolGraphMutation detaches the live graph from its published
// generation exactly once for a mutation batch. preservePublished is true for
// watcher/SCIP transactions: concurrent readers keep seeing the last complete
// generation until publishSymbolGraph commits the replacement. A standalone
// AddCGPSymbol/AddCGPEdge call passes false, invalidating the pointer so the
// next graph query lazily publishes the newly mutated state.
func (idx *Index) beginSymbolGraphMutation(preservePublished bool) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.beginSymbolGraphMutationLocked(preservePublished)
}

func (idx *Index) beginSymbolGraphMutationLocked(preservePublished bool) {
	if idx.symbolGraphMutable {
		return
	}
	if idx.compactSymbolEdges != nil {
		// The published generation owns the compact store. This fresh slice is
		// already detached, so cloning it again would only double the first
		// watch mutation's transient memory. Symbols still share the published
		// map, however, and must be copied before AddCGPSymbol can mutate it.
		clonedSymbols := make(map[string]CGPSymbol, len(idx.Symbols))
		for id, symbol := range idx.Symbols {
			clonedSymbols[id] = symbol
		}
		idx.Symbols = clonedSymbols
		idx.SymbolEdges = idx.compactSymbolEdges.materialize(true)
		idx.compactSymbolEdges = nil
		idx.symbolsByFile = nil
		idx.symbolsByName = nil
		idx.childrenByParent = nil
		idx.symbolIndexBuilt = false
		idx.orderedSymbolIDs = nil
		if !preservePublished {
			idx.publishedSymbolGraph.Store(nil)
		}
		idx.symbolGraphMutable = true
		return
	}
	published := idx.publishedSymbolGraph.Load()
	if published != nil {
		idx.Symbols, idx.SymbolEdges = cloneSymbolGraphData(idx.Symbols, idx.SymbolEdges)
		// Runtime lookup indexes contain value copies from the old generation.
		// Drop them now; the existing lazy builder recreates them from the new
		// live graph when a mutation phase needs them.
		idx.symbolsByFile = nil
		idx.symbolsByName = nil
		idx.childrenByParent = nil
		idx.symbolIndexBuilt = false
		idx.orderedSymbolIDs = nil
		if !preservePublished {
			idx.publishedSymbolGraph.Store(nil)
		}
	}
	idx.symbolGraphMutable = true
}

// publishSymbolGraph freezes the current live graph as a new generation.
// It runs on build/load/update goroutines, never on a query's hot path except
// for the small-index/library fallback where no generation has been published
// yet. Publication itself is one atomic pointer swap.
func (idx *Index) publishSymbolGraph() symbolGraphSnapshot {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return idx.publishSymbolGraphLocked()
}

func (idx *Index) publishSymbolGraphLocked() symbolGraphSnapshot {
	ids := make([]string, 0, len(idx.Symbols))
	byName := make(map[string][]string, len(idx.Symbols)/2+1)
	for id, sym := range idx.Symbols {
		ids = append(ids, id)
		byName[sym.Name] = append(byName[sym.Name], id)
	}
	sort.Strings(ids)
	for name := range byName {
		sort.Strings(byName[name])
	}
	idx.orderedSymbolIDs = ids
	idx.symbolGraphGeneration++
	next := &symbolGraphSnapshot{
		Generation:         idx.symbolGraphGeneration,
		Symbols:            idx.Symbols,
		SymbolEdges:        idx.SymbolEdges,
		CompactSymbolEdges: idx.compactSymbolEdges,
		OrderedSymbolIDs:   ids,
		SymbolsByName:      byName,
	}
	idx.publishedSymbolGraph.Store(next)
	idx.symbolGraphMutable = false
	return *next
}

func (idx *Index) symbolGraphSnapshot() symbolGraphSnapshot {
	if published := idx.publishedSymbolGraph.Load(); published != nil {
		return *published
	}
	return idx.publishSymbolGraph()
}

func (idx *Index) orderedSymbolGraphSnapshot() symbolGraphSnapshot {
	return idx.symbolGraphSnapshot()
}

func LoadIndex(path string) (*Index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if bytes.HasPrefix(data, indexBinaryMagicV2) {
		idx, err := unmarshalBinaryIndexV2(data)
		if err != nil {
			return nil, err
		}
		finalizeLoadedIndex(idx, path)
		return idx, nil
	}
	if bytes.HasPrefix(data, indexBinaryMagicV1) {
		idx, err := unmarshalBinaryIndexV1(data)
		if err != nil {
			return nil, err
		}
		finalizeLoadedIndex(idx, path)
		return idx, nil
	}
	data, err = maybeGunzipBytes(data)
	if err != nil {
		return nil, err
	}
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, err
	}
	if idx.SchemaVersion != SchemaVersion {
		return nil, unsupportedIndexSchemaError(idx.SchemaVersion)
	}
	finalizeLoadedIndex(&idx, path)
	return &idx, nil
}

func marshalBinaryIndexV1(snapshot indexSnapshot) ([]byte, error) {
	var buf bytes.Buffer
	buf.Write(indexBinaryMagicV1)
	if err := gob.NewEncoder(&buf).Encode(snapshot); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func unmarshalBinaryIndexV1(data []byte) (*Index, error) {
	var snapshot indexSnapshot
	if err := gob.NewDecoder(bytes.NewReader(data[len(indexBinaryMagicV1):])).Decode(&snapshot); err != nil {
		return nil, err
	}
	idx := &Index{
		SchemaVersion:   snapshot.SchemaVersion,
		Repo:            snapshot.Repo,
		Files:           snapshot.Files,
		Prefixes:        snapshot.Prefixes,
		Terms:           snapshot.Terms,
		Shapes:          snapshot.Shapes,
		References:      snapshot.References,
		Edges:           snapshot.Edges,
		DynamicIRICalls: snapshot.DynamicIRICalls,
		Symbols:         snapshot.Symbols,
		SymbolEdges:     snapshot.SymbolEdges,
	}
	if idx.SchemaVersion != SchemaVersion {
		return nil, unsupportedIndexSchemaError(idx.SchemaVersion)
	}
	return idx, nil
}

func unsupportedIndexSchemaError(got int) error {
	return fmt.Errorf(
		"index schemaVersion %d is not supported by mamari schemaVersion %d; rebuild the index with `mamari index`",
		got,
		SchemaVersion,
	)
}

func finalizeLoadedIndex(idx *Index, path string) {
	idx.literalSidecarPath = filepath.Join(filepath.Dir(path), "literals.jsonl")
	idx.codeSearchSidecarPath = filepath.Join(filepath.Dir(path), "search.json")
	idx.semanticSidecarPath = filepath.Join(filepath.Dir(path), "semantic.gob")
	if len(idx.Literals) > 0 {
		idx.literalsLoaded = true
	}
	if !idx.indexStringsInterned {
		compactLoadedIndexStrings(idx)
	}
	// Do not eagerly rebuild mutation-only dedup tables here. A loaded MCP
	// server may remain read-only for its entire lifetime; symbolEdgeSeen,
	// referenceSeen, termLocationSeen, and related maps can be large enough to
	// materially inflate its baseline RSS. Every mutation entry point already
	// calls initRuntimeLocked before adding evidence, and watcher deletion is
	// safe against nil maps, so the first actual edit initializes these tables
	// from the then-current graph instead. Small resolution indexes remain
	// eager because read APIs such as compiler-backed extension-method lookup
	// use them directly after loading.
	idx.mu.Lock()
	idx.initResolutionRuntimeLocked()
	idx.mu.Unlock()
	idx.publishSymbolGraph()
}

// ReleaseUnusedMemory prepares an index for a long-running read-mostly MCP/UI
// process. It replaces the expanded CGPEdge slice with fixed-width numeric
// records, publishes that immutable graph generation, and returns decode/build
// scratch pages to the OS. A later watcher mutation transparently materializes
// a writable edge slice once and preserves the old generation for concurrent
// readers. Call this once after every index has loaded and before requests are
// accepted; one-shot commands gain nothing because their process exits.
//
// An earlier version of this fix also called this (gated, backgrounded,
// once-per-cache-latched) after search_code's and the semantic index's own
// first builds, which independently add real RSS too. Removed after
// measuring on Linux that their incremental contribution to *sustained*
// (post-warm-up) RSS was small — within normal run-to-run noise — because
// Linux's background scavenger already reclaims most of that churn on its
// own within the time it takes to serve a few more requests, unlike the
// initial load's churn, which this call alone resolves immediately. The
// removed version also had a real, measured cost: forcing a second and
// third GC+scavenge pass while the server is actively handling requests
// occasionally added a multi-second pause to whatever call happened to be
// in flight at that moment — real latency risk for a benefit that turned
// out to be marginal. One synchronous call at startup gets nearly all of
// the win with none of that risk.
func (idx *Index) ReleaseUnusedMemory() {
	idx.compactReadOnlySymbolGraph()
	debug.FreeOSMemory()
}

// CompactReadOnly prepares an additional linked index for long-running query
// use without forcing a process-wide GC. The primary MCP/UI index should call
// ReleaseUnusedMemory after every linked index has been compacted.
func (idx *Index) CompactReadOnly() {
	idx.compactReadOnlySymbolGraph()
}

// compactReadOnlySymbolGraph swaps the expanded edge slice for the numeric
// representation used by long-running read-only query paths. A later watcher
// mutation materializes a writable slice exactly once.
func (idx *Index) compactReadOnlySymbolGraph() {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.compactSymbolEdges != nil || len(idx.SymbolEdges) == 0 || idx.symbolGraphMutable {
		return
	}
	store := newCompactSymbolEdgeStore(idx.SymbolEdges)
	if store == nil {
		return
	}
	idx.compactSymbolEdges = store
	idx.SymbolEdges = nil
	idx.publishSymbolGraphLocked()
}

// HasTTLContent reports whether the index contains an indexed Turtle file.
// Checking Files rather than the derived Terms map is important in watch
// mode: terms may also be retained while code references still point at
// them, but that alone does not make the TTL-specific tool surface useful.
func (idx *Index) HasTTLContent() bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for _, file := range idx.Files {
		if file.Language == "ttl" {
			return true
		}
	}
	return false
}

// HasEventEdges reports whether the index recorded any event-bus emit/
// listen/remove call sites. The MCP server calls this once at startup to
// decide whether to register trace_event/list_events — for a repo with no
// event-bus usage those tools would always return an empty result.
func (idx *Index) HasEventEdges() bool {
	found := false
	idx.symbolGraphSnapshot().forEachEdge(func(_ int, e CGPEdge) bool {
		switch e.Type {
		case EdgeEmitsEvent, EdgeListensEvent, EdgeRemovesEvent:
			found = true
			return false
		}
		return true
	})
	return found
}

func maybeGunzipBytes(data []byte) ([]byte, error) {
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		return data, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

func (idx *Index) ensureLiteralsLoaded() error {
	idx.mu.Lock()
	if idx.literalsLoaded || idx.literalSidecarPath == "" {
		if idx.literalSidecarPath == "" {
			idx.literalsLoaded = true
		}
		idx.mu.Unlock()
		return nil
	}
	path := idx.literalSidecarPath
	idx.mu.Unlock()
	literals, err := loadLiteralsSidecar(path)
	if err != nil {
		return err
	}
	idx.mu.Lock()
	idx.Literals = literals
	idx.literalsLoaded = true
	idx.mu.Unlock()
	return nil
}

func copyFileIfExists(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

func saveLiteralsSidecar(literals []Literal, path string) error {
	if len(literals) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	enc := json.NewEncoder(file)
	enc.SetEscapeHTML(false)
	for _, lit := range literals {
		if err := enc.Encode(lit); err != nil {
			return err
		}
	}
	return nil
}

func loadLiteralsSidecar(path string) ([]Literal, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()
	var literals []Literal
	dec := json.NewDecoder(file)
	for {
		var lit Literal
		if err := dec.Decode(&lit); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		literals = append(literals, lit)
	}
	return literals, nil
}

// initRuntimeLocked must be called with idx.mu held. It populates the various
// dedup maps used by the Add* methods.
func (idx *Index) initRuntimeLocked() {
	idx.initResolutionRuntimeLocked()
	if idx.termLocationSeen == nil {
		idx.termLocationSeen = map[string]map[string]bool{}
	}
	if idx.referenceSeen == nil {
		idx.referenceSeen = map[string]bool{}
		for _, ref := range idx.References {
			idx.referenceSeen[ref.ID] = true
		}
	}
	if idx.edgeSeen == nil {
		idx.edgeSeen = map[string]bool{}
		for _, edge := range idx.Edges {
			idx.edgeSeen[edge.ID] = true
		}
	}
	if idx.Symbols == nil {
		idx.Symbols = map[string]CGPSymbol{}
	}
	if idx.symbolSeen == nil {
		idx.symbolSeen = map[string]bool{}
		for id := range idx.Symbols {
			idx.symbolSeen[id] = true
		}
	}
	if idx.symbolEdgeSeen == nil {
		idx.symbolEdgeSeen = map[string]bool{}
		if idx.compactSymbolEdges != nil {
			for i := range idx.compactSymbolEdges.edges {
				idx.symbolEdgeSeen[idx.compactSymbolEdges.edgeAt(i, true).ID] = true
			}
		} else {
			for _, edge := range idx.SymbolEdges {
				idx.symbolEdgeSeen[edge.ID] = true
			}
		}
	}
	if len(idx.termLocationSeen) == 0 && len(idx.Terms) > 0 {
		for id, term := range idx.Terms {
			if idx.termLocationSeen[id] == nil {
				idx.termLocationSeen[id] = map[string]bool{}
			}
			for _, loc := range term.Locations {
				idx.termLocationSeen[id][locationKey(loc)] = true
			}
		}
	}
}

// initResolutionRuntimeLocked builds the small, read-visible derived indexes
// that are not serialized. Keep this separate from initRuntimeLocked's large
// mutation-only dedup maps so a loaded read-only server remains lean without
// changing compiler-backed lookup behavior.
func (idx *Index) initResolutionRuntimeLocked() {
	if !idx.goModulePathLoaded {
		idx.goModulePath = readGoModulePath(idx.Repo.Root)
		idx.goModulePathLoaded = true
	}
	if idx.Symbols == nil {
		idx.Symbols = map[string]CGPSymbol{}
	}
	initGoReturnTypes := idx.goReturnTypes == nil
	if initGoReturnTypes {
		idx.goReturnTypes = map[string][]string{}
	}
	if initGoReturnTypes {
		for id, sym := range idx.Symbols {
			if len(sym.ReturnTypes) > 0 {
				idx.goReturnTypes[id] = append([]string(nil), sym.ReturnTypes...)
			}
		}
	}
	if idx.extensionMethods == nil {
		idx.extensionMethods = map[string]map[string][]string{}
		for id, sym := range idx.Symbols {
			if sym.ReceiverType != "" {
				registerExtensionMethodLocked(idx, sym.Language, sym.ReceiverType, sym.Name, id)
			}
		}
	}
}

func readGoModulePath(root string) string {
	if root == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "module" {
			return fields[1]
		}
	}
	return ""
}

func (idx *Index) AddPrefix(prefix, iri string, loc Location) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if _, exists := idx.Prefixes[prefix]; exists {
		return
	}
	idx.Prefixes[prefix] = Prefix{Prefix: prefix, IRI: iri, Location: loc}
}

func (idx *Index) AddTerm(term, iri string, loc *Location) Term {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	prefix, local := splitTerm(term)
	if iri == "" {
		iri = idx.resolveTermLocked(term)
	}
	if local == "" && iri != "" {
		term = idx.compactIRILocked(iri)
		prefix, local = splitTerm(term)
	}
	if term == "" || local == "" {
		return Term{}
	}
	id := idx.termIDLocked(term, iri)
	existing := idx.Terms[id]
	if existing.ID == "" {
		existing = Term{ID: id, Term: term, IRI: iri, Prefix: prefix, LocalName: local}
	}
	if existing.IRI == "" {
		existing.IRI = iri
	}
	if loc != nil {
		idx.initRuntimeLocked()
		if idx.termLocationSeen[id] == nil {
			idx.termLocationSeen[id] = map[string]bool{}
		}
		key := locationKey(*loc)
		if !idx.termLocationSeen[id][key] {
			existing.Locations = append(existing.Locations, *loc)
			idx.termLocationSeen[id][key] = true
		}
	}
	idx.Terms[id] = existing
	return existing
}

func (idx *Index) termIDLocked(term, iri string) string {
	baseID := "term:" + term
	existing, exists := idx.Terms[baseID]
	if !exists || existing.IRI == "" || iri == "" || existing.IRI == iri {
		return baseID
	}
	return baseID + "<" + iri + ">"
}

// ResolveTerm and ResolveTermInFile are exported read paths used by callers
// outside BuildIndex. They take the lock to stay safe under watch-mode.
func (idx *Index) ResolveTerm(term string) string {
	return idx.ResolveTermInFile("", term)
}

func (idx *Index) ResolveTermInFile(file, term string) string {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	prefix, local := splitTerm(term)
	if prefix == "" || local == "" {
		return ""
	}
	if file != "" {
		if f, ok := idx.Files[file]; ok {
			if iri, ok := f.Prefixes[prefix]; ok {
				return iri + local
			}
		}
	}
	p, ok := idx.Prefixes[prefix]
	if !ok {
		return ""
	}
	return p.IRI + local
}

func (idx *Index) resolveTermLocked(term string) string {
	prefix, local := splitTerm(term)
	if prefix == "" || local == "" {
		return ""
	}
	p, ok := idx.Prefixes[prefix]
	if !ok {
		return ""
	}
	return p.IRI + local
}

func (idx *Index) CompactIRIInFile(file, iri string) string {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if file != "" {
		if f, ok := idx.Files[file]; ok && len(f.Prefixes) > 0 {
			type pair struct {
				prefix string
				base   string
			}
			var prefixes []pair
			for prefix, base := range f.Prefixes {
				prefixes = append(prefixes, pair{prefix: prefix, base: base})
			}
			sort.Slice(prefixes, func(i, j int) bool {
				if len(prefixes[i].base) == len(prefixes[j].base) {
					if len(prefixes[i].prefix) != len(prefixes[j].prefix) {
						return len(prefixes[i].prefix) > len(prefixes[j].prefix)
					}
					return prefixes[i].prefix < prefixes[j].prefix
				}
				return len(prefixes[i].base) > len(prefixes[j].base)
			})
			for _, p := range prefixes {
				if strings.HasPrefix(iri, p.base) {
					local := strings.TrimPrefix(iri, p.base)
					if isValidPNLocal(local) {
						return p.prefix + ":" + local
					}
				}
			}
		}
	}
	return idx.compactIRILocked(iri)
}

func (idx *Index) CompactIRI(iri string) string {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return idx.compactIRILocked(iri)
}

func (idx *Index) compactIRILocked(iri string) string {
	type pair struct {
		prefix string
		base   string
	}
	var prefixes []pair
	for key, value := range idx.Prefixes {
		prefixes = append(prefixes, pair{prefix: key, base: value.IRI})
	}
	sort.Slice(prefixes, func(i, j int) bool {
		if len(prefixes[i].base) == len(prefixes[j].base) {
			if len(prefixes[i].prefix) != len(prefixes[j].prefix) {
				return len(prefixes[i].prefix) > len(prefixes[j].prefix)
			}
			return prefixes[i].prefix < prefixes[j].prefix
		}
		return len(prefixes[i].base) > len(prefixes[j].base)
	})
	for _, p := range prefixes {
		if strings.HasPrefix(iri, p.base) {
			local := strings.TrimPrefix(iri, p.base)
			if isValidPNLocal(local) {
				return p.prefix + ":" + local
			}
		}
	}
	return ""
}

func isValidPNLocal(s string) bool {
	if s == "" {
		return false
	}
	c := s[0]
	if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_') {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.' {
			continue
		}
		return false
	}
	return true
}

func (idx *Index) AddReference(ref Reference) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if ref.ID == "" {
		ref.ID = fmt.Sprintf("ref:%s:%d:%d:%s:%s", ref.File, ref.StartLine, ref.StartColumn, ref.Term, ref.Confidence)
	}
	idx.initRuntimeLocked()
	if idx.referenceSeen[ref.ID] {
		return
	}
	idx.References = append(idx.References, ref)
	idx.referenceSeen[ref.ID] = true
}

func (idx *Index) AddEdge(from, to, edgeType, confidence string, evidence Location) {
	id := "edge:" + from + "->" + to + ":" + edgeType + ":" + evidence.File + ":" + fmt.Sprint(evidence.StartLine)
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.initRuntimeLocked()
	if idx.edgeSeen[id] {
		return
	}
	idx.Edges = append(idx.Edges, Edge{ID: id, From: from, To: to, Type: edgeType, Confidence: confidence, Evidence: evidence})
	idx.edgeSeen[id] = true
}

// addOrUpdateFile commits a file entry. Used by BuildIndex's read phase and
// by the watch-mode incremental rebake.
func (idx *Index) addOrUpdateFile(rel string, content []byte) File {
	lang := languageForContent(rel, content)
	entry := File{
		ID:          "file:" + filepath.ToSlash(rel),
		Path:        filepath.ToSlash(rel),
		Language:    lang,
		SHA256:      hash(content),
		LineCount:   countLines(string(content)),
		Parser:      parserFor(lang),
		ParseStatus: ParseStatusOK,
	}
	idx.mu.Lock()
	idx.Files[rel] = entry
	idx.mu.Unlock()
	return entry
}

// parserFor names the scanner that handles a given language. Doctor uses this
// to summarize coverage; agents can correlate parse-status with parser name.
func parserFor(language string) string {
	switch language {
	case "typescript", "javascript", "vue":
		return "jsparse-token"
	case "python":
		return "tree-sitter-python"
	case "java":
		return "tree-sitter-java"
	case "go":
		return "tree-sitter-go"
	case "csharp":
		return "tree-sitter-csharp"
	case "rust":
		return "tree-sitter-rust"
	case "ruby":
		return "tree-sitter-ruby"
	case "php":
		return "tree-sitter-php"
	case "c":
		return "tree-sitter-c"
	case "cpp":
		return "tree-sitter-cpp"
	case "kotlin":
		return "tree-sitter-kotlin"
	case "bash":
		return "tree-sitter-bash"
	case "scala":
		return "tree-sitter-scala"
	case "lua":
		return "tree-sitter-lua"
	case "elixir":
		return "tree-sitter-elixir"
	case "dart":
		return "tree-sitter-dart"
	case "haskell":
		return "tree-sitter-haskell"
	case "clojure":
		return "tree-sitter-clojure"
	case "swift":
		return "tree-sitter-swift"
	case "r":
		return "tree-sitter-r"
	case "julia":
		return "tree-sitter-julia"
	case "zig":
		return "tree-sitter-zig"
	case "ocaml":
		return "tree-sitter-ocaml"
	case "hcl":
		return "tree-sitter-hcl"
	case "yaml":
		return "yaml-ast"
	case "dockerfile":
		return "dockerfile-structural"
	case "ttl":
		return "ttl-lex"
	default:
		if _, ok := heuristicSpecs[language]; ok {
			return "heuristic-fallback"
		}
		return ""
	}
}

// markFileParseDiagnostics merges scanner diagnostics into the File entry.
// Multiple diagnostics are joined; the strongest status wins
// (error > partial > ok). Safe to call from concurrent scanners.
func (idx *Index) markFileParseDiagnostics(rel string, diags []ScanDiagnostic) {
	if len(diags) == 0 {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	entry, ok := idx.Files[rel]
	if !ok {
		return
	}
	messages := make([]string, 0, len(diags))
	for _, d := range diags {
		if d.Code != "" && d.Message != "" {
			messages = append(messages, d.Code+": "+d.Message)
		} else if d.Code != "" {
			messages = append(messages, d.Code)
		} else if d.Message != "" {
			messages = append(messages, d.Message)
		}
	}
	merged := strings.Join(messages, "; ")
	if entry.ParseError != "" {
		merged = entry.ParseError + "; " + merged
	}
	entry.ParseError = merged
	if entry.ParseStatus == "" || entry.ParseStatus == ParseStatusOK {
		entry.ParseStatus = ParseStatusPartial
	}
	idx.Files[rel] = entry
}

func gitCommit(root string) string {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func splitTerm(term string) (string, string) {
	if strings.HasPrefix(term, "http://") || strings.HasPrefix(term, "https://") {
		return "", ""
	}
	parts := strings.SplitN(term, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", term
	}
	return parts[0], parts[1]
}

func locationKey(loc Location) string {
	return loc.File + ":" + fmt.Sprint(loc.StartLine) + ":" + fmt.Sprint(loc.StartColumn) + ":" + loc.Kind
}

func hash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func countLines(content string) int {
	if content == "" {
		return 0
	}
	count := strings.Count(content, "\n")
	if !strings.HasSuffix(content, "\n") {
		count++
	}
	return count
}

func languageFor(path string) string {
	base := strings.ToLower(filepath.Base(path))
	if base == "dockerfile" || strings.HasSuffix(base, ".dockerfile") {
		return "dockerfile"
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ttl":
		return "ttl"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	case ".vue":
		return "vue"
	case ".py":
		return "python"
	case ".java":
		return "java"
	case ".go":
		return "go"
	case ".rs":
		return "rust"
	case ".cs":
		return "csharp"
	case ".rb":
		return "ruby"
	case ".php":
		return "php"
	case ".c":
		return "c"
	case ".h":
		// ".h" is ambiguous between C and C++ by extension alone. Defaults
		// to C++, not C: verified empirically that tree-sitter-cpp's grammar
		// parses plain C struct/function declarations cleanly (C++ is a
		// near-superset for these constructs), so a pure-C ".h" file mostly
		// still extracts fine, just tagged "cpp". The reverse is not true —
		// tree-sitter-c cannot parse classes/namespaces/templates at all —
		// and C++ projects routinely use plain ".h" for headers.
		return "cpp"
	case ".cpp", ".cc", ".cxx", ".hpp", ".hh", ".hxx":
		return "cpp"
	case ".kt", ".kts":
		return "kotlin"
	case ".sh", ".bash":
		// Deliberately excludes ".zsh": tree-sitter-bash's
		// grammar — which targets POSIX/Bash syntax — cannot parse zsh-only
		// extensions like `${+functions[$1]}` parameter flags or `(Ie)` glob
		// qualifiers. No tree-sitter-zsh grammar
		// exists to onboard instead; honestly leaving .zsh unrecognized
		// beats silently mis-parsing it.
		return "bash"
	case ".scala", ".sc":
		return "scala"
	case ".lua":
		return "lua"
	case ".ex", ".exs":
		return "elixir"
	case ".dart":
		return "dart"
	case ".hs":
		return "haskell"
	case ".clj", ".cljc", ".cljs":
		return "clojure"
	case ".swift":
		return "swift"
	case ".r", ".R":
		return "r"
	case ".jl":
		return "julia"
	case ".zig":
		return "zig"
	case ".ml", ".mli":
		return "ocaml"
	case ".tf", ".tfvars", ".hcl":
		return "hcl"
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	default:
		return "unknown"
	}
}

func languageForContent(path string, content []byte) string {
	if language := languageFor(path); language != "unknown" {
		return language
	}
	return shellLanguageFromShebang(content)
}

func shellLanguageFromShebang(content []byte) string {
	if len(content) < 2 || content[0] != '#' || content[1] != '!' {
		return "unknown"
	}
	end := bytes.IndexByte(content, '\n')
	if end < 0 {
		end = len(content)
	}
	line := strings.TrimSpace(string(content[2:end]))
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "unknown"
	}
	command := filepath.Base(fields[0])
	if command == "env" {
		for _, field := range fields[1:] {
			if strings.HasPrefix(field, "-") {
				continue
			}
			command = filepath.Base(field)
			break
		}
	}
	switch command {
	case "bash", "sh", "dash", "ksh":
		return "bash"
	default:
		return "unknown"
	}
}

func sortReferences(refs []Reference) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].File != refs[j].File {
			return refs[i].File < refs[j].File
		}
		if refs[i].StartLine != refs[j].StartLine {
			return refs[i].StartLine < refs[j].StartLine
		}
		return refs[i].StartColumn < refs[j].StartColumn
	})
}

func sortEdges(edges []Edge) {
	sort.Slice(edges, func(i, j int) bool { return edges[i].ID < edges[j].ID })
}

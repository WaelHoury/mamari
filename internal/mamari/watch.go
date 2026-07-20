package mamari

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// WatchOptions configure incremental rebakes.
type WatchOptions struct {
	// Debounce coalesces rapid bursts of filesystem events. Editors often
	// write a file as several events (rename, create, chmod); waiting briefly
	// before rebaking avoids redundant work.
	Debounce time.Duration

	// OnRebake, if set, is invoked after each successful incremental rebake
	// with the relative paths that were updated and any that were dropped.
	// Useful for hooking into MCP stdio reload, telemetry, or tests.
	OnRebake func(updated, removed []string)

	// OnError, if set, is invoked for non-fatal errors so callers can decide
	// to log or surface them. Fatal errors (watcher creation, root removal)
	// are returned from Watch directly.
	OnError func(error)

	// OnReady is called once after filesystem watches are installed and the
	// watcher can no longer miss an edit made by a newly connected client.
	OnReady func()
}

// Watch starts a filesystem watcher rooted at idx.Repo.Root and rebakes
// affected files when they change. The watcher honors the same ignore rules
// as WalkRepo. Watch blocks until ctx is canceled.
//
// The implementation is intentionally conservative: each event triggers a
// per-file rebake rather than a full rescan, which keeps update latency
// bounded by the size of the changed file rather than the size of the repo.
func Watch(ctx context.Context, idx *Index, opts WatchOptions) error {
	root := idx.Repo.Root
	if root == "" {
		return errors.New("watch: index has no repo root")
	}
	if opts.Debounce <= 0 {
		opts.Debounce = 200 * time.Millisecond
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()

	ignores, err := loadWatchIgnores(root)
	if err != nil {
		return err
	}
	tracked := trackedGitFiles(root)
	rescan := make(chan struct{}, 1)
	reportError := func(err error) {
		if opts.OnError != nil {
			opts.OnError(err)
		}
		select {
		case rescan <- struct{}{}:
		default:
		}
	}
	if err := addWatchDirs(w, root, ignores, tracked, reportError); err != nil {
		return err
	}

	pending := newPendingSet()
	timer := time.NewTimer(time.Hour) // disabled until events arrive
	timer.Stop()
	if opts.OnReady != nil {
		opts.OnReady()
	}

	flush := func() {
		updates := pending.drain()
		if len(updates) == 0 {
			return
		}
		var requestedUpdates, requestedRemoves []string
		for rel, kind := range updates {
			switch kind {
			case eventUpdate:
				requestedUpdates = append(requestedUpdates, rel)
			case eventRemove:
				requestedRemoves = append(requestedRemoves, rel)
			}
		}
		updated, removed, err := rebakeChangedFiles(idx, root, requestedUpdates, requestedRemoves)
		if err != nil && opts.OnError != nil {
			opts.OnError(err)
		}
		idx.recordRebake(updated, removed)
		if opts.OnRebake != nil {
			opts.OnRebake(updated, removed)
		}
		if err == nil {
			// Refresh the lock-free query snapshot on the watcher's own
			// goroutine, off any concurrent query's critical path — see
			// publishQuerySnapshot. Skipped after an error: rebakeChangedFiles
			// may have left CGP scanning partway through for the failed
			// file(s), and publishing from that state could hand readers a
			// snapshot reflecting a half-applied edit.
			changed := append(append([]string(nil), updated...), removed...)
			// Search and semantic indexes are intentionally lazy. Filesystem
			// activity can happen while no agent is using the server (editor
			// saves, branch switches, generators), and must not trigger a
			// whole-repository warm-up. If search has already been used, its
			// per-file cache was refreshed by rebakeChangedFiles and publishing
			// the replacement remains cheap. Otherwise the next search request
			// builds from the latest live graph on demand.
			idx.publishQuerySnapshotIfBuilt(changed)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			rel := relForWatch(root, ev.Name)
			if rel == "" {
				continue
			}
			info, statErr := os.Stat(ev.Name)
			isDir := statErr == nil && info.IsDir()
			if ignored(rel, isDir, ignores, tracked) {
				continue
			}
			if isDir && (ev.Has(fsnotify.Create) || ev.Has(fsnotify.Rename)) {
				_ = addWatchDir(w, ev.Name, root, ignores, tracked, reportError)
				if queueExistingWatchableFiles(ev.Name, root, ignores, tracked, pending) {
					timer.Reset(opts.Debounce)
				}
				continue
			}
			if !isWatchableEvent(idx, rel, ev.Name, statErr == nil) {
				// Could be a directory we want to keep watching but we don't
				// rebake unsupported files.
				continue
			}
			if statErr == nil && shouldSkipLargeGeneratedArtifactInfo(rel, info) {
				pending.set(rel, eventRemove)
				timer.Reset(opts.Debounce)
				continue
			}
			switch {
			case ev.Has(fsnotify.Remove), ev.Has(fsnotify.Rename):
				if statErr != nil {
					pending.set(rel, eventRemove)
				} else {
					pending.set(rel, eventUpdate)
				}
			case ev.Has(fsnotify.Write), ev.Has(fsnotify.Create):
				if statErr == nil {
					pending.set(rel, eventUpdate)
				}
			}
			timer.Reset(opts.Debounce)
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			reportError(err)
		case <-rescan:
			queued, err := queueIndexReconciliation(idx, root, pending)
			if err != nil {
				if opts.OnError != nil {
					opts.OnError(fmt.Errorf("watch reconciliation: %w", err))
				}
				continue
			}
			if queued {
				timer.Reset(opts.Debounce)
			}
		case <-timer.C:
			flush()
		}
	}
}

// queueExistingWatchableFiles closes the race between a directory creation
// event and registering watches below it. Editors and generators commonly
// create a directory and its first files in one burst; those file events can
// occur before fsnotify has a watch on the new directory. A one-time scan
// makes the resulting index deterministic without adding steady-state work.
func queueExistingWatchableFiles(abs, root string, ignores []ignorePattern, tracked map[string]bool, pending *pendingSet) bool {
	queued := false
	_ = filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel := relForWatch(root, path)
		if rel == "" {
			return nil
		}
		if ignored(rel, d.IsDir(), ignores, tracked) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !isWatchablePath(rel, path) {
			return nil
		}
		info, err := d.Info()
		if err != nil || shouldSkipLargeGeneratedArtifactInfo(rel, info) {
			return nil
		}
		pending.set(rel, eventUpdate)
		queued = true
		return nil
	})
	return queued
}

// queueIndexReconciliation repairs any changes missed after an fsnotify
// overflow or a directory-watch registration failure. It is intentionally
// error-triggered rather than periodic: healthy sessions pay no repeated
// full-tree scan, while degraded sessions recover to a hash-verified state.
func queueIndexReconciliation(idx *Index, root string, pending *pendingSet) (bool, error) {
	files, err := WalkRepo(root)
	if err != nil {
		return false, err
	}
	snap := idx.snapshot()
	onDisk := make(map[string]bool, len(files))
	queued := false
	for _, rel := range files {
		onDisk[rel] = true
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return false, err
		}
		indexed, ok := snap.Files[rel]
		if !ok || indexed.SHA256 == "" || indexed.SHA256 != hash(data) {
			pending.set(rel, eventUpdate)
			queued = true
		}
	}
	for rel := range snap.Files {
		if onDisk[rel] {
			continue
		}
		pending.set(rel, eventRemove)
		queued = true
	}
	return queued, nil
}

type pendingKind int

const (
	eventUpdate pendingKind = iota + 1
	eventRemove
)

type pendingSet struct {
	mu sync.Mutex
	m  map[string]pendingKind
}

func newPendingSet() *pendingSet { return &pendingSet{m: map[string]pendingKind{}} }

func (p *pendingSet) set(rel string, kind pendingKind) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Remove wins over update (we don't bother re-baking a file we're about
	// to drop), but a later update supersedes a remove (rename within repo).
	if kind == eventRemove {
		p.m[rel] = eventRemove
		return
	}
	if existing, ok := p.m[rel]; ok && existing == eventRemove {
		p.m[rel] = eventUpdate
		return
	}
	p.m[rel] = kind
}

func (p *pendingSet) drain() map[string]pendingKind {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.m
	p.m = map[string]pendingKind{}
	return out
}

// rebakeFile re-scans a single file. The previous evidence (refs, edges,
// symbols, dynamic-IRI calls, literals) tied to that file is dropped first
// so the rebake is idempotent.
func rebakeFile(idx *Index, root, rel string) error {
	_, _, err := rebakeChangedFiles(idx, root, []string{rel}, nil)
	return err
}

func rebakeChangedFiles(idx *Index, root string, requestedUpdates, requestedRemoves []string) ([]string, []string, error) {
	updateSet := map[string]bool{}
	removeSet := map[string]bool{}
	preserveExternalCGPEdges := map[string]bool{}
	for _, rel := range requestedRemoves {
		if rel == "" {
			continue
		}
		removeSet[rel] = true
		for _, dep := range dependentClosure(idx, rel) {
			if dep != rel {
				updateSet[dep] = true
			}
		}
	}
	for _, rel := range requestedUpdates {
		if rel == "" || removeSet[rel] {
			continue
		}
		updateSet[rel] = true
		if shouldRebakeDependentsForUpdate(idx, root, rel) {
			for _, dep := range dependentClosure(idx, rel) {
				if !removeSet[dep] {
					updateSet[dep] = true
				}
			}
		} else {
			preserveExternalCGPEdges[rel] = true
		}
	}
	for rel := range removeSet {
		delete(updateSet, rel)
	}
	if touchesTTL(idx, updateSet, removeSet) {
		if err := idx.ensureLiteralsLoaded(); err != nil {
			return nil, nil, err
		}
	}
	if len(updateSet) == 0 && len(removeSet) == 0 {
		return nil, nil, nil
	}

	// Detach once for the complete rebake. The previously published graph
	// remains immutable and queryable until the replacement is fully built;
	// thousands of AddCGPSymbol/AddCGPEdge calls below therefore pay one
	// graph copy for the batch rather than one copy per mutation or query.
	idx.beginSymbolGraphMutation(true)

	var removed []string
	for rel := range removeSet {
		dropFile(idx, rel)
		removed = append(removed, rel)
	}

	contents := map[string]string{}
	languages := map[string]string{}
	var missing []string
	for rel := range updateSet {
		abs := filepath.Join(root, rel)
		info, statErr := os.Stat(abs)
		if statErr == nil && shouldSkipLargeGeneratedArtifactInfo(rel, info) {
			dropFile(idx, rel)
			removed = append(removed, rel)
			delete(updateSet, rel)
			delete(preserveExternalCGPEdges, rel)
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				missing = append(missing, rel)
				continue
			}
			return nil, removed, err
		}
		contents[rel] = string(data)
		languages[rel] = languageForContent(rel, data)
	}
	for _, rel := range missing {
		dropFile(idx, rel)
		delete(updateSet, rel)
		delete(preserveExternalCGPEdges, rel)
		removed = append(removed, rel)
	}
	for rel := range updateSet {
		dropFileWithOptions(idx, rel, dropFileOptions{preserveExternalCGPEdges: preserveExternalCGPEdges[rel]})
		idx.addOrUpdateFile(rel, []byte(contents[rel]))
	}

	var ttlUpdated bool
	var codeFiles []string
	var cgpFiles []string
	for rel := range updateSet {
		switch languages[rel] {
		case "ttl":
			ttlUpdated = true
			if err := ScanTTL(idx, rel, contents[rel]); err != nil {
				return nil, removed, err
			}
		case "typescript", "javascript", "vue":
			codeFiles = append(codeFiles, rel)
			cgpFiles = append(cgpFiles, rel)
		case "python", "java", "go", "csharp", "rust", "ruby", "php", "c", "cpp", "kotlin", "bash", "scala", "lua", "elixir", "dart", "haskell", "clojure", "r", "julia", "zig", "ocaml", "hcl", "yaml", "dockerfile":
			cgpFiles = append(cgpFiles, rel)
		default:
			if _, ok := heuristicSpecs[languages[rel]]; ok {
				cgpFiles = append(cgpFiles, rel)
			}
		}
	}
	sort.Strings(codeFiles)
	sort.Strings(cgpFiles)
	if len(codeFiles) > 0 {
		effective := watchEffectiveNamespacesFor(idx, root, contents)
		idx.beginCodeScanSnapshot()
		for _, rel := range codeFiles {
			ScanCode(idx, rel, contents[rel], effective[rel])
			dyn := ScanDynamicIRICalls(rel, contents[rel])
			if len(dyn) > 0 {
				idx.appendDynamicIRICalls(dyn)
			}
		}
		idx.endCodeScanSnapshot()
	}
	for _, rel := range cgpFiles {
		ScanCGPSymbols(idx, rel, languages[rel], contents[rel])
	}
	if ttlUpdated {
		AddCGPFromTTL(idx)
	}
	idx.invalidateFileSymbolIndex()
	idx.ensureFileSymbolIndex()
	// Re-running ScanCGPSymbols above for just the changed file(s) is, for
	// an out-of-line method (a C++/Rust receiver-type redirect whose class
	// lives in a different, unchanged file), exactly the same per-file-only
	// lookup BuildIndex's first pass does — it misses every time, even
	// though the class is already indexed and unchanged, because the
	// lookup never looks outside the file being re-scanned. Without this,
	// a single edit to a .cpp file would permanently revert its methods to
	// file-parented on every incremental rebake, even after BuildIndex's
	// initial full index got them right. See resolveOutOfLineMethodParents'
	// own doc comment for the full rationale (it's the identical fix-up
	// BuildIndex itself runs after Phase 5, just re-run here for the
	// narrower per-rebake set of newly (re-)recorded misses).
	resolveOutOfLineMethodParents(idx)
	for _, rel := range codeFiles {
		if languages[rel] == "vue" {
			linkTemplateClassUsagesForFile(idx, rel)
		}
	}
	for _, rel := range cgpFiles {
		// Mask once, share across annotators. annotateShapeHash MUST run here
		// too — otherwise a changed file's symbols lose their structural
		// fingerprint on rebake and drop out of `duplicates` until a full
		// re-index (a gap in the original watch path).
		lines := maskedSourceLines(idx, rel, contents[rel])
		annotateComplexityLines(idx, rel, lines)
		annotateHotPathSignalsLines(idx, rel, lines)
		annotateShapeHashLines(idx, rel, lines)
	}
	for _, rel := range cgpFiles {
		ScanCGPRelations(idx, rel, languages[rel], contents[rel])
	}
	for _, rel := range codeFiles {
		ScanFrameworkSymbols(idx, rel, languages[rel], contents[rel])
	}
	idx.invalidateFileSymbolIndex()
	idx.ensureFileSymbolIndex()
	for _, rel := range codeFiles {
		ScanFrameworkRelations(idx, rel, languages[rel], contents[rel])
	}
	sortCGP(idx)
	propagateTransitiveLoopDepth(idx)

	// Refresh only the changed/removed files' search-cache entries instead of
	// invalidating the whole repo's (see updateCodeSearchIndexForFiles) — a
	// single-file edit in a long `--watch` session shouldn't force the next
	// search-code/inspect-flow/repo_map query to re-tokenize every other
	// unchanged file in the repo.
	var changedForSearch []string
	for rel := range updateSet {
		changedForSearch = append(changedForSearch, rel)
	}
	idx.updateCodeSearchIndexForFiles(changedForSearch, removed)
	idx.invalidateSemanticIndex()

	var updated []string
	for rel := range updateSet {
		updated = append(updated, rel)
	}
	sort.Strings(updated)
	sort.Strings(removed)
	idx.publishSymbolGraph()
	return updated, removed, nil
}

func shouldRebakeDependentsForUpdate(idx *Index, root, rel string) bool {
	switch languageFor(rel) {
	case "javascript", "typescript":
	default:
		return true
	}
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return true
	}
	return codeDependencySurfaceChanged(idx, rel, string(data))
}

func codeDependencySurfaceChanged(idx *Index, rel, content string) bool {
	parsed := ParseJS(content)
	newImports := scannedImportSpecSet(parsed.Imports)
	newSymbols := scannedSymbolSurface(parsed.Symbols)

	idx.mu.Lock()
	oldImports := indexedImportSpecSetLocked(idx, rel)
	oldSymbols := indexedSymbolSurfaceLocked(idx, rel)
	idx.mu.Unlock()

	return !equalStringSets(oldImports, newImports) || !equalStringSets(oldSymbols, newSymbols)
}

func scannedImportSpecSet(imports []ScannedImport) map[string]bool {
	out := map[string]bool{}
	for _, imp := range imports {
		if imp.Spec != "" {
			out[imp.Spec] = true
		}
	}
	return out
}

func scannedSymbolSurface(symbols []ScannedSymbol) map[string]bool {
	out := map[string]bool{}
	for _, sym := range symbols {
		if sym.Name == "" || !isDependencySurfaceKind(sym.Kind) {
			continue
		}
		out[sym.Kind+"\x00"+sym.Parent+"\x00"+sym.Name+"\x00"+fmt.Sprint(sym.Exported)] = true
	}
	return out
}

func indexedImportSpecSetLocked(idx *Index, rel string) map[string]bool {
	out := map[string]bool{}
	visit := func(edge CGPEdge) {
		if edge.Type != "imports" || edge.Evidence.File != rel || !strings.HasPrefix(edge.To, "module:") {
			return
		}
		spec := strings.TrimPrefix(edge.To, "module:")
		if spec != "" {
			out[spec] = true
		}
	}
	if idx.compactSymbolEdges != nil {
		for i := range idx.compactSymbolEdges.edges {
			visit(idx.compactSymbolEdges.edgeAt(i, false))
		}
	} else {
		for _, edge := range idx.SymbolEdges {
			visit(edge)
		}
	}
	return out
}

func indexedSymbolSurfaceLocked(idx *Index, rel string) map[string]bool {
	out := map[string]bool{}
	for _, sym := range idx.Symbols {
		if sym.File != rel || sym.Name == "" || !isDependencySurfaceKind(sym.Kind) {
			continue
		}
		parentName := ""
		if sym.ParentID != "" {
			if parent, ok := idx.Symbols[sym.ParentID]; ok {
				if parent.Kind == "file" {
					parentName = ""
				} else if parent.Kind == "class" {
					parentName = parent.Name
				} else {
					continue
				}
			} else {
				continue
			}
		}
		out[sym.Kind+"\x00"+parentName+"\x00"+sym.Name+"\x00"+fmt.Sprint(sym.Exported)] = true
	}
	return out
}

func isDependencySurfaceKind(kind string) bool {
	switch kind {
	case "function", "class", "method", "getter", "setter", "interface", "type", "enum", "constant":
		return true
	default:
		return false
	}
}

func equalStringSets(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func touchesTTL(idx *Index, updates, removes map[string]bool) bool {
	for rel := range updates {
		if languageFor(rel) == "ttl" {
			return true
		}
	}
	snap := idx.snapshot()
	for rel := range removes {
		if info, ok := snap.Files[rel]; ok && info.Language == "ttl" {
			return true
		}
		if languageFor(rel) == "ttl" {
			return true
		}
	}
	return false
}

// watchEffectiveNamespacesFor resolves cross-file namespace imports for all
// current code files. changedContent supplies unsaved freshly-read content for
// files in the current rebake batch.
func watchEffectiveNamespacesFor(idx *Index, root string, changedContent map[string]string) map[string]map[string]namespaceEntry {
	fileLocals, fileImports, codeFileSet := idx.ensureCodeNamespaceCache(root, changedContent)
	return resolveEffectiveNamespaces(fileLocals, fileImports, codeFileSet)
}

func (idx *Index) setCodeNamespaceCache(fileLocals map[string]map[string]namespaceEntry, fileImports map[string][]importStmt) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.codeNamespaceLocals = cloneNamespaceLocals(fileLocals)
	idx.codeNamespaceImports = cloneNamespaceImports(fileImports)
}

func (idx *Index) ensureCodeNamespaceCache(root string, changedContent map[string]string) (map[string]map[string]namespaceEntry, map[string][]importStmt, map[string]bool) {
	idx.mu.Lock()
	if idx.codeNamespaceLocals == nil {
		idx.codeNamespaceLocals = map[string]map[string]namespaceEntry{}
	}
	if idx.codeNamespaceImports == nil {
		idx.codeNamespaceImports = map[string][]importStmt{}
	}
	codeFileSet := make(map[string]bool, len(idx.Files))
	var toRead []string
	for path, info := range idx.Files {
		switch info.Language {
		case "typescript", "javascript", "vue":
			codeFileSet[path] = true
			if content, ok := changedContent[path]; ok {
				idx.codeNamespaceLocals[path] = fileLocalNamespaces(path, content)
				idx.codeNamespaceImports[path] = collectImports(path, content)
				continue
			}
			if _, ok := idx.codeNamespaceLocals[path]; !ok {
				toRead = append(toRead, path)
			}
			if _, ok := idx.codeNamespaceImports[path]; !ok {
				toRead = append(toRead, path)
			}
		}
	}
	idx.mu.Unlock()

	readLocals := map[string]map[string]namespaceEntry{}
	readImports := map[string][]importStmt{}
	if len(toRead) > 0 {
		sort.Strings(toRead)
		toRead = compactSortedStrings(toRead)
		for _, rel := range toRead {
			data, err := os.ReadFile(filepath.Join(root, rel))
			if err != nil {
				continue
			}
			content := string(data)
			readLocals[rel] = fileLocalNamespaces(rel, content)
			readImports[rel] = collectImports(rel, content)
		}
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()
	for rel := range idx.codeNamespaceLocals {
		if !codeFileSet[rel] {
			delete(idx.codeNamespaceLocals, rel)
		}
	}
	for rel := range idx.codeNamespaceImports {
		if !codeFileSet[rel] {
			delete(idx.codeNamespaceImports, rel)
		}
	}
	for rel, locals := range readLocals {
		if codeFileSet[rel] {
			idx.codeNamespaceLocals[rel] = locals
		}
	}
	for rel, imports := range readImports {
		if codeFileSet[rel] {
			idx.codeNamespaceImports[rel] = imports
		}
	}
	return cloneNamespaceLocalsForFiles(idx.codeNamespaceLocals, codeFileSet), cloneNamespaceImportsForFiles(idx.codeNamespaceImports, codeFileSet), cloneBoolMap(codeFileSet)
}

func cloneNamespaceLocals(in map[string]map[string]namespaceEntry) map[string]map[string]namespaceEntry {
	out := make(map[string]map[string]namespaceEntry, len(in))
	for file, locals := range in {
		cloned := make(map[string]namespaceEntry, len(locals))
		for k, v := range locals {
			cloned[k] = v
		}
		out[file] = cloned
	}
	return out
}

func cloneNamespaceLocalsForFiles(in map[string]map[string]namespaceEntry, files map[string]bool) map[string]map[string]namespaceEntry {
	out := make(map[string]map[string]namespaceEntry, len(files))
	for file := range files {
		if locals, ok := in[file]; ok {
			cloned := make(map[string]namespaceEntry, len(locals))
			for k, v := range locals {
				cloned[k] = v
			}
			out[file] = cloned
		}
	}
	return out
}

func cloneNamespaceImports(in map[string][]importStmt) map[string][]importStmt {
	out := make(map[string][]importStmt, len(in))
	for file, imports := range in {
		out[file] = cloneImportStmts(imports)
	}
	return out
}

func cloneNamespaceImportsForFiles(in map[string][]importStmt, files map[string]bool) map[string][]importStmt {
	out := make(map[string][]importStmt, len(files))
	for file := range files {
		if imports, ok := in[file]; ok {
			out[file] = cloneImportStmts(imports)
		}
	}
	return out
}

func cloneImportStmts(imports []importStmt) []importStmt {
	if len(imports) == 0 {
		return nil
	}
	out := make([]importStmt, len(imports))
	for i, stmt := range imports {
		out[i] = importStmt{
			spec:     stmt.spec,
			bindings: append([]importBinding(nil), stmt.bindings...),
		}
	}
	return out
}

func cloneBoolMap(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func compactSortedStrings(in []string) []string {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s == out[len(out)-1] {
			continue
		}
		out = append(out, s)
	}
	return out
}

func dependentClosure(idx *Index, rel string) []string {
	lang := languageFor(rel)
	if lang == "ttl" {
		return allFilesByLanguage(idx, "typescript", "javascript", "vue")
	}
	if lang == "yaml" || lang == "dockerfile" {
		return allFilesByLanguage(idx, "yaml", "dockerfile")
	}
	// Build the target->importers adjacency ONCE (a single snapshot + one edge
	// scan), then BFS over it. Previously directImportDependents took a full
	// idx.snapshot() and rescanned every edge for EVERY node visited — O(nodes
	// × (deep-copy + edges)) per changed file on a rebake. Now it is one copy +
	// one scan, and the BFS is O(1) lookups. Output is identical (same
	// dependents, sorted).
	adjacency := importDependentAdjacency(idx)
	closure := map[string]bool{}
	queue := []string{rel}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, dep := range adjacency[cur] {
			if closure[dep] {
				continue
			}
			closure[dep] = true
			queue = append(queue, dep)
		}
	}
	out := make([]string, 0, len(closure))
	for dep := range closure {
		out = append(out, dep)
	}
	sort.Strings(out)
	return out
}

// importDependentAdjacency maps each resolved import/dependency target to the
// sorted set of files that depend on it, from a single index snapshot. In
// addition to code imports, Terraform depends-on edges and local module-source
// imports participate so editing a declaration or child module rebakes the
// unchanged .tf files whose graph edges point at it.
func importDependentAdjacency(idx *Index) map[string][]string {
	snap := idx.snapshot()
	codeFiles := make(map[string]bool, len(snap.Files))
	for path, info := range snap.Files {
		switch info.Language {
		case "typescript", "javascript", "vue", "python":
			codeFiles[path] = true
		}
	}
	sets := map[string]map[string]bool{}
	snap.forEachEdge(func(_ int, edge CGPEdge) bool {
		if edge.Type == terraformDependencyEdge {
			target, ok := snap.Symbols[edge.To]
			if !ok || target.File == "" || edge.Evidence.File == "" || target.File == edge.Evidence.File {
				return true
			}
			if sets[target.File] == nil {
				sets[target.File] = map[string]bool{}
			}
			sets[target.File][edge.Evidence.File] = true
			return true
		}

		if edge.Type != "imports" {
			return true
		}
		importer := edge.Evidence.File
		resolved := ""
		if strings.HasPrefix(edge.To, "module:") {
			spec := strings.TrimPrefix(edge.To, "module:")
			resolved = resolveImportPath(importer, spec, codeFiles)
		} else if target, ok := snap.Symbols[edge.To]; ok && target.Kind == "file" && target.Language == "hcl" {
			resolved = target.File
		}
		if resolved == "" {
			return true
		}
		if sets[resolved] == nil {
			sets[resolved] = map[string]bool{}
		}
		sets[resolved][importer] = true
		return true
	})
	out := make(map[string][]string, len(sets))
	for target, importers := range sets {
		lst := make([]string, 0, len(importers))
		for imp := range importers {
			lst = append(lst, imp)
		}
		sort.Strings(lst)
		out[target] = lst
	}
	return out
}

func allFilesByLanguage(idx *Index, langs ...string) []string {
	want := map[string]bool{}
	for _, lang := range langs {
		want[lang] = true
	}
	snap := idx.snapshot()
	var out []string
	for path, info := range snap.Files {
		if want[info.Language] {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// dropFile removes every piece of evidence tied to a given file. The runtime
// dedup maps are kept in sync so a subsequent rebake can re-introduce the
// evidence with the same IDs.
func dropFile(idx *Index, rel string) {
	dropFileWithOptions(idx, rel, dropFileOptions{})
}

type dropFileOptions struct {
	preserveExternalCGPEdges bool
}

func dropFileWithOptions(idx *Index, rel string, opts dropFileOptions) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.invalidateRepoMapCache()

	delete(idx.Files, rel)

	// Term locations.
	for id, term := range idx.Terms {
		filtered := term.Locations[:0]
		for _, loc := range term.Locations {
			if loc.File == rel {
				if idx.termLocationSeen[id] != nil {
					delete(idx.termLocationSeen[id], locationKey(loc))
				}
				continue
			}
			filtered = append(filtered, loc)
		}
		term.Locations = append([]Location(nil), filtered...)
		// If a term has no locations and no IRI, drop it entirely.
		if len(term.Locations) == 0 && term.IRI == "" {
			delete(idx.Terms, id)
			continue
		}
		idx.Terms[id] = term
	}

	// Shapes anchored in this file.
	for id, shape := range idx.Shapes {
		if shape.Location.File == rel {
			delete(idx.Shapes, id)
		}
	}

	// References.
	keptRefs := idx.References[:0]
	for _, ref := range idx.References {
		if ref.File == rel {
			delete(idx.referenceSeen, ref.ID)
			continue
		}
		keptRefs = append(keptRefs, ref)
	}
	idx.References = append([]Reference(nil), keptRefs...)

	// RDF edges keyed by evidence file.
	keptEdges := idx.Edges[:0]
	for _, edge := range idx.Edges {
		if edge.Evidence.File == rel {
			delete(idx.edgeSeen, edge.ID)
			continue
		}
		keptEdges = append(keptEdges, edge)
	}
	idx.Edges = append([]Edge(nil), keptEdges...)

	// Remove terms that became completely unreachable after dropping the
	// file. Keeping an IRI string alone used to leave deleted TTL terms
	// queryable forever during a watch session.
	referencedTerms := make(map[string]bool, len(idx.References)+len(idx.Edges)*2)
	for _, ref := range idx.References {
		referencedTerms[ref.TermID] = true
	}
	for _, edge := range idx.Edges {
		referencedTerms[edge.From] = true
		referencedTerms[edge.To] = true
	}
	for id, term := range idx.Terms {
		if len(term.Locations) > 0 || referencedTerms[id] {
			continue
		}
		delete(idx.Terms, id)
		delete(idx.termLocationSeen, id)
	}

	// CGP symbols. Track the IDs we drop so we can also prune edges that
	// referred to them (cross-file edges whose evidence is in another file
	// but whose endpoint sat here).
	droppedSymbols := map[string]bool{}
	for id, sym := range idx.Symbols {
		if sym.File == rel {
			droppedSymbols[id] = true
			delete(idx.Symbols, id)
			delete(idx.symbolSeen, id)
		}
	}
	pruneDroppedSymbolRuntimeStateLocked(idx, rel, droppedSymbols)

	keptCGP := idx.SymbolEdges[:0]
	for _, edge := range idx.SymbolEdges {
		dropsEndpoint := droppedSymbols[edge.From] || droppedSymbols[edge.To]
		if edge.Evidence.File == rel || (dropsEndpoint && !opts.preserveExternalCGPEdges) {
			delete(idx.symbolEdgeSeen, edge.ID)
			continue
		}
		keptCGP = append(keptCGP, edge)
	}
	idx.SymbolEdges = append([]CGPEdge(nil), keptCGP...)

	// Literals.
	keptLits := idx.Literals[:0]
	for _, lit := range idx.Literals {
		if lit.Location.File == rel {
			continue
		}
		keptLits = append(keptLits, lit)
	}
	idx.Literals = append([]Literal(nil), keptLits...)

	// Dynamic IRI calls.
	keptDyn := idx.DynamicIRICalls[:0]
	for _, call := range idx.DynamicIRICalls {
		if call.File == rel {
			continue
		}
		keptDyn = append(keptDyn, call)
	}
	idx.DynamicIRICalls = append([]DynamicIRICall(nil), keptDyn...)

	delete(idx.codeNamespaceLocals, rel)
	delete(idx.codeNamespaceImports, rel)
	delete(idx.jsDefaultExports, rel)
	for id, resource := range idx.infraResources {
		if resource.file == rel {
			delete(idx.infraResources, id)
		}
	}
	delete(idx.infraKustomizations, rel)
	delete(idx.infraDockerfiles, rel)
	idx.symbolsByFile = nil
	idx.symbolsByName = nil
	idx.childrenByParent = nil
	idx.symbolIndexBuilt = false
	idx.orderedSymbolIDs = nil
}

func pruneDroppedSymbolRuntimeStateLocked(idx *Index, rel string, droppedSymbols map[string]bool) {
	for id := range droppedSymbols {
		delete(idx.classBases, id)
		delete(idx.classInterfaces, id)
		delete(idx.varTypes, id)
		delete(idx.goReceivers, id)
		delete(idx.goReturnTypes, id)
		delete(idx.luaReceiverTypeBySymbol, id)
		delete(idx.unresolvedMethodParents, id)
	}
	for scopeID := range idx.varTypes {
		if strings.HasPrefix(scopeID, "luatable:"+rel+":") {
			delete(idx.varTypes, scopeID)
		}
	}
	for _, methods := range idx.goMethodsByReceiverType {
		for name, id := range methods {
			if droppedSymbols[id] {
				delete(methods, name)
			}
		}
	}
	for receiver, methods := range idx.luaMethodsByReceiverType {
		for name, id := range methods {
			if droppedSymbols[id] {
				delete(methods, name)
			}
		}
		if len(methods) == 0 {
			delete(idx.luaMethodsByReceiverType, receiver)
		}
	}
	for key, methods := range idx.extensionMethods {
		for method, ids := range methods {
			kept := ids[:0]
			for _, id := range ids {
				if !droppedSymbols[id] {
					kept = append(kept, id)
				}
			}
			if len(kept) == 0 {
				delete(methods, method)
			} else {
				methods[method] = append([]string(nil), kept...)
			}
		}
		if len(methods) == 0 {
			delete(idx.extensionMethods, key)
		}
	}
	for fqn, id := range idx.javaFQN {
		if droppedSymbols[id] {
			delete(idx.javaFQN, fqn)
		}
	}
	for fqn, id := range idx.csharpFQN {
		if droppedSymbols[id] {
			delete(idx.csharpFQN, fqn)
		}
	}
	for fqn, fragments := range idx.csharpPartialFragments {
		kept := fragments[:0]
		for _, id := range fragments {
			if !droppedSymbols[id] {
				kept = append(kept, id)
			}
		}
		if len(kept) == 0 {
			delete(idx.csharpPartialFragments, fqn)
		} else {
			idx.csharpPartialFragments[fqn] = append([]string(nil), kept...)
		}
	}
	delete(idx.javaPackages, rel)
	delete(idx.javaImports, rel)
	delete(idx.csharpNamespaces, rel)
	delete(idx.csharpUsings, rel)
}

func loadWatchIgnores(root string) ([]ignorePattern, error) {
	patterns := make([]ignorePattern, 0, len(builtInIgnores))
	for _, pattern := range builtInIgnores {
		patterns = append(patterns, ignorePattern{value: pattern, builtIn: true})
	}
	patterns = append(patterns, readIgnoreFile(filepath.Join(root, ".gitignore"))...)
	patterns = append(patterns, readIgnoreFile(filepath.Join(root, ".mamariignore"))...)
	return patterns, nil
}

// addWatchDirs recursively registers every non-ignored directory under root
// with w. Adding an individual directory's watch can fail on large repos
// (e.g. the OS inotify watch-count limit is exhausted); such failures are
// reported via onWarn (if non-nil) and otherwise skipped rather than
// aborting the whole walk, so the rest of the repo still gets live updates.
func addWatchDirs(w *fsnotify.Watcher, root string, ignores []ignorePattern, tracked map[string]bool, onWarn func(error)) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel != "." && ignored(rel, true, ignores, tracked) {
			return filepath.SkipDir
		}
		if err := w.Add(path); err != nil && onWarn != nil {
			onWarn(fmt.Errorf("watch: failed to watch %s: %w (changes under this directory may not be detected live)", rel, err))
		}
		return nil
	})
}

func addWatchDir(w *fsnotify.Watcher, abs, root string, ignores []ignorePattern, tracked map[string]bool, onWarn func(error)) error {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(rel)
	if ignored(rel, true, ignores, tracked) {
		return nil
	}
	return filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		r, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		r = filepath.ToSlash(r)
		if ignored(r, true, ignores, tracked) {
			return filepath.SkipDir
		}
		if err := w.Add(path); err != nil && onWarn != nil {
			onWarn(fmt.Errorf("watch: failed to watch %s: %w (changes under this directory may not be detected live)", r, err))
		}
		return nil
	})
}

func relForWatch(root, abs string) string {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(rel)
}

func isWatchable(rel string) bool {
	return isIndexableSourceFile(rel)
}

func isWatchablePath(rel, abs string) bool {
	return isWatchable(rel) || isShellShebangFile(abs)
}

func isWatchableEvent(idx *Index, rel, abs string, exists bool) bool {
	if isWatchable(rel) {
		return true
	}
	if exists {
		return isShellShebangFile(abs)
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	file, ok := idx.Files[rel]
	return ok && file.Language == "bash"
}

package mamari

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	scip "github.com/sourcegraph/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

func TestTraceNamespaceHeuristicAndAmbiguity(t *testing.T) {
	root := t.TempDir()
	write(t, root, "shapes/main.ttl", `@prefix dcterms: <http://purl.org/dc/terms/> .
@prefix dct: <http://purl.org/dc/terms/> .
@prefix alt: <http://example.org/alt/> .
@prefix dcatap: <http://data.europa.eu/r5r/> .
@prefix sh: <http://www.w3.org/ns/shacl#> .

dcatap:Dataset_Shape
  a sh:NodeShape ;
  sh:path dcterms:identifier ;
  sh:path dcterms:publisher ;
  sh:path alt:publisher .
`)
	write(t, root, "src/useDataset.ts", "const NAMESPACES = {\n"+
		`  dcterms: 'http://purl.org/dc/terms/',
  ignored: runtimeValue,
} as const
export const PREFIX_SHACL = 'http://www.w3.org/ns/shacl#'
const nested = { value: NAMESPACES.dcterms }
`+"store.getQuads(subject, `${NAMESPACES.dcterms}identifier`, null, null)\n"+
		`const path = PREFIX_SHACL + 'path'
// "dcterms:publisher" should not count as a string literal reference.
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	got := TraceTerm(idx, "dcterms:identifier")
	if got.Status != "found" {
		t.Fatalf("status = %s, want found", got.Status)
	}
	if len(got.TTLUsages) == 0 {
		t.Fatal("expected TTL usage")
	}
	if len(got.CodeReferences) == 0 || got.CodeReferences[0].Confidence != "heuristic" {
		t.Fatalf("expected heuristic code reference, got %#v", got.CodeReferences)
	}
	if len(got.CodeReferences) != 1 || got.CodeReferences[0].Term != "dcterms:identifier" {
		t.Fatalf("expected a single dcterms reference, got %#v", got.CodeReferences)
	}
	ambiguous := TraceTerm(idx, "publisher")
	if ambiguous.Status != "ambiguous" || len(ambiguous.Candidates) != 2 {
		t.Fatalf("expected ambiguous publisher candidates, got %#v", ambiguous)
	}
	publisher := TraceTerm(idx, "dcterms:publisher")
	if len(publisher.CodeReferences) != 0 {
		t.Fatalf("comment string should not create a publisher code reference: %#v", publisher.CodeReferences)
	}
	notFound := TraceTerm(idx, "dcterms:missing")
	if notFound.Status != "not_found" || len(notFound.TTLUsages) != 0 || len(notFound.CodeReferences) != 0 {
		t.Fatalf("expected empty not_found response, got %#v", notFound)
	}
	missingIRI := TraceTerm(idx, "http://purl.org/dc/terms/missing")
	if missingIRI.Status != "not_found" || missingIRI.Term == nil || missingIRI.Term.Term != "dcterms:missing" {
		t.Fatalf("expected compact synthetic term for missing full IRI, got %#v", missingIRI)
	}
}

func TestFetchSource(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/file.ts", "one\ntwo\nthree\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := FetchSource(idx, "src/file.ts", 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "two\nthree\n" {
		t.Fatalf("text = %q", resp.Text)
	}
}

func TestRepositoryReadsRejectSymlinksOutsideRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation commonly requires elevated Windows privileges")
	}
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.ts")
	if err := os.WriteFile(outside, []byte("export const privateValue = 42\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape.ts")); err != nil {
		t.Fatal(err)
	}
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := idx.Files["escape.ts"]; ok {
		t.Fatal("BuildIndex followed a source symlink outside the repository")
	}

	write(t, root, "safe.ts", "export const safeValue = 1\n")
	idx, err = BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "safe.ts")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "safe.ts")); err != nil {
		t.Fatal(err)
	}
	if _, err := FetchSource(idx, "safe.ts", 1, 1); err == nil || !strings.Contains(err.Error(), "inside the indexed repo") {
		t.Fatalf("FetchSource external symlink error=%v", err)
	}
}

func TestFailedRebakeReadLeavesLastGoodBatchIntact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation commonly requires elevated Windows privileges")
	}
	root := t.TempDir()
	write(t, root, "keep.ts", "export const keep = 1\n")
	write(t, root, "change.ts", "export const before = 1\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "outside.ts")
	if err := os.WriteFile(out, []byte("export const outside = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "change.ts")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(out, filepath.Join(root, "change.ts")); err != nil {
		t.Fatal(err)
	}

	if _, _, err := rebakeChangedFiles(idx, root, []string{"change.ts"}, []string{"keep.ts"}); err == nil {
		t.Fatal("rebake unexpectedly accepted an external source symlink")
	}
	if _, ok := idx.Files["keep.ts"]; !ok {
		t.Fatal("failed rebake applied a removal from the same batch")
	}
	if got := idx.Files["change.ts"].SHA256; got != hash([]byte("export const before = 1\n")) {
		t.Fatal("failed rebake replaced the last good indexed source")
	}
}

func TestSaveIndexMovesLiteralsToSidecar(t *testing.T) {
	root := t.TempDir()
	write(t, root, "shapes/main.ttl", `@prefix ex: <http://example.org/> .
@prefix sh: <http://www.w3.org/ns/shacl#> .

ex:Shape a sh:NodeShape ; sh:name "Example"@en .
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Literals) == 0 {
		t.Fatal("expected indexed literal")
	}
	indexPath := filepath.Join(root, ".mamari", "index.json")
	if err := SaveIndex(idx, indexPath); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	data, err = maybeGunzipBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"literals"`) {
		t.Fatalf("main index should not contain literals payload: %s", data)
	}
	loaded, err := LoadIndex(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Literals) != 0 {
		t.Fatalf("literals should be lazy-loaded, got %d eager literals", len(loaded.Literals))
	}

	// Re-saving a lazy index must preserve the sidecar even when no literal
	// query has forced it into memory yet.
	if err := SaveIndex(loaded, indexPath); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadIndex(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	hits := SearchLiteral(reloaded, "Example", "en")
	if len(hits.Hits) != len(idx.Literals) {
		t.Fatalf("search hits = %d, want %d", len(hits.Hits), len(idx.Literals))
	}
}

func TestSaveIndexOmitsCodeSearchSidecarByDefault(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/lib.ts", `export function parseQuery(raw: string) {
  return raw.trim()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(root, ".mamari", "index.json")
	if err := SaveIndex(idx, indexPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".mamari", "search.json")); !os.IsNotExist(err) {
		t.Fatalf("search sidecar should be opt-in by default, got err=%v", err)
	}
	loaded, err := LoadIndex(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.codeSearchBuilt || len(loaded.codeSearchFiles) != 0 {
		t.Fatalf("LoadIndex should not eagerly hydrate code search cache by default")
	}
	resp := SearchCode(loaded, "parse raw query", SearchCodeOptions{Limit: 2, BudgetTokens: 300})
	if resp.Status != "ok" || len(resp.Hits) == 0 || resp.Hits[0].File != "src/lib.ts" {
		t.Fatalf("expected lazy search-cache build to work, got %#v", resp)
	}
}

func TestSaveIndexCanPersistCodeSearchSidecarWhenEnabled(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/lib.ts", `export function parseQuery(raw: string) {
  return raw.trim()
}
`)
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
		t.Fatalf("expected opt-in persisted search sidecar: %v", err)
	}
}

func TestCodeSearchSidecarStoresSymbolSummariesOncePerFile(t *testing.T) {
	root := t.TempDir()
	var source strings.Builder
	source.WriteString("export function longRunningHandler(input: number) {\n")
	for i := 0; i < 400; i++ {
		source.WriteString("  input += 1\n")
	}
	source.WriteString("  return input\n}\n")
	write(t, root, "src/long.ts", source.String())

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAMARI_PERSIST_SEARCH", "1")
	indexPath := filepath.Join(root, ".mamari", "index.json")
	if err := SaveIndex(idx, indexPath); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".mamari", "search.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, searchSidecarBinaryMagic) {
		t.Fatalf("search sidecar does not use the current binary format")
	}
	var payload persistedCodeSearchIndex
	if err := gob.NewDecoder(bytes.NewReader(data[len(searchSidecarBinaryMagic):])).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("persisted files=%d, want 1", len(payload.Files))
	}
	file := payload.Files[0]
	if len(file.Lines) < 400 || len(file.Symbols) == 0 {
		t.Fatalf("unexpected persisted search shape: lines=%d symbols=%d", len(file.Lines), len(file.Symbols))
	}
	if len(file.Symbols) >= len(file.Lines) {
		t.Fatalf("symbol summaries were duplicated per line: symbols=%d lines=%d", len(file.Symbols), len(file.Lines))
	}
}

// TestCodeSearchSidecarIsActuallyLoadedAndUsed proves loadCodeSearchSidecar
// is wired into ensureCodeSearchIndex and not just written-and-ignored: it
// deletes the source file after saving the sidecar, loads the index fresh,
// and confirms search-code still finds the original content. A from-scratch
// rebuild would find nothing (the source is gone), so a correct result here
// is only possible if the sidecar path was actually taken.
func TestCodeSearchSidecarIsActuallyLoadedAndUsed(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/lib.ts", `export function parseDistinctiveQueryTerm(raw: string) {
  return raw.trim()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAMARI_PERSIST_SEARCH", "1")
	indexPath := filepath.Join(root, ".mamari", "index.json")
	if err := SaveIndex(idx, indexPath); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(root, "src", "lib.ts")); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadIndex(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	resp := SearchCode(loaded, "parseDistinctiveQueryTerm", SearchCodeOptions{})
	if resp.Status != "ok" || len(resp.Hits) == 0 || resp.Hits[0].File != "src/lib.ts" {
		t.Fatalf("expected search to find content via the sidecar (source file was deleted, so a from-scratch rebuild would find nothing), got %#v", resp)
	}
}

// TestSearchCodeSymbolDetailDefaultsCompact locks in the
// a regression audit fix: a search hit's containing
// symbol summary defaults to name/kind/file/startLine only, dropping the
// id/language/signature/docstring/complexity that were the single largest
// contributor to search_code's response size. SymbolDetail:true must still
// restore every field, and — since both calls share a query/index/mode and
// only SymbolDetail differs — this also proves the cache key was updated to
// include SymbolDetail; if it hadn't been, the second call would silently
// return the first call's cached compact result instead of the full one.
func TestSearchCodeSymbolDetailDefaultsCompact(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/lib.ts", `// runDistinctiveAnchorTask does the distinctive anchor task.
export function runDistinctiveAnchorTask(raw: string) {
  return raw.trim()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	compact := SearchCode(idx, "runDistinctiveAnchorTask", SearchCodeOptions{})
	if compact.Status != "ok" || len(compact.Hits) == 0 || len(compact.Hits[0].Symbols) == 0 {
		t.Fatalf("expected a hit with at least one symbol, got %#v", compact)
	}
	cs := compact.Hits[0].Symbols[0]
	if cs.Name == "" || cs.Kind == "" || cs.File == "" || cs.StartLine == 0 {
		t.Fatalf("compact symbol summary missing required fields: %#v", cs)
	}
	if cs.ID != "" || cs.Language != "" || cs.Signature != "" || cs.Docstring != "" {
		t.Fatalf("default SearchCode call should drop id/language/signature/docstring, got %#v", cs)
	}

	detailed := SearchCode(idx, "runDistinctiveAnchorTask", SearchCodeOptions{SymbolDetail: true})
	if detailed.Status != "ok" || len(detailed.Hits) == 0 || len(detailed.Hits[0].Symbols) == 0 {
		t.Fatalf("expected a hit with at least one symbol, got %#v", detailed)
	}
	ds := detailed.Hits[0].Symbols[0]
	if ds.ID == "" || ds.Language == "" || ds.Signature == "" {
		t.Fatalf("SymbolDetail:true should restore id/language/signature, got %#v", ds)
	}
}

func TestSearchCodeBudgetIncludesSerializedMetadata(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 20; i++ {
		write(t, root,
			fmt.Sprintf("src/feature-with-a-deliberately-long-name-%02d/handler.ts", i),
			fmt.Sprintf("export function serializedBudgetAnchor%02d() { return %d }\n", i, i),
		)
	}
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	opts := SearchCodeOptions{
		Limit:        20,
		BudgetTokens: 300,
		Mode:         ModeEvidence,
	}
	unbounded := SearchCode(idx, "serialized budget anchor", opts)
	if len(unbounded.Hits) < 10 {
		t.Fatalf("fixture did not create enough metadata-heavy hits: %#v", unbounded)
	}

	bounded := SearchCode(idx, "serialized budget anchor", opts)
	FitSearchCodeResponse(&bounded, opts.BudgetTokens)
	if len(bounded.Hits) >= len(unbounded.Hits) {
		t.Fatalf("serialized budget did not trim hits: bounded=%d unbounded=%d", len(bounded.Hits), len(unbounded.Hits))
	}
	if got, max := searchCodeSerializedTokens(&bounded), opts.BudgetTokens*7/8; got > max {
		t.Fatalf("serialized response uses %d estimated tokens, want <= %d: %#v", got, max, bounded)
	}
	if !bounded.Truncated || len(bounded.Warnings) == 0 {
		t.Fatalf("trimmed response must report truncation and a warning: %#v", bounded)
	}
}

// TestSearchCodeBlastRadiusSurvivesSymbolCompaction guards against a real
// near-miss found while implementing the fix above: addSearchCodeBlastRadius
// resolves callers/callees via each symbol's ID, which the default compact
// projection drops. If the compaction ever runs before blast-radius lookup
// instead of after, this test fails with an empty BlastRadius instead of a
// populated one.
func TestSearchCodeBlastRadiusSurvivesSymbolCompaction(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/lib.ts", `export function helperForBlast(): number {
  return 1
}

export function callerForBlast(): number {
  return helperForBlast()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := SearchCode(idx, "helperForBlast", SearchCodeOptions{BlastRadius: true})
	if resp.Status != "ok" || len(resp.Hits) == 0 {
		t.Fatalf("expected at least one hit, got %#v", resp)
	}
	found := false
	for _, hit := range resp.Hits {
		if len(hit.BlastRadius) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected at least one hit to carry a populated BlastRadius even with the default compact symbol projection, got %#v", resp.Hits)
	}
}

// TestCodeSearchSidecarStaleHashFallsBackToRebuild covers the staleness
// contract: if a source file changes after the sidecar was written (so its
// content hash no longer matches), loadCodeSearchSidecar must reject the
// stale sidecar and ensureCodeSearchIndex must fall through to a correct
// from-scratch rebuild, not silently serve outdated search results.
func TestCodeSearchSidecarStaleHashFallsBackToRebuild(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/lib.ts", "export function originalName() {\n  return 1\n}\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAMARI_PERSIST_SEARCH", "1")
	indexPath := filepath.Join(root, ".mamari", "index.json")
	if err := SaveIndex(idx, indexPath); err != nil {
		t.Fatal(err)
	}
	staleSidecar, err := os.ReadFile(filepath.Join(root, ".mamari", "search.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Rebuild the index against changed content (simulating an edit between
	// the sidecar write and a later process loading index.json), then put
	// the OLD (stale) sidecar back so index.json reflects the new content
	// but search.json still reflects the old content/hash.
	write(t, root, "src/lib.ts", "export function renamedDistinctiveName() {\n  return 1\n}\n")
	idx2, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveIndex(idx2, indexPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".mamari", "search.json"), staleSidecar, 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadIndex(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	resp := SearchCode(loaded, "renamedDistinctiveName", SearchCodeOptions{})
	if resp.Status != "ok" || len(resp.Hits) == 0 {
		t.Fatalf("expected a stale sidecar to be rejected and the new content found via rebuild, got %#v", resp)
	}
}

func TestTTLScannerDoesNotPanicOnUnterminatedShape(t *testing.T) {
	root := t.TempDir()
	write(t, root, "shapes/broken.ttl", `@prefix ex: <http://example.org/> .
@prefix sh: <http://www.w3.org/ns/shacl#> .

ex:BrokenShape
  a sh:NodeShape ;
  sh:path ex:name
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	got := TraceTerm(idx, "ex:name")
	if got.Status != "found" || len(got.TTLUsages) != 1 {
		t.Fatalf("expected unterminated shape evidence, got %#v", got)
	}
}

func TestTTLFileScopedPrefixesAndArbitraryPredicateUsages(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a/hash.ttl", `@prefix custom: <http://example.org/ns/custom#> .
@prefix ex: <http://example.org/> .
@prefix sh: <http://www.w3.org/ns/shacl#> .

ex:HashShape
  a sh:NodeShape ;
  custom:hideIf ex:Condition .
`)
	write(t, root, "b/nohash.ttl", `@prefix custom: <http://example.org/ns/custom> .
@prefix ex: <http://example.org/> .
@prefix sh: <http://www.w3.org/ns/shacl#> .

ex:NoHashShape
  a sh:NodeShape ;
  custom:hideIf ex:Condition .
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	ambiguous := TraceTerm(idx, "custom:hideIf")
	if ambiguous.Status != "ambiguous" || len(ambiguous.Candidates) != 2 {
		t.Fatalf("expected file-scoped prefix ambiguity, got %#v", ambiguous)
	}
	hashIRI := TraceTerm(idx, "http://example.org/ns/custom#hideIf")
	if hashIRI.Status != "found" || len(hashIRI.TTLUsages) != 1 || hashIRI.TTLUsages[0].File != "a/hash.ttl" {
		t.Fatalf("expected hash-prefixed hideIf usage only, got %#v", hashIRI)
	}
	noHashIRI := TraceTerm(idx, "http://example.org/ns/customhideIf")
	if noHashIRI.Status != "found" || len(noHashIRI.TTLUsages) != 1 || noHashIRI.TTLUsages[0].File != "b/nohash.ttl" {
		t.Fatalf("expected no-hash-prefixed hideIf usage only, got %#v", noHashIRI)
	}
}

func TestWeakLocalReferencesSkipCommonWords(t *testing.T) {
	root := t.TempDir()
	write(t, root, "shapes/main.ttl", `@prefix sh: <http://www.w3.org/ns/shacl#> .
@prefix ex: <http://example.org/> .

ex:Shape
  a sh:NodeShape ;
  sh:path ex:name .
`)
	write(t, root, "src/svg.ts", `document.createElementNS("http://www.w3.org/2000/svg", "path")
const publisher = "publisher"
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	path := TraceTerm(idx, "sh:path")
	for _, ref := range path.CodeReferences {
		if ref.Confidence == "weak" {
			t.Fatalf("did not expect weak local path reference: %#v", ref)
		}
	}
}

func TestVueTemplateScanning(t *testing.T) {
	root := t.TempDir()
	write(t, root, "shapes/main.ttl", `@prefix dcterms: <http://purl.org/dc/terms/> .
@prefix sh: <http://www.w3.org/ns/shacl#> .

ex:Shape a sh:NodeShape ; sh:path dcterms:identifier .
`)
	write(t, root, "src/Card.vue", `<template>
  <a :href="'http://purl.org/dc/terms/identifier'">id</a>
  <span :data-term="'dcterms:identifier'">x</span>
  <!-- "dcterms:identifier" inside an html comment -->
</template>
<script setup lang="ts">
const noop = 1
</script>
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	got := TraceTerm(idx, "dcterms:identifier")
	if got.Status != "found" {
		t.Fatalf("status = %s, want found", got.Status)
	}
	var vueRefs []Reference
	for _, ref := range got.CodeReferences {
		if ref.File == "src/Card.vue" {
			vueRefs = append(vueRefs, ref)
		}
	}
	if len(vueRefs) < 2 {
		t.Fatalf("expected at least two Vue template references (literal-iri and prefixed-literal), got %#v", vueRefs)
	}
	for _, ref := range vueRefs {
		if ref.StartLine == 4 {
			t.Fatalf("html-commented term must not produce a reference: %#v", ref)
		}
	}
}

func TestJavaScriptScanning(t *testing.T) {
	root := t.TempDir()
	write(t, root, "shapes/main.ttl", `@prefix dcterms: <http://purl.org/dc/terms/> .
@prefix sh: <http://www.w3.org/ns/shacl#> .

ex:Shape a sh:NodeShape ; sh:path dcterms:identifier .
`)
	write(t, root, "src/lib.mjs", "const NAMESPACES = {\n  dcterms: 'http://purl.org/dc/terms/',\n}\nconst path = `${NAMESPACES.dcterms}identifier`\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	got := TraceTerm(idx, "dcterms:identifier")
	found := false
	for _, ref := range got.CodeReferences {
		if ref.File == "src/lib.mjs" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected reference inside .mjs file, got %#v", got.CodeReferences)
	}
}

func TestWeakRefsOptIn(t *testing.T) {
	root := t.TempDir()
	write(t, root, "shapes/main.ttl", `@prefix ex: <http://example.org/> .
@prefix sh: <http://www.w3.org/ns/shacl#> .

ex:Shape a sh:NodeShape ; sh:path ex:identifier .
`)
	write(t, root, "src/foo.ts", "const local = 'identifier'\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	def := TraceTerm(idx, "ex:identifier")
	for _, ref := range def.CodeReferences {
		if ref.Confidence == "weak" {
			t.Fatalf("default trace must not include weak refs, got %#v", ref)
		}
	}
	withWeak := TraceTerm(idx, "ex:identifier", QueryOptions{IncludeWeak: true})
	hasWeak := false
	for _, ref := range withWeak.CodeReferences {
		if ref.Confidence == "weak" {
			hasWeak = true
		}
	}
	if !hasWeak {
		t.Fatalf("IncludeWeak=true should expose weak refs, got %#v", withWeak.CodeReferences)
	}
}

func TestGroupedCompactTraceGroupsLocationsByFile(t *testing.T) {
	root := t.TempDir()
	write(t, root, "shapes/main.ttl", `@prefix dcterms: <http://purl.org/dc/terms/> .
@prefix sh: <http://www.w3.org/ns/shacl#> .
@prefix ex: <http://example.org/> .

ex:Shape a sh:NodeShape ; sh:path dcterms:identifier .
`)
	write(t, root, "src/use.ts", "const id = 'dcterms:identifier'\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	got := TraceTermGroupedCompact(idx, "dcterms:identifier")
	if got.Status != "found" || got.TTLUsageCount == 0 || got.CodeReferenceCount == 0 {
		t.Fatalf("expected grouped found trace with ttl and code counts, got %#v", got)
	}
	if len(got.TTLUsages["shapes/main.ttl"]) == 0 {
		t.Fatalf("expected TTL usage grouped under shapes/main.ttl, got %#v", got.TTLUsages)
	}
	if len(got.CodeReferences["src/use.ts"]) == 0 {
		t.Fatalf("expected code reference grouped under src/use.ts, got %#v", got.CodeReferences)
	}
}

// TestJSBuiltinAndChainedCallsNotEmittedAsEdges guards against noise from
// calls to JS/TS globals (require, Date, ref) and Array/String/Promise
// prototype methods reached via chained calls (e.g.
// `items.filter(...).map(...)`, where `.map`'s receiver is itself a call
// expression and its Callee comes through as the bare name "map"). These
// never resolve to anything in the repo, so they must not produce "calls"
// edges (previously these dominated doctor's topUnresolved with
// unresolved:require/unresolved:push/unresolved:map/etc.).
func TestJSBuiltinAndChainedCallsNotEmittedAsEdges(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/app.ts", `import { ref } from 'vue'

export function loadItems() {
  const fs = require('fs')
  const count = ref(0)
  const out = [1, 2, 3].filter(x => x > 1).map(x => x * 2)
  const textParts = ['a', 'b']
  const text = textParts.join(' ')
  console.log('items', out)
  return Array.isArray(out) ? out : [text, fs, count]
}
`)
	write(t, root, "src/join-helper.ts", `export function join(prefix: string, seg: string): string {
  return prefix ? prefix + '/' + seg : seg
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range idx.SymbolEdges {
		if e.Type != "calls" {
			continue
		}
		for _, builtin := range []string{"require", "ref", "filter", "map", "textParts.join", "console.log", "isArray", "Array.isArray"} {
			if e.Evidence.Raw == builtin {
				t.Fatalf("did not expect a calls edge for builtin %q, got %#v", builtin, e)
			}
		}
	}
}

// TestJSNestedGenericCloseDoesNotSwallowRestOfFile covers a real,
// production bug found via real-world verification testing: the
// tokenizer glues adjacent `>` characters into a single ">>"/">>>" token
// (so a real right-shift operator doesn't toggle regex-allowed state —
// see jstoken.go's punct()), so a nested generic type close like
// `Pick<T, Exclude<K, J>>` arrives as one ">>" token, not two ">" tokens.
// Six different bracket-depth-tracking functions in jsparse.go only
// recognized a literal ">" as closing one level, so this single line
// left every one of them permanently one level "stuck" open — the type
// alias's own End (and, transitively, every symbol after it for the rest
// of the file) silently extended to the file's last token. Confirmed
// against a real 3,465-line TypeScript file (got's source/core/options.ts)
// that yielded only 4 symbols total because of exactly this construct.
func TestJSNestedGenericCloseDoesNotSwallowRestOfFile(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/types.ts", `export type Except<ObjectType, KeysType extends keyof ObjectType> =
  Pick<ObjectType, Exclude<keyof ObjectType, KeysType>>;

export class AfterTheBug {
  find(): number {
    return 1;
  }
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	except := findSymbolByName(idx, "Except")
	if except.ID == "" {
		t.Fatal("expected Except type alias to be indexed")
	}
	if except.EndLine >= 3 {
		t.Fatalf("expected Except's span to end at its own statement (line 1-2), got EndLine=%d — it swallowed what comes after it", except.EndLine)
	}
	cls := findSymbolByName(idx, "AfterTheBug")
	if cls.ID == "" {
		t.Fatal("expected AfterTheBug class declared after the generic type alias to still be indexed as its own symbol")
	}
	if findSymbolByName(idx, "find").ID == "" {
		t.Fatal("expected AfterTheBug.find to still be indexed")
	}
}

// TestJSRealShiftAndComparisonOperatorsUnaffectedByGenericCloseFix is the
// companion negative test for the fix above: ">=", ">>=", and ">>>=" are
// real comparison/compound-assignment operators that happen to share a
// ">" prefix with the glued generic-close tokens, and must not be
// mistaken for closing brackets — that would under-count depth instead
// of over-counting it, with the same "never finds the real end" failure
// mode in the opposite direction.
func TestJSRealShiftAndComparisonOperatorsUnaffectedByGenericCloseFix(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/ops.ts", `export function check(a: number, b: number): boolean {
  if (a >= b) {
    return true;
  }
  let c = a;
  c >>= 1;
  let d = a;
  d >>>= 2;
  return a >> b > 0;
}

export class AfterOps {
  find(): number {
    return 1;
  }
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	check := findSymbolByName(idx, "check")
	if check.ID == "" || check.EndLine != 10 {
		t.Fatalf("expected check() to span exactly its own body (lines 1-10), got %#v", check)
	}
	if findSymbolByName(idx, "AfterOps").ID == "" || findSymbolByName(idx, "find").ID == "" {
		t.Fatal("expected AfterOps and its find() method, declared after the shift/comparison operators, to still be indexed")
	}
}

// TestJavaSymbolsAndRelationsTreeSitter guards the tree-sitter Java path:
// class/method nesting (ParentID), extends-based super.foo() resolution,
// this.foo() scoped resolution, `new Foo()` resolving to the Foo class, and
// import edges (plain, static, wildcard).
func TestJavaSymbolsAndRelationsTreeSitter(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/main/java/com/example/BaseAccount.java", `package com.example;

public class BaseAccount {
    public void save() {
        System.out.println("base save");
    }
}
`)
	write(t, root, "src/main/java/com/example/Account.java", `package com.example;

import java.util.List;
import java.util.ArrayList;
import static java.util.Arrays.asList;
import java.util.*;

public class Account extends BaseAccount {
    private List<String> items = new ArrayList<>();

    public void save() {
        super.save();
        this.validate();
        System.out.println("saving");
    }

    private void validate() {
    }
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	accountFile := "src/main/java/com/example/Account.java"

	accountClass := findSymbolByName(idx, "Account")
	if accountClass.ID == "" || accountClass.Kind != "class" {
		t.Fatalf("expected Account class symbol, got %#v", accountClass)
	}

	var saveMethod, validateMethod, baseSave CGPSymbol
	for _, sym := range idx.Symbols {
		switch {
		case sym.Name == "save" && sym.File == accountFile:
			saveMethod = sym
		case sym.Name == "validate" && sym.File == accountFile:
			validateMethod = sym
		case sym.Name == "save" && sym.File == "src/main/java/com/example/BaseAccount.java":
			baseSave = sym
		}
	}
	if saveMethod.ID == "" || saveMethod.Kind != "method" || saveMethod.ParentID != accountClass.ID {
		t.Fatalf("expected Account.save method nested under Account class, got %#v", saveMethod)
	}
	if validateMethod.ID == "" {
		t.Fatalf("expected Account.validate method symbol")
	}
	if baseSave.ID == "" {
		t.Fatalf("expected BaseAccount.save method symbol")
	}

	qualified := findSymbols(idx, "Account.save")
	if len(qualified) != 1 || qualified[0].ID != saveMethod.ID {
		t.Fatalf("expected Account.save to resolve the nested method exactly, got %#v", qualified)
	}
	if got := findSymbols(idx, "BaseAccount.save"); len(got) != 1 || got[0].ID != baseSave.ID {
		t.Fatalf("expected BaseAccount.save to resolve the base method exactly, got %#v", got)
	}

	var sawSuperCall, sawThisCall bool
	for _, e := range idx.SymbolEdges {
		if e.Type != "calls" || e.From != saveMethod.ID {
			continue
		}
		switch e.Evidence.Raw {
		case "super.save":
			if e.To != baseSave.ID || e.Confidence != ConfScoped {
				t.Fatalf("expected super.save() to resolve to BaseAccount.save with scoped confidence, got %#v", e)
			}
			sawSuperCall = true
		case "this.validate":
			if e.To != validateMethod.ID || e.Confidence != ConfScoped {
				t.Fatalf("expected this.validate() to resolve to Account.validate with scoped confidence, got %#v", e)
			}
			sawThisCall = true
		}
	}
	if !sawSuperCall {
		t.Fatalf("expected a super.save() call edge from Account.save")
	}
	if !sawThisCall {
		t.Fatalf("expected a this.validate() call edge from Account.save")
	}

	wantImports := map[string]bool{
		"module:java.util.List":          false,
		"module:java.util.ArrayList":     false,
		"module:java.util.Arrays.asList": false,
		"module:java.util.*":             false,
	}
	for _, e := range idx.SymbolEdges {
		if e.Type == "imports" && e.From == fileSymbolID(accountFile) {
			if _, ok := wantImports[e.To]; ok {
				wantImports[e.To] = true
			}
		}
	}
	for spec, found := range wantImports {
		if !found {
			t.Fatalf("expected import edge to %q from %s", spec, accountFile)
		}
	}

	// System.out.println must not be emitted as a calls edge (javaGlobalReceivers).
	for _, e := range idx.SymbolEdges {
		if e.Type == "calls" && e.Evidence.Raw == "System.out.println" {
			t.Fatalf("did not expect a calls edge for System.out.println, got %#v", e)
		}
	}
}

// TestGoSymbolsAndRelationsTreeSitter covers the tree-sitter Go integration:
// struct/interface/function/method symbol kinds, same-receiver method calls,
// struct-embedding ("promotion") resolved across files, import edges, and
// suppression of builtin calls.
func TestGoSymbolsAndRelationsTreeSitter(t *testing.T) {
	root := t.TempDir()
	write(t, root, "accounts/account.go", `package accounts

import "fmt"

type Account struct {
	Balance int
}

type Notifier interface {
	Notify(msg string) error
}

func (a *Account) Save() error {
	a.validate()
	fmt.Println("saving")
	return nil
}

func (a *Account) validate() {
}
`)
	write(t, root, "accounts/wrapper.go", `package accounts

type Wrapper struct {
	Account
	Label string
}

func (w *Wrapper) Run() {
	w.Save()
	make([]int, 0)
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	accountFile := "accounts/account.go"
	wrapperFile := "accounts/wrapper.go"

	accountStruct := findSymbolByName(idx, "Account")
	if accountStruct.ID == "" || accountStruct.Kind != "class" {
		t.Fatalf("expected Account struct symbol with kind class, got %#v", accountStruct)
	}

	notifier := findSymbolByName(idx, "Notifier")
	if notifier.ID == "" || notifier.Kind != "interface" {
		t.Fatalf("expected Notifier interface symbol, got %#v", notifier)
	}

	var saveMethod, validateMethod, runMethod, wrapperStruct CGPSymbol
	for _, sym := range idx.Symbols {
		switch {
		case sym.Name == "Save" && sym.File == accountFile:
			saveMethod = sym
		case sym.Name == "validate" && sym.File == accountFile:
			validateMethod = sym
		case sym.Name == "Run" && sym.File == wrapperFile:
			runMethod = sym
		case sym.Name == "Wrapper" && sym.File == wrapperFile:
			wrapperStruct = sym
		}
	}
	if saveMethod.ID == "" || saveMethod.Kind != "method" || saveMethod.ParentID != accountStruct.ID {
		t.Fatalf("expected Account.Save method nested under Account struct, got %#v", saveMethod)
	}
	if validateMethod.ID == "" || validateMethod.ParentID != accountStruct.ID {
		t.Fatalf("expected Account.validate method nested under Account struct, got %#v", validateMethod)
	}
	if wrapperStruct.ID == "" || wrapperStruct.Kind != "class" {
		t.Fatalf("expected Wrapper struct symbol, got %#v", wrapperStruct)
	}
	if runMethod.ID == "" || runMethod.Kind != "method" || runMethod.ParentID != wrapperStruct.ID {
		t.Fatalf("expected Wrapper.Run method nested under Wrapper struct, got %#v", runMethod)
	}

	// Struct embedding: Wrapper embeds Account.
	if bases := idx.classBases[wrapperStruct.ID]; len(bases) != 1 || bases[0] != "Account" {
		t.Fatalf("expected Wrapper to embed Account, got %#v", bases)
	}

	var sawSameReceiverCall, sawPromotedCall bool
	for _, e := range idx.SymbolEdges {
		if e.Type != "calls" {
			continue
		}
		switch {
		case e.From == saveMethod.ID && e.Evidence.Raw == "a.validate":
			if e.To != validateMethod.ID || e.Confidence != ConfScoped {
				t.Fatalf("expected a.validate() to resolve to Account.validate with scoped confidence, got %#v", e)
			}
			sawSameReceiverCall = true
		case e.From == runMethod.ID && e.Evidence.Raw == "w.Save":
			if e.To != saveMethod.ID || e.Confidence != ConfScoped {
				t.Fatalf("expected w.Save() to resolve to Account.Save via embedding with scoped confidence, got %#v", e)
			}
			sawPromotedCall = true
		case e.From == saveMethod.ID && e.Evidence.Raw == "fmt.Println":
			if e.Confidence == ConfExact || e.Confidence == ConfScoped {
				t.Fatalf("did not expect fmt.Println to resolve to a local symbol with high confidence, got %#v", e)
			}
		}
	}
	if !sawSameReceiverCall {
		t.Fatalf("expected an a.validate() call edge from Account.Save")
	}
	if !sawPromotedCall {
		t.Fatalf("expected a w.Save() call edge from Wrapper.Run resolved via struct embedding")
	}

	// make([]int, 0) is a builtin call and must not be emitted as a calls edge.
	for _, e := range idx.SymbolEdges {
		if e.Type == "calls" && e.From == runMethod.ID && e.Evidence.Raw == "make" {
			t.Fatalf("did not expect a calls edge for builtin make(), got %#v", e)
		}
	}

	// Import edge from account.go -> module:fmt.
	foundFmtImport := false
	for _, e := range idx.SymbolEdges {
		if e.Type == "imports" && e.From == fileSymbolID(accountFile) && e.To == "module:fmt" {
			foundFmtImport = true
		}
	}
	if !foundFmtImport {
		t.Fatalf("expected an import edge for %s -> module:fmt", accountFile)
	}
}

// TestGoVariableTypeCallResolution covers Go declared-variable-type call
// resolution (idx.varTypes, populated from `parameter_declaration`,
// `var_declaration`, and `x := &T{...}`/`x := T{...}` short declarations):
// a parameter, an explicit `var`, and both pointer/value composite-literal
// short declarations should all resolve `recv.Method()` to Account.Save at
// scoped confidence, the same as the method's own receiver does.
func TestGoVariableTypeCallResolution(t *testing.T) {
	root := t.TempDir()
	write(t, root, "accounts/account.go", `package accounts

type Account struct {
	Balance int
}

func (a *Account) Save() error {
	return nil
}
`)
	write(t, root, "accounts/runner.go", `package accounts

func RunAccount(acc *Account) {
	acc.Save()
}

func MakeAccount() {
	var w *Account
	w.Save()

	a := &Account{Balance: 1}
	a.Save()

	b := Account{}
	b.Save()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	saveMethod := findSymbolByName(idx, "Save")
	if saveMethod.ID == "" || saveMethod.Kind != "method" {
		t.Fatalf("expected Account.Save method symbol, got %#v", saveMethod)
	}

	wantRaws := map[string]bool{
		"acc.Save": false,
		"w.Save":   false,
		"a.Save":   false,
		"b.Save":   false,
	}
	for _, e := range idx.SymbolEdges {
		if e.Type != "calls" {
			continue
		}
		if _, ok := wantRaws[e.Evidence.Raw]; !ok {
			continue
		}
		if e.To != saveMethod.ID || e.Confidence != ConfScoped {
			t.Fatalf("expected %s to resolve to Account.Save with scoped confidence, got %#v", e.Evidence.Raw, e)
		}
		wantRaws[e.Evidence.Raw] = true
	}
	for raw, found := range wantRaws {
		if !found {
			t.Fatalf("expected a %s() call edge resolved to Account.Save", raw)
		}
	}
}

// TestGoConstructorReturnTypeInference covers `x := NewThing()`-style short
// var declarations whose declared type isn't written explicitly but can be
// inferred from the callee's return-type signature (idx.goReturnTypes),
// including multi-return (`b, err := NewAccountAndErr()`) and method-call
// RHS (`repo := svc.GetRepo()`).
func TestGoConstructorReturnTypeInference(t *testing.T) {
	root := t.TempDir()
	write(t, root, "accounts/account.go", `package accounts

type Account struct {
	Balance int
}

func (a *Account) Save() error {
	return nil
}

func NewAccount() *Account {
	return &Account{}
}

func NewAccountAndErr() (*Account, error) {
	return &Account{}, nil
}
`)
	write(t, root, "accounts/repo.go", `package accounts

type Repository struct{}

func (r *Repository) Save() error {
	return nil
}

type Service struct {
	repo *Repository
}

func (s *Service) GetRepo() *Repository {
	return s.repo
}
`)
	write(t, root, "accounts/runner.go", `package accounts

func RunAll(svc *Service) {
	a := NewAccount()
	a.Save()

	b, err := NewAccountAndErr()
	_ = err
	b.Save()

	repo := svc.GetRepo()
	repo.Save()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	saveByFile := map[string]string{}
	for _, s := range idx.Symbols {
		if s.Kind == "method" && s.Name == "Save" {
			saveByFile[s.File] = s.ID
		}
	}
	if saveByFile["accounts/account.go"] == "" || saveByFile["accounts/repo.go"] == "" {
		t.Fatalf("expected Account.Save and Repository.Save method symbols, got %#v", saveByFile)
	}

	wantTargets := map[string]string{
		"a.Save":    saveByFile["accounts/account.go"],
		"b.Save":    saveByFile["accounts/account.go"],
		"repo.Save": saveByFile["accounts/repo.go"],
	}
	found := map[string]bool{}
	for _, e := range idx.SymbolEdges {
		if e.Type != "calls" {
			continue
		}
		want, ok := wantTargets[e.Evidence.Raw]
		if !ok {
			continue
		}
		if e.To != want || e.Confidence != ConfScoped {
			t.Fatalf("expected %s to resolve to %s with scoped confidence, got %#v", e.Evidence.Raw, want, e)
		}
		found[e.Evidence.Raw] = true
	}
	for raw := range wantTargets {
		if !found[raw] {
			t.Fatalf("expected a %s() call edge resolved at scoped confidence", raw)
		}
	}
}

// TestJavaVariableTypeInterfaceSpringMethodRefs covers four production-depth
// Java call-resolution enhancements together on a realistic Spring
// controller:
//   - field/local/parameter declared-type tracking resolves
//     `variable.method()` to the variable's type's method (idx.varTypes)
//   - `implements` clauses produce "implements" edges, and an
//     interface-typed variable's call resolves to the interface method (or,
//     if the interface itself declares no body, the single implementer)
//   - Spring `@RequestMapping`/`@GetMapping` annotations produce "http-route"
//     symbols and "handles-route" edges to the handler method
//   - `variable::method` method references resolve like `variable.method()`
func TestJavaVariableTypeInterfaceSpringMethodRefs(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/main/java/com/example/Pet.java", `package com.example;

public class Pet {
    public String getName() {
        return "pet";
    }
}
`)
	write(t, root, "src/main/java/com/example/Owner.java", `package com.example;

public class Owner {
    public Pet getPet() {
        return null;
    }
}
`)
	write(t, root, "src/main/java/com/example/OwnerRepository.java", `package com.example;

public interface OwnerRepository {
    Owner findById(int id);
}
`)
	write(t, root, "src/main/java/com/example/JdbcOwnerRepository.java", `package com.example;

public class JdbcOwnerRepository implements OwnerRepository {
    public Owner findById(int id) {
        return new Owner();
    }
}
`)
	write(t, root, "src/main/java/com/example/OwnerController.java", `package com.example;

import org.springframework.web.bind.annotation.*;

@RestController
@RequestMapping("/api/owners")
public class OwnerController {

    @Autowired
    private OwnerRepository owners;

    @GetMapping("/{id}")
    public Owner show(@PathVariable("id") int ownerId) {
        Owner owner = owners.findById(ownerId);
        owner.getPet();
        return owner;
    }

    public java.util.function.Function<Integer, Owner> finder() {
        return owners::findById;
    }
}
`)

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	symByNameFile := func(name, file string) CGPSymbol {
		idx.mu.Lock()
		defer idx.mu.Unlock()
		for _, sym := range idx.Symbols {
			if sym.Name == name && sym.File == file {
				return sym
			}
		}
		return CGPSymbol{}
	}

	controllerFile := "src/main/java/com/example/OwnerController.java"
	repoFile := "src/main/java/com/example/OwnerRepository.java"

	controllerClass := symByNameFile("OwnerController", controllerFile)
	if controllerClass.ID == "" {
		t.Fatalf("expected OwnerController class symbol")
	}
	showMethod := symByNameFile("show", controllerFile)
	if showMethod.ID == "" {
		t.Fatalf("expected OwnerController.show method symbol")
	}
	finderMethod := symByNameFile("finder", controllerFile)
	if finderMethod.ID == "" {
		t.Fatalf("expected OwnerController.finder method symbol")
	}
	repoFindByID := symByNameFile("findById", repoFile)
	if repoFindByID.ID == "" {
		t.Fatalf("expected OwnerRepository.findById method symbol")
	}
	ownerGetPet := symByNameFile("getPet", "src/main/java/com/example/Owner.java")
	if ownerGetPet.ID == "" {
		t.Fatalf("expected Owner.getPet method symbol")
	}
	jdbcRepoClass := symByNameFile("JdbcOwnerRepository", "src/main/java/com/example/JdbcOwnerRepository.java")
	if jdbcRepoClass.ID == "" {
		t.Fatalf("expected JdbcOwnerRepository class symbol")
	}

	// Phase A: `owners.findById(ownerId)` (field type OwnerRepository) and
	// `owner.getPet()` (local var type Owner) resolve via declared types.
	var sawFindByID, sawGetPet, sawFinderFindByID bool
	for _, e := range idx.SymbolEdges {
		if e.Type != "calls" {
			continue
		}
		switch {
		case e.From == showMethod.ID && e.Evidence.Raw == "owners.findById":
			if e.To != repoFindByID.ID || e.Confidence != ConfScoped {
				t.Fatalf("expected owners.findById() to resolve to OwnerRepository.findById with scoped confidence, got %#v", e)
			}
			sawFindByID = true
		case e.From == showMethod.ID && e.Evidence.Raw == "owner.getPet":
			if e.To != ownerGetPet.ID || e.Confidence != ConfScoped {
				t.Fatalf("expected owner.getPet() to resolve to Owner.getPet with scoped confidence, got %#v", e)
			}
			sawGetPet = true
		case e.From == finderMethod.ID && e.Evidence.Raw == "owners.findById":
			// Phase D: `owners::findById` method reference.
			if e.To != repoFindByID.ID || e.Confidence != ConfScoped {
				t.Fatalf("expected owners::findById method reference to resolve to OwnerRepository.findById with scoped confidence, got %#v", e)
			}
			sawFinderFindByID = true
		}
	}
	if !sawFindByID {
		t.Fatalf("expected owners.findById() call edge from OwnerController.show")
	}
	if !sawGetPet {
		t.Fatalf("expected owner.getPet() call edge from OwnerController.show")
	}
	if !sawFinderFindByID {
		t.Fatalf("expected owners::findById method-reference call edge from OwnerController.finder")
	}

	// Phase C: JdbcOwnerRepository implements OwnerRepository.
	var sawImplements bool
	for _, e := range idx.SymbolEdges {
		if e.Type == "implements" && e.From == jdbcRepoClass.ID && e.To == repoFindByID.ParentID {
			sawImplements = true
		}
	}
	if !sawImplements {
		t.Fatalf("expected implements edge from JdbcOwnerRepository to OwnerRepository")
	}

	// Phase B: @GetMapping("/{id}") + class-level @RequestMapping("/api/owners")
	// produces an "http-route" symbol and a handles-route edge to show().
	route := findSymbolByName(idx, "GET /api/owners/{id}")
	if route.ID == "" || route.Kind != "http-route" {
		t.Fatalf("expected http-route symbol \"GET /api/owners/{id}\", got %#v", route)
	}
	var sawHandlesRoute bool
	for _, e := range idx.SymbolEdges {
		if e.Type == "handles-route" && e.From == route.ID && e.To == showMethod.ID {
			if e.Confidence != ConfExact {
				t.Fatalf("expected handles-route edge with exact confidence, got %#v", e)
			}
			sawHandlesRoute = true
		}
	}
	if !sawHandlesRoute {
		t.Fatalf("expected handles-route edge from %q to OwnerController.show", route.Name)
	}
}

// TestCSharpSymbolsAndRelationsTreeSitter covers the tree-sitter C#
// integration's core structural behavior: class/method nesting, `base.`/
// `this.` call resolution (mirroring Java's `super`/`this`), and `using`
// import edges.
func TestCSharpSymbolsAndRelationsTreeSitter(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Accounts/BaseAccount.cs", `using System;

namespace MyApp.Accounts
{
    public class BaseAccount
    {
        public void Save()
        {
            Console.WriteLine("base save");
        }
    }
}
`)
	write(t, root, "Accounts/Account.cs", `using System;
using System.Collections.Generic;

namespace MyApp.Accounts
{
    public class Account : BaseAccount
    {
        private List<string> items = new List<string>();

        public void Save()
        {
            base.Save();
            this.Validate();
            Console.WriteLine("saving");
        }

        private void Validate()
        {
        }
    }
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	accountFile := "Accounts/Account.cs"

	accountClass := findSymbolByName(idx, "Account")
	if accountClass.ID == "" || accountClass.Kind != "class" {
		t.Fatalf("expected Account class symbol, got %#v", accountClass)
	}

	var saveMethod, validateMethod, baseSave CGPSymbol
	for _, sym := range idx.Symbols {
		switch {
		case sym.Name == "Save" && sym.File == accountFile:
			saveMethod = sym
		case sym.Name == "Validate" && sym.File == accountFile:
			validateMethod = sym
		case sym.Name == "Save" && sym.File == "Accounts/BaseAccount.cs":
			baseSave = sym
		}
	}
	if saveMethod.ID == "" || saveMethod.Kind != "method" || saveMethod.ParentID != accountClass.ID {
		t.Fatalf("expected Account.Save method nested under Account class, got %#v", saveMethod)
	}
	if validateMethod.ID == "" {
		t.Fatalf("expected Account.Validate method symbol")
	}
	if baseSave.ID == "" {
		t.Fatalf("expected BaseAccount.Save method symbol")
	}

	var sawBaseCall, sawThisCall bool
	for _, e := range idx.SymbolEdges {
		if e.Type != "calls" || e.From != saveMethod.ID {
			continue
		}
		switch e.Evidence.Raw {
		case "base.Save":
			if e.To != baseSave.ID || e.Confidence != ConfScoped {
				t.Fatalf("expected base.Save() to resolve to BaseAccount.Save with scoped confidence, got %#v", e)
			}
			sawBaseCall = true
		case "this.Validate":
			if e.To != validateMethod.ID || e.Confidence != ConfScoped {
				t.Fatalf("expected this.Validate() to resolve to Account.Validate with scoped confidence, got %#v", e)
			}
			sawThisCall = true
		}
	}
	if !sawBaseCall {
		t.Fatalf("expected a base.Save() call edge from Account.Save")
	}
	if !sawThisCall {
		t.Fatalf("expected a this.Validate() call edge from Account.Save")
	}

	wantImports := map[string]bool{
		"module:System":                     false,
		"module:System.Collections.Generic": false,
	}
	for _, e := range idx.SymbolEdges {
		if e.Type == "imports" && e.From == fileSymbolID(accountFile) {
			if _, ok := wantImports[e.To]; ok {
				wantImports[e.To] = true
			}
		}
	}
	for spec, found := range wantImports {
		if !found {
			t.Fatalf("expected import edge to %q from %s", spec, accountFile)
		}
	}

	// Console.WriteLine must not be emitted as a calls edge (csharpGlobalReceivers).
	for _, e := range idx.SymbolEdges {
		if e.Type == "calls" && e.Evidence.Raw == "Console.WriteLine" {
			t.Fatalf("did not expect a calls edge for Console.WriteLine, got %#v", e)
		}
	}
}

// TestCSharpVariableTypeInterfaceASPNetRoutes covers four production-depth
// C# call-resolution enhancements together on a realistic ASP.NET Core
// controller:
//   - field declared-type tracking resolves `variable.Method()` to the
//     variable's type's method (idx.varTypes), with cross-file resolution
//     via `using` + namespace (idx.csharpUsings/csharpNamespaces/csharpFQN)
//   - `: IOwnerRepository` produces an "implements" edge (interface naming
//     convention: "I" + uppercase), and an interface-typed variable's call
//     resolves to the interface method (or, if the interface itself declares
//     no body, the single implementer)
//   - [Route("api/[controller]")] + [HttpGet("{id}")] produce an "http-route"
//     symbol (with "[controller]" substituted by the controller's name minus
//     "Controller") and a handles-route edge to the handler method
func TestCSharpVariableTypeInterfaceASPNetRoutes(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Domain/Pet.cs", `namespace MyApp.Domain
{
    public class Pet
    {
        public string GetName()
        {
            return "pet";
        }
    }
}
`)
	write(t, root, "Domain/Owner.cs", `namespace MyApp.Domain
{
    public class Owner
    {
        public Pet GetPet()
        {
            return null;
        }
    }
}
`)
	write(t, root, "Domain/IOwnerRepository.cs", `namespace MyApp.Domain
{
    public interface IOwnerRepository
    {
        Owner FindById(int id);
    }
}
`)
	write(t, root, "Domain/JdbcOwnerRepository.cs", `namespace MyApp.Domain
{
    public class JdbcOwnerRepository : IOwnerRepository
    {
        public Owner FindById(int id)
        {
            return new Owner();
        }
    }
}
`)
	write(t, root, "Controllers/OwnersController.cs", `using MyApp.Domain;

namespace MyApp.Controllers
{
    [ApiController]
    [Route("api/[controller]")]
    public class OwnersController : ControllerBase
    {
        private readonly IOwnerRepository _owners;

        [HttpGet("{id}")]
        public Owner GetOwner(int id)
        {
            Owner owner = _owners.FindById(id);
            owner.GetPet();
            return owner;
        }
    }
}
`)

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	symByNameFile := func(name, file string) CGPSymbol {
		idx.mu.Lock()
		defer idx.mu.Unlock()
		for _, sym := range idx.Symbols {
			if sym.Name == name && sym.File == file {
				return sym
			}
		}
		return CGPSymbol{}
	}

	controllerFile := "Controllers/OwnersController.cs"
	repoFile := "Domain/IOwnerRepository.cs"

	controllerClass := symByNameFile("OwnersController", controllerFile)
	if controllerClass.ID == "" {
		t.Fatalf("expected OwnersController class symbol")
	}
	getOwnerMethod := symByNameFile("GetOwner", controllerFile)
	if getOwnerMethod.ID == "" {
		t.Fatalf("expected OwnersController.GetOwner method symbol")
	}
	repoFindByID := symByNameFile("FindById", repoFile)
	if repoFindByID.ID == "" {
		t.Fatalf("expected IOwnerRepository.FindById method symbol")
	}
	ownerGetPet := symByNameFile("GetPet", "Domain/Owner.cs")
	if ownerGetPet.ID == "" {
		t.Fatalf("expected Owner.GetPet method symbol")
	}
	jdbcRepoClass := symByNameFile("JdbcOwnerRepository", "Domain/JdbcOwnerRepository.cs")
	if jdbcRepoClass.ID == "" {
		t.Fatalf("expected JdbcOwnerRepository class symbol")
	}

	// Field declared-type resolution: `_owners.FindById(id)` (field type
	// IOwnerRepository, resolved cross-file via `using MyApp.Domain`) and
	// `owner.GetPet()` (local var of declared type Owner) resolve via
	// declared types.
	var sawFindByID, sawGetPet bool
	for _, e := range idx.SymbolEdges {
		if e.Type != "calls" {
			continue
		}
		switch {
		case e.From == getOwnerMethod.ID && e.Evidence.Raw == "_owners.FindById":
			if e.To != repoFindByID.ID || e.Confidence != ConfScoped {
				t.Fatalf("expected _owners.FindById() to resolve to IOwnerRepository.FindById with scoped confidence, got %#v", e)
			}
			sawFindByID = true
		case e.From == getOwnerMethod.ID && e.Evidence.Raw == "owner.GetPet":
			if e.To != ownerGetPet.ID || e.Confidence != ConfScoped {
				t.Fatalf("expected owner.GetPet() to resolve to Owner.GetPet with scoped confidence, got %#v", e)
			}
			sawGetPet = true
		}
	}
	if !sawFindByID {
		t.Fatalf("expected _owners.FindById() call edge from OwnersController.GetOwner")
	}
	if !sawGetPet {
		t.Fatalf("expected owner.GetPet() call edge from OwnersController.GetOwner")
	}

	// JdbcOwnerRepository : IOwnerRepository produces an "implements" edge.
	var sawImplements bool
	for _, e := range idx.SymbolEdges {
		if e.Type == "implements" && e.From == jdbcRepoClass.ID && e.To == repoFindByID.ParentID {
			sawImplements = true
		}
	}
	if !sawImplements {
		t.Fatalf("expected implements edge from JdbcOwnerRepository to IOwnerRepository")
	}

	// [Route("api/[controller]")] + [HttpGet("{id}")] produces an "http-route"
	// symbol with "[controller]" substituted by "Owners" (OwnersController
	// minus "Controller"), and a handles-route edge to GetOwner().
	route := findSymbolByName(idx, "GET api/Owners/{id}")
	if route.ID == "" || route.Kind != "http-route" {
		t.Fatalf("expected http-route symbol \"GET api/Owners/{id}\", got %#v", route)
	}
	var sawHandlesRoute bool
	for _, e := range idx.SymbolEdges {
		if e.Type == "handles-route" && e.From == route.ID && e.To == getOwnerMethod.ID {
			if e.Confidence != ConfExact {
				t.Fatalf("expected handles-route edge with exact confidence, got %#v", e)
			}
			sawHandlesRoute = true
		}
	}
	if !sawHandlesRoute {
		t.Fatalf("expected handles-route edge from %q to OwnersController.GetOwner", route.Name)
	}
}

// TestJavaMultiPackageResolution covers Java type resolution across multiple
// packages that reuse the same simple names (Widget, WidgetRepository,
// JdbcWidgetRepository) for different domains — the common real-world
// pattern of one Repository/Service/Controller per feature package. It
// verifies: (1) package-qualified ("import"/same-package) resolution picks
// the caller's own package's class rather than failing on the repo-wide
// name collision, and (2) a `for (Widget w : all)` loop variable resolves
// `w.getId()` to a method inherited from a base class declared in yet
// another package via an explicit import.
func TestJavaMultiPackageResolution(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/main/java/com/example/model/BaseEntity.java", `package com.example.model;

public class BaseEntity {
    public int getId() {
        return 0;
    }
}
`)
	write(t, root, "src/main/java/com/example/a/Widget.java", `package com.example.a;

import com.example.model.BaseEntity;

public class Widget extends BaseEntity {
}
`)
	write(t, root, "src/main/java/com/example/a/WidgetRepository.java", `package com.example.a;

import java.util.List;

public interface WidgetRepository {
    List<Widget> findAll();
}
`)
	write(t, root, "src/main/java/com/example/a/JdbcWidgetRepository.java", `package com.example.a;

import java.util.ArrayList;
import java.util.List;

public class JdbcWidgetRepository implements WidgetRepository {
    public List<Widget> findAll() {
        return new ArrayList<>();
    }
}
`)
	write(t, root, "src/main/java/com/example/a/WidgetController.java", `package com.example.a;

import java.util.List;

public class WidgetController {
    private WidgetRepository repo;

    public void list() {
        List<Widget> all = repo.findAll();
        for (Widget w : all) {
            w.getId();
        }
    }
}
`)
	write(t, root, "src/main/java/com/example/b/Widget.java", `package com.example.b;

public class Widget {
}
`)
	write(t, root, "src/main/java/com/example/b/WidgetRepository.java", `package com.example.b;

public interface WidgetRepository {
    Widget findOne();
}
`)
	write(t, root, "src/main/java/com/example/b/JdbcWidgetRepository.java", `package com.example.b;

public class JdbcWidgetRepository implements WidgetRepository {
    public Widget findOne() {
        return new Widget();
    }
}
`)

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	symByNameFile := func(name, file string) CGPSymbol {
		idx.mu.Lock()
		defer idx.mu.Unlock()
		for _, sym := range idx.Symbols {
			if sym.Name == name && sym.File == file {
				return sym
			}
		}
		return CGPSymbol{}
	}

	listMethod := symByNameFile("list", "src/main/java/com/example/a/WidgetController.java")
	if listMethod.ID == "" {
		t.Fatalf("expected WidgetController.list method symbol")
	}
	aFindAll := symByNameFile("findAll", "src/main/java/com/example/a/WidgetRepository.java")
	if aFindAll.ID == "" {
		t.Fatalf("expected com.example.a.WidgetRepository.findAll method symbol")
	}
	baseGetID := symByNameFile("getId", "src/main/java/com/example/model/BaseEntity.java")
	if baseGetID.ID == "" {
		t.Fatalf("expected BaseEntity.getId method symbol")
	}

	var sawFindAll, sawGetID bool
	for _, e := range idx.SymbolEdges {
		if e.Type != "calls" || e.From != listMethod.ID {
			continue
		}
		switch e.Evidence.Raw {
		case "repo.findAll":
			if e.To != aFindAll.ID || e.Confidence != ConfScoped {
				t.Fatalf("expected repo.findAll() to resolve to com.example.a.WidgetRepository.findAll with scoped confidence, got %#v", e)
			}
			sawFindAll = true
		case "w.getId":
			if e.To != baseGetID.ID || e.Confidence != ConfScoped {
				t.Fatalf("expected w.getId() to resolve to BaseEntity.getId with scoped confidence, got %#v", e)
			}
			sawGetID = true
		}
	}
	if !sawFindAll {
		t.Fatalf("expected repo.findAll() call edge from WidgetController.list")
	}
	if !sawGetID {
		t.Fatalf("expected w.getId() call edge from WidgetController.list")
	}
}

func TestCGPSymbolsAndCallsAcrossSupportedCodeFiles(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/app.ts", `export function loadUser() {
  return normalizeUser()
}
function normalizeUser() {
  return true
}
`)
	write(t, root, "src/Card.vue", `<template><div /></template>
<script setup lang="ts">
const saveCard = () => {
  loadUser()
}
</script>
`)
	write(t, root, "scripts/build.py", `import os

def main():
    helper()

def helper():
    return os.getcwd()
`)
	write(t, root, "shapes/main.ttl", `@prefix ex: <http://example.org/> .
@prefix sh: <http://www.w3.org/ns/shacl#> .

ex:Shape a sh:NodeShape ; sh:path ex:name .
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"loadUser", "saveCard", "main", "ex:Shape"} {
		got := ListSymbols(idx, q, "", "")
		if len(got.Symbols) == 0 {
			t.Fatalf("expected CGP symbol for %s, got none", q)
		}
	}
	trace := TraceSymbol(idx, "normalizeUser")
	if trace.Status != "found" {
		t.Fatalf("expected trace for normalizeUser, got %#v", trace)
	}
	foundCaller := false
	for _, caller := range trace.Callers {
		if caller.Name == "loadUser" {
			foundCaller = true
		}
	}
	if !foundCaller {
		t.Fatalf("expected loadUser caller for normalizeUser, got %#v", trace.Callers)
	}
	pyTrace := TraceSymbol(idx, "helper")
	if pyTrace.Status != "found" {
		t.Fatalf("expected trace for helper, got %#v", pyTrace)
	}
}

func TestFetchContextForSymbolLineAndTermHonorsBudget(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/app.ts", `import { x } from './x'

export function loadUser() {
  return normalizeUser()
}
function normalizeUser() {
  return true
}
`)
	write(t, root, "shapes/main.ttl", `@prefix ex: <http://example.org/> .
@prefix sh: <http://www.w3.org/ns/shacl#> .

ex:Shape a sh:NodeShape ; sh:path ex:name .
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	symCtx, err := FetchContext(idx, "loadUser", FetchContextOptions{BudgetTokens: 80, IncludeCallees: true})
	if err != nil {
		t.Fatal(err)
	}
	if symCtx.Status != "ok" || symCtx.EstimatedTokens > 80 || len(symCtx.Slices) == 0 {
		t.Fatalf("expected budgeted symbol context, got %#v", symCtx)
	}
	if symCtx.Target == nil || symCtx.Target.Name != "loadUser" {
		t.Fatalf("expected loadUser target, got %#v", symCtx.Target)
	}
	lineCtx, err := FetchContext(idx, "src/app.ts:4", FetchContextOptions{BudgetTokens: 60})
	if err != nil {
		t.Fatal(err)
	}
	if lineCtx.Status != "ok" || lineCtx.Target == nil || lineCtx.Target.Name != "loadUser" {
		t.Fatalf("expected file:line to resolve to containing symbol, got %#v", lineCtx)
	}
	termCtx, err := FetchContext(idx, "ex:name", FetchContextOptions{BudgetTokens: 80, ContextLines: 2})
	if err != nil {
		t.Fatal(err)
	}
	if termCtx.Status != "ok" || len(termCtx.Slices) == 0 {
		t.Fatalf("expected RDF term context, got %#v", termCtx)
	}
}

func TestFetchContextManyMergesOverlappingRanges(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/emailTemplateParser.js", `export class EmailTemplateParser {
  parseChoice(rawChoice) {
    const nestedMatches = this.findRadioGroupMatches(rawChoice)
    const fieldReferences = []
    return { nestedMatches, fieldReferences }
  }

  replaceRadioGroupPlaceholders(htmlContent, radioGroups) {
    return radioGroups.map(group => group.selected).join(htmlContent)
  }
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := FetchContextMany(idx, []string{
		"src/emailTemplateParser.js:3",
		"src/emailTemplateParser.js:4",
	}, FetchContextOptions{BudgetTokens: 1000, ContextLines: 3})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != "ok" {
		t.Fatalf("FetchContextMany status=%s warnings=%v", resp.Status, resp.Warnings)
	}
	if len(resp.Slices) != 1 {
		t.Fatalf("expected overlapping ranges to merge into one slice, got %#v", resp.Slices)
	}
	if got := resp.Slices[0].FocusLines; len(got) != 2 || got[0] != 3 || got[1] != 4 {
		t.Fatalf("expected merged focus lines [3 4], got %#v", got)
	}
	if !strings.Contains(resp.Slices[0].Text, "nestedMatches") || !strings.Contains(resp.Slices[0].Text, "fieldReferences") {
		t.Fatalf("merged slice lost evidence: %q", resp.Slices[0].Text)
	}
}

func TestImportedNamespaceResolution(t *testing.T) {
	root := t.TempDir()
	write(t, root, "shapes/main.ttl", `@prefix xsd: <http://www.w3.org/2001/XMLSchema#> .
@prefix ex: <http://example.org/> .
@prefix dcterms: <http://purl.org/dc/terms/> .
@prefix sh: <http://www.w3.org/ns/shacl#> .

ex:Shape a sh:NodeShape ; sh:path xsd:date ; sh:path dcterms:publisher .
`)
	write(t, root, "src/constants.ts", `export const PREFIX_XSD = 'http://www.w3.org/2001/XMLSchema#'
export const NAMESPACES = {
  dcterms: 'http://purl.org/dc/terms/',
} as const
`)
	write(t, root, "src/uses-named.ts", "import { PREFIX_XSD } from './constants'\n"+
		"const a = `${PREFIX_XSD}date`\n"+
		"const b = PREFIX_XSD + 'date'\n")
	write(t, root, "src/uses-renamed.ts", "import { PREFIX_XSD as XSD } from './constants'\n"+
		"const a = `${XSD}date`\n")
	write(t, root, "src/uses-object.ts", "import { NAMESPACES as NS } from './constants'\n"+
		"const a = `${NS.dcterms}publisher`\n")
	write(t, root, "src/uses-star.ts", "import * as C from './constants'\n"+
		"const a = `${C.PREFIX_XSD}date`\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	got := TraceTerm(idx, "xsd:date")
	hit := func(file string) bool {
		for _, ref := range got.CodeReferences {
			if ref.File == file {
				return true
			}
		}
		return false
	}
	for _, file := range []string{"src/uses-named.ts", "src/uses-renamed.ts", "src/uses-star.ts"} {
		if !hit(file) {
			t.Fatalf("expected ref to xsd:date in %s, got %#v", file, got.CodeReferences)
		}
	}
	pubGot := TraceTerm(idx, "http://purl.org/dc/terms/publisher")
	objHit := false
	for _, ref := range pubGot.CodeReferences {
		if ref.File == "src/uses-object.ts" {
			objHit = true
		}
	}
	if !objHit {
		t.Fatalf("expected object-import ref in uses-object.ts, got %#v", pubGot.CodeReferences)
	}
}

func TestDynamicIRICalls(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/foo.ts", "import { DataFactory } from 'n3'\n"+
		"const a = DataFactory.namedNode('http://example.org/static')\n"+
		"const b = DataFactory.namedNode(`${PREFIX_X}foo`)\n"+
		"const c = DataFactory.namedNode(someUrl)\n"+
		"const d = namedNode(this.config.attributes.foo + uuidv4())\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	calls := ListDynamicIRIs(idx, "src/foo.ts").Calls
	if len(calls) != 2 {
		t.Fatalf("expected exactly 2 dynamic calls (variable, dynamic concat); static and template-NS are resolvable, got %#v", calls)
	}
	want := map[string]bool{
		"someUrl":                               true,
		"this.config.attributes.foo + uuidv4()": true,
	}
	for _, c := range calls {
		if !want[c.Snippet] {
			t.Fatalf("unexpected dynamic snippet %q", c.Snippet)
		}
		if strings.Contains(c.Snippet, "'http://example.org/static'") {
			t.Fatalf("plain string-literal namedNode should not be reported: %#v", c)
		}
	}
}

func TestLiteralIndexAndSearch(t *testing.T) {
	root := t.TempDir()
	write(t, root, "shapes/main.ttl", `@prefix dcterms: <http://purl.org/dc/terms/> .
@prefix sh: <http://www.w3.org/ns/shacl#> .
@prefix xsd: <http://www.w3.org/2001/XMLSchema#> .
@prefix ex: <http://example.org/> .

ex:DatasetShape
  a sh:NodeShape ;
  sh:property [
    sh:path dcterms:issued ;
    sh:name "Issued"@en, "Erstellungsdatum"@de ;
    sh:message "Acceptable date format YYYY-MM-DD"@en,
               "Akzeptables Datumsformat YYYY-MM-DD"@de ;
    sh:or ( [ sh:name "Nur Datum"@de, "Only date"@en ;
              sh:datatype xsd:date ;
              sh:pattern "\\d{4}-\\d{2}-\\d{2}" ]
            [ sh:name "Datum mit Uhrzeit"@de, "Date with Time"@en ;
              sh:datatype xsd:dateTime ;
              sh:pattern "\\d{4}-\\d{2}-\\d{2}T\\d{2}:\\d{2}:\\d{2}" ] ) ;
  ] .
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	hits := SearchLiteral(idx, "Erstellungsdatum", "").Hits
	if len(hits) == 0 {
		t.Fatal("expected to find 'Erstellungsdatum' literal")
	}
	if hits[0].Predicate != "sh:name" || hits[0].Lang != "de" {
		t.Fatalf("unexpected literal metadata: %#v", hits[0])
	}
	deOnly := SearchLiteral(idx, "Datum", "de").Hits
	if len(deOnly) < 3 {
		t.Fatalf("expected several @de hits for 'Datum', got %d", len(deOnly))
	}
	for _, h := range deOnly {
		if h.Lang != "de" {
			t.Fatalf("lang filter leaked %q", h.Lang)
		}
	}
}

func TestSHorBranchesIndexed(t *testing.T) {
	root := t.TempDir()
	write(t, root, "shapes/main.ttl", `@prefix dcterms: <http://purl.org/dc/terms/> .
@prefix sh: <http://www.w3.org/ns/shacl#> .
@prefix xsd: <http://www.w3.org/2001/XMLSchema#> .
@prefix ex: <http://example.org/> .

ex:DatasetShape
  a sh:NodeShape ;
  sh:path dcterms:issued ;
  sh:or ( [ sh:name "Nur Datum"@de ;
            sh:datatype xsd:date ;
            sh:pattern "\\d{4}-\\d{2}-\\d{2}" ]
          [ sh:name "Datum mit Uhrzeit"@de ;
            sh:datatype xsd:dateTime ;
            sh:pattern "\\d{4}-\\d{2}-\\d{2}T.*" ] ) .
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	var found *Shape
	for id := range idx.Shapes {
		sh := idx.Shapes[id]
		if sh.Term == "ex:DatasetShape" {
			found = &sh
			break
		}
	}
	if found == nil {
		t.Fatal("ex:DatasetShape not indexed")
	}
	if len(found.Branches) != 2 {
		t.Fatalf("expected 2 sh:or branches, got %d (%#v)", len(found.Branches), found.Branches)
	}
	if found.Branches[0].Datatype != "xsd:date" || found.Branches[0].Name != "Nur Datum" || found.Branches[0].Pattern == "" {
		t.Fatalf("branch 0 fields wrong: %#v", found.Branches[0])
	}
	if found.Branches[1].Datatype != "xsd:dateTime" {
		t.Fatalf("branch 1 datatype = %s", found.Branches[1].Datatype)
	}
}

func TestFindContainingShape(t *testing.T) {
	root := t.TempDir()
	write(t, root, "shapes/main.ttl", `@prefix dcterms: <http://purl.org/dc/terms/> .
@prefix sh: <http://www.w3.org/ns/shacl#> .
@prefix xsd: <http://www.w3.org/2001/XMLSchema#> .
@prefix ex: <http://example.org/> .

ex:DatasetShape
  a sh:NodeShape ;
  sh:path dcterms:issued ;
  sh:or ( [ sh:datatype xsd:date ]
          [ sh:datatype xsd:dateTime ] ) .

ex:OtherShape
  a sh:NodeShape ;
  sh:path dcterms:title .
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	got := FindContainingShape(idx, "dcterms:issued")
	if len(got.Containers) != 1 || got.Containers[0].Term != "ex:DatasetShape" {
		t.Fatalf("expected DatasetShape via path, got %#v", got.Containers)
	}
	dt := FindContainingShape(idx, "xsd:date")
	if len(dt.Containers) != 1 || dt.Containers[0].Term != "ex:DatasetShape" {
		t.Fatalf("expected DatasetShape via branch datatype, got %#v", dt.Containers)
	}
	none := FindContainingShape(idx, "dcterms:missing")
	if len(none.Containers) != 0 {
		t.Fatalf("expected no containers for missing term, got %#v", none.Containers)
	}
}

func TestCompactIRIRejectsInvalidLocalName(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a/hash.ttl", `@prefix custom: <http://example.org/ns/custom#> .
@prefix sh: <http://www.w3.org/ns/shacl#> .
@prefix ex: <http://example.org/> .

ex:Shape a sh:NodeShape ; custom:hideIf ex:Cond .
`)
	write(t, root, "b/nohash.ttl", `@prefix custom: <http://example.org/ns/custom> .
@prefix sh: <http://www.w3.org/ns/shacl#> .
@prefix ex: <http://example.org/> .

ex:Shape2 a sh:NodeShape ; <http://example.org/ns/custom#hideIf> ex:Cond .
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	for id, term := range idx.Terms {
		if strings.Contains(term.Term, ":#") || strings.HasPrefix(term.LocalName, "#") {
			t.Fatalf("phantom term created with invalid local name: id=%s term=%#v", id, term)
		}
	}
	got := TraceTerm(idx, "http://example.org/ns/custom#hideIf")
	if got.Status != "found" {
		t.Fatalf("expected single match for hash IRI, got status %s candidates %#v", got.Status, got.Candidates)
	}
}

func TestCustomPredicateTrackedInShape(t *testing.T) {
	root := t.TempDir()
	write(t, root, "shapes/main.ttl", `@prefix custom: <http://example.org/ns/custom#> .
@prefix sh: <http://www.w3.org/ns/shacl#> .
@prefix ex: <http://example.org/> .

ex:DatasetShape
  a sh:NodeShape ;
  sh:path ex:foo ;
  custom:hideIf ex:Cond .
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	got := FindContainingShape(idx, "custom:hideIf")
	if len(got.Containers) != 1 || got.Containers[0].Term != "ex:DatasetShape" {
		t.Fatalf("expected DatasetShape via predicate custom:hideIf, got %#v", got.Containers)
	}
	objHit := FindContainingShape(idx, "ex:Cond")
	if len(objHit.Containers) != 1 {
		t.Fatalf("expected DatasetShape via custom-predicate object, got %#v", objHit.Containers)
	}
}

func TestWalkRepoSkipsCommonVendoredAndBuildDirs(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/main.ts", "const keep = true\n")
	write(t, root, "vendor/pkg/noise.ts", "const noisy = true\n")
	write(t, root, "third_party/pkg/noise.ts", "const noisy = true\n")
	write(t, root, ".next/server/noise.js", "const noisy = true\n")
	write(t, root, "storybook-static/noise.js", "const noisy = true\n")
	write(t, root, "ios/Pods/Library/noise.swift", "func noisy() {}\n")
	write(t, root, "ios/DerivedData/App/noise.swift", "func noisy() {}\n")
	write(t, root, ".build/checkouts/noise.swift", "func noisy() {}\n")
	write(t, root, ".dart_tool/build/noise.dart", "void noisy() {}\n")
	write(t, root, ".gradle/generated/noise.kt", "fun noisy() {}\n")
	write(t, root, ".terraform/modules/noise.tf", "module \"noise\" {}\n")
	write(t, root, "server/obj/Debug/noise.cs", "class Noise {}\n")
	files, err := WalkRepo(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "src/main.ts" {
		t.Fatalf("expected only src/main.ts, got %#v", files)
	}
}

func TestWalkRepoBareCustomIgnoreDoesNotDropSourceDirectory(t *testing.T) {
	root := t.TempDir()
	write(t, root, ".gitignore", "mamari\n")
	write(t, root, "mamari", "binary placeholder")
	write(t, root, "internal/mamari/index.go", "package mamari\n")
	files, err := WalkRepo(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "internal/mamari/index.go" {
		t.Fatalf("bare binary ignore should keep source directory, got %#v", files)
	}
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCallExtractionMasksStringsAndComments verifies that the parser does
// NOT record call edges for identifiers that appear inside string literals,
// template literals, regex literals, or line/block comments. The previous
// regex-based scanner produced false-positive edges in all of these.
func TestCallExtractionMasksStringsAndComments(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/app.ts", `export function caller() {
  return realCallee()
}
function realCallee() { return 1 }
function noisy() {
  // realCallee()
  /* realCallee() */
  const a = "realCallee()"
  const b = `+"`"+`realCallee()`+"`"+`
  const r = /realCallee\(\)/
  return 0
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	trace := TraceSymbol(idx, "realCallee")
	if trace.Status != "found" {
		t.Fatalf("expected trace for realCallee, got %#v", trace)
	}
	for _, caller := range trace.Callers {
		if caller.Name == "noisy" {
			t.Fatalf("noisy() should not be recorded as a caller — its references to realCallee live inside comments/strings/regex: %#v", trace.Callers)
		}
	}
	if len(trace.Callers) != 1 || trace.Callers[0].Name != "caller" {
		t.Fatalf("expected exactly one real caller (caller), got %#v", trace.Callers)
	}
}

// TestClassMethodSymbolsAndCalls verifies that class methods, getters/setters,
// and static methods are emitted as their own symbols with the class as
// parent, and that calls inside method bodies are attributed to the method.
func TestClassMethodSymbolsAndCalls(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/store.ts", `export class Store<T extends { id: string }> {
  static empty(): Store<never> {
    return new Store()
  }
  get count() { return this.items.length }
  set count(v: number) { this.items.length = v }
  async load() {
    const data = await fetchData()
    this.items = data
  }
  items: T[] = []
}
function fetchData() { return [] }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	syms := ListSymbols(idx, "load", "", "").Symbols
	if len(syms) == 0 {
		t.Fatalf("expected class method `load` to be a symbol, got none")
	}
	if syms[0].Kind != "method" {
		t.Fatalf("expected kind=method for load, got %s", syms[0].Kind)
	}

	getter := ListSymbols(idx, "count", "getter", "").Symbols
	if len(getter) == 0 {
		t.Fatalf("expected getter symbol for count, got none")
	}
	setter := ListSymbols(idx, "count", "setter", "").Symbols
	if len(setter) == 0 {
		t.Fatalf("expected setter symbol for count, got none")
	}

	// Generic type parameter `<T extends { id: string }>` must not confuse
	// the body brace finder — Store should still cover the whole class body.
	classes := ListSymbols(idx, "Store", "class", "").Symbols
	if len(classes) == 0 {
		t.Fatalf("expected class Store, got none")
	}
	store := classes[0]
	if store.StartLine == 0 {
		t.Fatalf("class Store has zero start line: %#v", store)
	}

	// fetchData() inside load() must be attributed to load, not to Store.
	trace := TraceSymbol(idx, "fetchData")
	if trace.Status != "found" {
		t.Fatalf("expected fetchData symbol, got %#v", trace)
	}
	foundLoad := false
	for _, c := range trace.Callers {
		if c.Name == "load" && c.Kind == "method" {
			foundLoad = true
		}
	}
	if !foundLoad {
		t.Fatalf("expected load() to be a caller of fetchData, got %#v", trace.Callers)
	}
}

func TestTypeScriptUnionParamFunctionSymbolsWithASI(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/useDataset.ts", `const NAMESPACES = {
  dcterms: 'http://purl.org/dc/terms/',
} as const
export function useDataset(datasetIRI: Ref<string> | ComputedRef<string>) {
  const auth = useAuth()
  return { auth, datasetIRI }
}
function useAuth() { return {} }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if syms := ListSymbols(idx, "useDataset", "function", "").Symbols; len(syms) == 0 {
		t.Fatalf("expected union-typed exported function useDataset to be indexed")
	}
	trace := TraceSymbol(idx, "useAuth")
	if trace.Status != "found" {
		t.Fatalf("expected useAuth symbol, got %#v", trace)
	}
	found := false
	for _, caller := range trace.Callers {
		if caller.Name == "useDataset" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected useDataset caller for useAuth, got %#v", trace.Callers)
	}
}

func TestTopLevelConstASIDoesNotEatFollowingDeclarations(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/app.ts", `const appVersion = import.meta.env.VITE_APP_VERSION || '$VITE_APP_VERSION'

const { token } = useAuth()
function useAuth() { return {} }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sym := range idx.Symbols {
		if sym.Name == "appVersion" {
			found = true
			if sym.EndLine != 1 {
				t.Fatalf("appVersion should end before following declarations, got end line %d", sym.EndLine)
			}
		}
	}
	if !found {
		t.Fatalf("expected appVersion symbol")
	}
}

func TestConstantInitializerCallsAreIndexed(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/app.ts", `const auth = useAuth()
function useAuth() { return {} }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	trace := TraceSymbol(idx, "useAuth")
	if trace.Status != "found" {
		t.Fatalf("expected useAuth symbol, got %#v", trace)
	}
	if len(trace.Callers) != 1 || trace.Callers[0].Name != "auth" {
		t.Fatalf("expected auth constant to be the useAuth caller, got %#v", trace.Callers)
	}
}

func TestTemplateInterpolationCallsAreIndexed(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/useDataset.ts", "export function useDataset() {\n"+
		"  return `${useAuth().token.value}`\n"+
		"}\n"+
		"function useAuth() { return {} }\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	trace := TraceSymbol(idx, "useAuth")
	if trace.Status != "found" {
		t.Fatalf("expected useAuth symbol, got %#v", trace)
	}
	found := false
	for _, caller := range trace.Callers {
		if caller.Name == "useDataset" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected template interpolation call to be attributed to useDataset, got %#v", trace.Callers)
	}
}

func TestThisMethodCallsResolveToClassMethods(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/loader.ts", `class Loader {
  private urls: string[] = []
  load() {
    return this.fetchRDF()
  }
  fetchRDF() { return [] }
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	trace := TraceSymbol(idx, "fetchRDF")
	if trace.Status != "found" {
		t.Fatalf("expected fetchRDF symbol, got %#v", trace)
	}
	found := false
	for _, caller := range trace.Callers {
		if caller.Name == "load" && caller.Kind == "method" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected load() to call fetchRDF through this.fetchRDF(), got %#v", trace.Callers)
	}
}

func TestVueTemplateEventBindingsCallScriptSymbols(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/LoginButton.vue", `<template>
  <button @click="handleLogin">Log in</button>
  <form v-on:submit.prevent="this.submitForm()"></form>
  <!-- <button @click="commentedHandler">ghost</button> -->
</template>
<script setup lang="ts">
function handleLogin() {}
function submitForm() {}
function commentedHandler() {}
</script>
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"handleLogin", "submitForm"} {
		trace := TraceSymbol(idx, name)
		if trace.Status != "found" {
			t.Fatalf("expected %s symbol, got %#v", name, trace)
		}
		foundComponent := false
		for _, caller := range trace.Callers {
			if caller.Name == "LoginButton" && caller.Kind == "component" {
				foundComponent = true
			}
		}
		if !foundComponent {
			t.Fatalf("expected Vue component caller for %s, got %#v", name, trace.Callers)
		}
	}
	commented := TraceSymbol(idx, "commentedHandler")
	for _, caller := range commented.Callers {
		if caller.Name == "LoginButton" {
			t.Fatalf("commented template event must not create a caller edge: %#v", commented.Callers)
		}
	}
}

func TestVueTemplateExpressionBindingsCallScriptSymbols(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/FacetSidebar.vue", `<template>
  <DataOwnerCard
    :icon="getAccessLevelIcon(item.id)"
    v-bind:label="getAccessLevelLabel(item)"
    v-model:selected="selectedAccessLevel"
  >
    {{ formatTooltip(getAccessLevelTooltipKey(item.id)) }}
  </DataOwnerCard>
</template>
<script setup lang="ts">
const selectedAccessLevel = ''
function getAccessLevelIcon(id: string) { return id }
function getAccessLevelLabel(item: { id: string }) { return item.id }
function getAccessLevelTooltipKey(id: string) { return id }
function formatTooltip(key: string) { return key }
</script>
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"getAccessLevelIcon", "getAccessLevelLabel", "getAccessLevelTooltipKey", "formatTooltip"} {
		trace := TraceSymbol(idx, name)
		if trace.Status != "found" {
			t.Fatalf("expected %s symbol, got %#v", name, trace)
		}
		foundComponent := false
		for _, caller := range trace.Callers {
			if caller.Name == "FacetSidebar" && caller.Kind == "component" {
				foundComponent = true
			}
		}
		if !foundComponent {
			t.Fatalf("expected Vue component caller for %s, got %#v", name, trace.Callers)
		}
		if len(trace.CallerSites) == 0 {
			t.Fatalf("expected caller-site evidence for %s, got %#v", name, trace)
		}
	}
}

func TestListSymbolsRanksExactProductionSymbolsBeforeTemplateNoise(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/FacetSidebar.vue", `<template>
  <div class="FacetSidebar flex gap-2"></div>
</template>
<script setup lang="ts">
function getAccessLevelIcon(id: string) { return id }
</script>
`)
	write(t, root, "src/FacetSidebar.story.vue", `<template>
  <FacetSidebar class="bg-white" />
</template>
<script setup lang="ts">
import FacetSidebar from './FacetSidebar.vue'
</script>
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	resp := ListSymbolsWithOptions(idx, "FacetSidebar", "", "", ListSymbolsOptions{Limit: 2})
	if len(resp.Symbols) == 0 {
		t.Fatalf("expected ranked symbols")
	}
	if resp.Symbols[0].Name != "FacetSidebar" || resp.Symbols[0].Kind != "component" || resp.Symbols[0].File != "src/FacetSidebar.vue" {
		t.Fatalf("expected main component first, got %#v", resp.Symbols)
	}
	if !resp.Truncated || resp.Total <= len(resp.Symbols) {
		t.Fatalf("expected truncated metadata with total count, got %#v", resp)
	}

	sourceOnly := ListSymbolsWithOptions(idx, "FacetSidebar", "", "", ListSymbolsOptions{SourceOnly: true})
	for _, sym := range sourceOnly.Symbols {
		if strings.Contains(sym.File, ".story.") {
			t.Fatalf("source-only should exclude story symbols, got %#v", sourceOnly.Symbols)
		}
	}
	scored := ListSymbolsWithOptions(idx, "FacetSidebar", "", "", ListSymbolsOptions{Limit: 1, WithScores: true})
	if len(scored.Symbols) != 1 || scored.Symbols[0].Score == 0 {
		t.Fatalf("expected scored top symbol, got %#v", scored.Symbols)
	}
}

func TestListSymbolsEmptyResultIsNotFoundWithEmptyArray(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/main.ts", `export function getAccessLevelIcon(id: string) { return id }`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	resp := ListSymbolsWithOptions(idx, "doesNotExistAnywhere", "", "", ListSymbolsOptions{Limit: 5})
	if resp.Status != "not_found" {
		t.Fatalf("expected not_found status for no matches, got %q", resp.Status)
	}
	if resp.Symbols == nil || len(resp.Symbols) != 0 {
		t.Fatalf("expected empty (non-nil) symbols slice, got %#v", resp.Symbols)
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"symbols":[]`) {
		t.Fatalf("expected JSON-serialized symbols to be [], got %s", data)
	}

	// An empty query should still be "ok" even with no symbols (e.g. empty repo).
	emptyRoot := t.TempDir()
	emptyIdx, err := BuildIndex(emptyRoot)
	if err != nil {
		t.Fatal(err)
	}
	emptyResp := ListSymbolsWithOptions(emptyIdx, "", "", "", ListSymbolsOptions{Limit: 5})
	if emptyResp.Status != "ok" {
		t.Fatalf("expected ok status for empty query, got %q", emptyResp.Status)
	}
}

// TestCallGraphCapturesTopLevelCallbackArgs guards against the documented
// "module-top-level anonymous callback" gap: calls made inside a callback
// argument passed to a method call at file scope (not inside any named
// function) were previously dropped from the call graph entirely because
// containingSymbolFast had no enclosing symbol to attribute them to.
func TestCallGraphCapturesTopLevelCallbackArgs(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/lib.ts", `export function useAuth() { return { token: '' } }
export function logError(e: unknown) { console.log(e) }
`)
	write(t, root, "src/axiosService.ts", `import { useAuth } from './lib'

const axiosInstance = { interceptors: { response: { use: (a: any, b: any) => {} } } }

axiosInstance.interceptors.response.use(
  res => res,
  async (error) => {
    useAuth()
    return Promise.reject(error)
  }
)
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	trace := TraceSymbol(idx, "useAuth")
	if trace.Status != "found" {
		t.Fatalf("expected useAuth to be found, got %q", trace.Status)
	}
	foundCaller := false
	for _, caller := range trace.Callers {
		if caller.File == "src/axiosService.ts" {
			foundCaller = true
		}
	}
	if !foundCaller {
		t.Fatalf("expected useAuth call inside top-level .use(cb) callback to be attributed, got callers=%#v", trace.Callers)
	}

	// arr.forEach(item => fn(item)) at module top level should also be captured.
	write(t, root, "src/forEachTopLevel.ts", `import { logError } from './lib'

const errors: unknown[] = []
errors.forEach((e) => {
  logError(e)
})
`)
	idx2, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	trace2 := TraceSymbol(idx2, "logError")
	if trace2.Status != "found" {
		t.Fatalf("expected logError to be found, got %q", trace2.Status)
	}
	foundForEachCaller := false
	for _, caller := range trace2.Callers {
		if caller.File == "src/forEachTopLevel.ts" {
			foundForEachCaller = true
		}
	}
	if !foundForEachCaller {
		t.Fatalf("expected logError call inside top-level forEach callback to be attributed, got callers=%#v", trace2.Callers)
	}
}

// TestCallGraphCapturesTopLevelCallbackAfterUnterminatedConstArrow guards
// against a real-world regression seen in axiosService.js: a preceding
// `const X = async (...) => {...}` declaration with no trailing semicolon
// and a nested object/arrow in its body, followed immediately (no
// semicolon) by a top-level `.interceptors.response.use(cb1, cb2)` call.
// skipExpression previously failed to terminate X's range at its closing
// `}`, swallowing the following statement into X and leaving braceDepth != 0
// at the .use(...) call site, so the callback symbol was never created and
// calls inside it (e.g. useAuth()) were dropped from the call graph.
func TestCallGraphCapturesTopLevelCallbackAfterUnterminatedConstArrow(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/lib.ts", `export function useAuth() { return { token: '' } }
`)
	write(t, root, "src/axiosService.ts", `import { useAuth } from './lib'

const apiClient = { interceptors: { response: { use: (a: any, b: any) => {} } } }

const showErrorMessage = async (error: any, originalRequest: any) => {
  const obj = {
    onClick: (e: any) => {
      e.stopPropagation()
    }
  }
  return obj
}

apiClient.interceptors.response.use(
  res => res,
  async (error) => {
    useAuth()
    return Promise.reject(error)
  }
)
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	trace := TraceSymbol(idx, "useAuth")
	if trace.Status != "found" {
		t.Fatalf("expected useAuth to be found, got %q", trace.Status)
	}
	for _, caller := range trace.Callers {
		if caller.File == "src/axiosService.ts" {
			return
		}
	}
	t.Fatalf("expected useAuth call inside response interceptor callback to be attributed, got callers=%#v", trace.Callers)
}

func TestVueComponentPropModelAndEmitEdges(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/components/DataOwnerCard.vue", `<template><button /></template>
<script setup lang="ts">
const props = defineProps<{
  icon?: string
  label?: string
}>()
const selected = defineModel<boolean>('selected')
const emit = defineEmits(['open-card'])
</script>
`)
	write(t, root, "src/FacetSidebar.vue", `<template>
  <DataOwnerCard
    :icon="getAccessLevelIcon(item.id)"
    label="Open"
    v-model:selected="selected"
    @open-card="handleOpen"
  />
</template>
<script setup lang="ts">
import DataOwnerCard from './components/DataOwnerCard.vue'
const selected = false
function getAccessLevelIcon(id: string) { return id }
function handleOpen() {}
</script>
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	childTrace := TraceSymbol(idx, "DataOwnerCard")
	if !callerNamed(childTrace.Callers, "FacetSidebar") {
		t.Fatalf("expected FacetSidebar to render DataOwnerCard, got %#v", childTrace)
	}
	for _, tc := range []struct {
		query string
		raw   string
	}{
		{"symbol:vue:vue-prop:src/components/DataOwnerCard.vue:icon", "DataOwnerCard.icon"},
		{"symbol:vue:vue-model:src/components/DataOwnerCard.vue:selected", "DataOwnerCard.selected"},
		{"symbol:vue:vue-emit:src/components/DataOwnerCard.vue:open-card", "DataOwnerCard.open-card"},
	} {
		trace := TraceSymbol(idx, tc.query)
		found := false
		for _, site := range trace.CallerSites {
			if site.Raw == tc.raw && site.Caller == "FacetSidebar" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected frontend edge %s for %s, got %#v", tc.raw, tc.query, trace)
		}
	}
}

func TestInspectSymbolReturnsTraceAndContextPacket(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/FacetSidebar.vue", `<template>
  <DataOwnerCard :icon="getAccessLevelIcon(item.id)" />
</template>
<script setup lang="ts">
function getAccessLevelIcon(id: string) { return id }
</script>
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := InspectSymbol(idx, "getAccessLevelIcon", InspectSymbolOptions{
		BudgetTokens: 500,
		ContextLines: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != "ok" || resp.Symbol == nil || resp.Trace.Status != "found" || resp.Context.Status != "ok" {
		t.Fatalf("unexpected inspect response: %#v", resp)
	}
	if len(resp.Trace.CallerSites) == 0 {
		t.Fatalf("inspect should include caller sites, got %#v", resp.Trace)
	}
	foundContext := false
	for _, slice := range resp.Context.Slices {
		if strings.Contains(slice.Text, `getAccessLevelIcon`) {
			foundContext = true
		}
	}
	if !foundContext {
		t.Fatalf("inspect should include bounded context for target/caller, got %#v", resp.Context.Slices)
	}
}

func TestInspectSymbolNodeReturnsCompactSourceMetadataAndGraph(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/lib.ts", `/**
 * Parse raw user query filters.
 */
export function parseQuery(raw: string): QueryResult {
  return normalize(raw)
}

function normalize(raw: string): QueryResult {
  return { raw }
}

export function caller() {
  return parseQuery('active:true')
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := InspectSymbolNode(idx, "parseQuery", InspectSymbolNodeOptions{BudgetTokens: 700})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != "ok" || resp.Symbol == nil {
		t.Fatalf("unexpected node response: %#v", resp)
	}
	if resp.Docstring != "Parse raw user query filters." {
		t.Fatalf("docstring = %q", resp.Docstring)
	}
	if len(resp.ReturnTypes) != 1 || resp.ReturnTypes[0] != "QueryResult" {
		t.Fatalf("returnTypes = %#v", resp.ReturnTypes)
	}
	if !strings.Contains(resp.Source, "export function parseQuery") || !strings.Contains(resp.Source, "return normalize(raw)") {
		t.Fatalf("node source should include compact symbol body, got %q", resp.Source)
	}
	if !callerNamed(resp.Callers, "caller") || !summaryContains(resp.Callees, "normalize") {
		t.Fatalf("expected caller and callee summaries, got callers=%#v callees=%#v", resp.Callers, resp.Callees)
	}
	if resp.EstimatedTokens <= 0 || resp.EstimatedTokens > 900 {
		t.Fatalf("unexpected estimated token count %d for %#v", resp.EstimatedTokens, resp)
	}
}

func TestVueScriptSetupArrowHandlersAfterASIBlockAreIndexed(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/TrackingDrawer.vue", `<template>
  <section>
    <button @click="fetchRecordSignatures(record, true)">Refresh</button>
    <button @click="previewEnvelope(record)">Preview</button>
  </section>
</template>
<script setup lang="ts">
const openTrackingDrawer = () => {
  visible.value = true
  fetchTrackingData()
}

const fetchTrackingData = async () => {
  const response = await apiClient.get('/documents/application/123/records')
  trackingData.value = response.data
}

const formatDate = (ds) => new Date(ds).toLocaleDateString('en-US', { month: 'short', day: 'numeric' })
const getTimelineType = (s) => ({ 'COMPLETED': 'success', 'SIGNED': 'primary', 'SENT': 'info' }[s] || 'info')

const loadSignatureSummaries = (records) => {
  records
    .filter(record => record.envelopeId && record.signedParticipants > 0)
    .forEach(record => fetchRecordSignatures(record))
}

const fetchRecordSignatures = async (record, force = false) => {
  if (!record?.id || !record.envelopeId) return
  const response = await apiClient.get('/documents/signing/' + record.id + '/signatures', {
    params: { includeChrome: false }
  })
  record._signatures = (response.data?.signatures || []).map(signature => ({
    ...signature,
    imageUrl: 'data:' + (signature.contentType || 'image/gif') + ';base64,' + signature.imageBase64
  }))
}

const previewEnvelope = async (record) => {
  const response = await apiClient.get('/documents/signing/' + record.id + '/preview', {
    responseType: 'blob'
  })
  previewPdfUrl.value = URL.createObjectURL(new Blob([response.data], { type: 'application/pdf' }))
}
</script>
`)
	write(t, root, "backend/controllers/documentController.js", `class DocumentServiceController {
  async previewEnvelopeDocuments(req, res) {
    return res.json({})
  }
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"fetchTrackingData", "fetchRecordSignatures", "previewEnvelope"} {
		syms := ListSymbols(idx, name, "function", "").Symbols
		if len(syms) != 1 || syms[0].File != "src/TrackingDrawer.vue" {
			t.Fatalf("expected Vue script setup function %s to be indexed, got %#v", name, syms)
		}
	}

	trace := TraceSymbol(idx, "fetchRecordSignatures")
	if !callerNamed(trace.Callers, "TrackingDrawer") || len(trace.CallerSites) == 0 {
		t.Fatalf("template and script callers should point at fetchRecordSignatures, got %#v", trace)
	}

	resp, err := InspectSymbol(idx, "previewEnvelope", InspectSymbolOptions{
		BudgetTokens: 500,
		ContextLines: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != "ok" || resp.Symbol == nil || resp.Symbol.File != "src/TrackingDrawer.vue" || resp.Symbol.Name != "previewEnvelope" {
		t.Fatalf("inspect-symbol should prefer the exact frontend handler over backend fuzzy matches, got %#v", resp)
	}
}

func TestSearchCodeRanksNaturalVueTaskEvidence(t *testing.T) {
	root := t.TempDir()
	write(t, root, "frontend/src/views/Documents/TrackingDrawer.vue", `<template>
  <div>
    <button @click="previewEnvelope(record)">Preview</button>
    <div v-if="record.envelopeId" class="signature-panel">
      <div class="signature-header">
        <span>Signatures</span>
        <el-button @click="fetchRecordSignatures(record, true)">Refresh</el-button>
      </div>
      <div v-else-if="record._signatures?.length" class="signature-grid">
        <div v-for="signature in record._signatures" class="signature-thumb">
          <img :src="signature.imageUrl" :alt="signature.name || signature.email" />
        </div>
      </div>
    </div>
    <el-dialog v-model="previewDialogVisible" class="envelope-preview-dialog">
      <object v-if="previewPdfUrl" :data="previewPdfUrl" type="application/pdf" class="preview-frame"></object>
    </el-dialog>
  </div>
</template>
<script setup lang="ts">
const fetchRecordSignatures = async (record, force = false) => {
  const response = await apiClient.get('/documents/signing/' + record.id + '/signatures')
  record._signatures = response.data.signatures.map(signature => ({
    imageUrl: 'data:' + signature.contentType + ';base64,' + signature.imageBase64
  }))
}
const previewEnvelope = async (record) => {
  const response = await apiClient.get('/documents/signing/' + record.id + '/preview')
  previewPdfUrl.value = URL.createObjectURL(new Blob([response.data], { type: 'application/pdf' }))
}
</script>
`)
	write(t, root, "frontend/src/views/Documents/TrackingDrawer.test.ts", `it('previews signatures', () => {
  expect(true).toBe(true)
})
`)
	write(t, root, "frontend/src/views/Documents/SignatureSuccess.vue", `<template>
  <div>Signature completed</div>
</template>
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	resp := SearchCode(idx, "how does tracking drawer show a preview of documents signatures", SearchCodeOptions{
		Limit:        5,
		BudgetTokens: 450,
		SourceOnly:   true,
	})
	if resp.Status != "ok" {
		t.Fatalf("search status = %s warnings=%v", resp.Status, resp.Warnings)
	}
	if resp.EstimatedTokens > 450 {
		t.Fatalf("search exceeded budget: %#v", resp)
	}
	if len(resp.Hits) == 0 || resp.Hits[0].File != "frontend/src/views/Documents/TrackingDrawer.vue" {
		t.Fatalf("expected TrackingDrawer as top hit, got %#v", resp.Hits)
	}
	joined := ""
	for _, hit := range resp.Hits {
		if strings.Contains(hit.File, ".test.") {
			t.Fatalf("source-only search leaked test hit: %#v", hit)
		}
		joined += hit.Text + "\n"
	}
	for _, want := range []string{"signature-panel", "previewEnvelope", "fetchRecordSignatures", "previewPdfUrl"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("search evidence missed %q in:\n%s", want, joined)
		}
	}
}

func TestInspectFlowDiscoversAndMergesNaturalLanguageEvidence(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/Utilities/emailTemplateParser.js", `export class EmailTemplateParser {
  parseRadioGroupContent(content, entity, user, depth) {
    const rawChoices = this.splitOptions(content)
    return rawChoices.map((rawChoice, index) => this.parseChoice(rawChoice, entity, user, depth + 1, index))
  }

  splitOptions(optionsString) {
    const options = []
    let braceCount = 0
    return options
  }

  parseChoice(rawChoice, entity, user, depth, choiceIndex) {
    const nestedMatches = this.findRadioGroupMatches(rawChoice)
    const fieldReferences = []
    if (nestedMatches.length > 0) {
      return { hasNestedRadioGroup: true, nestedMatches, value: this.getDisplayValueForNestedChoice(rawChoice) }
    }
    return { hasFieldReference: fieldReferences.length > 0, fieldReferences, value: rawChoice }
  }

  replaceRadioGroupPlaceholders(htmlContent, radioGroups) {
    let processedHtml = htmlContent
    radioGroups.forEach((group) => {
      const selectedChoice = group.choices[group.selected]
      const displayValue = selectedChoice.hasNestedRadioGroup
        ? this.replaceRadioGroupPlaceholders(selectedChoice.processedValue, selectedChoice.nestedRadioGroups || [])
        : selectedChoice.value
      processedHtml = processedHtml.replace(group.tempPlaceholder, displayValue)
    })
    return processedHtml
  }
}
`)
	write(t, root, "src/Utilities/noise.js", `export function placeholderText() {
  return 'generic placeholder input'
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := InspectFlow(idx, "email template placeholders radio group choices nested fields parse display value", InspectFlowOptions{
		Limit:              6,
		BudgetTokens:       1800,
		SearchBudgetTokens: 600,
		ContextLines:       8,
		SourceOnly:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != "ok" {
		t.Fatalf("inspect-flow status=%s warnings=%v search=%#v", resp.Status, resp.Warnings, resp.Search)
	}
	if len(resp.Search.Hits) == 0 || resp.Search.Hits[0].File != "src/Utilities/emailTemplateParser.js" {
		t.Fatalf("expected parser file as top discovery hit, got %#v", resp.Search.Hits)
	}
	joined := ""
	for _, slice := range resp.Context.Slices {
		joined += slice.Text + "\n"
	}
	for _, want := range []string{"parseChoice", "nestedMatches", "fieldReferences", "replaceRadioGroupPlaceholders"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("inspect-flow context missing %q; context=%q", want, joined)
		}
	}
	if len(resp.Context.Slices) > 3 {
		t.Fatalf("expected merged context to avoid many overlapping slices, got %#v", resp.Context.Slices)
	}
}

func TestInspectFlowPrefersGuardOrchestrationOverRepeatedLeafLogs(t *testing.T) {
	root := t.TempDir()
	write(t, root, "utilities/backgroundJobs.js", `const jobConfigs = require('../background_jobs/jobConfigs')
const JobScheduler = require('../background_jobs/JobScheduler')

let jobsStarted = false

function startBackgroundJobs() {
  // Don't run background job if in development environment
  if (process.env.NODE_ENV === 'development') return

  // Guard against double-starting
  if (jobsStarted) {
    logger.info('Background jobs already started - skipping duplicate call')
    return
  }

  jobsStarted = true
  logger.info('Registering cron tasks')
  jobConfigs.forEach((config) => {
    const job = new JobScheduler(config.jobName, config.schedule, config.jobFunction)
    job.start()
  })
}
`)
	write(t, root, "background_jobs/JobScheduler.js", `const cron = require('node-cron')

class JobScheduler {
  start() {
    this.schedule.forEach((cronExpression) => {
      cron.schedule(cronExpression, async () => {
        logger.info('Running the scheduled job')
        await this.jobFunction()
      })
    })
  }
}
`)
	write(t, root, "background_jobs/applicationCharges_job.js", `async function applicationChargesChecker() {
  cronLogger.write('Running the scheduled job to check if there is any new charge.')
}
`)
	write(t, root, "background_jobs/balanceCheckForLoans.js", `async function loansBalanceCheck() {
  cronLogger.write('Running the scheduled job to check loans balance.')
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := InspectFlow(idx, "backend background jobs scheduled cron prevent duplicate running already running", InspectFlowOptions{
		Limit:              6,
		BudgetTokens:       1600,
		SearchBudgetTokens: 600,
		ContextLines:       8,
		SourceOnly:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != "ok" {
		t.Fatalf("inspect-flow status=%s warnings=%v", resp.Status, resp.Warnings)
	}
	if len(resp.Search.Hits) == 0 || resp.Search.Hits[0].File != "utilities/backgroundJobs.js" {
		t.Fatalf("expected orchestration guard as top hit, got %#v", resp.Search.Hits)
	}
	if len(resp.Context.Slices) == 0 || resp.Context.Slices[0].File != "utilities/backgroundJobs.js" {
		t.Fatalf("expected first context slice to preserve top discovery hit, got %#v", resp.Context.Slices)
	}
	joined := ""
	for _, slice := range resp.Context.Slices {
		joined += slice.Text + "\n"
	}
	for _, want := range []string{"Guard against double-starting", "jobsStarted", "new JobScheduler"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("inspect-flow context missing %q; context=%q", want, joined)
		}
	}
}

func TestFetchContextEvidenceModeUsesFocusedLine(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/FacetSidebar.vue", `<template>
  <section>
    <DataOwnerCard
      :icon="getAccessLevelIcon(item.id)"
      :label="getAccessLevelLabel(item)"
    />
  </section>
</template>
<script setup lang="ts">
function getAccessLevelIcon(id: string) { return id }
function getAccessLevelLabel(item: { id: string }) { return item.id }
</script>
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := FetchContext(idx, "src/FacetSidebar.vue:4", FetchContextOptions{
		BudgetTokens: 200,
		ContextLines: 2,
		Mode:         ModeEvidence,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Slices) == 0 {
		t.Fatalf("expected evidence slice, got %#v", resp)
	}
	if resp.Slices[0].StartLine != 4 || !strings.Contains(resp.Slices[0].Text, `:icon="getAccessLevelIcon(item.id)"`) {
		t.Fatalf("expected focused evidence line, got %#v", resp.Slices[0])
	}
}

func TestFetchContextIncludeCallersReturnsCallSiteEvidence(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/FacetSidebar.vue", `<template>
  <DataOwnerCard
    :icon="getAccessLevelIcon(item.id)"
    :label="getAccessLevelLabel(item)"
  />
</template>
<script setup lang="ts">
function getAccessLevelIcon(id: string) { return id }
function getAccessLevelLabel(item: { id: string }) { return item.id }
</script>
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := FetchContext(idx, "getAccessLevelIcon", FetchContextOptions{
		BudgetTokens:   500,
		ContextLines:   1,
		IncludeCallers: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, slice := range resp.Slices {
		if slice.Reason == "caller call site" && strings.Contains(slice.Text, `:icon="getAccessLevelIcon(item.id)"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected caller call-site context, got %#v", resp.Slices)
	}
}

// TestFetchContextReservesTargetBudget ensures imports cannot starve the
// target slice. Even with many imports declared first in a file, the target
// symbol slice must always appear in the response.
func TestFetchContextReservesTargetBudget(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/app.ts", `import { a } from './a'
import { b } from './b'
import { c } from './c'
import { d } from './d'
import { e } from './e'
import { f } from './f'
import { g } from './g'
import { h } from './h'
import { i } from './i'
import { j } from './j'

export function target() {
  return a + b + c + d + e + f + g + h + i + j
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := FetchContext(idx, "target", FetchContextOptions{BudgetTokens: 200})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != "ok" {
		t.Fatalf("expected ok status, got %s (%v)", resp.Status, resp.Warnings)
	}
	foundTarget := false
	for _, s := range resp.Slices {
		if s.Reason == "target symbol" {
			foundTarget = true
		}
	}
	if !foundTarget {
		t.Fatalf("target symbol slice must be present even when many imports compete for the budget; slices=%#v", resp.Slices)
	}
}

func TestFetchContextFileLineCentersInsideLargeVueComponent(t *testing.T) {
	root := t.TempDir()
	var filler strings.Builder
	for i := 0; i < 160; i++ {
		filler.WriteString("      <div class=\"filler\">row</div>\n")
	}
	write(t, root, "src/TrackingDrawer.vue", `<template>
  <section>
`+filler.String()+`    <el-dialog class="envelope-preview-dialog">
      <div class="preview-shell">
        <object class="preview-frame"></object>
      </div>
    </el-dialog>
  </section>
</template>
<script setup>
const openTrackingDrawer = () => {}
</script>
<style>
.preview-shell {
  min-height: 520px;
}
</style>
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := FetchContext(idx, "src/TrackingDrawer.vue:164", FetchContextOptions{
		BudgetTokens: 400,
		ContextLines: 4,
		Mode:         ModeContext,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != "ok" || len(resp.Slices) == 0 {
		t.Fatalf("FetchContext status=%s slices=%d warnings=%v", resp.Status, len(resp.Slices), resp.Warnings)
	}
	primary := resp.Slices[0]
	if primary.StartLine > 164 || primary.EndLine < 164 {
		t.Fatalf("primary slice should contain requested line 164, got %d:%d", primary.StartLine, primary.EndLine)
	}
	if primary.StartLine == 1 {
		t.Fatalf("file:line query inside large component should not start at component top")
	}
	if !strings.Contains(primary.Text, "preview-shell") || !strings.Contains(primary.Text, "preview-frame") {
		t.Fatalf("primary slice missed preview markup: %q", primary.Text)
	}
}

func TestVueCSSClassSymbolsAndTraceEdges(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/TrackingDrawer.vue", `<template>
  <el-dialog class="envelope-preview-dialog">
    <template #extra>
      <button class="retry-button">Retry</button>
    </template>
    <div class="preview-shell">
      <object class="preview-frame"></object>
    </div>
  </el-dialog>
</template>
<script setup>
const previewPdfUrl = ref('')
</script>
<style scoped>
.preview-shell {
  height: min(78vh, 880px);
  min-height: 520px;
}
.retry-button {
  color: red;
}
.preview-frame {
  width: 100%;
  height: 100%;
}
</style>
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	css := ListSymbols(idx, "preview-shell", "css-class", "")
	if len(css.Symbols) != 1 {
		t.Fatalf("expected one css-class preview-shell, got %#v", css.Symbols)
	}
	usage := ListSymbols(idx, "preview-shell", "template-class", "")
	if len(usage.Symbols) != 1 {
		t.Fatalf("expected one template-class preview-shell, got %#v", usage.Symbols)
	}
	trace := TraceSymbol(idx, css.Symbols[0].ID)
	if trace.Status != "found" {
		t.Fatalf("trace status=%s", trace.Status)
	}
	if len(trace.Callers) != 1 || trace.Callers[0].Kind != "template-class" || trace.Callers[0].Name != "preview-shell" {
		t.Fatalf("css class should be traced from template usage, got %#v", trace.Callers)
	}
	if len(trace.CallerSites) != 1 || trace.CallerSites[0].Raw != "preview-shell" {
		t.Fatalf("expected compact class usage site, got %#v", trace.CallerSites)
	}
	ctx, err := FetchContext(idx, css.Symbols[0].ID, FetchContextOptions{BudgetTokens: 300, ContextLines: 3, Mode: ModeContext})
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Status != "ok" || len(ctx.Slices) == 0 || !strings.Contains(ctx.Slices[0].Text, "min-height: 520px") {
		t.Fatalf("expected CSS rule context, got status=%s slices=%#v", ctx.Status, ctx.Slices)
	}
}

// TestStableSymbolIDsSurviveLineMoves ensures editing a file in a way that
// shifts line numbers does NOT change a symbol's stable ID.
func TestStableSymbolIDsSurviveLineMoves(t *testing.T) {
	root1 := t.TempDir()
	write(t, root1, "src/x.ts", `export function foo() { return 1 }
export function bar() { return 2 }
`)
	idx1, err := BuildIndex(root1)
	if err != nil {
		t.Fatal(err)
	}

	root2 := t.TempDir()
	write(t, root2, "src/x.ts", `// added comment line
// another added comment
export function foo() { return 1 }
export function bar() { return 2 }
`)
	idx2, err := BuildIndex(root2)
	if err != nil {
		t.Fatal(err)
	}
	a := ListSymbols(idx1, "foo", "function", "").Symbols
	b := ListSymbols(idx2, "foo", "function", "").Symbols
	if len(a) == 0 || len(b) == 0 {
		t.Fatalf("foo missing in one of the indexes: %v / %v", a, b)
	}
	if a[0].ID != b[0].ID {
		t.Fatalf("symbol id changed across line shift: %s vs %s", a[0].ID, b[0].ID)
	}
	if a[0].StartLine == b[0].StartLine {
		t.Fatalf("test setup invalid: line numbers did not shift")
	}
}

// TestParallelBuildIsDeterministic indexes the same tree twice and verifies
// the term/reference/edge counts and IDs are identical, which catches data
// races in the parallel scanner without needing -race.
func TestParallelBuildIsDeterministic(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 8; i++ {
		write(t, root, "src/file_a.ts", `export function alpha() { return beta() }
function beta() { return gamma() }
function gamma() { return 1 }
`)
		write(t, root, "src/file_b.ts", `import { alpha } from './file_a'
export function delta() { return alpha() }
`)
		write(t, root, "shapes/main.ttl", `@prefix ex: <http://example.org/> .
@prefix sh: <http://www.w3.org/ns/shacl#> .

ex:Shape a sh:NodeShape ; sh:path ex:name .
`)
	}

	idx1, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	idx2, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx1.Symbols) != len(idx2.Symbols) {
		t.Fatalf("symbol count differs across builds: %d vs %d", len(idx1.Symbols), len(idx2.Symbols))
	}
	if len(idx1.SymbolEdges) != len(idx2.SymbolEdges) {
		t.Fatalf("symbol edge count differs: %d vs %d", len(idx1.SymbolEdges), len(idx2.SymbolEdges))
	}
	if len(idx1.References) != len(idx2.References) {
		t.Fatalf("reference count differs: %d vs %d", len(idx1.References), len(idx2.References))
	}
}

// TestWatchRebakesOnEdit verifies that the watcher detects an edit and
// rebakes the affected file, replacing prior symbols with the new ones.
func TestWatchRebakesOnEdit(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/x.ts", `export function original() { return 1 }
`)
	write(t, root, "src/entry.ts", `import { original } from "./x"
export function entry() { return original() }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := ListSymbols(idx, "original", "function", "").Symbols; len(got) != 1 {
		t.Fatalf("baseline expected `original`, got %#v", got)
	}
	if resp := RepoMap(idx, RepoMapOptions{BudgetTokens: 600, Limit: 10}); resp.Status != "ok" {
		t.Fatalf("baseline repo map was not cacheable: %#v", resp)
	}
	idx.repoMapResultsMu.Lock()
	cachedBeforeEdit := len(idx.repoMapResults)
	idx.repoMapResultsMu.Unlock()
	if cachedBeforeEdit != 1 {
		t.Fatalf("baseline repo map cache entries=%d, want 1", cachedBeforeEdit)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var done sync.WaitGroup
	done.Add(1)

	ready := make(chan struct{})
	rebakes := make(chan struct{}, 4)
	go func() {
		defer done.Done()
		_ = Watch(ctx, idx, WatchOptions{
			Debounce: 50 * time.Millisecond,
			OnReady: func() {
				close(ready)
			},
			OnRebake: func(updated, removed []string) {
				select {
				case rebakes <- struct{}{}:
				default:
				}
			},
		})
	}()

	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		cancel()
		done.Wait()
		t.Fatal("watcher did not report readiness")
	}
	// Write immediately after readiness. This specifically guards the server
	// startup race where an edit could land after MCP initialization but
	// before fsnotify had registered the repository directories.
	write(t, root, "src/x.ts", `export function renamed() { return 2 }
`)
	select {
	case <-rebakes:
	case <-time.After(3 * time.Second):
		cancel()
		done.Wait()
		t.Fatal("watcher did not rebake within 3s")
	}

	if got := ListSymbols(idx, "renamed", "function", "").Symbols; len(got) != 1 {
		t.Fatalf("expected `renamed` after rebake, got %#v", got)
	}
	if got := ListSymbols(idx, "original", "function", "").Symbols; len(got) != 0 {
		t.Fatalf("`original` should have been dropped on rebake, got %#v", got)
	}
	idx.repoMapResultsMu.Lock()
	cachedAfterEdit := len(idx.repoMapResults)
	idx.repoMapResultsMu.Unlock()
	if cachedAfterEdit != 0 {
		t.Fatalf("watch rebake left %d stale repo map cache entries", cachedAfterEdit)
	}

	cancel()
	done.Wait()
}

func TestWatchReconciliationQueuesMissedChanges(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/edit.ts", "export const before = 1\n")
	write(t, root, "src/remove.ts", "export const removed = 1\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	write(t, root, "src/edit.ts", "export const after = 2\n")
	write(t, root, "generated/new.ts", "export const added = 3\n")
	if err := os.Remove(filepath.Join(root, "src/remove.ts")); err != nil {
		t.Fatal(err)
	}

	pending := newPendingSet()
	queued, err := queueIndexReconciliation(idx, root, pending)
	if err != nil {
		t.Fatal(err)
	}
	if !queued {
		t.Fatal("expected reconciliation to queue missed changes")
	}
	changes := pending.drain()
	if changes["src/edit.ts"] != eventUpdate {
		t.Fatalf("edited file not queued for update: %#v", changes)
	}
	if changes["generated/new.ts"] != eventUpdate {
		t.Fatalf("new file not queued for update: %#v", changes)
	}
	if changes["src/remove.ts"] != eventRemove {
		t.Fatalf("removed file not queued for removal: %#v", changes)
	}
}

func TestWatchRebakesOnJavaEdit(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/Foo.java", `package demo;

public class Foo {
    public void original() {}
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := ListSymbols(idx, "original", "method", "").Symbols; len(got) != 1 {
		t.Fatalf("baseline expected `original`, got %#v", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var done sync.WaitGroup
	done.Add(1)

	rebakes := make(chan struct{}, 4)
	go func() {
		defer done.Done()
		_ = Watch(ctx, idx, WatchOptions{
			Debounce: 50 * time.Millisecond,
			OnRebake: func(updated, removed []string) {
				select {
				case rebakes <- struct{}{}:
				default:
				}
			},
		})
	}()

	// Give the watcher a moment to register dirs.
	time.Sleep(150 * time.Millisecond)
	write(t, root, "src/Foo.java", `package demo;

public class Foo {
    public void renamed() {}
}
`)
	select {
	case <-rebakes:
	case <-time.After(3 * time.Second):
		cancel()
		done.Wait()
		t.Fatal("watcher did not rebake .java change within 3s")
	}

	if got := ListSymbols(idx, "renamed", "method", "").Symbols; len(got) != 1 {
		t.Fatalf("expected `renamed` after rebake, got %#v", got)
	}
	if got := ListSymbols(idx, "original", "method", "").Symbols; len(got) != 0 {
		t.Fatalf("`original` should have been dropped on rebake, got %#v", got)
	}

	cancel()
	done.Wait()
}

func TestIncrementalRebakeRefreshesImportDependents(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/a.ts", `export function foo() { return 1 }
`)
	write(t, root, "src/b.ts", `import { foo } from './a'

export function useFoo() {
  return foo()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	trace := TraceSymbolWithOptions(idx, "useFoo", TraceSymbolOptions{WithEdges: true})
	if trace.Status != "found" {
		t.Fatalf("expected useFoo trace, got %#v", trace)
	}
	if !hasCallEdgeToSuffix(trace.Edges, ":foo") {
		t.Fatalf("baseline expected useFoo to call foo, got %#v", trace.Edges)
	}

	write(t, root, "src/a.ts", `export function bar() { return 1 }
`)
	updated, removed, err := rebakeChangedFiles(idx, root, []string{"src/a.ts"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("expected no removed files, got %#v", removed)
	}
	if !containsString(updated, "src/a.ts") || !containsString(updated, "src/b.ts") {
		t.Fatalf("expected import dependent src/b.ts to rebake with src/a.ts, got updated=%#v", updated)
	}

	trace = TraceSymbolWithOptions(idx, "useFoo", TraceSymbolOptions{WithEdges: true})
	if trace.Status != "found" {
		t.Fatalf("expected useFoo trace after rebake, got %#v", trace)
	}
	if hasCallEdgeToSuffix(trace.Edges, ":foo") {
		t.Fatalf("stale resolved foo edge survived after foo was removed: %#v", trace.Edges)
	}
	if !hasCallEdge(trace.Edges, "unresolved:foo") {
		t.Fatalf("expected refreshed useFoo to point at unresolved:foo, got %#v", trace.Edges)
	}
}

// TestWatchRebakeDoesNotInvalidateWholeCodeSearchIndex covers the gap found
// while profiling long-running MCP sessions:
// `mamari serve --watch` rebaking a single edited file used to call
// invalidateCodeSearchIndex(), which discarded *every* file's tokenized
// search-cache entry — not just the edited file's — forcing the next
// search-code/inspect-flow/repo_map call to re-read and re-tokenize the
// entire repo from disk. In a long session with repeated edit-then-query
// cycles, that meant paying the full rebuild cost on every edit, not once
// per session. The fix (updateCodeSearchIndexForFiles) must refresh only the
// changed file's entry and leave the rest of the cache — and the "is it
// built" flag — untouched.
func TestWatchRebakeDoesNotInvalidateWholeCodeSearchIndex(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/a.ts", `export function alpha() { return 1 }
`)
	write(t, root, "src/b.ts", `export function bravo() { return 2 }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	// Force the search cache to build.
	SearchCode(idx, "alpha", SearchCodeOptions{Limit: 6, BudgetTokens: 1000})
	idx.mu.Lock()
	if !idx.codeSearchBuilt {
		idx.mu.Unlock()
		t.Fatalf("expected codeSearchBuilt=true after a SearchCode call")
	}
	bBefore, ok := findCodeSearchFile(idx.codeSearchFiles, "src/b.ts")
	idx.mu.Unlock()
	if !ok {
		t.Fatalf("expected src/b.ts to have a cached search entry before the edit")
	}
	if len(bBefore.lines) == 0 || !strings.Contains(bBefore.lineText(bBefore.lines[0]), "bravo") {
		t.Fatalf("expected src/b.ts cached entry to contain bravo, got %#v", bBefore.lines)
	}

	write(t, root, "src/a.ts", `export function alphaRenamed() { return 1 }
`)
	if _, _, err := rebakeChangedFiles(idx, root, []string{"src/a.ts"}, nil); err != nil {
		t.Fatal(err)
	}

	idx.mu.Lock()
	stillBuilt := idx.codeSearchBuilt
	bAfter, bOk := findCodeSearchFile(idx.codeSearchFiles, "src/b.ts")
	aAfter, aOk := findCodeSearchFile(idx.codeSearchFiles, "src/a.ts")
	idx.mu.Unlock()

	if !stillBuilt {
		t.Fatalf("expected codeSearchBuilt to remain true after rebaking an unrelated file — the whole cache should not be invalidated by a single-file edit")
	}
	if !bOk {
		t.Fatalf("expected src/b.ts cached entry to survive the rebake of src/a.ts")
	}
	if len(bAfter.lines) == 0 || !strings.Contains(bAfter.lineText(bAfter.lines[0]), "bravo") {
		t.Fatalf("expected src/b.ts cached entry to be untouched, got %#v", bAfter.lines)
	}
	if !aOk {
		t.Fatalf("expected src/a.ts cached entry to be refreshed after its own edit")
	}
	if len(aAfter.lines) == 0 || !strings.Contains(aAfter.lineText(aAfter.lines[0]), "alphaRenamed") {
		t.Fatalf("expected src/a.ts cached entry to reflect the new content, got %#v", aAfter.lines)
	}

	// And the search results themselves must reflect the edit, not a stale
	// pre-edit snapshot.
	resp := SearchCode(idx, "alphaRenamed", SearchCodeOptions{Limit: 6, BudgetTokens: 1000})
	found := false
	for _, hit := range resp.Hits {
		if hit.File == "src/a.ts" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected search for alphaRenamed to hit src/a.ts after incremental rebake, got %#v", resp.Hits)
	}
}

func TestWatchRebakeSkipsDependentsWhenJSSurfaceUnchanged(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/a.ts", `export function alpha() { return 1 }
`)
	write(t, root, "src/b.ts", `import { alpha } from './a'
export function bravo() { return alpha() }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	alphaID := findSymbolID(t, idx, "src/a.ts", "alpha")
	if !hasCGPEdge(idx, "src/b.ts", alphaID, "calls") {
		t.Fatalf("expected initial b.ts -> alpha call edge")
	}

	write(t, root, "src/a.ts", `export function alpha() { return 1 }
// comment-only edit
`)
	updated, _, err := rebakeChangedFiles(idx, root, []string{"src/a.ts"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if containsString(updated, "src/b.ts") {
		t.Fatalf("expected unchanged JS surface to skip dependent rebake, updated=%v", updated)
	}
	if !hasCGPEdge(idx, "src/b.ts", alphaID, "calls") {
		t.Fatalf("expected preserved external b.ts -> alpha edge after comment-only edit")
	}

	write(t, root, "src/a.ts", `export function alphaRenamed() { return 1 }
`)
	updated, _, err = rebakeChangedFiles(idx, root, []string{"src/a.ts"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(updated, "src/b.ts") {
		t.Fatalf("expected exported symbol surface change to rebake dependent b.ts, updated=%v", updated)
	}
}

// TestWatchKeepsSearchIndexLazyUntilRequested guards the idle-resource
// contract. Starting a watcher and receiving filesystem events must not
// tokenize the whole repository before a search request actually needs it.
func TestWatchKeepsSearchIndexLazyUntilRequested(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/a.ts", `export function alpha() { return 1 }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if idx.published.Load() != nil {
		t.Fatalf("expected no published snapshot before Watch() starts")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	rebakes := make(chan struct{}, 4)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = Watch(ctx, idx, WatchOptions{
			Debounce: 50 * time.Millisecond,
			OnReady:  func() { close(ready) },
			OnRebake: func(updated, removed []string) {
				select {
				case rebakes <- struct{}{}:
				default:
				}
			},
		})
	}()
	defer wg.Wait()
	defer cancel()
	<-ready

	idx.mu.Lock()
	builtAtStart := idx.codeSearchBuilt
	idx.mu.Unlock()
	if builtAtStart || idx.published.Load() != nil {
		t.Fatal("starting Watch built or published the search index without a query")
	}

	write(t, root, "src/a.ts", `export function alphaRenamed() { return 2 }
`)
	select {
	case <-rebakes:
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not rebake within 3s")
	}
	time.Sleep(50 * time.Millisecond)
	idx.mu.Lock()
	builtAfterEdit := idx.codeSearchBuilt
	idx.mu.Unlock()
	if builtAfterEdit || idx.published.Load() != nil {
		t.Fatal("idle watch rebake built or published the search index without a query")
	}

	resp := SearchCode(idx, "alphaRenamed", SearchCodeOptions{Limit: 6, BudgetTokens: 1000})
	found := false
	for _, hit := range resp.Hits {
		if hit.File == "src/a.ts" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected lazy search build to reflect the edit, got %#v", resp.Hits)
	}
	idx.mu.Lock()
	builtAfterQuery := idx.codeSearchBuilt
	idx.mu.Unlock()
	if !builtAfterQuery {
		t.Fatal("search request did not build the search index")
	}
}

// TestPublishQuerySnapshotRefreshesAfterRebake verifies the snapshot the
// watcher publishes after a rebake is a genuinely new object (not the same
// one mutated in place — see publishedQueryIndex's "replace, never mutate"
// contract) and reflects the edited content.
func TestPublishQuerySnapshotRefreshesAfterRebake(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/a.ts", `export function alpha() { return 1 }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if resp := SearchCode(idx, "alpha", SearchCodeOptions{Limit: 6, BudgetTokens: 1000}); len(resp.Hits) == 0 {
		t.Fatalf("expected initial search to build the lazy cache, got %#v", resp)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	rebakes := make(chan struct{}, 4)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = Watch(ctx, idx, WatchOptions{
			Debounce: 50 * time.Millisecond,
			OnReady:  func() { close(ready) },
			OnRebake: func(updated, removed []string) {
				select {
				case rebakes <- struct{}{}:
				default:
				}
			},
		})
	}()
	defer wg.Wait()
	defer cancel()
	<-ready
	before := idx.published.Load()

	write(t, root, "src/a.ts", `export function alphaRenamed() { return 2 }
`)
	select {
	case <-rebakes:
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not rebake within 3s")
	}

	deadline := time.Now().Add(2 * time.Second)
	for idx.published.Load() == before {
		if time.Now().After(deadline) {
			t.Fatalf("expected a freshly published snapshot after the rebake")
		}
		time.Sleep(5 * time.Millisecond)
	}

	resp := SearchCode(idx, "alphaRenamed", SearchCodeOptions{Limit: 6, BudgetTokens: 1000})
	found := false
	for _, hit := range resp.Hits {
		if hit.File == "src/a.ts" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected published snapshot to reflect the edit, got %#v", resp.Hits)
	}
}

func TestWatchEffectiveNamespacesUsesCachedUnchangedFiles(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/ns.ts", `export const NS = "https://example.com/ns/"
`)
	userContent := "import { NS } from './ns'\nconst iri = `${NS}Thing`\n"
	write(t, root, "src/user.ts", userContent)

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "src/ns.ts")); err != nil {
		t.Fatal(err)
	}

	effective := watchEffectiveNamespacesFor(idx, root, map[string]string{
		"src/user.ts": "import { NS } from './ns'\nconst iri = `${NS}Other`\n",
	})
	entry, ok := effective["src/user.ts"]["NS"]
	if !ok {
		t.Fatalf("expected unchanged imported namespace to come from cache, got %#v", effective["src/user.ts"])
	}
	if entry.IRI != "https://example.com/ns/" {
		t.Fatalf("unexpected namespace IRI: %q", entry.IRI)
	}
}

func TestSearchCodePublishedSnapshotDoesNotTakeIndexMutex(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/a.ts", `export function alpha() { return 1 }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	idx.publishQuerySnapshot(nil)
	if idx.published.Load() == nil {
		t.Fatalf("expected published snapshot")
	}

	idx.mu.Lock()
	locked := true
	defer func() {
		if locked {
			idx.mu.Unlock()
		}
	}()

	done := make(chan SearchCodeResponse, 1)
	go func() {
		done <- SearchCode(idx, "alpha", SearchCodeOptions{Limit: 6, BudgetTokens: 1000})
	}()

	select {
	case resp := <-done:
		idx.mu.Unlock()
		locked = false
		found := false
		for _, hit := range resp.Hits {
			if hit.File == "src/a.ts" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected lock-free search to find alpha, got %#v", resp.Hits)
		}
	case <-time.After(300 * time.Millisecond):
		idx.mu.Unlock()
		locked = false
		t.Fatal("SearchCode blocked on idx.mu despite a published query snapshot")
	}
}

func TestSearchCodeResultCacheSurvivesIrrelevantEdit(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/a.ts", "export function alphaTarget() { return 1 }\n")
	write(t, root, "src/b.ts", "export function betaHelper() { return 2 }\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	idx.publishQuerySnapshot(nil)

	opts := SearchCodeOptions{Limit: 6, BudgetTokens: 1000}
	first := SearchCode(idx, "alphaTarget", opts)
	if len(first.Hits) == 0 {
		t.Fatalf("expected initial hit, got %#v", first)
	}
	first.Hits[0].Text = "caller mutation must not poison cache"
	if len(first.Hits[0].Symbols) > 0 {
		first.Hits[0].Symbols[0].ReturnTypes = []string{"poisoned"}
	}

	idx.searchResultsMu.Lock()
	if len(idx.searchResults) != 1 {
		idx.searchResultsMu.Unlock()
		t.Fatalf("expected one cached result, got %d", len(idx.searchResults))
	}
	oldGeneration := idx.published.Load().generation
	idx.searchResultsMu.Unlock()

	write(t, root, "src/b.ts", "export function betaHelper() { return 3 }\n// unrelated marker\n")
	updated, removed, err := rebakeChangedFiles(idx, root, []string{"src/b.ts"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	idx.publishQuerySnapshot(append(updated, removed...))

	idx.searchResultsMu.Lock()
	if len(idx.searchResults) != 1 {
		idx.searchResultsMu.Unlock()
		t.Fatalf("irrelevant edit evicted cached result")
	}
	for _, entry := range idx.searchResults {
		if entry.generation <= oldGeneration {
			idx.searchResultsMu.Unlock()
			t.Fatalf("cached result was not advanced to the new generation")
		}
	}
	idx.searchResultsMu.Unlock()

	second := SearchCode(idx, "alphaTarget", opts)
	if len(second.Hits) == 0 || strings.Contains(second.Hits[0].Text, "caller mutation") {
		t.Fatalf("cache response was not deeply isolated: %#v", second.Hits)
	}
}

func TestSearchCodeResultCacheInvalidatesRelevantEdit(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/a.ts", "export function alphaTarget() { return 1 }\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	idx.publishQuerySnapshot(nil)
	opts := SearchCodeOptions{Limit: 6, BudgetTokens: 1000}
	if resp := SearchCode(idx, "alphaTarget", opts); len(resp.Hits) == 0 {
		t.Fatalf("expected initial hit, got %#v", resp)
	}

	write(t, root, "src/a.ts", "export function betaTarget() { return 2 }\n")
	updated, removed, err := rebakeChangedFiles(idx, root, []string{"src/a.ts"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	idx.publishQuerySnapshot(append(updated, removed...))

	idx.searchResultsMu.Lock()
	cached := len(idx.searchResults)
	idx.searchResultsMu.Unlock()
	if cached != 0 {
		t.Fatalf("relevant edit left %d stale cached result(s)", cached)
	}
	resp := SearchCode(idx, "alphaTarget", opts)
	for _, hit := range resp.Hits {
		if strings.Contains(hit.Text, "alphaTarget") {
			t.Fatalf("stale source survived relevant edit: %#v", resp.Hits)
		}
	}
}

// TestSearchCodeConcurrentWithWatchRebakesNoRace stresses the actual
// scenario this feature targets: a long-running watch session where an
// agent is editing files and calling search_code throughout. Run under
// `go test -race`. The property under test isn't just "no data race" (the
// race detector covers that) — it's that SearchCode never observes a
// torn/partially-built snapshot, since publishedQueryIndex's fields are
// only ever replaced wholesale, never mutated in place.
func TestSearchCodeConcurrentWithWatchRebakesNoRace(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		write(t, root, fmt.Sprintf("src/f%d.ts", i), fmt.Sprintf("export function gen%d() { return %d }\n", i, i))
	}
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = Watch(ctx, idx, WatchOptions{Debounce: 30 * time.Millisecond})
	}()

	var readers sync.WaitGroup
	stop := make(chan struct{})
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				resp := SearchCode(idx, "gen", SearchCodeOptions{Limit: 6, BudgetTokens: 1000})
				if resp.Status == "invalid" {
					t.Errorf("unexpected invalid status: %#v", resp)
					return
				}
			}
		}()
	}

	deadlineEnd := time.Now().Add(1500 * time.Millisecond)
	i := 0
	for time.Now().Before(deadlineEnd) {
		f := fmt.Sprintf("src/f%d.ts", i%5)
		write(t, root, f, fmt.Sprintf("export function gen%d() { return %d }\n", i%5, i))
		i++
		time.Sleep(20 * time.Millisecond)
	}

	close(stop)
	readers.Wait()
	cancel()
	wg.Wait()
}

func findCodeSearchFile(files []codeSearchFile, path string) (codeSearchFile, bool) {
	for _, f := range files {
		if f.file == path {
			return f, true
		}
	}
	return codeSearchFile{}, false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func findSymbolID(t *testing.T, idx *Index, file, name string) string {
	t.Helper()
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for id, sym := range idx.Symbols {
		if sym.File == file && sym.Name == name {
			return id
		}
	}
	t.Fatalf("symbol %s in %s not found", name, file)
	return ""
}

func hasCGPEdge(idx *Index, evidenceFile, to, edgeType string) bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for _, edge := range idx.SymbolEdges {
		if edge.Evidence.File == evidenceFile && edge.To == to && edge.Type == edgeType {
			return true
		}
	}
	return false
}

func containsExactPhrase(values []SearchCodeExactPhrase, literal, kind string) bool {
	for _, value := range values {
		if value.Literal == literal && value.Kind == kind {
			return true
		}
	}
	return false
}

func clusterHasLineText(cluster ExactEvidenceCluster, want string) bool {
	for _, line := range cluster.Lines {
		if strings.Contains(line.Text, want) {
			return true
		}
	}
	return false
}

func summaryContains(values []CGPSymbolSummary, want string) bool {
	for _, value := range values {
		if value.Name == want {
			return true
		}
	}
	return false
}

func callerNamed(callers []CGPSymbolSummary, want string) bool {
	for _, caller := range callers {
		if caller.Name == want {
			return true
		}
	}
	return false
}

func hasCallEdge(edges []CGPEdge, wantTo string) bool {
	for _, edge := range edges {
		if edge.Type == "calls" && edge.To == wantTo {
			return true
		}
	}
	return false
}

func hasCallEdgeToSuffix(edges []CGPEdge, wantSuffix string) bool {
	for _, edge := range edges {
		if edge.Type == "calls" && strings.HasPrefix(edge.To, "symbol:") && strings.HasSuffix(edge.To, wantSuffix) {
			return true
		}
	}
	return false
}

// TestUncertaintyTaxonomyClassifiesEdgesAndSymbols verifies that:
//   - Same-file unique-name calls produce ConfExact edges with no reason.
//   - Cross-file unique-name calls produce ConfScoped edges.
//   - Bare names with no candidate produce ConfUnresolved + ReasonMissingImport.
//   - Dotted callees with no candidate produce ConfUnresolved + ReasonDynamicReceiver.
//   - JS/TS structural symbols are ConfExact (vs the old blanket "heuristic").
//   - Files carry parser metadata after a clean build.
func TestUncertaintyTaxonomyClassifiesEdgesAndSymbols(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/util.ts", `export function helper() { return 1 }
`)
	write(t, root, "src/app.ts", `import { helper } from './util'
export function localOne() { return localTwo() }
function localTwo() { return helper() + missingFn() + obj.method() }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	edges := map[string]CGPEdge{}
	for _, e := range idx.SymbolEdges {
		if e.Type != "calls" {
			continue
		}
		edges[e.Evidence.Raw+"@"+e.Evidence.File] = e
	}
	checkEdge := func(rawAtFile, wantConf, wantReason string) {
		t.Helper()
		e, ok := edges[rawAtFile]
		if !ok {
			t.Fatalf("missing call edge for %s; have %v", rawAtFile, edges)
		}
		if e.Confidence != wantConf {
			t.Errorf("%s confidence = %q, want %q", rawAtFile, e.Confidence, wantConf)
		}
		if e.UnresolvedReason != wantReason {
			t.Errorf("%s unresolvedReason = %q, want %q", rawAtFile, e.UnresolvedReason, wantReason)
		}
	}
	checkEdge("localTwo@src/app.ts", ConfExact, "") // same-file unique
	checkEdge("helper@src/app.ts", ConfScoped, "")  // cross-file unique
	checkEdge("missingFn@src/app.ts", ConfUnresolved, ReasonMissingImport)
	checkEdge("obj.method@src/app.ts", ConfUnresolved, ReasonDynamicReceiver)

	for _, sym := range idx.Symbols {
		if sym.File == "src/app.ts" && (sym.Name == "localOne" || sym.Name == "localTwo") {
			if sym.Confidence != ConfExact {
				t.Errorf("symbol %s confidence = %q, want exact", sym.Name, sym.Confidence)
			}
		}
	}

	for _, f := range idx.Files {
		if f.Language != "typescript" {
			continue
		}
		if f.Parser != "jsparse-token" {
			t.Errorf("file %s parser = %q, want jsparse-token", f.Path, f.Parser)
		}
		if f.ParseStatus != ParseStatusOK {
			t.Errorf("file %s parseStatus = %q, want ok", f.Path, f.ParseStatus)
		}
	}
}

// TestParseDiagnosticsFlagUnbalancedBraces ensures the JS parser emits an
// "unbalanced_braces" diagnostic and the file is marked partial when input
// has mismatched braces — this is the signal doctor uses to surface
// suspect parse coverage.
func TestParseDiagnosticsFlagUnbalancedBraces(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/broken.ts", `export function ok() { return 1 }
function broken() { if (true) { return 2 }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := idx.Files["src/broken.ts"]
	if !ok {
		t.Fatal("missing broken.ts entry")
	}
	if f.ParseStatus != ParseStatusPartial {
		t.Fatalf("parseStatus = %q, want partial", f.ParseStatus)
	}
	if !strings.Contains(f.ParseError, "unbalanced_braces") {
		t.Fatalf("parseError = %q, want unbalanced_braces", f.ParseError)
	}
}

// TestBenchmarkCGPScoresSymbolsAgainstGold drives the harness end-to-end
// against a tiny synthetic repo + fixture. We cover the three score paths:
//   - all expected callers found → ok / appRecall=1
//   - one expected caller missing → ok status but reduced appRecall
//   - mustNotFind violation → recorded as a violation
func TestBenchmarkCGPScoresSymbolsAgainstGold(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/lib.ts", `export function helper() { return 1 }
export function used() { return helper() }
export function leak() { return used() }
`)
	write(t, root, "src/app.ts", `import { used } from './lib'
export function entrypoint() { return used() }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	goldPath := filepath.Join(root, "gold.json")
	// appCallers expects two lines; one (line 99) is intentionally bogus to
	// drive the missing-recall path. mustNotFind names line 3 of lib.ts,
	// which is a real call site for `used` — the harness should catch it
	// as a violation.
	gold := `{
  "symbols": {
    "used": {
      "appCallers": ["src/app.ts:2", "src/lib.ts:99"],
      "testCallers": [],
      "mustNotFind": ["src/lib.ts:3"]
    }
  }
}`
	if err := os.WriteFile(goldPath, []byte(gold), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := BenchmarkCGP(idx, goldPath)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := report.Results["used"]
	if !ok {
		t.Fatalf("missing result for `used`: %+v", report)
	}
	if stat.Status != "ok" {
		t.Fatalf("status = %s, want ok", stat.Status)
	}
	if stat.AppFound != 1 {
		t.Errorf("AppFound = %d, want 1", stat.AppFound)
	}
	if stat.AppExpected != 2 {
		t.Errorf("AppExpected = %d, want 2", stat.AppExpected)
	}
	if len(stat.AppMissing) != 1 || stat.AppMissing[0] != "src/lib.ts:99" {
		t.Errorf("AppMissing = %v, want [src/lib.ts:99]", stat.AppMissing)
	}
	if len(stat.Violations) != 1 || stat.Violations[0] != "src/lib.ts:3" {
		t.Errorf("Violations = %v, want [src/lib.ts:3]", stat.Violations)
	}
	if report.Summary.OverallStatus != "fail" {
		t.Errorf("overall status = %s, want fail (appRecall<1, violations>0)", report.Summary.OverallStatus)
	}
}

// TestVitestStyleCallbacksAttributeNestedCalls verifies that calls inside
// `it(name, () => {...})` callbacks are attributed to the callback symbol
// rather than the file. Without callback symbols, `containingSymbolFast`
// returns the file symbol and the call edge is dropped — that was the
// "anonymous test callback" gap called out in the benchmark report.
func TestVitestStyleCallbacksAttributeNestedCalls(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/lib.ts", `export function useDataset() { return { data: 1 } }
`)
	write(t, root, "tests/useDataset.test.ts", `import { useDataset } from '../src/lib'

describe('useDataset', () => {
  it('returns the dataset', () => {
    useDataset()
  })

  it('does work twice', () => {
    useDataset()
    useDataset()
  })

  beforeEach(() => {
    useDataset()
  })
})
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	// Callback symbols exist with descriptive names.
	got := ListSymbols(idx, "", "callback", "")
	gotNames := map[string]bool{}
	for _, s := range got.Symbols {
		gotNames[s.Name] = true
	}
	for _, want := range []string{"describe: useDataset", "it: returns the dataset", "it: does work twice", "beforeEach"} {
		if !gotNames[want] {
			t.Errorf("expected callback symbol %q, got %v", want, gotNames)
		}
	}

	// Calls to useDataset from inside callbacks land as caller edges of useDataset.
	trace := TraceSymbol(idx, "useDataset")
	if trace.Status != "found" {
		t.Fatalf("trace status = %s", trace.Status)
	}
	groupedTests := false
	for _, c := range trace.Callers {
		if c.Kind == "test-callback-group" && c.File == "tests/useDataset.test.ts" && c.Count == 4 {
			groupedTests = true
		}
	}
	if !groupedTests {
		t.Errorf("useDataset should group test callback callers, got %#v", trace.Callers)
	}

	detailed := TraceSymbolWithOptions(idx, "useDataset", TraceSymbolOptions{Sites: true, IncludeTestDetails: true})
	callerNames := map[string]bool{}
	for _, c := range detailed.Callers {
		callerNames[c.Name] = true
	}
	for _, want := range []string{"it: returns the dataset", "it: does work twice", "beforeEach"} {
		if !callerNames[want] {
			t.Errorf("useDataset should expose detailed caller %q with include-test-details, got %v", want, callerNames)
		}
	}
}

func TestTraceSymbolCompactDefaultAndEdgeOptIn(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/lib.ts", `export function useDataset() { return { data: 1 } }
export function appCaller() {
  return useDataset()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	trace := TraceSymbol(idx, "useDataset")
	if len(trace.Edges) != 0 {
		t.Fatalf("default trace should omit full edges, got %#v", trace.Edges)
	}
	encoded, err := json.Marshal(trace)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"edges"`) {
		t.Fatalf("default JSON should omit edges field, got %s", encoded)
	}
	if len(trace.CallerSites) != 1 {
		t.Fatalf("expected one compact caller site, got %#v", trace.CallerSites)
	}
	site := trace.CallerSites[0]
	if site.File != "src/lib.ts" || site.Line != 3 || site.Raw != "useDataset" || site.Caller != "appCaller" || site.Confidence == "" {
		t.Fatalf("unexpected caller site: %#v", site)
	}
	totalConfidence := trace.CallerConfidence.Exact + trace.CallerConfidence.Scoped + trace.CallerConfidence.Heuristic + trace.CallerConfidence.Unresolved
	if totalConfidence != len(trace.CallerSites) {
		t.Fatalf("confidence summary total=%d, sites=%d: %#v", totalConfidence, len(trace.CallerSites), trace.CallerConfidence)
	}

	full := TraceSymbolWithOptions(idx, "useDataset", TraceSymbolOptions{Sites: true, WithEdges: true})
	if len(full.Edges) == 0 {
		t.Fatalf("with-edges should preserve full raw edge behavior")
	}
}

func TestFindReferencesFallsBackToCGPSymbolGraph(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/lib.ts", `export function useDataset() { return { data: 1 } }
export function appCaller() {
  return useDataset()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	// "useDataset" has no RDF term, so TraceTerm alone returns not_found.
	if trace := TraceTerm(idx, "useDataset"); trace.Status != "not_found" {
		t.Fatalf("expected RDF trace to be not_found, got %q", trace.Status)
	}

	resp := FindReferences(idx, "useDataset")
	if resp.Status != "found" {
		t.Fatalf("expected fallback status found, got %q (%#v)", resp.Status, resp)
	}
	if resp.Symbol == nil || resp.Symbol.Name != "useDataset" {
		t.Fatalf("expected resolved CGP symbol, got %#v", resp.Symbol)
	}
	if len(resp.References) != 1 || resp.References[0].File != "src/lib.ts" || resp.References[0].StartLine != 3 {
		t.Fatalf("expected one caller reference from CGP graph, got %#v", resp.References)
	}
	if len(resp.Callers) != 1 || resp.Callers[0].Name != "appCaller" {
		t.Fatalf("expected appCaller as caller, got %#v", resp.Callers)
	}
	if len(resp.Warnings) == 0 {
		t.Fatalf("expected a warning noting the CGP fallback, got none")
	}

	// A query that matches no RDF term and no CGP symbol stays not_found.
	if resp := FindReferences(idx, "definitelyNotASymbol"); resp.Status != "not_found" {
		t.Fatalf("expected not_found for unmatched query, got %q", resp.Status)
	}
}

func TestFindReferencesCGPFallbackAmbiguous(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/a.ts", `export function loadHelperThing() { return 1 }`)
	write(t, root, "src/b.ts", `export function saveHelperThing() { return 2 }`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	resp := FindReferences(idx, "helperThing")
	if resp.Status != "ambiguous" {
		t.Fatalf("expected ambiguous status, got %q (%#v)", resp.Status, resp)
	}
	if len(resp.SymbolCandidates) < 2 {
		t.Fatalf("expected multiple symbol candidates, got %#v", resp.SymbolCandidates)
	}
}

func TestTraceSymbolTestCallerGroupingRulesAndFlags(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/lib.ts", `export function useDataset() { return { data: 1 } }
export function appCaller() { return useDataset() }
`)
	write(t, root, "tests/useDataset.test.ts", `import { useDataset } from '../src/lib'
it('extracts dataset ID', () => {
  useDataset()
})
it('handles network errors', () => {
  useDataset()
})
`)
	write(t, root, "__tests__/pathOnly.js", `function pathOnly() {
  useDataset()
}
`)
	write(t, root, "src/pathSuffix.spec.ts", `export function suffixSpec() {
  useDataset()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	trace := TraceSymbol(idx, "useDataset")
	if trace.Status != "found" {
		t.Fatalf("trace status = %s", trace.Status)
	}
	groups := map[string]CGPSymbolSummary{}
	for _, caller := range trace.Callers {
		if caller.Kind == "test-callback-group" {
			groups[caller.File] = caller
		}
	}
	for _, file := range []string{"tests/useDataset.test.ts", "__tests__/pathOnly.js", "src/pathSuffix.spec.ts"} {
		if _, ok := groups[file]; !ok {
			t.Fatalf("missing grouped test caller for %s in %#v", file, trace.Callers)
		}
	}
	if groups["tests/useDataset.test.ts"].Count != 2 {
		t.Fatalf("group count = %d, want 2", groups["tests/useDataset.test.ts"].Count)
	}
	if groups["tests/useDataset.test.ts"].Name == "" || groups["tests/useDataset.test.ts"].StartLine <= 0 {
		t.Fatalf("grouped test caller should have a stable name and representative start line, got %#v", groups["tests/useDataset.test.ts"])
	}
	if !containsStringValue(groups["tests/useDataset.test.ts"].NamesPreview, "it: extracts dataset ID") {
		t.Fatalf("grouped test caller should preview representative test names, got %#v", groups["tests/useDataset.test.ts"])
	}
	for _, sym := range idx.Symbols {
		if sym.Kind == "test-callback-group" {
			t.Fatalf("test-callback-group must not be stored as a symbol: %#v", sym)
		}
	}

	detailed := TraceSymbolWithOptions(idx, "useDataset", TraceSymbolOptions{Sites: true, IncludeTestDetails: true})
	for _, caller := range detailed.Callers {
		if caller.Kind == "test-callback-group" {
			t.Fatalf("include-test-details should not return grouped callers: %#v", detailed.Callers)
		}
	}
	if !callerNamed(detailed.Callers, "it: extracts dataset ID") || !callerNamed(detailed.Callers, "it: handles network errors") {
		t.Fatalf("expected individual test callback callers, got %#v", detailed.Callers)
	}

	sourceOnly := TraceSymbolWithOptions(idx, "useDataset", TraceSymbolOptions{Sites: true, ExcludeTests: true})
	if len(sourceOnly.Callers) != 1 || sourceOnly.Callers[0].Name != "appCaller" {
		t.Fatalf("exclude-tests should keep only source caller, got %#v", sourceOnly.Callers)
	}
	for _, site := range sourceOnly.CallerSites {
		if strings.HasPrefix(site.File, "tests/") || strings.HasPrefix(site.File, "__tests__/") || strings.HasSuffix(site.File, ".spec.ts") {
			t.Fatalf("exclude-tests leaked test caller site: %#v", sourceOnly.CallerSites)
		}
	}
}

func TestBuildIndexSkipsHugeGeneratedParserArtifacts(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/real.c", `int real_function(void) {
  return 1;
}
`)
	write(t, root, "internal/mamari/treesitter/swiftgrammar/parser.c", "/* generated parser */\n"+strings.Repeat("static int generated_symbol(void) { return 0; }\n", 70000))
	write(t, root, "internal/mamari/treesitter/clojuregrammar/parser.c", "/* generated parser */\n"+strings.Repeat("static int medium_generated_symbol(void) { return 0; }\n", 12000))
	write(t, root, "internal/mamari/treesitter/clojuregrammar/tree_sitter/parser.h", "#define TREE_SITTER_SERIALIZATION_BUFFER_SIZE 1024\n")
	write(t, root, "internal/mamari/treesitter/tinygrammar/parser.c", `int tiny_parser(void) {
  return 1;
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := idx.Files["internal/mamari/treesitter/swiftgrammar/parser.c"]; ok {
		t.Fatalf("huge generated parser.c should be skipped before read/parse")
	}
	if _, ok := idx.Files["internal/mamari/treesitter/clojuregrammar/parser.c"]; ok {
		t.Fatalf("medium generated parser.c should be skipped before read/parse")
	}
	if _, ok := idx.Files["internal/mamari/treesitter/clojuregrammar/tree_sitter/parser.h"]; ok {
		t.Fatalf("vendored tree-sitter parser header should be skipped")
	}
	if _, ok := idx.Files["internal/mamari/treesitter/tinygrammar/parser.c"]; !ok {
		t.Fatalf("small generated-looking parser.c should remain indexable")
	}
	if findSymbolByName(idx, "real_function").ID == "" {
		t.Fatalf("normal C source should still be indexed")
	}
	if findSymbolByName(idx, "generated_symbol").ID != "" {
		t.Fatalf("skipped generated parser should not contribute symbols")
	}
}

// TestChangedSinceJournalsRebakes verifies the watch journal: each rebake
// gets a monotonically increasing sequence number, querying with the
// previous seq returns only what changed after it, and AffectedSymbols
// reflects the current symbols belonging to the changed files.
func TestChangedSinceJournalsRebakes(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/a.ts", `export function alpha() { return 1 }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	// Baseline: empty journal.
	baseline := ChangedSince(idx, 0)
	if baseline.LatestSeq != 0 {
		t.Fatalf("baseline LatestSeq = %d, want 0", baseline.LatestSeq)
	}
	if len(baseline.Updated) != 0 {
		t.Fatalf("baseline Updated should be empty, got %v", baseline.Updated)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var done sync.WaitGroup
	done.Add(1)
	rebakes := make(chan struct{}, 4)
	go func() {
		defer done.Done()
		_ = Watch(ctx, idx, WatchOptions{
			Debounce: 50 * time.Millisecond,
			OnRebake: func(updated, removed []string) {
				if len(updated)+len(removed) > 0 {
					rebakes <- struct{}{}
				}
			},
		})
	}()
	time.Sleep(150 * time.Millisecond)
	write(t, root, "src/a.ts", `export function alpha() { return 1 }
export function beta() { return 2 }
`)
	select {
	case <-rebakes:
	case <-time.After(3 * time.Second):
		cancel()
		done.Wait()
		t.Fatal("rebake did not fire within 3s")
	}

	first := ChangedSince(idx, 0)
	if first.LatestSeq < 1 {
		t.Fatalf("LatestSeq = %d, want >= 1 after edit", first.LatestSeq)
	}
	if !contains(first.Updated, "src/a.ts") {
		t.Fatalf("Updated should include src/a.ts, got %v", first.Updated)
	}
	betaFound := false
	for _, s := range first.AffectedSymbols {
		if s.Name == "beta" {
			betaFound = true
		}
	}
	if !betaFound {
		t.Errorf("AffectedSymbols should include beta after rebake, got %+v", first.AffectedSymbols)
	}

	// Polling at the latest seq returns nothing new.
	idle := ChangedSince(idx, first.LatestSeq)
	if len(idle.Updated) != 0 {
		t.Errorf("idle poll should report no changes, got %v", idle.Updated)
	}

	cancel()
	done.Wait()
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestFetchContextModesShapeResponseVerbosity verifies that the four
// response modes return progressively more text:
//   - compact: locations only, zero token text.
//   - evidence: one line per slice.
//   - context (default): full multi-line slices.
//   - full: forces callers and callees to be included.
//
// The slice graph (file/startLine/kind/reason) is preserved across modes so
// agents can always map locations regardless of how much text rides along.
func TestFetchContextModesShapeResponseVerbosity(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/lib.ts", `export function target() {
  return work()
}
function work() { return 42 }
function caller() { return target() }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	compact, err := FetchContext(idx, "target", FetchContextOptions{Mode: ModeCompact, BudgetTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if compact.Status != "ok" {
		t.Fatalf("compact status = %s", compact.Status)
	}
	if compact.EstimatedTokens != 0 {
		t.Errorf("compact estimatedTokens = %d, want 0", compact.EstimatedTokens)
	}
	for _, s := range compact.Slices {
		if s.Text != "" {
			t.Errorf("compact slice %s:%d carries text: %q", s.File, s.StartLine, s.Text)
		}
	}

	evidence, err := FetchContext(idx, "target", FetchContextOptions{Mode: ModeEvidence, BudgetTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range evidence.Slices {
		if s.Text == "" {
			t.Errorf("evidence slice %s:%d missing text", s.File, s.StartLine)
		}
		if strings.Count(s.Text, "\n") > 1 {
			t.Errorf("evidence slice %s:%d has multi-line text: %q", s.File, s.StartLine, s.Text)
		}
	}

	ctxResp, err := FetchContext(idx, "target", FetchContextOptions{Mode: ModeContext, BudgetTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	hasMultiline := false
	for _, s := range ctxResp.Slices {
		if strings.Count(s.Text, "\n") >= 2 {
			hasMultiline = true
		}
	}
	if !hasMultiline {
		t.Errorf("expected at least one multi-line slice in context mode, got %+v", ctxResp.Slices)
	}

	full, err := FetchContext(idx, "target", FetchContextOptions{Mode: ModeFull, BudgetTokens: 4000})
	if err != nil {
		t.Fatal(err)
	}
	hasCallerSlice := false
	hasCalleeSlice := false
	for _, s := range full.Slices {
		switch s.Reason {
		case "caller signature":
			hasCallerSlice = true
		case "callee signature":
			hasCalleeSlice = true
		}
	}
	if !hasCallerSlice || !hasCalleeSlice {
		t.Errorf("full mode missing caller/callee slices: callers=%v callees=%v", hasCallerSlice, hasCalleeSlice)
	}
}

// TestImpactClosureClassifiesPathConfidence walks the reverse caller graph
// and verifies that pathConfidence reflects the *weakest* edge along the
// chain. Direct same-file callers stay exact; callers reached via a
// cross-file scoped edge get scoped; callers reached only through an
// unresolved hop are reported as unresolved with the originating reason.
func TestImpactClosureClassifiesPathConfidence(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/util.ts", `export function leaf() { return 1 }
`)
	write(t, root, "src/mid.ts", `import { leaf } from './util'
export function midA() { return leaf() }
export function midB() { return leaf() }
`)
	write(t, root, "src/top.ts", `import { midA, midB } from './mid'
export function top() { return midA() + midB() }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := Impact(idx, "leaf", 2)
	if resp.Status != "found" {
		t.Fatalf("status = %s, want found", resp.Status)
	}
	if len(resp.Layers) < 2 {
		t.Fatalf("expected at least 2 layers, got %d", len(resp.Layers))
	}
	// Layer 1: direct callers (midA, midB) reached via cross-file scoped edges.
	gotDirect := map[string]string{}
	for _, sym := range resp.Layers[0].Symbols {
		gotDirect[sym.Name] = sym.PathConfidence
	}
	for _, want := range []string{"midA", "midB"} {
		conf, ok := gotDirect[want]
		if !ok {
			t.Errorf("expected direct caller %s, got %v", want, gotDirect)
			continue
		}
		if conf != ConfScoped {
			t.Errorf("%s pathConfidence = %q, want scoped", want, conf)
		}
	}
	// Layer 2: top reached via two scoped hops, still scoped.
	foundTop := false
	for _, sym := range resp.Layers[1].Symbols {
		if sym.Name == "top" {
			foundTop = true
			if sym.PathConfidence != ConfScoped {
				t.Errorf("top pathConfidence = %q, want scoped", sym.PathConfidence)
			}
		}
	}
	if !foundTop {
		t.Errorf("expected top in layer 2, got %+v", resp.Layers[1])
	}
}

// TestImpactCompactOptionDropsDetailFieldsButKeepsIdentity covers
// ImpactOptions.Compact, added by mirroring TraceSymbolOptions.Compact to
// the same CGPSymbolSummary-returning shape: every layer entry on a wide
// blast radius previously always carried its full Signature/Docstring/
// ReturnTypes/hot-path/ID/Language fields, the same dominant per-entry
// cost trace_symbol had before its own compact fix — found by auditing
// every other tool returning this same shape once trace_symbol's fix
// shipped. Compact must drop those fields from layer entries while
// keeping enough to identify and locate each one (name/kind/file/
// startLine, plus PathConfidence/PathReason, which are Impact-specific
// and never dropped), leave the default (Impact, Compact left unset)
// completely unchanged, and leave the queried target's own Symbol at full
// detail regardless — that's the actual answer to what was asked about,
// not incidental per-entry ballast, the same distinction trace_symbol's
// own Compact draws for its top-level Symbol.
func TestImpactCompactOptionDropsDetailFieldsButKeepsIdentity(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/util.ts", `export function leaf() { return 1 }
`)
	write(t, root, "src/mid.ts", `import { leaf } from './util'
export function midA() { return leaf() }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	full := Impact(idx, "leaf", 1)
	if full.Status != "found" || len(full.Layers) == 0 || len(full.Layers[0].Symbols) == 0 {
		t.Fatalf("expected a found impact with at least one layer entry, got %#v", full)
	}
	fullEntry := full.Layers[0].Symbols[0]
	if fullEntry.ID == "" || fullEntry.Language == "" {
		t.Fatalf("expected default (non-compact) impact to keep id/language, got %#v", fullEntry)
	}
	if full.Symbol == nil || full.Symbol.Signature == "" {
		t.Fatalf("expected the queried target's own Symbol to keep its signature, got %#v", full.Symbol)
	}

	compact := ImpactWithOptions(idx, "leaf", ImpactOptions{Depth: 1, Compact: true})
	if compact.Status != "found" || len(compact.Layers) == 0 || len(compact.Layers[0].Symbols) == 0 {
		t.Fatalf("expected a found compact impact with at least one layer entry, got %#v", compact)
	}
	c := compact.Layers[0].Symbols[0]
	if c.Name != "midA" || c.Kind != "function" || c.File != "src/mid.ts" || c.StartLine == 0 {
		t.Fatalf("expected compact layer entry to keep identifying fields, got %#v", c)
	}
	if c.ID != "" || c.Language != "" {
		t.Fatalf("expected compact layer entry to drop id/language, got %#v", c)
	}
	if c.Signature != "" || c.Docstring != "" || c.Complexity != 0 {
		t.Fatalf("expected compact layer entry to drop detail fields, got %#v", c)
	}
	if c.PathConfidence == "" {
		t.Fatalf("expected compact layer entry to keep PathConfidence (Impact-specific, never dropped), got %#v", c)
	}
	if compact.Symbol == nil || compact.Symbol.Signature == "" {
		t.Fatalf("expected the queried target's own Symbol to keep its signature even in compact mode, got %#v", compact.Symbol)
	}

	compactBytes, err := json.Marshal(compact)
	if err != nil {
		t.Fatal(err)
	}
	fullBytes, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	if len(compactBytes) >= len(fullBytes) {
		t.Fatalf("expected compact response to be smaller: compact=%d full=%d", len(compactBytes), len(fullBytes))
	}
}

// TestDoctorReportsParseFailuresAndUnresolvedBreakdown verifies the doctor
// surface: stale flags, parse failures with file paths, and an unresolved
// edge breakdown grouped by reason. This is the readiness check agents run
// when their graph queries look thin.
func TestDoctorReportsParseFailuresAndUnresolvedBreakdown(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/clean.ts", `export function called() { return 1 }
export function caller() { return called() + missing() + obj.method() }
`)
	write(t, root, "src/broken.ts", `function broken() { if (true) { return 2 }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	report := Doctor(idx)
	if report.Status == "ok" {
		t.Fatalf("status = ok, want warn (broken.ts is partial); report=%+v", report)
	}
	if report.Files.ByStatus[ParseStatusPartial] < 1 {
		t.Errorf("expected at least one partial-parse file, got %v", report.Files.ByStatus)
	}
	foundBroken := false
	for _, pf := range report.ParseFailures {
		if pf.File == "src/broken.ts" {
			foundBroken = true
			if pf.Parser != "jsparse-token" {
				t.Errorf("broken.ts parser = %q, want jsparse-token", pf.Parser)
			}
		}
	}
	if !foundBroken {
		t.Errorf("expected src/broken.ts in parseFailures, got %v", report.ParseFailures)
	}
	if report.Unresolved.Total < 2 {
		t.Errorf("expected at least 2 unresolved edges (missing + obj.method), got %d", report.Unresolved.Total)
	}
	if report.Unresolved.ByReason[ReasonMissingImport] < 1 {
		t.Errorf("expected at least one missing_import unresolved, got %v", report.Unresolved.ByReason)
	}
	if report.Unresolved.ByReason[ReasonDynamicReceiver] < 1 {
		t.Errorf("expected at least one dynamic_receiver unresolved, got %v", report.Unresolved.ByReason)
	}
	if len(report.IgnorePatterns) == 0 {
		t.Errorf("expected ignore patterns to be reported")
	}
}

func TestDoctorDetectsContentStalenessIndependentOfGit(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/a.ts", `export function a() { return 1 }`)
	write(t, root, "src/b.ts", `export function b() { return 2 }`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	clean := Doctor(idx)
	if len(clean.FilesChangedSinceIndex) != 0 || len(clean.FilesDeletedSinceIndex) != 0 {
		t.Fatalf("expected no staleness right after build, got changed=%v deleted=%v", clean.FilesChangedSinceIndex, clean.FilesDeletedSinceIndex)
	}
	if clean.Status != "ok" {
		t.Fatalf("expected ok status right after build, got %q (%v)", clean.Status, clean.Warnings)
	}

	// Edit a.ts on disk without rebuilding the index (uncommitted change).
	write(t, root, "src/a.ts", `export function a() { return 100 }`)
	// Delete b.ts on disk.
	if err := os.Remove(filepath.Join(root, "src/b.ts")); err != nil {
		t.Fatal(err)
	}

	report := Doctor(idx)
	if len(report.FilesChangedSinceIndex) != 1 || report.FilesChangedSinceIndex[0] != "src/a.ts" {
		t.Fatalf("expected src/a.ts in filesChangedSinceIndex, got %v", report.FilesChangedSinceIndex)
	}
	if len(report.FilesDeletedSinceIndex) != 1 || report.FilesDeletedSinceIndex[0] != "src/b.ts" {
		t.Fatalf("expected src/b.ts in filesDeletedSinceIndex, got %v", report.FilesDeletedSinceIndex)
	}
	if report.Status != "warn" {
		t.Fatalf("expected warn status when files changed on disk, got %q", report.Status)
	}
}

// TestTokenizerHandlesTemplateAndRegex sanity-checks that tricky lexical
// constructs round-trip without producing spurious tokens. This guards
// regressions in the foundation that the parser depends on.
func TestTokenizerHandlesTemplateAndRegex(t *testing.T) {
	src := "const r = /a\\/b/g\n" +
		"const t = `x ${y} z ${ {nested: `inner ${k}`}.nested } end`\n" +
		"const s = 'line1\\\nline2'\n" +
		"// foo() bar()\n" +
		"/* baz() */ qux()\n"
	tokens := TokenizeJS(src)
	var calls []string
	for i, tok := range tokens {
		if tok.Kind != TokIdent {
			continue
		}
		next := i + 1
		for next < len(tokens) && (tokens[next].Kind == TokComment || tokens[next].Kind == TokLineComment) {
			next++
		}
		if next < len(tokens) && tokens[next].Kind == TokPunct && tokens[next].Value == "(" {
			calls = append(calls, tok.Value)
		}
	}
	want := []string{"qux"}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("only qux() should be a call site outside comments/strings/regex/templates; got %v", calls)
	}
}

// TestSearchCodePredicateNormalizationRanksRDFLines guards Phase-1 issue 1:
// a natural-language query with a known prefix and a separated local name
// like "sh in" should rank a line containing the literal `sh:in` predicate
// above generic SPARQL or template-text matches.
func TestSearchCodePredicateNormalizationRanksRDFLines(t *testing.T) {
	root := t.TempDir()
	write(t, root, "shapes/main.ttl", `@prefix sh: <http://www.w3.org/ns/shacl#> .
@prefix ex: <http://example.org/> .

ex:Shape a sh:NodeShape ; sh:path ex:status .
`)
	write(t, root, "src/themes/default.ts", `// renderer for sh:in lists
export function createListEditor(template) {
  return template.config.lists[template.shaclIn].map(option => option)
}
`)
	write(t, root, "src/property-template.ts", `// pulls list values when sh:in is set
const PREFIX_SHACL = 'http://www.w3.org/ns/shacl#'
function readShaclIn(props) {
  if (props[PREFIX_SHACL + 'in']) {
    template.shaclIn = props[PREFIX_SHACL + 'in']
  }
}
`)
	write(t, root, "src/util.ts", `// SPARQL select example: SELECT ?x WHERE { ?x sh:select ?y }
export const sparqlSelect = 'SELECT ?x WHERE { ?x ?p ?y }'
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := SearchCode(idx, "how are sh in options rendered as select choices", SearchCodeOptions{
		Limit:        5,
		BudgetTokens: 600,
		SourceOnly:   true,
	})
	if resp.Status != "ok" {
		t.Fatalf("status = %s warnings=%v", resp.Status, resp.Warnings)
	}
	if len(resp.ExactPhrases) == 0 {
		t.Fatalf("expected sh:in to be extracted as an exact predicate phrase, got %#v", resp.ExactPhrases)
	}
	foundShIn := false
	for _, ph := range resp.ExactPhrases {
		if ph.Kind == "predicate" && ph.Literal == "sh:in" {
			foundShIn = true
			break
		}
	}
	if !foundShIn {
		t.Fatalf("expected predicate hint sh:in in %#v", resp.ExactPhrases)
	}
	if len(resp.Hits) == 0 {
		t.Fatalf("no hits returned")
	}
	top := resp.Hits[0]
	if !strings.Contains(top.Text, "sh:in") && !strings.Contains(top.Text, "shaclIn") && !strings.Contains(top.Text, "PREFIX_SHACL + 'in'") {
		t.Fatalf("top hit should include sh:in evidence, got %s:%d %q", top.File, top.StartLine, top.Text)
	}
	for _, hit := range resp.Hits {
		if strings.Contains(hit.Text, "sh:select") && hit.Score > top.Score {
			t.Fatalf("sh:select line outranked sh:in evidence: %#v vs %#v", hit, top)
		}
	}
}

// TestSearchCodeExactRouteAndMimeBoostsLines guards Phase-1 issue 5: when
// the query already names exact strings (route literal, MIME), the ranker
// should rank lines containing those strings ahead of token-overlap noise.
func TestSearchCodeExactRouteAndMimeBoostsLines(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/signing/preview.ts", `// envelope preview endpoint
export function previewEnvelopeDocuments(req, res) {
  res.setHeader('Content-Type', 'application/pdf')
  return res.end(body)
}
app.get('/signing/:id/preview', previewEnvelopeDocuments)
`)
	write(t, root, "src/signing/other.ts", `// noise that mentions preview and pdf separately
function previewSomething() { return 'pdf' }
function notARoute() { return '/healthz' }
`)
	write(t, root, "src/signing/notes.ts", `// only mentions preview the word, no route, no mime
function describePreviewFeature() { return 'preview' }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := SearchCode(idx, "where is GET /signing/:id/preview returning application/pdf", SearchCodeOptions{
		Limit:        5,
		BudgetTokens: 600,
		SourceOnly:   true,
	})
	if resp.Status != "ok" {
		t.Fatalf("status = %s warnings=%v", resp.Status, resp.Warnings)
	}
	kinds := map[string]bool{}
	literals := map[string]bool{}
	for _, ph := range resp.ExactPhrases {
		kinds[ph.Kind] = true
		literals[ph.Literal] = true
	}
	if !kinds["route"] {
		t.Fatalf("expected a route exact phrase, got %#v", resp.ExactPhrases)
	}
	if !kinds["mime"] {
		t.Fatalf("expected a mime exact phrase, got %#v", resp.ExactPhrases)
	}
	if literals["/pdf"] {
		t.Fatalf("mime suffix /pdf must not be extracted as a route: %#v", resp.ExactPhrases)
	}
	if len(resp.Hits) == 0 {
		t.Fatalf("no hits")
	}
	top := resp.Hits[0]
	if top.File != "src/signing/preview.ts" {
		t.Fatalf("expected src/signing/preview.ts as top hit, got %s:%d", top.File, top.StartLine)
	}
	if len(top.MatchedExact) == 0 {
		t.Fatalf("top hit should record matchedExact, got %#v", top)
	}
}

// TestSearchCodeCaseInsensitiveWholeLineBoost guards the fix that removed
// codeSearchLine.lowerText (a precomputed lowercased copy of every line,
// stored eagerly at build time for every line in the repo) in favor of
// lowercasing only the few per-query candidate lines on demand inside
// scoreSearchCodeLine. The behavior that field existed for — ranking a line
// whose lowercased text contains the lowercased query as a literal
// substring above a line with the same token overlap but no such substring
// match — must be byte-for-byte preserved, including across letter casing
// the query and the source line don't share.
func TestSearchCodeCaseInsensitiveWholeLineBoost(t *testing.T) {
	root := t.TempDir()
	// "zzz_scattered.ts" deliberately sorts after "exact.ts" so that if the
	// case-insensitive bonus silently stopped applying (every line tying
	// on score), a same-order tie-break could never accidentally make this
	// assertion pass for the wrong reason.
	write(t, root, "src/notify/exact.ts", `// send EMAIL now to confirm delivery
function unrelatedHelper() { return 1 }
`)
	write(t, root, "src/notify/zzz_scattered.ts", `// now send the email notification queue
function anotherHelper() { return 2 }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	// Query and source line deliberately differ in letter case
	// ("Send Email Now" vs "send EMAIL now") — this only resolves to the
	// same line if the comparison actually folds case, which is the exact
	// behavior the removed lowerText field existed for.
	resp := SearchCode(idx, "Send Email Now", SearchCodeOptions{
		Limit:        5,
		BudgetTokens: 600,
		SourceOnly:   true,
	})
	if resp.Status != "ok" || len(resp.Hits) < 2 {
		t.Fatalf("status=%s hits=%d", resp.Status, len(resp.Hits))
	}
	var exactScore, scatteredScore int
	for _, h := range resp.Hits {
		switch h.File {
		case "src/notify/exact.ts":
			exactScore = h.Score
		case "src/notify/zzz_scattered.ts":
			scatteredScore = h.Score
		}
	}
	if exactScore <= scatteredScore {
		t.Fatalf("expected exact.ts (literal case-insensitive substring match, +160 bonus) to outscore zzz_scattered.ts (same tokens, no contiguous substring match): exact=%d scattered=%d hits=%#v",
			exactScore, scatteredScore, resp.Hits)
	}
	if exactScore != scatteredScore+160 {
		t.Fatalf("expected exactly a +160 case-insensitive whole-line bonus, got exact=%d scattered=%d (diff=%d)",
			exactScore, scatteredScore, exactScore-scatteredScore)
	}
}

// TestSearchCodeExactIdentifierBoostsExactName guards Phase-1 issue 5: when
// the query contains a long camel/Pascal identifier the user already knows,
// the ranker should rank exact-name lines ahead of camel-split token bag
// matches.
func TestSearchCodeExactIdentifierBoostsExactName(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/documents/preview.ts", `export async function previewEnvelopeDocuments(envelopeId: string) {
  return documents.envelopesApi.getDocument(envelopeId, 'combined')
}
`)
	write(t, root, "src/documents/other.ts", `// only camel-split overlap: preview, envelope, documents
function preview() { return 'preview' }
function envelope() { return 'envelope' }
function documents() { return 'documents' }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := SearchCode(idx, "find previewEnvelopeDocuments handler", SearchCodeOptions{
		Limit:        5,
		BudgetTokens: 600,
		SourceOnly:   true,
	})
	if resp.Status != "ok" {
		t.Fatalf("status=%s", resp.Status)
	}
	wantIdent := false
	for _, ph := range resp.ExactPhrases {
		if ph.Kind == "ident" && ph.Literal == "previewEnvelopeDocuments" {
			wantIdent = true
		}
	}
	if !wantIdent {
		t.Fatalf("expected ident exact phrase previewEnvelopeDocuments, got %#v", resp.ExactPhrases)
	}
	if len(resp.Hits) == 0 || resp.Hits[0].File != "src/documents/preview.ts" {
		t.Fatalf("expected src/documents/preview.ts as top hit, got %#v", resp.Hits)
	}
}

func TestIsBackupOrDeadFile(t *testing.T) {
	backups := []string{
		"src/utils/class_js_backup_07-10-2024.js",
		"src/utils/env.js.bak",
		"src/config.old.json",
		"src/App.tsx.orig",
		"src/Foo copy.ts",
		"src/Foo copy 2.ts",
		"src/.notes.txt~",
		"reports/export_07-10-2024_backup.csv",
	}
	for _, f := range backups {
		if !isBackupOrDeadFile(f) {
			t.Errorf("expected %q to be detected as a backup/dead file", f)
		}
	}

	active := []string{
		"src/utils/env.js",
		"src/config.json",
		"src/App.tsx",
		"src/household.ts",
		"src/oldServiceClient.ts",
		"reports/export_2024-07-10.csv",
		"src/models/Order.ts",
	}
	for _, f := range active {
		if isBackupOrDeadFile(f) {
			t.Errorf("expected %q to NOT be detected as a backup/dead file", f)
		}
	}
}

func TestSearchQueryTermsFiltersStopwordsBeforeStemming(t *testing.T) {
	terms := searchQueryTerms("zzqx blorf nothing should match this")
	for _, stop := range []string{"thi", "this", "should", "nothing"} {
		for _, term := range terms {
			if term == stop {
				t.Fatalf("stopword %q leaked into query terms %v", stop, terms)
			}
		}
	}
	found := false
	for _, term := range terms {
		if term == "match" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected content word %q to remain in query terms %v", "match", terms)
	}
}

func TestSearchCodeExcludesBackupFilesByDefault(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/sendEmail.ts", `export function sendEmail(to: string) { return notify(to) }`)
	write(t, root, "src/sendEmail_backup_07-10-2024.ts", `export function sendEmail(to: string) { return legacyNotify(to) }`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	resp := SearchCode(idx, "sendEmail", SearchCodeOptions{Limit: 10, BudgetTokens: 2000})
	if resp.Status != "ok" {
		t.Fatalf("status=%s", resp.Status)
	}
	for _, hit := range resp.Hits {
		if hit.File == "src/sendEmail_backup_07-10-2024.ts" {
			t.Fatalf("backup file should be excluded from search-code hits, got %#v", resp.Hits)
		}
	}
}

func TestSearchCodePreferDefinitionsAndEvidenceMode(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/composables/useAccessPermissions.ts", `export function usePermissions() {
  return { accessControlEnabled: true, getDatasetPermissions: () => [] }
}
`)
	write(t, root, "src/views/DatasetDetailsView.vue", `<script setup>
import { usePermissions } from '@/composables/useAccessPermissions'
const { accessControlEnabled, getDatasetPermissions } = usePermissions()
</script>
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := SearchCode(idx, "access permissions dataset scopes usePermissions", SearchCodeOptions{
		Limit:             4,
		BudgetTokens:      300,
		SourceOnly:        true,
		PreferDefinitions: true,
		Mode:              ModeEvidence,
	})
	if resp.Status != "ok" {
		t.Fatalf("status=%s warnings=%v", resp.Status, resp.Warnings)
	}
	if resp.Mode != ModeEvidence {
		t.Fatalf("mode=%q, want evidence", resp.Mode)
	}
	if len(resp.Hits) == 0 || resp.Hits[0].File != "src/composables/useAccessPermissions.ts" {
		t.Fatalf("prefer-definitions should rank the composable definition first, got %#v", resp.Hits)
	}
	if resp.Hits[0].StartLine != resp.Hits[0].FocusLine || resp.Hits[0].EndLine != resp.Hits[0].FocusLine {
		t.Fatalf("evidence mode should collapse to focused line, got %#v", resp.Hits[0])
	}
}

func TestSearchCodeArchitectureQueryBoostsCoreSymbolsOverSupportScripts(t *testing.T) {
	root := t.TempDir()
	write(t, root, "internal/mamari/trace.go", `package mamari

type TraceSymbolCandidateDetail struct{}

func TraceSymbol() {
  detail := TraceSymbolCandidateDetail{}
  _ = detail
}
`)
	write(t, root, "scripts/benchmark_tokens.py", `def candidate_trace_benchmark():
    candidate = "trace symbol callers ambiguity candidate details"
    return candidate
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := SearchCode(idx, "trace symbol callers ambiguity candidate details", SearchCodeOptions{
		Limit:        4,
		BudgetTokens: 1000,
		SourceOnly:   true,
	})
	if resp.Status != "ok" {
		t.Fatalf("status=%s warnings=%v", resp.Status, resp.Warnings)
	}
	if len(resp.Hits) == 0 || resp.Hits[0].File != "internal/mamari/trace.go" {
		t.Fatalf("expected core symbol file before benchmark script, got %#v", resp.Hits)
	}
}

func TestInspectFlowPrefersCoreImplementationOverExamples(t *testing.T) {
	root := t.TempDir()
	write(t, root, "lib/application.js", `var Router = require('router')
var finalhandler = require('finalhandler')

app.handle = function handle(req, res, callback) {
  var done = callback || finalhandler(req, res, {})
  this.router.handle(req, res, done)
}

app.use = function use(path, fn) {
  return this.router.use(path, fn)
}

app.route = function route(path) {
  return this.router.route(path)
}
`)
	write(t, root, "examples/error/index.js", `function error(err, req, res, next) {
  // error handling middleware when you next(err)
  // router layer match params middleware dispatch next error handling
  res.status(500)
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := InspectFlow(idx, "router layer match params middleware dispatch next error handling", InspectFlowOptions{
		Limit:              6,
		BudgetTokens:       1600,
		SearchBudgetTokens: 700,
		ContextLines:       6,
		SourceOnly:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != "ok" {
		t.Fatalf("inspect-flow status=%s warnings=%v", resp.Status, resp.Warnings)
	}
	if len(resp.Search.Hits) == 0 || resp.Search.Hits[0].File != "lib/application.js" {
		t.Fatalf("expected core router implementation before examples, got %#v", resp.Search.Hits)
	}
	if len(resp.Context.Slices) == 0 || resp.Context.Slices[0].File != "lib/application.js" {
		t.Fatalf("expected core implementation context first, got %#v", resp.Context.Slices)
	}
	for _, slice := range resp.Context.Slices {
		if strings.HasPrefix(slice.File, "examples/") {
			t.Fatalf("source-only inspect-flow should not pull example caller context back in, got %#v", resp.Context.Slices)
		}
	}
}

func TestInspectFlowRanksFrameworkLifecycleImplementationOverRequestWrappers(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/webapp/wrappers.py", `class Request:
    """Request wrapper.

    A before/after handler may inspect request.url_rule.methods.
    If request data exceeds a limit, raise RequestEntityTooLarge exception.
    This request wrapper documents exceptions and after handler behavior.
    """

    def close(self):
        pass
`)
	write(t, root, "src/webapp/app.py", `class Application:
    def handle_exception(self, ctx, exc):
        return self.finalize_request(ctx, exc, from_error_handler=True)

    def full_dispatch_request(self, ctx):
        rv = self.preprocess_request(ctx)
        if rv is None:
            rv = self.dispatch_request(ctx)
        return self.finalize_request(ctx, rv)

    def dispatch_request(self, ctx):
        return self.ensure_sync(ctx.view_function)(**ctx.view_args)

    def finalize_request(self, ctx, rv, from_error_handler=False):
        response = self.make_response(rv)
        return self.process_response(ctx, response)

    def process_response(self, ctx, response):
        for func in reversed(self.after_request_funcs):
            response = self.ensure_sync(func)(response)
        return response

    def do_teardown_request(self, ctx, exc=None):
        for func in reversed(self.teardown_request_funcs):
            self.ensure_sync(func)(exc)
`)
	write(t, root, "src/webapp/context.py", `class RequestContext:
    def pop(self, exc=None):
        self.app.do_teardown_request(self, exc)
        self.request.close()
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := InspectFlow(idx, "request dispatch before after teardown exceptions", InspectFlowOptions{
		Limit:              6,
		BudgetTokens:       1800,
		SearchBudgetTokens: 700,
		ContextLines:       6,
		SourceOnly:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != "ok" {
		t.Fatalf("inspect-flow status=%s warnings=%v", resp.Status, resp.Warnings)
	}
	if len(resp.Search.Hits) == 0 {
		t.Fatalf("expected lifecycle implementation hits, got none")
	}
	firstFile := resp.Search.Hits[0].File
	if firstFile != "src/webapp/app.py" && firstFile != "src/webapp/context.py" {
		t.Fatalf("expected lifecycle implementation before request wrapper, got %#v", resp.Search.Hits)
	}
	for i, hit := range resp.Search.Hits {
		if hit.File == "src/webapp/wrappers.py" && i < 2 {
			t.Fatalf("request wrapper should not outrank lifecycle implementation, got %#v", resp.Search.Hits)
		}
	}
	if len(resp.Context.Slices) == 0 || (resp.Context.Slices[0].File != "src/webapp/app.py" && resp.Context.Slices[0].File != "src/webapp/context.py") {
		t.Fatalf("expected lifecycle implementation context first, got %#v", resp.Context.Slices)
	}
	hasContextFile := false
	for _, slice := range resp.Context.Slices {
		if slice.File == "src/webapp/context.py" {
			hasContextFile = true
			break
		}
	}
	if !hasContextFile {
		t.Fatalf("expected broad lifecycle context to include adjacent request context file, got %#v", resp.Context.Slices)
	}
}

func TestInspectFlowBoundsLargeParserContext(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/search/query-parser.ts", `export function parseQuery(raw: string): ParsedQuery {
  const filters: Filter[] = []
  const terms: string[] = []
  const tokens = raw.split(/\s+/)
  for (const token of tokens) {
    if (!token) {
      continue
    }
    if (token.startsWith("file:")) {
      filters.push({ kind: "file", value: token.slice(5) })
      continue
    }
    if (token.startsWith("symbol:")) {
      filters.push({ kind: "symbol", value: token.slice(7) })
      continue
    }
    if (token.startsWith("kind:")) {
      filters.push({ kind: "kind", value: token.slice(5) })
      continue
    }
    if (token.startsWith("lang:")) {
      filters.push({ kind: "language", value: token.slice(5) })
      continue
    }
    if (token.startsWith("-")) {
      filters.push({ kind: "exclude", value: token.slice(1) })
      continue
    }
    terms.push(token)
  }
  const normalized = terms
    .map((term) => term.trim())
    .filter(Boolean)
    .join(" ")
  const query: ParsedQuery = {
    raw,
    terms,
    filters,
    normalized,
  }
  if (filters.length > 0) {
    query.hasFilters = true
  }
  if (normalized.length === 0 && filters.length === 0) {
    query.empty = true
  }
  query.debug = [
    raw,
    normalized,
    String(filters.length),
    String(terms.length),
  ]
  query.scoreHints = terms.map((term) => ({
    term,
    weight: term.length > 3 ? 2 : 1,
  }))
  query.filterText = filters
    .map((filter) => filter.kind + ":" + filter.value)
    .join(" ")
  query.searchText = [normalized, query.filterText].filter(Boolean).join(" ")
  query.extra = {
    rawLength: raw.length,
    tokenCount: tokens.length,
    filterCount: filters.length,
  }
  query.auditTrail = []
  query.auditTrail.push("parse raw query")
  query.auditTrail.push("extract filters")
  query.auditTrail.push("normalize remaining terms")
  query.auditTrail.push("prepare search text")
  return query
}
`)
	write(t, root, "src/db/queries.ts", `export function buildSearchWhere(parsed: ParsedQuery): SQL {
  const clauses: SQL[] = []
  for (const filter of parsed.filters) {
    if (filter.kind === "file") {
      clauses.push(sql`+"`files.path like ${filter.value}`"+`)
    }
    if (filter.kind === "symbol") {
      clauses.push(sql`+"`symbols.name = ${filter.value}`"+`)
    }
  }
  return and(clauses)
}

export function applyFilters(parsed: ParsedQuery, rows: Row[]): Row[] {
  return rows.filter((row) => parsed.filters.every((filter) => rowMatchesFilter(row, filter)))
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := InspectFlow(idx, "parse raw query filters", InspectFlowOptions{
		Limit:              6,
		BudgetTokens:       1800,
		SearchBudgetTokens: 700,
		ContextLines:       8,
		SourceOnly:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != "ok" {
		t.Fatalf("inspect-flow status=%s warnings=%v", resp.Status, resp.Warnings)
	}
	hasParser := false
	for _, slice := range resp.Context.Slices {
		if slice.File == "src/search/query-parser.ts" {
			hasParser = true
			if got := slice.EndLine - slice.StartLine + 1; got > defaultInspectFlowSymbolLines {
				t.Fatalf("parser context should be bounded to %d lines, got %d in %#v", defaultInspectFlowSymbolLines, got, slice)
			}
			if !slice.Truncated || slice.FullStartLine != 1 || slice.FullEndLine <= slice.EndLine {
				t.Fatalf("bounded context should identify the full fetchable range, got %#v", slice)
			}
		}
	}
	if !hasParser {
		t.Fatalf("expected parser context, got %#v", resp.Context.Slices)
	}
}

func TestSearchCodeExactFirstFiltersNoise(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/routes/preview.ts", `app.get('/signing/:id/preview', previewEnvelopeDocuments)
function previewEnvelopeDocuments() {
  res.setHeader('Content-Type', 'application/pdf')
}
`)
	write(t, root, "src/routes/noise.ts", `function preview() {
  return 'pdf preview route'
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := SearchCode(idx, "preview route /signing/:id/preview application/pdf", SearchCodeOptions{
		Limit:        5,
		BudgetTokens: 300,
		SourceOnly:   true,
		ExactFirst:   true,
		Mode:         ModeEvidence,
	})
	if resp.Status != "ok" {
		t.Fatalf("status=%s warnings=%v", resp.Status, resp.Warnings)
	}
	for _, hit := range resp.Hits {
		if hit.File == "src/routes/noise.ts" {
			t.Fatalf("exact-first should filter non-exact noise, got %#v", resp.Hits)
		}
	}
}

func TestInspectExactBundlesSymbolAndRouteEvidence(t *testing.T) {
	root := t.TempDir()
	write(t, root, "controllers/documentController.js", `async function downloadEnvelopeDocuments(req) {
  return Buffer.from('pdf')
}
export async function previewEnvelopeDocuments(req, res) {
  const body = await downloadEnvelopeDocuments(req)
  res.setHeader('Content-Type', 'application/pdf')
  return res.end(body)
}
`)
	write(t, root, "routes/documentRoutes.js", `import { previewEnvelopeDocuments } from '../controllers/documentController'
router.post('/signing/:id/preview', previewEnvelopeDocuments)
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := InspectExact(idx, "DocumentService envelope preview PDF application/pdf previewEnvelopeDocuments /signing/:id/preview", InspectExactOptions{
		Limit:      5,
		SourceOnly: true,
	})
	if resp.Status != "ok" {
		t.Fatalf("status=%s warnings=%v", resp.Status, resp.Warnings)
	}
	if resp.EstimatedTokens >= 700 {
		t.Fatalf("expected compact exact bundle under target budget, got %d tokens: %#v", resp.EstimatedTokens, resp.Clusters)
	}
	var symbolCluster, routeCluster *ExactEvidenceCluster
	for i := range resp.Clusters {
		if resp.Clusters[i].Symbol == "previewEnvelopeDocuments" {
			symbolCluster = &resp.Clusters[i]
		}
		if resp.Clusters[i].Route == "POST /signing/:id/preview" {
			routeCluster = &resp.Clusters[i]
		}
	}
	if symbolCluster == nil {
		t.Fatalf("expected previewEnvelopeDocuments cluster, got %#v", resp.Clusters)
	}
	if !containsString(symbolCluster.Matched, "application/pdf") || !containsString(symbolCluster.Matched, "previewEnvelopeDocuments") {
		t.Fatalf("symbol cluster should co-locate identifier and MIME evidence, got %#v", symbolCluster)
	}
	if len(symbolCluster.Callees) == 0 || symbolCluster.Callees[0].Name != "downloadEnvelopeDocuments" {
		t.Fatalf("expected compact callee evidence, got %#v", symbolCluster.Callees)
	}
	if routeCluster == nil || routeCluster.Handler != "previewEnvelopeDocuments" {
		t.Fatalf("expected compact route/handler cluster, got %#v", resp.Clusters)
	}
}

func TestInspectExactBundlesVueKebabClassEvidence(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/views/Documents/TrackingDrawer.vue", `<template>
  <div v-if="record._signatures?.length" class="signature-grid">
    <div v-for="signature in record._signatures" class="signature-thumb">
      <div class="signature-image-wrap">
        <img :src="signature.imageUrl" :alt="signature.name || signature.email" />
      </div>
    </div>
  </div>
</template>

<style scoped>
.signature-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
}

.signature-thumb {
  display: grid;
  grid-template-columns: 84px 1fr;
  min-height: 58px;
}

.signature-image-wrap {
  display: flex;
  height: 42px;
}

.signature-image-wrap img {
  max-width: 100%;
  max-height: 38px;
  object-fit: contain;
}
</style>
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := InspectExact(idx, "signature-image-wrap signature-thumb max-height", InspectExactOptions{
		Limit:      5,
		SourceOnly: true,
	})
	if resp.Status != "ok" {
		t.Fatalf("status=%s warnings=%v phrases=%#v", resp.Status, resp.Warnings, resp.ExactPhrases)
	}
	if !containsExactPhrase(resp.ExactPhrases, "signature-image-wrap", "kebab-ident") {
		t.Fatalf("expected kebab exact phrase for signature-image-wrap, got %#v", resp.ExactPhrases)
	}
	if containsExactPhrase(resp.ExactPhrases, "max-height", "kebab-ident") {
		t.Fatalf("common CSS properties should not become exact identifiers unless quoted, got %#v", resp.ExactPhrases)
	}
	var wrap, thumb *ExactEvidenceCluster
	for i := range resp.Clusters {
		switch resp.Clusters[i].Symbol {
		case "signature-image-wrap":
			wrap = &resp.Clusters[i]
		case "signature-thumb":
			thumb = &resp.Clusters[i]
		}
	}
	if wrap == nil {
		t.Fatalf("expected signature-image-wrap cluster, got %#v", resp.Clusters)
	}
	if !clusterHasLineText(*wrap, "height: 42px") || !clusterHasLineText(*wrap, "max-height: 38px") {
		t.Fatalf("expected wrapped CSS rule declarations in one class cluster, got %#v", wrap.Lines)
	}
	if !clusterHasLineText(*wrap, `class="signature-image-wrap"`) {
		t.Fatalf("expected template class usage in class cluster, got %#v", wrap.Lines)
	}
	if thumb == nil || !clusterHasLineText(*thumb, "grid-template-columns: 84px 1fr") || !clusterHasLineText(*thumb, "min-height: 58px") {
		t.Fatalf("expected signature-thumb sizing declarations, got %#v", thumb)
	}
}

func TestFrameworkFactsConnectRoutesHandlersAndHTTPClients(t *testing.T) {
	root := t.TempDir()
	write(t, root, "frontend/TrackingDrawer.vue", "<script setup lang=\"ts\">\n"+
		"import axios from 'axios'\n\n"+
		"async function loadSignatureImages(record) {\n"+
		"  return axios.get(`/documents/signing/${record.id}/signatures`)\n"+
		"}\n"+
		"</script>\n")
	write(t, root, "routes/documentRoutes.js", `import * as documentController from '../controllers/documentController'

router.post('/signing/:id/mark-signer-as-signed', ensureAuth, documentController.markSignerCompleted)
router.get('/signing/:id/signatures', ensureAuth, documentController.getEnvelopeSignatureImages)
`)
	write(t, root, "controllers/documentController.js", `export async function markSignerCompleted(req, res) {
  signing.status = 'COMPLETED'
}

export async function getEnvelopeSignatureImages(req, res) {
  return res.json([])
}
`)

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	routes := ListSymbols(idx, "GET /signing/:id/signatures", "http-route", "")
	if len(routes.Symbols) != 1 {
		t.Fatalf("expected route symbol, got %#v", routes.Symbols)
	}
	traceRoute := TraceSymbolWithOptions(idx, routes.Symbols[0].ID, TraceSymbolOptions{Sites: true})
	if traceRoute.Status != "found" {
		t.Fatalf("expected route trace, got %#v", traceRoute)
	}
	if !summaryContains(traceRoute.Callers, "loadSignatureImages") {
		t.Fatalf("expected frontend HTTP caller to route, got %#v", traceRoute.Callers)
	}
	if !summaryContains(traceRoute.Callees, "getEnvelopeSignatureImages") {
		t.Fatalf("expected route handler callee, got %#v", traceRoute.Callees)
	}

	handlerTrace := TraceSymbolWithOptions(idx, "markSignerCompleted", TraceSymbolOptions{Sites: true})
	if handlerTrace.Status != "found" || !summaryContains(handlerTrace.Callers, "POST /signing/:id/mark-signer-as-signed") {
		t.Fatalf("expected route as handler caller, got %#v", handlerTrace)
	}

	exact := InspectExact(idx, "/documents/signing/:id/signatures", InspectExactOptions{Limit: 2})
	if exact.Status != "ok" || len(exact.Clusters) == 0 || exact.Clusters[0].Symbol != "GET /signing/:id/signatures" {
		t.Fatalf("mounted route phrase should resolve to compact route symbol, got %#v", exact)
	}
	if !containsString(exact.Clusters[0].Matched, "/documents/signing/:id/signatures") {
		t.Fatalf("expected semantic route match to be recorded, got %#v", exact.Clusters[0])
	}
}

func TestInspectExactFiltersSingleCommonIdentifierNoise(t *testing.T) {
	root := t.TempDir()
	write(t, root, "routes/documentRoutes.js", `import { markSignerCompleted } from './controller'
router.post('/signing/:id/mark-signer-as-signed', markSignerCompleted)
`)
	write(t, root, "routes/controller.js", `export async function markSignerCompleted(req, res) {
  return res.json({ ok: true })
}
`)
	var noise strings.Builder
	noise.WriteString("export function unrelated(applicationId) {\n")
	for i := 0; i < 40; i++ {
		noise.WriteString("  console.log(applicationId)\n")
	}
	noise.WriteString("}\n")
	write(t, root, "src/noise.js", noise.String())

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := InspectExact(idx, "applicationId markSignerCompleted /signing/:id/mark-signer-as-signed", InspectExactOptions{Limit: 8})
	if resp.Status != "ok" {
		t.Fatalf("expected ok response, got %#v", resp)
	}
	if len(resp.Clusters) == 0 {
		t.Fatalf("expected exact route/handler evidence")
	}
	for _, cluster := range resp.Clusters {
		if cluster.File == "src/noise.js" {
			t.Fatalf("single common exact identifier should not create noise cluster: %#v", resp.Clusters)
		}
	}
	if resp.Clusters[0].Route == "" && resp.Clusters[0].Symbol != "markSignerCompleted" {
		t.Fatalf("expected route/handler evidence first, got %#v", resp.Clusters[0])
	}
}

func TestRepoMapPersonalizesMentionedIdentifier(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/api.ts", `export function useDataset() {
  return normalizeDataset()
}
function normalizeDataset() { return true }
`)
	write(t, root, "src/sidebar.ts", `import { useDataset } from './api'
export function renderSidebar() {
  return useDataset()
}
`)
	write(t, root, "src/noise.ts", `export function utility() { return true }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := RepoMap(idx, RepoMapOptions{BudgetTokens: 1200, Mentioned: []string{"useDataset"}, SourceOnly: true})
	if resp.Status != "ok" {
		t.Fatalf("status=%s warnings=%v", resp.Status, resp.Warnings)
	}
	if len(resp.Symbols) == 0 || resp.Symbols[0].Name != "useDataset" {
		t.Fatalf("mentioned identifier should top symbol map, got %#v", resp.Symbols)
	}
	foundAPI := false
	for _, file := range resp.Files {
		if file.File == "src/api.ts" && containsString(file.MatchedMention, "usedataset") {
			foundAPI = true
		}
	}
	if !foundAPI {
		t.Fatalf("expected api.ts to carry useDataset personalization, got %#v", resp.Files)
	}
}

func TestIngestSCIPAddsReferencesWithoutCalls(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/app.ts", `export function foo() {
  return bar()
}
export function bar() {
  return true
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	index := &scip.Index{
		Documents: []*scip.Document{{
			RelativePath: "src/app.ts",
			Occurrences: []*scip.Occurrence{
				{Range: []int32{0, 16, 19}, Symbol: "local 0 package . foo().", SymbolRoles: int32(scip.SymbolRole_Definition)},
				{Range: []int32{3, 16, 19}, Symbol: "local 0 package . bar().", SymbolRoles: int32(scip.SymbolRole_Definition)},
				{Range: []int32{1, 9, 12}, Symbol: "local 0 package . bar()."},
			},
			Symbols: []*scip.SymbolInformation{
				{Symbol: "local 0 package . foo().", DisplayName: "foo", Kind: scip.SymbolInformation_Function},
				{Symbol: "local 0 package . bar().", DisplayName: "bar", Kind: scip.SymbolInformation_Function},
			},
		}},
	}
	data, err := proto.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := IngestSCIP(idx, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	hasReference := false
	for _, edge := range idx.SymbolEdges {
		if edge.Type == "references-symbol" && edge.Evidence.File == "src/app.ts" && edge.Evidence.StartLine == 2 {
			hasReference = true
		}
		if edge.Type == "calls" && edge.Evidence.Kind == "scip-reference" {
			t.Fatalf("SCIP reference must not become calls edge: %#v", edge)
		}
	}
	if !hasReference {
		t.Fatalf("expected references-symbol edge from SCIP ingestion, got %#v", idx.SymbolEdges)
	}
	if got := ListSymbols(idx, "bar", "", ""); len(got.Symbols) == 0 {
		t.Fatalf("expected compiler-backed symbol to be queryable, got %#v", got)
	}
}

func TestExactEvidenceGoldenSnapshot(t *testing.T) {
	root := filepath.Join("testdata", "exact-evidence")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	inspectExact := InspectExact(idx, "previewEnvelopeDocuments application/pdf /signing/:id/preview", InspectExactOptions{
		Limit:      4,
		SourceOnly: true,
	})
	searchCode := SearchCode(idx, "GET /signing/:id/preview application/pdf", SearchCodeOptions{
		Limit:        4,
		BudgetTokens: 400,
		SourceOnly:   true,
		ExactFirst:   true,
		Mode:         ModeEvidence,
	})
	repoMap := RepoMap(idx, RepoMapOptions{
		BudgetTokens: 500,
		Mentioned:    []string{"previewEnvelopeDocuments"},
		SourceOnly:   true,
	})
	snapshot := struct {
		Symbols      []CGPSymbolSummary   `json:"symbols"`
		SymbolEdges  []CGPEdge            `json:"symbolEdges"`
		References   []Reference          `json:"references"`
		InspectExact InspectExactResponse `json:"inspectExact"`
		SearchCode   SearchCodeResponse   `json:"searchCode"`
		RepoMap      RepoMapResponse      `json:"repoMap"`
	}{
		Symbols:      ListSymbolsWithOptions(idx, "", "", "", ListSymbolsOptions{SourceOnly: true}).Symbols,
		SymbolEdges:  idx.snapshot().SymbolEdges,
		References:   idx.snapshot().References,
		InspectExact: inspectExact,
		SearchCode:   searchCode,
		RepoMap:      repoMap,
	}
	got, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	golden := filepath.Join(root, "inspect_exact.golden.json")
	if os.Getenv("MAMARI_UPDATE_SNAPSHOTS") == "1" {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("exact evidence snapshot mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestInspectTermBridgesRDFTermToImplementationIdentifiers(t *testing.T) {
	root := t.TempDir()
	write(t, root, "shapes/main.ttl", `@prefix sh: <http://www.w3.org/ns/shacl#> .
@prefix ex: <http://example.org/> .

ex:Shape
  a sh:NodeShape ;
  sh:path ex:status ;
  sh:in ("draft" "published") .
`)
	write(t, root, "src/property-template.ts", `const PREFIX_SHACL = 'http://www.w3.org/ns/shacl#'
export function readTemplate(props, template) {
  if (props[PREFIX_SHACL + 'in']) {
    template.shaclIn = props[PREFIX_SHACL + 'in']
  }
}
`)
	write(t, root, "src/util.ts", `export function findInstancesOf(template) {
  if (template.shaclIn) {
    return template.config.lists[template.shaclIn]
  }
  return []
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := InspectTerm(idx, "sh:in", InspectTermOptions{
		BudgetTokens: 500,
		ContextLines: 3,
		Mode:         ModeEvidence,
		Limit:        6,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != "ok" || resp.Term == nil || resp.Term.Term != "sh:in" {
		t.Fatalf("inspect-term status/term = %#v", resp)
	}
	joined := ""
	for _, hit := range resp.Implementation {
		joined += hit.File + "\n" + hit.Text + "\n"
	}
	if !strings.Contains(joined, "property-template.ts") || !strings.Contains(joined, "util.ts") {
		t.Fatalf("expected implementation hints for direct ref and shaclIn consumers, got:\n%s", joined)
	}
	if resp.EstimatedTokens > 500 {
		t.Fatalf("inspect-term exceeded budget: %#v", resp)
	}
}

// TestTraceTermAmbiguousRanksWellFormedAndWarns guards Phase-1 issue 6: when
// the same prefix:local resolves to both a well-formed (#local) and a
// malformed (concatenated) IRI, ambiguous status is preserved (technically
// honest) but the well-formed candidate ranks first and a warning surfaces
// the malformed prefix declaration so agents can act on it.
func TestTraceTermAmbiguousRanksWellFormedAndWarns(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a/hash.ttl", `@prefix custom: <http://example.org/ns/custom#> .
@prefix ex: <http://example.org/> .
@prefix sh: <http://www.w3.org/ns/shacl#> .

ex:HashShape a sh:NodeShape ; custom:hideIf ex:Cond .
`)
	write(t, root, "b/nohash.ttl", `@prefix custom: <http://example.org/ns/custom> .
@prefix ex: <http://example.org/> .
@prefix sh: <http://www.w3.org/ns/shacl#> .

ex:NoHashShape a sh:NodeShape ; custom:hideIf ex:Cond .
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	got := TraceTerm(idx, "custom:hideIf")
	if got.Status != "ambiguous" || len(got.Candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %#v", got)
	}
	if got.Candidates[0].IRI != "http://example.org/ns/custom#hideIf" {
		t.Fatalf("expected #hideIf as canonical first candidate, got %#v", got.Candidates)
	}
	if got.Candidates[1].IRI != "http://example.org/ns/customhideIf" {
		t.Fatalf("expected customhideIf last, got %#v", got.Candidates)
	}
	if len(got.Warnings) == 0 {
		t.Fatalf("expected a malformed-IRI warning, got none")
	}
	joined := strings.Join(got.Warnings, "|")
	if !strings.Contains(joined, "customhideIf") || !strings.Contains(joined, "missing-separator") {
		t.Fatalf("warning should call out the malformed customhideIf IRI, got %q", joined)
	}
	compact := TraceTermCompact(idx, "custom:hideIf")
	if len(compact.Warnings) == 0 {
		t.Fatalf("compact response should propagate warnings, got %#v", compact)
	}
}

// Phase 2 — tail evidence anchors.
//
// TestFetchContextEmitsTailAnchorForLongFunctionBody guards the Phase-2
// fix: when a function body is long enough that the head clamp would lose
// the trailing return / .filter() / closing brace evidence, FetchContext
// must emit a second slice with reason "tail anchor" focused on the last
// return-bearing line. Long filtering helpers previously had their
// `.filter(...)` lines clipped.
func TestFetchContextEmitsTailAnchorForLongFunctionBody(t *testing.T) {
	root := t.TempDir()
	var b strings.Builder
	b.WriteString("export function useSidebarModules(modules: any[], auth: any) {\n")
	b.WriteString("  const items: any[] = []\n")
	for i := 0; i < 90; i++ {
		b.WriteString("  // padding line to make this a long function body so head clamp matters\n")
	}
	b.WriteString("  return modules\n")
	b.WriteString("    .filter(m => m.available)\n")
	b.WriteString("    .filter(m => !m.requiresLogin || auth.authenticated)\n")
	b.WriteString("    .map(m => ({ ...m, ready: true }))\n")
	b.WriteString("}\n")
	write(t, root, "src/useSidebarModules.ts", b.String())
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := FetchContext(idx, "useSidebarModules", FetchContextOptions{
		BudgetTokens: 600,
		ContextLines: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != "ok" {
		t.Fatalf("status = %s warnings=%v", resp.Status, resp.Warnings)
	}
	hasTail := false
	hasTarget := false
	for _, slice := range resp.Slices {
		switch slice.Reason {
		case "tail anchor":
			hasTail = true
			if !strings.Contains(slice.Text, "filter") {
				t.Fatalf("tail anchor should include the .filter(...) lines, got %q", slice.Text)
			}
		case "target symbol":
			hasTarget = true
		}
	}
	if !hasTarget {
		t.Fatalf("expected target symbol slice, got %#v", resp.Slices)
	}
	if !hasTail {
		t.Fatalf("expected tail anchor slice for long body, got %#v", resp.Slices)
	}
}

// TestFetchContextSkipsTailAnchorForShortFunction guards that the tail
// anchor is *not* emitted for small bodies — adding a redundant slice for
// every symbol would waste tokens on the common case.
func TestFetchContextSkipsTailAnchorForShortFunction(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/short.ts", `export function tiny() {
  return 1
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := FetchContext(idx, "tiny", FetchContextOptions{BudgetTokens: 600, ContextLines: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, slice := range resp.Slices {
		if slice.Reason == "tail anchor" {
			t.Fatalf("tail anchor leaked into short function: %#v", resp.Slices)
		}
	}
}

// Phase 3 — nested function symbols.
//
// TestNestedFunctionsInComposableEmittedWithParentID guards that
// composable-local helpers now appear
// as first-class symbols with ParentID set to the enclosing function. The
// SCIP-style qualified ID `useDistributions.downloadDataset` survives
// later lookups via inspect-symbol.
func TestNestedFunctionsInComposableEmittedWithParentID(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/useDistributions.ts", "export function useDistributions() {\n"+
		"  const downloadDataset = async (datasetId: string, format: string) => {\n"+
		"    const blob = await fetch('/x')\n"+
		"    const url = URL.createObjectURL(blob)\n"+
		"    const link = document.createElement('a')\n"+
		"    link.download = `dataset-${datasetId}.${getFileExtension(format)}`\n"+
		"    link.click()\n"+
		"  }\n"+
		"  function getFileExtension(format: string) {\n"+
		"    return format === 'csv' ? 'csv' : 'json'\n"+
		"  }\n"+
		"  return { downloadDataset, getFileExtension }\n"+
		"}\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	parent := findSymbolByName(idx, "useDistributions")
	if parent.ID == "" {
		t.Fatal("expected useDistributions to be indexed")
	}
	download := findSymbolByName(idx, "downloadDataset")
	if download.ID == "" {
		t.Fatal("expected nested downloadDataset to be indexed")
	}
	if download.ParentID != parent.ID {
		t.Fatalf("downloadDataset.ParentID = %q, want %q", download.ParentID, parent.ID)
	}
	if !strings.Contains(download.ID, "useDistributions.downloadDataset") {
		t.Fatalf("expected SCIP-style qualified id, got %q", download.ID)
	}
	getExt := findSymbolByName(idx, "getFileExtension")
	if getExt.ID == "" {
		t.Fatal("expected nested getFileExtension to be indexed")
	}
	if getExt.ParentID != parent.ID {
		t.Fatalf("getFileExtension.ParentID = %q, want %q", getExt.ParentID, parent.ID)
	}
	// inspect-symbol on the nested name must now resolve directly.
	resp, err := InspectSymbol(idx, "downloadDataset", InspectSymbolOptions{BudgetTokens: 800})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != "ok" || resp.Symbol == nil {
		t.Fatalf("inspect-symbol should resolve nested downloadDataset, got %#v", resp)
	}
}

// TestNestedFunctionDoesNotShadowCallAttribution guards a regression: a
// nested constant initializer like `const data = await fetchData()` must
// NOT become the deepest symbol on the call's line — the call belongs to
// the enclosing function, not the const.
func TestNestedFunctionDoesNotShadowCallAttribution(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/store.ts", `export class Store {
  async load() {
    const data = await fetchData()
    return data
  }
}
function fetchData() { return 1 }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	trace := TraceSymbolWithOptions(idx, "fetchData", TraceSymbolOptions{Sites: true})
	if len(trace.Callers) == 0 {
		t.Fatalf("expected a caller for fetchData, got %#v", trace)
	}
	loadSeen := false
	for _, c := range trace.Callers {
		if c.Name == "load" {
			loadSeen = true
			break
		}
	}
	if !loadSeen {
		t.Fatalf("expected load() as caller of fetchData, got %#v", trace.Callers)
	}
}

// Phase 4 — event-flow tracing.
//
// TestTraceEventPairsEmitAndListenSites guards the Phase-4 fix: when one
// function emits an event and another listens, trace-event must return
// both attributed to their containing symbols. Confidence is "exact" for
// string-literal keys and "scoped" for dotted constant paths.
func TestTraceEventPairsEmitAndListenSites(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/events.ts", `export const APP_EVENTS = { EMAIL_QUEUE_ADD: 'EMAIL_QUEUE_ADD' as const }
export const appEvents = { emit: (k: string, p: any) => {}, on: (k: string, cb: any) => {}, off: (k: string, cb: any) => {} }
`)
	write(t, root, "src/handler.ts", `import { appEvents, APP_EVENTS } from './events'
export function handleApplicationClosed(reason: string) {
  if (reason === 'rejected') {
    appEvents.emit('EMAIL_QUEUE_ADD', { template: 'rejection' })
  }
}
export function initializeApplicationEventSubscribers() {
  appEvents.on('EMAIL_QUEUE_ADD', payload => sendEmail(payload))
}
function sendEmail(p: any) { return p }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := TraceEvent(idx, "EMAIL_QUEUE_ADD")
	if resp.Status != "found" {
		t.Fatalf("status = %s warnings=%v", resp.Status, resp.Warnings)
	}
	if len(resp.Emits) != 1 {
		t.Fatalf("expected 1 emit site, got %d (%#v)", len(resp.Emits), resp.Emits)
	}
	if resp.Emits[0].SymbolName != "handleApplicationClosed" {
		t.Fatalf("emit site should be attributed to handleApplicationClosed, got %#v", resp.Emits[0])
	}
	if resp.Emits[0].Confidence != ConfExact {
		t.Fatalf("string-literal emit confidence should be exact, got %q", resp.Emits[0].Confidence)
	}
	if len(resp.Listens) != 1 {
		t.Fatalf("expected 1 listen site, got %d (%#v)", len(resp.Listens), resp.Listens)
	}
	if resp.Listens[0].SymbolName != "initializeApplicationEventSubscribers" {
		t.Fatalf("listen site should be attributed to initializeApplicationEventSubscribers, got %#v", resp.Listens[0])
	}
}

// TestTraceEventDottedConstantPath guards that emits and listens that use
// `APP_EVENTS.FOO` rather than a string literal still pair correctly when
// both sides use the same constant path. Confidence is "scoped" to signal
// that the agent should verify the constant's runtime value if it cares.
func TestTraceEventDottedConstantPath(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/events.ts", `export const APP_EVENTS = { CLOSE: 'close' }
export const bus: any = {}
export function fireClose() { bus.emit(APP_EVENTS.CLOSE) }
export function watchClose() { bus.on(APP_EVENTS.CLOSE, () => 1) }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := TraceEvent(idx, "APP_EVENTS.CLOSE")
	if resp.Status != "found" {
		t.Fatalf("status = %s warnings=%v", resp.Status, resp.Warnings)
	}
	if len(resp.Emits) != 1 || resp.Emits[0].Confidence != ConfScoped {
		t.Fatalf("expected one scoped emit site, got %#v", resp.Emits)
	}
	if len(resp.Listens) != 1 || resp.Listens[0].Confidence != ConfScoped {
		t.Fatalf("expected one scoped listen site, got %#v", resp.Listens)
	}
	bare := TraceEvent(idx, "CLOSE")
	if len(bare.Candidates) == 0 {
		t.Fatalf("bare key should surface APP_EVENTS.CLOSE as a candidate, got %#v", bare)
	}
}

// TestTraceEventDoesNotMatchUnrelatedDotMethods guards that we do not
// false-positive on patterns like `array.find(...)` or `req.match(...)` —
// only the four event-emitter method names are considered.
func TestTraceEventDoesNotMatchUnrelatedDotMethods(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/noise.ts", `export function noise(items: any[]) {
  return items.find(x => x === 'EMAIL_QUEUE_ADD')
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := TraceEvent(idx, "EMAIL_QUEUE_ADD")
	if resp.Status == "found" {
		t.Fatalf("array.find should not produce an event edge, got %#v", resp)
	}
}

// TestListEventsAggregatesSiteCounts guards that ListEvents returns each
// distinct event key with site counts, sorted by total descending.
func TestListEventsAggregatesSiteCounts(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/bus.ts", `export const bus: any = {}
export function a() { bus.emit('FOO') }
export function b() { bus.emit('FOO') }
export function c() { bus.on('FOO', () => 1) }
export function d() { bus.emit('BAR') }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := ListEvents(idx)
	if resp.Status != "ok" || len(resp.Events) < 2 {
		t.Fatalf("expected at least 2 events, got %#v", resp)
	}
	if resp.Events[0].Event != "FOO" || resp.Events[0].TotalSites != 3 {
		t.Fatalf("expected FOO with 3 sites first, got %#v", resp.Events[0])
	}
	if resp.Events[0].EmitCount != 2 || resp.Events[0].ListenCount != 1 {
		t.Fatalf("expected 2 emits + 1 listen for FOO, got %#v", resp.Events[0])
	}
}

// TestPythonSymbolsTreeSitterNestingAndDecorators guards the tree-sitter
// Python symbol path: nested class -> method gets ParentID/QualifiedName
// from the containment chain, decorators are folded into the def's range
// and signature, and two classes that each define a same-named method get
// distinct IDs disambiguated by their qualified names.
func TestPythonSymbolsTreeSitterNestingAndDecorators(t *testing.T) {
	root := t.TempDir()
	write(t, root, "pkg/models.py", `class Outer:
    class Inner:
        def method(self):
            return 1

    @staticmethod
    def helper():
        return 2


class A:
    def process(self):
        return "a"


class B:
    def process(self):
        return "b"
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	outer := findSymbolByName(idx, "Outer")
	if outer.ID == "" || outer.Kind != "class" {
		t.Fatalf("expected Outer class symbol, got %#v", outer)
	}

	var inner, method, helper CGPSymbol
	idx.mu.Lock()
	for _, sym := range idx.Symbols {
		switch {
		case sym.File == "pkg/models.py" && sym.Name == "Inner":
			inner = sym
		case sym.File == "pkg/models.py" && sym.Name == "method":
			method = sym
		case sym.File == "pkg/models.py" && sym.Name == "helper":
			helper = sym
		}
	}
	idx.mu.Unlock()

	if inner.ID == "" || inner.Kind != "class" || inner.ParentID != outer.ID {
		t.Fatalf("expected Inner class with ParentID=Outer, got %#v (outer=%q)", inner, outer.ID)
	}
	if method.ID == "" || method.Kind != "method" || method.ParentID != inner.ID {
		t.Fatalf("expected method with Kind=method and ParentID=Inner, got %#v (inner=%q)", method, inner.ID)
	}
	if !strings.Contains(method.ID, "Outer.Inner.method") {
		t.Fatalf("expected qualified id for method, got %q", method.ID)
	}

	if helper.ID == "" || helper.Kind != "method" || helper.ParentID != outer.ID {
		t.Fatalf("expected decorated helper with Kind=method and ParentID=Outer, got %#v (outer=%q)", helper, outer.ID)
	}
	if !strings.HasPrefix(helper.Signature, "@staticmethod") {
		t.Fatalf("expected decorator-inclusive signature, got %q", helper.Signature)
	}

	// Two classes defining same-named "process" methods must resolve to
	// distinct symbols with distinct qualified ids.
	var processIDs []string
	var processParents []string
	idx.mu.Lock()
	for _, sym := range idx.Symbols {
		if sym.File == "pkg/models.py" && sym.Name == "process" {
			processIDs = append(processIDs, sym.ID)
			processParents = append(processParents, sym.ParentID)
		}
	}
	idx.mu.Unlock()
	if len(processIDs) != 2 {
		t.Fatalf("expected 2 'process' methods, got %d: %v", len(processIDs), processIDs)
	}
	if processIDs[0] == processIDs[1] {
		t.Fatalf("expected distinct ids for A.process and B.process, got %v", processIDs)
	}
	if processParents[0] == processParents[1] {
		t.Fatalf("expected distinct ParentID for A.process and B.process, got %v", processParents)
	}
}

// TestPythonModuleLevelCallsAttributeToFile guards a common Python idiom:
// calls made at module scope (outside any class/function), such as
// `if __name__ == "__main__": main()`, must not be silently dropped just
// because containingSymbolFast finds no enclosing def — they attribute to
// the file symbol, mirroring decorator-call attribution.
func TestPythonModuleLevelCallsAttributeToFile(t *testing.T) {
	root := t.TempDir()
	write(t, root, "pkg/main.py", `def main():
    return 1


if __name__ == "__main__":
    main()
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	fileSym := fileSymbolID("pkg/main.py")
	mainSym := findSymbolByName(idx, "main")
	if mainSym.ID == "" {
		t.Fatalf("expected main symbol")
	}

	found := false
	for _, e := range idx.SymbolEdges {
		if e.Type == "calls" && e.Evidence.Raw == "main" && e.Evidence.File == "pkg/main.py" {
			if e.From != fileSym || e.To != mainSym.ID {
				t.Fatalf("expected module-level main() call from file %q to %q, got %#v", fileSym, mainSym.ID, e)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a call edge for module-level main(), got edges=%#v", idx.SymbolEdges)
	}
}

// TestPythonBuiltinCallsNotEmittedAsEdges guards against noise from calls to
// Python builtins (print, len, isinstance, super(), etc.) with no receiver:
// these never resolve to anything in the repo, so they must not produce
// "calls" edges (previously these dominated doctor's topUnresolved with
// unresolved:print/unresolved:len/etc.).
func TestPythonBuiltinCallsNotEmittedAsEdges(t *testing.T) {
	root := t.TempDir()
	write(t, root, "pkg/main.py", `class Base:
    def save(self):
        print("saving")


class Account(Base):
    def save(self):
        items = [str(x) for x in range(len(self.items))]
        if isinstance(self, Base):
            super().save()
        return items
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range idx.SymbolEdges {
		if e.Type != "calls" {
			continue
		}
		for _, builtin := range []string{"print", "str", "range", "len", "isinstance"} {
			if e.Evidence.Raw == builtin {
				t.Fatalf("did not expect a calls edge for builtin %q, got %#v", builtin, e)
			}
		}
	}
}

// TestPythonRelationsTreeSitterScopedCallsImportsAndDecorators guards the
// tree-sitter Python relations path: self.process() inside two classes that
// each define process() resolves to that class's own method (ConfScoped,
// not an ambiguous/unresolved guess); all import forms (plain, aliased,
// relative, bare-dot, dotted-relative) produce import edges; and a
// decorator call expression (e.g. @app.route("/x")) attributes to the
// file/module scope rather than the decorated function.
func TestPythonRelationsTreeSitterScopedCallsImportsAndDecorators(t *testing.T) {
	root := t.TempDir()
	write(t, root, "pkg/models.py", `class A:
    def process(self):
        return "a"

    def run(self):
        return self.process()


class B:
    def process(self):
        return "b"

    def run(self):
        return self.process()
`)
	write(t, root, "pkg/imports.py", `import os
import os.path as osp
from collections import OrderedDict
from .models import User
from . import db
from ..pkg.sub import Thing as T
`)
	write(t, root, "pkg/routes.py", `import app


@app.route("/x")
def view():
    return "ok"
`)

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	var aProcess, bProcess, aRun, bRun CGPSymbol
	idx.mu.Lock()
	for _, sym := range idx.Symbols {
		if sym.File != "pkg/models.py" {
			continue
		}
		switch {
		case sym.Name == "process" && strings.Contains(sym.ID, "A.process"):
			aProcess = sym
		case sym.Name == "process" && strings.Contains(sym.ID, "B.process"):
			bProcess = sym
		case sym.Name == "run" && strings.Contains(sym.ID, "A.run"):
			aRun = sym
		case sym.Name == "run" && strings.Contains(sym.ID, "B.run"):
			bRun = sym
		}
	}
	idx.mu.Unlock()
	if aProcess.ID == "" || bProcess.ID == "" || aRun.ID == "" || bRun.ID == "" {
		t.Fatalf("expected A/B process/run symbols, got aProcess=%#v bProcess=%#v aRun=%#v bRun=%#v", aProcess, bProcess, aRun, bRun)
	}

	foundARun, foundBRun := false, false
	for _, e := range idx.SymbolEdges {
		if e.Type != "calls" {
			continue
		}
		switch e.From {
		case aRun.ID:
			if e.To != aProcess.ID || e.Confidence != ConfScoped {
				t.Fatalf("expected A.run -> A.process (scoped), got %#v", e)
			}
			foundARun = true
		case bRun.ID:
			if e.To != bProcess.ID || e.Confidence != ConfScoped {
				t.Fatalf("expected B.run -> B.process (scoped), got %#v", e)
			}
			foundBRun = true
		}
	}
	if !foundARun || !foundBRun {
		t.Fatalf("expected self.process() call edges from both A.run and B.run, edges=%#v", idx.SymbolEdges)
	}

	wantImports := map[string]bool{
		"module:os":          false,
		"module:os.path":     false,
		"module:collections": false,
		"module:.models":     false,
		"module:.":           false,
		"module:..pkg.sub":   false,
	}
	for _, e := range idx.SymbolEdges {
		if e.Type != "imports" || e.Evidence.File != "pkg/imports.py" {
			continue
		}
		if _, ok := wantImports[e.To]; ok {
			wantImports[e.To] = true
		}
	}
	for spec, found := range wantImports {
		if !found {
			t.Fatalf("expected import edge to %s, got edges=%#v", spec, idx.SymbolEdges)
		}
	}

	view := findSymbolByName(idx, "view")
	if view.ID == "" {
		t.Fatalf("expected view symbol")
	}
	fileSym := fileSymbolID("pkg/routes.py")
	foundDecoratorEdge := false
	for _, e := range idx.SymbolEdges {
		if e.Type != "calls" || e.Evidence.File != "pkg/routes.py" {
			continue
		}
		if e.Evidence.Raw == "app.route" {
			if e.From != fileSym {
				t.Fatalf("expected decorator call attributed to file scope %q, got From=%q", fileSym, e.From)
			}
			if e.From == view.ID {
				t.Fatalf("decorator call must not attribute to the decorated function")
			}
			foundDecoratorEdge = true
		}
	}
	if !foundDecoratorEdge {
		t.Fatalf("expected app.route call edge, got edges=%#v", idx.SymbolEdges)
	}
}

// TestRequireSingletonMethodCallResolvesCrossFile covers a CommonJS singleton
// (`module.exports = { x: new X() }` / `module.exports = new X()`) accessed
// through a `require()`-bound local, where the *same method name* is
// ambiguous repo-wide (defined in two different files). Before the fix,
// resolveSymbolCall's bare-name search saw 2 global candidates and gave up
// (`unresolved`, ReasonAmbiguousName), so neither caller edge was ever
// created — trace-symbol on either definition returned `callers: []`. The
// fix (jsRequireBindingFiles + resolveImportBoundCall in cgp.go, require()
// parsing in jsparse.go) must resolve each call to the right file using the
// require() binding instead of falling through to the ambiguous global
// search.
func TestRequireSingletonMethodCallResolvesCrossFile(t *testing.T) {
	root := t.TempDir()
	write(t, root, "services/notificationService.js", `class NotificationService {
  async sendEmail(content) {
    return content
  }
}

module.exports = {
  NotificationService,
  notificationService: new NotificationService()
}
`)
	write(t, root, "services/emailHandler.js", `class EmailHandler {
  async sendEmail(data) {
    return data
  }
}

module.exports = new EmailHandler()
`)
	write(t, root, "callers.js", `const { notificationService } = require('./services/notificationService')
const emailHandler = require('./services/emailHandler')

async function notify() {
  await notificationService.sendEmail('hi')
}

async function handle() {
  await emailHandler.sendEmail({})
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	findInFile := func(name, file string) CGPSymbol {
		idx.mu.Lock()
		defer idx.mu.Unlock()
		for _, sym := range idx.Symbols {
			if sym.Name == name && sym.File == file {
				return sym
			}
		}
		return CGPSymbol{}
	}
	hasCaller := func(callers []CGPSymbolSummary, name string) bool {
		for _, c := range callers {
			if c.Name == name {
				return true
			}
		}
		return false
	}

	notifSend := findInFile("sendEmail", "services/notificationService.js")
	if notifSend.ID == "" {
		t.Fatalf("expected NotificationService.sendEmail symbol")
	}
	notifTrace := TraceSymbol(idx, notifSend.ID)
	if notifTrace.Status != "found" {
		t.Fatalf("expected found status, got %#v", notifTrace)
	}
	if !hasCaller(notifTrace.Callers, "notify") {
		t.Fatalf("expected notify() as caller of notificationService.sendEmail via require() singleton, got callers=%#v", notifTrace.Callers)
	}
	if hasCaller(notifTrace.Callers, "handle") {
		t.Fatalf("handle() must not be attributed to notificationService.sendEmail — it calls the other (emailHandler) sendEmail")
	}

	handlerSend := findInFile("sendEmail", "services/emailHandler.js")
	if handlerSend.ID == "" {
		t.Fatalf("expected EmailHandler.sendEmail symbol")
	}
	handlerTrace := TraceSymbol(idx, handlerSend.ID)
	if handlerTrace.Status != "found" {
		t.Fatalf("expected found status, got %#v", handlerTrace)
	}
	if !hasCaller(handlerTrace.Callers, "handle") {
		t.Fatalf("expected handle() as caller of emailHandler.sendEmail via require() singleton, got callers=%#v", handlerTrace.Callers)
	}
	if hasCaller(handlerTrace.Callers, "notify") {
		t.Fatalf("notify() must not be attributed to emailHandler.sendEmail — it calls the other (notificationService) sendEmail")
	}

	// The ambiguous bare-name lookup must still report both candidates —
	// the fix must not collapse genuine ambiguity, only resolve it when a
	// require()-bound receiver disambiguates which file is meant.
	ambiguous := TraceSymbol(idx, "sendEmail")
	if ambiguous.Status != "ambiguous" || len(ambiguous.Candidates) != 2 {
		t.Fatalf("expected bare 'sendEmail' lookup to remain ambiguous with 2 candidates, got %#v", ambiguous)
	}

	// Inline candidate-detail expansion: a small ambiguous candidate set
	// should carry each candidate's full caller/callee trace in the same
	// response, so an agent can answer "who calls this ambiguous name" in
	// one round trip instead of disambiguating and re-querying per
	// candidate.
	if len(ambiguous.CandidateDetails) != 2 {
		t.Fatalf("expected 2 inline candidate details, got %#v", ambiguous.CandidateDetails)
	}
	var notifDetail, handlerDetail *TraceSymbolCandidateDetail
	for i := range ambiguous.CandidateDetails {
		d := &ambiguous.CandidateDetails[i]
		if d.Symbol == nil {
			t.Fatalf("candidate detail missing symbol: %#v", d)
		}
		switch d.Symbol.File {
		case "services/notificationService.js":
			notifDetail = d
		case "services/emailHandler.js":
			handlerDetail = d
		}
	}
	if notifDetail == nil || handlerDetail == nil {
		t.Fatalf("expected one candidate detail per file, got %#v", ambiguous.CandidateDetails)
	}
	if !hasCaller(notifDetail.Callers, "notify") || hasCaller(notifDetail.Callers, "handle") {
		t.Fatalf("notificationService.sendEmail candidate detail has wrong callers: %#v", notifDetail.Callers)
	}
	if !hasCaller(handlerDetail.Callers, "handle") || hasCaller(handlerDetail.Callers, "notify") {
		t.Fatalf("emailHandler.sendEmail candidate detail has wrong callers: %#v", handlerDetail.Callers)
	}

	// find-references must surface the same inline expansion.
	fr := FindReferences(idx, "sendEmail")
	if fr.Status != "ambiguous" || len(fr.SymbolCandidateDetails) != 2 {
		t.Fatalf("expected find-references to mirror trace-symbol's inline candidate details, got %#v", fr)
	}

	// inspect-symbol (both response formats) must mirror the same inline
	// expansion instead of forcing a second disambiguate-then-trace round
	// trip.
	is, err := InspectSymbol(idx, "sendEmail", InspectSymbolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if is.Status != "ambiguous" || len(is.CandidateDetails) != 2 {
		t.Fatalf("expected inspect-symbol to mirror trace-symbol's inline candidate details, got %#v", is)
	}

	isNode, err := InspectSymbolNode(idx, "sendEmail", InspectSymbolNodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if isNode.Status != "ambiguous" || len(isNode.CandidateDetails) != 2 {
		t.Fatalf("expected inspect-symbol-node to mirror trace-symbol's inline candidate details, got %#v", isNode)
	}
}

func TestJSTSReturnValueMethodCallAssignmentResolvesCrossFile(t *testing.T) {
	root := t.TempDir()
	write(t, root, "services/notificationService.js", `class NotificationService {
  async sendEmail(content) {
    return content
  }
}

module.exports = {
  notificationService: new NotificationService()
}
`)
	write(t, root, "services/email/emailHandler.js", `class EmailHandler {
  async sendEmail(data) {
    return data
  }
}

module.exports = new EmailHandler()
`)
	write(t, root, "services/email/EmailQueueService.js", `class EmailQueueService {
  constructor() {
    this.emailHandler = null
  }

  getEmailHandler() {
    if (!this.emailHandler) {
      this.emailHandler = require('./emailHandler')
    }
    return this.emailHandler
  }

  async approveEmail(emailData) {
    const emailHandler = this.getEmailHandler()
    return emailHandler.sendEmail(emailData)
  }
}

module.exports = new EmailQueueService()
`)
	write(t, root, "jobs/notifier.js", `const { notificationService } = require('../services/notificationService')

async function notify() {
  return notificationService.sendEmail('hi')
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	findInFile := func(name, file string) CGPSymbol {
		idx.mu.Lock()
		defer idx.mu.Unlock()
		for _, sym := range idx.Symbols {
			if sym.Name == name && sym.File == file {
				return sym
			}
		}
		return CGPSymbol{}
	}
	hasCaller := func(callers []CGPSymbolSummary, name string) bool {
		for _, c := range callers {
			if c.Name == name {
				return true
			}
		}
		return false
	}

	handlerSend := findInFile("sendEmail", "services/email/emailHandler.js")
	if handlerSend.ID == "" {
		t.Fatalf("expected EmailHandler.sendEmail symbol")
	}
	handlerTrace := TraceSymbol(idx, handlerSend.ID)
	if handlerTrace.Status != "found" {
		t.Fatalf("expected found status, got %#v", handlerTrace)
	}
	if !hasCaller(handlerTrace.Callers, "approveEmail") {
		t.Fatalf("expected approveEmail() as caller through getEmailHandler return inference, got callers=%#v", handlerTrace.Callers)
	}
	if hasCaller(handlerTrace.Callers, "notify") {
		t.Fatalf("notify() must not be attributed to emailHandler.sendEmail")
	}

	notifSend := findInFile("sendEmail", "services/notificationService.js")
	if notifSend.ID == "" {
		t.Fatalf("expected NotificationService.sendEmail symbol")
	}
	notifTrace := TraceSymbol(idx, notifSend.ID)
	if !hasCaller(notifTrace.Callers, "notify") {
		t.Fatalf("expected notify() as caller of notificationService.sendEmail, got callers=%#v", notifTrace.Callers)
	}
	if hasCaller(notifTrace.Callers, "approveEmail") {
		t.Fatalf("approveEmail() must not be attributed to notificationService.sendEmail")
	}

	ambiguous := TraceSymbol(idx, "sendEmail")
	if ambiguous.Status != "ambiguous" || len(ambiguous.CandidateDetails) != 2 {
		t.Fatalf("expected ambiguous sendEmail with inline candidate details, got %#v", ambiguous)
	}
}

// TestJSTSLocalNewInstanceMethodCallResolvesCrossFile covers the gap closed
// by jsNewInstanceAssignmentAt/jsNewInstanceClassAt: `const repo = new
// UserRepository()` (UserRepository imported) followed by `repo.findById()`
// previously produced no binding at all — jsCallAssignmentAt only matched
// dotted-call assignments, not `new` expressions — so the call fell through
// to the repo-wide ambiguous bare-name search. This is arguably more common
// than the already-fixed require()-singleton pattern.
func TestJSTSLocalNewInstanceMethodCallResolvesCrossFile(t *testing.T) {
	root := t.TempDir()
	write(t, root, "repos/userRepository.js", `class UserRepository {
  findById(id) {
    return id
  }
}

module.exports = { UserRepository }
`)
	write(t, root, "repos/otherRepository.js", `class OtherRepository {
  findById(id) {
    return id
  }
}

module.exports = { OtherRepository }
`)
	write(t, root, "controllers/userController.js", `const { UserRepository } = require('../repos/userRepository')

function getUser(id) {
  const repo = new UserRepository()
  return repo.findById(id)
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	findInFile := func(name, file string) CGPSymbol {
		idx.mu.Lock()
		defer idx.mu.Unlock()
		for _, sym := range idx.Symbols {
			if sym.Name == name && sym.File == file {
				return sym
			}
		}
		return CGPSymbol{}
	}
	hasCaller := func(callers []CGPSymbolSummary, name string) bool {
		for _, c := range callers {
			if c.Name == name {
				return true
			}
		}
		return false
	}

	userFindByID := findInFile("findById", "repos/userRepository.js")
	if userFindByID.ID == "" {
		t.Fatalf("expected UserRepository.findById symbol")
	}
	trace := TraceSymbol(idx, userFindByID.ID)
	if trace.Status != "found" {
		t.Fatalf("expected found status, got %#v", trace)
	}
	if !hasCaller(trace.Callers, "getUser") {
		t.Fatalf("expected getUser() as caller of UserRepository.findById via local `new` instantiation, got callers=%#v", trace.Callers)
	}

	// The ambiguous bare-name lookup must still report both candidates (this
	// fix must not collapse genuine ambiguity, only resolve it when a local
	// `new`-bound receiver disambiguates which file is meant), and the
	// unrelated OtherRepository.findById must not gain a caller from this.
	ambiguous := TraceSymbol(idx, "findById")
	if ambiguous.Status != "ambiguous" || len(ambiguous.Candidates) != 2 {
		t.Fatalf("expected bare 'findById' lookup to remain ambiguous with 2 candidates, got %#v", ambiguous)
	}
	otherFindByID := findInFile("findById", "repos/otherRepository.js")
	if otherFindByID.ID == "" {
		t.Fatalf("expected OtherRepository.findById symbol")
	}
	otherTrace := TraceSymbol(idx, otherFindByID.ID)
	if hasCaller(otherTrace.Callers, "getUser") {
		t.Fatalf("getUser() must not be attributed to OtherRepository.findById")
	}
}

// TestJSTSDirectReturnNewInstanceMethodCallResolvesCrossFile covers the
// direct-return shape: a factory function returning `new ClassName()`
// inline (no intermediate local variable), then immediately calling a
// method on the factory's result.
func TestJSTSDirectReturnNewInstanceMethodCallResolvesCrossFile(t *testing.T) {
	root := t.TempDir()
	write(t, root, "repos/userRepository.js", `class UserRepository {
  findById(id) {
    return id
  }
}

module.exports = { UserRepository }
`)
	write(t, root, "controllers/userController.js", `const { UserRepository } = require('../repos/userRepository')

function getRepo() {
  return new UserRepository()
}

function getUser(id) {
  return getRepo().findById(id)
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	userFindByID := findSymbolByName(idx, "findById")
	if userFindByID.ID == "" {
		t.Fatalf("expected findById symbol")
	}
	trace := TraceSymbol(idx, userFindByID.ID)
	if trace.Status != "found" {
		t.Fatalf("expected found status, got %#v", trace)
	}
	found := false
	for _, c := range trace.Callers {
		if c.Name == "getUser" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected getUser() as caller via getRepo()'s direct `return new UserRepository()`, got callers=%#v", trace.Callers)
	}
}

// TestJSTSReturnOfLocalNewInstanceResolvesCrossFile covers the one-hop
// recursive case: `const repo = new ClassName(); return repo` — the
// factory's return value is traced back through the local `new` binding,
// not just a direct inline `return new ClassName()`.
func TestJSTSReturnOfLocalNewInstanceResolvesCrossFile(t *testing.T) {
	root := t.TempDir()
	write(t, root, "repos/userRepository.js", `class UserRepository {
  findById(id) {
    return id
  }
}

module.exports = { UserRepository }
`)
	write(t, root, "controllers/userController.js", `const { UserRepository } = require('../repos/userRepository')

function getRepo() {
  const repo = new UserRepository()
  return repo
}

function getUser(id) {
  return getRepo().findById(id)
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	userFindByID := findSymbolByName(idx, "findById")
	if userFindByID.ID == "" {
		t.Fatalf("expected findById symbol")
	}
	trace := TraceSymbol(idx, userFindByID.ID)
	if trace.Status != "found" {
		t.Fatalf("expected found status, got %#v", trace)
	}
	found := false
	for _, c := range trace.Callers {
		if c.Name == "getUser" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected getUser() as caller via getRepo()'s `const repo = new UserRepository(); return repo`, got callers=%#v", trace.Callers)
	}
}

// TestJSTSNewInstanceReassignmentResolvesToLatestClass covers correctness
// under reassignment: `let x = new A(); x = new B(); x.method()` must
// resolve to B's file, not A's — jsAssignedReturnFile's existing
// "latest assignment before the call site wins" semantics, exercised here
// for the new `new`-instance binding source rather than a new mechanism.
func TestJSTSNewInstanceReassignmentResolvesToLatestClass(t *testing.T) {
	root := t.TempDir()
	write(t, root, "repos/a.js", `class A {
  run() {
    return 1
  }
}

module.exports = { A }
`)
	write(t, root, "repos/b.js", `class B {
  run() {
    return 2
  }
}

module.exports = { B }
`)
	write(t, root, "main.js", `const { A } = require('./repos/a')
const { B } = require('./repos/b')

function pick(useB) {
  let x = new A()
  if (useB) {
    x = new B()
  }
  return x.run()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	findInFile := func(name, file string) CGPSymbol {
		idx.mu.Lock()
		defer idx.mu.Unlock()
		for _, sym := range idx.Symbols {
			if sym.Name == name && sym.File == file {
				return sym
			}
		}
		return CGPSymbol{}
	}
	hasCaller := func(callers []CGPSymbolSummary, name string) bool {
		for _, c := range callers {
			if c.Name == name {
				return true
			}
		}
		return false
	}

	bRun := findInFile("run", "repos/b.js")
	if bRun.ID == "" {
		t.Fatalf("expected B.run symbol")
	}
	bTrace := TraceSymbol(idx, bRun.ID)
	if !hasCaller(bTrace.Callers, "pick") {
		t.Fatalf("expected pick() to resolve x.run() to B.run (the reassigned value), got callers=%#v", bTrace.Callers)
	}

	aRun := findInFile("run", "repos/a.js")
	if aRun.ID == "" {
		t.Fatalf("expected A.run symbol")
	}
	aTrace := TraceSymbol(idx, aRun.ID)
	if hasCaller(aTrace.Callers, "pick") {
		t.Fatalf("pick() must not be attributed to A.run after x was reassigned to new B(), got callers=%#v", aTrace.Callers)
	}
}

// TestJSTSLocalClassInstantiationUnaffectedByNewInstanceBinding is the
// negative control: instantiating a class defined in the SAME file (not
// imported) must be completely unaffected by jsNewInstanceAssignmentAt,
// since requireBindings[className] is empty for a locally-defined class —
// it must still resolve via the existing same-file class-method resolution
// path, proving no regression on the common (non-cross-file) case.
func TestJSTSLocalClassInstantiationUnaffectedByNewInstanceBinding(t *testing.T) {
	root := t.TempDir()
	write(t, root, "local.js", `class Counter {
  increment() {
    return 1
  }
}

function use() {
  const c = new Counter()
  return c.increment()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	increment := findSymbolByName(idx, "increment")
	if increment.ID == "" {
		t.Fatalf("expected increment symbol")
	}
	trace := TraceSymbol(idx, increment.ID)
	if trace.Status != "found" {
		t.Fatalf("expected found status (unambiguous, single definition), got %#v", trace)
	}
	found := false
	for _, c := range trace.Callers {
		if c.Name == "use" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected use() as caller of Counter.increment via ordinary same-file resolution, got callers=%#v", trace.Callers)
	}
}

// TestTraceSymbolAmbiguousCandidateDetailsCappedAtThreshold ensures the
// inline expansion only applies to small ambiguous sets — a genuinely
// common name with many candidates would make every response huge if
// expanded unconditionally.
func TestTraceSymbolAmbiguousCandidateDetailsCappedAtThreshold(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < maxAmbiguousTraceDetails+1; i++ {
		write(t, root, fmt.Sprintf("src/f%d.ts", i), fmt.Sprintf("export function run() { return %d }\n", i))
	}
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	trace := TraceSymbol(idx, "run")
	if trace.Status != "ambiguous" || len(trace.Candidates) != maxAmbiguousTraceDetails+1 {
		t.Fatalf("expected %d ambiguous candidates, got %#v", maxAmbiguousTraceDetails+1, trace)
	}
	if len(trace.CandidateDetails) != 0 {
		t.Fatalf("expected no inline candidate details above the threshold, got %#v", trace.CandidateDetails)
	}
	if len(trace.Warnings) == 0 {
		t.Fatalf("expected a warning explaining why candidate details were not expanded")
	}
}

func findSymbolByName(idx *Index, name string) CGPSymbol {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for _, sym := range idx.Symbols {
		if sym.Name == name {
			return sym
		}
	}
	return CGPSymbol{}
}

// TestHotPathSignalsBraceLanguage covers hotPathForRangeBraces: nested loops,
// a linear scan and an allocation inside the inner loop, and a self-call
// inside the loop (recursion-in-loop). These signals cover loop depth,
// linear scans, allocations, and recursion within loops.
func TestHotPathSignalsBraceLanguage(t *testing.T) {
	root := t.TempDir()
	write(t, root, "hot.js", `function walk(items, seen) {
  for (let i = 0; i < items.length; i++) {
    for (let j = 0; j < items[i].length; j++) {
      if (seen.indexOf(items[i][j]) === -1) {
        seen.push(items[i][j])
        walk(items[i][j].children, seen)
      }
    }
  }
}

function flat() {
  return 1
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	walkSym := findSymbolByName(idx, "walk")
	if walkSym.ID == "" {
		t.Fatalf("expected walk symbol")
	}
	if walkSym.LoopDepth != 2 {
		t.Fatalf("expected LoopDepth=2 for nested for-loops, got %d (%#v)", walkSym.LoopDepth, walkSym)
	}
	if walkSym.LinearScanInLoop < 1 {
		t.Fatalf("expected LinearScanInLoop>=1 for items.indexOf(...) inside the loop, got %#v", walkSym)
	}
	if walkSym.AllocInLoop < 1 {
		t.Fatalf("expected AllocInLoop>=1 for seen.push(...) inside the loop, got %#v", walkSym)
	}
	if !walkSym.RecursionInLoop {
		t.Fatalf("expected RecursionInLoop=true for walk(...) calling itself inside the loop, got %#v", walkSym)
	}

	flatSym := findSymbolByName(idx, "flat")
	if flatSym.ID == "" {
		t.Fatalf("expected flat symbol")
	}
	if flatSym.LoopDepth != 0 || flatSym.LinearScanInLoop != 0 || flatSym.AllocInLoop != 0 || flatSym.RecursionInLoop {
		t.Fatalf("expected flat() to have zero hot-path signals, got %#v", flatSym)
	}
}

// TestHotPathSignalsPythonIndentation covers hotPathForRangePython: nested
// for-loops tracked by indentation instead of braces.
func TestHotPathSignalsPythonIndentation(t *testing.T) {
	root := t.TempDir()
	write(t, root, "hot.py", `def walk(rows):
    seen = []
    for row in rows:
        for cell in row:
            if cell not in seen:
                seen.append(cell)
    return seen
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	walkSym := findSymbolByName(idx, "walk")
	if walkSym.ID == "" {
		t.Fatalf("expected walk symbol")
	}
	if walkSym.LoopDepth != 2 {
		t.Fatalf("expected LoopDepth=2 for nested for-loops, got %d (%#v)", walkSym.LoopDepth, walkSym)
	}
	if walkSym.AllocInLoop < 1 {
		t.Fatalf("expected AllocInLoop>=1 for seen.append(cell) inside the loop, got %#v", walkSym)
	}
}

// TestHotPathSignalsRubyEndKeyword covers hotPathForRangeRuby, a precision-
// audit finding: Ruby has no braces for control-flow blocks at all (`while`/
// `for`/iterator `do...end` blocks all close with a bare `end`), so before
// this fix the brace-counting heuristic's loopDepth could never advance
// past zero for any Ruby file regardless of real loop nesting — every
// hot-path signal (LoopDepth, LinearScanInLoop, AllocInLoop,
// RecursionInLoop) silently reported zero for every Ruby symbol ever
// indexed.
func TestHotPathSignalsRubyEndKeyword(t *testing.T) {
	root := t.TempDir()
	write(t, root, "hot.rb", `def walk(items, seen)
  items.each do |outer|
    outer.each do |item|
      if seen.index(item).nil?
        seen.push(item)
        walk(item.children, seen)
      end
    end
  end
end

def flat
  1
end
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	walkSym := findSymbolByName(idx, "walk")
	if walkSym.ID == "" {
		t.Fatalf("expected walk symbol")
	}
	if walkSym.LoopDepth != 2 {
		t.Fatalf("expected LoopDepth=2 for nested each-do blocks, got %d (%#v)", walkSym.LoopDepth, walkSym)
	}
	if walkSym.LinearScanInLoop < 1 {
		t.Fatalf("expected LinearScanInLoop>=1 for seen.index(item) inside the loop, got %#v", walkSym)
	}
	if walkSym.AllocInLoop < 1 {
		t.Fatalf("expected AllocInLoop>=1 for seen.push(item) inside the loop, got %#v", walkSym)
	}
	if !walkSym.RecursionInLoop {
		t.Fatalf("expected RecursionInLoop=true for walk(...) calling itself inside the loop, got %#v", walkSym)
	}

	flatSym := findSymbolByName(idx, "flat")
	if flatSym.ID == "" {
		t.Fatalf("expected flat symbol")
	}
	if flatSym.LoopDepth != 0 || flatSym.LinearScanInLoop != 0 || flatSym.AllocInLoop != 0 || flatSym.RecursionInLoop {
		t.Fatalf("expected flat to have zero hot-path signals, got %#v", flatSym)
	}
}

// TestTransitiveLoopDepthPropagatesAcrossCalls covers
// propagateTransitiveLoopDepth: a shallow-looking caller that invokes a
// deeply-looping callee should inherit the callee's loop depth via the
// "calls" edge, capped at maxTransitiveLoopDepthHops.
func TestTransitiveLoopDepthPropagatesAcrossCalls(t *testing.T) {
	root := t.TempDir()
	write(t, root, "hot.js", `function shallow() {
  deep()
}

function deep() {
  for (let i = 0; i < 10; i++) {
    for (let j = 0; j < 10; j++) {
      noop()
    }
  }
}

function noop() {}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	shallowSym := findSymbolByName(idx, "shallow")
	if shallowSym.ID == "" {
		t.Fatalf("expected shallow symbol")
	}
	if shallowSym.LoopDepth != 0 {
		t.Fatalf("expected shallow() to have no loop of its own, got LoopDepth=%d", shallowSym.LoopDepth)
	}
	if shallowSym.TransitiveLoopDepth != 2 {
		t.Fatalf("expected shallow()'s TransitiveLoopDepth to inherit deep()'s LoopDepth=2 via the calls edge, got %d (%#v)", shallowSym.TransitiveLoopDepth, shallowSym)
	}
	deepSym := findSymbolByName(idx, "deep")
	if deepSym.TransitiveLoopDepth != 2 {
		t.Fatalf("expected deep()'s own TransitiveLoopDepth to be at least its own LoopDepth=2, got %d", deepSym.TransitiveLoopDepth)
	}
}

// TestSearchLiteralRanksExactAndNonTestMatchesFirst covers SearchLiteral's
// relevance ranking (sortSearchLiteralHits): an exact-value match should
// outrank a longer incidental substring match, and among same-length
// matches a non-test file should outrank a test fixture, before falling
// back to file:line order. SearchLiteral previously sorted purely by
// file:line with no relevance signal at all.
func TestSearchLiteralRanksExactAndNonTestMatchesFirst(t *testing.T) {
	hits := []Literal{
		{Value: "user-name-field-extra", Location: Location{File: "z_app/widget.go", StartLine: 1}},
		{Value: "user-name", Location: Location{File: "widget_test.go", StartLine: 2}},
		{Value: "user-name", Location: Location{File: "app/widget.go", StartLine: 5}},
		{Value: "user-name", Location: Location{File: "app/widget.go", StartLine: 1}},
	}
	sortSearchLiteralHits(hits, "user-name")

	if hits[0].Value != "user-name" || hits[0].Location.File != "app/widget.go" || hits[0].Location.StartLine != 1 {
		t.Fatalf("expected the exact, non-test, earliest match first, got %#v", hits[0])
	}
	if hits[1].Location.File != "app/widget.go" || hits[1].Location.StartLine != 5 {
		t.Fatalf("expected the second exact non-test match next, got %#v", hits[1])
	}
	if hits[2].Location.File != "widget_test.go" {
		t.Fatalf("expected the exact test-file match to outrank the longer incidental match, got %#v", hits[2])
	}
	if hits[3].Value != "user-name-field-extra" {
		t.Fatalf("expected the longer incidental substring match last, got %#v", hits[3])
	}
}

// TestJSDottedCallDoesNotEmitPhantomBareSuffixCall covers a parser bug where
// `strategy.execute()` previously
// produced TWO ScannedCall records — the correct dotted "strategy.execute"
// AND a spurious bare "execute" (because the run loop revisited the
// identifier after "." as if it were its own standalone call). The bare
// phantom then resolved independently and could fabricate a confident edge
// to any unrelated same-named symbol elsewhere in the file. Only one "calls"
// edge must exist per call site now.
func TestJSDottedCallDoesNotEmitPhantomBareSuffixCall(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.js", `class A { execute() { return 1 } }
class B { execute() { return 2 } }
function pick(strategy) {
  return strategy.execute()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	pick := findSymbolByName(idx, "pick")
	if pick.ID == "" {
		t.Fatalf("expected pick symbol")
	}
	idx.mu.Lock()
	var outgoing []CGPEdge
	for _, e := range idx.SymbolEdges {
		if e.From == pick.ID && e.Type == "calls" {
			outgoing = append(outgoing, e)
		}
	}
	idx.mu.Unlock()
	if len(outgoing) != 1 {
		t.Fatalf("expected exactly 1 outgoing call edge for `strategy.execute()`, got %d: %#v", len(outgoing), outgoing)
	}
	if outgoing[0].To != "unresolved:strategy.execute" {
		t.Fatalf("expected the single edge to target the full dotted callee, got %#v", outgoing[0])
	}
	// Genuinely ambiguous (2 candidates, runtime-polymorphic receiver): must
	// stay unresolved, not silently pick a winner.
	if outgoing[0].Confidence != ConfUnresolved || outgoing[0].UnresolvedReason != ReasonAmbiguousName {
		t.Fatalf("expected unresolved/ambiguous_name, got %#v", outgoing[0])
	}
}

// TestJSChainedCallsAfterDottedCallStillResolve is the regression guard for
// the chained-call case the phantom-call fix must NOT break: `promise.then(x)
// .catch(y)` has two real, independent calls. The fix advances the parser
// cursor past a matched dotted chain up to its "(" — this test confirms a
// SECOND call starting right after that chain's closing paren is still
// discovered normally.
func TestJSChainedCallsAfterDottedCallStillResolve(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.js", `function run(builder) {
  return builder.stepOne().stepTwo()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	run := findSymbolByName(idx, "run")
	if run.ID == "" {
		t.Fatalf("expected run symbol")
	}
	idx.mu.Lock()
	var callees []string
	for _, e := range idx.SymbolEdges {
		if e.From == run.ID && e.Type == "calls" {
			callees = append(callees, e.To)
		}
	}
	idx.mu.Unlock()
	hasUnresolvedSuffix := func(suffix string) bool {
		for _, c := range callees {
			if strings.HasSuffix(c, suffix) {
				return true
			}
		}
		return false
	}
	if !hasUnresolvedSuffix("stepOne") {
		t.Fatalf("expected a `.stepOne` call edge to still be recorded, got %#v", callees)
	}
	if !hasUnresolvedSuffix("stepTwo") {
		t.Fatalf("expected the chained `.stepTwo` call to still be recorded as its own call (not swallowed by the chain-skip fix), got %#v", callees)
	}
}

// TestJSDottedCallOnUnknownReceiverDowngradesToHeuristic covers a dotted call
// on a receiver of unknown type (`obj.unrelatedHelper()`, where
// nothing ties `obj` to the file-level `unrelatedHelper` function) must not
// be reported at `exact`/`scoped` confidence just because the bare method
// name happens to uniquely match an unrelated symbol — that's a name
// coincidence, not structural evidence. It should resolve (recall is still
// useful) but at `heuristic` confidence so an agent doesn't over-trust it.
func TestJSDottedCallOnUnknownReceiverDowngradesToHeuristic(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.js", `function unrelatedHelper() {
  return 99
}
function caller(obj) {
  return obj.unrelatedHelper()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	caller := findSymbolByName(idx, "caller")
	if caller.ID == "" {
		t.Fatalf("expected caller symbol")
	}
	idx.mu.Lock()
	var outgoing []CGPEdge
	for _, e := range idx.SymbolEdges {
		if e.From == caller.ID && e.Type == "calls" {
			outgoing = append(outgoing, e)
		}
	}
	idx.mu.Unlock()
	if len(outgoing) != 1 {
		t.Fatalf("expected exactly 1 outgoing call edge, got %d: %#v", len(outgoing), outgoing)
	}
	if outgoing[0].Confidence != ConfHeuristic {
		t.Fatalf("expected heuristic confidence for a coincidental bare-name match on an unknown-receiver dotted call, got %#v", outgoing[0])
	}
}

// TestJSBareCallStillResolvesAtExactOrScopedConfidence is the regression
// guard for TestJSDottedCallOnUnknownReceiverDowngradesToHeuristic's fix: a
// genuine BARE (non-dotted) function call — real structural evidence, the
// well-tested bare-call case must keep its
// exact/scoped confidence. Only the dotted/unknown-receiver fallback case
// is downgraded.
func TestJSBareCallStillResolvesAtExactOrScopedConfidence(t *testing.T) {
	root := t.TempDir()
	write(t, root, "same.js", `function helper() {
  return 1
}
function caller() {
  return helper()
}
`)
	write(t, root, "other.js", `function globalHelper() {
  return 2
}
`)
	write(t, root, "caller2.js", `function caller2() {
  return globalHelper()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	checkConfidence := func(fromName, wantConfidence string) {
		from := findSymbolByName(idx, fromName)
		if from.ID == "" {
			t.Fatalf("expected %s symbol", fromName)
		}
		idx.mu.Lock()
		var found []CGPEdge
		for _, e := range idx.SymbolEdges {
			if e.From == from.ID && e.Type == "calls" {
				found = append(found, e)
			}
		}
		idx.mu.Unlock()
		if len(found) != 1 || found[0].Confidence != wantConfidence {
			t.Fatalf("expected %s to have one %s-confidence call edge, got %#v", fromName, wantConfidence, found)
		}
	}
	checkConfidence("caller", ConfExact)   // same-file bare call
	checkConfidence("caller2", ConfScoped) // cross-file bare call, single global candidate
}

// outgoingCallEdges returns every "calls" edge whose From is sym.ID.
func outgoingCallEdges(t *testing.T, idx *Index, sym CGPSymbol) []CGPEdge {
	t.Helper()
	if sym.ID == "" {
		t.Fatalf("outgoingCallEdges: empty symbol")
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	var out []CGPEdge
	for _, e := range idx.SymbolEdges {
		if e.From == sym.ID && e.Type == "calls" {
			out = append(out, e)
		}
	}
	return out
}

// TestJSModuleExportsObjectLiteralFunctionPropertiesGetSymbolsAndCalls
// covers a common export pattern:
// `module.exports = { key: async (...) => {...} }` — a very common
// Node.js export pattern — previously produced NO symbol at all for the
// property, so any call inside it vanished completely (not even recorded
// as unresolved), because containingSymbolFast had no symbol to attribute
// it to. Covers both the colon-value form and method-shorthand form, and
// confirms the resulting symbol's calls are discoverable end to end via
// DeadCode (the original real-world symptom: a class used only via such a
// property was a false-positive "dead code" candidate).
func TestJSModuleExportsObjectLiteralFunctionPropertiesGetSymbolsAndCalls(t *testing.T) {
	root := t.TempDir()
	write(t, root, "errors.js", `class TooManyRequestError extends Error {}
module.exports = { TooManyRequestError }
`)
	write(t, root, "api.js", `const { TooManyRequestError } = require('./errors')
module.exports = {
  getListOfCompanies: async (input) => {
    if (input === 429) throw new TooManyRequestError('too many')
  },
  getCompanyDirectors(input) {
    if (input === 429) throw new TooManyRequestError('too many')
  }
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	getList := findSymbolByName(idx, "getListOfCompanies")
	if getList.ID == "" {
		t.Fatalf("expected a symbol for the colon-value arrow property")
	}
	getDirectors := findSymbolByName(idx, "getCompanyDirectors")
	if getDirectors.ID == "" {
		t.Fatalf("expected a symbol for the method-shorthand property")
	}

	errorClass := findSymbolByName(idx, "TooManyRequestError")
	if errorClass.ID == "" {
		t.Fatalf("expected TooManyRequestError class symbol")
	}

	resp := DeadCode(idx, DeadCodeOptions{IncludeExported: true})
	if isDeadCodeCandidate(resp, "TooManyRequestError") {
		t.Fatalf("expected TooManyRequestError to NOT be flagged dead (used via `new` in both exported properties), got %#v", resp.Symbols)
	}
}

// TestJSModuleExportsConciseArrowPropertyCallsAreDiscovered covers the
// trickiest sub-case: a CONCISE arrow body (`name: () => helper()`, no
// braces) has no brace-depth change to naturally stop the object-literal
// property parser from misreading the body's own tokens as the next
// property. Both the colon-value and direct-export forms are covered.
func TestJSModuleExportsConciseArrowPropertyCallsAreDiscovered(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.js", `function helper() {
  return 1
}
module.exports = {
  doWork: () => helper()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	doWork := findSymbolByName(idx, "doWork")
	if doWork.ID == "" {
		t.Fatalf("expected a symbol for the concise-arrow property")
	}
	edges := outgoingCallEdges(t, idx, doWork)
	if len(edges) != 1 || edges[0].Confidence != ConfExact {
		t.Fatalf("expected one exact-confidence call to helper(), got %#v", edges)
	}
}

// TestJSExportsDotPropertyAssignmentGetsSymbolAndCalls covers
// `exports.foo = ...` (as opposed to `module.exports = {...}`) for both an
// anonymous arrow value and a named function expression — the latter must
// keep ITS OWN name ("doWork"), not the assignment target's name ("foo"),
// since that already worked correctly before this fix via a separate path
// (parseTopLevelDecl's `function` branch fires regardless of what assigned
// it) and must not regress.
func TestJSExportsDotPropertyAssignmentGetsSymbolAndCalls(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.js", `function helper() {
  return 1
}
exports.anon = () => {
  return helper()
}
exports.named = function doWork() {
  return helper()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	anon := findSymbolByName(idx, "anon")
	if anon.ID == "" {
		t.Fatalf("expected a symbol named 'anon' (the assignment target) for the anonymous arrow")
	}
	if edges := outgoingCallEdges(t, idx, anon); len(edges) != 1 {
		t.Fatalf("expected one call edge from the anonymous arrow's body, got %#v", edges)
	}

	doWork := findSymbolByName(idx, "doWork")
	if doWork.ID == "" {
		t.Fatalf("expected the named function expression to keep its OWN name 'doWork', not the assignment target's name 'named'")
	}
	if foo := findSymbolByName(idx, "named"); foo.ID != "" {
		t.Fatalf("expected no symbol named 'named' — the named function expression's own name must win, got %#v", foo)
	}
	if edges := outgoingCallEdges(t, idx, doWork); len(edges) != 1 {
		t.Fatalf("expected one call edge from doWork's body, got %#v", edges)
	}
}

// TestJSObjectLiteralPropertyNonFunctionValuesStillUnaffected is the
// regression guard: properties whose value is NOT function-like (numbers,
// strings, nested non-function objects, shorthand properties, spreads,
// computed keys) must behave exactly as before — no spurious symbol for
// the property itself. Uses `const handlers = {...}` (not `module.exports
// = {...}`) so there's a backing "constant" symbol to attribute a call to:
// `module.exports = {...}` has no such backing symbol by design (there is
// nothing named "exports" to be the containing scope), so a call inside a
// non-function property there has nowhere to attribute to — the same
// pre-existing limitation as any bare top-level call with no enclosing
// declaration at all, not something this fix changes either way.
func TestJSObjectLiteralPropertyNonFunctionValuesStillUnaffected(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.js", `function getDefaultPort() {
  return 3000
}
const extra = { a: 1 }
const handlers = {
  PORT: getDefaultPort(),
  NAME: 'service',
  NESTED: { x: 1 },
  ...extra,
  shorthandProp,
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"PORT", "NAME", "NESTED"} {
		if sym := findSymbolByName(idx, bad); sym.ID != "" && sym.Kind == "function" {
			t.Fatalf("expected no function symbol for non-function property %q, got %#v", bad, sym)
		}
	}
	getDefaultPort := findSymbolByName(idx, "getDefaultPort")
	if getDefaultPort.ID == "" {
		t.Fatalf("expected getDefaultPort symbol")
	}
	// The call inside PORT's value must still be recorded, attributed to
	// the enclosing `handlers` constant (the only thing with a symbol that
	// covers this line) — same as a top-level `const x = getDefaultPort()`
	// already did before this fix.
	idx.mu.Lock()
	found := false
	for _, e := range idx.SymbolEdges {
		if e.To == getDefaultPort.ID {
			found = true
		}
	}
	idx.mu.Unlock()
	if !found {
		t.Fatalf("expected a call edge into getDefaultPort from PORT's value expression")
	}
}

// TestJSObjectLiteralFunctionPropertyDoesNotBreakOnMalformedTrailer is a
// defensive guard against the parser hanging or panicking on unusual
// input near an object-literal property — must never be a production
// indexer's failure mode.
func TestJSObjectLiteralFunctionPropertyDoesNotBreakOnMalformedTrailer(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.js", `module.exports = {
  [computedKey()]: () => 1,
  get accessor() { return 1 },
  set accessor(v) {},
  async *gen() {},
}
`)
	done := make(chan struct{})
	go func() {
		if _, err := BuildIndex(root); err != nil {
			t.Error(err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("BuildIndex hung on unusual object-literal property forms")
	}
}

func TestJSDeepObjectLiteralFunctionPropertiesGetSymbolsAndCalls(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.js", `function helper() { return 1 }
module.exports = {
  handlers: {
    admin: {
      run() { return helper() },
      concise: () => helper(),
    }
  }
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"run", "concise"} {
		sym := findSymbolByName(idx, name)
		if sym.ID == "" {
			t.Fatalf("expected nested object-literal property %q to be a symbol", name)
		}
		if edges := outgoingCallEdges(t, idx, sym); len(edges) != 1 || edges[0].Confidence != ConfExact {
			t.Fatalf("expected nested property %q to call helper exactly, got %#v", name, edges)
		}
		if !strings.Contains(sym.ID, "handlers.admin."+name) {
			t.Fatalf("expected collision-resistant nested qualification for %q, got %s", name, sym.ID)
		}
	}
}

func TestVueOptionsObjectMethodsGetSymbolsAndCalls(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Widget.vue", `<template><button @click="submit">Go</button></template>
<script>
function persist() { return true }
export default {
  name: 'Widget',
  methods: {
    submit() { return persist() }
  },
  computed: {
    ready: () => persist()
  }
}
</script>
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"submit", "ready"} {
		sym := findSymbolByName(idx, name)
		if sym.ID == "" {
			t.Fatalf("expected Vue option %q to be a first-class symbol", name)
		}
		if edges := outgoingCallEdges(t, idx, sym); len(edges) != 1 || edges[0].Confidence != ConfExact {
			t.Fatalf("expected Vue option %q to call persist exactly, got %#v", name, edges)
		}
	}
}

// TestGenericEngineCompanionObjectMethodsParentByPositionNotName covers a
// real bug found while onboarding Scala: emitGenericSymbolsTS's method
// parenting used a name-only typeIDByName map. Rust/C++/Kotlin never
// exercise the case where two distinct lexical containers share a name, but
// Scala's companion-object pattern (`class Foo { ... }` and
// `object Foo { ... }` in the same file, a core, extremely common Scala
// idiom) does — a name-only lookup silently parented every method to
// whichever container was emitted last, regardless of which one actually
// lexically contained it. Fixed by resolving the lexical case (ParentStart
// != -1) by exact byte offset (typeIDByStart) instead of by name; the
// non-lexical case (Rust's repeated same-type `impl` blocks, ParentStart ==
// -1) still falls back to typeIDByName, which is correct there since those
// are deliberately meant to collapse onto one class. Written against Scala
// directly since that's what surfaced it, but the fix lives in the
// language-agnostic generic engine, not anything Scala-specific.
func TestGenericEngineCompanionObjectMethodsParentByPositionNotName(t *testing.T) {
	root := t.TempDir()
	write(t, root, "UserService.scala", `class UserService {
  def load(id: Int): String = "x"
}

object UserService {
  def factory(): UserService = new UserService()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	load := findSymbolByName(idx, "load")
	factory := findSymbolByName(idx, "factory")
	if load.ID == "" || factory.ID == "" {
		t.Fatalf("expected both load and factory symbols, got load=%#v factory=%#v", load, factory)
	}
	if load.ParentID == factory.ParentID {
		t.Fatalf("expected load and factory to have different parents (class vs companion object), both got %q", load.ParentID)
	}
	classSym, ok := idx.Symbols[load.ParentID]
	if !ok || classSym.StartLine != 1 {
		t.Fatalf("expected load's parent to be the class definition (line 1), got %#v", classSym)
	}
	objectSym, ok := idx.Symbols[factory.ParentID]
	if !ok || objectSym.StartLine != 5 {
		t.Fatalf("expected factory's parent to be the companion object (line 5), got %#v", objectSym)
	}
}

func TestScalaConstructorSelectsClassOverCompanionObject(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Widget.scala", `class Widget {
  def run(): String = "ok"
}

object Widget {
  def named(): Widget = new Widget()
}

object Consumer {
  def build(): Widget = new Widget()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	var classID, buildID string
	for _, sym := range idx.Symbols {
		switch {
		case sym.Name == "Widget" && strings.HasPrefix(strings.TrimSpace(sym.Signature), "class "):
			classID = sym.ID
		case sym.Name == "build":
			buildID = sym.ID
		}
	}
	if classID == "" || buildID == "" {
		t.Fatalf("missing class/build symbols: class=%q build=%q", classID, buildID)
	}
	for _, edge := range idx.SymbolEdges {
		if edge.From == buildID && edge.Type == "calls" && edge.Evidence.Raw == "Widget" {
			if edge.To != classID || edge.Confidence != ConfScoped {
				t.Fatalf("Scala constructor resolved to companion or ambiguity: %#v", edge)
			}
			return
		}
	}
	t.Fatal("new Widget() constructor edge not found")
}

// TestSelfAttributeTypeTrackingAcrossLanguages covers the instance-attribute
// constructor-call type-inference rule (`@repo = Repo.new`, `repo: Repo`
// body property, etc.) for every language found missing it during a
// hands-on benchmark against competitor tools: each had a class with a
// same-named decoy method elsewhere in the repo, so resolution could only
// succeed via real type-tracking, not a lucky globally-unique name. Each
// case is a regression test for a real, confirmed gap (not a hypothetical
// one) found by deliberately re-running that exact scenario.
func TestSelfAttributeTypeTrackingAcrossLanguages(t *testing.T) {
	cases := []struct {
		name string
		file string
		src  string
	}{
		{
			name: "ruby_ivar_new",
			file: "demo.rb",
			src: `class Repo
  def find(id)
    id
  end
end
class Decoy
  def find(id)
    "decoy"
  end
end
class Service
  def initialize
    @repo = Repo.new
  end
  def load(id)
    @repo.find(id)
  end
end
`,
		},
		{
			name: "cpp_value_type_field",
			file: "demo.cpp",
			src: `class Repo {
public:
    int find(int id) { return id; }
};
class Decoy {
public:
    int find(int id) { return -1; }
};
class Service {
    Repo repo;
public:
    int load(int id) {
        return repo.find(id);
    }
};
`,
		},
		{
			name: "kotlin_body_property",
			file: "demo.kt",
			src: `class Repo {
    fun find(id: Int): String { return "x" }
}
class Decoy {
    fun find(id: Int): String { return "decoy" }
}
class Service {
    var repo: Repo
    constructor() {
        this.repo = Repo()
    }
    fun load(id: Int): String {
        return this.repo.find(id)
    }
}
`,
		},
		{
			name: "scala_body_property",
			file: "demo.scala",
			src: `class Repo {
  def find(id: Int): Int = id
}
class Decoy {
  def find(id: Int): Int = -1
}
class Service {
  var repo: Repo = new Repo()
  def load(id: Int): Int = {
    repo.find(id)
  }
}
`,
		},
		{
			name: "dart_constructor_shorthand_field",
			file: "demo.dart",
			src: `class Repo {
  String find(int id) { return "x"; }
}
class Decoy {
  String find(int id) { return "decoy"; }
}
class Service {
  Repo repo;
  Service(this.repo);
  String load(int id) {
    return repo.find(id);
  }
}
`,
		},
		{
			name: "lua_self_attribute_cross_method",
			file: "demo.lua",
			src: `local Repo = {}
function Repo.new()
  return setmetatable({}, Repo)
end
function Repo:find(id)
  return id
end

local Decoy = {}
function Decoy:find(id)
  return "decoy"
end

local Service = {}
function Service.new()
  local self = setmetatable({}, Service)
  self.repo = Repo.new()
  return self
end
function Service:load(id)
  return self.repo:find(id)
end
`,
		},
		{
			name: "php_this_attribute_untyped",
			file: "demo.php",
			src: `<?php
class Repo {
    function find($id) { return $id; }
}
class Decoy {
    function find($id) { return "decoy"; }
}
class Service {
    private $repo;
    function __construct() {
        $this->repo = new Repo();
    }
    function load($id) {
        return $this->repo->find($id);
    }
}
`,
		},
		{
			name: "javascript_plain_class_this_attribute",
			file: "demo.js",
			src: `class Repo {
  find(id) { return id; }
}
class Decoy {
  find(id) { return "decoy"; }
}
class Service {
  constructor() {
    this.repo = new Repo();
  }
  load(id) {
    return this.repo.find(id);
  }
}
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, tc.file, tc.src)
			idx, err := BuildIndex(root)
			if err != nil {
				t.Fatal(err)
			}
			load := findSymbolByName(idx, "load")
			if load.ID == "" {
				load = findSymbolByName(idx, "initialize")
			}
			edges := outgoingCallEdges(t, idx, findSymbolByName(idx, "load"))
			var foundRepoFind bool
			for _, e := range edges {
				target, ok := idx.Symbols[e.To]
				if ok && target.Name == "find" && strings.Contains(e.To, "Repo") {
					foundRepoFind = true
					if e.Confidence == ConfHeuristic || e.Confidence == ConfUnresolved {
						t.Errorf("%s: expected scoped/exact resolution via type-tracking, got confidence=%s", tc.name, e.Confidence)
					}
				}
			}
			if !foundRepoFind {
				t.Errorf("%s: expected load() to resolve a call to Repo.find, got edges=%#v", tc.name, edges)
			}
		})
	}
}

// TestCppOutOfLineMethodParentsToDeclaringClass covers the foundational gap
// found via C++ fixture testing: `ReturnType
// Class::method() {...}` defined out-of-line in a .cpp file parents to its
// real class declared in a separate .h, not to the .cpp file's own file
// symbol — the dominant real-world C++ pattern (declaration in a header,
// definition out-of-line), not an edge case. Without this, neither a call
// *from inside* the method (a self-attribute lookup) nor a call *into* it
// from elsewhere (findMethodInClass) can ever find it via its real class.
func TestCppOutOfLineMethodParentsToDeclaringClass(t *testing.T) {
	root := t.TempDir()
	write(t, root, "scanner.h", `class Stream {
public:
    char peek() const;
};
class Scanner {
public:
    char mark() const;
private:
    Stream INPUT;
};
`)
	write(t, root, "stream.cpp", `#include "scanner.h"
char Stream::peek() const { return 0; }
`)
	write(t, root, "scanner.cpp", `#include "scanner.h"
char Scanner::mark() const {
    return INPUT.peek();
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	mark := findSymbolByName(idx, "mark")
	if mark.ID == "" {
		t.Fatal("expected Scanner::mark to be indexed")
	}
	classSym, ok := idx.Symbols[mark.ParentID]
	if !ok || classSym.Name != "Scanner" || classSym.File != "scanner.h" {
		t.Fatalf("expected Scanner::mark's parent to be the Scanner class declared in scanner.h, got %#v", classSym)
	}
	edges := outgoingCallEdges(t, idx, mark)
	if len(edges) != 1 || edges[0].Confidence == ConfUnresolved {
		t.Fatalf("expected mark() to resolve its self-attribute call to Stream::peek, got %#v", edges)
	}

	// Incremental rebake of just the .cpp file (the header is unchanged)
	// must not regress the fix-up: emitGenericSymbolsTS's per-file lookup
	// misses again on every rescan of scanner.cpp alone (Scanner is
	// declared in a different, unchanged file), so rebakeChangedFiles must
	// re-run the same global fix-up BuildIndex's first pass does.
	write(t, root, "scanner.cpp", `#include "scanner.h"
char Scanner::mark() const {
    return INPUT.peek();
}
// touched
`)
	if _, _, err := rebakeChangedFiles(idx, root, []string{"scanner.cpp"}, nil); err != nil {
		t.Fatal(err)
	}
	mark = findSymbolByName(idx, "mark")
	classSym, ok = idx.Symbols[mark.ParentID]
	if !ok || classSym.Name != "Scanner" || classSym.File != "scanner.h" {
		t.Fatalf("expected Scanner::mark's parent to still be the Scanner class after an incremental rebake of only scanner.cpp, got %#v", classSym)
	}
	edges = outgoingCallEdges(t, idx, mark)
	if len(edges) != 1 || edges[0].Confidence == ConfUnresolved {
		t.Fatalf("expected mark() to still resolve its self-attribute call to Stream::peek after rebake, got %#v", edges)
	}
}

// TestRustCrossFileImplBlockParentsToDeclaringStruct covers the same
// generic-engine redirect mechanism's cross-file case for Rust: `impl Repo
// { fn find(...) {...} }` in one file, `struct Repo;` declared in another —
// less common than C++'s header/cpp split (Rust convention favors
// collocating a type with its impl blocks), but the same fix-up applies
// with zero Rust-specific code, a direct consequence of fixing the shared
// mechanism rather than special-casing C++ alone.
func TestRustCrossFileImplBlockParentsToDeclaringStruct(t *testing.T) {
	root := t.TempDir()
	write(t, root, "types.rs", `pub struct Repo;
`)
	write(t, root, "repo_impl.rs", `use crate::types::Repo;
impl Repo {
    pub fn find(&self, id: i32) -> i32 { id }
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	find := findSymbolByName(idx, "find")
	if find.ID == "" {
		t.Fatal("expected Repo::find to be indexed")
	}
	classSym, ok := idx.Symbols[find.ParentID]
	if !ok || classSym.Name != "Repo" || classSym.File != "types.rs" {
		t.Fatalf("expected Repo::find's parent to be the Repo struct declared in types.rs, got %#v", classSym)
	}
}

// TestGoCrossFileMethodParentsToDeclaringStruct covers a real, confirmed
// gap found while verifying the same architecture's coverage across
// languages: emitGoSymbolsTS's method-parenting redirect (mirroring the
// C/C++/Rust case resolveOutOfLineMethodParents was built for) only
// looked up the receiver type in *this file's own* type defs. A struct
// declared in one file with its methods defined in others — a real,
// common Go pattern, not a contrived one — left every such method
// permanently parented to its own file rather than the real struct.
func TestGoCrossFileMethodParentsToDeclaringStruct(t *testing.T) {
	root := t.TempDir()
	write(t, root, "types.go", `package main

type Repo struct{}
type Decoy struct{}
`)
	write(t, root, "repo_methods.go", `package main

func (r *Repo) Find(id int) int { return id }
`)
	write(t, root, "decoy_methods.go", `package main

func (d *Decoy) Find(id int) int { return -1 }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	repoFind := findSymbolByID(t, idx, "symbol:go:method:repo_methods.go:Repo.Find")
	classSym, ok := idx.Symbols[repoFind.ParentID]
	if !ok || classSym.Name != "Repo" || classSym.File != "types.go" {
		t.Fatalf("expected Repo.Find's parent to be the Repo struct declared in types.go, got %#v", classSym)
	}
	decoyFind := findSymbolByID(t, idx, "symbol:go:method:decoy_methods.go:Decoy.Find")
	classSym, ok = idx.Symbols[decoyFind.ParentID]
	if !ok || classSym.Name != "Decoy" || classSym.File != "types.go" {
		t.Fatalf("expected Decoy.Find's parent to be the Decoy struct declared in types.go, got %#v", classSym)
	}

	// Incremental rebake of just one method file (types.go is unchanged)
	// must not regress the fix-up — see the identical C++ rebake test for
	// the full rationale (the per-file lookup misses again on every
	// rescan of repo_methods.go alone).
	write(t, root, "repo_methods.go", `package main

func (r *Repo) Find(id int) int { return id }
// touched
`)
	if _, _, err := rebakeChangedFiles(idx, root, []string{"repo_methods.go"}, nil); err != nil {
		t.Fatal(err)
	}
	repoFind = findSymbolByID(t, idx, "symbol:go:method:repo_methods.go:Repo.Find")
	classSym, ok = idx.Symbols[repoFind.ParentID]
	if !ok || classSym.Name != "Repo" || classSym.File != "types.go" {
		t.Fatalf("expected Repo.Find's parent to still be the Repo struct after an incremental rebake of only repo_methods.go, got %#v", classSym)
	}
}

// TestGoTwoLevelFieldAccessCallResolvesAcrossFiles covers the deeper gap
// found while investigating the above: even with ParentID fixed, a
// two-level field-access call (`s.repo.Find()`, the dominant real-world
// shape for a struct depending on another struct-typed field) could not
// resolve at all — Go struct fields were never captured as VarDecls in
// the first place (a separate, missing extraction, not just a missing
// lookup), and resolveGoReceiverCall had no case for a dotted receiver.
// Both gaps had to be closed together for this call to resolve; either
// alone would have left it falling back to an honest-but-lossy bare-name
// search.
func TestGoTwoLevelFieldAccessCallResolvesAcrossFiles(t *testing.T) {
	root := t.TempDir()
	write(t, root, "types.go", `package main

type Repo struct{}
type Decoy struct{}
`)
	write(t, root, "repo_methods.go", `package main

func (r *Repo) Find(id int) int { return id }
`)
	write(t, root, "decoy_methods.go", `package main

func (d *Decoy) Find(id int) int { return -1 }
`)
	write(t, root, "service.go", `package main

type Service struct {
	repo Repo
}

func (s *Service) Load(id int) int {
	return s.repo.Find(id)
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	load := findSymbolByName(idx, "Load")
	edges := outgoingCallEdges(t, idx, load)
	var foundRepoFind bool
	for _, e := range edges {
		if e.To == "symbol:go:method:repo_methods.go:Repo.Find" {
			foundRepoFind = true
			if e.Confidence != ConfScoped {
				t.Errorf("expected Service.Load's call to Repo.Find to resolve at scoped, got confidence=%s", e.Confidence)
			}
		}
	}
	if !foundRepoFind {
		t.Fatalf("expected Service.Load to resolve a call to Repo.Find via its repo field, got edges=%#v", edges)
	}
}

func findSymbolByID(t *testing.T, idx *Index, id string) CGPSymbol {
	t.Helper()
	sym, ok := idx.Symbols[id]
	if !ok {
		t.Fatalf("expected symbol %q to exist", id)
	}
	return sym
}

// TestCSharpPartialClassFieldVisibleAcrossFragments covers a real bug
// found while verifying whether Java/C# had the same class of cross-file
// gap Go did: `partial class Service` split across multiple files creates
// one symbol per fragment with no link between them at all — a field
// declared in one fragment (here, Service.cs) was invisible to a method
// defined in a different fragment of the *same logical class*
// (Service.Methods.cs), even though both genuinely are Service. This is a
// real, common C# pattern (designer-generated code, source generators, EF
// scaffolding), not a contrived one.
func TestCSharpPartialClassFieldVisibleAcrossFragments(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Repo.cs", `namespace App {
    public class Repo {
        public int Find(int id) { return id; }
    }
    public class Decoy {
        public int Find(int id) { return -1; }
    }
}
`)
	write(t, root, "Service.cs", `namespace App {
    public partial class Service {
        private Repo repo;
    }
}
`)
	write(t, root, "Service.Methods.cs", `namespace App {
    public partial class Service {
        public int Load(int id) {
            return this.repo.Find(id);
        }
    }
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	load := findSymbolByName(idx, "Load")
	edges := outgoingCallEdges(t, idx, load)
	var foundRepoFind bool
	for _, e := range edges {
		if strings.Contains(e.To, "Repo.cs:Repo.Find") {
			foundRepoFind = true
			if e.Confidence != ConfScoped {
				t.Errorf("expected Service.Load's call to Repo.Find to resolve at scoped, got confidence=%s", e.Confidence)
			}
		}
	}
	if !foundRepoFind {
		t.Fatalf("expected Service.Load (declared in Service.Methods.cs) to resolve a call to Repo.Find via its repo field (declared in the sibling fragment Service.cs), got edges=%#v", edges)
	}
}

// TestCSharpPartialClassMethodCallableFromOutsideAnyFragment covers the
// companion gap to the one above: external code calling a partial class's
// method by name (`s.Load(id)`, not through "this") goes through
// findClassByName -> findMethodInClass, not resolveVarCall's field-lookup
// path — and idx.csharpFQN (which findClassByName consults) only ever
// keeps *one* fragment's symbol ID, decided by which file Phase 5's
// parallel scan happened to process last. Without checking sibling
// fragments inside findMethodInClassLocked too, this resolves correctly
// only when the method happens to live in whichever fragment won that
// race — a real, schedule-order-dependent flake, not a deterministic
// pass. Re-indexed repeatedly during verification to confirm the fix
// removes the dependency on scan order entirely, not just in one run.
func TestCSharpPartialClassMethodCallableFromOutsideAnyFragment(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Repo.cs", `namespace App {
    public class Repo {
        public int Find(int id) { return id; }
    }
}
`)
	write(t, root, "Service.cs", `namespace App {
    public partial class Service {
        private Repo repo;
    }
}
`)
	write(t, root, "Service.Methods.cs", `namespace App {
    public partial class Service {
        public int Load(int id) {
            return this.repo.Find(id);
        }
    }
}
`)
	write(t, root, "Caller.cs", `namespace App {
    public class Caller {
        public int Run(Service s, int id) {
            return s.Load(id);
        }
    }
}
`)
	for i := 0; i < 5; i++ {
		idx, err := BuildIndex(root)
		if err != nil {
			t.Fatal(err)
		}
		run := findSymbolByName(idx, "Run")
		edges := outgoingCallEdges(t, idx, run)
		var foundLoad bool
		for _, e := range edges {
			if strings.Contains(e.To, "Service.Load") {
				foundLoad = true
				if e.Confidence != ConfScoped {
					t.Errorf("run %d: expected Caller.Run's call to Service.Load to resolve at scoped, got confidence=%s", i, e.Confidence)
				}
			}
		}
		if !foundLoad {
			t.Fatalf("run %d: expected Caller.Run to resolve a call to Service.Load (declared in one fragment, referenced via a parameter typed by the class name), got edges=%#v", i, edges)
		}
	}
}

// TestGenericEngineInheritanceResolvesCallsThroughBaseClass covers a gap
// found while verifying PHP traits: idx.classBases (the map every
// findMethodInClassLocked base-class walk relies on) was populated only by
// the four bespoke emitters (Python/Java/C#/Go) — emitGenericSymbolsTS,
// shared by every other tree-sitter language (Rust/Ruby/PHP/C/C++/Kotlin/
// Scala/Lua/Elixir/Dart/Haskell/Clojure/Bash), never wrote to it at all, so
// none of those languages could resolve a bare call to an inherited
// method. Fixed by populating idx.classBases generically, alongside
// typeIDByName/typeIDByStart, whenever a "class" def carries Bases. This
// covers the fix for C++ specifically (chosen since it has unambiguous
// `class Derived : public Base` syntax); Kotlin's `class Derived : Base()`
// is covered by TestKotlinClassInheritanceResolvesCallsThroughBaseClass.
func TestGenericEngineInheritanceResolvesCallsThroughBaseClass(t *testing.T) {
	root := t.TempDir()
	write(t, root, "base.h", `class Base {
public:
    int helper(int x) {
        return x + 1;
    }
};
`)
	write(t, root, "derived.h", `#include "base.h"
class Derived : public Base {
public:
    int useIt(int x);
};
`)
	write(t, root, "derived.cpp", `#include "derived.h"
int Derived::useIt(int x) {
    return helper(x);
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	useIt := findSymbolByName(idx, "useIt")
	edges := outgoingCallEdges(t, idx, useIt)
	var foundHelper bool
	for _, e := range edges {
		if strings.Contains(e.To, "Base.helper") {
			foundHelper = true
			if e.Confidence != ConfScoped {
				t.Errorf("expected Derived.useIt's call to inherited Base.helper to resolve at scoped, got confidence=%s", e.Confidence)
			}
		}
	}
	if !foundHelper {
		t.Fatalf("expected Derived.useIt to resolve a bare call to the inherited Base.helper, got edges=%#v", edges)
	}
}

// TestKotlinClassInheritanceResolvesCallsThroughBaseClass is the Kotlin
// half of TestGenericEngineInheritanceResolvesCallsThroughBaseClass.
func TestKotlinClassInheritanceResolvesCallsThroughBaseClass(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Base.kt", `package app
open class Base {
    open fun helper(x: Int): Int {
        return x + 1
    }
}
`)
	write(t, root, "Derived.kt", `package app
class Derived : Base() {
    fun useIt(x: Int): Int {
        return helper(x)
    }
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	useIt := findSymbolByName(idx, "useIt")
	edges := outgoingCallEdges(t, idx, useIt)
	var foundHelper bool
	for _, e := range edges {
		if strings.Contains(e.To, "Base.helper") {
			foundHelper = true
			if e.Confidence != ConfScoped {
				t.Errorf("expected Derived.useIt's call to inherited Base.helper to resolve at scoped, got confidence=%s", e.Confidence)
			}
		}
	}
	if !foundHelper {
		t.Fatalf("expected Derived.useIt to resolve a bare call to the inherited Base.helper, got edges=%#v", edges)
	}
}

// TestPHPTraitMethodResolvesThroughUsingClass covers a real, common PHP
// pattern (Laravel and most idiomatic PHP lean on traits heavily): a class
// that `use`s a trait can call the trait's methods via `$this->method()`
// as if they were its own. Two compounding gaps made this unresolved
// before this fix: (1) PHP's "trait_declaration" was not captured by
// tags.scm at all, so a trait's methods were emitted as bare top-level
// functions with no container symbol; (2) even after capturing traits as
// class-kind symbols, classBases never recorded the `use TraitName;`
// composition (a class-body-level use_declaration, distinct from the
// identically-shaped file-level `use Some\Namespace\Class;` import
// statement — disambiguated by checking the use_declaration's parent is
// the class/trait body, not the file). A second class with a same-named
// method (Bar.greet) confirms this resolves to the trait specifically, not
// merely by lucky unique-name coincidence.
func TestPHPTraitMethodResolvesThroughUsingClass(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Greetable.php", `<?php
trait Greetable {
    public function greet(): string {
        return "hi";
    }
}
`)
	write(t, root, "Foo.php", `<?php
class Foo {
    use Greetable;

    public function welcome(): string {
        return $this->greet();
    }
}
`)
	write(t, root, "Bar.php", `<?php
class Bar {
    public function greet(): string {
        return "wrong";
    }
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	welcome := findSymbolByID(t, idx, "symbol:php:method:Foo.php:Foo.welcome")
	edges := outgoingCallEdges(t, idx, welcome)
	var foundGreet bool
	for _, e := range edges {
		if strings.Contains(e.To, "Greetable.greet") {
			foundGreet = true
			if e.Confidence != ConfScoped {
				t.Errorf("expected Foo.welcome's call to trait method Greetable.greet to resolve at scoped, got confidence=%s", e.Confidence)
			}
		}
		if strings.Contains(e.To, "Bar.greet") {
			t.Errorf("Foo.welcome's $this->greet() call must not resolve to the unrelated Bar.greet, got edge=%#v", e)
		}
	}
	if !foundGreet {
		t.Fatalf("expected Foo.welcome to resolve $this->greet() to the used trait's Greetable.greet, got edges=%#v", edges)
	}
}

// TestKotlinExtensionFunctionParentsToReceiverType covers Kotlin extension
// functions (`fun Repo.extendedFind(id: Int): String {...}`), an idiomatic,
// extremely common Kotlin pattern (Android/Compose code leans on them
// heavily) declared outside any class body — often in a different file
// from the type they extend. Before this fix they were indistinguishable
// from a bare top-level function: never parented to their receiver type,
// and `this` inside the extension function (which refers to the receiver
// instance) could not resolve to the receiver type's own methods. A decoy
// class with a same-named method (Other.baseFind) confirms the resolution
// is real receiver-type-aware linking, not a lucky unique-name guess.
func TestKotlinExtensionFunctionParentsToReceiverType(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Repo.kt", `package app

class Repo {
    fun baseFind(id: Int): String {
        return "x"
    }
}
`)
	write(t, root, "RepoExt.kt", `package app

fun Repo.extendedFind(id: Int): String {
    return this.baseFind(id)
}
`)
	write(t, root, "Other.kt", `package app

class Other {
    fun baseFind(id: Int): String {
        return "wrong"
    }
}
`)
	write(t, root, "Caller.kt", `package app

class Service {
    val repo = Repo()
    fun load(id: Int): String {
        return repo.extendedFind(id)
    }
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	extendedFind := findSymbolByID(t, idx, "symbol:kotlin:method:RepoExt.kt:Repo.extendedFind")
	if extendedFind.Kind != "method" {
		t.Fatalf("expected Repo.extendedFind to be kind=method, got %q", extendedFind.Kind)
	}
	classSym, ok := idx.Symbols[extendedFind.ParentID]
	if !ok || classSym.Name != "Repo" {
		t.Fatalf("expected Repo.extendedFind's parent to be the Repo class, got %#v", classSym)
	}

	edges := outgoingCallEdges(t, idx, extendedFind)
	var foundBaseFind bool
	for _, e := range edges {
		if strings.Contains(e.To, "Repo.baseFind") {
			foundBaseFind = true
			if e.Confidence != ConfScoped {
				t.Errorf("expected this.baseFind(id) to resolve at scoped, got confidence=%s", e.Confidence)
			}
		}
		if strings.Contains(e.To, "Other.baseFind") {
			t.Errorf("this.baseFind(id) inside Repo.extendedFind must not resolve to the unrelated Other.baseFind, got edge=%#v", e)
		}
	}
	if !foundBaseFind {
		t.Fatalf("expected Repo.extendedFind's this.baseFind(id) to resolve to Repo.baseFind, got edges=%#v", edges)
	}

	load := findSymbolByName(idx, "load")
	loadEdges := outgoingCallEdges(t, idx, load)
	var foundExtendedFind bool
	for _, e := range loadEdges {
		if strings.Contains(e.To, "Repo.extendedFind") {
			foundExtendedFind = true
		}
	}
	if !foundExtendedFind {
		t.Fatalf("expected Service.load's repo.extendedFind(id) to resolve to Repo.extendedFind, got edges=%#v", loadEdges)
	}
}

// TestPlainKotlinTopLevelFunctionStaysFunctionKind guards against a
// regression in the extension-function fix above: an ordinary top-level
// function with no receiver type must remain kind="function" with no
// parent class, not be misidentified as an extension method.
func TestPlainKotlinTopLevelFunctionStaysFunctionKind(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Utils.kt", `package app

fun plainTopLevel(x: Int): Int {
    return x * 2
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	sym := findSymbolByName(idx, "plainTopLevel")
	if sym.Kind != "function" {
		t.Fatalf("expected plainTopLevel to stay kind=function, got %q", sym.Kind)
	}
	if sym.ParentID != "" {
		if parent, ok := idx.Symbols[sym.ParentID]; ok && parent.Kind == "class" {
			t.Fatalf("expected plainTopLevel to have no class parent, got %#v", parent)
		}
	}
}

// TestRubyInheritedMethodResolvesThroughBaseClass confirms Ruby benefits
// from the generic-engine classBases fix
// (see TestGenericEngineInheritanceResolvesCallsThroughBaseClass) with no
// Ruby-specific code needed: `class Derived < Base` calling an inherited
// bare method already resolved correctly once idx.classBases was
// populated generically.
func TestRubyInheritedMethodResolvesThroughBaseClass(t *testing.T) {
	root := t.TempDir()
	write(t, root, "base.rb", `class Base
  def helper(x)
    x + 1
  end
end
`)
	write(t, root, "decoy.rb", `class Decoy
  def helper(x)
    -1
  end
end
`)
	write(t, root, "derived.rb", `class Derived < Base
  def use_it(x)
    helper(x)
  end
end
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	useIt := findSymbolByName(idx, "use_it")
	edges := outgoingCallEdges(t, idx, useIt)
	var foundHelper bool
	for _, e := range edges {
		if strings.Contains(e.To, "Base.helper") {
			foundHelper = true
			if e.Confidence != ConfScoped {
				t.Errorf("expected Derived.use_it's call to inherited Base.helper to resolve at scoped, got confidence=%s", e.Confidence)
			}
		}
		if strings.Contains(e.To, "Decoy.helper") {
			t.Errorf("Derived.use_it's helper(x) call must not resolve to the unrelated Decoy.helper, got edge=%#v", e)
		}
	}
	if !foundHelper {
		t.Fatalf("expected Derived.use_it to resolve a bare call to the inherited Base.helper, got edges=%#v", edges)
	}
}

// TestRubySuperCallResolvesToSameNamedBaseMethod covers Ruby's bare
// `super`/`super(args)`, which (unlike Java's `super.foo()` or Python's
// `super().foo()`) has no explicit method name: it implicitly calls the
// same-named method on the base class. Before this fix, the generic
// engine treated `super` as a bare call to a method literally named
// "super" (which never exists), always landing on
// unresolved/missing_import.
func TestRubySuperCallResolvesToSameNamedBaseMethod(t *testing.T) {
	root := t.TempDir()
	write(t, root, "base.rb", `class Base
  def helper(x)
    x + 1
  end
end
`)
	write(t, root, "derived.rb", `class Derived < Base
  def helper(x)
    super(x) + 100
  end
end
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	derivedHelper := findSymbolByID(t, idx, "symbol:ruby:method:derived.rb:Derived.helper")
	edges := outgoingCallEdges(t, idx, derivedHelper)
	var foundBaseHelper bool
	for _, e := range edges {
		if strings.Contains(e.To, "Base.helper") {
			foundBaseHelper = true
			if e.Confidence != ConfScoped {
				t.Errorf("expected super(x) to resolve at scoped, got confidence=%s", e.Confidence)
			}
		}
	}
	if !foundBaseHelper {
		t.Fatalf("expected Derived.helper's super(x) to resolve to Base.helper, got edges=%#v", edges)
	}
}

// TestRustTraitDefaultMethodResolvesAcrossFiles covers `impl Trait for
// Type {}` (a trait with no override, relying on the trait's default
// method body) declared in a different file from the trait itself — the
// realistic, common shape (trait in one module, impl in another). Two
// compounding gaps made `self.greet()` unresolved before this fix: (1)
// `impl_item`'s "trait" field (distinguishing `impl Trait for Type` from a
// plain `impl Type`) was never captured at all, so Type never gained
// Trait as a Base; (2) even with Bases populated, findClassByNameLocked's
// generic name-lookup fallback only ever matched kind=="class", and Rust
// traits are captured as kind=="interface" — so the lookup could never
// find the trait by name. A same-named decoy method on an unrelated
// struct confirms the resolution is real trait-aware linking, not a lucky
// same-file/unique-name guess (same-file would have hidden this gap
// entirely, which is exactly how it was first found before being moved to
// separate files to confirm).
func TestRustTraitDefaultMethodResolvesAcrossFiles(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/greetable.rs", `pub trait Greetable {
    fn greet(&self) -> String {
        "hi".to_string()
    }
}
`)
	write(t, root, "src/lib.rs", `mod greetable;
use greetable::Greetable;

pub struct Foo {
    pub name: String,
}

impl Greetable for Foo {}

impl Foo {
    pub fn welcome(&self) -> String {
        self.greet()
    }
}
`)
	write(t, root, "src/decoy.rs", `pub struct Bar;

impl Bar {
    pub fn greet(&self) -> String {
        "wrong".to_string()
    }
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	welcome := findSymbolByID(t, idx, "symbol:rust:method:src/lib.rs:Foo.welcome")
	edges := outgoingCallEdges(t, idx, welcome)
	var foundGreet bool
	for _, e := range edges {
		if strings.Contains(e.To, "Greetable.greet") {
			foundGreet = true
			if e.Confidence != ConfScoped {
				t.Errorf("expected self.greet() to resolve at scoped, got confidence=%s", e.Confidence)
			}
		}
		if strings.Contains(e.To, "Bar.greet") {
			t.Errorf("Foo.welcome's self.greet() must not resolve to the unrelated Bar.greet, got edge=%#v", e)
		}
	}
	if !foundGreet {
		t.Fatalf("expected Foo.welcome's self.greet() to resolve to the trait's default Greetable.greet, got edges=%#v", edges)
	}
}

// TestScalaTraitMethodResolvesThroughMixingClass confirms Scala trait
// mixins (`class Foo extends Object with Greetable`) already benefit from
// the generic-engine classBases fix with no Scala-specific code needed:
// scalaClassBaseNames already folds `with` clause traits into the same
// Bases list as the `extends` clause (both are children of one
// extends_clause node in this grammar), so this was already correct once
// idx.classBases was populated generically — confirmed here, not assumed.
func TestScalaTraitMethodResolvesThroughMixingClass(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Greetable.scala", `package app

trait Greetable {
  def greet(): String = "hi"
}
`)
	write(t, root, "Decoy.scala", `package app

class Decoy {
  def greet(): String = "wrong"
}
`)
	write(t, root, "Foo.scala", `package app

class Foo extends Object with Greetable {
  def welcome(): String = {
    this.greet()
  }
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	welcome := findSymbolByName(idx, "welcome")
	edges := outgoingCallEdges(t, idx, welcome)
	var foundGreet bool
	for _, e := range edges {
		if strings.Contains(e.To, "Greetable.greet") {
			foundGreet = true
			if e.Confidence != ConfScoped {
				t.Errorf("expected this.greet() to resolve at scoped, got confidence=%s", e.Confidence)
			}
		}
		if strings.Contains(e.To, "Decoy.greet") {
			t.Errorf("Foo.welcome's this.greet() must not resolve to the unrelated Decoy.greet, got edge=%#v", e)
		}
	}
	if !foundGreet {
		t.Fatalf("expected Foo.welcome's this.greet() to resolve to the mixed-in Greetable.greet, got edges=%#v", edges)
	}
}

// TestSuperCallNeverResolvesToItself covers a real correctness bug found
// while checking Scala's `super.foo()` against a class with a trait mixed
// in: `class Foo extends Object with Greetable { override def greet() =
// super.greet() }` has classBases = ["Object", "Greetable"] (len > 1, the
// *common* case for any Scala class using traits, not a rare edge case),
// so the old resolveSuperCall's `len(bases) == 1` requirement always
// failed and fell through to a bare-name search. That search matched
// Foo.greet itself (same name, same file as the override) before ever
// considering Greetable.greet, producing a self-loop edge (Foo.greet
// "calling" Foo.greet) — silently wrong, not just an honest unresolved.
// Fixed by checking every base (not just a sole one) for the method, and
// guarding the bare-name fallback against ever resolving back to the
// calling symbol itself.
func TestSuperCallNeverResolvesToItself(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Greetable.scala", `package app

trait Greetable {
  def greet(): String = "hi"
}
`)
	write(t, root, "Foo.scala", `package app

class Foo extends Object with Greetable {
  override def greet(): String = {
    super.greet() + "!"
  }
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	fooGreet := findSymbolByID(t, idx, "symbol:scala:method:Foo.scala:Foo.greet")
	edges := outgoingCallEdges(t, idx, fooGreet)
	var foundTraitGreet bool
	for _, e := range edges {
		if e.To == fooGreet.ID {
			t.Fatalf("super.greet() must never resolve to the calling method itself (self-loop), got edge=%#v", e)
		}
		if strings.Contains(e.To, "Greetable.greet") {
			foundTraitGreet = true
			if e.Confidence != ConfScoped {
				t.Errorf("expected super.greet() to resolve at scoped, got confidence=%s", e.Confidence)
			}
		}
	}
	if !foundTraitGreet {
		t.Fatalf("expected Foo.greet's super.greet() to resolve to the trait's Greetable.greet, got edges=%#v", edges)
	}
}

// TestDartMixinMethodResolvesThroughUsingClass covers Dart's `class Foo
// extends Object with Greetable` mixin composition (the common
// Flutter/Dart pattern, e.g. `class Foo extends StatefulWidget with
// SomeMixin`). Dart's "class_definition" node shares its kind with
// Python's, but unlike Python has no "superclasses" field — the generic
// fallback in classBases() silently returned nil for every Dart class
// before this fix, so a mixed-in method was never linked at all.
func TestDartMixinMethodResolvesThroughUsingClass(t *testing.T) {
	root := t.TempDir()
	write(t, root, "foo.dart", `mixin Greetable {
  String greet() => "hi";
}

class Foo extends Object with Greetable {
  String welcome() {
    return this.greet();
  }
}
`)
	write(t, root, "decoy.dart", `class Bar {
  String greet() {
    return "wrong";
  }
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	welcome := findSymbolByName(idx, "welcome")
	edges := outgoingCallEdges(t, idx, welcome)
	var foundGreet bool
	for _, e := range edges {
		if strings.Contains(e.To, "Greetable.greet") {
			foundGreet = true
			if e.Confidence != ConfScoped {
				t.Errorf("expected this.greet() to resolve at scoped, got confidence=%s", e.Confidence)
			}
		}
		if strings.Contains(e.To, "Bar.greet") {
			t.Errorf("Foo.welcome's this.greet() must not resolve to the unrelated Bar.greet, got edge=%#v", e)
		}
	}
	if !foundGreet {
		t.Fatalf("expected Foo.welcome's this.greet() to resolve to the mixed-in Greetable.greet, got edges=%#v", edges)
	}
}

// TestRubyBareSuperWithNoParensResolves covers a gap found while verifying
// the bare-super fix (TestRubySuperCallResolvesToSameNamedBaseMethod)
// against real-world Ruby (jekyll): that test only covered `super(args)`
// (parenthesized). `super` alone, with no parens and no args — relying on
// the enclosing method's own arguments, an equally common and even
// simpler idiomatic form — was never even extracted as a call at all: the
// grammar represents parenthesized `super(args)` as a "call" node (whose
// "method" field resolves to a "super" subtype), but bare `super` is a
// bare "super"-kind node with no wrapping "call" node, invisible to the
// `(call) @call` query pattern. Found by dumping the parse tree for a
// real jekyll file (SiteDrop#key? calling bare `super`) after the
// synthetic parenthesized-only test passed but this real call still came
// back with zero callers.
func TestRubyBareSuperWithNoParensResolves(t *testing.T) {
	root := t.TempDir()
	write(t, root, "base.rb", `class Base
  def key?(k)
    true
  end
end
`)
	write(t, root, "derived.rb", `class Derived < Base
  def key?(k)
    super
  end
end
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	derivedKey := findSymbolByID(t, idx, "symbol:ruby:method:derived.rb:Derived.key?")
	edges := outgoingCallEdges(t, idx, derivedKey)
	var foundBaseKey bool
	for _, e := range edges {
		if strings.Contains(e.To, "Base.key?") {
			foundBaseKey = true
			if e.Confidence != ConfScoped {
				t.Errorf("expected bare super to resolve at scoped, got confidence=%s", e.Confidence)
			}
		}
	}
	if !foundBaseKey {
		t.Fatalf("expected Derived.key?'s bare `super` to resolve to Base.key?, got edges=%#v", edges)
	}
}

// TestTraceSymbolCompactOptionDropsDetailFieldsButKeepsIdentity covers the
// opt-in TraceSymbolOptions.Compact, added after response-size auditing: a
// symbol with several callers/callees previously always carried each
// one's full Signature/Docstring/ReturnTypes/hot-path fields, dominating
// response size once there are more than a handful. ID and Language were
// originally kept even in Compact mode, but profiling a real multi-
// thousand-file repo found ID's full qualified-path string (e.g.
// "symbol:kotlin:method:<full-file-path>:Class.method") was the single
// largest per-entry cost once dropped down to "compact" — pure
// duplication of File+StartLine+Name, not optional ballast like Signature/
// Docstring. Compact now drops ID and Language too, matching the same lean
// caller/callee shape inspect_symbol's format=node already uses
// (compactNodeSymbolSummary, includeDetails=false) — and must not change
// anything when left at its default (false), the explicit backward-
// compatibility requirement for an opt-in flag touching a function used by
// many other callers/tests.
func TestTraceSymbolCompactOptionDropsDetailFieldsButKeepsIdentity(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/lib.go", `package lib

// Helper does a thing.
func Helper(x int) int {
	return x + 1
}

// Caller calls Helper.
func Caller() int {
	return Helper(1)
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	full := TraceSymbolWithOptions(idx, "Helper", TraceSymbolOptions{Sites: true})
	if len(full.Callers) != 1 || full.Callers[0].Signature == "" || full.Callers[0].Docstring == "" {
		t.Fatalf("expected default (non-compact) trace to keep full caller detail, got %#v", full.Callers)
	}
	if full.Callers[0].ID == "" || full.Callers[0].Language == "" {
		t.Fatalf("expected default (non-compact) trace to keep id/language, got %#v", full.Callers)
	}

	compact := TraceSymbolWithOptions(idx, "Helper", TraceSymbolOptions{Sites: true, Compact: true})
	if len(compact.Callers) != 1 {
		t.Fatalf("expected one caller in compact trace, got %#v", compact.Callers)
	}
	c := compact.Callers[0]
	if c.Name != "Caller" || c.Kind != "function" || c.File != "src/lib.go" || c.StartLine == 0 {
		t.Fatalf("expected compact caller to keep identifying fields, got %#v", c)
	}
	if c.ID != "" || c.Language != "" {
		t.Fatalf("expected compact caller to drop id/language too, got %#v", c)
	}
	if c.Signature != "" || c.Docstring != "" || c.Complexity != 0 || c.LoopDepth != 0 {
		t.Fatalf("expected compact caller to drop detail fields, got %#v", c)
	}

	compactBytes, err := json.Marshal(compact)
	if err != nil {
		t.Fatal(err)
	}
	fullBytes, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	if len(compactBytes) >= len(fullBytes) {
		t.Fatalf("expected compact response to be smaller: compact=%d full=%d", len(compactBytes), len(fullBytes))
	}
	// The top-level Symbol object keeps its own signature/id/language and
	// a (possibly first-sentence-trimmed, see TestTraceSymbolCompact
	// TrimsMainSymbolDocstringToFirstSentence) docstring regardless of
	// Compact — Compact's id/language/detail-field drop only applies to
	// Callers/Callees entries — check only within the callers array, not
	// the whole marshaled response.
	var decoded struct {
		Callers []map[string]any `json:"callers"`
	}
	if err := json.Unmarshal(compactBytes, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, caller := range decoded.Callers {
		for _, droppedKey := range []string{"docstring", "signature", "id", "language"} {
			if _, ok := caller[droppedKey]; ok {
				t.Fatalf("compact caller JSON should never contain a %q key, got %v", droppedKey, caller)
			}
		}
	}
}

// TestTraceSymbolCompactTrimsMainSymbolDocstringToFirstSentence covers the
// final known-symbol-lookup token reduction: the queried symbol's own
// Docstring is the one field Compact mode deliberately doesn't drop outright
// (it's the actual answer to what was
// asked about), but a long, multi-sentence docstring — the common shape
// for KDoc/Javadoc-style conventions, which routinely pad a one-sentence
// summary with "@param" tags and "[Report a problem](url)"-style
// boilerplate — is still real, avoidable cost. Compact trims it to its
// first sentence (firstSentenceOrLimit); non-compact must keep the whole
// thing unchanged, and signature/id/language on the main Symbol must be
// completely unaffected by Compact either way (Compact's id/language drop
// only ever applies to Callers/Callees entries, never the main Symbol).
func TestTraceSymbolCompactTrimsMainSymbolDocstringToFirstSentence(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/lib.go", `package lib

// Helper does a thing. It has several other sentences describing edge
// cases and parameter semantics that an agent does not need to fully
// reproduce just to know it exists and what it returns.
func Helper(x int) int {
	return x + 1
}

func Caller() int {
	return Helper(1)
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	full := TraceSymbolWithOptions(idx, "Helper", TraceSymbolOptions{})
	if full.Symbol == nil {
		t.Fatal("expected a resolved Symbol")
	}
	fullDoc := full.Symbol.Docstring
	if !strings.Contains(fullDoc, "edge") {
		t.Fatalf("expected non-compact docstring to keep later sentences, got %q", fullDoc)
	}

	compact := TraceSymbolWithOptions(idx, "Helper", TraceSymbolOptions{Compact: true})
	if compact.Symbol == nil {
		t.Fatal("expected a resolved Symbol in compact mode too")
	}
	compactDoc := compact.Symbol.Docstring
	if !strings.HasPrefix(compactDoc, "Helper does a thing.") {
		t.Fatalf("expected compact docstring to keep the first sentence, got %q", compactDoc)
	}
	if strings.Contains(compactDoc, "edge") {
		t.Fatalf("expected compact docstring to drop later sentences, got %q", compactDoc)
	}
	if len(compactDoc) >= len(fullDoc) {
		t.Fatalf("expected compact docstring to be shorter: compact=%d full=%d", len(compactDoc), len(fullDoc))
	}

	// Signature/id/language on the main Symbol are never touched by Compact.
	if compact.Symbol.Signature != full.Symbol.Signature {
		t.Fatalf("expected Compact to leave the main Symbol's signature unchanged, got %q vs %q", compact.Symbol.Signature, full.Symbol.Signature)
	}
	if compact.Symbol.ID != full.Symbol.ID || compact.Symbol.Language != full.Symbol.Language {
		t.Fatalf("expected Compact to leave the main Symbol's id/language unchanged, got %#v vs %#v", compact.Symbol, full.Symbol)
	}
}

// TestFirstSentenceOrLimit exercises firstSentenceOrLimit directly against
// the edge cases its own doc comment calls out: empty input, an
// abbreviation right at the start that must not be mistaken for a
// sentence break, a long run-on paragraph with no early period at all
// (falls back to the hard length cap), and an already-short single
// sentence that should pass through unchanged.
func TestFirstSentenceOrLimit(t *testing.T) {
	if got := firstSentenceOrLimit(""); got != "" {
		t.Fatalf("empty input: got %q, want empty", got)
	}

	short := "Helper does a thing."
	if got := firstSentenceOrLimit(short); got != short {
		t.Fatalf("already-short docstring should pass through unchanged: got %q, want %q", got, short)
	}

	multi := "Helper does a thing. It also does several other things across many more words."
	if got := firstSentenceOrLimit(multi); got != "Helper does a thing." {
		t.Fatalf("expected only the first sentence, got %q", got)
	}

	// "e.g." right at the start must not be mistaken for the sentence's
	// own end — minSentenceLen guards against this.
	abbrev := "e.g. this still counts as one sentence overall, no early break."
	if got := firstSentenceOrLimit(abbrev); got != abbrev {
		t.Fatalf("expected the abbreviation near the start not to trigger a false sentence break, got %q", got)
	}

	runOn := strings.Repeat("word ", 60) // 300 chars, no periods at all
	got := firstSentenceOrLimit(runOn)
	// firstSentenceOrLimit cuts at the nearest preceding word boundary, not
	// exactly at compactDocstringMaxLen, so allow a little slop — the point
	// of this assertion is "bounded," not "exactly N chars".
	if len(got) > compactDocstringMaxLen+10 {
		t.Fatalf("expected a run-on paragraph to be capped at ~%d chars, got %d: %q", compactDocstringMaxLen, len(got), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected a truncated run-on paragraph to end with an ellipsis marker, got %q", got)
	}
}

// TestTraceSymbolCompactMainSymbolJSONDropsIDAndLanguage closes more of the
// remaining known-symbol-lookup token cost: the queried symbol's own
// id/startColumn/endLine/endColumn/exported/parentId
// previously always serialized in full even in Compact mode (only
// Docstring was trimmed), though compact output needs none of these seven
// fields. Verifies the actual marshaled JSON (not just Go struct values, which TraceSymbolResponse never
// mutates — only MarshalJSON's *rendering* differs) drops them in compact
// mode while keeping name/kind/file/startLine/signature/docstring/
// confidence, and that the default (non-compact) path's JSON is
// completely unaffected by MarshalJSON existing at all.
func TestTraceSymbolCompactMainSymbolJSONDropsIDAndLanguage(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/lib.go", `package lib

// Helper does a thing.
func Helper(x int) int {
	return x + 1
}

func Caller() int {
	return Helper(1)
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	full := TraceSymbolWithOptions(idx, "Helper", TraceSymbolOptions{})
	fullBytes, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	var fullDecoded map[string]any
	if err := json.Unmarshal(fullBytes, &fullDecoded); err != nil {
		t.Fatal(err)
	}
	fullSymbol, _ := fullDecoded["symbol"].(map[string]any)
	for _, wantKey := range []string{"id", "language", "startColumn", "endLine", "endColumn", "confidence"} {
		if _, ok := fullSymbol[wantKey]; !ok {
			t.Fatalf("expected non-compact symbol JSON to keep %q, got %v", wantKey, fullSymbol)
		}
	}

	compact := TraceSymbolWithOptions(idx, "Helper", TraceSymbolOptions{Compact: true})
	compactBytes, err := json.Marshal(compact)
	if err != nil {
		t.Fatal(err)
	}
	var compactDecoded map[string]any
	if err := json.Unmarshal(compactBytes, &compactDecoded); err != nil {
		t.Fatal(err)
	}
	compactSymbol, ok := compactDecoded["symbol"].(map[string]any)
	if !ok {
		t.Fatalf("expected a symbol object in compact JSON, got %v", compactDecoded)
	}
	for _, droppedKey := range []string{"id", "language", "startColumn", "endLine", "endColumn", "exported", "parentId"} {
		if _, ok := compactSymbol[droppedKey]; ok {
			t.Fatalf("expected compact main symbol JSON to drop %q, got %v", droppedKey, compactSymbol)
		}
	}
	for _, keptKey := range []string{"name", "kind", "file", "startLine", "signature", "docstring", "confidence"} {
		if _, ok := compactSymbol[keptKey]; !ok {
			t.Fatalf("expected compact main symbol JSON to keep %q, got %v", keptKey, compactSymbol)
		}
	}
	if len(compactBytes) >= len(fullBytes) {
		t.Fatalf("expected the compact response to be smaller: compact=%d full=%d", len(compactBytes), len(fullBytes))
	}
}

// TestImpactCompactMainSymbolJSONDropsIDAndLanguage mirrors
// TestTraceSymbolCompactMainSymbolJSONDropsIDAndLanguage for Impact's own
// MarshalJSON (impact.go) — same mechanism, applied to ImpactResponse.
func TestImpactCompactMainSymbolJSONDropsIDAndLanguage(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/lib.go", `package lib

// Helper does a thing.
func Helper(x int) int {
	return x + 1
}

func Caller() int {
	return Helper(1)
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	full := Impact(idx, "Helper", 1)
	fullBytes, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	var fullDecoded map[string]any
	if err := json.Unmarshal(fullBytes, &fullDecoded); err != nil {
		t.Fatal(err)
	}
	fullSymbol, _ := fullDecoded["symbol"].(map[string]any)
	if _, ok := fullSymbol["id"]; !ok {
		t.Fatalf("expected non-compact impact symbol JSON to keep id, got %v", fullSymbol)
	}
	if _, ok := fullSymbol["language"]; !ok {
		t.Fatalf("expected non-compact impact symbol JSON to keep language, got %v", fullSymbol)
	}

	compact := ImpactWithOptions(idx, "Helper", ImpactOptions{Depth: 1, Compact: true})
	compactBytes, err := json.Marshal(compact)
	if err != nil {
		t.Fatal(err)
	}
	var compactDecoded map[string]any
	if err := json.Unmarshal(compactBytes, &compactDecoded); err != nil {
		t.Fatal(err)
	}
	compactSymbol, ok := compactDecoded["symbol"].(map[string]any)
	if !ok {
		t.Fatalf("expected a symbol object in compact impact JSON, got %v", compactDecoded)
	}
	for _, droppedKey := range []string{"id", "language"} {
		if _, ok := compactSymbol[droppedKey]; ok {
			t.Fatalf("expected compact impact main symbol JSON to drop %q, got %v", droppedKey, compactSymbol)
		}
	}
	for _, keptKey := range []string{"name", "kind", "file", "startLine", "signature", "docstring"} {
		if _, ok := compactSymbol[keptKey]; !ok {
			t.Fatalf("expected compact impact main symbol JSON to keep %q, got %v", keptKey, compactSymbol)
		}
	}
	if len(compactBytes) >= len(fullBytes) {
		t.Fatalf("expected the compact impact response to be smaller: compact=%d full=%d", len(compactBytes), len(fullBytes))
	}
}

// TestTraceSymbolCompactSuppressesCallerSites guards the other half of
// reducing token cost: CallerSites duplicates the same who-calls-this
// information at individual call sites that Callers
// already carries per distinct caller symbol — real, additional cost for
// compact's whole point ("the leanest possible identify+locate view").
// Compact must drop CallerSites even when the caller explicitly requests
// Sites: true, since the two options otherwise contradict each other and
// compact should win that contradiction — and must leave CallerSites
// completely alone when Compact is false (the explicit backward-
// compatibility requirement for this opt-in flag).
func TestTraceSymbolCompactSuppressesCallerSites(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/lib.go", `package lib

func Helper(x int) int {
	return x + 1
}

func Caller() int {
	return Helper(1)
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	full := TraceSymbolWithOptions(idx, "Helper", TraceSymbolOptions{Sites: true})
	if len(full.CallerSites) == 0 {
		t.Fatalf("expected non-compact trace with Sites:true to include caller sites, got %#v", full)
	}

	compact := TraceSymbolWithOptions(idx, "Helper", TraceSymbolOptions{Sites: true, Compact: true})
	if len(compact.CallerSites) != 0 {
		t.Fatalf("expected compact trace to suppress caller sites even with Sites:true, got %#v", compact.CallerSites)
	}
}

// TestTraceSymbolCompactDropsLanguageFromTestCallerGroups is the
// regression guard for a real bug caught while regression-testing the
// compact-mode token fix across multiple language fixtures: grouped test-
// caller rows ("test callbacks", kind "test-callback-group") are built by
// addTestCallerGroup, a separate construction path from summarize/
// summarizeSymbolCompact entirely — compact never reached it, so a symbol
// whose only callers are test callbacks (the common case for a small
// helper function) kept Language on its grouped caller row even with
// Compact: true, while every other caller/callee entry had it dropped.
// These synthetic rows never had an ID to begin with (no single real
// symbol backs a group of many test callers), so only Language needs
// checking here.
func TestTraceSymbolCompactDropsLanguageFromTestCallerGroups(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/lib.ts", `export function useDataset() { return { data: 1 } }
`)
	write(t, root, "tests/useDataset.test.ts", `import { useDataset } from '../src/lib'

describe('useDataset', () => {
  it('returns the dataset', () => {
    useDataset()
  })
  it('does work twice', () => {
    useDataset()
    useDataset()
  })
})
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	full := TraceSymbolWithOptions(idx, "useDataset", TraceSymbolOptions{})
	foundGroupFull := false
	for _, c := range full.Callers {
		if c.Kind == "test-callback-group" {
			foundGroupFull = true
			if c.Language == "" {
				t.Fatalf("expected non-compact test-callback-group row to keep language, got %#v", c)
			}
		}
	}
	if !foundGroupFull {
		t.Fatalf("expected a test-callback-group caller, got %#v", full.Callers)
	}

	compact := TraceSymbolWithOptions(idx, "useDataset", TraceSymbolOptions{Compact: true})
	foundGroupCompact := false
	for _, c := range compact.Callers {
		if c.Kind == "test-callback-group" {
			foundGroupCompact = true
			if c.Language != "" {
				t.Fatalf("expected compact test-callback-group row to drop language too, got %#v", c)
			}
		}
	}
	if !foundGroupCompact {
		t.Fatalf("expected a test-callback-group caller in compact mode too, got %#v", compact.Callers)
	}
}

func TestTraceSymbolCompactTrimsTestCallerGroupDetails(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/lib.ts", `export function route(path) {
  return path
}
`)
	write(t, root, "test/app.route.js", `it('first route test', () => {
  route('/a')
})
it('second route test', () => {
  route('/b')
})
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	full := TraceSymbolWithOptions(idx, "route", TraceSymbolOptions{})
	fullGroup := CGPSymbolSummary{}
	for _, caller := range full.Callers {
		if caller.Kind == "test-callback-group" {
			fullGroup = caller
			break
		}
	}
	if len(fullGroup.Lines) == 0 || len(fullGroup.NamesPreview) == 0 {
		t.Fatalf("non-compact test group should keep line/name previews, got %#v", full.Callers)
	}

	compact := TraceSymbolWithOptions(idx, "route", TraceSymbolOptions{Compact: true})
	for _, caller := range compact.Callers {
		if caller.Kind != "test-callback-group" {
			continue
		}
		if caller.Count != 2 || caller.StartLine <= 0 {
			t.Fatalf("compact group should keep count and representative line, got %#v", caller)
		}
		if caller.Language != "" || len(caller.Lines) != 0 || len(caller.NamesPreview) != 0 {
			t.Fatalf("compact group should omit language and per-test arrays, got %#v", caller)
		}
		return
	}
	t.Fatalf("expected compact test-callback-group caller, got %#v", compact.Callers)
}

// TestTraceSymbolCompactOptionAppliesToCandidateDetails covers the
// ambiguous-name path: Compact must propagate into
// TraceSymbolResponse.CandidateDetails[i].Callers too (each candidate's
// own inline trace), not just the single-match path.
func TestTraceSymbolCompactOptionAppliesToCandidateDetails(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.go", `package a

func Helper(x int) int { return x }
func CallerA() int { return Helper(1) }
`)
	write(t, root, "b.go", `package b

func Helper(x int) int { return x }
func CallerB() int { return Helper(1) }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := TraceSymbolWithOptions(idx, "Helper", TraceSymbolOptions{Sites: true, Compact: true})
	if resp.Status != "ambiguous" {
		t.Fatalf("expected ambiguous status for two Helper definitions, got %#v", resp)
	}
	if len(resp.CandidateDetails) == 0 {
		t.Fatalf("expected inline candidate details for 2 candidates, got %#v", resp)
	}
	for _, candidate := range resp.Candidates {
		// ID is deliberately dropped from compact candidates: it embeds the
		// file path a second time (already sent in File), and re-querying with
		// "<file>:<name>" resolves exactly via findSymbols' file:name clause.
		// File+Name+StartLine+Signature are the disambiguation contract now.
		if candidate.ID != "" {
			t.Fatalf("compact ambiguous candidate should omit the id (file:name re-queries instead), got %#v", candidate)
		}
		if candidate.File == "" || candidate.Name == "" || candidate.StartLine == 0 || candidate.Signature == "" {
			t.Fatalf("compact ambiguous candidate must keep file/name/startLine/signature for disambiguation, got %#v", candidate)
		}
		if candidate.Docstring != "" || candidate.Complexity != 0 || candidate.Language != "" {
			t.Fatalf("compact ambiguous candidate should omit descriptive metrics, got %#v", candidate)
		}
		// The advertised re-query form must actually resolve.
		requery := TraceSymbolWithOptions(idx, candidate.File+":"+candidate.Name, TraceSymbolOptions{Sites: true, Compact: true})
		if requery.Status != "found" {
			t.Fatalf("file:name re-query %q should resolve, got %s", candidate.File+":"+candidate.Name, requery.Status)
		}
	}
	for _, detail := range resp.CandidateDetails {
		if detail.Symbol == nil || detail.Symbol.File == "" || detail.Symbol.Signature == "" {
			t.Fatalf("compact candidate detail must retain candidate identity, got %#v", detail.Symbol)
		}
		for _, caller := range detail.Callers {
			if caller.Signature != "" || caller.Docstring != "" {
				t.Fatalf("expected compact candidate-detail callers to drop signature/docstring, got %#v", caller)
			}
		}
	}
}

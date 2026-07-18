package mamari

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

func completeV2SnapshotFixture() indexSnapshot {
	loc := Location{File: "src/main.ts", StartLine: 7, StartColumn: 3, EndLine: 8, EndColumn: 11, Kind: "call", Raw: "helper(value)"}
	root := CGPSymbol{
		ID: "symbol:main", Name: "main", Kind: "function", Language: "typescript", File: "src/main.ts",
		StartLine: 5, StartColumn: 1, EndLine: 10, EndColumn: 2, Signature: "main(value: string): Promise<Result>",
		Docstring: "Runs the operation.", ReturnTypes: []string{"Promise", "Result"}, ReceiverType: "Service",
		Exported: true, ParentID: "symbol:file", Confidence: ConfExact, SCIPSymbol: "scip-typescript npm pkg main().",
		Complexity: 4, LoopDepth: 1, TransitiveLoopDepth: 2, LinearScanInLoop: 1, AllocInLoop: 2,
		RecursionInLoop: true, ShapeHash: "shape:abc",
	}
	target := CGPSymbol{ID: "symbol:helper", Name: "helper", Kind: "function", Language: "typescript", File: "src/helper.ts", StartLine: 2, EndLine: 4, Confidence: ConfScoped}
	cgpEdge := CGPEdge{From: root.ID, To: target.ID, Type: "calls", Confidence: ConfScoped, Evidence: loc}
	cgpEdge.ID = canonicalCGPEdgeID(cgpEdge.From, cgpEdge.To, cgpEdge.Type, cgpEdge.Evidence)
	return indexSnapshot{
		SchemaVersion: SchemaVersion,
		Repo:          RepoInfo{Root: "/repo", IndexedAt: "2026-07-15T00:00:00Z", GitCommit: "abc123"},
		Files: map[string]File{
			"src/main.ts": {
				ID: "file:main", Path: "src/main.ts", Language: "typescript", SHA256: "deadbeef", LineCount: 10,
				Prefixes: map[string]string{"ex": "https://example.test/"}, Parser: "tree-sitter-typescript",
				ParseStatus: ParseStatusPartial, ParseError: "recovered fixture warning",
			},
		},
		Prefixes: map[string]Prefix{"ex": {Prefix: "ex", IRI: "https://example.test/", Location: loc}},
		Terms: map[string]Term{"term:Thing": {
			ID: "term:Thing", Term: "ex:Thing", IRI: "https://example.test/Thing", Prefix: "ex", LocalName: "Thing", Locations: []Location{loc},
		}},
		Shapes: map[string]Shape{"shape:Thing": {
			ID: "shape:Thing", TermID: "term:Thing", Term: "ex:ThingShape", IRI: "https://example.test/ThingShape", Location: loc,
			TargetClasses: []ShapeLink{{Predicate: "sh:targetClass", Term: "ex:Thing", IRI: "https://example.test/Thing", Location: loc}},
			Paths:         []ShapeLink{{Predicate: "sh:path", Term: "ex:name", IRI: "https://example.test/name", Location: loc}},
			Nodes:         []ShapeLink{{Predicate: "sh:node", Term: "ex:Nested", IRI: "https://example.test/Nested", Location: loc}},
			Predicates:    []ShapeLink{{Predicate: "sh:class", Term: "ex:Class", IRI: "https://example.test/Class", Location: loc}},
			Branches:      []Branch{{Kind: "or", Name: "choice", Datatype: "xsd:string", DatatypeIRI: "http://www.w3.org/2001/XMLSchema#string", Pattern: "[a-z]+", Path: "ex:name", PathIRI: "https://example.test/name", Location: loc}},
			Names:         []Literal{{Predicate: "sh:name", Value: "Thing", Lang: "en", Location: loc, ShapeID: "shape:Thing"}},
			Unsupported:   []Location{loc},
		}},
		References: []Reference{{
			ID: "ref:1", TermID: "term:Thing", Term: "ex:Thing", IRI: "https://example.test/Thing", File: "src/main.ts",
			StartLine: 7, StartColumn: 3, EndLine: 7, EndColumn: 11, Confidence: ConfExact, Kind: "identifier", Context: "Thing",
		}},
		Edges:           []Edge{{ID: "edge:1", From: "shape:Thing", To: "term:Thing", Type: "targets", Confidence: ConfExact, Evidence: loc}},
		DynamicIRICalls: []DynamicIRICall{{File: "src/main.ts", Line: 9, Column: 4, Callee: "namedNode", Snippet: "namedNode(value)"}},
		Symbols:         map[string]CGPSymbol{root.ID: root, target.ID: target},
		SymbolEdges: []CGPEdge{cgpEdge, {
			ID: "external:custom-edge-id", From: target.ID, To: "unresolved:typescript#later", Type: "calls",
			Confidence: ConfUnresolved, UnresolvedReason: ReasonDynamicReceiver, Evidence: loc,
		}},
	}
}

func TestBinaryIndexV2RoundTripsEveryPersistedField(t *testing.T) {
	want := completeV2SnapshotFixture()
	data, err := marshalBinaryIndexV2(want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, indexBinaryMagicV2) {
		t.Fatalf("index starts with %q, want v2 magic", data[:min(len(data), len(indexBinaryMagicV2))])
	}
	idx, err := unmarshalBinaryIndexV2(data)
	if err != nil {
		t.Fatal(err)
	}
	got := idx.snapshot()
	if !reflect.DeepEqual(got, want) {
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("v2 round trip changed the snapshot\nwant: %s\n got: %s", wantJSON, gotJSON)
	}
	// Repeated values must share the v2 table's backing bytes, not merely be
	// value-equal fresh allocations.
	if unsafe.StringData(idx.Symbols["symbol:main"].ID) != unsafe.StringData(idx.SymbolEdges[0].From) {
		t.Fatal("v2 decoder did not preserve canonical string-table sharing")
	}
}

func TestBinaryIndexV2IsDeterministicAndChecksummed(t *testing.T) {
	snapshot := completeV2SnapshotFixture()
	first, err := marshalBinaryIndexV2(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	second, err := marshalBinaryIndexV2(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("identical snapshots produced different v2 bytes")
	}
	corrupt := append([]byte(nil), first...)
	corrupt[len(indexBinaryMagicV2)+5] ^= 0x40
	if _, err := unmarshalBinaryIndexV2(corrupt); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("corrupt v2 error = %v, want checksum mismatch", err)
	}
}

func TestLoadIndexReadsLegacyV1AndWritesV2(t *testing.T) {
	snapshot := completeV2SnapshotFixture()
	legacy, err := marshalBinaryIndexV1(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "index.bin")
	if err := os.WriteFile(path, legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := LoadIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveIndex(idx, path); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(written, indexBinaryMagicV2) {
		t.Fatal("SaveIndex did not upgrade a loaded v1 index to v2")
	}
	reloaded, err := LoadIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reloaded.snapshot(), snapshot) {
		t.Fatal("v1 -> v2 upgrade changed persisted data")
	}
}

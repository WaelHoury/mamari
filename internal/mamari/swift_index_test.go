package mamari

import "testing"

func TestSwiftConditionalCompilationBuildsImportGraph(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Sources/App.swift", `import Foundation
import struct Networking.Request

#if canImport(UIKit)
func platformHelper() {}
#else
func platformHelperFallback() {}
#endif

func run() {
    platformHelper()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	file, ok := idx.Files["Sources/App.swift"]
	if !ok {
		t.Fatal("Swift source was not indexed")
	}
	if file.ParseStatus != ParseStatusOK || file.Parser != "tree-sitter-swift" {
		t.Fatalf("unexpected Swift parse metadata: %#v", file)
	}
	for _, name := range []string{"platformHelper", "platformHelperFallback", "run"} {
		if len(findSymbols(idx, name)) == 0 {
			t.Fatalf("missing Swift symbol %q", name)
		}
	}
	imports := map[string]bool{}
	for _, edge := range idx.SymbolEdges {
		if edge.Type == "imports" && edge.From == fileSymbolID("Sources/App.swift") {
			imports[edge.To] = true
		}
	}
	for _, target := range []string{"module:Foundation", "module:Networking.Request"} {
		if !imports[target] {
			t.Fatalf("missing Swift import edge %q; imports=%v", target, imports)
		}
	}
}

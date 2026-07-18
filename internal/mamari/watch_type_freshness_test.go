package mamari

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRebakeDoesNotReuseRemovedPythonTypeFacts(t *testing.T) {
	root := t.TempDir()
	write(t, root, "primary.py", "class Repo:\n    def find(self):\n        return 1\n")
	write(t, root, "decoy.py", "class OtherRepo:\n    def find(self):\n        return 2\n")
	write(t, root, "service.py", `class Service:
    def __init__(self):
        self.repo = Repo()

    def run(self):
        return self.repo.find()
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	updated := `class Service:
    def run(self):
        return self.repo.find()
`
	if err := os.WriteFile(filepath.Join(root, "service.py"), []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	dropFile(idx, "service.py")
	idx.addOrUpdateFile("service.py", []byte(updated))
	ScanCGPSymbols(idx, "service.py", "python", updated)
	idx.invalidateFileSymbolIndex()
	idx.ensureFileSymbolIndex()
	ScanCGPRelations(idx, "service.py", "python", updated)

	for _, edge := range idx.SymbolEdges {
		if edge.Evidence.File != "service.py" || edge.Evidence.Raw != "self.repo.find" {
			continue
		}
		if edge.Confidence != ConfUnresolved || edge.UnresolvedReason != ReasonAmbiguousName {
			t.Fatalf("rebake reused a removed self.repo type fact: %#v", edge)
		}
		return
	}
	t.Fatal("rebaked self.repo.find edge not found")
}

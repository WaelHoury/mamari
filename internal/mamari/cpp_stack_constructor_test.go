package mamari

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCppStackConstructionResolvesClassWithoutPrimitiveOrPrototypeCalls(t *testing.T) {
	root := t.TempDir()
	source := `class Repo {
public:
    Repo(int);
    void find() {}
};

Repo make_repo(int id);

void use_repo() {
    Repo repo{1};
    int count(5);
    repo.find();
}
`
	if err := os.WriteFile(filepath.Join(root, "repo.cpp"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	snap := idx.snapshot()

	var useID, repoID, findID string
	for _, sym := range snap.Symbols {
		switch {
		case sym.Name == "use_repo":
			useID = sym.ID
		case sym.Name == "Repo" && sym.Kind == "class":
			repoID = sym.ID
		case sym.Name == "find":
			findID = sym.ID
		}
	}
	if useID == "" || repoID == "" || findID == "" {
		t.Fatalf("missing fixture symbols: use=%q repo=%q find=%q", useID, repoID, findID)
	}

	gotRepo, gotFind := false, false
	for _, edge := range snap.SymbolEdges {
		if edge.From != useID || edge.Type != "calls" {
			continue
		}
		switch edge.To {
		case repoID:
			gotRepo = true
		case findID:
			gotFind = true
		}
		if strings.Contains(edge.To, "int") || strings.Contains(edge.To, "make_repo") {
			t.Fatalf("primitive initializer or prototype became a call edge: %#v", edge)
		}
	}
	if !gotRepo || !gotFind {
		t.Fatalf("stack construction edges: class=%v method=%v, edges=%#v", gotRepo, gotFind, snap.SymbolEdges)
	}
}

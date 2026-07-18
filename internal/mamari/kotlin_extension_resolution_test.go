package mamari

import (
	"path/filepath"
	"testing"
)

func TestKotlinExtensionCallResolvesByReceiverType(t *testing.T) {
	root := t.TempDir()
	write(t, root, "extensions.kt", `fun String.normalized(): String = this
fun Int.normalized(): Int = this

fun normalizeText(value: String): String {
    return value.normalized()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	var stringExtensionID, callerID string
	for _, sym := range idx.Symbols {
		switch {
		case sym.Name == "normalized" && sym.ParentID == "":
			// Extension methods may remain file-parented when their receiver
			// is a standard-library type absent from the repo. Identify the
			// desired overload by its stable qualified ID.
			if sym.ID == "symbol:kotlin:method:extensions.kt:String.normalized" {
				stringExtensionID = sym.ID
			}
		case sym.Name == "normalizeText":
			callerID = sym.ID
		}
	}
	if stringExtensionID == "" {
		stringExtensionID = "symbol:kotlin:method:extensions.kt:String.normalized"
		if _, ok := idx.Symbols[stringExtensionID]; !ok {
			t.Fatalf("String.normalized symbol not found; symbols=%#v", idx.Symbols)
		}
	}
	if callerID == "" {
		t.Fatal("normalizeText symbol not found")
	}
	for _, edge := range idx.SymbolEdges {
		if edge.From == callerID && edge.Type == "calls" && edge.Evidence.Raw == "value.normalized" {
			if edge.To != stringExtensionID || edge.Confidence != ConfScoped {
				t.Fatalf("typed extension call resolved incorrectly: %#v", edge)
			}
			indexPath := filepath.Join(root, ".mamari", "index.json")
			if err := SaveIndex(idx, indexPath); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadIndex(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			if got := findExtensionMethod(loaded, "kotlin", "String", "normalized"); got != stringExtensionID {
				t.Fatalf("loaded extension index resolved %q, want %q", got, stringExtensionID)
			}
			return
		}
	}
	t.Fatal("value.normalized call edge not found")
}

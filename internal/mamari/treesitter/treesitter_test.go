package treesitter

import "testing"

func TestParsePythonRootNode(t *testing.T) {
	src := []byte("def foo():\n    pass\n")
	tree, err := ParsePython(src)
	if err != nil {
		t.Fatalf("ParsePython: %v", err)
	}
	defer tree.Close()

	root := tree.RootNode()
	if root.Kind() != "module" {
		t.Fatalf("root kind = %q, want %q", root.Kind(), "module")
	}
	if root.HasError() {
		t.Fatalf("unexpected parse error in %q", src)
	}
}

func TestParsePythonSyntaxError(t *testing.T) {
	src := []byte("def foo(:\n    pass\n")
	tree, err := ParsePython(src)
	if err != nil {
		t.Fatalf("ParsePython: %v", err)
	}
	defer tree.Close()

	if !tree.RootNode().HasError() {
		t.Fatalf("expected parse error for invalid syntax %q", src)
	}
}

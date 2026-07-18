// Package treesitter provides minimal real bindings to the official
// go-tree-sitter library and per-language grammars, used as the foundation
// for production-grade structural parsing (starting with Python).
package treesitter

import (
	sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_python "github.com/tree-sitter/tree-sitter-python/bindings/go"
)

// ParsePython parses src as Python source and returns the resulting tree.
// Callers must call tree.Close() when done.
func ParsePython(src []byte) (*sitter.Tree, error) {
	lang := sitter.NewLanguage(tree_sitter_python.Language())
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(lang); err != nil {
		return nil, err
	}
	return parser.Parse(src, nil), nil
}

// Package elixirgrammar wraps the tree-sitter Elixir grammar's C parser
// directly. Upstream moved from github.com/tree-sitter/tree-sitter-elixir to
// github.com/elixir-lang/tree-sitter-elixir without changing the module path
// in go.mod. Depending on it therefore requires a replace directive, which
// makes mamari impossible to install with `go install ...@version`.
//
// parser.c, scanner.c, and parser.h are vendored verbatim from upstream v0.3.5.
// Update them together from the src directory of a newer upstream release.
package elixirgrammar

// #cgo CFLAGS: -std=c11 -fPIC
// typedef struct TSLanguage TSLanguage;
// const TSLanguage *tree_sitter_elixir(void);
import "C"

import "unsafe"

// Language returns the tree-sitter Language for the Elixir grammar.
func Language() unsafe.Pointer {
	return unsafe.Pointer(C.tree_sitter_elixir())
}

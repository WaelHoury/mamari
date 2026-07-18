// Package clojuregrammar wraps the tree-sitter Clojure grammar's C parser
// directly, since the upstream repo (github.com/sogaiu/tree-sitter-clojure,
// CC0-1.0 licensed) publishes no Go bindings — unlike every other grammar
// used by mamari, which is pulled in via a plain `go get` of an
// upstream-published bindings/go package. parser.c is vendored verbatim
// (v0.0.13) below; bump it by copying a newer tagged release's src/parser.c
// and src/tree_sitter/parser.h over these files (there is no `go get` path
// to follow for version updates, since there's no Go module to point at).
package clojuregrammar

// #cgo CFLAGS: -std=c11 -fPIC
// typedef struct TSLanguage TSLanguage;
// const TSLanguage *tree_sitter_clojure(void);
import "C"

import "unsafe"

// Language returns the tree-sitter Language for the Clojure grammar.
func Language() unsafe.Pointer {
	return unsafe.Pointer(C.tree_sitter_clojure())
}

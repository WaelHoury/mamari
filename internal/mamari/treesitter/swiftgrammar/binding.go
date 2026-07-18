// Package swiftgrammar wraps the tree-sitter Swift grammar's C parser
// directly. The actively-maintained upstream repo
// (github.com/alex-pinkus/tree-sitter-swift, MIT licensed) publishes no
// go-gettable Go module that actually builds: its bindings/go package's
// cgo preamble includes src/parser.c, but that generated file (~18MB) is
// not committed to the grammar's git repository — it's only produced at
// release time and bundled into the npm package. parser.c/scanner.c here
// are vendored verbatim from the published npm release (tree-sitter-swift
// v0.7.1); bump them by extracting a newer release's tarball
// (`npm pack tree-sitter-swift@<version>`) and copying its src/parser.c,
// src/scanner.c, and src/tree_sitter/parser.h over these files — there is
// no `go get -u` path to follow for version updates, since there's no Go
// module to point at. Same situation and same fix as this repo's
// clojuregrammar package one directory up.
package swiftgrammar

// #cgo CFLAGS: -std=c11 -fPIC
// typedef struct TSLanguage TSLanguage;
// const TSLanguage *tree_sitter_swift(void);
import "C"

import "unsafe"

// Language returns the tree-sitter Language for the Swift grammar.
func Language() unsafe.Pointer {
	return unsafe.Pointer(C.tree_sitter_swift())
}

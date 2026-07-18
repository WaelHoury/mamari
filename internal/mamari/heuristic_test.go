package mamari

import "testing"

func TestHeuristicFallbackExtractorAcrossLanguages(t *testing.T) {
	root := t.TempDir()
	write(t, root, "go/main.go", `package main

import (
	"fmt"
	"os"
)

type Server struct {
	Addr string
}

type Runner interface {
	Run() error
}

func NewServer(addr string) *Server {
	return &Server{Addr: addr}
}

func helper() {
	fmt.Println(os.Args)
}
`)
	write(t, root, "rust/lib.rs", `use std::collections::HashMap;

pub struct Config {
    name: String,
}

pub trait Loader {
    fn load(&self) -> Config;
}

pub fn run() {
    let _ = HashMap::<String, String>::new();
}
`)
	write(t, root, "ruby/app.rb", `require 'json'

class Widget
  def initialize(name)
    @name = name
  end

  def to_s
    @name
  end
end

def top_level_helper
  Widget.new('x')
end
`)

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	// Go (tree-sitter): struct, interface, two functions, plus an import edge.
	for _, q := range []string{"Server", "Runner", "NewServer", "helper"} {
		got := ListSymbols(idx, q, "", "")
		if len(got.Symbols) == 0 {
			t.Fatalf("expected Go symbol for %q, got none", q)
		}
		if got.Symbols[0].Language != "go" {
			t.Fatalf("expected language go for %q, got %q", q, got.Symbols[0].Language)
		}
	}
	if file, ok := idx.Files["go/main.go"]; !ok || file.Parser != "tree-sitter-go" {
		t.Fatalf("expected go/main.go to be parsed by tree-sitter-go, got %#v", file)
	}

	// Rust (tree-sitter): struct, trait, function, all at exact confidence.
	for _, q := range []string{"Config", "Loader", "run"} {
		got := ListSymbols(idx, q, "", "")
		if len(got.Symbols) == 0 {
			t.Fatalf("expected Rust symbol for %q, got none", q)
		}
		if got.Symbols[0].Language != "rust" {
			t.Fatalf("expected language rust for %q, got %q", q, got.Symbols[0].Language)
		}
	}
	if file, ok := idx.Files["rust/lib.rs"]; !ok || file.Parser != "tree-sitter-rust" {
		t.Fatalf("expected rust/lib.rs to be parsed by tree-sitter-rust, got %#v", file)
	}

	// Ruby (tree-sitter): class, top-level def, AND nested methods
	// (initialize/to_s) — real structural parsing finds nested methods the
	// old heuristic fallback could not.
	for _, q := range []string{"Widget", "top_level_helper", "initialize", "to_s"} {
		got := ListSymbols(idx, q, "", "")
		if len(got.Symbols) == 0 {
			t.Fatalf("expected Ruby symbol for %q, got none", q)
		}
		if got.Symbols[0].Language != "ruby" {
			t.Fatalf("expected language ruby for %q, got %q", q, got.Symbols[0].Language)
		}
	}
	if file, ok := idx.Files["ruby/app.rb"]; !ok || file.Parser != "tree-sitter-ruby" {
		t.Fatalf("expected ruby/app.rb to be parsed by tree-sitter-ruby, got %#v", file)
	}

	// Go/Rust/Ruby (all tree-sitter) symbols are exact.
	snap := idx.snapshot()
	for _, sym := range snap.Symbols {
		switch sym.File {
		case "go/main.go", "rust/lib.rs", "ruby/app.rb":
			if sym.Kind == "file" {
				continue
			}
			if sym.Confidence != ConfExact {
				t.Fatalf("expected exact confidence for %s/%s, got %q", sym.File, sym.Name, sym.Confidence)
			}
		}
	}

	// Import edges (tree-sitter for go/rust/ruby).
	foundGoImport := false
	foundRustUse := false
	foundRubyRequire := false
	for _, e := range snap.SymbolEdges {
		if e.Type != "imports" {
			continue
		}
		switch {
		case e.From == fileSymbolID("go/main.go") && e.To == "module:fmt":
			foundGoImport = true
		case e.From == fileSymbolID("rust/lib.rs") && e.To == "module:std::collections::HashMap":
			foundRustUse = true
		case e.From == fileSymbolID("ruby/app.rb") && e.To == "module:json":
			foundRubyRequire = true
		}
	}
	if !foundGoImport {
		t.Fatalf("expected an import edge for go/main.go -> module:fmt")
	}
	if !foundRustUse {
		t.Fatalf("expected a use edge for rust/lib.rs -> module:std::collections::HashMap")
	}
	if !foundRubyRequire {
		t.Fatalf("expected a require edge for ruby/app.rb -> module:json")
	}
}

func TestBraceBodyEndAndRubyBodyEnd(t *testing.T) {
	lines := []string{
		"func Foo() {",
		"  if true {",
		"    bar()",
		"  }",
		"}",
		"func Bar() {}",
	}
	if end := braceBodyEnd(lines, 1); end != 5 {
		t.Fatalf("expected Foo body to end at line 5, got %d", end)
	}
	if end := braceBodyEnd(lines, 6); end != 6 {
		t.Fatalf("expected single-line Bar body to end at line 6, got %d", end)
	}

	rubyLines := []string{
		"class Widget",
		"  def initialize",
		"    if true",
		"      do_thing",
		"    end",
		"  end",
		"end",
	}
	if end := rubyBodyEnd(rubyLines, 1); end != 7 {
		t.Fatalf("expected class Widget to end at line 7, got %d", end)
	}
}

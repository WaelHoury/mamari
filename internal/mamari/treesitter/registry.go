package treesitter

import (
	_ "embed"
	"unsafe"

	tree_sitter_dart "github.com/UserNobody14/tree-sitter-dart/bindings/go"
	tree_sitter_r "github.com/r-lib/tree-sitter-r/bindings/go"
	tree_sitter_hcl "github.com/tree-sitter-grammars/tree-sitter-hcl/bindings/go"
	tree_sitter_kotlin "github.com/tree-sitter-grammars/tree-sitter-kotlin/bindings/go"
	tree_sitter_lua "github.com/tree-sitter-grammars/tree-sitter-lua/bindings/go"
	tree_sitter_zig "github.com/tree-sitter-grammars/tree-sitter-zig/bindings/go"
	tree_sitter_bash "github.com/tree-sitter/tree-sitter-bash/bindings/go"
	tree_sitter_c_sharp "github.com/tree-sitter/tree-sitter-c-sharp/bindings/go"
	tree_sitter_c "github.com/tree-sitter/tree-sitter-c/bindings/go"
	tree_sitter_cpp "github.com/tree-sitter/tree-sitter-cpp/bindings/go"
	tree_sitter_go "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tree_sitter_haskell "github.com/tree-sitter/tree-sitter-haskell/bindings/go"
	tree_sitter_java "github.com/tree-sitter/tree-sitter-java/bindings/go"
	tree_sitter_julia "github.com/tree-sitter/tree-sitter-julia/bindings/go"
	tree_sitter_ocaml "github.com/tree-sitter/tree-sitter-ocaml/bindings/go"
	tree_sitter_php "github.com/tree-sitter/tree-sitter-php/bindings/go"
	tree_sitter_python "github.com/tree-sitter/tree-sitter-python/bindings/go"
	tree_sitter_ruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
	tree_sitter_rust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	tree_sitter_scala "github.com/tree-sitter/tree-sitter-scala/bindings/go"
	"github.com/waelhoury/mamari/internal/mamari/treesitter/clojuregrammar"
	"github.com/waelhoury/mamari/internal/mamari/treesitter/elixirgrammar"
	"github.com/waelhoury/mamari/internal/mamari/treesitter/swiftgrammar"
)

//go:embed queries/python/tags.scm
var pythonTagsQuery string

//go:embed queries/python/calls.scm
var pythonCallsQuery string

//go:embed queries/java/tags.scm
var javaTagsQuery string

//go:embed queries/java/calls.scm
var javaCallsQuery string

//go:embed queries/go/tags.scm
var goTagsQuery string

//go:embed queries/go/calls.scm
var goCallsQuery string

//go:embed queries/csharp/tags.scm
var csharpTagsQuery string

//go:embed queries/csharp/calls.scm
var csharpCallsQuery string

//go:embed queries/rust/tags.scm
var rustTagsQuery string

//go:embed queries/rust/calls.scm
var rustCallsQuery string

//go:embed queries/ruby/tags.scm
var rubyTagsQuery string

//go:embed queries/ruby/calls.scm
var rubyCallsQuery string

//go:embed queries/php/tags.scm
var phpTagsQuery string

//go:embed queries/php/calls.scm
var phpCallsQuery string

//go:embed queries/c/tags.scm
var cTagsQuery string

//go:embed queries/c/calls.scm
var cCallsQuery string

//go:embed queries/cpp/tags.scm
var cppTagsQuery string

//go:embed queries/cpp/calls.scm
var cppCallsQuery string

//go:embed queries/kotlin/tags.scm
var kotlinTagsQuery string

//go:embed queries/kotlin/calls.scm
var kotlinCallsQuery string

//go:embed queries/bash/tags.scm
var bashTagsQuery string

//go:embed queries/bash/calls.scm
var bashCallsQuery string

//go:embed queries/scala/tags.scm
var scalaTagsQuery string

//go:embed queries/scala/calls.scm
var scalaCallsQuery string

//go:embed queries/lua/tags.scm
var luaTagsQuery string

//go:embed queries/lua/calls.scm
var luaCallsQuery string

//go:embed queries/elixir/tags.scm
var elixirTagsQuery string

//go:embed queries/elixir/calls.scm
var elixirCallsQuery string

//go:embed queries/dart/tags.scm
var dartTagsQuery string

//go:embed queries/dart/calls.scm
var dartCallsQuery string

//go:embed queries/haskell/tags.scm
var haskellTagsQuery string

//go:embed queries/haskell/calls.scm
var haskellCallsQuery string

//go:embed queries/clojure/tags.scm
var clojureTagsQuery string

//go:embed queries/clojure/calls.scm
var clojureCallsQuery string

//go:embed queries/swift/tags.scm
var swiftTagsQuery string

//go:embed queries/swift/calls.scm
var swiftCallsQuery string

//go:embed queries/r/tags.scm
var rTagsQuery string

//go:embed queries/r/calls.scm
var rCallsQuery string

//go:embed queries/julia/tags.scm
var juliaTagsQuery string

//go:embed queries/julia/calls.scm
var juliaCallsQuery string

//go:embed queries/zig/tags.scm
var zigTagsQuery string

//go:embed queries/zig/calls.scm
var zigCallsQuery string

//go:embed queries/ocaml/tags.scm
var ocamlTagsQuery string

//go:embed queries/ocaml/calls.scm
var ocamlCallsQuery string

//go:embed queries/hcl/tags.scm
var hclTagsQuery string

//go:embed queries/hcl/calls.scm
var hclCallsQuery string

type langSpec struct {
	grammar    func() unsafe.Pointer
	tagsQuery  string
	callsQuery string
}

var registry = map[string]langSpec{
	"python": {
		grammar:    tree_sitter_python.Language,
		tagsQuery:  pythonTagsQuery,
		callsQuery: pythonCallsQuery,
	},
	"java": {
		grammar:    tree_sitter_java.Language,
		tagsQuery:  javaTagsQuery,
		callsQuery: javaCallsQuery,
	},
	"go": {
		grammar:    tree_sitter_go.Language,
		tagsQuery:  goTagsQuery,
		callsQuery: goCallsQuery,
	},
	"csharp": {
		grammar:    tree_sitter_c_sharp.Language,
		tagsQuery:  csharpTagsQuery,
		callsQuery: csharpCallsQuery,
	},
	"rust": {
		grammar:    tree_sitter_rust.Language,
		tagsQuery:  rustTagsQuery,
		callsQuery: rustCallsQuery,
	},
	"ruby": {
		grammar:    tree_sitter_ruby.Language,
		tagsQuery:  rubyTagsQuery,
		callsQuery: rubyCallsQuery,
	},
	"php": {
		grammar:    tree_sitter_php.LanguagePHP,
		tagsQuery:  phpTagsQuery,
		callsQuery: phpCallsQuery,
	},
	"c": {
		grammar:    tree_sitter_c.Language,
		tagsQuery:  cTagsQuery,
		callsQuery: cCallsQuery,
	},
	"cpp": {
		grammar:    tree_sitter_cpp.Language,
		tagsQuery:  cppTagsQuery,
		callsQuery: cppCallsQuery,
	},
	"kotlin": {
		grammar:    tree_sitter_kotlin.Language,
		tagsQuery:  kotlinTagsQuery,
		callsQuery: kotlinCallsQuery,
	},
	"bash": {
		grammar:    tree_sitter_bash.Language,
		tagsQuery:  bashTagsQuery,
		callsQuery: bashCallsQuery,
	},
	"scala": {
		grammar:    tree_sitter_scala.Language,
		tagsQuery:  scalaTagsQuery,
		callsQuery: scalaCallsQuery,
	},
	"lua": {
		grammar:    tree_sitter_lua.Language,
		tagsQuery:  luaTagsQuery,
		callsQuery: luaCallsQuery,
	},
	"elixir": {
		grammar:    elixirgrammar.Language,
		tagsQuery:  elixirTagsQuery,
		callsQuery: elixirCallsQuery,
	},
	"dart": {
		grammar:    tree_sitter_dart.Language,
		tagsQuery:  dartTagsQuery,
		callsQuery: dartCallsQuery,
	},
	"haskell": {
		grammar:    tree_sitter_haskell.Language,
		tagsQuery:  haskellTagsQuery,
		callsQuery: haskellCallsQuery,
	},
	"clojure": {
		grammar:    clojuregrammar.Language,
		tagsQuery:  clojureTagsQuery,
		callsQuery: clojureCallsQuery,
	},
	"swift": {
		grammar:    swiftgrammar.Language,
		tagsQuery:  swiftTagsQuery,
		callsQuery: swiftCallsQuery,
	},
	"r": {
		grammar:    tree_sitter_r.Language,
		tagsQuery:  rTagsQuery,
		callsQuery: rCallsQuery,
	},
	"julia": {
		grammar:    tree_sitter_julia.Language,
		tagsQuery:  juliaTagsQuery,
		callsQuery: juliaCallsQuery,
	},
	"zig": {
		grammar:    tree_sitter_zig.Language,
		tagsQuery:  zigTagsQuery,
		callsQuery: zigCallsQuery,
	},
	"ocaml": {
		grammar:    tree_sitter_ocaml.LanguageOCaml,
		tagsQuery:  ocamlTagsQuery,
		callsQuery: ocamlCallsQuery,
	},
	"hcl": {
		grammar:    tree_sitter_hcl.Language,
		tagsQuery:  hclTagsQuery,
		callsQuery: hclCallsQuery,
	},
}

// Supported reports whether language has a registered tree-sitter grammar.
func Supported(language string) bool {
	_, ok := registry[language]
	return ok
}

// RegisteredLanguages returns the language keys with a registered tree-sitter
// grammar, e.g. "python", "rust". Used by generic, registry-driven tests and
// emitters that must stay correct as new languages are added without being
// edited themselves.
func RegisteredLanguages() []string {
	out := make([]string, 0, len(registry))
	for lang := range registry {
		out = append(out, lang)
	}
	return out
}

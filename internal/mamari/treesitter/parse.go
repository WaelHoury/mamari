package treesitter

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	sitter "github.com/tree-sitter/go-tree-sitter"
)

// compiledLang caches the per-language *sitter.Language and its two
// compiled queries (tags, calls). registry.go's spec.grammar()/tagsQuery/
// callsQuery are static per language — every Parse call previously
// recompiled both the Language and both Queries from scratch, even though
// nothing about them differs between files of the same language. Found
// via CPU profiling a real 10,000-line file: ts_query_new alone accounted
// for ~47% of total parse time, because it (and the Language/other Query)
// were being rebuilt on every single file. tree-sitter's Language and
// Query objects are immutable once built and documented-safe to share
// read-only across concurrently-running parses (each call still gets its
// own fresh Parser/QueryCursor, the genuinely mutable per-parse state);
// only construction is cached here, guarded by a mutex since BuildIndex
// parses many files concurrently via runParallel.
type compiledLang struct {
	lang       *sitter.Language
	tagsQuery  *sitter.Query
	callsQuery *sitter.Query
	err        error
}

var (
	compiledMu  sync.Mutex
	compiledFor = map[string]*compiledLang{}
)

func compiledForLanguage(language string, spec langSpec) *compiledLang {
	compiledMu.Lock()
	defer compiledMu.Unlock()
	if c, ok := compiledFor[language]; ok {
		return c
	}
	c := &compiledLang{lang: sitter.NewLanguage(spec.grammar())}
	if tagsQuery, qerr := sitter.NewQuery(c.lang, spec.tagsQuery); qerr != nil {
		c.err = fmt.Errorf("treesitter: tags query: %s", qerr.Message)
	} else {
		c.tagsQuery = tagsQuery
	}
	if c.err == nil {
		if callsQuery, qerr := sitter.NewQuery(c.lang, spec.callsQuery); qerr != nil {
			c.err = fmt.Errorf("treesitter: calls query: %s", qerr.Message)
		} else {
			c.callsQuery = callsQuery
		}
	}
	compiledFor[language] = c
	return c
}

// readChunkSize bounds each read callback's slice to a fixed size. The
// tree-sitter C lexer calls the read callback repeatedly as it advances
// through the file (not once for the whole file); go-tree-sitter's own
// Parser.Parse uses a callback that returns the *entire remaining
// suffix* on every call, which then gets copied into a fresh C string
// (C.CString) on the C side of each and every read — quadratic in file
// size for a single parse (read #1 copies the whole file, read #2 copies
// almost the whole file again, and so on). Confirmed via CPU profiling a
// real 10,000-line file: a tools-and-tests benchmark repo with similar
// file *counts* but far smaller individual files indexed proportionally
// far faster, and the profile's hottest frame was exactly this
// C.CString/readUTF8 path. Returning a small bounded chunk per call
// instead (the read callback contract explicitly allows slices "of any
// length") makes the total copy cost O(file size) again.
const readChunkSize = 4096

// readChunk returns a tree-sitter read callback that serves content in
// fixed-size chunks — see readChunkSize's doc comment for why this beats
// go-tree-sitter's own default (whole-remaining-suffix) callback.
func readChunk(content []byte) func(int, sitter.Point) []byte {
	return func(offset int, _ sitter.Point) []byte {
		if offset >= len(content) {
			return nil
		}
		end := offset + readChunkSize
		if end > len(content) {
			end = len(content)
		}
		return content[offset:end]
	}
}

// Parse runs the registered tree-sitter grammar for language over content
// and returns its definitions, calls, and imports as plain Go structs.
func Parse(language string, content []byte) (ParseResult, error) {
	spec, ok := registry[language]
	if !ok {
		return ParseResult{}, fmt.Errorf("treesitter: unsupported language %q", language)
	}

	c := compiledForLanguage(language, spec)
	if c.err != nil {
		return ParseResult{}, c.err
	}

	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(c.lang); err != nil {
		return ParseResult{}, fmt.Errorf("treesitter: set language %q: %w", language, err)
	}

	// C/C++ macro-based annotations (Clang thread-safety attributes,
	// library export/visibility macros) are real, valid, common modern
	// C++ that tree-sitter-cpp's grammar can't parse without the macro
	// expansion a real compiler would perform — found via real-world
	// verification (24.6% parse-failure rate on a real C++ repo). Blanked
	// out in a same-length copy used only for parsing; signature/docstring
	// extraction downstream reads the real, unmodified source
	// independently, so this is invisible outside the parser itself.
	parseContent := content
	if language == "cpp" || language == "c" {
		parseContent = stripCppMacroAnnotations(content)
	} else if language == "swift" {
		parseContent = stripSwiftConditionalDirectives(content)
	} else if language == "csharp" {
		parseContent = stripCSharpNonSemanticDirectives(content)
	}

	tree := parser.ParseWithOptions(readChunk(parseContent), nil, nil)
	defer tree.Close()
	root := tree.RootNode()

	res := ParseResult{ParseOK: !root.HasError()}
	if !res.ParseOK {
		// HasError means the grammar recovered through one or more ERROR
		// nodes. That can be invalid source, but it can also be valid syntax
		// the pinned grammar does not support. Keep the diagnostic neutral so
		// doctor does not turn a parser limitation into a false source claim.
		res.ParseError = fmt.Sprintf("%s: parser found invalid or unsupported syntax", language)
	}

	defs, err := extractDefs(language, c.tagsQuery, root, parseContent)
	if err != nil {
		return ParseResult{}, err
	}
	res.Defs = defs

	calls, imports, vars, callAssigns, err := extractCallsAndImports(language, c.callsQuery, root, parseContent)
	if err != nil {
		return ParseResult{}, err
	}
	res.Calls = calls
	res.Imports = imports
	res.Vars = vars
	res.CallAssigns = callAssigns
	res.Package = packageName(language, root, parseContent)
	if language == "hcl" {
		res.HCLBlocks, res.HCLAttrs, res.HCLRefs = extractHCL(root, parseContent)
	}

	return res, nil
}

// packageName returns the dotted package/namespace name declared at the top
// of the file: Java's `package a.b.c;` or C#'s `namespace a.b.c { ... }` /
// `namespace a.b.c;` (file-scoped). Returns "" for languages with no such
// declaration or files with no declaration (Java default package).
func packageName(language string, root *sitter.Node, src []byte) string {
	if language == "csharp" {
		return csharpNamespaceName(root, src)
	}
	return javaPackageName(root, src)
}

// javaPackageName returns the dotted package name from a Java
// `package a.b.c;` declaration at the top of the file, or "" if the file has
// no package declaration (default package) or isn't Java.
func javaPackageName(root *sitter.Node, src []byte) string {
	cursor := root.Walk()
	defer cursor.Close()
	for _, child := range root.Children(cursor) {
		if child.Kind() != "package_declaration" {
			continue
		}
		nc := child.Walk()
		defer nc.Close()
		for _, name := range child.NamedChildren(nc) {
			switch name.Kind() {
			case "scoped_identifier", "identifier":
				return name.Utf8Text(src)
			}
		}
	}
	return ""
}

// csharpNamespaceName returns the dotted namespace name from a top-level C#
// `namespace a.b.c { ... }` or file-scoped `namespace a.b.c;` declaration, or
// "" if the file has no namespace declaration.
func csharpNamespaceName(root *sitter.Node, src []byte) string {
	cursor := root.Walk()
	defer cursor.Close()
	for _, child := range root.Children(cursor) {
		switch child.Kind() {
		case "namespace_declaration", "file_scoped_namespace_declaration":
			if name := child.ChildByFieldName("name"); name != nil {
				return name.Utf8Text(src)
			}
		}
	}
	return ""
}

type rawDef struct {
	kind               string
	name               string
	nameStart, nameEnd int
	start, end         int
	bases              []string
	interfaces         []string
	annotations        []Annotation
	receiverName       string
	receiverType       string
	returnTypes        []string
	exported           bool
	exportedSet        bool
	isPartial          bool
}

// dedupeRawDefsBySpan collapses raw defs that share an identical [start,end)
// byte span into one entry, keeping the one with a populated receiverType.
// This happens when a tags query has both a generic top-level pattern (e.g.
// Rust's bare `function_item` -> "function") and a more specific nested
// pattern for the same node (e.g. a method inside an `impl` block ->
// "method" with receiverType set) — tree-sitter queries match every
// satisfied pattern independently, so the same node can be captured twice.
func dedupeRawDefsBySpan(raws []rawDef) []rawDef {
	if len(raws) < 2 {
		return raws
	}
	bySpan := make(map[[2]int]int, len(raws))
	out := make([]rawDef, 0, len(raws))
	for _, r := range raws {
		key := [2]int{r.start, r.end}
		if idx, ok := bySpan[key]; ok {
			if out[idx].receiverType == "" && r.receiverType != "" {
				out[idx] = r
			}
			continue
		}
		bySpan[key] = len(out)
		out = append(out, r)
	}
	return out
}

// extractDefs runs the tags query and applies the stack-based containment
// algorithm to derive parent/qualified-name information for nested
// class/function/method definitions.
func extractDefs(language string, query *sitter.Query, root *sitter.Node, src []byte) ([]Def, error) {
	cursor := sitter.NewQueryCursor()
	defer cursor.Close()

	names := query.CaptureNames()

	type traitImplPair struct {
		implType, traitName string
	}
	var traitImpls []traitImplPair

	var raws []rawDef
	matches := cursor.Matches(query, root, src)
	for {
		m := matches.Next()
		if m == nil {
			break
		}

		var defNode, nameNode, receiverTypeNode, elixirKeywordNode, clojureKeywordNode *sitter.Node
		var traitNameNode, implTypeNode *sitter.Node
		var kind string
		for _, c := range m.Captures {
			node := c.Node
			switch names[c.Index] {
			case "definition.class":
				defNode = &node
				kind = "class"
			case "definition.function":
				defNode = &node
				kind = "function"
			case "definition.method":
				defNode = &node
				kind = "method"
			case "definition.interface":
				defNode = &node
				kind = "interface"
			case "definition.callback":
				defNode = &node
				kind = "callback"
			case "name":
				nameNode = &node
			case "receiver.type":
				// Declarative analogue of Go's goMethodReceiver: a query can
				// capture the enclosing receiver/owner type directly (e.g.
				// Rust's `impl Type { fn method() }`) instead of requiring a
				// per-language Go helper that walks the syntax tree.
				receiverTypeNode = &node
			case "elixir.keyword":
				elixirKeywordNode = &node
			case "clj.keyword":
				clojureKeywordNode = &node
			case "trait.name":
				traitNameNode = &node
			case "impl.type":
				implTypeNode = &node
			}
		}
		if traitNameNode != nil && implTypeNode != nil {
			// `impl Trait for Type { ... }` — Type gains Trait's default
			// methods the same way a subclass gains a base class's methods.
			// Not a definition itself (no defNode/nameNode), so it doesn't
			// go through the rawDef/stack machinery below; collected
			// separately and merged into the matching "class" Def's Bases
			// once the main pass below has built every Def.
			traitImpls = append(traitImpls, traitImplPair{
				implType:  simpleTypeName(implTypeNode, src),
				traitName: simpleTypeName(traitNameNode, src),
			})
			continue
		}
		if defNode == nil || nameNode == nil {
			continue
		}
		if language == "elixir" {
			// Elixir's tree-sitter grammar gives def/defp/defmodule/etc no
			// dedicated node kind at all — they are ordinary macro calls
			// (`defmodule Foo do...end` and `IO.inspect(Foo)` are both just
			// "call" nodes), indistinguishable by shape alone from any other
			// call that happens to pass a single alias or nested-call
			// argument (e.g. `apply(foo(), [])` would otherwise spuriously
			// match the same pattern as `def foo(), do: ...`). The tags
			// query captures broadly; only a real def-keyword's literal text
			// confirms this match is an actual definition, not a query
			// predicate's job because go-tree-sitter's QueryCursor.Matches
			// does not auto-filter on #eq?/#match? (callers must invoke
			// SatisfiesTextPredicate manually), and adding that exclusively
			// for one language's two patterns is not worth a new mechanism.
			if elixirKeywordNode == nil {
				continue
			}
			if !elixirDefKeywords[elixirKeywordNode.Utf8Text(src)] {
				continue
			}
		}
		if language == "clojure" {
			// Clojure is fully homoiconic — `(defn find-user [id] ...)` and
			// a real call like `(helper id)` are both, structurally, just a
			// "list_lit" whose first child is a symbol; there is no
			// dedicated node kind for definitions at all, only convention.
			// Same reasoning as Elixir's def-keyword check: confirm via the
			// first symbol's literal text, not a query predicate.
			if clojureKeywordNode == nil {
				continue
			}
			if !clojureDefKeywords[clojureKeywordNode.Utf8Text(src)] {
				continue
			}
		}

		start := int(defNode.StartByte())
		end := int(defNode.EndByte())
		if parent := defNode.Parent(); parent != nil && parent.Kind() == "decorated_definition" {
			start = int(parent.StartByte())
			end = int(parent.EndByte())
		}
		if language == "dart" {
			// Dart's grammar never wraps a signature and its body in one
			// node — "method_signature"/"function_signature" and the
			// following "function_body" are plain siblings under
			// class_body/program. Without this, every method/function's
			// span would cover only its signature line, breaking
			// containingSymbolFast (call-site -> enclosing-symbol lookup),
			// hot-path/complexity scoring, and any caller that needs a
			// symbol's body range.
			if body := defNode.NextNamedSibling(); body != nil && body.Kind() == "function_body" {
				end = int(body.EndByte())
			}
		}

		var bases, interfaces []string
		var isPartial bool
		if kind == "class" {
			bases = classBases(language, defNode, src)
			interfaces = classInterfaces(language, defNode, src)
			if language == "csharp" {
				isPartial = csharpIsPartialClass(defNode, src)
			}
		}

		var kotlinExtType string
		if language == "kotlin" && kind == "function" {
			kotlinExtType = kotlinExtensionReceiverType(defNode, src)
			if kotlinExtType != "" {
				kind = "method"
			}
		}

		var receiverName, receiverType string
		if kind == "method" {
			receiverName, receiverType = goMethodReceiver(defNode, src)
		}
		if receiverTypeNode != nil {
			receiverType = receiverTypeNode.Utf8Text(src)
		}
		if kotlinExtType != "" {
			receiverType = kotlinExtType
		}

		var returnTypes []string
		if kind == "function" || kind == "method" {
			returnTypes = goFuncReturnTypes(defNode, src)
		}

		var annotations []Annotation
		switch language {
		case "csharp":
			annotations = csharpAttributes(defNode, src)
		default:
			annotations = javaAnnotations(defNode, src)
		}

		var exported bool
		var exportedSet bool
		if language == "rust" {
			exported = rustIsPublic(defNode)
			exportedSet = true
		}
		if language == "php" {
			exported = phpIsPublic(defNode, src)
			exportedSet = true
		}
		if language == "kotlin" {
			exported = kotlinIsExported(defNode, src)
			exportedSet = true
		}
		if language == "elixir" && elixirKeywordNode != nil {
			exported = elixirKeywordNode.Utf8Text(src) != "defp" && elixirKeywordNode.Utf8Text(src) != "defmacrop"
			exportedSet = true
		}

		raws = append(raws, rawDef{
			kind:         kind,
			name:         nameNode.Utf8Text(src),
			nameStart:    int(nameNode.StartByte()),
			nameEnd:      int(nameNode.EndByte()),
			start:        start,
			end:          end,
			bases:        bases,
			interfaces:   interfaces,
			annotations:  annotations,
			receiverName: receiverName,
			receiverType: receiverType,
			returnTypes:  returnTypes,
			exported:     exported,
			exportedSet:  exportedSet,
			isPartial:    isPartial,
		})
	}

	raws = dedupeRawDefsBySpan(raws)

	sort.SliceStable(raws, func(i, j int) bool { return raws[i].start < raws[j].start })

	type stackEntry struct {
		name, kind string
		start, end int
	}

	var stack []stackEntry
	defs := make([]Def, 0, len(raws))
	for _, r := range raws {
		for len(stack) > 0 && stack[len(stack)-1].end <= r.start {
			stack = stack[:len(stack)-1]
		}

		kind := r.kind
		var parentName, parentKind string
		var parentStart int
		qualifiedParts := make([]string, 0, len(stack)+1)
		if len(stack) > 0 {
			top := stack[len(stack)-1]
			parentName = top.name
			parentKind = top.kind
			parentStart = top.start
			if kind == "function" && (parentKind == "class" || parentKind == "interface") {
				kind = "method"
			}
		}
		for _, e := range stack {
			qualifiedParts = append(qualifiedParts, e.name)
		}
		qualifiedParts = append(qualifiedParts, r.name)

		// Go methods are not lexically nested inside their receiver type's
		// declaration, so the stack-based containment above never assigns
		// them a parent. Derive "ReceiverType.Method" qualification and
		// parent linkage directly from the receiver clause instead; the
		// emitter resolves ParentStart == -1 by looking up the receiver type
		// by name rather than by stack offset.
		if kind == "method" && r.receiverType != "" {
			parentName = r.receiverType
			parentKind = "class"
			parentStart = -1
			qualifiedParts = []string{r.receiverType, r.name}
		}

		exported := !strings.HasPrefix(r.name, "_")
		if r.exportedSet {
			exported = r.exported
		}
		defs = append(defs, Def{
			Kind:          kind,
			Name:          r.name,
			QualifiedName: strings.Join(qualifiedParts, "."),
			NameStart:     r.nameStart,
			NameEnd:       r.nameEnd,
			Start:         r.start,
			End:           r.end,
			ParentName:    parentName,
			ParentKind:    parentKind,
			ParentStart:   parentStart,
			Exported:      exported,
			Bases:         r.bases,
			Interfaces:    r.interfaces,
			Annotations:   r.annotations,
			ReceiverName:  r.receiverName,
			ReceiverType:  r.receiverType,
			ReturnTypes:   r.returnTypes,
			IsPartial:     r.isPartial,
		})

		stack = append(stack, stackEntry{name: r.name, kind: kind, start: r.start, end: r.end})
	}

	if len(traitImpls) > 0 {
		for i := range defs {
			if defs[i].Kind != "class" {
				continue
			}
			for _, ti := range traitImpls {
				if ti.implType == defs[i].Name {
					defs[i].Bases = append(defs[i].Bases, ti.traitName)
				}
			}
		}
	}

	return defs, nil
}

// rustIsPublic reports whether a Rust item def has a leading
// `pub`/`pub(crate)`/`pub(super)` visibility modifier. Rust's default
// visibility is private (unlike Python's leading-underscore convention, the
// generic Exported fallback this overrides), so a def with no
// visibility_modifier child is not exported.
// elixirDefKeywords names the def-like macro calls extractDefs treats as
// real definitions ("defmodule" -> class, the rest -> function/method via
// the usual lexical-nesting promotion). "defguard"/"defguardp" and
// "defdelegate" are deliberately excluded: they declare macros/delegated
// stubs, not bodies worth indexing as first-class symbols.
var elixirDefKeywords = map[string]bool{
	"defmodule": true, "def": true, "defp": true,
	"defmacro": true, "defmacrop": true, "defprotocol": true, "defimpl": true,
}

// clojureDefKeywords names the first-symbol forms extractDefs treats as
// real definitions when found as a list_lit's first child (kind always
// stays "function" — Clojure has no OOP concept for these to nest under).
var clojureDefKeywords = map[string]bool{
	"defn": true, "defn-": true, "def": true, "defmacro": true,
	"defmethod": true, "defmulti": true,
}

// clojureSpecialForms names symbols that, as the first element of a
// list_lit, mark a special form or near-universal core macro rather than a
// real function/macro call — `(let [x 1] ...)` must never be treated as "a
// call to let". Clojure code essentially never redefines these names, so
// excluding them by literal text is sound, not just a heuristic; the
// alternative (modeling every special form's distinct binding/control-flow
// semantics) is far more machinery than a call graph needs.
var clojureSpecialForms = map[string]bool{
	"def": true, "defn": true, "defn-": true, "defmacro": true, "defmethod": true,
	"defmulti": true, "defprotocol": true, "defrecord": true, "deftype": true,
	"defstruct": true, "ns": true, "let": true, "let*": true, "letfn": true,
	"if": true, "if-let": true, "if-not": true, "when": true, "when-let": true,
	"when-not": true, "when-first": true, "do": true, "fn": true, "fn*": true,
	"loop": true, "loop*": true, "recur": true, "cond": true, "condp": true,
	"case": true, "throw": true, "try": true, "catch": true, "finally": true,
	"quote": true, "var": true, "set!": true, "new": true, "monitor-enter": true,
	"monitor-exit": true, "import": true, "require": true, "use": true,
	"in-ns": true, "declare": true, "comment": true, "->": true, "->>": true,
	"and": true, "or": true, "doseq": true, "dotimes": true, "for": true,
	"while": true,
}

// elixirIsDefNameCall reports whether call is the synthetic "name(args)"
// wrapper tree-sitter-elixir produces for a def-like macro's first argument
// (e.g. the inner `find_user(id)` call inside `def find_user(id) do...end`)
// rather than a genuine invocation — callFromNode and the relations walker
// must both skip it, or every definition would also emit a spurious
// self-call edge to its own name.
// dartVarDecls extracts declared simple-type names from a Dart class-body
// "declaration" node — `Repo repo;` (explicit type) or `var repo = Repo();`
// (type inferred from a same-statement bare-call initializer). This is the
// same "declaration" node kind used for Dart's body-less constructor
// shorthand (`UserService(this.repo);`, see classBases/the tags.scm
// definition.method pattern) — discriminated here by its first child's kind
// (a constructor_signature there, never a type node), so the two shapes
// never collide. A "declaration" can list several comma-separated names
// (`Repo a, b;`); every one of them is its own "initialized_identifier".
// luaVarDecls extracts a single local or self-attribute assignment whose
// right side constructs a table via `ClassName.new(...)`, e.g.
// `local repo = Repo.new()` or `self.repo = Repo.new()`.
func luaVarDecls(node *sitter.Node, src []byte) []VarDecl {
	if node.Kind() != "assignment_statement" {
		return nil
	}
	left, call := luaSingleAssignment(node)
	if left == nil || call == nil {
		return nil
	}
	name := ""
	switch left.Kind() {
	case "identifier":
		name = left.Utf8Text(src)
	case "dot_index_expression":
		table := left.ChildByFieldName("table")
		field := left.ChildByFieldName("field")
		if table == nil || field == nil || table.Kind() != "identifier" {
			return nil
		}
		name = table.Utf8Text(src) + "." + field.Utf8Text(src)
	default:
		return nil
	}
	fn := call.ChildByFieldName("name")
	if fn == nil || fn.Kind() != "dot_index_expression" {
		return nil
	}
	cls := fn.ChildByFieldName("table")
	method := fn.ChildByFieldName("field")
	if cls == nil || method == nil || method.Utf8Text(src) != "new" {
		return nil
	}
	return []VarDecl{{Name: name, Type: cls.Utf8Text(src), Pos: int(node.StartByte())}}
}

func luaSingleAssignment(node *sitter.Node) (left, right *sitter.Node) {
	if directLeft := node.ChildByFieldName("name"); directLeft != nil {
		if directRight := node.ChildByFieldName("value"); directRight != nil && directRight.Kind() == "function_call" {
			return directLeft, directRight
		}
	}
	if node.NamedChildCount() != 2 {
		return nil, nil
	}
	leftList := node.NamedChild(0)
	rightList := node.NamedChild(1)
	if leftList == nil || rightList == nil || leftList.Kind() != "variable_list" || rightList.Kind() != "expression_list" {
		return nil, nil
	}
	leftCursor := leftList.Walk()
	defer leftCursor.Close()
	lefts := leftList.ChildrenByFieldName("name", leftCursor)
	rightCursor := rightList.Walk()
	defer rightCursor.Close()
	rights := rightList.ChildrenByFieldName("value", rightCursor)
	if len(lefts) != 1 || len(rights) != 1 || rights[0].Kind() != "function_call" {
		return nil, nil
	}
	return &lefts[0], &rights[0]
}

func dartVarDecls(node *sitter.Node, src []byte) []VarDecl {
	if node.Kind() != "declaration" {
		return nil
	}
	typeNode := node.NamedChild(0)
	if typeNode == nil {
		return nil
	}
	explicitType := ""
	switch typeNode.Kind() {
	case "type_identifier":
		explicitType = simpleTypeName(typeNode, src)
	case "inferred_type":
		// `var`/`final` with no type annotation — only resolvable via a
		// same-statement bare-call initializer below.
	default:
		return nil
	}
	list := node.NamedChild(1)
	if list == nil || list.Kind() != "initialized_identifier_list" {
		return nil
	}
	cursor := list.Walk()
	defer cursor.Close()
	var out []VarDecl
	for _, item := range list.NamedChildren(cursor) {
		if item.Kind() != "initialized_identifier" {
			continue
		}
		nameNode := item.NamedChild(0)
		if nameNode == nil || nameNode.Kind() != "identifier" {
			continue
		}
		typ := explicitType
		if typ == "" {
			// `var repo = Repo();` — the initializer's callee identifier,
			// the same bare-call shape dartCallReceiverCallee recognizes:
			// [identifier name, identifier callee, selector(args)].
			if callee := item.NamedChild(1); callee != nil && callee.Kind() == "identifier" {
				typ = callee.Utf8Text(src)
			}
		}
		if typ == "" {
			continue
		}
		out = append(out, VarDecl{Name: nameNode.Utf8Text(src), Type: typ, Pos: int(node.StartByte())})
	}
	return out
}

// dartCallReceiverCallee recovers (receiver, callee) for a Dart
// argument-bearing "selector" node from its position among its parent's
// named children, since Dart's grammar represents `a.b.c()` as a flat
// sibling run — [identifier "a", selector ".b", selector ".c", selector
// "(...)"] — rather than a single call node. The selector immediately
// before the call selector is either a dot-access selector (the chain's
// receiver is everything before it, as raw text — correctly handling
// multi-segment chains) or, for a bare call like `helper(id)`, the primary
// identifier directly.
func dartCallReceiverCallee(call *sitter.Node, src []byte) (receiver, callee string) {
	parent := call.Parent()
	if parent == nil {
		return "", ""
	}
	cursor := parent.Walk()
	defer cursor.Close()
	children := parent.NamedChildren(cursor)
	idx := -1
	for i := range children {
		if children[i].StartByte() == call.StartByte() && children[i].EndByte() == call.EndByte() {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return "", ""
	}
	prev := children[idx-1]
	if prev.Kind() == "selector" {
		dotSel := prev.NamedChild(0)
		if dotSel == nil {
			return "", ""
		}
		// "unconditional_assignable_selector" (`.name`) and
		// "conditional_assignable_selector" (`?.name`, Dart's null-safe
		// access — pervasive in modern null-safe Dart) share the same
		// dot-then-identifier shape; both are accepted here.
		if dotSel.Kind() != "unconditional_assignable_selector" && dotSel.Kind() != "conditional_assignable_selector" {
			return "", ""
		}
		id := dotSel.NamedChild(0)
		if id == nil {
			return "", ""
		}
		start := children[0].StartByte()
		end := prev.StartByte()
		return string(src[start:end]), id.Utf8Text(src)
	}
	if prev.Kind() == "identifier" {
		return "", prev.Utf8Text(src)
	}
	return "", ""
}

func elixirIsDefNameCall(call *sitter.Node, src []byte) bool {
	args := call.Parent()
	if args != nil && args.Kind() == "binary_operator" {
		// `def foo(x) when guard do...end` wraps the name+params call as
		// the left side of a "binary_operator" (the `when`), one level
		// deeper than the unguarded form — found via a real gap: Plug's
		// (extremely common) guarded clauses were silently missing
		// entirely, not just mis-tagged, until this case was added.
		args = args.Parent()
	}
	if args == nil || args.Kind() != "arguments" {
		return false
	}
	outer := args.Parent()
	if outer == nil || outer.Kind() != "call" {
		return false
	}
	target := outer.ChildByFieldName("target")
	if target == nil || target.Kind() != "identifier" {
		return false
	}
	return elixirDefKeywords[target.Utf8Text(src)]
}

// csharpIsPartialClass reports whether a C# class_declaration carries the
// `partial` modifier (`public partial class Service { ... }`) — each such
// declaration in a different file is one fragment of a single logical
// class, not a separate class. classDef's direct children include each
// modifier keyword as its own "modifier" node (no field name distinguishing
// "partial" from "public"/"static"/etc.), so this checks their text
// directly rather than a dedicated field.
func csharpIsPartialClass(classDef *sitter.Node, src []byte) bool {
	cursor := classDef.Walk()
	defer cursor.Close()
	for _, c := range classDef.NamedChildren(cursor) {
		if c.Kind() == "modifier" && c.Utf8Text(src) == "partial" {
			return true
		}
	}
	return false
}

func rustIsPublic(defNode *sitter.Node) bool {
	cursor := defNode.Walk()
	defer cursor.Close()
	for _, c := range defNode.Children(cursor) {
		if c.Kind() == "visibility_modifier" {
			return true
		}
	}
	return false
}

// phpIsPublic reports whether a PHP class-member def is visible outside its
// class. Unlike Rust, PHP's default (no explicit visibility_modifier) is
// public — only an explicit "private"/"protected" modifier narrows it.
// Top-level classes/interfaces/functions never carry a visibility_modifier
// and are correctly treated as public by this same default.
func phpIsPublic(defNode *sitter.Node, src []byte) bool {
	cursor := defNode.Walk()
	defer cursor.Close()
	for _, c := range defNode.Children(cursor) {
		if c.Kind() != "visibility_modifier" {
			continue
		}
		switch c.Utf8Text(src) {
		case "private", "protected":
			return false
		}
	}
	return true
}

// goMethodReceiver extracts the receiver variable name and simple receiver
// type name from a Go `method_declaration`, e.g. `func (s *Service) Foo()`
// -> ("s", "Service"). Returns ("", "") for an unnamed receiver
// (`func (Service) Foo()`) or a defNode with no `receiver` field.
func goMethodReceiver(defNode *sitter.Node, src []byte) (name, typ string) {
	recv := defNode.ChildByFieldName("receiver")
	if recv == nil {
		return "", ""
	}
	cursor := recv.Walk()
	defer cursor.Close()
	for _, pd := range recv.NamedChildren(cursor) {
		if pd.Kind() != "parameter_declaration" {
			continue
		}
		if t := pd.ChildByFieldName("type"); t != nil {
			typ = simpleTypeName(t, src)
		}
		if n := pd.ChildByFieldName("name"); n != nil {
			name = n.Utf8Text(src)
		}
		return name, typ
	}
	return "", ""
}

// kotlinExtensionReceiverType returns the simple receiver type name of a
// Kotlin extension function (`fun Repo.extendedFind(id: Int): String {...}`
// -> "Repo"), or "" for an ordinary function with no receiver. The
// grammar's function_declaration places the receiver type as a plain
// child (user_type/nullable_type/parenthesized_type — no field name,
// since `_receiver_type` is an inlined/hidden rule) immediately followed
// by a literal "." token, both occurring before the "name" field — found
// by dumping the parse tree directly rather than assumed, since the
// node-types.json listing for function_declaration gives no field to key
// off. Extension functions are a real, idiomatic, extremely common Kotlin
// pattern (Android/Compose codebases lean on them heavily) declared
// anywhere, often a file away from the type they extend — without this,
// they are indistinguishable from a bare top-level function and never
// link to their receiver type at all.
func kotlinExtensionReceiverType(defNode *sitter.Node, src []byte) string {
	nameNode := defNode.ChildByFieldName("name")
	cursor := defNode.Walk()
	defer cursor.Close()
	var prevType *sitter.Node
	for _, c := range defNode.Children(cursor) {
		if nameNode != nil && c.StartByte() == nameNode.StartByte() && c.EndByte() == nameNode.EndByte() {
			break
		}
		switch c.Kind() {
		case "user_type", "nullable_type", "parenthesized_type":
			t := c
			prevType = &t
		case ".":
			if prevType != nil {
				return simpleTypeName(prevType, src)
			}
		default:
			prevType = nil
		}
	}
	return ""
}

// goFuncReturnTypes returns the simple (pointer-stripped) return type names
// of a Go function_declaration/method_declaration's `result` field, in
// order. Returns nil if the def has no return value (result == nil).
func goFuncReturnTypes(defNode *sitter.Node, src []byte) []string {
	result := defNode.ChildByFieldName("result")
	if result == nil {
		return nil
	}
	if result.Kind() == "parameter_list" {
		cursor := result.Walk()
		defer cursor.Close()
		var out []string
		for _, c := range result.NamedChildren(cursor) {
			if t := c.ChildByFieldName("type"); t != nil {
				out = append(out, simpleTypeName(t, src))
			} else {
				out = append(out, "")
			}
		}
		return out
	}
	return []string{simpleTypeName(result, src)}
}

// callAssignsFromNode extracts CallAssigns from a Go `short_var_declaration`
// whose right-hand side is a single call expression, e.g. `x := NewThing()`
// or `x, err := s.GetRepo()`. Returns nil for any other shape (composite
// literals, multi-call RHS, etc.).
func callAssignsFromNode(node *sitter.Node, src []byte) []CallAssign {
	left := node.ChildByFieldName("left")
	right := node.ChildByFieldName("right")
	if left == nil || right == nil {
		return nil
	}
	if right.NamedChildCount() != 1 {
		return nil
	}
	rcursor := right.Walk()
	defer rcursor.Close()
	rightChildren := right.NamedChildren(rcursor)
	if len(rightChildren) != 1 {
		return nil
	}
	callExpr := rightChildren[0]
	if callExpr.Kind() != "call_expression" {
		return nil
	}
	fn := callExpr.ChildByFieldName("function")
	if fn == nil {
		return nil
	}
	var callee, receiver string
	switch fn.Kind() {
	case "identifier":
		callee = fn.Utf8Text(src)
	case "selector_expression":
		field := fn.ChildByFieldName("field")
		operand := fn.ChildByFieldName("operand")
		if field == nil || operand == nil {
			return nil
		}
		callee = field.Utf8Text(src)
		receiver = operand.Utf8Text(src)
	default:
		return nil
	}

	lcursor := left.Walk()
	defer lcursor.Close()
	var out []CallAssign
	for i, c := range left.NamedChildren(lcursor) {
		if c.Kind() != "identifier" {
			continue
		}
		name := c.Utf8Text(src)
		if name == "_" {
			continue
		}
		out = append(out, CallAssign{
			Name:        name,
			Receiver:    receiver,
			Callee:      callee,
			ResultIndex: i,
			Pos:         int(node.StartByte()),
		})
	}
	return out
}

// classBases returns the base-class expressions for a class definition:
// Python's `class_definition.superclasses` argument_list (e.g.
// `class Account(Base, mod.Mixin):` -> ["Base", "mod.Mixin"]; keyword
// arguments like `metaclass=ABCMeta` are skipped), or Java's
// `class_declaration.superclass` field (e.g. `class Account extends Base`
// -> ["Base"]). Other def kinds (interfaces, enums, records) return nil.
func classBases(language string, classDef *sitter.Node, src []byte) []string {
	if language == "csharp" {
		bases, _ := csharpBaseList(classDef, src)
		return bases
	}
	if language == "php" {
		// PHP's "class_declaration" node kind is identical to Java's, but its
		// extends clause is an unnamed "base_clause" sibling child (no
		// "superclass" field, unlike Java) — must be disambiguated by
		// language, not the (here, ambiguous) node kind.
		bases := phpClassBaseNames(classDef, "base_clause", src)
		// `use TraitName;` inside a class/trait body composes the trait's
		// methods into this class as if inherited — folded into the same
		// Bases list (rather than a separate field) so the existing
		// classBases-walk in findMethodInClassLocked resolves trait methods
		// for free, the same way it already resolves `extends` methods.
		return append(bases, phpTraitUseNames(classDef, src)...)
	}
	if language == "cpp" {
		return cppClassBaseNames(classDef, src)
	}
	if language == "scala" {
		// Scala's "class_definition" node kind is identical to Python's
		// (and "object_definition"/"trait_definition" never collide with
		// anything, but are routed through the same helper for symmetry),
		// but its base clause is an unnamed "extends_clause" sibling child
		// (no "superclasses" field, unlike Python) — must be disambiguated
		// by language, the same lesson as PHP's class_declaration/Java
		// collision.
		return scalaClassBaseNames(classDef, src)
	}
	if language == "dart" {
		// Dart's "class_definition" node kind is identical to Python's, but
		// its `extends Base with Mixin1, Mixin2` clause is a "superclass"
		// field (no "superclasses" field, unlike Python) whose own children
		// hold both the real superclass and a nested "mixins" node — must be
		// disambiguated by language, the same lesson as PHP/Scala's
		// class-node-kind collisions above.
		return dartClassBaseNames(classDef, src)
	}
	switch classDef.Kind() {
	case "class_definition":
		args := classDef.ChildByFieldName("superclasses")
		if args == nil {
			return nil
		}
		cursor := args.Walk()
		defer cursor.Close()

		var bases []string
		for _, c := range args.Children(cursor) {
			switch c.Kind() {
			case "identifier", "attribute":
				bases = append(bases, c.Utf8Text(src))
			}
		}
		return bases
	case "class_declaration":
		sc := classDef.ChildByFieldName("superclass")
		if sc == nil {
			return nil
		}
		cursor := sc.Walk()
		defer cursor.Close()

		var bases []string
		for _, c := range sc.Children(cursor) {
			switch c.Kind() {
			case "type_identifier", "generic_type", "scoped_type_identifier":
				bases = append(bases, simpleTypeName(&c, src))
			}
		}
		return bases
	case "class":
		// Ruby `class Foo < Bar` -> the "superclass" field wraps the "< "
		// token and the actual superclass expression as its sole named child.
		sc := classDef.ChildByFieldName("superclass")
		if sc == nil || sc.NamedChildCount() == 0 {
			return nil
		}
		base := sc.NamedChild(0)
		if base == nil {
			return nil
		}
		return []string{base.Utf8Text(src)}
	case "type_spec":
		// Go struct embedding: `field_declaration` children of the
		// `struct_type` with no `name` field are anonymous (embedded)
		// fields, e.g. `type Account struct { Base; *mod.Mixin }` ->
		// ["Base", "Mixin"]. Named fields and non-struct type_specs are
		// skipped.
		structType := classDef.ChildByFieldName("type")
		if structType == nil || structType.Kind() != "struct_type" {
			return nil
		}
		fieldList := structType.ChildByFieldName("body")
		if fieldList == nil {
			// Fall back to the (only) named child if the field isn't named
			// "body" in this grammar version.
			if structType.NamedChildCount() == 0 {
				return nil
			}
			fieldList = structType.NamedChild(0)
		}
		cursor := fieldList.Walk()
		defer cursor.Close()

		var bases []string
		for _, c := range fieldList.NamedChildren(cursor) {
			if c.Kind() != "field_declaration" {
				continue
			}
			if c.ChildByFieldName("name") != nil {
				continue
			}
			// Embedded fields have a single type child (type_identifier,
			// qualified_type, or pointer_type) with no "type"/"name" field.
			if t := c.ChildByFieldName("type"); t != nil {
				bases = append(bases, simpleTypeName(t, src))
			} else if c.NamedChildCount() > 0 {
				if t := c.NamedChild(0); t != nil {
					bases = append(bases, simpleTypeName(t, src))
				}
			}
		}
		return bases
	}
	return nil
}

// classInterfaces returns the simple names of interfaces a Java class
// declares via `implements`, e.g. `class Foo implements Repository<Owner>,
// Serializable` -> ["Repository", "Serializable"]. Returns nil for non-Java
// classes or classes with no `implements` clause.
func classInterfaces(language string, classDef *sitter.Node, src []byte) []string {
	if language == "csharp" {
		_, interfaces := csharpBaseList(classDef, src)
		return interfaces
	}
	if language == "php" {
		return phpClassBaseNames(classDef, "class_interface_clause", src)
	}
	if classDef.Kind() != "class_declaration" {
		return nil
	}
	ifaces := classDef.ChildByFieldName("interfaces")
	if ifaces == nil {
		return nil
	}
	cursor := ifaces.Walk()
	defer cursor.Close()

	var out []string
	for _, c := range ifaces.Children(cursor) {
		if c.Kind() != "type_list" {
			continue
		}
		tlCursor := c.Walk()
		defer tlCursor.Close()
		for _, t := range c.Children(tlCursor) {
			switch t.Kind() {
			case "type_identifier", "generic_type", "scoped_type_identifier":
				out = append(out, simpleTypeName(&t, src))
			}
		}
	}
	return out
}

// csharpBaseList splits a C# class/interface/struct/record declaration's
// unlabeled `base_list` child (e.g. `class Foo : Base, IRepository,
// IDisposable`) into base-class and implemented-interface simple names.
// The C# grammar does not syntactically distinguish a base class from
// implemented interfaces, so this applies the standard C# naming convention:
// an entry whose simple name matches `I` followed by an uppercase letter
// (e.g. "IRepository", "IDisposable") is treated as an interface; all other
// entries are treated as base classes. Returns (nil, nil) for declarations
// with no `base_list`.
func csharpBaseList(classDef *sitter.Node, src []byte) (bases, interfaces []string) {
	cursor := classDef.Walk()
	defer cursor.Close()
	for _, c := range classDef.Children(cursor) {
		if c.Kind() != "base_list" {
			continue
		}
		blCursor := c.Walk()
		defer blCursor.Close()
		for _, t := range c.NamedChildren(blCursor) {
			name := simpleTypeName(&t, src)
			if name == "" {
				continue
			}
			if isCSharpInterfaceName(name) {
				interfaces = append(interfaces, name)
			} else {
				bases = append(bases, name)
			}
		}
	}
	return bases, interfaces
}

// phpClassBaseNames returns the simple names listed in a PHP
// class_declaration's "base_clause" (`extends Base`) or
// "class_interface_clause" (`implements A, B`) child — both childKind
// values, distinct sibling nodes with no field name on the class itself, so
// the caller selects which one it wants.
func phpClassBaseNames(classDef *sitter.Node, childKind string, src []byte) []string {
	cursor := classDef.Walk()
	defer cursor.Close()
	var out []string
	for _, c := range classDef.NamedChildren(cursor) {
		if c.Kind() != childKind {
			continue
		}
		ccursor := c.Walk()
		defer ccursor.Close()
		for _, n := range c.NamedChildren(ccursor) {
			out = append(out, simpleTypeName(&n, src))
		}
	}
	return out
}

// phpTraitUseNames returns the simple names listed in a PHP class or trait
// body's `use TraitA, TraitB;` statements (trait composition) — distinct
// from a file-level `use Some\Namespace\Class;` import, which shares the
// same "use_declaration" node kind but is a direct child of "program", not
// of the class/trait's "declaration_list" body. Only body-level use
// statements are trait-composition; this walks the body explicitly rather
// than classDef's own children (where extends/implements clauses live) to
// avoid matching the wrong "use".
func phpTraitUseNames(classDef *sitter.Node, src []byte) []string {
	body := classDef.ChildByFieldName("body")
	if body == nil {
		return nil
	}
	bcursor := body.Walk()
	defer bcursor.Close()
	var out []string
	for _, c := range body.NamedChildren(bcursor) {
		if c.Kind() != "use_declaration" {
			continue
		}
		ccursor := c.Walk()
		defer ccursor.Close()
		for _, n := range c.NamedChildren(ccursor) {
			switch n.Kind() {
			case "name", "qualified_name":
				out = append(out, simpleTypeName(&n, src))
			}
		}
	}
	return out
}

// cppClassBaseNames returns the simple names listed in a C++
// class_specifier/struct_specifier's "base_class_clause" child (e.g.
// `class UserService : public BaseService, public Greeter` ->
// ["BaseService", "Greeter"]), skipping the access_specifier
// ("public"/"private"/"protected") and virtual_specifier tokens interspersed
// between base names. C++ has no syntactic extends/implements split (unlike
// PHP/Java) — every base goes into Bases; Interfaces is left empty rather
// than guessed at via a naming convention.
func cppClassBaseNames(classDef *sitter.Node, src []byte) []string {
	cursor := classDef.Walk()
	defer cursor.Close()
	var out []string
	for _, c := range classDef.NamedChildren(cursor) {
		if c.Kind() != "base_class_clause" {
			continue
		}
		bcursor := c.Walk()
		defer bcursor.Close()
		for _, n := range c.NamedChildren(bcursor) {
			switch n.Kind() {
			case "type_identifier", "qualified_identifier", "template_type":
				// "template_type" is a generic base class, e.g.
				// `class Foo : public Bar<int>` — simpleTypeName already
				// unwraps it to the bare name.
				out = append(out, simpleTypeName(&n, src))
			}
		}
	}
	return out
}

// scalaClassBaseNames returns the simple names listed in a Scala
// class/object/trait definition's "extends_clause" child (e.g.
// `class UserService extends BaseService with Greeter with Logger` ->
// ["BaseService", "Greeter", "Logger"]). Scala has no syntactic
// extends/implements split (the `with`-chained mixins are not
// distinguished from the single real superclass) — every name goes into
// Bases, mirroring the same tradeoff already accepted for C++.
func scalaClassBaseNames(classDef *sitter.Node, src []byte) []string {
	cursor := classDef.Walk()
	defer cursor.Close()
	var out []string
	for _, c := range classDef.NamedChildren(cursor) {
		if c.Kind() != "extends_clause" {
			continue
		}
		ecursor := c.Walk()
		defer ecursor.Close()
		for _, n := range c.NamedChildren(ecursor) {
			if n.Kind() == "type_identifier" {
				out = append(out, n.Utf8Text(src))
			}
		}
	}
	return out
}

// dartClassBaseNames returns the simple names listed in a Dart
// class_definition's "superclass" field — both the real superclass
// (`extends Base`) and any mixed-in types (`with Mixin1, Mixin2`, nested
// inside the same "superclass" node as a "mixins" child, not a separate
// field) — folded into one flat list the same way PHP folds `use Trait;`
// into its Bases. `with Mixin1, Mixin2` with no explicit `extends` (a
// mixin applied directly to Object) has no "superclass" field at all in
// that shape; only the explicit-extends form is covered, the common
// real-world Dart/Flutter pattern (`class Foo extends StatefulWidget with
// Mixin {}`).
func dartClassBaseNames(classDef *sitter.Node, src []byte) []string {
	super := classDef.ChildByFieldName("superclass")
	if super == nil {
		return nil
	}
	cursor := super.Walk()
	defer cursor.Close()
	var out []string
	for _, c := range super.NamedChildren(cursor) {
		switch c.Kind() {
		case "type_identifier", "generic_type", "nullable_type":
			out = append(out, simpleTypeName(&c, src))
		case "mixins":
			mcursor := c.Walk()
			defer mcursor.Close()
			for _, m := range c.NamedChildren(mcursor) {
				out = append(out, simpleTypeName(&m, src))
			}
		}
	}
	return out
}

// zigVarDecls extracts declared simple-type names from a Zig `parameter`
// (`self: *Server`, `res: Response`) or a typed `variable_declaration`
// (`var local: Buffer = ...`, `const c: *Config = ...`), used to resolve
// `recv.method()` calls to recv's declared type. A variable_declaration
// without an explicit type annotation (e.g. `const std = @import("std")`)
// yields nothing — Zig has no return-type inference here, so an untyped
// binding stays honestly unresolved rather than guessed.
func zigVarDecls(node *sitter.Node, src []byte) []VarDecl {
	switch node.Kind() {
	case "parameter":
		nameNode := node.ChildByFieldName("name")
		typeNode := node.ChildByFieldName("type")
		if nameNode == nil || typeNode == nil {
			return nil
		}
		return []VarDecl{{Name: nameNode.Utf8Text(src), Type: zigSimpleType(typeNode, src), Pos: int(node.StartByte())}}
	case "variable_declaration":
		typeNode := node.ChildByFieldName("type")
		if typeNode == nil {
			return nil
		}
		nameNode := node.NamedChild(0)
		if nameNode == nil || nameNode.Kind() != "identifier" {
			return nil
		}
		return []VarDecl{{Name: nameNode.Utf8Text(src), Type: zigSimpleType(typeNode, src), Pos: int(node.StartByte())}}
	}
	return nil
}

// zigSimpleType unwraps pointer/optional wrappers (`*T`, `?T`, `*const T`) to
// the base type's simple name so `self: *Server` resolves receiver `self` to
// type `Server`, the same way the enclosing struct would.
func zigSimpleType(node *sitter.Node, src []byte) string {
	for node != nil {
		switch node.Kind() {
		case "pointer_type", "optional_type":
			inner := node.NamedChild(node.NamedChildCount() - 1)
			if inner == nil {
				return simpleTypeName(node, src)
			}
			node = inner
			continue
		case "field_expression":
			// A qualified type `pkg.Type` (and `*pkg.Type`, which parses as a
			// field_expression whose object is the pointer) — the bare type
			// name is the member. Reduces the type to what findClassByName can
			// match; whether the repo has a single or colliding same-named type
			// is then the resolver's (precision-preserving) call.
			if m := node.ChildByFieldName("member"); m != nil {
				return m.Utf8Text(src)
			}
		}
		break
	}
	return simpleTypeName(node, src)
}

// scalaVarDecls extracts declared simple-type names from a Scala
// `class_parameter` (primary-constructor property, e.g.
// `private val repo: UserRepo`) or `parameter` (function parameter, e.g.
// `id: Int`) for `variable.method()` call resolution. Unlike Kotlin, both
// node kinds expose clean "name"/"type" fields directly — no positional
// inference needed.
func scalaVarDecls(node *sitter.Node, src []byte) []VarDecl {
	switch node.Kind() {
	case "class_parameter", "parameter":
		nameNode := node.ChildByFieldName("name")
		typeNode := node.ChildByFieldName("type")
		if nameNode == nil || typeNode == nil {
			return nil
		}
		return []VarDecl{{Name: nameNode.Utf8Text(src), Type: simpleTypeName(typeNode, src), Pos: int(node.StartByte())}}
	case "var_definition", "val_definition":
		// A body-level `var`/`val` property, e.g. `var repo: Repo = ...`
		// or `var repo = new Repo()` — distinct from class_parameter (a
		// primary-constructor property) and the only other shape that
		// declares a typed instance field in idiomatic Scala. Unlike
		// class_parameter/parameter, the bound name is field "pattern", not
		// "name". Found missing via a same-name-collision test: a class
		// declaring its dependency as a body-level property had its
		// `repo.find(id)` call silently fail to type-resolve.
		nameNode := node.ChildByFieldName("pattern")
		if nameNode == nil || nameNode.Kind() != "identifier" {
			return nil
		}
		if typeNode := node.ChildByFieldName("type"); typeNode != nil {
			return []VarDecl{{Name: nameNode.Utf8Text(src), Type: simpleTypeName(typeNode, src), Pos: int(node.StartByte())}}
		}
		// No explicit type: infer it from a `new ClassName(...)` value
		// initializer, the same "instance_expression" shape callFromNode's
		// Scala case already extracts a callee from.
		value := node.ChildByFieldName("value")
		if value == nil || value.Kind() != "instance_expression" {
			return nil
		}
		cls := value.NamedChild(0)
		if cls == nil {
			return nil
		}
		return []VarDecl{{Name: nameNode.Utf8Text(src), Type: simpleTypeName(cls, src), Pos: int(node.StartByte())}}
	}
	return nil
}

// isCSharpInterfaceName reports whether name follows the C# interface naming
// convention: an "I" prefix followed by an uppercase letter, e.g.
// "IRepository", "IDisposable" (but not "Item" or a bare "I").
func isCSharpInterfaceName(name string) bool {
	if len(name) < 2 || name[0] != 'I' {
		return false
	}
	return name[1] >= 'A' && name[1] <= 'Z'
}

// csharpAttributes extracts the C# attributes attached to a type/method
// declaration via its preceding `attribute_list` children, e.g. `[Route("api/[controller]")]`
// -> {Name: "Route", Value: "api/[controller]"} and `[ApiController]` ->
// {Name: "ApiController"}. Returns nil for defs with no attributes.
func csharpAttributes(defNode *sitter.Node, src []byte) []Annotation {
	cursor := defNode.Walk()
	defer cursor.Close()

	var out []Annotation
	for _, c := range defNode.Children(cursor) {
		if c.Kind() != "attribute_list" {
			continue
		}
		lCursor := c.Walk()
		defer lCursor.Close()
		for _, a := range c.NamedChildren(lCursor) {
			if a.Kind() != "attribute" {
				continue
			}
			ann := Annotation{}
			if name := a.ChildByFieldName("name"); name != nil {
				ann.Name = simpleTypeName(name, src)
			}
			ann.Value = csharpAttributeValue(&a, src)
			out = append(out, ann)
		}
	}
	return out
}

// csharpAttributeValue extracts the first positional string-literal argument
// from a C# attribute's argument list, e.g. `[Route("api/[controller]")]` ->
// "api/[controller]". Returns "" if the attribute has no arguments or no
// positional string argument.
func csharpAttributeValue(a *sitter.Node, src []byte) string {
	cursor := a.Walk()
	defer cursor.Close()
	for _, c := range a.Children(cursor) {
		if c.Kind() != "attribute_argument_list" {
			continue
		}
		argCursor := c.Walk()
		defer argCursor.Close()
		for _, arg := range c.NamedChildren(argCursor) {
			if arg.Kind() != "attribute_argument" {
				continue
			}
			if arg.ChildByFieldName("name") != nil {
				// Named argument (e.g. `Order = 1`) - skip in favor of the
				// first positional argument.
				continue
			}
			argCursor2 := arg.Walk()
			defer argCursor2.Close()
			for _, v := range arg.NamedChildren(argCursor2) {
				if v.Kind() == "string_literal" {
					return csharpStringLiteralValue(&v, src)
				}
			}
		}
	}
	return ""
}

// csharpStringLiteralValue strips the surrounding quotes (and an optional
// leading `@`/`$` verbatim/interpolation marker) from a C# `string_literal`
// node's text.
func csharpStringLiteralValue(n *sitter.Node, src []byte) string {
	return strings.Trim(n.Utf8Text(src), "\"@$")
}

// javaAnnotations extracts the Java annotations attached to a class/method
// declaration via its `modifiers` child, e.g. `@RestController` ->
// {Name: "RestController"} and `@RequestMapping("/api/owners")` ->
// {Name: "RequestMapping", Value: "/api/owners"}. Returns nil for non-Java
// defs (no `modifiers` child) or defs with no annotations.
func javaAnnotations(defNode *sitter.Node, src []byte) []Annotation {
	cursor := defNode.Walk()
	defer cursor.Close()

	var out []Annotation
	for _, c := range defNode.Children(cursor) {
		if c.Kind() != "modifiers" {
			continue
		}
		mCursor := c.Walk()
		defer mCursor.Close()
		for _, a := range c.Children(mCursor) {
			switch a.Kind() {
			case "marker_annotation":
				if name := a.ChildByFieldName("name"); name != nil {
					out = append(out, Annotation{Name: name.Utf8Text(src)})
				}
			case "annotation":
				ann := Annotation{}
				if name := a.ChildByFieldName("name"); name != nil {
					ann.Name = name.Utf8Text(src)
				}
				ann.Value = annotationValue(&a, src)
				ann.Method = annotationMethod(&a, src)
				out = append(out, ann)
			}
		}
	}
	return out
}

// annotationValue extracts the primary string argument from a Java
// annotation's argument list: a bare positional string literal (e.g.
// `@RequestMapping("/api/owners")`), or the `value`/`path` element of a
// `key = value` pair (e.g. `@PostMapping(value = "/new", ...)`), including
// the first element of an array initializer (e.g. `value = {"/a", "/b"}`).
// Returns "" if no such argument is present.
func annotationValue(a *sitter.Node, src []byte) string {
	args := a.ChildByFieldName("arguments")
	if args == nil {
		return ""
	}
	cursor := args.Walk()
	defer cursor.Close()

	for _, c := range args.Children(cursor) {
		switch c.Kind() {
		case "string_literal":
			return stringLiteralValue(&c, src)
		case "element_value_pair":
			key := ""
			if k := c.ChildByFieldName("key"); k != nil {
				key = k.Utf8Text(src)
			}
			if key != "value" && key != "path" {
				continue
			}
			v := c.ChildByFieldName("value")
			if v == nil {
				continue
			}
			switch v.Kind() {
			case "string_literal":
				return stringLiteralValue(v, src)
			case "array_initializer":
				vCursor := v.Walk()
				defer vCursor.Close()
				for _, el := range v.Children(vCursor) {
					if el.Kind() == "string_literal" {
						return stringLiteralValue(&el, src)
					}
				}
			}
		}
	}
	return ""
}

// stringLiteralValue strips the surrounding quotes from a Java
// `string_literal` node's text.
func stringLiteralValue(n *sitter.Node, src []byte) string {
	return strings.Trim(n.Utf8Text(src), "\"")
}

// annotationMethod extracts the HTTP method named by a Spring
// `@RequestMapping(method = RequestMethod.POST)` element (or the first
// element of `method = {RequestMethod.GET, RequestMethod.POST}`), returning
// "POST"/"GET" etc. Returns "" if the annotation has no `method` element.
func annotationMethod(a *sitter.Node, src []byte) string {
	args := a.ChildByFieldName("arguments")
	if args == nil {
		return ""
	}
	cursor := args.Walk()
	defer cursor.Close()

	for _, c := range args.Children(cursor) {
		if c.Kind() != "element_value_pair" {
			continue
		}
		key := ""
		if k := c.ChildByFieldName("key"); k != nil {
			key = k.Utf8Text(src)
		}
		if key != "method" {
			continue
		}
		v := c.ChildByFieldName("value")
		if v == nil {
			continue
		}
		switch v.Kind() {
		case "field_access":
			return fieldAccessLastPart(v, src)
		case "array_initializer":
			vCursor := v.Walk()
			defer vCursor.Close()
			for _, el := range v.Children(vCursor) {
				if el.Kind() == "field_access" {
					return fieldAccessLastPart(&el, src)
				}
			}
		}
	}
	return ""
}

// fieldAccessLastPart returns the field name of a `field_access` node, e.g.
// `RequestMethod.POST` -> "POST".
func fieldAccessLastPart(n *sitter.Node, src []byte) string {
	if f := n.ChildByFieldName("field"); f != nil {
		return f.Utf8Text(src)
	}
	return n.Utf8Text(src)
}

// varDeclsFromNode extracts variable-type bindings from a Java
// `field_declaration`/`local_variable_declaration` (one per declarator, e.g.
// `private final VisitRepository visits;`) or `formal_parameter` (e.g.
// `@PathVariable("id") int ownerId`), used to resolve `variable.method()`
// calls to the variable's declared (simple) type.
func varDeclsFromNode(language string, node *sitter.Node, src []byte) []VarDecl {
	if language == "python" {
		return pythonVarDecls(node, src)
	}
	if language == "csharp" {
		return csharpVarDecls(node, src)
	}
	if language == "rust" {
		return rustVarDecls(node, src)
	}
	if language == "ruby" {
		return rubyVarDecls(node, src)
	}
	if language == "php" {
		return phpVarDecls(node, src)
	}
	if language == "c" || language == "cpp" {
		return cVarDecls(node, src)
	}
	if language == "kotlin" {
		return kotlinVarDecls(node, src)
	}
	if language == "dart" {
		return dartVarDecls(node, src)
	}
	if language == "lua" {
		return luaVarDecls(node, src)
	}
	if language == "scala" {
		return scalaVarDecls(node, src)
	}
	if language == "zig" {
		return zigVarDecls(node, src)
	}
	switch node.Kind() {
	case "field_declaration", "local_variable_declaration":
		if language == "go" {
			// Go's "field_declaration" (a struct field, e.g. `repo Repo`
			// inside a struct body) has direct "name"/"type" fields, no
			// "declarator" wrapper at all — a different shape from Java's
			// identically-named node kind handled below. Found completely
			// missing: Go struct fields were never captured as VarDecls,
			// which silently blocked every `s.repo.Find()`-shaped
			// two-level field-access call (resolveGoReceiverCall has
			// nothing to look the field's type up in without this) —
			// found via real-world testing of cross-file struct/method
			// linking, not assumed.
			typeNode := node.ChildByFieldName("type")
			nameNode := node.ChildByFieldName("name")
			if typeNode == nil || nameNode == nil {
				return nil
			}
			return []VarDecl{{Name: nameNode.Utf8Text(src), Type: goTypeName(typeNode, src), Pos: int(node.StartByte())}}
		}
		typeNode := node.ChildByFieldName("type")
		if typeNode == nil {
			return nil
		}
		typeName := simpleTypeName(typeNode, src)
		pos := int(node.StartByte())

		cursor := node.Walk()
		defer cursor.Close()
		var out []VarDecl
		for _, d := range node.ChildrenByFieldName("declarator", cursor) {
			if name := d.ChildByFieldName("name"); name != nil {
				out = append(out, VarDecl{Name: name.Utf8Text(src), Type: typeName, Pos: pos})
			}
		}
		return out
	case "formal_parameter", "parameter_declaration":
		// Java `formal_parameter` (e.g. `@PathVariable("id") int ownerId`) and
		// Go `parameter_declaration` (e.g. `idx *Index`) both expose "type"
		// and "name" fields.
		typeNode := node.ChildByFieldName("type")
		nameNode := node.ChildByFieldName("name")
		if typeNode == nil || nameNode == nil {
			return nil
		}
		typeName := simpleTypeName(typeNode, src)
		if language == "go" {
			typeName = goTypeName(typeNode, src)
		}
		return []VarDecl{{Name: nameNode.Utf8Text(src), Type: typeName, Pos: int(node.StartByte())}}
	case "var_declaration":
		// Go `var w *Wrapper` (one or more `var_spec` children, only those
		// with an explicit type are useful for call resolution).
		cursor := node.Walk()
		defer cursor.Close()
		var out []VarDecl
		for _, spec := range node.NamedChildren(cursor) {
			if spec.Kind() != "var_spec" {
				continue
			}
			typeNode := spec.ChildByFieldName("type")
			if typeNode == nil {
				continue
			}
			typeName := goTypeName(typeNode, src)
			specCursor := spec.Walk()
			defer specCursor.Close()
			for _, n := range spec.ChildrenByFieldName("name", specCursor) {
				out = append(out, VarDecl{Name: n.Utf8Text(src), Type: typeName, Pos: int(node.StartByte())})
			}
		}
		return out
	case "short_var_declaration":
		// Go `a := &Account{...}` / `b := Account{}` — only handle the
		// single-name, composite-literal-RHS shape; multi-value assignments
		// and function-call RHS would require return-type inference.
		left := node.ChildByFieldName("left")
		right := node.ChildByFieldName("right")
		if left == nil || right == nil || left.NamedChildCount() != 1 || right.NamedChildCount() != 1 {
			return nil
		}
		nameNode := left.NamedChild(0)
		if nameNode == nil || nameNode.Kind() != "identifier" {
			return nil
		}
		valNode := right.NamedChild(0)
		if valNode == nil {
			return nil
		}
		typeName := goCompositeLiteralType(valNode, src)
		if typeName == "" {
			return nil
		}
		return []VarDecl{{Name: nameNode.Utf8Text(src), Type: typeName, Pos: int(node.StartByte())}}
	case "enhanced_for_statement":
		// `for (Type name : value) { ... }` — the loop variable's declared
		// element type, scoped to the enclosing method like any other local.
		typeNode := node.ChildByFieldName("type")
		nameNode := node.ChildByFieldName("name")
		if typeNode == nil || nameNode == nil {
			return nil
		}
		return []VarDecl{{Name: nameNode.Utf8Text(src), Type: simpleTypeName(typeNode, src), Pos: int(node.StartByte())}}
	}
	return nil
}

// pythonVarDecls extracts explicit annotations and assignments whose right
// side constructs a class. This deliberately records only evidence that can
// identify a concrete declared type; arbitrary runtime assignments remain
// unknown rather than becoming name-based guesses.
func pythonVarDecls(node *sitter.Node, src []byte) []VarDecl {
	switch node.Kind() {
	case "typed_parameter":
		name := node.ChildByFieldName("name")
		if name == nil {
			name = node.NamedChild(0)
		}
		typ := node.ChildByFieldName("type")
		if name == nil || typ == nil {
			return nil
		}
		return []VarDecl{{Name: name.Utf8Text(src), Type: pythonTypeName(typ.Utf8Text(src)), Pos: int(node.StartByte())}}
	case "assignment":
		left := node.ChildByFieldName("left")
		if left == nil {
			return nil
		}
		name := pythonBindingName(left, src)
		if name == "" {
			return nil
		}
		if typ := node.ChildByFieldName("type"); typ != nil {
			if typeName := pythonTypeName(typ.Utf8Text(src)); typeName != "" {
				return []VarDecl{{Name: name, Type: typeName, Pos: int(node.StartByte())}}
			}
		}
		right := node.ChildByFieldName("right")
		if right == nil {
			return nil
		}
		if right.Kind() == "identifier" {
			// A class-body binding such as `url_map_class = Map` stores a
			// class object for later construction through
			// `self.url_map_class(...)`. Recording the identifier as a
			// candidate type is safe: graph resolution still requires it
			// to identify one concrete class and method. Function-local
			// `alias = value` remains ordinary type propagation.
			if pythonAssignmentInsideClass(node) {
				return []VarDecl{{Name: name, Type: right.Utf8Text(src), Pos: int(node.StartByte())}}
			}
			return []VarDecl{{Name: name, AliasOf: right.Utf8Text(src), Pos: int(node.StartByte())}}
		}
		if right.Kind() != "call" {
			return nil
		}
		fn := right.ChildByFieldName("function")
		if fn == nil || (fn.Kind() != "identifier" && fn.Kind() != "attribute") {
			return nil
		}
		if fn.Kind() == "attribute" {
			object := fn.ChildByFieldName("object")
			attribute := fn.ChildByFieldName("attribute")
			if object != nil && attribute != nil && (object.Utf8Text(src) == "self" || object.Utf8Text(src) == "cls") {
				return []VarDecl{{Name: name, AliasOf: attribute.Utf8Text(src), Pos: int(node.StartByte())}}
			}
		}
		return []VarDecl{{Name: name, Type: fn.Utf8Text(src), Pos: int(node.StartByte())}}
	}
	return nil
}

func pythonAssignmentInsideClass(node *sitter.Node) bool {
	for parent := node.Parent(); parent != nil; parent = parent.Parent() {
		switch parent.Kind() {
		case "function_definition", "lambda":
			return false
		case "class_definition":
			return true
		}
	}
	return false
}

func kotlinIsExported(defNode *sitter.Node, src []byte) bool {
	cursor := defNode.Walk()
	defer cursor.Close()
	for _, child := range defNode.NamedChildren(cursor) {
		if child.Kind() != "modifiers" {
			continue
		}
		for _, field := range strings.Fields(child.Utf8Text(src)) {
			switch field {
			case "private", "protected", "internal":
				return false
			case "public":
				return true
			}
		}
	}
	return true
}

func pythonBindingName(node *sitter.Node, src []byte) string {
	switch node.Kind() {
	case "identifier":
		return node.Utf8Text(src)
	case "attribute":
		object := node.ChildByFieldName("object")
		attribute := node.ChildByFieldName("attribute")
		if object != nil && attribute != nil && object.Utf8Text(src) == "self" {
			return "self." + attribute.Utf8Text(src)
		}
	}
	return ""
}

func pythonTypeName(raw string) string {
	raw = strings.Trim(strings.TrimSpace(raw), "\"'")
	if raw == "" {
		return ""
	}
	if pipe := strings.IndexByte(raw, '|'); pipe >= 0 {
		raw = strings.TrimSpace(raw[:pipe])
	}
	for _, wrapper := range []string{"Optional[", "Annotated[", "ClassVar["} {
		if strings.HasPrefix(raw, wrapper) && strings.HasSuffix(raw, "]") {
			raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(raw, wrapper), "]"))
			if comma := strings.IndexByte(raw, ','); comma >= 0 {
				raw = strings.TrimSpace(raw[:comma])
			}
			break
		}
	}
	return raw
}

// csharpVarDecls extracts variable-type bindings from a C# `field_declaration`
// or `local_declaration_statement` (each wraps a `variable_declaration` with
// a `type` field and one or more `variable_declarator` children, e.g.
// `private readonly IOwnerRepository _owners;`), `property_declaration`
// (fields `type`+`name`, e.g. `public IOwnerRepository Owners { get; }`), or
// `parameter` (fields `type`+`name`, e.g. `int ownerId`).
func csharpVarDecls(node *sitter.Node, src []byte) []VarDecl {
	switch node.Kind() {
	case "property_declaration", "parameter":
		typeNode := node.ChildByFieldName("type")
		nameNode := node.ChildByFieldName("name")
		if typeNode == nil || nameNode == nil {
			return nil
		}
		return []VarDecl{{Name: nameNode.Utf8Text(src), Type: simpleTypeName(typeNode, src), Pos: int(node.StartByte())}}
	case "field_declaration", "local_declaration_statement":
		cursor := node.Walk()
		defer cursor.Close()
		var out []VarDecl
		for _, c := range node.NamedChildren(cursor) {
			if c.Kind() != "variable_declaration" {
				continue
			}
			typeNode := c.ChildByFieldName("type")
			if typeNode == nil {
				continue
			}
			typeName := simpleTypeName(typeNode, src)
			vCursor := c.Walk()
			defer vCursor.Close()
			for _, d := range c.NamedChildren(vCursor) {
				if d.Kind() != "variable_declarator" {
					continue
				}
				if name := d.ChildByFieldName("name"); name != nil {
					out = append(out, VarDecl{Name: name.Utf8Text(src), Type: typeName, Pos: int(node.StartByte())})
				}
			}
		}
		return out
	}
	return nil
}

// rustVarDecls extracts declared simple-type names from a Rust `parameter`
// (`repo: PrimaryRepo`) or struct `field_declaration` (`repo: PrimaryRepo`
// inside `struct S { ... }`) for `variable.method()` call resolution.
// `&self`/`self` parameters are `self_parameter` nodes, not "parameter", so
// they never reach the parameter case (and need no type — resolveVarCall's
// "self."-prefix handling covers them separately).
// rubyVarDecls extracts an instance-variable assignment whose right side
// constructs a class, e.g. `@repo = Repo.new` — Ruby has no field
// declarations at all (unlike Java/C#/Rust/C++), so an ivar's type can only
// ever come from evidence at its assignment site. Ruby's idiom for
// construction is always `ClassName.new(...)` (a `call` node whose
// "receiver" is the class and "method" is literally "new", never a
// dedicated construction syntax), so the class name is the call's receiver,
// not the call's own callee text. Mirrors pythonVarDecls' "assignment" case;
// the caller promotes the "@"-prefixed name to the enclosing class via
// populateSelfAttributeVarTypes, the same role populatePythonVarTypes plays
// for "self.".
func rubyVarDecls(node *sitter.Node, src []byte) []VarDecl {
	if node.Kind() != "assignment" {
		return nil
	}
	left := node.ChildByFieldName("left")
	right := node.ChildByFieldName("right")
	if left == nil || right == nil {
		return nil
	}
	var name string
	switch left.Kind() {
	case "instance_variable":
		name = left.Utf8Text(src)
	case "identifier":
		name = left.Utf8Text(src)
	default:
		return nil
	}
	if right.Kind() == "identifier" {
		return []VarDecl{{Name: name, AliasOf: right.Utf8Text(src), Pos: int(node.StartByte())}}
	}
	if right.Kind() != "call" {
		return nil
	}
	method := right.ChildByFieldName("method")
	receiver := right.ChildByFieldName("receiver")
	if method == nil || receiver == nil || method.Utf8Text(src) != "new" {
		return nil
	}
	return []VarDecl{{Name: name, Type: receiver.Utf8Text(src), Pos: int(node.StartByte())}}
}

func rustVarDecls(node *sitter.Node, src []byte) []VarDecl {
	switch node.Kind() {
	case "parameter", "field_declaration":
		typeNode := node.ChildByFieldName("type")
		if typeNode == nil {
			return nil
		}
		nameNode := node.ChildByFieldName("pattern")
		if nameNode == nil {
			nameNode = node.ChildByFieldName("name")
		}
		if nameNode == nil || (nameNode.Kind() != "identifier" && nameNode.Kind() != "field_identifier") {
			return nil
		}
		return []VarDecl{{Name: nameNode.Utf8Text(src), Type: simpleTypeName(typeNode, src), Pos: int(node.StartByte())}}
	}
	return nil
}

// phpVarDecls extracts declared simple-type names from a PHP typed
// `simple_parameter` (`UserRepo $repo`, name kept with its "$" sigil — it
// matches a bare `$repo->method()` receiver's text exactly) or typed
// `property_declaration` (`private UserRepo $repo;`, one type shared across
// possibly several `property_element` declarators; name has its "$" sigil
// stripped, matching resolveVarCall's "$this->"-prefix-stripped receiver
// text) for `variable.method()`/`$this->field.method()` call resolution.
func phpVarDecls(node *sitter.Node, src []byte) []VarDecl {
	switch node.Kind() {
	case "simple_parameter":
		typeNode := node.ChildByFieldName("type")
		nameNode := node.ChildByFieldName("name")
		if typeNode == nil || nameNode == nil {
			return nil
		}
		return []VarDecl{{Name: nameNode.Utf8Text(src), Type: simpleTypeName(typeNode, src), Pos: int(node.StartByte())}}
	case "property_promotion_parameter":
		// PHP 8's constructor property promotion
		// (`__construct(private Repo $repo)`) is, in modern PHP, the
		// dominant way a class declares a typed dependency — found via
		// real-world testing (neither Laravel nor Monolog had a single
		// `$this->x = new Y();` assignment anywhere; both have fully moved
		// to this syntax instead). A promoted parameter is both the
		// parameter declaration *and* an implicit `$this->name = $name`
		// assignment with no literal "$this->" text anywhere to capture —
		// returning the name pre-prefixed with "$this->" routes it through
		// the exact same populateSelfAttributeVarTypes promotion already
		// wired for PHP's real "$this->x = new Y()" case, with no new
		// plumbing needed, even though nothing in the source actually
		// reads "$this->repo".
		typeNode := node.ChildByFieldName("type")
		nameNode := node.ChildByFieldName("name")
		if typeNode == nil || nameNode == nil {
			return nil
		}
		return []VarDecl{{
			Name: "$this->" + strings.TrimPrefix(nameNode.Utf8Text(src), "$"),
			Type: simpleTypeName(typeNode, src),
			Pos:  int(node.StartByte()),
		}}
	case "property_declaration":
		typeNode := node.ChildByFieldName("type")
		if typeNode == nil {
			return nil
		}
		typeName := simpleTypeName(typeNode, src)
		pos := int(node.StartByte())
		cursor := node.Walk()
		defer cursor.Close()
		var out []VarDecl
		for _, c := range node.NamedChildren(cursor) {
			if c.Kind() != "property_element" {
				continue
			}
			varNode := c.NamedChild(0)
			if varNode == nil {
				continue
			}
			out = append(out, VarDecl{Name: strings.TrimPrefix(varNode.Utf8Text(src), "$"), Type: typeName, Pos: pos})
		}
		return out
	case "assignment_expression":
		// `$this->repo = new Repo();` — PHP's untyped-property convention
		// (no `private Repo $repo;` type hint at all, just bare
		// `private $repo;`, still extremely common in PHP 5/7-style code);
		// the property_declaration case above only has evidence when a
		// type hint is actually present. The caller promotes the
		// "$this->"-prefixed name to the enclosing class via
		// populateSelfAttributeVarTypes, the same role
		// populatePythonVarTypes plays for Python's "self.".
		left := node.ChildByFieldName("left")
		right := node.ChildByFieldName("right")
		if left == nil || right == nil || left.Kind() != "member_access_expression" {
			return nil
		}
		object := left.ChildByFieldName("object")
		name := left.ChildByFieldName("name")
		if object == nil || name == nil || object.Utf8Text(src) != "$this" {
			return nil
		}
		fullName := "$this->" + name.Utf8Text(src)
		if right.Kind() == "variable_name" {
			// Keep the "$" prefix in AliasOf, matching simple_parameter's
			// own Name format ("$files", not "files") exactly — found via
			// a real-world test (Laravel's `$this->files = $files;`,
			// `$files` typed by a constructor parameter) silently failing
			// to resolve: stripping "$" here while simple_parameter's
			// Name keeps it meant the alias lookup could never match.
			return []VarDecl{{Name: fullName, AliasOf: right.Utf8Text(src), Pos: int(node.StartByte())}}
		}
		if right.Kind() != "object_creation_expression" {
			return nil
		}
		cls := right.NamedChild(0)
		if cls == nil {
			return nil
		}
		return []VarDecl{{Name: fullName, Type: simpleTypeName(cls, src), Pos: int(node.StartByte())}}
	}
	return nil
}

// cVarDecls extracts declared simple-type names from a C `parameter_declaration`
// (`struct UserRepo *repo`) or `field_declaration` (`struct UserRepo *repo;`
// inside `struct S { ... }`), unwrapping any pointer_declarator layers via
// cDeclaratorName to find the variable's name — both node kinds expose only
// "type"/"declarator" fields (no "name" field, unlike Java/Go's same-named
// node kinds, since C declarators nest pointer/array/function wrapping
// arbitrarily) for `variable->method()`-shaped call resolution (mamari has
// no concept of methods in C, so this only powers field/parameter type
// lookups, not method dispatch).
func cVarDecls(node *sitter.Node, src []byte) []VarDecl {
	switch node.Kind() {
	case "parameter_declaration", "field_declaration":
		typeNode := node.ChildByFieldName("type")
		declNode := node.ChildByFieldName("declarator")
		if typeNode == nil || declNode == nil {
			return nil
		}
		nameNode := cDeclaratorName(declNode)
		if nameNode == nil {
			return nil
		}
		return []VarDecl{{Name: nameNode.Utf8Text(src), Type: simpleTypeName(typeNode, src), Pos: int(node.StartByte())}}
	case "declaration":
		typeNode := node.ChildByFieldName("type")
		if typeNode == nil {
			return nil
		}
		typeName := simpleTypeName(typeNode, src)
		cursor := node.Walk()
		defer cursor.Close()
		var out []VarDecl
		for _, declarator := range node.ChildrenByFieldName("declarator", cursor) {
			decl := &declarator
			if declarator.Kind() == "init_declarator" {
				decl = declarator.ChildByFieldName("declarator")
			}
			nameNode := cDeclaratorName(decl)
			if nameNode == nil {
				continue
			}
			out = append(out, VarDecl{Name: nameNode.Utf8Text(src), Type: typeName, Pos: int(node.StartByte())})
		}
		return out
	}
	return nil
}

// cppStackConstructorCalls extracts only the declaration forms that the C++
// grammar distinguishes from function declarations: `Type value(args)` and
// `Type value{args}`. A prototype such as `Type value(int)` has a
// function_declarator instead of an init_declarator and is therefore ignored.
// Primitive direct initialization (`int value(1)`) is returned too, but marked
// Constructor so graph resolution can require a class-like target and drop it.
func cppStackConstructorCalls(node *sitter.Node, src []byte) []Call {
	if node.Kind() != "declaration" {
		return nil
	}
	typeNode := node.ChildByFieldName("type")
	if typeNode == nil {
		return nil
	}
	typeName := simpleTypeName(typeNode, src)
	if typeName == "" {
		return nil
	}

	cursor := node.Walk()
	defer cursor.Close()
	var out []Call
	for _, declarator := range node.ChildrenByFieldName("declarator", cursor) {
		if declarator.Kind() != "init_declarator" {
			continue
		}
		value := declarator.ChildByFieldName("value")
		if value == nil || value.Kind() != "argument_list" && value.Kind() != "initializer_list" {
			continue
		}
		out = append(out, Call{
			Callee: typeName, Start: int(declarator.StartByte()), End: int(declarator.EndByte()),
			Constructor: true,
		})
	}
	return out
}

// kotlinVarDecls extracts declared simple-type names from a Kotlin
// `class_parameter` (primary-constructor property, e.g.
// `private val repo: UserRepo`) or `parameter` (function parameter, e.g.
// `id: Int`) for `variable.method()` call resolution. Neither node kind has
// field names in this grammar (unlike every other language handled here):
// both shapes consistently end with [..., name: identifier, type: user_type]
// regardless of whether optional leading modifiers/val/var tokens are
// present, so the last two named children are always (name, type) — no
// positional ambiguity to resolve.
func kotlinVarDecls(node *sitter.Node, src []byte) []VarDecl {
	switch node.Kind() {
	case "class_parameter", "parameter":
		n := node.NamedChildCount()
		if n < 2 {
			return nil
		}
		nameNode := node.NamedChild(n - 2)
		typeNode := node.NamedChild(n - 1)
		if nameNode == nil || typeNode == nil || nameNode.Kind() != "identifier" {
			return nil
		}
		return []VarDecl{{Name: nameNode.Utf8Text(src), Type: simpleTypeName(typeNode, src), Pos: int(node.StartByte())}}
	case "property_declaration":
		// A body-level `var`/`val` property, e.g. `var repo: Repo` or
		// `var repo = Repo()` — distinct from class_parameter (a
		// primary-constructor property) and the only other shape that
		// declares a typed instance field in idiomatic Kotlin. Found
		// missing via a same-name-collision test: a class declaring its
		// dependency as a body-level property (instead of a constructor
		// parameter) had its `repo.find(id)` call silently fail to
		// type-resolve even though the type annotation was right there.
		// An optional leading "modifiers" child (`private val client: ...`)
		// shifts every later child's position by one — the same hazard
		// kotlinVarDecls' class_parameter/parameter case already avoids by
		// indexing from the end. Found via real-world testing: OkHttp's
		// `private val client: OkHttpClient = ...` silently extracted
		// nothing at all, since NamedChild(0) was "modifiers", not
		// "variable_declaration", whenever a visibility modifier was
		// present — which is most real-world Kotlin properties. Locate the
		// two children we need by kind instead of fixed position.
		var decl, init *sitter.Node
		propCursor := node.Walk()
		defer propCursor.Close()
		for _, c := range node.NamedChildren(propCursor) {
			switch c.Kind() {
			case "variable_declaration":
				if decl == nil {
					decl = &c
				}
			case "modifiers":
				// skip
			default:
				if init == nil {
					init = &c
				}
			}
		}
		if decl == nil {
			return nil
		}
		nameNode := decl.NamedChild(0)
		if nameNode == nil || nameNode.Kind() != "identifier" {
			return nil
		}
		if typeNode := decl.NamedChild(1); typeNode != nil {
			return []VarDecl{{Name: nameNode.Utf8Text(src), Type: simpleTypeName(typeNode, src), Pos: int(node.StartByte())}}
		}
		// No explicit type: infer it from a same-line constructor call
		// initializer (`var repo = Repo()`). Kotlin's call_expression has
		// no field names at all (see callFromNode's kotlin case) — the
		// callee is always the first named child positionally.
		if init == nil || init.Kind() != "call_expression" {
			return nil
		}
		fn := init.NamedChild(0)
		if fn == nil || fn.Kind() != "identifier" {
			return nil
		}
		return []VarDecl{{Name: nameNode.Utf8Text(src), Type: fn.Utf8Text(src), Pos: int(node.StartByte())}}
	}
	return nil
}

// simpleTypeName collapses a Java type node to its simple (unqualified,
// non-generic) name, e.g. `ArrayList<String>` -> "ArrayList",
// `java.util.ArrayList` -> "ArrayList".
func simpleTypeName(typ *sitter.Node, src []byte) string {
	switch typ.Kind() {
	case "generic_type":
		if base := typ.Child(0); base != nil {
			return simpleTypeName(base, src)
		}
	case "scoped_type_identifier", "qualified_type", "qualified_identifier":
		// C++ `app::UserRepo` used as a type -> "UserRepo".
		if n := typ.ChildCount(); n > 0 {
			if last := typ.Child(n - 1); last != nil {
				return simpleTypeName(last, src)
			}
		}
	case "pointer_type":
		// Go `*Foo` / `*pkg.Foo` -> "Foo".
		if base := typ.NamedChild(0); base != nil {
			return simpleTypeName(base, src)
		}
	case "generic_name", "template_type", "user_type":
		// C# `List<Owner>` / C++ `Foo<int>` / Kotlin `List<Foo>` -> "List" /
		// "Foo" / "List". Kotlin's "user_type" is the type node for every
		// declared type, generic or not (unlike C#/C++ where the
		// generic-vs-plain distinction is two different node kinds) — its
		// first named child is always the bare name either way.
		if base := typ.NamedChild(0); base != nil {
			return simpleTypeName(base, src)
		}
	case "nullable_type", "array_type":
		// C# `Owner?` / `Owner[]` -> "Owner".
		if base := typ.ChildByFieldName("type"); base != nil {
			return simpleTypeName(base, src)
		}
		if base := typ.NamedChild(0); base != nil {
			return simpleTypeName(base, src)
		}
	case "qualified_name":
		// C# `System.Collections.Generic.List` -> "List".
		if name := typ.ChildByFieldName("name"); name != nil {
			return simpleTypeName(name, src)
		}
	case "reference_type":
		// Rust `&Logger` / `&mut Config` -> "Logger" / "Config".
		if base := typ.ChildByFieldName("type"); base != nil {
			return simpleTypeName(base, src)
		}
	case "struct_specifier", "union_specifier", "enum_specifier", "class_specifier":
		// C/C++ `struct UserService` / `union X` / `enum Status` / C++
		// `class Foo` -> the bare name, stripping the keyword (the default
		// Utf8Text fallback would otherwise return "struct UserService",
		// which never matches a struct symbol's plain Name).
		if name := typ.ChildByFieldName("name"); name != nil {
			return name.Utf8Text(src)
		}
	}
	return typ.Utf8Text(src)
}

// goTypeName removes pointer/collection wrappers while preserving a package
// qualifier. `*testing.T` therefore becomes `testing.T`, not just `T`.
// That qualifier is required to distinguish a proven external receiver from
// an unknown object that could still dispatch to a repository method.
func goTypeName(typ *sitter.Node, src []byte) string {
	switch typ.Kind() {
	case "pointer_type":
		if base := typ.NamedChild(0); base != nil {
			return goTypeName(base, src)
		}
	case "generic_type":
		if base := typ.NamedChild(0); base != nil {
			return goTypeName(base, src)
		}
	case "slice_type", "array_type", "channel_type":
		if element := typ.ChildByFieldName("element"); element != nil {
			return goTypeName(element, src)
		}
		if element := typ.NamedChild(0); element != nil {
			return goTypeName(element, src)
		}
	}
	return typ.Utf8Text(src)
}

// cDeclaratorName unwraps a C/C++ declarator through any number of
// pointer_declarator layers (`*x`, `**x`, ...) to find the innermost
// identifier, e.g. for a `struct UserService *svc` parameter's declarator.
// Returns nil for declarator shapes this doesn't handle (arrays, function
// pointers) rather than guessing.
func cDeclaratorName(decl *sitter.Node) *sitter.Node {
	for decl != nil && decl.Kind() == "pointer_declarator" {
		decl = decl.ChildByFieldName("declarator")
	}
	if decl == nil {
		return nil
	}
	switch decl.Kind() {
	case "identifier":
		return decl
	case "field_identifier":
		// A value-type (non-pointer) struct/class member's declarator is a
		// "field_identifier", a distinct node kind from the "identifier"
		// used for free variables and parameters in tree-sitter-c/cpp's
		// grammar — e.g. `Repo repo;` inside a class body. Missing this
		// meant every plain (non-pointer) C++ class member, and C struct
		// member, was silently never recorded as a VarDecl at all, found
		// via a direct AST dump after a same-name-collision test showed
		// `repo.find(id)` failing to resolve despite `Repo repo;` being
		// declared right there in the class.
		return decl
	}
	return nil
}

// goCompositeLiteralType returns the simple type name of a Go composite
// literal, stripping a leading address-of: `&Account{...}` or `Account{}`
// -> "Account". Returns "" for any other expression kind (function calls,
// identifiers, etc.) since those would require return-type inference.
func goCompositeLiteralType(n *sitter.Node, src []byte) string {
	switch n.Kind() {
	case "unary_expression":
		if op := n.ChildByFieldName("operand"); op != nil {
			return goCompositeLiteralType(op, src)
		}
	case "composite_literal":
		if t := n.ChildByFieldName("type"); t != nil {
			return simpleTypeName(t, src)
		}
	}
	return ""
}

// importsFromRustUse extracts every leaf import spec from a Rust
// `use_declaration`, recursing through `use_list`/`scoped_use_list` group
// forms and resolving `as` aliases, e.g. `use std::{fmt, io::Write};` ->
// ["std::fmt", "std::io::Write"], `use a::B as C;` -> [{Spec: "a::B", Alias:
// "C"}], `use a::*;` -> ["a::*"].
func importsFromRustUse(stmt *sitter.Node, src []byte) []Import {
	arg := stmt.ChildByFieldName("argument")
	if arg == nil {
		return nil
	}
	var out []Import
	var walk func(n *sitter.Node, prefix string)
	walk = func(n *sitter.Node, prefix string) {
		join := func(part string) string {
			if prefix == "" {
				return part
			}
			return prefix + "::" + part
		}
		switch n.Kind() {
		case "use_as_clause":
			path := n.ChildByFieldName("path")
			alias := n.ChildByFieldName("alias")
			if path == nil {
				return
			}
			imp := Import{Spec: join(path.Utf8Text(src)), Start: int(stmt.StartByte()), End: int(stmt.EndByte())}
			if alias != nil {
				imp.Alias = alias.Utf8Text(src)
			}
			out = append(out, imp)
		case "use_list":
			cursor := n.Walk()
			defer cursor.Close()
			for _, c := range n.NamedChildren(cursor) {
				walk(&c, prefix)
			}
		case "scoped_use_list":
			path := n.ChildByFieldName("path")
			list := n.ChildByFieldName("list")
			newPrefix := prefix
			if path != nil {
				newPrefix = join(path.Utf8Text(src))
			}
			if list != nil {
				walk(list, newPrefix)
			}
		case "use_wildcard":
			if path := n.NamedChild(0); path != nil {
				out = append(out, Import{Spec: join(path.Utf8Text(src)) + "::*", Start: int(stmt.StartByte()), End: int(stmt.EndByte())})
			}
		case "scoped_identifier", "identifier", "crate", "self", "super", "metavariable":
			out = append(out, Import{Spec: join(n.Utf8Text(src)), Start: int(stmt.StartByte()), End: int(stmt.EndByte())})
		}
	}
	walk(arg, "")
	return out
}

// importsFromRubyRequire extracts the literal path argument of a Ruby
// `require '...'` / `require_relative '...'` call, e.g.
// `require_relative './repo'` -> "./repo". call is the outer `call` node
// (the calls.scm pattern already restricted method to require/
// require_relative and confirmed a single string argument via predicate).
func importsFromRubyRequire(call *sitter.Node, src []byte) []Import {
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return nil
	}
	cursor := args.Walk()
	defer cursor.Close()
	for _, c := range args.NamedChildren(cursor) {
		if c.Kind() != "string" {
			continue
		}
		content := c.NamedChild(0)
		if content == nil {
			continue
		}
		return []Import{{Spec: content.Utf8Text(src), Start: int(call.StartByte()), End: int(call.EndByte())}}
	}
	return nil
}

// importsFromPHPUse extracts the namespace spec from one PHP
// `namespace_use_clause` (one per imported name — PHP's grouped
// `use A, B;`/`use Ns\{A, B};` forms produce one clause node per name, so no
// recursion is needed here unlike Rust's nested use-tree). Aliasing (`use X
// as Y;`) is retained in Import.Alias so relation resolution can map local
// type names back to their canonical imported class.
func importsFromPHPUse(clause *sitter.Node, src []byte) []Import {
	name := clause.NamedChild(0)
	if name == nil {
		return nil
	}
	spec := name.Utf8Text(src)
	if spec == "" {
		return nil
	}
	imp := Import{Spec: spec, Start: int(clause.StartByte()), End: int(clause.EndByte())}
	if alias := clause.ChildByFieldName("alias"); alias != nil {
		imp.Alias = alias.Utf8Text(src)
	}
	return []Import{imp}
}

func importsFromHaskellImport(stmt *sitter.Node, src []byte) []Import {
	module := stmt.ChildByFieldName("module")
	if module == nil {
		return nil
	}
	imp := Import{Spec: module.Utf8Text(src), Start: int(stmt.StartByte()), End: int(stmt.EndByte())}
	if alias := stmt.ChildByFieldName("alias"); alias != nil {
		imp.Alias = alias.Utf8Text(src)
	}
	return []Import{imp}
}

func importsFromElixirAlias(call *sitter.Node, src []byte) []Import {
	target := call.ChildByFieldName("target")
	if target == nil || target.Kind() != "identifier" || target.Utf8Text(src) != "alias" {
		return nil
	}
	args := call.ChildByFieldName("arguments")
	if args == nil {
		cursor := call.Walk()
		defer cursor.Close()
		for _, child := range call.NamedChildren(cursor) {
			if child.Kind() == "arguments" {
				c := child
				args = &c
				break
			}
		}
	}
	if args == nil {
		return nil
	}
	var aliases []*sitter.Node
	var collect func(*sitter.Node)
	collect = func(node *sitter.Node) {
		if node.Kind() == "alias" {
			aliases = append(aliases, node)
			return
		}
		cursor := node.Walk()
		defer cursor.Close()
		for _, child := range node.NamedChildren(cursor) {
			c := child
			collect(&c)
		}
	}
	collect(args)
	if len(aliases) == 0 {
		return nil
	}
	imp := Import{Spec: aliases[0].Utf8Text(src), Start: int(call.StartByte()), End: int(call.EndByte())}
	if len(aliases) > 1 {
		imp.Alias = aliases[len(aliases)-1].Utf8Text(src)
	}
	return []Import{imp}
}

func importsFromLuaRequire(decl *sitter.Node, src []byte) []Import {
	assignment := decl.NamedChild(0)
	if assignment == nil || assignment.Kind() != "assignment_statement" {
		return nil
	}
	left := assignment.NamedChild(0)
	right := assignment.NamedChild(1)
	if left == nil || right == nil || left.Kind() != "variable_list" || right.Kind() != "expression_list" {
		return nil
	}
	leftCursor := left.Walk()
	defer leftCursor.Close()
	names := left.ChildrenByFieldName("name", leftCursor)
	rightCursor := right.Walk()
	defer rightCursor.Close()
	values := right.ChildrenByFieldName("value", rightCursor)
	if len(names) != 1 || len(values) != 1 || names[0].Kind() != "identifier" || values[0].Kind() != "function_call" {
		return nil
	}
	fn := values[0].ChildByFieldName("name")
	if fn == nil || fn.Kind() != "identifier" || fn.Utf8Text(src) != "require" {
		return nil
	}
	args := values[0].ChildByFieldName("arguments")
	if args == nil || args.NamedChildCount() != 1 {
		return nil
	}
	value := args.NamedChild(0)
	if value == nil || value.Kind() != "string" {
		return nil
	}
	spec := strings.Trim(value.Utf8Text(src), "\"'")
	if spec == "" {
		return nil
	}
	return []Import{{
		Spec: spec, Alias: names[0].Utf8Text(src),
		Start: int(decl.StartByte()), End: int(decl.EndByte()),
	}}
}

func importsFromSwiftImport(decl *sitter.Node, src []byte) []Import {
	fields := strings.Fields(decl.Utf8Text(src))
	if len(fields) < 2 || fields[0] != "import" {
		return nil
	}
	specIndex := 1
	switch fields[1] {
	case "actor", "class", "enum", "func", "let", "protocol", "struct", "typealias", "var":
		specIndex = 2
	}
	if specIndex >= len(fields) {
		return nil
	}
	spec := strings.TrimSpace(fields[specIndex])
	if spec == "" {
		return nil
	}
	return []Import{{Spec: spec, Start: int(decl.StartByte()), End: int(decl.EndByte())}}
}

// importsFromKotlinImport extracts the dotted path from a Kotlin
// `import a.b.C` (sole named child, a "qualified_identifier") or `import
// a.b.C as D` (a second named child holds the "as" alias) statement.
func importsFromKotlinImport(stmt *sitter.Node, src []byte) []Import {
	path := stmt.NamedChild(0)
	if path == nil {
		return nil
	}
	spec := path.Utf8Text(src)
	if spec == "" {
		return nil
	}
	imp := Import{Spec: spec, Start: int(stmt.StartByte()), End: int(stmt.EndByte())}
	if alias := stmt.NamedChild(1); alias != nil {
		imp.Alias = alias.Utf8Text(src)
	}
	return []Import{imp}
}

// importsFromBashSource extracts the path argument from a Bash
// `source ./lib.sh` or `. ./lib.sh` statement — Bash's own form of
// importing another file's definitions into the current shell. stmt is the
// outer "command" node (the calls.scm pattern already restricted its name
// to "source"/"." via predicate).
func importsFromBashSource(stmt *sitter.Node, src []byte) []Import {
	cursor := stmt.Walk()
	defer cursor.Close()
	for _, arg := range stmt.ChildrenByFieldName("argument", cursor) {
		if arg.Kind() != "word" {
			continue
		}
		spec := arg.Utf8Text(src)
		if spec == "" {
			return nil
		}
		return []Import{{Spec: spec, Start: int(stmt.StartByte()), End: int(stmt.EndByte())}}
	}
	return nil
}

// importsFromScalaImport extracts the dotted path(s) from a Scala
// `import a.b.C` (single path, flat `identifier` children) or
// `import a.b.{C, D}` (grouped form, a trailing "namespace_selectors" child
// listing multiple names sharing the same prefix) statement. The renaming
// form `import a.b.{C => D}` is not specially handled — out of scope, the
// same tradeoff PHP's `use X as Y` import aliasing already accepts.
func importsFromScalaImport(stmt *sitter.Node, src []byte) []Import {
	cursor := stmt.Walk()
	defer cursor.Close()
	var prefix []string
	var out []Import
	for _, c := range stmt.NamedChildren(cursor) {
		if c.Kind() == "namespace_selectors" {
			scursor := c.Walk()
			defer scursor.Close()
			for _, sel := range c.NamedChildren(scursor) {
				if sel.Kind() != "identifier" {
					continue
				}
				spec := strings.Join(append(append([]string(nil), prefix...), sel.Utf8Text(src)), ".")
				out = append(out, Import{Spec: spec, Start: int(stmt.StartByte()), End: int(stmt.EndByte())})
			}
			continue
		}
		if c.Kind() == "identifier" {
			prefix = append(prefix, c.Utf8Text(src))
		}
	}
	if len(out) == 0 && len(prefix) > 0 {
		out = append(out, Import{Spec: strings.Join(prefix, "."), Start: int(stmt.StartByte()), End: int(stmt.EndByte())})
	}
	return out
}

// importsFromCInclude extracts the path from a C/C++ `#include <stdio.h>`
// (sole child `system_lib_string`, angle brackets stripped) or
// `#include "repo.h"` (sole child `string_literal`, whose own sole child
// `string_content` is already quote-stripped).
func importsFromCInclude(stmt *sitter.Node, src []byte) []Import {
	path := stmt.NamedChild(0)
	if path == nil {
		return nil
	}
	var spec string
	switch path.Kind() {
	case "system_lib_string":
		spec = strings.Trim(path.Utf8Text(src), "<>")
	case "string_literal":
		if content := path.NamedChild(0); content != nil {
			spec = content.Utf8Text(src)
		}
	}
	if spec == "" {
		return nil
	}
	return []Import{{Spec: spec, Start: int(stmt.StartByte()), End: int(stmt.EndByte())}}
}

// extractCallsAndImports runs the calls query and turns call/import
// statement nodes into plain Call/Import structs.
func extractCallsAndImports(language string, query *sitter.Query, root *sitter.Node, src []byte) ([]Call, []Import, []VarDecl, []CallAssign, error) {
	cursor := sitter.NewQueryCursor()
	defer cursor.Close()

	names := query.CaptureNames()

	var calls []Call
	var imports []Import
	var vars []VarDecl
	var callAssigns []CallAssign

	matches := cursor.Matches(query, root, src)
	for {
		m := matches.Next()
		if m == nil {
			break
		}
		for _, c := range m.Captures {
			node := c.Node
			switch names[c.Index] {
			case "call":
				calls = append(calls, callFromNode(language, &node, src))
			case "import":
				switch node.Kind() {
				case "import_declaration":
					imports = append(imports, importsFromJavaImport(&node, src)...)
				default:
					imports = append(imports, importsFromImportStatement(&node, src)...)
				}
			case "import_from":
				imports = append(imports, importsFromFromStatement(&node, src)...)
			case "import_go":
				imports = append(imports, importsFromGoImport(&node, src)...)
			case "import_csharp":
				imports = append(imports, importsFromCSharpUsing(&node, src)...)
			case "import_rust":
				imports = append(imports, importsFromRustUse(&node, src)...)
			case "import_ruby":
				imports = append(imports, importsFromRubyRequire(&node, src)...)
			case "import_php":
				imports = append(imports, importsFromPHPUse(&node, src)...)
			case "import_c":
				imports = append(imports, importsFromCInclude(&node, src)...)
			case "import_kotlin":
				imports = append(imports, importsFromKotlinImport(&node, src)...)
			case "import_bash":
				imports = append(imports, importsFromBashSource(&node, src)...)
			case "import_scala":
				imports = append(imports, importsFromScalaImport(&node, src)...)
			case "import_haskell":
				imports = append(imports, importsFromHaskellImport(&node, src)...)
			case "import_elixir":
				imports = append(imports, importsFromElixirAlias(&node, src)...)
			case "import_lua":
				imports = append(imports, importsFromLuaRequire(&node, src)...)
			case "import_swift":
				imports = append(imports, importsFromSwiftImport(&node, src)...)
			case "vardecl":
				vars = append(vars, varDeclsFromNode(language, &node, src)...)
			case "callassign":
				callAssigns = append(callAssigns, callAssignsFromNode(&node, src)...)
			case "constructor_cpp":
				calls = append(calls, cppStackConstructorCalls(&node, src)...)
			}
		}
	}

	return calls, imports, vars, callAssigns, nil
}

func callFromNode(language string, call *sitter.Node, src []byte) Call {
	c := Call{Start: int(call.StartByte()), End: int(call.EndByte()), InDecorator: callInDecorator(call)}

	if language == "ruby" && call.Kind() == "super" {
		// Bare `super`/`super(args)` with no explicit receiver shares the
		// "call" node shape (method field resolves to a "super" node, a
		// subtype of the "_variable" supertype) only when parenthesized.
		// Argument-less, paren-less `super` — the simpler, equally common
		// form (`super` alone, relying on the same arguments as the
		// enclosing method) — has no wrapping "call" node at all; the
		// "super" node itself is a direct statement, found only by dumping
		// the actual parse tree, not assumed from the parenthesized case
		// alone.
		c.Callee = "super"
		return c
	}

	if language == "ruby" && call.Kind() == "call" {
		// Ruby's "call" node exposes receiver/method directly as fields,
		// unlike Python's same-named "call" node (function field, handled by
		// the generic fallback below) — must be disambiguated by language,
		// not by node kind alone.
		if name := call.ChildByFieldName("method"); name != nil {
			c.Callee = name.Utf8Text(src)
		}
		if recv := call.ChildByFieldName("receiver"); recv != nil {
			c.Receiver = recv.Utf8Text(src)
		}
		return c
	}

	if language == "php" {
		// PHP's "object_creation_expression" (`new UserRepo()`) has no
		// "type" field — unlike Java's same-named node, handled by the case
		// below — so it must be disambiguated by language too; the class
		// name is simply the node's first named child.
		switch call.Kind() {
		case "object_creation_expression":
			if cls := call.NamedChild(0); cls != nil {
				c.Callee = simpleTypeName(cls, src)
			}
			return c
		case "function_call_expression":
			if fn := call.ChildByFieldName("function"); fn != nil {
				c.Callee = fn.Utf8Text(src)
			}
			return c
		case "member_call_expression", "nullsafe_member_call_expression":
			// `$a->b()` and PHP 8's null-safe `$a?->b()` share the same
			// "object"/"name" fields — only the node kind differs.
			if name := call.ChildByFieldName("name"); name != nil {
				c.Callee = name.Utf8Text(src)
			}
			if obj := call.ChildByFieldName("object"); obj != nil {
				c.Receiver = obj.Utf8Text(src)
			}
			return c
		case "scoped_call_expression":
			if name := call.ChildByFieldName("name"); name != nil {
				c.Callee = name.Utf8Text(src)
			}
			if scope := call.ChildByFieldName("scope"); scope != nil {
				c.Receiver = scope.Utf8Text(src)
			}
			return c
		}
	}

	if language == "clojure" && call.Kind() == "list_lit" {
		// Fully homoiconic: `(helper id)` and `(let [x 1] x)` are both,
		// structurally, just a "list_lit" with a symbol first child — the
		// literal text of that symbol is the only signal distinguishing a
		// real call from a special form/core macro that must not be
		// treated as one (clojureSpecialForms). A namespaced call like
		// `(string/upper-case s)` has its first child's own text already
		// containing the "/" qualifier, so no separate receiver needs to
		// be extracted — the whole symbol is just used as the callee.
		first := call.NamedChild(0)
		if first == nil || first.Kind() != "sym_lit" {
			return c
		}
		nameNode := first.NamedChild(0)
		if nameNode == nil {
			return c
		}
		text := nameNode.Utf8Text(src)
		if clojureSpecialForms[text] {
			return c
		}
		c.Callee = text
		return c
	}

	if language == "haskell" && call.Kind() == "apply" {
		// Haskell's curried application is right-nested: `f x y` is
		// `apply{function: apply{function: f, argument: x}, argument: y}`
		// — one "apply" node per argument, not one node per call. Only the
		// outermost apply in a chain should be treated as "the call";
		// inner ones are intermediate nodes, not separate invocations, and
		// must be skipped (returning an empty Call, which the caller drops)
		// or every multi-arg call would also emit a phantom partial-
		// application "call" for each intermediate arity.
		if parent := call.Parent(); parent != nil && parent.Kind() == "apply" {
			if fn := parent.ChildByFieldName("function"); fn != nil &&
				fn.StartByte() == call.StartByte() && fn.EndByte() == call.EndByte() {
				return c
			}
		}
		base := call
		for base.Kind() == "apply" {
			fn := base.ChildByFieldName("function")
			if fn == nil {
				return c
			}
			base = fn
		}
		switch base.Kind() {
		case "variable", "constructor":
			c.Callee = base.Utf8Text(src)
		case "qualified":
			if module := base.ChildByFieldName("module"); module != nil {
				c.Receiver = strings.TrimSuffix(module.Utf8Text(src), ".")
			}
			if id := base.ChildByFieldName("id"); id != nil {
				c.Callee = id.Utf8Text(src)
			}
		}
		return c
	}

	if language == "dart" && call.Kind() == "selector" {
		// Dart's grammar has no call-expression node at all: `a.b()` parses
		// as a flat run of siblings under whatever the enclosing
		// expression/statement is — [identifier "a", selector ".b" (a
		// dot-access selector), selector "(...)" (this capture, an
		// argument-bearing selector)] — not a single node with
		// receiver/callee fields like every other language handled here.
		// The callee is recovered positionally: the selector immediately
		// preceding the call selector is either a dot-access selector
		// (whose own name is the callee, with the receiver reconstructed
		// as the raw text of everything before it, correctly handling
		// chains like `a.b.c()`) or, for a bare call, the primary
		// identifier itself.
		c.Receiver, c.Callee = dartCallReceiverCallee(call, src)
		return c
	}

	if language == "elixir" && call.Kind() == "call" {
		target := call.ChildByFieldName("target")
		if target == nil {
			return c
		}
		// Skip both shapes that are syntactically calls but are not real
		// invocations: the def-keyword wrapper itself (`def foo(...) do...
		// end` is, structurally, just a call to a function literally named
		// "def") and the synthetic "name(args)" call tree-sitter-elixir
		// nests as that wrapper's own first argument — see
		// elixirIsDefNameCall. Without both exclusions every single
		// definition would also emit a spurious self-call edge to its own
		// name.
		if target.Kind() == "identifier" && elixirDefKeywords[target.Utf8Text(src)] {
			return c
		}
		if elixirIsDefNameCall(call, src) {
			return c
		}
		switch target.Kind() {
		case "identifier":
			c.Callee = target.Utf8Text(src)
		case "dot":
			if left := target.ChildByFieldName("left"); left != nil {
				c.Receiver = left.Utf8Text(src)
			}
			if right := target.ChildByFieldName("right"); right != nil {
				c.Callee = right.Utf8Text(src)
			}
		}
		return c
	}

	if language == "lua" && call.Kind() == "function_call" {
		// Lua's "function_call" node kind is unique to Lua (no collision
		// risk). Its "name" field is one of three shapes: a bare identifier
		// (`helper()`), a "dot_index_expression" (`Table.field()`, no
		// implicit self), or a "method_index_expression" (`obj:method()`,
		// Lua's colon-call sugar implicitly passing obj as the first
		// argument) — both compound shapes share the same table/field-or
		// -method field names.
		name := call.ChildByFieldName("name")
		if name == nil {
			return c
		}
		switch name.Kind() {
		case "identifier":
			c.Callee = name.Utf8Text(src)
		case "dot_index_expression":
			if tbl := name.ChildByFieldName("table"); tbl != nil {
				c.Receiver = tbl.Utf8Text(src)
			}
			if fld := name.ChildByFieldName("field"); fld != nil {
				c.Callee = fld.Utf8Text(src)
			}
		case "method_index_expression":
			if tbl := name.ChildByFieldName("table"); tbl != nil {
				c.Receiver = tbl.Utf8Text(src)
			}
			if mth := name.ChildByFieldName("method"); mth != nil {
				c.Callee = mth.Utf8Text(src)
			}
		}
		return c
	}

	if language == "cpp" && call.Kind() == "new_expression" {
		// C++ `new UserRepo()` — the class name is the node's first named
		// child, the same shape as PHP's object_creation_expression.
		if cls := call.NamedChild(0); cls != nil {
			c.Callee = simpleTypeName(cls, src)
		}
		return c
	}

	if call.Kind() == "instance_expression" {
		// Scala `new UserRepo()` — node kind name is unique to Scala (no
		// collision risk), same shape as C++/PHP's construction syntax.
		if cls := call.NamedChild(0); cls != nil {
			c.Callee = simpleTypeName(cls, src)
			c.Constructor = true
		}
		return c
	}

	if language == "swift" && call.Kind() == "call_expression" {
		// Unlike Kotlin's call_expression (no field names at all), Swift's
		// has the function part as its first named child positionally, but
		// that child itself DOES expose named fields once it's a
		// navigation_expression: "target" (the receiver) and "suffix" (a
		// navigation_suffix node whose own "suffix" field is the actual
		// method/property identifier) — found by dumping the actual parse
		// tree for `recv.method()` rather than assuming the shape.
		fn := call.NamedChild(0)
		if fn == nil {
			return c
		}
		switch fn.Kind() {
		case "simple_identifier":
			c.Callee = fn.Utf8Text(src)
		case "navigation_expression":
			if target := fn.ChildByFieldName("target"); target != nil {
				c.Receiver = target.Utf8Text(src)
			}
			if suffix := fn.ChildByFieldName("suffix"); suffix != nil {
				if name := suffix.ChildByFieldName("suffix"); name != nil {
					c.Callee = name.Utf8Text(src)
				}
			}
		}
		return c
	}

	if language == "kotlin" && call.Kind() == "call_expression" {
		// Kotlin's grammar defines no field names at all on call_expression
		// (unlike every other language's call node) — the function part is
		// always its first named child, positionally: a bare identifier
		// (`helper()`) or a "navigation_expression" for `recv.method()`,
		// itself field-less with the receiver as its first named child and
		// the method name as its second.
		fn := call.NamedChild(0)
		if fn == nil {
			return c
		}
		switch fn.Kind() {
		case "identifier":
			c.Callee = fn.Utf8Text(src)
		case "navigation_expression":
			if recv := fn.NamedChild(0); recv != nil {
				c.Receiver = recv.Utf8Text(src)
			}
			if name := fn.NamedChild(1); name != nil {
				c.Callee = name.Utf8Text(src)
			}
		}
		return c
	}

	if language == "r" && call.Kind() == "call" {
		// R's "call" node has a "function" field that is one of: a bare
		// identifier (`helper(x)`), a "namespace_operator" (`pkg::fn(x)`, fields
		// lhs/rhs) or an "extract_operator" (`obj$method(x)`, fields lhs/rhs).
		// The bare case would resolve via the generic fallback, but the two
		// qualified shapes are R-specific node kinds it doesn't know.
		fn := call.ChildByFieldName("function")
		if fn == nil {
			return c
		}
		switch fn.Kind() {
		case "identifier":
			c.Callee = fn.Utf8Text(src)
		case "namespace_operator", "extract_operator":
			if lhs := fn.ChildByFieldName("lhs"); lhs != nil {
				c.Receiver = lhs.Utf8Text(src)
			}
			if rhs := fn.ChildByFieldName("rhs"); rhs != nil {
				c.Callee = rhs.Utf8Text(src)
			}
		}
		return c
	}

	if language == "julia" && call.Kind() == "call_expression" {
		// Julia's call_expression exposes no field names; the callee is the
		// first named child (a bare identifier, or a field_expression
		// `Mod.fn`). It is also the node the tags query matches for a function
		// signature, so a call_expression that IS a definition's signature
		// (parent "signature", long form) or the target of a short-form
		// definition (`f(x) = ...`, first child of an "assignment") must be
		// skipped or every def would emit a self-call to its own name.
		if p := call.Parent(); p != nil {
			if p.Kind() == "signature" {
				return c
			}
			if p.Kind() == "assignment" {
				if first := p.NamedChild(0); first != nil &&
					first.StartByte() == call.StartByte() && first.EndByte() == call.EndByte() {
					return c
				}
			}
		}
		fn := call.NamedChild(0)
		if fn == nil {
			return c
		}
		switch fn.Kind() {
		case "identifier":
			c.Callee = fn.Utf8Text(src)
		case "field_expression":
			if v := fn.ChildByFieldName("value"); v != nil {
				c.Receiver = v.Utf8Text(src)
			}
			if name := fn.NamedChild(1); name != nil {
				c.Callee = name.Utf8Text(src)
			}
		}
		return c
	}

	if language == "zig" && call.Kind() == "call_expression" {
		// Zig's call_expression has a "function" field: a bare identifier
		// (`bare()`) or a "field_expression" (`S.m()` / `obj.m()`, fields
		// object/member — distinct from the value/field fields the generic
		// field_expression case below assumes).
		fn := call.ChildByFieldName("function")
		if fn == nil {
			return c
		}
		switch fn.Kind() {
		case "identifier":
			c.Callee = fn.Utf8Text(src)
		case "field_expression":
			if o := fn.ChildByFieldName("object"); o != nil {
				c.Receiver = o.Utf8Text(src)
			}
			if m := fn.ChildByFieldName("member"); m != nil {
				c.Callee = m.Utf8Text(src)
			}
		}
		return c
	}

	if language == "ocaml" && call.Kind() == "application_expression" {
		// OCaml application is curried and right-... actually left-nested:
		// `f a b` is application{function: application{function: f, arg: a},
		// arg: b}. Only the outermost application is "the call"; skip an
		// application that is itself the "function" field of its parent
		// application (an intermediate partial application), mirroring Haskell.
		if p := call.Parent(); p != nil && p.Kind() == "application_expression" {
			if fn := p.ChildByFieldName("function"); fn != nil &&
				fn.StartByte() == call.StartByte() && fn.EndByte() == call.EndByte() {
				return c
			}
		}
		base := call
		for base.Kind() == "application_expression" {
			fn := base.ChildByFieldName("function")
			if fn == nil {
				return c
			}
			base = fn
		}
		if base.Kind() == "value_path" {
			// value_path is either `(value_name)` or `(module_path...) (value_name)`.
			bc := base.Walk()
			defer bc.Close()
			var modParts []string
			for _, child := range base.NamedChildren(bc) {
				switch child.Kind() {
				case "value_name":
					c.Callee = child.Utf8Text(src)
				case "module_path":
					modParts = append(modParts, child.Utf8Text(src))
				}
			}
			c.Receiver = strings.Join(modParts, ".")
		}
		return c
	}

	switch call.Kind() {
	case "command":
		// Bash has no syntactic distinction between "calling a function"
		// and "running an external command/builtin" — both are just a
		// "command" node with a "name" field wrapping a command_name(word).
		// No "receiver" concept exists in Bash at all.
		if name := call.ChildByFieldName("name"); name != nil {
			c.Callee = name.Utf8Text(src)
		}
		return c
	case "method_invocation":
		if name := call.ChildByFieldName("name"); name != nil {
			c.Callee = name.Utf8Text(src)
		}
		if obj := call.ChildByFieldName("object"); obj != nil {
			c.Receiver = obj.Utf8Text(src)
		}
		return c
	case "object_creation_expression":
		if typ := call.ChildByFieldName("type"); typ != nil {
			c.Callee = simpleTypeName(typ, src)
		}
		return c
	case "invocation_expression":
		// C# `obj.Method(...)` (function: member_access_expression, fields
		// `expression`+`name`) or bare `Method(...)` (function: identifier).
		fn := call.ChildByFieldName("function")
		if fn == nil {
			return c
		}
		switch fn.Kind() {
		case "member_access_expression":
			// "name" is a plain identifier for `obj.Method()`, but a
			// "generic_name" (identifier + type_argument_list) for a generic
			// method call like `list.OfType<Foo>()` — simpleTypeName already
			// unwraps "generic_name" to its bare identifier for type
			// positions, so reuse it here instead of name.Utf8Text(src),
			// which would otherwise pollute Callee with "OfType<Foo>" and
			// silently break every bare-name lookup for that method.
			if name := fn.ChildByFieldName("name"); name != nil {
				c.Callee = simpleTypeName(name, src)
			}
			if expr := fn.ChildByFieldName("expression"); expr != nil {
				c.Receiver = expr.Utf8Text(src)
			}
		case "conditional_access_expression":
			// C# `a?.b()` — the null-conditional operator. The receiver is
			// the node's first named child; the member name is nested one
			// level deeper inside a "member_binding_expression" with its own
			// "name" field, unlike the unconditional "member_access_expression"
			// case above which exposes "name" directly.
			if recv := fn.NamedChild(0); recv != nil {
				c.Receiver = recv.Utf8Text(src)
			}
			if binding := fn.NamedChild(1); binding != nil && binding.Kind() == "member_binding_expression" {
				if name := binding.ChildByFieldName("name"); name != nil {
					c.Callee = name.Utf8Text(src)
				}
			}
		default:
			c.Callee = fn.Utf8Text(src)
		}
		return c
	case "method_reference":
		// `Account::getName` / `System.out::println` -> two named children
		// (receiver expression, method name). `Foo::new` (constructor
		// reference) -> one named child (the type); treat like
		// object_creation_expression.
		cursor := call.Walk()
		defer cursor.Close()
		children := call.NamedChildren(cursor)
		switch len(children) {
		case 2:
			c.Receiver = children[0].Utf8Text(src)
			c.Callee = children[1].Utf8Text(src)
		case 1:
			c.Callee = simpleTypeName(&children[0], src)
		}
		return c
	}

	fn := call.ChildByFieldName("function")
	if fn == nil {
		return c
	}

	// A bare generic-typed call (`func::<T>()` in Rust, `foo<int>()` in
	// C++) wraps the actual callee expression in a "generic_function"/
	// "template_function" node alongside the type-argument list — unwrap to
	// the real expression (first named child) before dispatching below, or
	// every case in this switch sees an unrecognized wrapper kind and the
	// call is silently dropped (empty Receiver/Callee) instead of resolved.
	for fn.Kind() == "generic_function" || fn.Kind() == "template_function" {
		inner := fn.NamedChild(0)
		if inner == nil {
			break
		}
		fn = inner
	}

	switch fn.Kind() {
	case "identifier":
		c.Callee = fn.Utf8Text(src)
	case "attribute":
		if attr := fn.ChildByFieldName("attribute"); attr != nil {
			c.Callee = attr.Utf8Text(src)
		}
		if obj := fn.ChildByFieldName("object"); obj != nil {
			c.Receiver = obj.Utf8Text(src)
		}
	case "selector_expression":
		// Go `recv.Method(...)` / `pkg.Func(...)`.
		if field := fn.ChildByFieldName("field"); field != nil {
			c.Callee = field.Utf8Text(src)
		}
		if operand := fn.ChildByFieldName("operand"); operand != nil {
			c.Receiver = operand.Utf8Text(src)
		}
	case "field_expression":
		// Rust `recv.method(...)`, e.g. `self.repo.find_user(id)` ->
		// Receiver "self.repo", Callee "find_user" — fields "value"/"field".
		// C/C++ use the same node kind for `recv->method(...)` (calling a
		// function-pointer struct member) but expose "argument"/"field"
		// instead — must be disambiguated by language, not node kind alone.
		if language == "c" || language == "cpp" {
			if field := fn.ChildByFieldName("field"); field != nil {
				// A generic method call (`obj.method<int>()`) wraps the
				// name in a "template_method" node instead of a plain
				// field_identifier — unwrap it, or Callee ends up polluted
				// with "method<int>" and never matches the bare method name.
				if field.Kind() == "template_method" {
					if name := field.ChildByFieldName("name"); name != nil {
						field = name
					}
				}
				c.Callee = field.Utf8Text(src)
			}
			if arg := fn.ChildByFieldName("argument"); arg != nil {
				c.Receiver = arg.Utf8Text(src)
			}
			break
		}
		if field := fn.ChildByFieldName("field"); field != nil {
			c.Callee = field.Utf8Text(src)
		}
		if value := fn.ChildByFieldName("value"); value != nil {
			c.Receiver = value.Utf8Text(src)
		}
	case "scoped_identifier":
		// Rust `Type::method(...)` / `std::collections::HashMap::new(...)` ->
		// Receiver is everything before the last "::", Callee is the final
		// segment.
		if name := fn.ChildByFieldName("name"); name != nil {
			c.Callee = name.Utf8Text(src)
		}
		if path := fn.ChildByFieldName("path"); path != nil {
			c.Receiver = path.Utf8Text(src)
		}
	case "qualified_identifier":
		// C++ `UserRepo::staticMethod(...)` -> fields "scope"/"name" directly.
		// "scope" is a bare type_identifier normally, but a "template_type"
		// (now unwrapped by simpleTypeName) for a generic class's static
		// call, e.g. `Foo<int>::bar()`.
		if name := fn.ChildByFieldName("name"); name != nil {
			c.Callee = name.Utf8Text(src)
		}
		if scope := fn.ChildByFieldName("scope"); scope != nil {
			c.Receiver = simpleTypeName(scope, src)
		}
	}

	return c
}

// callInDecorator reports whether call is (part of) a decorator expression,
// e.g. the `app.route("/x")` in `@app.route("/x")`. It walks ancestors until
// it finds a "decorator" node, or a "module"/"block" node (a normal
// statement context, meaning the call is not inside a decorator).
func callInDecorator(call *sitter.Node) bool {
	for n := call.Parent(); n != nil; n = n.Parent() {
		switch n.Kind() {
		case "decorator":
			return true
		case "module", "block":
			return false
		}
	}
	return false
}

// importsFromJavaImport extracts the module spec from a Java
// `import_declaration`, e.g. `import java.util.List;` -> "java.util.List",
// `import static a.b.C.method;` -> "a.b.C.method", and
// `import java.util.*;` -> "java.util.*".
func importsFromJavaImport(stmt *sitter.Node, src []byte) []Import {
	cursor := stmt.Walk()
	defer cursor.Close()

	var spec string
	wildcard := false
	for _, c := range stmt.Children(cursor) {
		switch c.Kind() {
		case "scoped_identifier", "identifier":
			spec = c.Utf8Text(src)
		case "asterisk":
			wildcard = true
		}
	}
	if spec == "" {
		return nil
	}
	if wildcard {
		spec += ".*"
	}
	return []Import{{Spec: spec, Start: int(stmt.StartByte()), End: int(stmt.EndByte())}}
}

func importsFromImportStatement(stmt *sitter.Node, src []byte) []Import {
	cursor := stmt.Walk()
	defer cursor.Close()

	var out []Import
	for _, n := range stmt.ChildrenByFieldName("name", cursor) {
		out = append(out, importFromNameNode(&n, src, stmt))
	}
	return out
}

func importFromNameNode(n *sitter.Node, src []byte, stmt *sitter.Node) Import {
	imp := Import{Start: int(stmt.StartByte()), End: int(stmt.EndByte())}

	switch n.Kind() {
	case "aliased_import":
		if name := n.ChildByFieldName("name"); name != nil {
			imp.Spec = name.Utf8Text(src)
		}
		if alias := n.ChildByFieldName("alias"); alias != nil {
			imp.Alias = alias.Utf8Text(src)
		}
	case "dotted_name":
		imp.Spec = n.Utf8Text(src)
	}

	return imp
}

func importsFromFromStatement(stmt *sitter.Node, src []byte) []Import {
	moduleSpec := moduleSpecFromNode(stmt.ChildByFieldName("module_name"), src)

	cursor := stmt.Walk()
	defer cursor.Close()
	names := stmt.ChildrenByFieldName("name", cursor)

	if len(names) == 0 {
		// `from x import *` or a bare `from . import` with no names.
		return []Import{{Spec: moduleSpec, Start: int(stmt.StartByte()), End: int(stmt.EndByte())}}
	}

	out := make([]Import, 0, len(names))
	for _, n := range names {
		imp := Import{Spec: moduleSpec, Start: int(stmt.StartByte()), End: int(stmt.EndByte())}
		if n.Kind() == "aliased_import" {
			if name := n.ChildByFieldName("name"); name != nil {
				imp.Name = name.Utf8Text(src)
			}
			if alias := n.ChildByFieldName("alias"); alias != nil {
				imp.Alias = alias.Utf8Text(src)
			}
		} else {
			imp.Name = n.Utf8Text(src)
		}
		out = append(out, imp)
	}
	return out
}

// importsFromGoImport extracts one Import per `import_spec` from a Go
// `import_declaration`, e.g. `import "fmt"` or
// `import ( "fmt"; m "github.com/x/y/models" )` -> ["fmt", "github.com/x/y/models"
// (Alias "m")]. The surrounding quotes (or backticks for raw strings) are
// stripped from the import path.
func importsFromGoImport(stmt *sitter.Node, src []byte) []Import {
	cursor := stmt.Walk()
	defer cursor.Close()

	var specs []sitter.Node
	for _, c := range stmt.Children(cursor) {
		switch c.Kind() {
		case "import_spec":
			specs = append(specs, c)
		case "import_spec_list":
			lCursor := c.Walk()
			defer lCursor.Close()
			for _, s := range c.Children(lCursor) {
				if s.Kind() == "import_spec" {
					specs = append(specs, s)
				}
			}
		}
	}

	out := make([]Import, 0, len(specs))
	for _, s := range specs {
		pathNode := s.ChildByFieldName("path")
		if pathNode == nil {
			continue
		}
		imp := Import{
			Spec:  strings.Trim(pathNode.Utf8Text(src), "\"`"),
			Start: int(stmt.StartByte()),
			End:   int(stmt.EndByte()),
		}
		if name := s.ChildByFieldName("name"); name != nil {
			imp.Alias = name.Utf8Text(src)
		}
		out = append(out, imp)
	}
	return out
}

// importsFromCSharpUsing extracts the namespace spec from a C# `using_directive`,
// e.g. `using System;` / `using System.Collections.Generic;` -> Spec
// "System"/"System.Collections.Generic"; `using static System.Math;` ->
// Spec "System.Math" (the `static` keyword is ignored); and
// `using Models = MyApp.Domain.Models;` -> Spec "MyApp.Domain.Models", Alias
// "Models" (the aliased name is the using_directive's `name` field, the
// target namespace is its other identifier/qualified_name child). Returns
// nil if no namespace spec could be found.
func importsFromCSharpUsing(stmt *sitter.Node, src []byte) []Import {
	imp := Import{Start: int(stmt.StartByte()), End: int(stmt.EndByte())}
	nameField := stmt.ChildByFieldName("name")

	cursor := stmt.Walk()
	defer cursor.Close()
	for _, c := range stmt.NamedChildren(cursor) {
		switch c.Kind() {
		case "identifier", "qualified_name":
			if nameField != nil && c.StartByte() == nameField.StartByte() {
				imp.Alias = c.Utf8Text(src)
				continue
			}
			imp.Spec = c.Utf8Text(src)
		}
	}
	if imp.Spec == "" {
		return nil
	}
	return []Import{imp}
}

func moduleSpecFromNode(n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}

	switch n.Kind() {
	case "relative_import":
		var sb strings.Builder
		cursor := n.Walk()
		defer cursor.Close()
		for _, c := range n.Children(cursor) {
			switch c.Kind() {
			case "import_prefix", "dotted_name":
				sb.WriteString(c.Utf8Text(src))
			}
		}
		return sb.String()
	default:
		return n.Utf8Text(src)
	}
}

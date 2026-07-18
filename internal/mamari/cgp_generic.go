package mamari

import (
	"path/filepath"
	"strings"

	"github.com/waelhoury/mamari/internal/mamari/treesitter"
)

// languageProfile holds the small set of per-language behaviors the generic
// tree-sitter emitter needs beyond what treesitter.Parse already provides
// uniformly. New tree-sitter-backed languages typically need only a
// registry.go grammar/query entry plus an entry here (or none at all) — not
// a new hand-written emit function. See SESSION_HANDOFF/the language
// coverage plan for the rollout this is built for.
type languageProfile struct {
	// selfReceivers names receiver expressions that resolve like Java's
	// "this": the enclosing type's own scope (e.g. Rust's "self"/"Self").
	selfReceivers map[string]bool

	// builtinCallables names bare (no-receiver) callee names that never
	// resolve to a repo symbol, skipped instead of emitted as unresolved
	// noise (e.g. Rust's Some/None/Ok/Err enum-variant constructors).
	builtinCallables map[string]bool

	// globalReceivers names receiver roots that are known stdlib/builtin
	// types, skipped instead of emitted as unresolved noise (e.g. Rust's
	// String/Vec/HashMap).
	globalReceivers map[string]bool

	// superReceivers names receiver expressions that resolve like Java's
	// "super": the enclosing type's base class (e.g. PHP's "parent").
	superReceivers map[string]bool
}

var rustBuiltinCallables = map[string]bool{
	"Some": true, "None": true, "Ok": true, "Err": true,
}

var rustGlobalReceivers = map[string]bool{
	"String": true, "Vec": true, "Box": true, "Option": true, "Result": true,
	"HashMap": true, "HashSet": true, "BTreeMap": true, "BTreeSet": true,
	"Default": true, "Arc": true, "Rc": true, "RefCell": true, "Mutex": true,
}

// rubyBuiltinCallables names bare (no-receiver) Ruby calls that never
// resolve to a repo symbol: require/require_relative are also captured as
// plain calls (Ruby has no separate import-statement node kind, so the
// import-detecting query pattern and the generic call pattern both match
// the same node) and are skipped here rather than emitted as a redundant
// unresolved call edge alongside the real import edge; the rest are
// kernel/class-body DSL methods extremely common in idiomatic Ruby.
var rubyBuiltinCallables = map[string]bool{
	"require": true, "require_relative": true, "puts": true, "print": true,
	"p": true, "pp": true, "raise": true, "loop": true, "lambda": true,
	"proc": true, "attr_accessor": true, "attr_reader": true, "attr_writer": true,
	"include": true, "extend": true, "module_function": true,
	"private": true, "public": true, "protected": true, "freeze": true,
}

var rubyGlobalReceivers = map[string]bool{
	"File": true, "Dir": true, "ENV": true, "Time": true, "Process": true,
	"Math": true, "JSON": true, "Kernel": true, "Object": true, "Array": true,
	"Hash": true, "String": true, "Integer": true, "Float": true,
	"Comparable": true, "Enumerable": true, "Struct": true,
}

// phpBuiltinCallables names bare PHP stdlib/language functions extremely
// common in idiomatic code, skipped instead of emitted as unresolved noise.
var phpBuiltinCallables = map[string]bool{
	"var_dump": true, "print_r": true, "array_map": true, "array_filter": true,
	"array_merge": true, "array_keys": true, "array_values": true, "count": true,
	"strlen": true, "sprintf": true, "printf": true, "isset": true, "empty": true,
	"is_null": true, "is_array": true, "is_string": true, "is_int": true,
	"in_array": true, "implode": true, "explode": true, "trim": true,
	"str_replace": true, "json_encode": true, "json_decode": true, "define": true,
	"function_exists": true, "class_exists": true, "method_exists": true,
	"array_push": true, "array_pop": true, "array_shift": true, "compact": true,
}

// cBuiltinCallables names bare libc/standard-header functions extremely
// common in idiomatic C, skipped instead of emitted as unresolved noise. C
// has no module/namespace system to scope these to, so the list is purely
// by name (the same risk every bare-name skip-list in this file already
// accepts: a repo-defined function that happens to share one of these names
// would also be skipped — acceptable, since these names are reserved/
// near-universally stdlib in practice).
var cBuiltinCallables = map[string]bool{
	"printf": true, "fprintf": true, "sprintf": true, "snprintf": true,
	"malloc": true, "calloc": true, "realloc": true, "free": true,
	"memcpy": true, "memmove": true, "memset": true, "memcmp": true,
	"strlen": true, "strcpy": true, "strncpy": true, "strcmp": true,
	"strncmp": true, "strcat": true, "strstr": true, "strtok": true,
	"fopen": true, "fclose": true, "fread": true, "fwrite": true,
	"fgets": true, "fputs": true, "fseek": true, "ftell": true,
	"exit": true, "abort": true, "assert": true, "atoi": true, "atof": true,
	"qsort": true, "abs": true,
}

var languageProfiles = map[string]languageProfile{
	"rust": {
		selfReceivers:    map[string]bool{"self": true, "Self": true},
		builtinCallables: rustBuiltinCallables,
		globalReceivers:  rustGlobalReceivers,
	},
	"ruby": {
		selfReceivers:    map[string]bool{"self": true},
		builtinCallables: rubyBuiltinCallables,
		globalReceivers:  rubyGlobalReceivers,
	},
	"c": {
		builtinCallables: cBuiltinCallables,
	},
	"php": {
		selfReceivers:    map[string]bool{"self": true, "static": true, "$this": true},
		superReceivers:   map[string]bool{"parent": true},
		builtinCallables: phpBuiltinCallables,
	},
	"cpp": {
		selfReceivers:    map[string]bool{"this": true},
		builtinCallables: cBuiltinCallables,
	},
	"kotlin": {
		selfReceivers:    map[string]bool{"this": true},
		superReceivers:   map[string]bool{"super": true},
		builtinCallables: kotlinBuiltinCallables,
	},
	"bash": {
		builtinCallables: bashBuiltinCallables,
	},
	"scala": {
		selfReceivers:    map[string]bool{"this": true},
		superReceivers:   map[string]bool{"super": true},
		builtinCallables: scalaBuiltinCallables,
	},
	"lua": {
		selfReceivers:    map[string]bool{"self": true},
		builtinCallables: luaBuiltinCallables,
	},
	"elixir": {
		builtinCallables: elixirBuiltinCallables,
	},
	"dart": {
		selfReceivers:  map[string]bool{"this": true},
		superReceivers: map[string]bool{"super": true},
	},
	"haskell": {
		builtinCallables: haskellBuiltinCallables,
	},
	"clojure": {
		builtinCallables: clojureBuiltinCallables,
	},
	"r": {
		builtinCallables: rBuiltinCallables,
	},
	"julia": {
		builtinCallables: juliaBuiltinCallables,
	},
	"zig": {
		selfReceivers:    map[string]bool{"self": true, "Self": true},
		builtinCallables: zigBuiltinCallables,
	},
	"ocaml": {
		builtinCallables: ocamlBuiltinCallables,
	},
	"hcl": {},
}

// rBuiltinCallables names bare R functions extremely common in idiomatic code
// (base/stats primitives, and library/require which — like Lua's require —
// load packages via a path R resolves at runtime, not a followable in-repo
// target), skipped instead of emitted as unresolved noise.
var rBuiltinCallables = map[string]bool{
	"library": true, "require": true, "c": true, "list": true, "print": true,
	"cat": true, "paste": true, "paste0": true, "length": true, "nrow": true,
	"ncol": true, "return": true, "invisible": true, "stop": true, "warning": true,
	"is.null": true, "is.na": true, "as.character": true, "as.numeric": true,
	"sapply": true, "lapply": true, "vapply": true, "mapply": true, "apply": true,
	"seq_len": true, "seq_along": true, "vector": true, "names": true, "unlist": true,
}

// juliaBuiltinCallables names bare Julia Base functions extremely common in
// idiomatic code, skipped instead of emitted as unresolved noise.
var juliaBuiltinCallables = map[string]bool{
	"println": true, "print": true, "length": true, "push!": true, "pop!": true,
	"map": true, "filter": true, "reduce": true, "collect": true, "sum": true,
	"error": true, "throw": true, "typeof": true, "isa": true, "convert": true,
	"getindex": true, "setindex!": true, "haskey": true, "get": true, "zeros": true,
	"ones": true, "size": true, "reshape": true, "string": true, "parse": true,
}

// zigBuiltinCallables names bare Zig std helpers extremely common in idiomatic
// code, skipped instead of emitted as unresolved noise. Zig's builtins
// (@import/@sizeOf/...) parse as builtin_function nodes, not call_expression,
// so they never reach the call resolver and need no entry here.
var zigBuiltinCallables = map[string]bool{
	"try": true, "assert": true, "print": true, "panic": true, "expect": true,
	"alloc": true, "free": true, "create": true, "destroy": true, "init": true,
	"deinit": true, "append": true, "allocPrint": true,
}

// ocamlBuiltinCallables names bare OCaml Stdlib/Pervasives functions extremely
// common in idiomatic code, skipped instead of emitted as unresolved noise.
var ocamlBuiltinCallables = map[string]bool{
	"print_string": true, "print_endline": true, "print_int": true, "printf": true,
	"sprintf": true, "failwith": true, "raise": true, "ignore": true, "ref": true,
	"incr": true, "decr": true, "fst": true, "snd": true, "not": true, "compare": true,
	"succ": true, "pred": true, "min": true, "max": true, "abs": true,
}

// clojureBuiltinCallables names bare clojure.core functions extremely
// common in idiomatic code, skipped instead of emitted as unresolved noise.
var clojureBuiltinCallables = map[string]bool{
	"println": true, "print": true, "str": true, "map": true, "filter": true,
	"reduce": true, "into": true, "conj": true, "assoc": true, "dissoc": true,
	"get": true, "get-in": true, "merge": true, "first": true, "rest": true,
	"last": true, "count": true, "empty?": true, "nil?": true, "some": true,
	"every?": true, "apply": true, "vec": true, "vector": true, "list": true,
	"hash-map": true, "keyword": true, "name": true, "symbol": true,
	"throw": true, "ex-info": true, "atom": true, "swap!": true, "reset!": true,
	"deref": true, "partial": true, "comp": true, "identity": true,
	"format": true, "concat": true, "remove": true, "sort": true, "sort-by": true,
}

// haskellBuiltinCallables names bare Prelude/base functions extremely
// common in idiomatic Haskell, skipped instead of emitted as unresolved
// noise.
var haskellBuiltinCallables = map[string]bool{
	"map": true, "filter": true, "foldr": true, "foldl": true, "concat": true,
	"concatMap": true, "length": true, "reverse": true, "show": true,
	"print": true, "putStrLn": true, "putStr": true, "error": true,
	"otherwise": true, "id": true, "const": true, "fst": true, "snd": true,
	"fmap": true, "mapM_": true, "mapM": true, "sequence": true, "return": true,
	"pure": true, "elem": true, "null": true, "head": true, "tail": true,
	"take": true, "drop": true, "zip": true, "zipWith": true, "lookup": true,
	"maybe": true, "either": true, "not": true, "compare": true,
}

// elixirBuiltinCallables names bare functions/macros extremely common in
// idiomatic Elixir, skipped instead of emitted as unresolved noise.
// "import"/"alias"/"require"/"use" specifically: like def/defmodule, these
// have no dedicated grammar node — they parse as ordinary calls — but
// unlike def/defmodule they are genuinely not worth modeling as real import
// edges, since mamari has no static module-path resolution for Elixir's
// `use`-based metaprogramming (a `use` invocation can inject arbitrary
// code via `__using__/1`, not a fixed, followable target).
var elixirBuiltinCallables = map[string]bool{
	"import": true, "alias": true, "require": true, "use": true,
	"raise": true, "throw": true, "IO": true, "inspect": true,
	"is_nil": true, "is_atom": true, "is_binary": true, "is_list": true,
	"is_map": true, "is_function": true, "is_integer": true,
}

// luaBuiltinCallables names bare Lua stdlib functions extremely common in
// idiomatic code, skipped instead of emitted as unresolved noise.
// "require" specifically: Lua has no static module-path resolution mamari
// can follow (require("foo.bar") maps to a file via package.path at
// runtime), the same reasoning that keeps Bash's "source" in its own
// builtin list rather than modeled as a real import edge.
var luaBuiltinCallables = map[string]bool{
	"require": true, "print": true, "pairs": true, "ipairs": true,
	"type": true, "tostring": true, "tonumber": true, "pcall": true,
	"xpcall": true, "error": true, "assert": true, "setmetatable": true,
	"getmetatable": true, "rawget": true, "rawset": true, "select": true,
	"unpack": true,
}

// scalaBuiltinCallables names bare Scala stdlib functions/case-object
// constructors extremely common in idiomatic code, skipped instead of
// emitted as unresolved noise.
var scalaBuiltinCallables = map[string]bool{
	"println": true, "print": true, "require": true, "assert": true,
	"Some": true, "None": true, "Left": true, "Right": true, "Try": true,
}

// bashBuiltinCallables names Bash builtins and extremely common external
// utilities, skipped instead of emitted as unresolved noise. Bash has no
// module/namespace system at all and no syntactic distinction between
// "calling a function" and "running a command" (see callFromNode's
// "command" case) — every invocation in a real shell script is one of:
// a function defined in the repo (resolved normally), a builtin/coreutil
// (skipped here), or some other external binary (honestly unresolved,
// the same treatment any language gives a third-party library call).
// "source"/"." are included because the import-detecting query pattern and
// the generic call pattern both match the same node (see Ruby's
// require/require_relative for the identical reason), so without this the
// real import edge would be accompanied by a redundant unresolved call edge.
var bashBuiltinCallables = map[string]bool{
	"source": true, ".": true, "cd": true, "ls": true, "echo": true, "pwd": true,
	"mkdir": true, "rm": true, "cp": true, "mv": true, "cat": true, "grep": true,
	"sed": true, "awk": true, "find": true, "chmod": true, "chown": true,
	"export": true, "unset": true, "read": true, "printf": true, "test": true,
	"true": true, "false": true, "exit": true, "return": true, "set": true,
	"shift": true, "trap": true, "wait": true, "kill": true, "eval": true,
	"exec": true, "declare": true, "local": true, "readonly": true,
	"alias": true, "type": true, "which": true, "command": true, "builtin": true,
	"let": true, "umask": true, "getopts": true, "[": true, "[[": true,
}

// kotlinBuiltinCallables names bare Kotlin stdlib functions extremely common
// in idiomatic code, skipped instead of emitted as unresolved noise.
var kotlinBuiltinCallables = map[string]bool{
	"println": true, "print": true, "listOf": true, "mapOf": true,
	"setOf": true, "arrayOf": true, "mutableListOf": true, "mutableMapOf": true,
	"require": true, "requireNotNull": true, "check": true, "checkNotNull": true,
	"TODO": true, "error": true, "lazy": true,
}

// emitGenericSymbolsTS emits CGPSymbols for any tree-sitter-backed language
// registered in treesitter's registry, using the same two-pass
// non-method/then-method pattern proven by emitGoSymbolsTS: non-method defs
// are emitted first (recording type symbol IDs by name), then methods,
// parented to the same-file type symbol named by ReceiverType when one is
// found (falling back to parentID, the file symbol, otherwise). This
// function does not change when a new language is added — see
// languageProfiles.
func emitGenericSymbolsTS(idx *Index, language, file, content, parentID string) {
	res, err := treesitter.Parse(language, []byte(content))
	if err != nil {
		idx.markFileParseDiagnostics(file, []ScanDiagnostic{{Code: "treesitter_error", Message: err.Error()}})
		return
	}
	if !res.ParseOK {
		idx.markFileParseDiagnostics(file, []ScanDiagnostic{{Code: "parse_error", Message: res.ParseError}})
	}

	starts := lineStarts(content)
	lines := strings.Split(content, "\n")
	typeIDByName := map[string]string{}
	typeIDByStart := map[int]string{}
	var ranges []varScopeRange

	var methodDefs []treesitter.Def
	for _, def := range res.Defs {
		if def.Kind == "method" {
			methodDefs = append(methodDefs, def)
			continue
		}

		startLine, startCol := offsetToLineCol(starts, def.Start)
		endLine, endCol := offsetToLineCol(starts, clampOffset(content, def.End))

		added := idx.AddCGPSymbol(CGPSymbol{
			ID:          stableSymbolID(language, def.Kind, file, def.QualifiedName, idx),
			Name:        def.Name,
			Kind:        def.Kind,
			Language:    language,
			File:        file,
			StartLine:   startLine,
			StartColumn: startCol,
			EndLine:     endLine,
			EndColumn:   endCol,
			Signature:   collapseJavaSignature(content, def.Start, def.End),
			Docstring:   extractSymbolDocstring(lines, startLine, language),
			Exported:    def.Exported,
			ParentID:    parentID,
			Confidence:  ConfExact,
		})

		if def.Kind == "class" || def.Kind == "interface" {
			typeIDByName[def.Name] = added.ID
			typeIDByStart[def.Start] = added.ID
		}
		if def.Kind == "class" && len(def.Bases) > 0 {
			idx.mu.Lock()
			if idx.classBases == nil {
				idx.classBases = map[string][]string{}
			}
			idx.classBases[added.ID] = def.Bases
			idx.mu.Unlock()
		}
		if def.Kind == "function" || def.Kind == "class" || def.Kind == "interface" {
			ranges = append(ranges, varScopeRange{start: def.Start, end: def.End, id: added.ID})
		}
	}

	for _, def := range methodDefs {
		startLine, startCol := offsetToLineCol(starts, def.Start)
		endLine, endCol := offsetToLineCol(starts, clampOffset(content, def.End))

		// ParentName covers both shapes Def can express containment in:
		// lexical nesting (Ruby/Python-style — set directly by extractDefs'
		// stack, ParentStart is the parent's real byte offset) and
		// non-lexical receiver association (Rust impl blocks — extractDefs
		// redirects ParentName to ReceiverType and sets ParentStart = -1 for
		// these). The lexical case must be resolved by exact offset
		// (typeIDByStart), not by name: Scala's companion-object pattern
		// (`class Foo { ... }` and `object Foo { ... }` sharing one name in
		// the same file, a core, extremely common Scala idiom) means
		// typeIDByName can hold two different containers under the same key,
		// and a name-only lookup would silently parent a method to whichever
		// one was emitted last — found via exactly that false attribution
		// on a companion-object fixture, not assumed. Only the non-lexical
		// case (ParentStart == -1) needs the name-based fallback, since it
		// has no real position to look up and Rust's repeated same-type impl
		// blocks are deliberately meant to collapse onto one class anyway.
		parent := parentID
		redirectMissed := false
		if def.ParentStart != -1 {
			if pid, ok := typeIDByStart[def.ParentStart]; ok {
				parent = pid
			}
		} else if pid, ok := typeIDByName[def.ParentName]; ok {
			parent = pid
		} else if def.ParentName != "" {
			// A redirect was requested (def.ParentName came from
			// ReceiverType, not lexical nesting — see the comment above)
			// but no class with that name exists in *this* file. That does
			// not mean no such class exists: BuildIndex's symbol-extraction
			// phase runs every file in parallel with no defined order, so
			// the declaring file (e.g. a C++ out-of-line method's class,
			// usually declared in a separate header) may simply not have
			// been scanned yet. Recorded for resolveOutOfLineMethodParents
			// to retry globally once every file is done, rather than
			// leaving this permanently mis-parented to the file.
			redirectMissed = true
		}

		receiverType := ""
		if language == "kotlin" {
			receiverType = def.ReceiverType
		}
		added := idx.AddCGPSymbol(CGPSymbol{
			ID:           stableSymbolID(language, "method", file, def.QualifiedName, idx),
			Name:         def.Name,
			Kind:         "method",
			Language:     language,
			File:         file,
			StartLine:    startLine,
			StartColumn:  startCol,
			EndLine:      endLine,
			EndColumn:    endCol,
			Signature:    collapseJavaSignature(content, def.Start, def.End),
			Docstring:    extractSymbolDocstring(lines, startLine, language),
			ReceiverType: receiverType,
			Exported:     def.Exported,
			ParentID:     parent,
			Confidence:   ConfExact,
		})

		if receiverType != "" {
			idx.mu.Lock()
			registerExtensionMethodLocked(idx, language, receiverType, added.Name, added.ID)
			idx.mu.Unlock()
		}

		if redirectMissed {
			idx.mu.Lock()
			if idx.unresolvedMethodParents == nil {
				idx.unresolvedMethodParents = map[string]string{}
			}
			idx.unresolvedMethodParents[added.ID] = def.ParentName
			idx.mu.Unlock()
		}

		ranges = append(ranges, varScopeRange{start: def.Start, end: def.End, id: added.ID})

		if language == "lua" && def.ReceiverType != "" {
			idx.mu.Lock()
			if idx.luaReceiverTypeBySymbol == nil {
				idx.luaReceiverTypeBySymbol = map[string]string{}
			}
			idx.luaReceiverTypeBySymbol[added.ID] = def.ReceiverType
			if idx.luaMethodsByReceiverType == nil {
				idx.luaMethodsByReceiverType = map[string]map[string]string{}
			}
			if idx.luaMethodsByReceiverType[def.ReceiverType] == nil {
				idx.luaMethodsByReceiverType[def.ReceiverType] = map[string]string{}
			}
			idx.luaMethodsByReceiverType[def.ReceiverType][def.Name] = added.ID
			idx.mu.Unlock()
		}
	}

	switch language {
	case "ruby":
		// `@repo = Repo.new` belongs to the enclosing class, not only to
		// the method that set it (typically `initialize`), so calls
		// through `@repo` in any sibling method can use the type — the
		// same promotion rule Python's "self." needs, generalized via
		// populateSelfAttributeVarTypes since Ruby has no field
		// declarations at all to fall back on.
		populateSelfAttributeVarTypes(idx, res.Vars, ranges, "@")
	case "lua":
		// Lua has no class symbol to promote a "self.attr" binding to at
		// all (see populateLuaSelfAttributeVarTypes) — every method is
		// parented to its file, so the usual class-walk promotion used for
		// every other language with this rule doesn't apply here.
		populateLuaSelfAttributeVarTypes(idx, file, res.Vars, ranges)
	case "php":
		// `$this->repo = new Repo();` — PHP's untyped-property convention,
		// still extremely common, has no type hint at all to fall back on
		// (the plain field_declaration path only has evidence when one is
		// present). Same promotion rule Python's "self." needs.
		// Resolve `use ... as ...` aliases first so the promoted field type
		// is the imported class, not an unrelated class whose declaration
		// happens to share the local alias name.
		normalizeImportedVarTypes(res.Vars, res.Imports)
		populateSelfAttributeVarTypes(idx, res.Vars, ranges, "$this->")
	default:
		normalizeImportedVarTypes(res.Vars, res.Imports)
		populateVarTypes(idx, res.Vars, ranges)
	}
}

// emitGenericRelationsTS emits import and call edges for any tree-sitter
// language registered in languageProfiles, mirroring emitJavaRelationsTS:
// receiver-less and self-receiver calls resolve via resolveScopedCall,
// named-receiver calls try resolveVarCall (declared-type resolution) before
// falling back to resolveSymbolCall's repo-wide name search. Builtin
// callables/global receivers (per languageProfile) are skipped rather than
// emitted as unresolved noise, the same treatment Java gives
// java.lang/java.util usage.
func emitGenericRelationsTS(idx *Index, language, file, content string) {
	res, err := treesitter.Parse(language, []byte(content))
	if err != nil {
		idx.markFileParseDiagnostics(file, []ScanDiagnostic{{Code: "treesitter_error", Message: err.Error()}})
		return
	}
	starts := lineStarts(content)
	profile := languageProfiles[language]

	for _, imp := range res.Imports {
		if imp.Spec == "" {
			continue
		}
		line, col := offsetToLineCol(starts, imp.Start)
		idx.AddCGPEdge(
			fileSymbolID(file),
			"module:"+imp.Spec,
			"imports",
			ConfExact,
			Location{File: file, StartLine: line, StartColumn: col, EndLine: line, EndColumn: col + len(imp.Spec), Kind: "import", Raw: imp.Spec},
		)
	}

	idx.ensureFileSymbolIndex()
	for _, call := range res.Calls {
		if call.Callee == "" {
			continue
		}
		if call.Receiver == "" && profile.builtinCallables[call.Callee] {
			continue
		}
		if call.Receiver != "" && profile.globalReceivers[receiverRoot(call.Receiver)] {
			continue
		}
		callLine, callCol := offsetToLineCol(starts, call.Start)

		from := idx.containingSymbolFast(file, callLine)
		fromID := from.ID
		if fromID == "" {
			fromID = fileSymbolID(file)
		}

		var target, confidence, reason string
		if call.Constructor {
			target = findConstructibleClassByName(idx, call.Callee, file, language)
			if target == "" {
				continue
			}
			confidence = ConfScoped
		} else if importedTarget, importedConfidence, ok := resolveGenericImportedCall(idx, file, language, call, res.Imports); ok {
			target, confidence = importedTarget, importedConfidence
		} else {
			switch {
			case language == "ruby" && call.Receiver == "" && call.Callee == "super":
				// Ruby's `super`/`super(args)` has no explicit method name (unlike
				// Java's `super.foo()` or Python's `super().foo()`): it implicitly
				// calls the *same-named* method on the base class as the one it's
				// written inside. The generic bare-call path below would instead
				// search for a method literally named "super" (which never
				// exists), always landing on unresolved/missing_import — found via
				// exactly that symptom on a synthetic fixture, not assumed.
				target, confidence, reason = resolveSuperCall(idx, file, from, from.Name)
			case call.Receiver == "" || profile.selfReceivers[call.Receiver]:
				target, confidence, reason = resolveScopedCall(idx, file, from, call.Callee)
			case profile.superReceivers[call.Receiver]:
				target, confidence, reason = resolveSuperCall(idx, file, from, call.Callee)
			case language == "ruby" && call.Callee == "new" && call.Receiver != "":
				// Ruby has no `new` keyword — `ClassName.new(...)` is just a
				// regular method call where the literal callee text happens to
				// be "new" (a Kernel-provided method, never user-defined), so
				// the generic bare-name search below would never find a target
				// and always report unresolved. Every other language handled by
				// this engine has explicit `new`/equivalent construction syntax
				// whose callee is the class name itself (see PHP/Rust/C++'s
				// object_creation_expression-shaped handling in callFromNode),
				// so this is a Ruby-specific gap, not a generic one: resolve as
				// if the call were to the receiver's bare name directly. Found
				// via a dead_code false positive — a class only ever
				// constructed via `.new` looked completely unreferenced.
				target, confidence, reason = resolveSymbolCall(idx, file, call.Receiver)
			default:
				if t, c, ok := resolveVarCall(idx, from, call, language); ok {
					target, confidence = t, c
				} else {
					target, confidence, reason = resolveSymbolCall(idx, file, call.Receiver+"."+call.Callee)
				}
			}
		}

		raw := call.Callee
		if call.Receiver != "" {
			raw = call.Receiver + "." + call.Callee
		}
		idx.AddCGPEdgeWithReason(
			fromID, target, "calls", confidence, reason,
			Location{File: file, StartLine: callLine, StartColumn: callCol, EndLine: callLine, EndColumn: callCol + len(raw), Kind: "call", Raw: raw},
		)
	}
}

func normalizeImportedVarTypes(vars []treesitter.VarDecl, imports []treesitter.Import) {
	bindings := genericImportBindings(imports)
	for i := range vars {
		if canonical := bindings[vars[i].Type]; canonical != "" {
			vars[i].Type = importSimpleName(canonical)
		}
	}
}

func genericImportBindings(imports []treesitter.Import) map[string]string {
	out := map[string]string{}
	for _, imp := range imports {
		if imp.Spec == "" {
			continue
		}
		if imp.Alias != "" {
			out[imp.Alias] = imp.Spec
		}
		if imp.Name != "" {
			name := imp.Name
			if imp.Alias != "" {
				name = imp.Alias
			}
			out[name] = imp.Spec
		}
		simple := importSimpleName(imp.Spec)
		if simple != "" && out[simple] == "" {
			out[simple] = imp.Spec
		}
		// Qualified Haskell calls may use the full imported module name
		// when no `as` alias is present.
		out[imp.Spec] = imp.Spec
	}
	return out
}

func importSimpleName(spec string) string {
	spec = strings.TrimSpace(strings.Trim(spec, "\"'"))
	spec = strings.TrimSuffix(spec, "::*")
	spec = strings.ReplaceAll(spec, "\\", "/")
	spec = strings.ReplaceAll(spec, "::", "/")
	spec = strings.ReplaceAll(spec, ".", "/")
	spec = strings.Trim(spec, "/")
	if slash := strings.LastIndexByte(spec, '/'); slash >= 0 {
		spec = spec[slash+1:]
	}
	return strings.TrimSuffix(spec, filepath.Ext(spec))
}

func resolveGenericImportedCall(idx *Index, file, language string, call treesitter.Call, imports []treesitter.Import) (string, string, bool) {
	bindings := genericImportBindings(imports)
	key := call.Receiver
	if key == "" {
		key = call.Callee
	}
	spec := bindings[key]
	if spec == "" && call.Receiver != "" {
		spec = bindings[receiverRoot(call.Receiver)]
	}
	if spec == "" {
		return "", "", false
	}

	idx.ensureFileSymbolIndex()
	idx.mu.Lock()
	defer idx.mu.Unlock()

	ownerName := importSimpleName(spec)
	if call.Receiver == "" {
		var matches []string
		for _, sym := range idx.symbolsByName[ownerName] {
			if languageFamily(sym.Language) != languageFamily(language) {
				continue
			}
			switch sym.Kind {
			case "class", "interface", "function":
				matches = append(matches, sym.ID)
			}
		}
		if len(matches) == 1 {
			return matches[0], ConfScoped, true
		}
		return "", "", false
	}

	var matches []string
	seen := map[string]bool{}
	for _, owner := range idx.Symbols {
		if languageFamily(owner.Language) != languageFamily(language) || owner.Kind != "class" && owner.Kind != "interface" {
			continue
		}
		if owner.Name != ownerName && owner.Name != spec && !strings.HasSuffix(owner.Name, "."+ownerName) {
			continue
		}
		if id := findMethodInClassLocked(idx, owner.ID, call.Callee, map[string]bool{}); id != "" && !seen[id] {
			seen[id] = true
			matches = append(matches, id)
		}
	}
	if len(matches) == 1 {
		return matches[0], ConfScoped, true
	}

	// Module-oriented languages do not necessarily have a class/container
	// symbol. Restrict same-named functions or table methods to the imported
	// module's conventional file path instead.
	for _, sym := range idx.symbolsByName[call.Callee] {
		if languageFamily(sym.Language) != languageFamily(language) || !importSpecMatchesFile(spec, sym.File) || seen[sym.ID] {
			continue
		}
		seen[sym.ID] = true
		matches = append(matches, sym.ID)
	}
	if len(matches) == 1 {
		return matches[0], ConfScoped, true
	}
	return "", "", false
}

func importSpecMatchesFile(spec, file string) bool {
	spec = strings.ToLower(strings.TrimSpace(strings.Trim(spec, "\"'")))
	file = strings.ToLower(filepath.ToSlash(file))
	spec = strings.ReplaceAll(spec, "\\", "/")
	spec = strings.ReplaceAll(spec, "::", "/")
	spec = strings.ReplaceAll(spec, ".", "/")
	spec = strings.Trim(spec, "/")
	if spec == "" {
		return false
	}
	withoutExt := strings.TrimSuffix(file, filepath.Ext(file))
	return withoutExt == spec || strings.HasSuffix(withoutExt, "/"+spec) ||
		strings.TrimSuffix(filepath.Base(file), filepath.Ext(file)) == filepath.Base(spec)
}

// findConstructibleClassByName is stricter than findClassByName: constructor
// syntax can target only a concrete class, never an interface. Scala needs
// one additional distinction because `class Foo` and its companion
// `object Foo` are both represented as class-kind containers; the signature
// preserves their declaration keyword, so `new Foo` can select the class
// without changing how ordinary `Foo.method()` ambiguity is handled.
func findConstructibleClassByName(idx *Index, name, fromFile, language string) string {
	idx.ensureFileSymbolIndex()
	idx.mu.Lock()
	defer idx.mu.Unlock()

	fromDir := filepath.ToSlash(filepath.Dir(fromFile))
	var found, sameDirFound string
	matches, sameDirMatches := 0, 0
	for _, sym := range idx.symbolsByName[name] {
		if sym.Language != language || sym.Kind != "class" {
			continue
		}
		if language == "scala" && strings.HasPrefix(strings.TrimSpace(sym.Signature), "object ") {
			continue
		}
		found = sym.ID
		matches++
		if filepath.ToSlash(filepath.Dir(sym.File)) == fromDir {
			sameDirFound = sym.ID
			sameDirMatches++
		}
	}
	if sameDirMatches == 1 {
		return sameDirFound
	}
	if matches == 1 {
		return found
	}
	return ""
}

func extensionMethodKey(language, receiverType string) string {
	return language + "\x00" + receiverType
}

func registerExtensionMethodLocked(idx *Index, language, receiverType, method, id string) {
	if receiverType == "" || method == "" || id == "" {
		return
	}
	if idx.extensionMethods == nil {
		idx.extensionMethods = map[string]map[string][]string{}
	}
	key := extensionMethodKey(language, receiverType)
	if idx.extensionMethods[key] == nil {
		idx.extensionMethods[key] = map[string][]string{}
	}
	for _, existing := range idx.extensionMethods[key][method] {
		if existing == id {
			return
		}
	}
	idx.extensionMethods[key][method] = append(idx.extensionMethods[key][method], id)
}

func findExtensionMethod(idx *Index, language, receiverType, method string) string {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	ids := idx.extensionMethods[extensionMethodKey(language, receiverType)][method]
	found := ""
	for _, id := range ids {
		if _, ok := idx.Symbols[id]; !ok {
			continue
		}
		if found != "" && found != id {
			return ""
		}
		found = id
	}
	return found
}

// receiverRoot returns the first dotted segment of a call receiver
// expression, e.g. "self.repo" -> "self", "HashMap" -> "HashMap".
func receiverRoot(receiver string) string {
	if dot := strings.IndexByte(receiver, '.'); dot >= 0 {
		return receiver[:dot]
	}
	return receiver
}

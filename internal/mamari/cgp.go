package mamari

import (
	"encoding/json"
	"fmt"
	"html"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/waelhoury/mamari/internal/mamari/treesitter"
)

var (
	vueEventBindingRe      = regexp.MustCompile(`(?is)(?:^|[\s<])((?:@|v-on:)[A-Za-z0-9_$:.-]+)\s*(?:=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'<>]+)))?`)
	vueExpressionBindingRe = regexp.MustCompile(`(?is)(?:^|[\s<])((?::[A-Za-z0-9_$:.-]+)|(?:v-bind(?::[A-Za-z0-9_$:.-]+)?)|(?:v-model(?::[A-Za-z0-9_$:.-]+)?))\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'<>]+))`)
	// Structural directives carry full expressions too. Before this pattern
	// existed, a method used only inside v-if/v-show/v-for never got a
	// template edge and could be reported dead while guarding live UI.
	vueStructuralDirectiveRe = regexp.MustCompile(`(?is)(?:^|[\s<])(v-(?:if|else-if|show|for|html|text|memo))\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'<>]+))`)
	vueInterpolationRe       = regexp.MustCompile(`(?is)\{\{(.*?)\}\}`)
	vueBareHandlerRe         = regexp.MustCompile(`^(?:this\.|super\.)?[A-Za-z_$][A-Za-z0-9_$]*(?:\.[A-Za-z_$][A-Za-z0-9_$]*)*$`)
	vueStartTagRe            = regexp.MustCompile(`(?is)<\s*([A-Za-z][A-Za-z0-9_.:-]*)([^<>]*)>`)
	vueAttrRe                = regexp.MustCompile(`(?is)([@:A-Za-z_][A-Za-z0-9_:@.:-]*)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'=<>` + "`" + `]+)))?`)
	vueDefineModelRe         = regexp.MustCompile(`(?s)\bdefineModel(?:\s*<[^>]*>)?\s*\(\s*(?:['"]([^'"]+)['"])?`)
	vueDefinePropsTypeRe     = regexp.MustCompile(`(?s)\bdefineProps\s*<\s*\{(.*?)\}\s*>\s*\(`)
	vueDefinePropsObjectRe   = regexp.MustCompile(`(?s)\bdefineProps\s*\(\s*\{(.*?)\}\s*\)`)
	vueDefineEmitsArrayRe    = regexp.MustCompile(`(?s)\bdefineEmits(?:\s*<[^>]*>)?\s*\(\s*\[(.*?)\]`)
	vueObjectKeyRe           = regexp.MustCompile(`(?m)([A-Za-z_$][A-Za-z0-9_$]*|'[^']+'|"[^"]+")\s*[?:]?\s*[:(]`)
	vueStringLiteralRe       = regexp.MustCompile(`['"]([^'"]+)['"]`)
	cssClassSelectorRe       = regexp.MustCompile(`(?m)(^|[,{]\s*)\.(-?[_a-zA-Z][_a-zA-Z0-9-]*)\b`)
)

// callStopWords filters keywords masquerading as identifiers in JS/Vue call
// sites reached via heuristic (non-structural) scanning. The JS/TS parser
// itself never confuses keywords with idents; this only matters for
// secondary heuristics like framework route detection.
var callStopWords = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "catch": true,
	"return": true, "typeof": true, "new": true, "class": true, "def": true,
	"with": true, "super": true, "await": true, "import": true, "from": true,
	"yield": true, "raise": true, "lambda": true, "and": true, "or": true,
	"not": true, "in": true, "is": true, "elif": true, "else": true, "print": true,
	"async": true,
	// Literal keywords. None of these can be a call target in any indexed
	// language, but a Vue bound attribute like :disabled="false" reaches the
	// template bare-handler path as a plausible identifier and would otherwise
	// be emitted as an unresolved call target.
	"true": true, "false": true, "null": true, "undefined": true,
	"NaN": true, "Infinity": true, "None": true, "True": true, "False": true,
	"nil": true,
}

// jsBuiltinCallables holds JS/TS global functions/constructors and
// Array/String/Object/Promise prototype method names that are extremely
// common as the tail of a chained call (e.g. `items.filter(...).map(...)`,
// where the `.map` receiver is a call expression rather than a simple
// identifier, so its Callee comes through as the bare name "map") or as a
// genuinely receiver-less global call (`require(...)`, `ref(...)`,
// `Date.now` truncated to `Date`, etc.). Bare calls (no `.` in Callee) to
// these names never resolve to anything in the repo, so they are skipped
// rather than emitted as `unresolved:push`/`unresolved:require`/etc. noise.
var jsBuiltinCallables = map[string]bool{
	// Globals / constructors / module loaders.
	"require": true, "Date": true, "Promise": true, "Map": true, "Set": true,
	"WeakMap": true, "WeakSet": true, "RegExp": true, "Array": true,
	"Object": true, "JSON": true, "Math": true, "Number": true, "String": true,
	"Boolean": true, "Symbol": true, "Proxy": true, "Reflect": true,
	"parseInt": true, "parseFloat": true, "setTimeout": true, "setInterval": true,
	"encodeURIComponent": true, "decodeURIComponent": true,
	// Vue composition API (imported from the external "vue" package).
	"ref": true, "reactive": true, "computed": true, "watch": true, "watchEffect": true,
	// Array/String/Object/Promise prototype methods commonly reached via
	// chained calls, where the receiver is itself a call expression.
	"push": true, "pop": true, "shift": true, "unshift": true, "map": true,
	"filter": true, "forEach": true, "reduce": true, "reduceRight": true,
	"find": true, "findIndex": true, "some": true, "every": true,
	"slice": true, "splice": true, "concat": true, "join": true, "sort": true,
	"reverse": true, "includes": true, "indexOf": true, "lastIndexOf": true,
	"flat": true, "flatMap": true, "replace": true, "replaceAll": true,
	"split": true, "trim": true, "trimStart": true, "trimEnd": true,
	"toLowerCase": true, "toUpperCase": true, "match": true, "matchAll": true,
	"test": true, "keys": true, "values": true, "entries": true, "assign": true,
	"then": true, "finally": true, "toString": true, "hasOwnProperty": true,
	// Global error constructors and Array statics, also reachable bare via chains.
	"Error": true, "TypeError": true, "RangeError": true, "isArray": true,
	// fetch Response body readers, chained off a call expression.
	"json": true, "text": true, "blob": true,
	// DOM API methods, chained off document/element call expressions.
	"createElement": true, "appendChild": true, "removeChild": true,
	"querySelector": true, "querySelectorAll": true, "getElementById": true,
	"addEventListener": true, "removeEventListener": true, "setAttribute": true,
	"getAttribute": true,
	// jest/vitest globals and matchers.
	"expect": true, "describe": true, "beforeEach": true, "afterEach": true,
	"beforeAll": true, "afterAll": true, "toBe": true, "toEqual": true,
	"toHaveBeenCalled": true, "toHaveBeenCalledWith": true, "toBeNull": true,
	"toBeUndefined": true, "toBeTruthy": true, "toBeFalsy": true,
	"toContain": true, "toThrow": true, "toMatchObject": true,
	// Date/Number formatting and jest mock-chain methods, chained off a call
	// expression (e.g. `Date.now()`, `jest.fn().mockResolvedValue(...)`).
	"now": true, "toLocaleString": true, "toLocaleDateString": true,
	"toLocaleTimeString": true, "toFixed": true, "mockResolvedValue": true,
	"mockReturnValue": true, "mockImplementation": true, "mockRejectedValue": true,
	// Express response/request chain methods, chained off a call expression
	// (e.g. `res.status(200).send(...)`, where `.send`'s receiver is the
	// `res.status(200)` call expression, so its Callee comes through bare).
	"send": true, "sendStatus": true, "redirect": true, "render": true,
	"sendFile": true, "download": true, "attachment": true, "vary": true,
	"clearCookie": true, "append": true, "location": true, "links": true,
}

// jsGlobalReceivers holds JS global objects (console, document, window,
// process, and built-in namespaces) whose methods (e.g. `console.log`,
// `document.createElement`, `Array.isArray`) never resolve to anything in
// the repo. Dotted calls whose receiver root is one of these are skipped.
var jsGlobalReceivers = map[string]bool{
	"console": true, "document": true, "window": true, "process": true,
	"Array": true, "Object": true, "Math": true, "JSON": true, "Number": true,
	"String": true, "Promise": true, "Reflect": true, "globalThis": true,
	"jest": true, "vi": true,
}

// pythonBuiltinCallables holds Python's builtin functions/types (the
// contents of the `builtins` module commonly called directly, e.g.
// `print(...)`, `len(...)`, `str(x)`). Bare calls to these are not edges to
// anything in the repo, so emitPythonRelationsTS skips them rather than
// emitting `unresolved:print`/`unresolved:len`/etc. noise that dominates
// `doctor`'s topUnresolved on Python repos.
var pythonBuiltinCallables = map[string]bool{
	"abs": true, "aiter": true, "all": true, "anext": true, "any": true,
	"ascii": true, "bin": true, "bool": true, "breakpoint": true,
	"bytearray": true, "bytes": true, "callable": true, "chr": true,
	"classmethod": true, "compile": true, "complex": true, "delattr": true,
	"dict": true, "dir": true, "divmod": true, "enumerate": true, "eval": true,
	"exec": true, "filter": true, "float": true, "format": true,
	"frozenset": true, "getattr": true, "globals": true, "hasattr": true,
	"hash": true, "help": true, "hex": true, "id": true, "input": true,
	"int": true, "isinstance": true, "issubclass": true, "iter": true,
	"len": true, "list": true, "locals": true, "map": true, "max": true,
	"memoryview": true, "min": true, "next": true, "object": true, "oct": true,
	"open": true, "ord": true, "pow": true, "print": true, "property": true,
	"range": true, "repr": true, "reversed": true, "round": true, "set": true,
	"setattr": true, "slice": true, "sorted": true, "staticmethod": true,
	"str": true, "sum": true, "super": true, "tuple": true, "type": true,
	"vars": true, "zip": true, "__import__": true,
}

// AddCGPSymbol registers a CGP symbol. It is safe under concurrent BuildIndex
// because Index mutations are guarded by mu (see types.go).
func (idx *Index) AddCGPSymbol(sym CGPSymbol) CGPSymbol {
	if sym.ID == "" || sym.Name == "" {
		return CGPSymbol{}
	}
	if sym.Confidence == "" {
		sym.Confidence = ConfHeuristic
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.initRuntimeLocked()
	if idx.symbolSeen[sym.ID] {
		return idx.Symbols[sym.ID]
	}
	idx.beginSymbolGraphMutationLocked(false)
	idx.Symbols[sym.ID] = sym
	idx.symbolSeen[sym.ID] = true
	idx.orderedSymbolIDs = nil
	if len(sym.ReturnTypes) > 0 {
		idx.goReturnTypes[sym.ID] = append([]string(nil), sym.ReturnTypes...)
	}
	idx.invalidateRepoMapCache()
	return sym
}

func (idx *Index) AddCGPEdge(from, to, edgeType, confidence string, loc Location) {
	idx.AddCGPEdgeWithReason(from, to, edgeType, confidence, "", loc)
}

// AddCGPEdgeWithReason is the full call surface; reason is only recorded for
// unresolved edges and is otherwise ignored. The dedup key intentionally
// excludes confidence/reason so two scans of the same site do not produce
// duplicate edges with different confidences — last writer wins on metadata,
// which keeps incremental rebakes idempotent.
func (idx *Index) AddCGPEdgeWithReason(from, to, edgeType, confidence, reason string, loc Location) {
	if from == "" || to == "" || edgeType == "" {
		return
	}
	if confidence == "" {
		confidence = ConfHeuristic
	}
	if confidence != ConfUnresolved {
		reason = ""
	}
	id := canonicalCGPEdgeID(from, to, edgeType, loc)
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.initRuntimeLocked()
	if idx.symbolEdgeSeen[id] {
		return
	}
	idx.beginSymbolGraphMutationLocked(false)
	idx.SymbolEdges = append(idx.SymbolEdges, CGPEdge{
		ID: id, From: from, To: to, Type: edgeType,
		Confidence: confidence, UnresolvedReason: reason, Evidence: loc,
	})
	idx.symbolEdgeSeen[id] = true
	idx.invalidateRepoMapCache()
}

func AddCGPFromTTL(idx *Index) {
	idx.mu.Lock()
	terms := make([]Term, 0, len(idx.Terms))
	for _, term := range idx.Terms {
		terms = append(terms, term)
	}
	shapes := make([]Shape, 0, len(idx.Shapes))
	for _, shape := range idx.Shapes {
		shapes = append(shapes, shape)
	}
	idx.mu.Unlock()

	for _, term := range terms {
		for _, loc := range term.Locations {
			idx.AddCGPSymbol(CGPSymbol{
				ID:          ttlTermSymbolID(term.ID, loc.File, loc.StartLine),
				Name:        term.Term,
				Kind:        "ttl-term",
				Language:    "ttl",
				File:        loc.File,
				StartLine:   loc.StartLine,
				StartColumn: loc.StartColumn,
				EndLine:     loc.EndLine,
				EndColumn:   loc.EndColumn,
				Signature:   term.IRI,
				Confidence:  "exact",
			})
		}
	}
	for _, shape := range shapes {
		idx.AddCGPSymbol(CGPSymbol{
			ID:          "symbol:ttl:shape:" + shape.ID,
			Name:        shape.Term,
			Kind:        "ttl-shape",
			Language:    "ttl",
			File:        shape.Location.File,
			StartLine:   shape.Location.StartLine,
			StartColumn: shape.Location.StartColumn,
			EndLine:     shape.Location.EndLine,
			EndColumn:   shape.Location.EndColumn,
			Signature:   shape.IRI,
			Confidence:  "exact",
		})
	}
}

// ScanCGPSymbols extracts the symbol layer for a single file. The JS/TS/Vue
// path delegates to the token-driven parser; Python and Java are parsed
// structurally via tree-sitter.
func ScanCGPSymbols(idx *Index, file, language, content string) {
	fileSym := idx.AddCGPSymbol(CGPSymbol{
		ID:          fileSymbolID(file),
		Name:        file,
		Kind:        "file",
		Language:    language,
		File:        file,
		StartLine:   1,
		StartColumn: 1,
		EndLine:     countLines(content),
		EndColumn:   1,
		Confidence:  "exact",
	})
	switch language {
	case "javascript", "typescript":
		emitJSTSSymbols(idx, file, language, content, 0, fileSym.ID)
		recordJSDefaultExport(idx, file, content)
	case "vue":
		emitVueSymbols(idx, file, content, fileSym.ID)
	case "python":
		emitPythonSymbolsTS(idx, file, content, fileSym.ID)
	case "java":
		emitJavaSymbolsTS(idx, file, content, fileSym.ID)
	case "go":
		emitGoSymbolsTS(idx, file, content, fileSym.ID)
	case "csharp":
		emitCSharpSymbolsTS(idx, file, content, fileSym.ID)
	case "rust", "ruby", "php", "c", "cpp", "kotlin", "bash", "scala", "lua", "elixir", "dart", "haskell", "clojure", "swift", "r", "julia", "zig", "ocaml":
		emitGenericSymbolsTS(idx, language, file, content, fileSym.ID)
	case "hcl":
		if isTerraformNativeConfigFile(file) {
			emitTerraformSymbolsTS(idx, file, content, fileSym.ID)
		} else {
			emitGenericSymbolsTS(idx, language, file, content, fileSym.ID)
		}
	case "yaml":
		emitYAMLInfraSymbols(idx, file, content, fileSym.ID)
	case "dockerfile":
		emitDockerfileSymbols(idx, file, content, fileSym.ID)
	default:
		emitHeuristicSymbols(idx, file, language, content, fileSym.ID)
	}
}

// ScanCGPRelations emits import and call edges for a single file. Vue script
// blocks are scanned with the same JS/TS pipeline; Python and Java are parsed
// structurally via tree-sitter.
func ScanCGPRelations(idx *Index, file, language, content string) {
	switch language {
	case "javascript", "typescript":
		emitJSTSRelations(idx, file, content, 0, content)
	case "vue":
		for _, block := range scriptBlockRe.FindAllStringSubmatchIndex(content, -1) {
			body := content[block[2]:block[3]]
			emitJSTSRelations(idx, file, body, block[2], content)
		}
		emitVueTemplateRelations(idx, file, content)
	case "python":
		emitPythonRelationsTS(idx, file, content)
	case "java":
		emitJavaRelationsTS(idx, file, content)
	case "go":
		emitGoRelationsTS(idx, file, content)
	case "csharp":
		emitCSharpRelationsTS(idx, file, content)
	case "rust", "ruby", "php", "c", "cpp", "kotlin", "bash", "scala", "lua", "elixir", "dart", "haskell", "clojure", "swift", "r", "julia", "zig", "ocaml":
		emitGenericRelationsTS(idx, language, file, content)
	case "hcl":
		if isTerraformNativeConfigFile(file) {
			emitTerraformRelationsTS(idx, file, content)
		} else {
			emitGenericRelationsTS(idx, language, file, content)
		}
	case "yaml":
		emitYAMLInfraRelations(idx, file)
	case "dockerfile":
		emitDockerfileRelations(idx, file)
	default:
		emitHeuristicImports(idx, file, language, content)
	}
}

// emitJSTSSymbols runs the structural parser over a JS/TS source slice and
// converts ScannedSymbol entries into CGPSymbol entries. The slice may be a
// substring of the original file (e.g. a Vue <script> block); baseOffset is
// the byte offset of the slice within the original file content.
func emitJSTSSymbols(idx *Index, file, language, content string, baseOffset int, parentID string) {
	starts := lineStarts(content)
	lines := strings.Split(content, "\n")
	emitJSTSSymbolsScoped(idx, file, language, content, content, 0, starts, lines, parentID, "", true)
	_ = baseOffset // baseOffset is preserved on the call API for parity; line/col are derived from the local slice.
}

// emitJSTSSymbolsScoped is the recursive worker for JS/TS symbol emission.
// It accepts a `region` slice — a substring of the canonical `content` that
// starts at byte offset `regionStart` in `content` — and emits symbols whose
// line/column are computed against the canonical content's `starts`. After
// emitting each function-like symbol, it recurses into the symbol's body so
// nested declarations (composable-local arrows, helpers inside class
// methods, etc.) become first-class CGPSymbols with ParentID set. This is
// the universal-ctags scope-tracking pattern adapted to mamari's existing
// token-scan parser, and approximates SCIP's `parent.child` symbol grammar
// via the qualified-name chain in `parentQualified`.
func emitJSTSSymbolsScoped(idx *Index, file, language, region, content string, regionStart int, starts []int, lines []string, parentID, parentQualified string, recordDiagnostics bool) {
	res := ParseJS(region)
	if recordDiagnostics {
		idx.markFileParseDiagnostics(file, res.Diagnostics)
	}
	nested := regionStart > 0
	classIDByName := map[string]string{}
	for _, sym := range res.Symbols {
		// Skip non-callable nested decls so they don't shadow call-site
		// attribution. A nested `const data = await fetchData()` would
		// otherwise win over the enclosing function in containingSymbolFast.
		// Top-level constants are still emitted for namespace tracking.
		if nested && !isJSTSEmittableNestedKind(sym.Kind) {
			continue
		}
		absStart := regionStart + sym.Start
		absEnd := regionStart + clampOffset(content, sym.End)
		startLine, startCol := offsetToLineCol(starts, absStart)
		endLine, endCol := offsetToLineCol(starts, clampOffset(content, absEnd))
		signature := signatureLine(content, starts, startLine)
		returnTypes := inferReturnTypesFromSignature(signature, language)

		parent := parentID
		qualified := sym.Name
		switch sym.Kind {
		case "method", "getter", "setter":
			if pid, ok := classIDByName[sym.Parent]; ok {
				parent = pid
			}
			if sym.Parent != "" {
				qualified = sym.Parent + "." + sym.Name
			}
			if parentQualified != "" && sym.Parent == "" {
				qualified = parentQualified + "." + sym.Name
			}
		default:
			if sym.Parent != "" {
				qualified = sym.Parent + "." + sym.Name
			} else if parentQualified != "" {
				qualified = parentQualified + "." + sym.Name
			}
		}
		id := stableSymbolID(language, sym.Kind, file, qualified, idx)
		added := idx.AddCGPSymbol(CGPSymbol{
			ID:          id,
			Name:        sym.Name,
			Kind:        sym.Kind,
			Language:    language,
			File:        file,
			StartLine:   startLine,
			StartColumn: startCol,
			EndLine:     endLine,
			EndColumn:   endCol,
			Signature:   signature,
			Docstring:   extractSymbolDocstring(lines, startLine, language),
			ReturnTypes: returnTypes,
			Exported:    sym.Exported,
			ParentID:    parent,
			Confidence:  ConfExact,
		})
		if sym.Kind == "class" {
			classIDByName[sym.Name] = added.ID
		}
		if !isJSTSFunctionLikeKind(sym.Kind) || added.ID == "" {
			continue
		}
		bodyStart, bodyEnd, ok := jsFunctionBodyByteRange(content, absStart, absEnd)
		if !ok || bodyEnd-bodyStart < 4 {
			continue
		}
		nestedRegion := content[bodyStart:bodyEnd]
		emitJSTSSymbolsScoped(idx, file, language, nestedRegion, content, bodyStart, starts, lines, added.ID, qualified, false)
	}
}

func isJSTSFunctionLikeKind(kind string) bool {
	switch kind {
	case "function", "method", "getter", "setter":
		return true
	}
	return false
}

// isJSTSEmittableNestedKind decides whether a kind discovered inside another
// function's body deserves its own CGPSymbol entry. Functions, classes, and
// the structural type definitions (interface/type/enum) are useful — they
// host nested calls or document the inner shape — while plain constants and
// variables are intentionally suppressed. A nested `const data = fetch()`
// would otherwise become the deepest symbol on the call's line and steal
// caller attribution from the enclosing function.
func isJSTSEmittableNestedKind(kind string) bool {
	switch kind {
	case "function", "method", "getter", "setter", "class", "interface", "type", "enum":
		return true
	}
	return false
}

// jsFunctionBodyByteRange locates the body byte range (between { and }, both
// exclusive) of a function-like symbol that spans [symStart, symEnd) in
// `content`. Strings/comments inside the parameter region are masked so a
// curly inside a default-arg string ({ '{': true }) does not fool the scan.
// Returns false for abstract/overload signatures that have no body.
func jsFunctionBodyByteRange(content string, symStart, symEnd int) (int, int, bool) {
	if symStart < 0 || symEnd <= symStart || symEnd > len(content) {
		return 0, 0, false
	}
	region := content[symStart:symEnd]
	masked := MaskStringsAndComments(region)
	open := strings.IndexByte(masked, '{')
	if open < 0 {
		return 0, 0, false
	}
	closeRel := matchingClose(masked, open)
	if closeRel <= open+1 {
		return 0, 0, false
	}
	bodyStart := symStart + open + 1
	bodyEnd := symStart + closeRel - 1
	if bodyStart >= bodyEnd {
		return 0, 0, false
	}
	return bodyStart, bodyEnd, true
}

func emitVueSymbols(idx *Index, file, content, parentID string) {
	componentName := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	componentConfidence := ConfHeuristic // filename-derived names are best-effort.
	if name := vueComponentName(content); name != "" {
		componentName = name
		componentConfidence = ConfExact // explicit defineOptions/export default name.
	}
	component := idx.AddCGPSymbol(CGPSymbol{
		ID:          stableSymbolID("vue", "component", file, componentName, idx),
		Name:        componentName,
		Kind:        "component",
		Language:    "vue",
		File:        file,
		StartLine:   1,
		StartColumn: 1,
		EndLine:     countLines(content),
		EndColumn:   1,
		ParentID:    parentID,
		Confidence:  componentConfidence,
	})
	emitVueTemplateClassSymbols(idx, file, content, component.ID)
	emitVueStyleClassSymbols(idx, file, content, component.ID)
	emitVueAPISymbols(idx, file, content, component.ID)
	for _, block := range scriptBlockRe.FindAllStringSubmatchIndex(content, -1) {
		body := content[block[2]:block[3]]
		emitJSTSSymbolsForVueScript(idx, file, body, block[2], content, parentID)
	}
}

func emitVueAPISymbols(idx *Index, file, content, parentID string) {
	fileStarts := lineStarts(content)
	seen := map[string]bool{}
	for _, block := range scriptBlockRe.FindAllStringSubmatchIndex(content, -1) {
		bodyStart := block[2]
		body := content[block[2]:block[3]]
		for _, item := range vuePropsFromScript(body) {
			addVueAPISymbol(idx, file, content, fileStarts, bodyStart+item.offset, "vue-prop", item.name, parentID, seen)
		}
		for _, item := range vueModelsFromScript(body) {
			addVueAPISymbol(idx, file, content, fileStarts, bodyStart+item.offset, "vue-model", item.name, parentID, seen)
		}
		for _, item := range vueEmitsFromScript(body) {
			addVueAPISymbol(idx, file, content, fileStarts, bodyStart+item.offset, "vue-emit", item.name, parentID, seen)
		}
	}
}

type vueAPIItem struct {
	name   string
	offset int
}

func addVueAPISymbol(idx *Index, file, content string, starts []int, offset int, kind, name, parentID string, seen map[string]bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	key := kind + ":" + name
	if seen[key] {
		return
	}
	seen[key] = true
	line, col := offsetToLineCol(starts, clampOffset(content, offset))
	idx.AddCGPSymbol(CGPSymbol{
		ID:          stableSymbolID("vue", kind, file, name, idx),
		Name:        name,
		Kind:        kind,
		Language:    "vue",
		File:        file,
		StartLine:   line,
		StartColumn: col,
		EndLine:     line,
		EndColumn:   col + len(name),
		Signature:   strings.TrimSpace(signatureLine(content, starts, line)),
		ParentID:    parentID,
		Confidence:  ConfHeuristic,
	})
}

func vuePropsFromScript(body string) []vueAPIItem {
	var out []vueAPIItem
	out = append(out, vuePropsFromMatches(body, vueDefinePropsTypeRe.FindAllStringSubmatchIndex(body, -1))...)
	out = append(out, vuePropsFromMatches(body, vueDefinePropsObjectRe.FindAllStringSubmatchIndex(body, -1))...)
	return dedupeVueAPIItems(out)
}

func vuePropsFromMatches(body string, matches [][]int) []vueAPIItem {
	var out []vueAPIItem
	for _, m := range matches {
		if len(m) < 4 || m[2] < 0 {
			continue
		}
		blockStart, blockEnd := m[2], m[3]
		block := body[blockStart:blockEnd]
		for _, km := range vueObjectKeyRe.FindAllStringSubmatchIndex(block, -1) {
			raw := block[km[2]:km[3]]
			name := trimVueQuotedName(raw)
			if name == "" || vueIgnoredObjectKey(name) {
				continue
			}
			out = append(out, vueAPIItem{name: name, offset: blockStart + km[2]})
		}
	}
	return out
}

func vueModelsFromScript(body string) []vueAPIItem {
	var out []vueAPIItem
	for _, m := range vueDefineModelRe.FindAllStringSubmatchIndex(body, -1) {
		name := "modelValue"
		offset := m[0]
		if len(m) >= 4 && m[2] >= 0 {
			name = body[m[2]:m[3]]
			offset = m[2]
		}
		out = append(out, vueAPIItem{name: name, offset: offset})
	}
	return dedupeVueAPIItems(out)
}

func vueEmitsFromScript(body string) []vueAPIItem {
	var out []vueAPIItem
	for _, m := range vueDefineEmitsArrayRe.FindAllStringSubmatchIndex(body, -1) {
		if len(m) < 4 || m[2] < 0 {
			continue
		}
		blockStart := m[2]
		block := body[m[2]:m[3]]
		for _, sm := range vueStringLiteralRe.FindAllStringSubmatchIndex(block, -1) {
			out = append(out, vueAPIItem{name: block[sm[2]:sm[3]], offset: blockStart + sm[2]})
		}
	}
	return dedupeVueAPIItems(out)
}

func dedupeVueAPIItems(in []vueAPIItem) []vueAPIItem {
	seen := map[string]bool{}
	out := make([]vueAPIItem, 0, len(in))
	for _, item := range in {
		if item.name == "" || seen[item.name] {
			continue
		}
		seen[item.name] = true
		out = append(out, item)
	}
	return out
}

func trimVueQuotedName(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func vueIgnoredObjectKey(name string) bool {
	switch name {
	case "type", "required", "default", "validator":
		return true
	default:
		return false
	}
}

func emitVueTemplateClassSymbols(idx *Index, file, content, parentID string) {
	starts := lineStarts(content)
	for _, block := range vueTemplateRanges(content) {
		bodyStart := block.start
		body := content[block.start:block.end]
		masked := maskHTMLComments(body)
		for _, attr := range staticClassAttributes(masked) {
			valueStart := attr.valueStart
			valueEnd := attr.valueEnd
			value := html.UnescapeString(body[valueStart:valueEnd])
			for _, className := range splitStaticClassNames(value) {
				classOffset := strings.Index(body[valueStart:valueEnd], className)
				if classOffset < 0 {
					classOffset = 0
				}
				fileOffset := bodyStart + valueStart + classOffset
				line, col := offsetToLineCol(starts, fileOffset)
				idx.AddCGPSymbol(CGPSymbol{
					ID:          stableSymbolID("vue", "template-class", file, className, idx),
					Name:        className,
					Kind:        "template-class",
					Language:    "vue",
					File:        file,
					StartLine:   line,
					StartColumn: col,
					EndLine:     line,
					EndColumn:   col + len(className),
					Signature:   strings.TrimSpace(signatureLine(content, starts, line)),
					ParentID:    parentID,
					Confidence:  ConfExact,
				})
			}
		}
	}
}

type classAttribute struct {
	valueStart int
	valueEnd   int
}

func staticClassAttributes(s string) []classAttribute {
	lower := strings.ToLower(s)
	var out []classAttribute
	for offset := 0; offset < len(lower); {
		idx := strings.Index(lower[offset:], "class")
		if idx < 0 {
			break
		}
		idx += offset
		if idx > 0 {
			prev := lower[idx-1]
			if prev == ':' || prev == '@' || prev == '_' || prev == '-' || isAlphaNumByte(prev) {
				offset = idx + len("class")
				continue
			}
		}
		j := idx + len("class")
		for j < len(lower) && isHTMLSpace(lower[j]) {
			j++
		}
		if j >= len(lower) || lower[j] != '=' {
			offset = idx + len("class")
			continue
		}
		j++
		for j < len(lower) && isHTMLSpace(lower[j]) {
			j++
		}
		if j >= len(lower) || (lower[j] != '"' && lower[j] != '\'') {
			offset = idx + len("class")
			continue
		}
		quote := lower[j]
		valueStart := j + 1
		valueEnd := valueStart
		for valueEnd < len(lower) && lower[valueEnd] != quote {
			valueEnd++
		}
		if valueEnd >= len(lower) {
			break
		}
		out = append(out, classAttribute{valueStart: valueStart, valueEnd: valueEnd})
		offset = valueEnd + 1
	}
	return out
}

func isHTMLSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f'
}

func isAlphaNumByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

func emitVueStyleClassSymbols(idx *Index, file, content, parentID string) {
	starts := lineStarts(content)
	defsByName := map[string][]CGPSymbol{}
	for _, block := range styleBlockRe.FindAllStringSubmatchIndex(content, -1) {
		bodyStart := block[2]
		body := content[block[2]:block[3]]
		for _, match := range cssClassSelectorRe.FindAllStringSubmatchIndex(body, -1) {
			className := body[match[4]:match[5]]
			fileOffset := bodyStart + match[4]
			line, col := offsetToLineCol(starts, fileOffset)
			endLine := cssRuleEndLine(content, starts, bodyStart+match[0])
			sym := idx.AddCGPSymbol(CGPSymbol{
				ID:          stableSymbolID("css", "css-class", file, className, idx),
				Name:        className,
				Kind:        "css-class",
				Language:    "css",
				File:        file,
				StartLine:   line,
				StartColumn: col,
				EndLine:     endLine,
				EndColumn:   1,
				Signature:   strings.TrimSpace(signatureLine(content, starts, line)),
				ParentID:    parentID,
				Confidence:  ConfExact,
			})
			defsByName[className] = append(defsByName[className], sym)
		}
	}
	if len(defsByName) == 0 {
		return
	}
}

func linkAllTemplateClassUsages(idx *Index) {
	idx.ensureFileSymbolIndex()
	idx.mu.Lock()
	files := map[string]bool{}
	for _, sym := range idx.Symbols {
		if sym.Kind == "css-class" {
			files[sym.File] = true
		}
	}
	idx.mu.Unlock()
	paths := make([]string, 0, len(files))
	for file := range files {
		paths = append(paths, file)
	}
	sort.Strings(paths)
	for _, file := range paths {
		linkTemplateClassUsagesForFile(idx, file)
	}
}

func linkTemplateClassUsagesForFile(idx *Index, file string) {
	idx.ensureFileSymbolIndex()
	idx.mu.Lock()
	defsByName := map[string][]CGPSymbol{}
	for _, sym := range idx.symbolsByFile[file] {
		if sym.Kind == "css-class" {
			defsByName[sym.Name] = append(defsByName[sym.Name], sym)
		}
	}
	idx.mu.Unlock()
	if len(defsByName) > 0 {
		linkTemplateClassUsages(idx, file, defsByName)
	}
}

func splitStaticClassNames(value string) []string {
	fields := strings.Fields(value)
	out := make([]string, 0, len(fields))
	seen := map[string]bool{}
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" || strings.ContainsAny(field, "{}[]():") {
			continue
		}
		if seen[field] {
			continue
		}
		seen[field] = true
		out = append(out, field)
	}
	return out
}

func cssRuleEndLine(content string, starts []int, selectorOffset int) int {
	endOffset := selectorOffset
	if open := strings.IndexByte(content[selectorOffset:], '{'); open >= 0 {
		open += selectorOffset
		if close := strings.IndexByte(content[open:], '}'); close >= 0 {
			endOffset = open + close + 1
		}
	}
	line, _ := offsetToLineCol(starts, clampOffset(content, endOffset))
	return line
}

func linkTemplateClassUsages(idx *Index, file string, defsByName map[string][]CGPSymbol) {
	idx.ensureFileSymbolIndex()
	idx.mu.Lock()
	var usages []CGPSymbol
	for _, sym := range idx.symbolsByFile[file] {
		if sym.Kind == "template-class" {
			usages = append(usages, sym)
		}
	}
	idx.mu.Unlock()
	sort.Slice(usages, func(i, j int) bool {
		if usages[i].StartLine != usages[j].StartLine {
			return usages[i].StartLine < usages[j].StartLine
		}
		return usages[i].StartColumn < usages[j].StartColumn
	})
	for _, usage := range usages {
		for _, def := range defsByName[usage.Name] {
			idx.AddCGPEdge(usage.ID, def.ID, "uses-css-class", ConfExact, Location{
				File:        usage.File,
				StartLine:   usage.StartLine,
				StartColumn: usage.StartColumn,
				EndLine:     usage.EndLine,
				EndColumn:   usage.EndColumn,
				Kind:        "template-class",
				Raw:         usage.Name,
			})
		}
	}
}

// emitJSTSSymbolsForVueScript scans a Vue <script> block. Symbol StartLine/
// StartColumn are reported against the *full file content* so agents can
// jump straight into the .vue file.
func emitJSTSSymbolsForVueScript(idx *Index, file, body string, baseOffset int, fullContent string, parentID string) {
	starts := lineStarts(fullContent)
	lines := strings.Split(fullContent, "\n")
	emitJSTSSymbolsScoped(idx, file, "typescript", body, fullContent, baseOffset, starts, lines, parentID, "", true)
}

// emitPythonSymbolsTS uses the tree-sitter Python grammar to emit
// CGPSymbols for classes, functions, and methods with proper nesting
// (ParentID set from the containment chain), decorator-inclusive ranges,
// and collapsed multi-line signatures.
func emitPythonSymbolsTS(idx *Index, file, content, parentID string) {
	res, err := treesitter.Parse("python", []byte(content))
	if err != nil {
		idx.markFileParseDiagnostics(file, []ScanDiagnostic{{Code: "treesitter_error", Message: err.Error()}})
		return
	}
	if !res.ParseOK {
		idx.markFileParseDiagnostics(file, []ScanDiagnostic{{Code: "parse_error", Message: res.ParseError}})
	}

	starts := lineStarts(content)
	lines := strings.Split(content, "\n")
	idByStart := map[int]string{}
	ranges := []varScopeRange{{start: 0, end: len(content), id: parentID}}

	for _, def := range res.Defs {
		startLine, startCol := offsetToLineCol(starts, def.Start)
		endLine, endCol := offsetToLineCol(starts, clampOffset(content, def.End))

		parent := parentID
		if def.ParentName != "" {
			if pid, ok := idByStart[def.ParentStart]; ok {
				parent = pid
			}
		}

		added := idx.AddCGPSymbol(CGPSymbol{
			ID:          stableSymbolID("python", def.Kind, file, def.QualifiedName, idx),
			Name:        def.Name,
			Kind:        def.Kind,
			Language:    "python",
			File:        file,
			StartLine:   startLine,
			StartColumn: startCol,
			EndLine:     endLine,
			EndColumn:   endCol,
			Signature:   collapseSignature(content, def.Start, def.End),
			Docstring:   extractSymbolDocstring(lines, startLine, "python"),
			Exported:    def.Exported,
			ParentID:    parent,
			Confidence:  ConfExact,
		})
		idByStart[def.Start] = added.ID
		ranges = append(ranges, varScopeRange{start: def.Start, end: def.End, id: added.ID})

		if def.Kind == "class" && len(def.Bases) > 0 {
			idx.mu.Lock()
			if idx.classBases == nil {
				idx.classBases = map[string][]string{}
			}
			idx.classBases[added.ID] = def.Bases
			idx.mu.Unlock()
		}
	}
	populatePythonVarTypes(idx, res.Vars, ranges)
}

// collapseSignature collapses a possibly multi-line `def`/`class` header
// (through its trailing top-level ':') into a single line, for use as a
// symbol's Signature field. Tree-sitter gives exact node boundaries, so this
// works even for signatures spanning many lines with default-arg strings
// containing ':' or '#'.
func collapseSignature(content string, start, end int) string {
	end = clampOffset(content, end)
	if start < 0 || start >= end {
		return ""
	}
	region := content[start:end]
	masked := maskPythonStringsAndComments(region)

	headerEnd := len(region)
	depth := 0
loop:
	for i := 0; i < len(masked); i++ {
		switch masked[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ':':
			if depth == 0 {
				headerEnd = i + 1
				break loop
			}
		}
	}

	return strings.Join(strings.Fields(region[:headerEnd]), " ")
}

// emitJavaSymbolsTS uses the tree-sitter Java grammar to emit CGPSymbols for
// classes, interfaces, enums, records, and methods with proper nesting
// (ParentID set from the containment chain) and collapsed multi-line
// signatures.
func emitJavaSymbolsTS(idx *Index, file, content, parentID string) {
	res, err := treesitter.Parse("java", []byte(content))
	if err != nil {
		idx.markFileParseDiagnostics(file, []ScanDiagnostic{{Code: "treesitter_error", Message: err.Error()}})
		return
	}
	if !res.ParseOK {
		idx.markFileParseDiagnostics(file, []ScanDiagnostic{{Code: "parse_error", Message: res.ParseError}})
	}

	starts := lineStarts(content)
	idByStart := map[int]string{}
	var ranges []varScopeRange

	// springClassBase maps a class def's Start offset to its
	// @RequestMapping base path (e.g. "/api/owners"), for combining with
	// method-level @GetMapping/@PostMapping/etc. paths below.
	springClassBase := map[int]string{}
	for _, def := range res.Defs {
		if def.Kind != "class" {
			continue
		}
		for _, ann := range def.Annotations {
			if ann.Name == "RequestMapping" {
				springClassBase[def.Start] = ann.Value
			}
		}
	}

	if res.Package != "" || len(res.Imports) > 0 {
		idx.mu.Lock()
		if res.Package != "" {
			if idx.javaPackages == nil {
				idx.javaPackages = map[string]string{}
			}
			idx.javaPackages[file] = res.Package
		}
		if len(res.Imports) > 0 {
			if idx.javaImports == nil {
				idx.javaImports = map[string][]string{}
			}
			specs := make([]string, 0, len(res.Imports))
			for _, imp := range res.Imports {
				specs = append(specs, imp.Spec)
			}
			idx.javaImports[file] = specs
		}
		idx.mu.Unlock()
	}

	for _, def := range res.Defs {
		startLine, startCol := offsetToLineCol(starts, def.Start)
		endLine, endCol := offsetToLineCol(starts, clampOffset(content, def.End))

		parent := parentID
		if def.ParentName != "" {
			if pid, ok := idByStart[def.ParentStart]; ok {
				parent = pid
			}
		}

		added := idx.AddCGPSymbol(CGPSymbol{
			ID:          stableSymbolID("java", def.Kind, file, def.QualifiedName, idx),
			Name:        def.Name,
			Kind:        def.Kind,
			Language:    "java",
			File:        file,
			StartLine:   startLine,
			StartColumn: startCol,
			EndLine:     endLine,
			EndColumn:   endCol,
			Signature:   collapseJavaSignature(content, def.Start, def.End),
			Exported:    def.Exported,
			ParentID:    parent,
			Confidence:  ConfExact,
		})
		idByStart[def.Start] = added.ID
		ranges = append(ranges, varScopeRange{start: def.Start, end: def.End, id: added.ID})

		if def.Kind == "class" && res.Package != "" {
			idx.mu.Lock()
			if idx.javaFQN == nil {
				idx.javaFQN = map[string]string{}
			}
			idx.javaFQN[res.Package+"."+def.Name] = added.ID
			idx.mu.Unlock()
		}
		if def.Kind == "class" && len(def.Bases) > 0 {
			idx.mu.Lock()
			if idx.classBases == nil {
				idx.classBases = map[string][]string{}
			}
			idx.classBases[added.ID] = def.Bases
			idx.mu.Unlock()
		}
		if def.Kind == "class" && len(def.Interfaces) > 0 {
			idx.mu.Lock()
			if idx.classInterfaces == nil {
				idx.classInterfaces = map[string][]string{}
			}
			idx.classInterfaces[added.ID] = def.Interfaces
			idx.mu.Unlock()
		}

		if def.Kind == "method" {
			for _, ann := range def.Annotations {
				httpMethod, ok := springHTTPMethod(ann)
				if !ok {
					continue
				}
				fullPath := joinHTTPPaths(springClassBase[def.ParentStart], ann.Value)
				routeName := httpMethod + " " + fullPath
				routeSym := idx.AddCGPSymbol(CGPSymbol{
					ID:          stableSymbolID("http", "http-route", file, routeName, idx),
					Name:        routeName,
					Kind:        "http-route",
					Language:    "java",
					File:        file,
					StartLine:   startLine,
					StartColumn: startCol,
					EndLine:     startLine,
					EndColumn:   startCol + len(routeName),
					Signature:   strings.TrimSpace(signatureLine(content, starts, startLine)),
					Confidence:  ConfExact,
				})
				idx.AddCGPEdge(routeSym.ID, added.ID, "handles-route", ConfExact, Location{
					File: file, StartLine: startLine, StartColumn: startCol, EndLine: startLine, EndColumn: startCol + len(routeName),
					Kind: "http-route", Raw: routeName + " -> " + def.QualifiedName,
				})
			}
		}
	}

	populateVarTypes(idx, res.Vars, ranges)
}

// aspNetHTTPMethodAttrs maps ASP.NET Core route-attribute names to the HTTP
// method they declare.
var aspNetHTTPMethodAttrs = map[string]string{
	"HttpGet":     "GET",
	"HttpPost":    "POST",
	"HttpPut":     "PUT",
	"HttpDelete":  "DELETE",
	"HttpPatch":   "PATCH",
	"HttpHead":    "HEAD",
	"HttpOptions": "OPTIONS",
}

// aspNetHTTPMethod reports whether ann is an ASP.NET Core HTTP-method
// attribute (`[HttpGet]`/`[HttpPost]`/etc.) and, if so, the HTTP method it
// declares.
func aspNetHTTPMethod(ann treesitter.Annotation) (string, bool) {
	m, ok := aspNetHTTPMethodAttrs[ann.Name]
	return m, ok
}

// joinASPNetPaths combines an ASP.NET Core controller's class-level
// [Route("api/[controller]")] base path template with a method-level
// [HttpGet("...")]/etc. path, substituting the "[controller]" placeholder
// token with controllerName (the controller's class name, minus its
// trailing "Controller"), then joining as joinHTTPPaths does, e.g.
// joinASPNetPaths("api/[controller]", "Owners", "{id}") -> "/api/Owners/{id}".
func joinASPNetPaths(base, controllerName, sub string) string {
	base = strings.ReplaceAll(base, "[controller]", controllerName)
	return joinHTTPPaths(base, sub)
}

// emitCSharpSymbolsTS uses the tree-sitter C# grammar to emit
// class/interface/struct/record/enum/constructor/method symbols with proper
// nesting, ASP.NET Core route symbols/edges (from [Route]/[HttpGet]/etc.
// attributes), and namespace/using/FQN index data for cross-file class
// resolution (idx.csharpNamespaces/csharpUsings/csharpFQN).
func emitCSharpSymbolsTS(idx *Index, file, content, parentID string) {
	res, err := treesitter.Parse("csharp", []byte(content))
	if err != nil {
		idx.markFileParseDiagnostics(file, []ScanDiagnostic{{Code: "treesitter_error", Message: err.Error()}})
		return
	}
	if !res.ParseOK {
		idx.markFileParseDiagnostics(file, []ScanDiagnostic{{Code: "parse_error", Message: res.ParseError}})
	}

	starts := lineStarts(content)
	idByStart := map[int]string{}
	var ranges []varScopeRange

	// aspNetClassBase maps a class def's Start offset to its
	// [Route("api/[controller]")] base path template, for combining with
	// method-level [HttpGet]/[HttpPost]/etc. paths below.
	aspNetClassBase := map[int]string{}
	for _, def := range res.Defs {
		if def.Kind != "class" {
			continue
		}
		for _, ann := range def.Annotations {
			if ann.Name == "Route" {
				aspNetClassBase[def.Start] = ann.Value
			}
		}
	}

	if res.Package != "" || len(res.Imports) > 0 {
		idx.mu.Lock()
		if idx.csharpNamespaces == nil {
			idx.csharpNamespaces = map[string]string{}
		}
		idx.csharpNamespaces[file] = res.Package
		if len(res.Imports) > 0 {
			if idx.csharpUsings == nil {
				idx.csharpUsings = map[string][]string{}
			}
			specs := make([]string, 0, len(res.Imports))
			for _, imp := range res.Imports {
				specs = append(specs, imp.Spec)
			}
			idx.csharpUsings[file] = specs
		}
		idx.mu.Unlock()
	}

	for _, def := range res.Defs {
		startLine, startCol := offsetToLineCol(starts, def.Start)
		endLine, endCol := offsetToLineCol(starts, clampOffset(content, def.End))

		parent := parentID
		if def.ParentName != "" {
			if pid, ok := idByStart[def.ParentStart]; ok {
				parent = pid
			}
		}

		added := idx.AddCGPSymbol(CGPSymbol{
			ID:          stableSymbolID("csharp", def.Kind, file, def.QualifiedName, idx),
			Name:        def.Name,
			Kind:        def.Kind,
			Language:    "csharp",
			File:        file,
			StartLine:   startLine,
			StartColumn: startCol,
			EndLine:     endLine,
			EndColumn:   endCol,
			Signature:   collapseJavaSignature(content, def.Start, def.End),
			Exported:    def.Exported,
			ParentID:    parent,
			Confidence:  ConfExact,
		})
		idByStart[def.Start] = added.ID
		ranges = append(ranges, varScopeRange{start: def.Start, end: def.End, id: added.ID})

		if def.Kind == "class" {
			idx.mu.Lock()
			if idx.csharpFQN == nil {
				idx.csharpFQN = map[string]string{}
			}
			idx.csharpFQN[res.Package+"."+def.Name] = added.ID
			if def.IsPartial {
				if idx.csharpPartialFragments == nil {
					idx.csharpPartialFragments = map[string][]string{}
				}
				fqn := res.Package + "." + def.Name
				dup := false
				for _, id := range idx.csharpPartialFragments[fqn] {
					if id == added.ID {
						dup = true
						break
					}
				}
				if !dup {
					idx.csharpPartialFragments[fqn] = append(idx.csharpPartialFragments[fqn], added.ID)
				}
			}
			idx.mu.Unlock()
		}
		if def.Kind == "class" && len(def.Bases) > 0 {
			idx.mu.Lock()
			if idx.classBases == nil {
				idx.classBases = map[string][]string{}
			}
			idx.classBases[added.ID] = def.Bases
			idx.mu.Unlock()
		}
		if def.Kind == "class" && len(def.Interfaces) > 0 {
			idx.mu.Lock()
			if idx.classInterfaces == nil {
				idx.classInterfaces = map[string][]string{}
			}
			idx.classInterfaces[added.ID] = def.Interfaces
			idx.mu.Unlock()
		}

		if def.Kind == "method" {
			for _, ann := range def.Annotations {
				httpMethod, ok := aspNetHTTPMethod(ann)
				if !ok {
					continue
				}
				controllerName := strings.TrimSuffix(def.ParentName, "Controller")
				fullPath := joinASPNetPaths(aspNetClassBase[def.ParentStart], controllerName, ann.Value)
				routeName := httpMethod + " " + fullPath
				routeSym := idx.AddCGPSymbol(CGPSymbol{
					ID:          stableSymbolID("http", "http-route", file, routeName, idx),
					Name:        routeName,
					Kind:        "http-route",
					Language:    "csharp",
					File:        file,
					StartLine:   startLine,
					StartColumn: startCol,
					EndLine:     startLine,
					EndColumn:   startCol + len(routeName),
					Signature:   strings.TrimSpace(signatureLine(content, starts, startLine)),
					Confidence:  ConfExact,
				})
				idx.AddCGPEdge(routeSym.ID, added.ID, "handles-route", ConfExact, Location{
					File: file, StartLine: startLine, StartColumn: startCol, EndLine: startLine, EndColumn: startCol + len(routeName),
					Kind: "http-route", Raw: routeName + " -> " + def.QualifiedName,
				})
			}
		}
	}

	populateVarTypes(idx, res.Vars, ranges)
}

// csharpBuiltinCallables holds .NET BCL/test-framework method names commonly
// called bare (no receiver) via `using static` or DSL-style assertion
// libraries. Bare calls to these never resolve to anything in the repo, so
// they are skipped rather than emitted as `unresolved:WriteLine`/etc. noise.
var csharpBuiltinCallables = map[string]bool{
	"WriteLine": true, "Write": true, "ReadLine": true, "ReadKey": true,
	"Format": true, "Parse": true, "TryParse": true, "Join": true, "Split": true,
	"ToString": true, "Equals": true, "GetHashCode": true, "Compare": true,
	"Max": true, "Min": true, "Abs": true, "Round": true, "Floor": true, "Ceiling": true,
	"Sqrt": true, "Pow": true,

	// xUnit/NUnit/MSTest/FluentAssertions/Moq DSL names.
	"AreEqual": true, "AreNotEqual": true, "IsTrue": true, "IsFalse": true,
	"IsNull": true, "IsNotNull": true, "Throws": true, "ThrowsAsync": true,
	"Should": true, "Be": true, "Equal": true, "NotNull": true,
	"Setup": true, "Returns": true, "Verify": true, "VerifyAll": true,
}

// csharpGlobalReceivers holds .NET BCL types/namespaces whose static methods
// (e.g. `Console.WriteLine`, `Math.Max`, `Convert.ToInt32`) never resolve to
// anything in the repo. Calls whose receiver's root identifier is one of
// these are skipped.
var csharpGlobalReceivers = map[string]bool{
	"Console": true, "Math": true, "String": true, "Convert": true,
	"Enumerable": true, "Task": true, "Environment": true, "DateTime": true,
	"TimeSpan": true, "Guid": true, "Path": true, "File": true, "Directory": true,
	"Regex": true, "JsonConvert": true,
	"Int32": true, "Int64": true, "Double": true, "Decimal": true, "Boolean": true,
	"Object": true, "Type": true, "Array": true, "List": true, "Dictionary": true,
}

// csharpReceiverRoot returns the leading identifier of a (possibly chained)
// receiver expression, e.g. "Console.Out" -> "Console".
func csharpReceiverRoot(receiver string) string {
	if dot := strings.IndexByte(receiver, '.'); dot >= 0 {
		return receiver[:dot]
	}
	return receiver
}

// csharpChainedBuiltinReceiver reports whether receiver is itself a call
// expression whose callee is a known builtin/DSL function, e.g.
// "Should()" (from `result.Should().Be(...)`). Such chains never resolve to
// repo symbols and are suppressed for the same reason as
// csharpBuiltinCallables.
func csharpChainedBuiltinReceiver(receiver string) bool {
	paren := strings.IndexByte(receiver, '(')
	if paren <= 0 || !strings.HasSuffix(receiver, ")") {
		return false
	}
	return csharpBuiltinCallables[receiver[:paren]]
}

// emitCSharpRelationsTS uses the tree-sitter C# grammar to emit import and
// call edges. `this.Foo()`/`base.Foo()` resolve to the enclosing class's (or
// its single base class's) method via resolveScopedCall/resolveSuperCall,
// mirroring Java's this/super handling.
func emitCSharpRelationsTS(idx *Index, file, content string) {
	res, err := treesitter.Parse("csharp", []byte(content))
	if err != nil {
		idx.markFileParseDiagnostics(file, []ScanDiagnostic{{Code: "treesitter_error", Message: err.Error()}})
		return
	}
	starts := lineStarts(content)

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

	emitImplementsEdges(idx, file, "csharp")

	idx.ensureFileSymbolIndex()
	for _, call := range res.Calls {
		if call.Callee == "" {
			continue
		}
		if call.Receiver == "" && csharpBuiltinCallables[call.Callee] {
			continue
		}
		if call.Receiver != "" && csharpGlobalReceivers[csharpReceiverRoot(call.Receiver)] {
			continue
		}
		if call.Receiver != "" && csharpChainedBuiltinReceiver(call.Receiver) {
			continue
		}
		callLine, callCol := offsetToLineCol(starts, call.Start)

		from := idx.containingSymbolFast(file, callLine)
		fromID := from.ID
		if fromID == "" {
			fromID = fileSymbolID(file)
		}

		var target, confidence, reason string
		switch call.Receiver {
		case "this":
			target, confidence, reason = resolveScopedCall(idx, file, from, call.Callee)
		case "base":
			target, confidence, reason = resolveSuperCall(idx, file, from, call.Callee)
		case "":
			target, confidence, reason = resolveScopedCall(idx, file, from, call.Callee)
		default:
			if t, c, ok := resolveVarCall(idx, from, call, "csharp"); ok {
				target, confidence = t, c
			} else {
				target, confidence, reason = resolveSymbolCall(idx, file, call.Receiver+"."+call.Callee)
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

// varScopeRange associates a function/method/class symbol's source range
// with its symbol ID, so populateVarTypes can map a variable declaration's
// byte offset to its enclosing scope.
type varScopeRange struct {
	start, end int
	id         string
}

// populateVarTypes records declared simple-type names for variables/fields
// (idx.varTypes), scoped to the innermost def in ranges containing each
// declaration's byte offset. Used by resolveJavaVarCall (Java
// fields/locals/parameters) and the Go receiver-call resolver
// (parameters, `var` declarations, and `x := &T{...}`/`x := T{...}`
// composite-literal short declarations).
func populateVarTypes(idx *Index, vars []treesitter.VarDecl, ranges []varScopeRange) {
	if len(vars) == 0 {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.varTypes == nil {
		idx.varTypes = map[string]map[string]string{}
	}
	for _, v := range vars {
		if v.Name == "" || v.Type == "" {
			continue
		}
		scopeID, bestStart := "", -1
		for _, r := range ranges {
			if r.start <= v.Pos && v.Pos < r.end && r.start > bestStart {
				bestStart, scopeID = r.start, r.id
			}
		}
		if scopeID == "" {
			continue
		}
		if idx.varTypes[scopeID] == nil {
			idx.varTypes[scopeID] = map[string]string{}
		}
		idx.varTypes[scopeID][v.Name] = v.Type
	}
}

// populatePythonVarTypes is populateVarTypes with Python's instance-field
// rule: `self.repo = Repo()` belongs to the enclosing class, not only to
// __init__, so calls through self.repo in any sibling method can use it.
func populatePythonVarTypes(idx *Index, vars []treesitter.VarDecl, ranges []varScopeRange) {
	if len(vars) == 0 {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.varTypes == nil {
		idx.varTypes = map[string]map[string]string{}
	}
	type scopedVar struct {
		decl                    treesitter.VarDecl
		lexicalScope, destScope string
		name                    string
	}
	bindings := make([]scopedVar, 0, len(vars))
	for _, v := range vars {
		if v.Name == "" || v.Type == "" && v.AliasOf == "" {
			continue
		}
		lexicalScope, bestStart := "", -1
		for _, r := range ranges {
			if r.start <= v.Pos && v.Pos < r.end && r.start > bestStart {
				bestStart, lexicalScope = r.start, r.id
			}
		}
		destScope := lexicalScope
		name := v.Name
		if strings.HasPrefix(name, "self.") {
			name = strings.TrimPrefix(name, "self.")
			for destScope != "" {
				sym, ok := idx.Symbols[destScope]
				if !ok {
					break
				}
				if sym.Kind == "class" {
					break
				}
				destScope = sym.ParentID
			}
		}
		if destScope == "" || name == "" {
			continue
		}
		bindings = append(bindings, scopedVar{decl: v, lexicalScope: lexicalScope, destScope: destScope, name: name})
		if v.Type != "" {
			if idx.varTypes[destScope] == nil {
				idx.varTypes[destScope] = map[string]string{}
			}
			idx.varTypes[destScope][name] = v.Type
		}
	}
	// Resolve aliases to a fixed point so `alias = repo` and
	// `self.repo = alias` work even when query capture order differs.
	for pass := 0; pass < len(bindings); pass++ {
		changed := false
		for _, binding := range bindings {
			if binding.decl.AliasOf == "" {
				continue
			}
			typ := lexicalVarTypeInScopeLocked(idx, binding.lexicalScope, binding.decl.AliasOf)
			if typ == "" || idx.varTypes[binding.destScope][binding.name] == typ {
				continue
			}
			if idx.varTypes[binding.destScope] == nil {
				idx.varTypes[binding.destScope] = map[string]string{}
			}
			idx.varTypes[binding.destScope][binding.name] = typ
			changed = true
		}
		if !changed {
			break
		}
	}
}

// populateSelfAttributeVarTypes is populatePythonVarTypes generalized to any
// language whose instance-attribute assignments use a single fixed,
// recognizable prefix on the VarDecl's Name (Ruby's "@" sigil, Lua's
// "self."/"this." convention, PHP's "$this->") instead of Python's
// "self." — the rule that an attribute set in one method (typically a
// constructor) belongs to the enclosing class, not just that one method,
// so a call through it from any sibling method can use the type. Kept
// separate from populatePythonVarTypes (used only by Python, untouched)
// rather than rewritten in terms of this one, to carry zero risk of
// regressing Python's already-proven behavior while generalizing it for
// every newly-onboarded language that needs the identical promotion rule.
func populateSelfAttributeVarTypes(idx *Index, vars []treesitter.VarDecl, ranges []varScopeRange, prefix string) {
	if len(vars) == 0 {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.varTypes == nil {
		idx.varTypes = map[string]map[string]string{}
	}
	type scopedVar struct {
		decl                    treesitter.VarDecl
		lexicalScope, destScope string
		name                    string
	}
	bindings := make([]scopedVar, 0, len(vars))
	for _, v := range vars {
		if v.Name == "" || v.Type == "" && v.AliasOf == "" {
			continue
		}
		lexicalScope, bestStart := "", -1
		for _, r := range ranges {
			if r.start <= v.Pos && v.Pos < r.end && r.start > bestStart {
				bestStart, lexicalScope = r.start, r.id
			}
		}
		destScope := lexicalScope
		name := v.Name
		if strings.HasPrefix(name, prefix) {
			name = strings.TrimPrefix(name, prefix)
			for destScope != "" {
				sym, ok := idx.Symbols[destScope]
				if !ok {
					break
				}
				if sym.Kind == "class" {
					break
				}
				destScope = sym.ParentID
			}
		}
		if destScope == "" || name == "" {
			continue
		}
		bindings = append(bindings, scopedVar{decl: v, lexicalScope: lexicalScope, destScope: destScope, name: name})
		if v.Type != "" {
			if idx.varTypes[destScope] == nil {
				idx.varTypes[destScope] = map[string]string{}
			}
			idx.varTypes[destScope][name] = v.Type
		}
	}
	// Resolve aliases to a fixed point so `alias = repo` and
	// `self.repo = alias` work even when query capture order differs.
	for pass := 0; pass < len(bindings); pass++ {
		changed := false
		for _, binding := range bindings {
			if binding.decl.AliasOf == "" {
				continue
			}
			typ := lexicalVarTypeInScopeLocked(idx, binding.lexicalScope, binding.decl.AliasOf)
			if typ == "" || idx.varTypes[binding.destScope][binding.name] == typ {
				continue
			}
			if idx.varTypes[binding.destScope] == nil {
				idx.varTypes[binding.destScope] = map[string]string{}
			}
			idx.varTypes[binding.destScope][binding.name] = typ
			changed = true
		}
		if !changed {
			break
		}
	}
}

// populateLuaSelfAttributeVarTypes is populateSelfAttributeVarTypes's
// promotion rule adapted for Lua, which has no real "class" symbol to walk
// up to at all (every Lua method is parented to its file, not to a
// synthetic table symbol — see emitGenericSymbolsTS). Instead, a
// "self.repo = Repo.new()" assignment is promoted to a scope synthesized
// from the assigning method's own colon/dot-call receiver table name
// (idx.luaReceiverTypeBySymbol, e.g. "Service" for `function Service:load()`)
// plus its file, so every method sharing that same table name in that file
// — not just the one that happened to set the attribute — can resolve a
// call through it. Skips (records nothing for) an assignment whose lexical
// scope isn't itself a Lua method with a known receiver table, since there
// is then no table identity to scope the binding to.
func populateLuaSelfAttributeVarTypes(idx *Index, file string, vars []treesitter.VarDecl, ranges []varScopeRange) {
	if len(vars) == 0 {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.varTypes == nil {
		idx.varTypes = map[string]map[string]string{}
	}
	for _, v := range vars {
		if v.Name == "" || v.Type == "" {
			continue
		}
		lexicalScope, bestStart := "", -1
		for _, r := range ranges {
			if r.start <= v.Pos && v.Pos < r.end && r.start > bestStart {
				bestStart, lexicalScope = r.start, r.id
			}
		}
		if lexicalScope == "" {
			continue
		}
		if !strings.HasPrefix(v.Name, "self.") {
			if idx.varTypes[lexicalScope] == nil {
				idx.varTypes[lexicalScope] = map[string]string{}
			}
			idx.varTypes[lexicalScope][v.Name] = v.Type
			continue
		}
		name := strings.TrimPrefix(v.Name, "self.")
		table := idx.luaReceiverTypeBySymbol[lexicalScope]
		if table == "" || name == "" {
			continue
		}
		destScope := "luatable:" + file + ":" + table
		if idx.varTypes[destScope] == nil {
			idx.varTypes[destScope] = map[string]string{}
		}
		idx.varTypes[destScope][name] = v.Type
	}
}

func lexicalVarTypeInScopeLocked(idx *Index, scopeID, name string) string {
	for scopeID != "" {
		if typ := idx.varTypes[scopeID][name]; typ != "" {
			return typ
		}
		sym, ok := idx.Symbols[scopeID]
		if !ok {
			break
		}
		scopeID = sym.ParentID
	}
	return ""
}

// goReceiver records a Go method's receiver variable name and simple
// (pointer-stripped) receiver type, as declared on the method itself, e.g.
// `func (s *Service) Foo()` -> {Name: "s", Type: "Service"}. Used to resolve
// same-receiver calls inside the method body (`s.Bar()`).
type goReceiver struct {
	Name string
	Type string
}

// emitGoSymbolsTS uses the tree-sitter Go grammar to emit CGPSymbols for
// top-level functions, struct/interface type declarations, and methods.
//
// Go methods are not lexically nested inside their receiver type's
// declaration (unlike Python/Java class methods), so defs are emitted in two
// passes: non-methods first (recording struct/interface symbol IDs by name),
// then methods, whose ParentID is set to the same-file receiver-type symbol
// when found (falling back to the file symbol otherwise). Regardless of
// same-file linkage, every method with a named receiver type is recorded in
// idx.goMethodsByReceiverType (global, cross-file) and idx.goReceivers, which
// emitGoRelationsTS uses to resolve `recv.Method()` calls.
func emitGoSymbolsTS(idx *Index, file, content, parentID string) {
	res, err := treesitter.Parse("go", []byte(content))
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
			ID:          stableSymbolID("go", def.Kind, file, def.QualifiedName, idx),
			Name:        def.Name,
			Kind:        def.Kind,
			Language:    "go",
			File:        file,
			StartLine:   startLine,
			StartColumn: startCol,
			EndLine:     endLine,
			EndColumn:   endCol,
			Signature:   collapseJavaSignature(content, def.Start, def.End),
			Docstring:   extractSymbolDocstring(lines, startLine, "go"),
			ReturnTypes: append([]string(nil), def.ReturnTypes...),
			Exported:    def.Exported,
			ParentID:    parentID,
			Confidence:  ConfExact,
		})

		if def.Kind == "class" || def.Kind == "interface" {
			typeIDByName[def.Name] = added.ID
		}
		if def.Kind == "class" && len(def.Bases) > 0 {
			idx.mu.Lock()
			if idx.classBases == nil {
				idx.classBases = map[string][]string{}
			}
			idx.classBases[added.ID] = def.Bases
			idx.mu.Unlock()
		}
		if def.Kind == "class" {
			// A struct's own field declarations (`repo Repo` inside its
			// body) need a scope range too, the same role this already
			// plays for "function" below — without it, populateVarTypes
			// has nowhere to attribute a field's VarDecl to, and it's
			// silently dropped rather than scoped to the struct.
			ranges = append(ranges, varScopeRange{start: def.Start, end: def.End, id: added.ID})
		}
		if def.Kind == "function" {
			ranges = append(ranges, varScopeRange{start: def.Start, end: def.End, id: added.ID})
			if len(def.ReturnTypes) > 0 {
				idx.mu.Lock()
				if idx.goReturnTypes == nil {
					idx.goReturnTypes = map[string][]string{}
				}
				idx.goReturnTypes[added.ID] = def.ReturnTypes
				idx.mu.Unlock()
			}
		}
	}

	for _, def := range methodDefs {
		startLine, startCol := offsetToLineCol(starts, def.Start)
		endLine, endCol := offsetToLineCol(starts, clampOffset(content, def.End))

		parent := parentID
		redirectMissed := false
		if pid, ok := typeIDByName[def.ReceiverType]; ok {
			parent = pid
		} else if def.ReceiverType != "" {
			// Go methods are never lexically nested inside their receiver
			// type's declaration, so the only way to find the real struct
			// is by name — and typeIDByName here only knows about *this
			// file's* own type defs. A receiver type declared in a
			// different file (a real, common Go pattern: a struct in one
			// file, its methods split across several others) misses here
			// every time, for the same reason — and with the same fix —
			// as the C/C++/Rust case resolveOutOfLineMethodParents was
			// built for: BuildIndex's symbol-extraction phase scans every
			// file in parallel with no defined order, so the declaring
			// file may simply not have been scanned yet. Recorded here so
			// that same fix-up pass retries it once every file is done,
			// instead of leaving the method permanently parented to its
			// file. Found via real-world testing of that exact case: a
			// struct's methods split across files left every one of them
			// unreachable via findMethodInClass, the same failure mode
			// the C++ fix targeted.
			redirectMissed = true
		}

		added := idx.AddCGPSymbol(CGPSymbol{
			ID:          stableSymbolID("go", "method", file, def.QualifiedName, idx),
			Name:        def.Name,
			Kind:        "method",
			Language:    "go",
			File:        file,
			StartLine:   startLine,
			StartColumn: startCol,
			EndLine:     endLine,
			EndColumn:   endCol,
			Signature:   collapseJavaSignature(content, def.Start, def.End),
			Docstring:   extractSymbolDocstring(lines, startLine, "go"),
			ReturnTypes: append([]string(nil), def.ReturnTypes...),
			Exported:    def.Exported,
			ParentID:    parent,
			Confidence:  ConfExact,
		})

		if redirectMissed {
			idx.mu.Lock()
			if idx.unresolvedMethodParents == nil {
				idx.unresolvedMethodParents = map[string]string{}
			}
			idx.unresolvedMethodParents[added.ID] = def.ReceiverType
			idx.mu.Unlock()
		}

		if def.ReceiverType != "" {
			idx.mu.Lock()
			if idx.goMethodsByReceiverType == nil {
				idx.goMethodsByReceiverType = map[string]map[string]string{}
			}
			if idx.goMethodsByReceiverType[def.ReceiverType] == nil {
				idx.goMethodsByReceiverType[def.ReceiverType] = map[string]string{}
			}
			idx.goMethodsByReceiverType[def.ReceiverType][def.Name] = added.ID
			if idx.goReceivers == nil {
				idx.goReceivers = map[string]goReceiver{}
			}
			idx.goReceivers[added.ID] = goReceiver{Name: def.ReceiverName, Type: def.ReceiverType}
			idx.mu.Unlock()
		}

		if len(def.ReturnTypes) > 0 {
			idx.mu.Lock()
			if idx.goReturnTypes == nil {
				idx.goReturnTypes = map[string][]string{}
			}
			idx.goReturnTypes[added.ID] = def.ReturnTypes
			idx.mu.Unlock()
		}

		ranges = append(ranges, varScopeRange{start: def.Start, end: def.End, id: added.ID})
	}

	populateVarTypes(idx, markExternalGoVarTypes(idx, res.Imports, res.Vars), ranges)
}

// collapseJavaSignature collapses a possibly multi-line class/method header
// (through its trailing top-level '{' or ';') into a single line, for use as
// a symbol's Signature field.
func collapseJavaSignature(content string, start, end int) string {
	end = clampOffset(content, end)
	if start < 0 || start >= end {
		return ""
	}
	region := content[start:end]
	masked := maskCStyleStringsAndComments(region)

	headerEnd := len(region)
	depth := 0
loop:
	for i := 0; i < len(masked); i++ {
		switch masked[i] {
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case '{', ';':
			if depth == 0 {
				headerEnd = i + 1
				break loop
			}
		}
	}

	return strings.Join(strings.Fields(region[:headerEnd]), " ")
}

// maskCStyleStringsAndComments blanks out the contents of "..."/'...'
// literals and //.../ /*...*/ comments in src, preserving length and
// newlines, so a brace/paren-depth scan over the result ignores braces or
// semicolons that appear inside strings or comments.
func maskCStyleStringsAndComments(src string) string {
	if src == "" {
		return src
	}
	buf := []byte(src)
	i := 0
	for i < len(buf) {
		switch {
		case buf[i] == '/' && i+1 < len(buf) && buf[i+1] == '/':
			for i < len(buf) && buf[i] != '\n' {
				buf[i] = ' '
				i++
			}
		case buf[i] == '/' && i+1 < len(buf) && buf[i+1] == '*':
			buf[i] = ' '
			buf[i+1] = ' '
			i += 2
			for i+1 < len(buf) && !(buf[i] == '*' && buf[i+1] == '/') {
				if buf[i] != '\n' {
					buf[i] = ' '
				}
				i++
			}
			if i+1 < len(buf) {
				buf[i] = ' '
				buf[i+1] = ' '
				i += 2
			}
		case buf[i] == '"' || buf[i] == '\'':
			quote := buf[i]
			buf[i] = ' '
			i++
			for i < len(buf) && buf[i] != quote {
				if buf[i] == '\\' && i+1 < len(buf) {
					buf[i] = ' '
					if buf[i+1] != '\n' {
						buf[i+1] = ' '
					}
					i += 2
					continue
				}
				if buf[i] != '\n' {
					buf[i] = ' '
				}
				i++
			}
			if i < len(buf) {
				buf[i] = ' '
				i++
			}
		default:
			i++
		}
	}
	return string(buf)
}

func emitJSTSRelations(idx *Index, file, body string, baseOffset int, fullContent string) {
	fileStarts := lineStarts(fullContent)
	res := ParseJS(body)

	// Imports.
	for _, imp := range res.Imports {
		startLine, startCol := offsetToLineCol(fileStarts, baseOffset+imp.Start)
		idx.AddCGPEdge(
			fileSymbolID(file),
			"module:"+imp.Spec,
			"imports",
			"exact",
			Location{
				File: file, StartLine: startLine, StartColumn: startCol,
				EndLine: startLine, EndColumn: startCol + len(imp.Spec),
				Kind: "import", Raw: imp.Spec,
			},
		)
	}

	// Dynamic imports — `() => import('@/components/X.vue')`. Typically a
	// lazy-loaded Vue Router route component that is never rendered via a tag,
	// so without this edge every route-only component looks dead. Scanned across the whole body because
	// these nest deep inside route tables. The edge lands on the imported
	// component (or file) so dead-code sees it used.
	for _, di := range scanJSDynamicImports(body) {
		targetFile := resolveImportSpecToIndexedFile(idx, file, di.Spec)
		if targetFile == "" {
			continue
		}
		toID := idx.componentSymbolForFile(targetFile).ID
		if toID == "" {
			toID = fileSymbolID(targetFile)
		}
		line, col := offsetToLineCol(fileStarts, baseOffset+di.Start)
		from := idx.containingSymbolFast(file, line)
		fromID := from.ID
		if fromID == "" {
			fromID = fileSymbolID(file)
		}
		if fromID == toID {
			continue
		}
		idx.AddCGPEdge(fromID, toID, "references-symbol", "exact", Location{
			File: file, StartLine: line, StartColumn: col,
			EndLine: line, EndColumn: col + len(di.Spec),
			Kind: "dynamic-import", Raw: di.Spec,
		})
	}

	// Calls.
	idx.ensureFileSymbolIndex()
	requireBindings := jsRequireBindingFiles(idx, file, res.Imports)
	importedBindings := jsImportBindingTargets(idx, file, res.Imports)
	localReturnAssignments := jsReturnFileInference(idx, file, body, baseOffset, fullContent, requireBindings)
	localReturnAssignments = append(localReturnAssignments, jsTypedBindingInference(idx, file, body, baseOffset, fullContent, requireBindings)...)
	for _, call := range res.Calls {
		root := call.Callee
		if dot := strings.IndexByte(root, '.'); dot >= 0 {
			root = root[:dot]
		}
		if callStopWords[root] && !(root == "super" && strings.Contains(call.Callee, ".")) {
			continue
		}
		if call.Callee == root && jsBuiltinCallables[call.Callee] {
			continue
		}
		if call.Callee != root && jsGlobalReceivers[root] {
			continue
		}
		callLine, callCol := offsetToLineCol(fileStarts, baseOffset+call.Start)
		from := idx.containingSymbolFast(file, callLine)
		fromID := from.ID
		if fromID == "" {
			// A call at module scope (not inside any function) — e.g. an
			// entry-point `runMigration();` at the bottom of a script, or a
			// top-level `registerRoutes(app)`. It has no enclosing function
			// symbol, so attribute it to the file, matching the value-ref and
			// generic-language paths. Without this the call produced no edge
			// at all, so its target looked unreferenced — every script
			// entry-point invocation was a dead-code false positive.
			fromID = fileSymbolID(file)
		}
		// Skip self-recursive declaration site (rare false positive).
		if from.ID != "" && from.Name == root && from.StartLine == callLine {
			continue
		}
		var target, confidence, reason string
		// A bare imported constructor is otherwise shadowed by the local
		// import-binding constant (`const ErrorType = require(...)`), causing
		// `new ErrorType()` to point at that constant instead of the class in
		// the imported file. Resolve construction against the known import
		// target first so the graph records actual class usage.
		if call.Constructor && root == call.Callee {
			if targetFile, ok := requireBindings[root]; ok {
				target, confidence, reason, _ = resolveImportBoundConstructor(idx, targetFile, root)
			}
		}
		// Bare call on an import-bound local name (`useAuth()` after
		// `import { useAuth } from '@/composables/useAuth'`): the import
		// statement pins exactly which file declares the callee, so resolve
		// inside that file before any repo-wide name search — the same
		// binding-first rule value references already follow. Without this,
		// a callee name declared in 2+ files in a mirrored-app monorepo fell through to
		// resolveSymbolCall and was reported ambiguous_name with zero
		// resolved callers despite the import naming its file. Not-found here
		// still falls through: a re-export barrel's real declaration lives in
		// another file, and the repo-wide search keeps that recall.
		if target == "" && root == call.Callee && !call.Constructor {
			if binding, ok := importedBindings[call.Callee]; ok {
				if t, c, r, found := resolveImportBoundBareCall(idx, binding); found {
					target, confidence, reason = t, c, r
				}
			}
		}
		if root != call.Callee {
			method := call.Callee
			receiver := root
			if li := strings.LastIndexByte(call.Callee, '.'); li >= 0 {
				method = call.Callee[li+1:]
				receiver = call.Callee[:li]
			}
			// Same-class lookup applies only to single-hop receivers
			// (`this.method()` / `super.method()`). A deeper chain like
			// `this.mlService.categorizeTransactions()` invokes a method on a
			// *member object*, and matching its bare name against the
			// enclosing class produced a confident-but-wrong scoped edge
			// whenever the class happened to define a same-named method. Deep
			// this-chains fall through to the field-type lookup
			// below, which resolves against the member's actual class.
			switch {
			case root == "this" && receiver == "this":
				target, confidence, reason = resolveScopedCall(idx, file, from, method)
			case root == "super" && receiver == "super":
				target, confidence, reason = resolveSuperCall(idx, file, from, method)
			}
			// An unresolved this/super lookup is not a final answer: a longer
			// receiver such as `this.repo.find()` may have an explicit field
			// type that resolves below. Preserve only confident scoped results.
			if confidence == ConfUnresolved {
				target, confidence, reason = "", "", ""
			}
			// Dotted call on a local assigned from a method whose return value
			// we traced to an import/require-bound singleton:
			//   const h = this.getEmailHandler()
			//   await h.sendEmail(...)
			// This resolves the target without guessing from the receiver name.
			if target == "" {
				targetFile := jsReceiverTargetFile(idx, localReturnAssignments, from, receiver, call.Start)
				if targetFile != "" {
					if t, c, r, found := resolveImportBoundCall(idx, targetFile, method); found {
						target, confidence, reason = t, c, r
					}
				}
			}
			// `this.repo.find()` where `this.repo` was assigned a same-file
			// `new Repo()` (jsThisFieldNewAssignment, idx.varTypes) rather
			// than imported/required from elsewhere — the JS analogue of
			// every tree-sitter language's resolveVarCall self-attribute
			// lookup.
			if target == "" && strings.HasPrefix(receiver, "this.") {
				if classID := enclosingClassID(idx, from); classID != "" {
					if typeName, found := idx.lookupVarType(classID, strings.TrimPrefix(receiver, "this.")); found && typeName != "" {
						if fieldClassID := findClassByName(idx, typeName, file, idx.languageFor(file)); fieldClassID != "" {
							if id := findMethodInClass(idx, fieldClassID, method); id != "" {
								target, confidence, reason = id, ConfScoped, ""
							}
						}
					}
				}
			}
			// Dotted call (`obj.method`). If `obj` is a local bound to a
			// require()'d/imported file (singleton instance, namespace
			// object, etc.), resolve the method directly within that file
			// instead of falling through to a repo-wide bare-name search —
			// this disambiguates same-named methods across files even when
			// resolveSymbolCall alone would have to give up.
			if target == "" {
				if targetFile, ok := requireBindings[root]; ok {
					if t, c, r, found := resolveImportBoundCall(idx, targetFile, method); found {
						target, confidence, reason = t, c, r
					} else if !jsBuiltinCallables[method] {
						// The receiver is import-bound to targetFile and no
						// symbol named `method` exists there: the real target
						// is outside the graph (an external-lib instance the
						// module exports — pino, EventEmitter — a re-export,
						// or a dynamic attach). Falling through to a repo-wide
						// bare-name match would contradict this direct import
						// evidence; sampled fall-throughs instead matched
						// unrelated same-named methods. Report honestly.
						// Builtin-named methods (`mod.keys()`) keep their
						// existing silent skip below — prototype calls on
						// module-derived values are not graph evidence.
						target = "unresolved:" + call.Callee
						confidence = ConfUnresolved
						reason = ReasonDynamicReceiver
					}
				}
			}
			// If an unknown receiver calls a common JS prototype method, do
			// not fall through to a repo-wide bare-name match. `items.join()`
			// should not resolve to some unrelated local helper named `join`.
			if target == "" && jsBuiltinCallables[method] {
				continue
			}
		}
		if target == "" {
			target, confidence, reason = resolveSymbolCall(idx, file, call.Callee)
		}
		idx.AddCGPEdgeWithReason(
			fromID, target, "calls", confidence, reason,
			Location{
				File: file, StartLine: callLine, StartColumn: callCol,
				EndLine: callLine, EndColumn: callCol + len(call.Callee),
				Kind: "call", Raw: call.Callee,
			},
		)
		// `new X()` executes X's constructor. The class-targeted edge above
		// preserves the long-standing "who uses class X" semantics; this
		// additional edge gives the constructor method itself the inbound
		// call it actually receives, so test-reachability and dead-code see
		// `new ReconciliationCheckService()` in a test as reaching the
		// constructor (both were blind to it in the 2026-07-02 audit).
		if call.Constructor && confidence != ConfUnresolved {
			if ctorID := constructorMethodForClass(idx, target); ctorID != "" {
				idx.AddCGPEdge(fromID, ctorID, "calls", confidence, Location{
					File: file, StartLine: callLine, StartColumn: callCol,
					EndLine: callLine, EndColumn: callCol + len(call.Callee),
					Kind: "constructor-call", Raw: "new " + call.Callee,
				})
			}
		}
	}

	// Value references — identifiers used as values rather than called
	// (route handlers registered with router.post, job-config functions,
	// event-listener callbacks, composable returns). Emitted as
	// references-symbol edges so dead-code sees the usage; execution-centric
	// consumers (impact, hot paths, test closure) filter on "calls" and are
	// unaffected.
	emitJSValueRefEdges(idx, file, body, baseOffset, fullContent, res)

	// Phase 4 — overlay event-flow edges (`emit` / `on` / `once` / `off`)
	// against the same containingSymbolFast attribution. Run after call
	// edges so the symbol index is warm and from-attribution has settled.
	emitEventEdges(idx, file, body, fileStarts, baseOffset)
}

// constructorMethodForClass returns the ID of classID's explicit
// `constructor` method, or "" when classID is not a class symbol or declares
// no constructor of its own.
func constructorMethodForClass(idx *Index, classID string) string {
	idx.mu.Lock()
	sym, ok := idx.Symbols[classID]
	idx.mu.Unlock()
	if !ok || sym.Kind != "class" {
		return ""
	}
	return findMethodInClass(idx, classID, "constructor")
}

func emitVueTemplateRelations(idx *Index, file, content string) {
	from := idx.componentSymbolForFile(file)
	if from.ID == "" {
		from = idx.containingSymbolFast(file, 1)
	}
	if from.ID == "" {
		return
	}

	starts := lineStarts(content)
	for _, block := range vueTemplateRanges(content) {
		bodyStart := block.start
		body := content[block.start:block.end]
		masked := maskHTMLComments(body)
		emitVueTemplateComponentRelations(idx, file, from.ID, body, masked, bodyStart, starts, content)
		emitVueTemplateAttributeCalls(idx, file, from.ID, body, masked, bodyStart, starts, vueEventBindingRe)
		emitVueTemplateAttributeCalls(idx, file, from.ID, body, masked, bodyStart, starts, vueExpressionBindingRe)
		emitVueTemplateAttributeCalls(idx, file, from.ID, body, masked, bodyStart, starts, vueStructuralDirectiveRe)
		emitVueTemplateInterpolationCalls(idx, file, from.ID, body, masked, bodyStart, starts)
	}
}

func emitVueTemplateComponentRelations(idx *Index, file, fromID, body, masked string, bodyStart int, starts []int, fullContent string) {
	components := vueComponentImports(idx, file, fullContent)
	for _, m := range vueStartTagRe.FindAllStringSubmatchIndex(masked, -1) {
		if len(m) < 6 || m[2] < 0 {
			continue
		}
		tag := body[m[2]:m[3]]
		if !looksLikeVueComponentTag(tag) {
			continue
		}
		child := resolveVueComponentTag(idx, tag, components)
		if child.ID == "" {
			continue
		}
		line, col := offsetToLineCol(starts, bodyStart+m[2])
		idx.AddCGPEdge(fromID, child.ID, "renders-component", ConfExact, Location{
			File: file, StartLine: line, StartColumn: col,
			EndLine: line, EndColumn: col + len(tag),
			Kind: "component-usage", Raw: tag,
		})
		if len(m) >= 6 && m[4] >= 0 {
			attrs := body[m[4]:m[5]]
			emitVueTemplatePropRelations(idx, file, fromID, child, attrs, bodyStart+m[4], starts)
		}
	}
}

func vueComponentImports(idx *Index, file, content string) map[string]CGPSymbol {
	out := map[string]CGPSymbol{}
	for _, block := range scriptBlockRe.FindAllStringSubmatchIndex(content, -1) {
		body := content[block[2]:block[3]]
		res := ParseJS(body)
		for _, imp := range res.Imports {
			targetFile := resolveImportSpecToIndexedFile(idx, file, imp.Spec)
			if targetFile == "" {
				continue
			}
			component := idx.componentSymbolForFile(targetFile)
			if component.ID == "" {
				continue
			}
			for _, binding := range imp.Bindings {
				if binding.Local == "" {
					continue
				}
				out[binding.Local] = component
				out[kebabCase(binding.Local)] = component
			}
		}
	}
	return out
}

func resolveImportSpecToIndexedFile(idx *Index, file, spec string) string {
	if spec == "" {
		return ""
	}
	var bases []string
	switch {
	case strings.HasPrefix(spec, "."):
		bases = []string{filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(file), spec)))}
	default:
		// Config-declared aliases (tsconfig/jsconfig paths, vite alias) take
		// precedence — they are authoritative for this project's layout.
		bases = idx.resolveAliasBases(file, spec)
		if strings.HasPrefix(spec, "@/") {
			// Fall back to the structural `@`→enclosing-src heuristic (and
			// repo-root) for the near-universal convention when no config
			// rule matched.
			bases = append(bases, aliasImportBases(file, strings.TrimPrefix(spec, "@/"))...)
		}
		if len(bases) == 0 {
			return ""
		}
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for _, base := range bases {
		for _, cand := range importFileCandidates(base) {
			if _, ok := idx.Files[cand]; ok {
				return cand
			}
		}
	}
	return ""
}

// aliasImportBases resolves the `@/` path alias (Vite/webpack convention)
// against the importing file's own project root. By near-universal Vue/Vite
// convention `@` maps to the app's `src` directory — but in a monorepo that
// may be `<app>/src`, not the repo root, so the
// alias base depends on which app the importing file lives in. Rather than
// parse each app's vite.config alias, we derive it structurally: the alias
// target is the importing file's nearest enclosing `src` directory (where
// `@/`-imported files always live). The repo-root-relative interpretation is
// kept as a fallback for single-app repos where `@` maps to the root.
//
// Before this, every `@/` import in a `<app>/src` layout silently failed to
// resolve, so cross-file frontend edges and route-component liveness were lost.
func aliasImportBases(file, rest string) []string {
	rest = filepath.ToSlash(filepath.Clean(rest))
	var bases []string
	parts := strings.Split(filepath.ToSlash(file), "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] == "src" {
			bases = append(bases, filepath.ToSlash(filepath.Clean(strings.Join(parts[:i+1], "/")+"/"+rest)))
			break
		}
	}
	bases = append(bases, rest) // repo-root fallback (single-app `@`=root)
	return bases
}

func importFileCandidates(base string) []string {
	ext := filepath.Ext(base)
	var out []string
	if ext != "" {
		out = append(out, base)
	} else {
		for _, suffix := range []string{".vue", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"} {
			out = append(out, base+suffix)
		}
		for _, name := range []string{"index.vue", "index.ts", "index.tsx", "index.js", "index.jsx"} {
			out = append(out, filepath.ToSlash(filepath.Join(base, name)))
		}
	}
	return out
}

func resolveVueComponentTag(idx *Index, tag string, imports map[string]CGPSymbol) CGPSymbol {
	if sym, ok := imports[tag]; ok {
		return sym
	}
	if sym, ok := imports[kebabCase(tag)]; ok {
		return sym
	}
	matches := findSymbols(idx, tag)
	var found CGPSymbol
	for _, sym := range matches {
		if sym.Kind != "component" {
			continue
		}
		if found.ID != "" {
			return CGPSymbol{}
		}
		found = sym
	}
	return found
}

func looksLikeVueComponentTag(tag string) bool {
	if tag == "" {
		return false
	}
	return tag[0] >= 'A' && tag[0] <= 'Z' || strings.Contains(tag, "-")
}

func emitVueTemplatePropRelations(idx *Index, file, fromID string, child CGPSymbol, attrs string, attrsOffset int, starts []int) {
	for _, m := range vueAttrRe.FindAllStringSubmatchIndex(attrs, -1) {
		if len(m) < 4 || m[2] < 0 {
			continue
		}
		rawName := attrs[m[2]:m[3]]
		propName, edgeType, ok := vueAttrSemantic(rawName)
		if !ok {
			continue
		}
		target := child.ID
		if api := findVueAPISymbolForComponent(idx, child, propName, edgeType); api.ID != "" {
			target = api.ID
		}
		line, col := offsetToLineCol(starts, attrsOffset+m[2])
		idx.AddCGPEdge(fromID, target, edgeType, ConfExact, Location{
			File: file, StartLine: line, StartColumn: col,
			EndLine: line, EndColumn: col + len(rawName),
			Kind: edgeType, Raw: child.Name + "." + propName,
		})
	}
}

func vueAttrSemantic(raw string) (name, edgeType string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "class" || raw == ":class" || raw == "style" || raw == ":style" || raw == "key" || raw == ":key" {
		return "", "", false
	}
	if strings.HasPrefix(raw, "v-model") {
		name = "modelValue"
		if idx := strings.IndexByte(raw, ':'); idx >= 0 {
			name = raw[idx+1:]
		}
		return normalizeVuePublicName(name), "binds-model", true
	}
	if strings.HasPrefix(raw, "@") || strings.HasPrefix(raw, "v-on:") {
		name = strings.TrimPrefix(strings.TrimPrefix(raw, "@"), "v-on:")
		if dot := strings.IndexByte(name, '.'); dot >= 0 {
			name = name[:dot]
		}
		return normalizeVueEventName(name), "listens-event", true
	}
	if strings.HasPrefix(raw, ":") {
		return normalizeVuePublicName(strings.TrimPrefix(raw, ":")), "passes-prop", true
	}
	if strings.HasPrefix(raw, "v-bind:") {
		return normalizeVuePublicName(strings.TrimPrefix(raw, "v-bind:")), "passes-prop", true
	}
	if strings.HasPrefix(raw, "v-") || strings.HasPrefix(raw, "#") {
		return "", "", false
	}
	return normalizeVuePublicName(raw), "passes-prop", true
}

func findVueAPISymbolForComponent(idx *Index, component CGPSymbol, publicName, edgeType string) CGPSymbol {
	wantKind := "vue-prop"
	if edgeType == "binds-model" {
		wantKind = "vue-model"
	} else if edgeType == "listens-event" {
		wantKind = "vue-emit"
	}
	aliases := map[string]bool{publicName: true, normalizeVuePublicName(publicName): true, kebabCase(publicName): true}
	idx.ensureFileSymbolIndex()
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for _, sym := range idx.childrenByParent[component.ID] {
		if sym.Kind != wantKind {
			continue
		}
		if aliases[sym.Name] || aliases[normalizeVuePublicName(sym.Name)] || aliases[kebabCase(sym.Name)] {
			return sym
		}
	}
	return CGPSymbol{}
}

func normalizeVuePublicName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	parts := strings.Split(name, "-")
	if len(parts) == 1 {
		return name
	}
	var b strings.Builder
	b.WriteString(parts[0])
	for _, part := range parts[1:] {
		if part == "" {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]))
		if len(part) > 1 {
			b.WriteString(part[1:])
		}
	}
	return b.String()
}

func normalizeVueEventName(name string) string {
	if strings.HasPrefix(name, "update:") {
		return "update:" + normalizeVuePublicName(strings.TrimPrefix(name, "update:"))
	}
	return name
}

func kebabCase(name string) string {
	var b strings.Builder
	for i, r := range name {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func emitVueTemplateAttributeCalls(idx *Index, file, fromID, body, masked string, bodyStart int, starts []int, re *regexp.Regexp) {
	for _, m := range re.FindAllStringSubmatchIndex(masked, -1) {
		valueStart, valueEnd := quotedAttrValueRange(m)
		if valueStart < 0 {
			continue
		}
		emitVueTemplateExpressionAtRange(idx, file, fromID, body, bodyStart, starts, valueStart, valueEnd)
	}
}

func quotedAttrValueRange(match []int) (int, int) {
	for group := 4; group < len(match); group += 2 {
		if match[group] >= 0 {
			return match[group], match[group+1]
		}
	}
	return -1, -1
}

func emitVueTemplateInterpolationCalls(idx *Index, file, fromID, body, masked string, bodyStart int, starts []int) {
	for _, m := range vueInterpolationRe.FindAllStringSubmatchIndex(masked, -1) {
		if len(m) < 4 || m[2] < 0 {
			continue
		}
		emitVueTemplateExpressionAtRange(idx, file, fromID, body, bodyStart, starts, m[2], m[3])
	}
}

func emitVueTemplateExpressionAtRange(idx *Index, file, fromID, body string, bodyStart int, starts []int, valueStart, valueEnd int) {
	raw := body[valueStart:valueEnd]
	trimmed := strings.TrimLeft(raw, " \t\r\n")
	expr := strings.TrimSpace(html.UnescapeString(raw))
	if expr == "" {
		return
	}
	offset := bodyStart + valueStart + strings.Index(raw, trimmed)
	if offset < bodyStart+valueStart {
		offset = bodyStart + valueStart
	}
	emitVueTemplateExpressionCalls(idx, file, fromID, expr, starts, offset)
}

func emitVueTemplateExpressionCalls(idx *Index, file, fromID, expr string, starts []int, offset int) {
	calls := ParseJS(expr).Calls
	if vueBareHandlerRe.MatchString(expr) {
		calls = append(calls, ScannedCall{Callee: expr, Start: 0, End: len(expr)})
	}
	for _, call := range calls {
		callee := call.Callee
		root := callee
		if dot := strings.IndexByte(root, '.'); dot >= 0 {
			root = root[:dot]
		}
		if callStopWords[root] && !(root == "super" && strings.Contains(callee, ".")) {
			continue
		}
		if callee == root && jsBuiltinCallables[callee] {
			continue
		}
		if callee != root && jsGlobalReceivers[root] {
			continue
		}
		target, confidence, reason := resolveSymbolCall(idx, file, callee)
		line, col := offsetToLineCol(starts, offset+call.Start)
		idx.AddCGPEdgeWithReason(
			fromID, target, "calls", confidence, reason,
			Location{
				File: file, StartLine: line, StartColumn: col,
				EndLine: line, EndColumn: col + len(callee),
				Kind: "call", Raw: callee,
			},
		)
	}
	// Identifiers *read* by the expression (`v-if="canEdit && isLocked"`)
	// reference script members without calling them — record those too, or
	// script state used only by the template looks unreferenced.
	jsValueRefsInExpression(idx, file, fromID, expr, starts, offset)
}

// emitPythonRelationsTS uses the tree-sitter Python grammar to emit import
// and call edges. Imports cover plain/aliased/relative forms (including
// `from .models import User` and `from . import X`, which the old regex
// path silently dropped). Calls resolve `self.foo()`/`cls.foo()` to the
// method of the enclosing class when one exists (ConfScoped), falling back
// to the same name-based resolution as other languages otherwise.
// Decorator-call expressions (e.g. `@app.route("/x")`) are attributed to the
// file/module scope, since decorators run at decoration time rather than as
// part of the decorated function's body.
func emitPythonRelationsTS(idx *Index, file, content string) {
	res, err := treesitter.Parse("python", []byte(content))
	if err != nil {
		idx.markFileParseDiagnostics(file, []ScanDiagnostic{{Code: "treesitter_error", Message: err.Error()}})
		return
	}
	starts := lineStarts(content)
	importBindings := pythonImportBindings(idx, file, res.Imports)

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
		if call.Receiver == "" && pythonBuiltinCallables[call.Callee] {
			continue
		}
		callLine, callCol := offsetToLineCol(starts, call.Start)

		var fromID string
		var from CGPSymbol
		if call.InDecorator {
			fromID = fileSymbolID(file)
		} else {
			from = idx.containingSymbolFast(file, callLine)
			switch {
			case from.ID == "":
				// Module-level call (e.g. `app = Application()`, or
				// `main()` inside `if __name__ == "__main__":`) — attribute
				// to the file rather than dropping it, mirroring decorator
				// calls.
				fromID = fileSymbolID(file)
				idx.mu.Lock()
				from = idx.Symbols[fromID]
				idx.mu.Unlock()
			case call.Receiver == "" && from.Name == call.Callee && from.StartLine == callLine:
				continue
			default:
				fromID = from.ID
			}
		}

		var target, confidence, reason string
		switch call.Receiver {
		case "self", "cls":
			target, confidence, reason = resolveScopedCall(idx, file, from, call.Callee)
		case "super()":
			target, confidence, reason = resolveSuperCall(idx, file, from, call.Callee)
		case "":
			if binding, ok := importBindings[call.Callee]; ok {
				target, confidence, reason, _ = resolveImportBoundCall(idx, binding.file, binding.declaredName)
			}
			if target == "" {
				target, confidence, reason = resolveSymbolCall(idx, file, call.Callee)
			}
		default:
			if t, c, ok := resolvePythonVarCall(idx, from, call, importBindings); ok {
				target, confidence = t, c
			} else if binding, ok := importBindings[call.Receiver]; ok {
				target, confidence, reason, _ = resolveImportBoundCall(idx, binding.file, call.Callee)
			}
			if target == "" {
				// Python's runtime receiver type is not implied by a
				// same-file function sharing the member name. Treating
				// current_app.make_response() as a call to the surrounding
				// helpers.make_response() function creates false recursion.
				// Preserve an ambiguity reason when multiple real methods
				// share the name, but never promote a unique name coincidence
				// on an otherwise unknown receiver.
				_, candidateConfidence, candidateReason := resolveSymbolCall(idx, file, call.Receiver+"."+call.Callee)
				reason = ReasonDynamicReceiver
				if candidateConfidence == ConfUnresolved && candidateReason == ReasonAmbiguousName {
					reason = ReasonAmbiguousName
				}
				target = "unresolved:" + call.Receiver + "." + call.Callee
				confidence = ConfUnresolved
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

type pythonImportBinding struct {
	file         string
	declaredName string
}

func pythonImportBindings(idx *Index, file string, imports []treesitter.Import) map[string]pythonImportBinding {
	out := map[string]pythonImportBinding{}
	for _, imp := range imports {
		targetFile := resolvePythonModuleFile(idx, file, imp.Spec)
		if targetFile == "" {
			continue
		}
		if imp.Name != "" {
			local := imp.Name
			if imp.Alias != "" {
				local = imp.Alias
			}
			out[local] = pythonImportBinding{file: targetFile, declaredName: imp.Name}
			continue
		}
		local := imp.Alias
		if local == "" {
			local = strings.Split(imp.Spec, ".")[0]
		}
		if local != "" {
			out[local] = pythonImportBinding{file: targetFile}
		}
	}
	return out
}

func resolvePythonModuleFile(idx *Index, file, spec string) string {
	if spec == "" {
		return ""
	}
	dots := 0
	for dots < len(spec) && spec[dots] == '.' {
		dots++
	}
	module := strings.TrimPrefix(spec[dots:], ".")
	module = strings.ReplaceAll(module, ".", "/")
	base := module
	if dots > 0 {
		base = path.Dir(file)
		for i := 1; i < dots; i++ {
			base = path.Dir(base)
		}
		if module != "" {
			base = path.Join(base, module)
		}
	}
	candidates := []string{base + ".py", path.Join(base, "__init__.py")}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for _, candidate := range candidates {
		candidate = filepath.ToSlash(path.Clean(candidate))
		if _, ok := idx.Files[candidate]; ok {
			return candidate
		}
	}
	if dots == 0 {
		var found string
		for indexed := range idx.Files {
			for _, candidate := range candidates {
				candidate = filepath.ToSlash(path.Clean(candidate))
				if indexed == candidate || strings.HasSuffix(indexed, "/"+candidate) {
					if found != "" && found != indexed {
						return ""
					}
					found = indexed
				}
			}
		}
		return found
	}
	return ""
}

func resolvePythonVarCall(idx *Index, from CGPSymbol, call treesitter.Call, imports map[string]pythonImportBinding) (target, confidence string, ok bool) {
	name := call.Receiver
	if strings.HasPrefix(name, "self.") {
		name = strings.TrimPrefix(name, "self.")
	} else if strings.ContainsAny(name, ".()") {
		return "", "", false
	}
	typeName, found := idx.lookupVarType(from.ID, name)
	if !found {
		if classID := enclosingClassID(idx, from); classID != "" {
			typeName, found = idx.lookupVarType(classID, name)
		}
	}
	if !found || typeName == "" {
		return "", "", false
	}

	className := typeName
	var binding pythonImportBinding
	if dot := strings.LastIndexByte(typeName, '.'); dot >= 0 {
		binding = imports[typeName[:dot]]
		className = typeName[dot+1:]
	} else {
		binding = imports[typeName]
		if binding.declaredName != "" {
			className = binding.declaredName
		}
	}
	classID := ""
	if binding.file != "" {
		classID = findClassInFile(idx, binding.file, className, "python")
	} else {
		classID = findClassByName(idx, className, from.File, "python")
	}
	if classID == "" {
		return "", "", false
	}
	if id := findMethodInClass(idx, classID, call.Callee); id != "" {
		return id, ConfScoped, true
	}
	return "", "", false
}

func findClassInFile(idx *Index, file, name, language string) string {
	idx.ensureFileSymbolIndex()
	idx.mu.Lock()
	defer idx.mu.Unlock()
	found := ""
	for _, sym := range idx.symbolsByName[name] {
		if sym.File != file || sym.Language != language || sym.Kind != "class" {
			continue
		}
		if found != "" {
			return ""
		}
		found = sym.ID
	}
	return found
}

// javaSpringMappingMethods maps Spring MVC mapping-annotation simple names to
// the HTTP method they declare. `@RequestMapping` is handled separately
// (springHTTPMethod): it has no fixed method, so its Annotation.Method
// (parsed from `method = RequestMethod.X`) or a "GET" default is used
// instead.
var javaSpringMappingMethods = map[string]string{
	"GetMapping":    "GET",
	"PostMapping":   "POST",
	"PutMapping":    "PUT",
	"DeleteMapping": "DELETE",
	"PatchMapping":  "PATCH",
}

// springHTTPMethod reports whether ann is a Spring MVC route-mapping
// annotation and, if so, the HTTP method it declares: the fixed method for
// `@GetMapping`/`@PostMapping`/etc., or for `@RequestMapping` either its
// explicit `method = RequestMethod.X` element or a "GET" default.
func springHTTPMethod(ann treesitter.Annotation) (string, bool) {
	if m, ok := javaSpringMappingMethods[ann.Name]; ok {
		return m, true
	}
	if ann.Name == "RequestMapping" {
		if ann.Method != "" {
			return ann.Method, true
		}
		return "GET", true
	}
	return "", false
}

// joinHTTPPaths combines a Spring controller's class-level @RequestMapping
// base path with a method-level mapping path, e.g.
// joinHTTPPaths("/api/owners", "/{id}") -> "/api/owners/{id}",
// joinHTTPPaths("/api/owners", "") -> "/api/owners",
// joinHTTPPaths("", "/api/owners") -> "/api/owners",
// joinHTTPPaths("", "") -> "/".
func joinHTTPPaths(base, sub string) string {
	base = strings.TrimSuffix(base, "/")
	if sub == "" {
		if base == "" {
			return "/"
		}
		return base
	}
	if !strings.HasPrefix(sub, "/") {
		sub = "/" + sub
	}
	return base + sub
}

// javaBuiltinCallables holds JDK static-import/utility method names commonly
// called bare (no receiver), e.g. `asList(...)` from `import static
// java.util.Arrays.asList`. Bare calls to these never resolve to anything in
// the repo, so they are skipped rather than emitted as
// `unresolved:asList`/etc. noise.
var javaBuiltinCallables = map[string]bool{
	"asList": true, "of": true, "requireNonNull": true, "requireNonNullElse": true,
	"valueOf": true, "format": true, "println": true, "print": true, "printf": true,
	"max": true, "min": true, "abs": true, "sum": true, "sort": true, "compare": true,
	"emptyList": true, "emptyMap": true, "emptySet": true, "singletonList": true,
	"toList": true, "toSet": true, "toMap": true, "joining": true, "groupingBy": true,

	// JUnit/Hamcrest/AssertJ/Mockito/Spring-MVC-test static-import DSL names.
	// These are near-universal in real Java test suites and, called bare
	// (Receiver == ""), never resolve to repo symbols - suppressing them
	// avoids large amounts of unresolved:assertThat/etc. noise.
	"assertThat": true, "assertEquals": true, "assertNotEquals": true,
	"assertTrue": true, "assertFalse": true, "assertNull": true,
	"assertNotNull": true, "assertSame": true, "assertNotSame": true,
	"assertArrayEquals": true, "assertThrows": true, "fail": true,
	"is": true, "not": true, "equalTo": true, "hasProperty": true,
	"hasItem": true, "hasItems": true, "hasSize": true, "hasKey": true,
	"hasEntry": true, "contains": true, "containsString": true,
	"containsInAnyOrder": true, "nullValue": true, "notNullValue": true,
	"instanceOf": true, "anyOf": true, "allOf": true,
	"given": true, "when": true, "then": true, "verify": true, "verifyNoMoreInteractions": true,
	"times": true, "never": true, "atLeast": true, "atMost": true, "atLeastOnce": true,
	"any": true, "anyString": true, "anyInt": true, "anyLong": true,
	"anyDouble": true, "anyBoolean": true, "anyList": true, "anyMap": true,
	"eq": true, "mock": true, "spy": true, "doReturn": true, "doThrow": true,
	"doNothing": true, "doAnswer": true, "lenient": true,
	"status": true, "view": true, "model": true, "redirectedUrl": true,
	"forwardedUrl": true, "flash": true, "jsonPath": true, "content": true,
	"header": true, "cookie": true, "request": true, "get": true, "post": true,
	"put": true, "delete": true, "patch": true, "options": true,
}

// javaGlobalReceivers holds JDK standard-library types/namespaces whose
// static methods (e.g. `System.out.println`, `Objects.requireNonNull`,
// `Math.max`) never resolve to anything in the repo. Calls whose receiver's
// root identifier is one of these are skipped.
var javaGlobalReceivers = map[string]bool{
	"System": true, "Math": true, "Objects": true, "Arrays": true,
	"Collections": true, "Optional": true, "Stream": true, "IntStream": true,
	"Collectors": true, "Files": true, "Paths": true, "Thread": true,
	"String": true, "Integer": true, "Long": true, "Double": true, "Float": true,
	"Boolean": true, "Character": true, "List": true, "Map": true, "Set": true,
	"LocalDate": true, "LocalDateTime": true, "Instant": true, "Duration": true,
}

// javaReceiverRoot returns the leading identifier of a (possibly chained)
// receiver expression, e.g. "System.out" -> "System".
func javaReceiverRoot(receiver string) string {
	if dot := strings.IndexByte(receiver, '.'); dot >= 0 {
		return receiver[:dot]
	}
	return receiver
}

// javaChainedBuiltinReceiver reports whether receiver is itself a call
// expression whose callee is a known builtin/DSL function, e.g.
// "status()" (from `status().isOk()`) or "assertThat(optionalOwner)" (from
// `assertThat(optionalOwner).isPresent()`). Such chains never resolve to
// repo symbols and are suppressed for the same reason as javaBuiltinCallables.
func javaChainedBuiltinReceiver(receiver string) bool {
	paren := strings.IndexByte(receiver, '(')
	if paren <= 0 || !strings.HasSuffix(receiver, ")") {
		return false
	}
	return javaBuiltinCallables[receiver[:paren]]
}

// emitJavaRelationsTS uses the tree-sitter Java grammar to emit import and
// call edges. `this.foo()`/`super.foo()` resolve to the enclosing class's
// (or its single extends-base's) method via resolveScopedCall/
// resolveSuperCall, mirroring Python's self/super handling.
func emitJavaRelationsTS(idx *Index, file, content string) {
	res, err := treesitter.Parse("java", []byte(content))
	if err != nil {
		idx.markFileParseDiagnostics(file, []ScanDiagnostic{{Code: "treesitter_error", Message: err.Error()}})
		return
	}
	starts := lineStarts(content)

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

	emitImplementsEdges(idx, file, "java")

	idx.ensureFileSymbolIndex()
	for _, call := range res.Calls {
		if call.Callee == "" {
			continue
		}
		if call.Receiver == "" && javaBuiltinCallables[call.Callee] {
			continue
		}
		if call.Receiver != "" && javaGlobalReceivers[javaReceiverRoot(call.Receiver)] {
			continue
		}
		if call.Receiver != "" && javaChainedBuiltinReceiver(call.Receiver) {
			continue
		}
		callLine, callCol := offsetToLineCol(starts, call.Start)

		from := idx.containingSymbolFast(file, callLine)
		fromID := from.ID
		if fromID == "" {
			fromID = fileSymbolID(file)
		}

		var target, confidence, reason string
		switch call.Receiver {
		case "this":
			target, confidence, reason = resolveScopedCall(idx, file, from, call.Callee)
		case "super":
			target, confidence, reason = resolveSuperCall(idx, file, from, call.Callee)
		case "":
			target, confidence, reason = resolveScopedCall(idx, file, from, call.Callee)
		default:
			if t, c, ok := resolveVarCall(idx, from, call, "java"); ok {
				target, confidence = t, c
			} else {
				target, confidence, reason = resolveSymbolCall(idx, file, call.Receiver+"."+call.Callee)
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

// goBuiltinCallables holds Go predeclared (builtin) function names. Bare
// calls (no `.` in Callee) to these never resolve to a repo symbol and would
// otherwise be emitted as unresolved:make/unresolved:len/etc. noise.
var goBuiltinCallables = map[string]bool{
	"make": true, "len": true, "cap": true, "append": true, "copy": true,
	"delete": true, "panic": true, "recover": true, "print": true,
	"println": true, "new": true, "close": true, "complex": true,
	"real": true, "imag": true, "min": true, "max": true, "clear": true,
	"error": true, "string": true, "int": true, "int8": true, "int16": true,
	"int32": true, "int64": true, "uint": true, "uint8": true, "uint16": true,
	"uint32": true, "uint64": true, "uintptr": true, "byte": true,
	"rune": true, "bool": true, "float32": true, "float64": true,
	"complex64": true, "complex128": true, "any": true,
}

// emitGoRelationsTS uses the tree-sitter Go grammar to emit import edges and
// call edges. Calls with a receiver (`recv.Method()`) are resolved via
// resolveGoReceiverCall, which uses idx.goReceivers/idx.goMethodsByReceiverType
// (built during emitGoSymbolsTS) plus struct-embedding fallback
// (idx.classBases) for an exact, receiver-type-aware resolution that doesn't
// need Java-style variable-type inference: a Go method's receiver type is
// always statically known from its own declaration. Receiver-less calls
// (bare functions or package-qualified `pkg.Func()`) fall back to
// resolveSymbolCall, consistent with Python/Java's handling of
// module-qualified calls (stdlib/external packages naturally resolve to
// unresolved/missing_import).
func emitGoRelationsTS(idx *Index, file, content string) {
	res, err := treesitter.Parse("go", []byte(content))
	if err != nil {
		idx.markFileParseDiagnostics(file, []ScanDiagnostic{{Code: "treesitter_error", Message: err.Error()}})
		return
	}
	starts := lineStarts(content)
	externalPackages := goExternalPackageBindings(idx, res.Imports)

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

	// Infer declared types for `x := NewThing()` / `x, err := s.GetRepo()`
	// short var declarations from the callee's known return types, so that
	// later `x.Method()` calls in this file resolve via idx.varTypes at
	// ConfScoped. Sorted by source position so chained inference (`repo :=
	// s.GetRepo(); x := repo.Find()`) sees idx.varTypes entries from earlier
	// CallAssigns in the same scope.
	sort.SliceStable(res.CallAssigns, func(i, j int) bool { return res.CallAssigns[i].Pos < res.CallAssigns[j].Pos })
	for _, ca := range res.CallAssigns {
		caLine, _ := offsetToLineCol(starts, ca.Pos)
		from := idx.containingSymbolFast(file, caLine)
		if from.ID == "" {
			continue
		}

		var target, confidence string
		if ca.Receiver == "" {
			target, confidence, _ = resolveSymbolCall(idx, file, ca.Callee)
		} else {
			receiverRoot := ca.Receiver
			if dot := strings.IndexByte(receiverRoot, '.'); dot >= 0 {
				receiverRoot = receiverRoot[:dot]
			}
			if externalPackages[receiverRoot] || goReceiverUsesExternalType(idx, from, ca.Receiver, externalPackages) {
				recordGoVarType(idx, from.ID, ca.Name, "external:"+receiverRoot)
				continue
			}
			target, confidence, _ = resolveGoReceiverCall(idx, file, from, ca.Receiver, ca.Callee)
		}
		if confidence != ConfExact && confidence != ConfScoped {
			continue
		}

		idx.mu.Lock()
		types := append([]string(nil), idx.goReturnTypes[target]...)
		idx.mu.Unlock()
		if ca.ResultIndex < len(types) && types[ca.ResultIndex] != "" {
			recordGoVarType(idx, from.ID, ca.Name, types[ca.ResultIndex])
		}
	}

	for _, call := range res.Calls {
		if call.Callee == "" {
			continue
		}
		if call.Receiver == "" && goBuiltinCallables[call.Callee] {
			continue
		}
		callLine, callCol := offsetToLineCol(starts, call.Start)

		from := idx.containingSymbolFast(file, callLine)
		fromID := from.ID
		if fromID == "" {
			fromID = fileSymbolID(file)
		}

		var target, confidence, reason string
		if call.Receiver == "" {
			target, confidence, reason = resolveSymbolCall(idx, file, call.Callee)
		} else {
			receiverRoot := call.Receiver
			if dot := strings.IndexByte(receiverRoot, '.'); dot >= 0 {
				receiverRoot = receiverRoot[:dot]
			}
			if externalPackages[receiverRoot] || goReceiverUsesExternalType(idx, from, call.Receiver, externalPackages) {
				// The exact import edge already records this dependency.
				// A package-qualified external call is not an uncertain
				// candidate for an internal method with the same tail name.
				continue
			}
			target, confidence, reason = resolveGoReceiverCall(idx, file, from, call.Receiver, call.Callee)
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

func recordGoVarType(idx *Index, scopeID, name, typeName string) {
	if scopeID == "" || name == "" || typeName == "" {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.varTypes == nil {
		idx.varTypes = map[string]map[string]string{}
	}
	if idx.varTypes[scopeID] == nil {
		idx.varTypes[scopeID] = map[string]string{}
	}
	idx.varTypes[scopeID][name] = typeName
}

// goExternalPackageBindings returns receiver names that are proven to refer
// to packages outside the indexed repository. Go import bindings are
// statically known, unlike an arbitrary object receiver; preserving them as
// unresolved internal calls adds graph noise and can make an unrelated
// same-named repo method look possibly called. The import edge itself remains
// in the graph, so external dependency evidence is not lost.
func goExternalPackageBindings(idx *Index, imports []treesitter.Import) map[string]bool {
	if len(imports) == 0 {
		return nil
	}
	idx.mu.Lock()
	indexedDirs := make(map[string]bool, len(idx.Files))
	modulePath := idx.goModulePath
	for file := range idx.Files {
		dir := path.Dir(filepath.ToSlash(file))
		if dir != "." {
			indexedDirs[dir] = true
		}
	}
	idx.mu.Unlock()

	out := map[string]bool{}
	for _, imp := range imports {
		if imp.Spec == "" || imp.Alias == "." || imp.Alias == "_" {
			continue
		}
		internal := modulePath != "" &&
			(imp.Spec == modulePath || strings.HasPrefix(imp.Spec, modulePath+"/"))
		if modulePath == "" {
			for dir := range indexedDirs {
				if imp.Spec == dir || strings.HasSuffix(imp.Spec, "/"+dir) {
					internal = true
					break
				}
			}
		}
		if internal {
			continue
		}
		binding := imp.Alias
		if binding == "" {
			binding = path.Base(imp.Spec)
		}
		if binding != "" && binding != "." {
			out[binding] = true
		}
	}
	return out
}

func markExternalGoVarTypes(idx *Index, imports []treesitter.Import, vars []treesitter.VarDecl) []treesitter.VarDecl {
	externalPackages := goExternalPackageBindings(idx, imports)
	if len(externalPackages) == 0 || len(vars) == 0 {
		return vars
	}
	out := append([]treesitter.VarDecl(nil), vars...)
	for i := range out {
		typeName := out[i].Type
		if dot := strings.IndexByte(typeName, '.'); dot > 0 && externalPackages[typeName[:dot]] {
			out[i].Type = "external:" + typeName
		}
	}
	return out
}

func goReceiverUsesExternalType(idx *Index, from CGPSymbol, receiver string, externalPackages map[string]bool) bool {
	if from.ID == "" || receiver == "" {
		return false
	}
	parts := strings.Split(receiver, ".")
	typeName, found := idx.lookupVarType(from.ID, parts[0])
	if !found {
		return false
	}
	if goTypeUsesExternalPackage(typeName, externalPackages) {
		return true
	}
	for _, field := range parts[1:] {
		classID := findClassByName(idx, unqualifiedGoType(typeName), from.File, "go")
		if classID == "" {
			return false
		}
		typeName, found = idx.lookupVarType(classID, field)
		if !found {
			return false
		}
		if goTypeUsesExternalPackage(typeName, externalPackages) {
			return true
		}
	}
	return false
}

func goTypeUsesExternalPackage(typeName string, externalPackages map[string]bool) bool {
	if strings.HasPrefix(typeName, "external:") {
		return true
	}
	if dot := strings.IndexByte(typeName, '.'); dot > 0 {
		return externalPackages[typeName[:dot]]
	}
	return false
}

func unqualifiedGoType(typeName string) string {
	typeName = strings.TrimPrefix(typeName, "external:")
	if dot := strings.LastIndexByte(typeName, '.'); dot >= 0 {
		return typeName[dot+1:]
	}
	return typeName
}

// resolveGoReceiverCall resolves `recv.Method()`. If recv is the enclosing
// method's own receiver variable (e.g. `func (s *Service) Foo() { s.Bar() }`
// -> recv "s" matches the enclosing method's receiver name, whose type
// "Service" is known exactly from its declaration), the call resolves via
// findMethodOnGoType against that exact type. Otherwise, if recv is a
// local variable, parameter, or field whose declared type was recorded by
// populateVarTypes (idx.varTypes — explicit `var`/parameter types, or
// `x := &T{...}`/`x := T{...}` composite literals), the call resolves via
// findMethodOnGoType against that declared type. Otherwise falls back to
// resolveSymbolCall on "recv.Method", which strips to the bare method name
// and resolves it if it uniquely identifies one repo symbol (handles
// package-qualified calls like `pkg.NewThing()`, and stdlib/external
// receivers naturally fall through to unresolved/missing_import).
func resolveGoReceiverCall(idx *Index, file string, from CGPSymbol, receiver, callee string) (target, confidence, reason string) {
	idx.mu.Lock()
	recv, ok := idx.goReceivers[from.ID]
	idx.mu.Unlock()

	if ok && recv.Name != "" && receiver == recv.Name {
		if id := findMethodOnGoType(idx, recv.Type, callee, file); id != "" {
			return id, ConfScoped, ""
		}
	}

	if typeName, found := idx.lookupVarType(from.ID, receiver); found {
		if id := findMethodOnGoType(idx, typeName, callee, file); id != "" {
			return id, ConfScoped, ""
		}
	}

	// Two-level field access (`s.repo.Find()`, the dominant real-world
	// shape for a struct whose dependencies are themselves struct-typed
	// fields, not just primitives) — found missing entirely: neither
	// branch above can match a dotted receiver like "s.repo" at all (the
	// method's own receiver name is exactly "s", and idx.varTypes is
	// keyed by simple names, never a dotted chain), so every such call
	// fell straight through to the bare-name fallback below, which is
	// honest but loses real, resolvable recall whenever the field's type
	// has a same-named method elsewhere in the repo. Only goes one level
	// beyond the root (matching every other language's "self.field"-style
	// resolution depth in this codebase, not specific to Go): root's type
	// resolves via the same two paths as a plain receiver above, then
	// field's type is looked up scoped to *that struct's own symbol ID* —
	// the struct-range fix to emitGoSymbolsTS, and the new Go-shaped
	// field_declaration case in varDeclsFromNode, are what make idx.varTypes
	// actually have an entry to find here at all.
	if dot := strings.LastIndexByte(receiver, '.'); dot > 0 {
		root, field := receiver[:dot], receiver[dot+1:]
		var rootType string
		if ok && recv.Name != "" && root == recv.Name {
			rootType = recv.Type
		} else if t, found := idx.lookupVarType(from.ID, root); found {
			rootType = t
		}
		if rootType != "" {
			if structID := findClassByName(idx, rootType, file, "go"); structID != "" {
				if fieldType, found := idx.lookupVarType(structID, field); found {
					if id := findMethodOnGoType(idx, fieldType, callee, file); id != "" {
						return id, ConfScoped, ""
					}
				}
			}
		}
	}

	return resolveSymbolCall(idx, file, receiver+"."+callee)
}

// findMethodOnGoType returns the symbol ID of method `methodName` declared
// directly on Go type `typeName` (idx.goMethodsByReceiverType), or promoted
// from one of typeName's embedded fields (idx.classBases, populated from
// struct-embedding by emitGoSymbolsTS), or "" if neither finds a match.
func findMethodOnGoType(idx *Index, typeName, methodName, fromFile string) string {
	typeName = unqualifiedGoType(typeName)
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return findMethodOnGoTypeLocked(idx, typeName, methodName, fromFile, map[string]bool{})
}

func findMethodOnGoTypeLocked(idx *Index, typeName, methodName, fromFile string, visited map[string]bool) string {
	if typeName == "" || visited[typeName] {
		return ""
	}
	visited[typeName] = true

	if m, ok := idx.goMethodsByReceiverType[typeName]; ok {
		if id, ok := m[methodName]; ok {
			return id
		}
	}

	typeID := findGoTypeByNameLocked(idx, typeName, fromFile)
	for _, baseName := range idx.classBases[typeID] {
		if id := findMethodOnGoTypeLocked(idx, strings.TrimPrefix(baseName, "*"), methodName, fromFile, visited); id != "" {
			return id
		}
	}
	return ""
}

// findGoTypeByNameLocked returns the symbol ID of the Go struct/interface
// type named `name`, or "" if zero or multiple such types are indexed. When
// the name is ambiguous repo-wide, a type declared in the same directory as
// fromFile (i.e. the same Go package, by convention) is preferred.
func findGoTypeByNameLocked(idx *Index, name, fromFile string) string {
	fromDir := path.Dir(fromFile)
	var found, sameDirFound string
	matches, sameDirMatches := 0, 0
	for _, sym := range idx.symbolsByName[name] {
		if sym.Language != "go" || (sym.Kind != "class" && sym.Kind != "interface") {
			continue
		}
		found = sym.ID
		matches++
		if path.Dir(sym.File) == fromDir {
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

// resolveScopedCall resolves `self.foo()`/`cls.foo()` (Python), `this.foo()`
// (Java), or a bare `foo()` call inside a Java instance/static method
// (implicit `this`) to a method of the class enclosing `from` or one of its
// ancestors (via findMethodInClass). Falls back to plain name-based
// resolution (resolveSymbolCall) otherwise.
func resolveScopedCall(idx *Index, file string, from CGPSymbol, callee string) (target, confidence, reason string) {
	if classID := enclosingClassID(idx, from); classID != "" {
		if id := findMethodInClass(idx, classID, callee); id != "" {
			return id, ConfScoped, ""
		}
	}
	return resolveSymbolCall(idx, file, callee)
}

// resolveSuperCall resolves `super().foo()` (Python), `super.foo()` (Java),
// or `super.foo()` with a trait mixed in via `with` (Scala — folded into
// the same Bases list as the `extends` base, so a class with one real
// superclass and any traits has len(bases) > 1 as the *common* case, not
// an edge case) to the base/trait's `foo` method. Every base is checked
// (not just a sole one); finding `foo` on exactly one of them is
// unambiguous regardless of how many bases there are in total. Finding it
// on more than one base (genuine diamond ambiguity) or none reports
// unresolved rather than guessing.
func resolveSuperCall(idx *Index, file string, from CGPSymbol, callee string) (target, confidence, reason string) {
	classID := enclosingClassID(idx, from)
	if classID != "" {
		idx.ensureFileSymbolIndex()
		idx.mu.Lock()
		bases := idx.classBases[classID]
		var found string
		methodMatches := 0
		for _, baseName := range bases {
			simple := baseName
			if dot := strings.LastIndexByte(simple, '.'); dot >= 0 {
				simple = simple[dot+1:]
			}
			baseClassID := findClassByNameLocked(idx, simple, from.File, from.Language)
			if baseClassID == "" {
				continue
			}
			if id := findMethodInClassLocked(idx, baseClassID, callee, map[string]bool{}); id != "" && id != found {
				found = id
				methodMatches++
			}
		}
		idx.mu.Unlock()
		if methodMatches == 1 {
			return found, ConfScoped, ""
		}
		if methodMatches > 1 {
			return "unresolved:" + callee, ConfUnresolved, ReasonAmbiguousName
		}
	}
	target, confidence, reason = resolveSymbolCall(idx, file, callee)
	if target == from.ID {
		// A super call can never legitimately resolve to the very method
		// it's written inside (that would mean the override calls itself,
		// not its base implementation). Reaching here means the bare-name
		// fallback above matched the override itself by coincidence (same
		// name, same file) rather than a real base-class implementation —
		// found via exactly that self-loop on a Scala trait-mixin fixture,
		// not assumed. Report honestly as unresolved instead of returning
		// a misleading self-referencing edge.
		return "unresolved:" + callee, ConfUnresolved, ReasonAmbiguousName
	}
	return target, confidence, reason
}

// enclosingClassID walks sym's ParentID chain and returns the ID of
// the nearest enclosing "class" symbol, or "" if sym is not nested inside a
// class (or has no ParentID chain at all).
func enclosingClassID(idx *Index, sym CGPSymbol) string {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	current := sym
	for current.ParentID != "" {
		parent, ok := idx.Symbols[current.ParentID]
		if !ok {
			return ""
		}
		if parent.Kind == "class" {
			return parent.ID
		}
		current = parent
	}
	return ""
}

// emitJavaImplementsEdges adds "implements" edges from classes declared in
// file to the interfaces they implement (idx.classInterfaces, populated
// during the symbols phase from Java `implements` clauses), when the
// interface's simple name uniquely identifies one indexed Java class symbol.
func emitImplementsEdges(idx *Index, file, language string) {
	type pair struct {
		classID, ifaceID         string
		startLine, startCol, end int
	}
	idx.mu.Lock()
	var pairs []pair
	for classID, ifaces := range idx.classInterfaces {
		sym, ok := idx.Symbols[classID]
		if !ok || sym.File != file {
			continue
		}
		for _, ifaceName := range ifaces {
			if ifaceID := findClassByNameLocked(idx, ifaceName, file, language); ifaceID != "" {
				pairs = append(pairs, pair{classID: classID, ifaceID: ifaceID, startLine: sym.StartLine, startCol: sym.StartColumn, end: sym.StartColumn + len(ifaceName)})
			}
		}
	}
	idx.mu.Unlock()

	for _, p := range pairs {
		idx.AddCGPEdge(p.classID, p.ifaceID, "implements", ConfScoped, Location{
			File: file, StartLine: p.startLine, StartColumn: p.startCol, EndLine: p.startLine, EndColumn: p.end, Kind: "implements",
		})
	}
}

// resolveVarCall resolves `variable.method()` and `variable::method`
// (method-reference) calls to the declared method of the variable's declared
// type, using field/parameter/local-variable type information collected
// during the symbols phase (idx.varTypes / Phase A) and, for
// interface-typed variables, the single concrete implementation of that
// interface (idx.classInterfaces / Phase C). Returns ok=false if the
// receiver isn't a simple identifier or `this.field`, the variable's type is
// unknown, or no matching method is found. language selects the
// import/namespace maps used to resolve the variable's type to a class
// ("java" or "csharp").
func resolveVarCall(idx *Index, from CGPSymbol, call treesitter.Call, language string) (target, confidence string, ok bool) {
	receiver := call.Receiver
	name := receiver
	switch {
	case strings.HasPrefix(receiver, "this."):
		name = strings.TrimPrefix(receiver, "this.")
	case strings.HasPrefix(receiver, "self."):
		// Rust `self.field.method()` — same shape as Java's "this.field".
		name = strings.TrimPrefix(receiver, "self.")
	case strings.HasPrefix(receiver, "$this->"):
		// PHP `$this->field->method()` — same shape, "->" instead of ".".
		name = strings.TrimPrefix(receiver, "$this->")
	case strings.HasPrefix(receiver, "this->"):
		// C++ `this->field->method()` — same shape, no "$" sigil.
		name = strings.TrimPrefix(receiver, "this->")
	case language == "ruby" && strings.HasPrefix(receiver, "@"):
		// Ruby `@repo.find(id)` — the "@" sigil itself is the
		// self-reference marker (Ruby has no separate "self.repo" form
		// for ivars), so the whole receiver text minus the sigil is the
		// bound name populateSelfAttributeVarTypes stored it under.
		name = strings.TrimPrefix(receiver, "@")
	case strings.ContainsAny(receiver, ".()"):
		return "", "", false
	}

	typeName, found := idx.lookupVarType(from.ID, name)
	var ownClassID string
	if !found {
		ownClassID = enclosingClassID(idx, from)
		if ownClassID != "" {
			typeName, found = idx.lookupVarType(ownClassID, name)
		}
	}
	if !found && language == "csharp" && ownClassID != "" {
		// `partial class Service` splits one logical class across
		// multiple files (designer-generated code, source generators, EF
		// scaffolding) — each file's own symbol only has the fields
		// declared in *that* fragment's own idx.varTypes scope. A field
		// declared in a sibling fragment (a different file's "partial
		// class Service { ... }") is invisible to ownClassID's own lookup,
		// even though it's logically the same class — found via real-world
		// testing of exactly this pattern, not a hypothetical: `this.repo`
		// (declared in one fragment) used in a method defined in another
		// fragment fell through to an ambiguous bare-name guess. Check
		// every other fragment of the same logical class before giving up.
		idx.mu.Lock()
		ownSym, ok := idx.Symbols[ownClassID]
		var siblings []string
		if ok {
			fqn := idx.csharpNamespaces[ownSym.File] + "." + ownSym.Name
			siblings = idx.csharpPartialFragments[fqn]
		}
		idx.mu.Unlock()
		for _, fragID := range siblings {
			if fragID == ownClassID {
				continue
			}
			if typeName, found = idx.lookupVarType(fragID, name); found {
				break
			}
		}
	}
	if !found && language == "lua" {
		// Lua has no class symbol for enclosingClassID to find — look up
		// the synthetic table-name-scoped binding populateLuaSelfAttributeVarTypes
		// recorded instead, keyed by the calling method's own receiver
		// table name (idx.luaReceiverTypeBySymbol), not a class ID.
		idx.mu.Lock()
		table := idx.luaReceiverTypeBySymbol[from.ID]
		idx.mu.Unlock()
		if table != "" {
			typeName, found = idx.lookupVarType("luatable:"+from.File+":"+table, name)
		}
	}
	if !found || typeName == "" {
		return "", "", false
	}

	if id := findExtensionMethod(idx, language, typeName, call.Callee); id != "" {
		return id, ConfScoped, true
	}

	if language == "lua" {
		// Lua tables are never "class"-kind symbols, so findClassByName
		// would never find one — look the method up directly via the
		// table-name-keyed side channel populated alongside
		// idx.luaReceiverTypeBySymbol.
		idx.mu.Lock()
		id := idx.luaMethodsByReceiverType[typeName][call.Callee]
		idx.mu.Unlock()
		if id != "" {
			return id, ConfScoped, true
		}
		return "", "", false
	}

	classID := findClassByName(idx, typeName, from.File, language)
	if classID == "" {
		return "", "", false
	}
	if id := findMethodInClass(idx, classID, call.Callee); id != "" {
		return id, ConfScoped, true
	}
	if id := singleImplementerMethod(idx, classID, call.Callee); id != "" {
		return id, ConfScoped, true
	}
	return "", "", false
}

// lookupVarType returns the declared simple type name of a variable/field
// named `name` within scope `scopeID` (a class or method symbol ID), as
// recorded by emitJavaSymbolsTS from field/local-variable/parameter
// declarations.
func (idx *Index) lookupVarType(scopeID, name string) (string, bool) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	m, ok := idx.varTypes[scopeID]
	if !ok {
		return "", false
	}
	t, ok := m[name]
	return t, ok
}

// findClassByName resolves a simple class/interface name to a symbol ID for
// the given language, acquiring idx.mu itself.
func findClassByName(idx *Index, name, fromFile, language string) string {
	idx.ensureFileSymbolIndex()
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return findClassByNameLocked(idx, name, fromFile, language)
}

// resolveOutOfLineMethodParents fixes up the ParentID of every
// generic-engine method recorded in idx.unresolvedMethodParents during
// Phase 5 symbol extraction (see emitGenericSymbolsTS): a method whose
// def.ParentName came from a non-lexical receiver-type redirect (Rust impl
// blocks, C/C++ qualified `Type::method` definitions) but whose class
// wasn't found in its *own* file. That miss does not mean the class
// doesn't exist anywhere — files are scanned in parallel with no defined
// order, so the declaring file (almost always a separate header, for the
// single most common real-world C++ pattern: a class declared in a .h with
// its methods defined out-of-line in a .cpp) may simply not have been
// scanned yet. Call once, after every file's Phase 5 symbol extraction has
// completed, before anything that depends on a method's ParentID being
// correct: resolveVarCall's class-scoped self-attribute lookups, and any
// caller resolving `obj.method()` into this method via findMethodInClass
// (which walks idx.childrenByParent[classID] — until this fix-up runs, an
// out-of-line method is parented to its file, not its class, so neither
// path can ever find it).
//
// Without this, the gap silently degrades two ways depending on direction:
// a call *from inside* the method (e.g. `self.field.method()`) reports
// honestly unresolved (no false positive, since enclosingClassID also
// fails the same way); a call *into* the method from elsewhere
// (`scanner.mark()`) instead falls through to a same-name bare-text search
// across the whole repo, which can resolve to the wrong same-named method
// in a real codebase rather than reporting unresolved at all — the second
// case is the more dangerous one structurally, even though both stem from
// the same root cause.
func resolveOutOfLineMethodParents(idx *Index) {
	idx.mu.Lock()
	if len(idx.unresolvedMethodParents) == 0 {
		idx.mu.Unlock()
		return
	}
	pending := make(map[string]string, len(idx.unresolvedMethodParents))
	for id, name := range idx.unresolvedMethodParents {
		pending[id] = name
	}
	idx.mu.Unlock()

	idx.ensureFileSymbolIndex()

	type fix struct{ methodID, classID string }
	var fixes []fix
	idx.mu.Lock()
	for methodID, className := range pending {
		sym, ok := idx.Symbols[methodID]
		if !ok {
			// The symbol no longer exists (its file was removed or its def
			// dropped on rescan) — nothing left to ever fix.
			delete(idx.unresolvedMethodParents, methodID)
			continue
		}
		classID := findClassByNameLocked(idx, className, sym.File, sym.Language)
		if classID == "" {
			// Still not found anywhere in the repo (the class may simply
			// not have been scanned yet on this exact call, or may not
			// exist at all — e.g. a namespace-qualified free function,
			// which this same redirect shape also captures). Left in the
			// map so the very next call (the next incremental rebake, or
			// the next file's worth of Phase 5 on a fresh BuildIndex)
			// retries it for free, rather than dropping it and giving up
			// permanently on a class that might simply not exist *yet*.
			continue
		}
		// Found — whether or not this is the first time (a watch-mode
		// rescan of just the .cpp file re-records the same already-correct
		// miss every time, since the per-file lookup that recorded it has
		// no memory of having succeeded before), this entry never needs
		// rechecking again unless the same file is rescanned and
		// re-records it fresh. Removing it here is what keeps
		// idx.unresolvedMethodParents from growing without bound over a
		// long-running `--watch` session.
		delete(idx.unresolvedMethodParents, methodID)
		if classID == sym.ParentID {
			continue
		}
		fixes = append(fixes, fix{methodID: methodID, classID: classID})
	}
	for _, f := range fixes {
		sym := idx.Symbols[f.methodID]
		sym.ParentID = f.classID
		idx.Symbols[f.methodID] = sym
	}
	idx.mu.Unlock()

	if len(fixes) > 0 {
		idx.invalidateFileSymbolIndex()
		idx.invalidateCodeSearchIndex()
		idx.ensureFileSymbolIndex()
	}
}

// findClassByNameLocked is findClassByName for callers that already hold
// idx.mu. It also backs findMethodInClassLocked's ancestor-class resolution,
// which determines language from the class symbol it's walking.
func findClassByNameLocked(idx *Index, name, fromFile, language string) string {
	switch language {
	case "java":
		// Prefer package-qualified resolution: an explicit single-type import
		// ("import pkg.Name;"), a wildcard import ("import pkg.*;") whose
		// package declares Name, or fromFile's own package.
		for _, imp := range idx.javaImports[fromFile] {
			if strings.HasSuffix(imp, "."+name) {
				if id, ok := idx.javaFQN[imp]; ok {
					return id
				}
			} else if strings.HasSuffix(imp, ".*") {
				pkg := strings.TrimSuffix(imp, "*")
				if id, ok := idx.javaFQN[pkg+name]; ok {
					return id
				}
			}
		}
		if pkg, ok := idx.javaPackages[fromFile]; ok {
			if id, ok := idx.javaFQN[pkg+"."+name]; ok {
				return id
			}
		}
	case "csharp":
		// Every C# `using` directive imports an entire namespace (no
		// wildcard syntax needed), so each using is tried as a namespace
		// prefix directly, plus fromFile's own namespace.
		for _, ns := range idx.csharpUsings[fromFile] {
			if id, ok := idx.csharpFQN[ns+"."+name]; ok {
				return id
			}
		}
		if ns, ok := idx.csharpNamespaces[fromFile]; ok {
			if id, ok := idx.csharpFQN[ns+"."+name]; ok {
				return id
			}
		}
	}

	fromDir := path.Dir(fromFile)
	var found, sameDirFound string
	matches, sameDirMatches := 0, 0
	for _, sym := range idx.symbolsByName[name] {
		// "interface" is included alongside "class": every caller of this
		// fallback (emitImplementsEdges resolving a `implements`/trait-bound
		// name, and findMethodInClassLocked's classBases walk resolving a
		// Rust `impl Trait for Type`'s default methods) is specifically
		// trying to find a trait/interface-kind symbol by name just as
		// often as a class — excluding "interface" here meant those calls
		// could never succeed for any language that captures traits/
		// interfaces under their own kind (Java, C#, Rust, Scala).
		if (sym.Kind == "class" || sym.Kind == "interface") && sym.Language == language {
			found = sym.ID
			matches++
			if path.Dir(sym.File) == fromDir {
				sameDirFound = sym.ID
				sameDirMatches++
			}
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

// findMethodInClass returns the symbol ID of the method named `methodName`
// declared on class `classID` or inherited from one of its ancestors (Java
// `extends`), or "" if none is found. Ancestor classes are resolved by
// simple name via findClassByNameLocked, which prefers a same-package
// (same-directory) candidate when the base name is ambiguous repo-wide; an
// ancestor name that remains ambiguous is skipped rather than guessed.
func findMethodInClass(idx *Index, classID, methodName string) string {
	idx.ensureFileSymbolIndex()
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return findMethodInClassLocked(idx, classID, methodName, map[string]bool{})
}

func findMethodInClassLocked(idx *Index, classID, methodName string, visited map[string]bool) string {
	if classID == "" || visited[classID] {
		return ""
	}
	visited[classID] = true
	for _, sym := range idx.childrenByParent[classID] {
		if sym.Name == methodName && (sym.Kind == "method" || sym.Kind == "function") {
			return sym.ID
		}
	}
	classSym, ok := idx.Symbols[classID]
	if !ok {
		return ""
	}
	if classSym.Language == "csharp" {
		// `partial class Service` splits one logical class across files;
		// idx.csharpFQN (used by findClassByNameLocked just below, and by
		// every external caller resolving "Service" by name) only ever
		// keeps *one* fragment's symbol ID — whichever happened to be
		// written last under Phase 5's parallel, non-deterministic file
		// scan. A method declared in a *different* fragment is invisible
		// to classID's own idx.childrenByParent, even though it's
		// logically on the same class — confirmed as a real, latent
		// (schedule-order-dependent, not consistently reproducible)
		// failure mode while verifying the field-lookup fix above. Check
		// every other fragment before falling through to base-class
		// lookup, which has no idea these IDs are the same class either.
		fqn := idx.csharpNamespaces[classSym.File] + "." + classSym.Name
		for _, fragID := range idx.csharpPartialFragments[fqn] {
			if fragID == classID || visited[fragID] {
				continue
			}
			visited[fragID] = true
			for _, sym := range idx.childrenByParent[fragID] {
				if sym.Name == methodName && (sym.Kind == "method" || sym.Kind == "function") {
					return sym.ID
				}
			}
		}
	}
	for _, baseName := range idx.classBases[classID] {
		simple := baseName
		if dot := strings.LastIndexByte(simple, '.'); dot >= 0 {
			simple = simple[dot+1:]
		}
		if baseID := findClassByNameLocked(idx, simple, classSym.File, classSym.Language); baseID != "" {
			if id := findMethodInClassLocked(idx, baseID, methodName, visited); id != "" {
				return id
			}
		}
	}
	return ""
}

// singleImplementerMethod handles calls through an interface-typed variable:
// if exactly one indexed class implements the interface identified by
// ifaceClassID and declares methodName, return that method's symbol ID.
// Multiple implementers (ambiguous dispatch target) return "".
func singleImplementerMethod(idx *Index, ifaceClassID, methodName string) string {
	idx.mu.Lock()
	ifaceName := ""
	if sym, ok := idx.Symbols[ifaceClassID]; ok {
		ifaceName = sym.Name
	}
	var implClassIDs []string
	for classID, ifaces := range idx.classInterfaces {
		for _, ifn := range ifaces {
			if ifn == ifaceName {
				implClassIDs = append(implClassIDs, classID)
				break
			}
		}
	}
	idx.mu.Unlock()

	var found string
	matches := 0
	for _, classID := range implClassIDs {
		if id := findMethodInClass(idx, classID, methodName); id != "" {
			found = id
			matches++
		}
	}
	if matches == 1 {
		return found
	}
	return ""
}

// ListSymbols / TraceSymbol / supporting query helpers.

func ListSymbols(idx *Index, query, kind, lang string) ListSymbolsResponse {
	return ListSymbolsWithOptions(idx, query, kind, lang, ListSymbolsOptions{})
}

func ListSymbolsWithOptions(idx *Index, query, kind, lang string, opts ListSymbolsOptions) ListSymbolsResponse {
	snap := idx.orderedSymbolGraphSnapshot()

	var out []CGPSymbolSummary
	q := strings.ToLower(strings.TrimSpace(query))
	kindSet := symbolKindFilterSet(kind, opts.Kinds)
	for _, id := range snap.OrderedSymbolIDs {
		sym := snap.Symbols[id]
		if len(kindSet) > 0 && !kindSet[sym.Kind] {
			continue
		}
		if lang != "" && sym.Language != lang {
			continue
		}
		if opts.SourceOnly && shouldExcludeNoisyFile(sym.File, opts) {
			continue
		}
		if q != "" && !symbolMatchesListQuery(sym, q) {
			continue
		}
		summary := summarizeSymbol(sym)
		if opts.WithScores {
			summary.Score = symbolSearchScore(summary, q)
		}
		out = append(out, summary)
	}
	sortSymbolSummaries(out, q)
	total := len(out)
	truncated := false
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
		truncated = true
	}
	if out == nil {
		out = []CGPSymbolSummary{}
	}
	status := "ok"
	if total == 0 && q != "" {
		status = "not_found"
	}
	return ListSymbolsResponse{Status: status, Query: query, Kind: kind, Lang: lang, Limit: opts.Limit, Total: total, Truncated: truncated, Symbols: out}
}

func symbolKindFilterSet(kind string, kinds []string) map[string]bool {
	out := map[string]bool{}
	if kind != "" {
		out[kind] = true
	}
	for _, item := range kinds {
		for _, part := range strings.Split(item, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				out[part] = true
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func shouldExcludeNoisyFile(file string, opts ListSymbolsOptions) bool {
	if isBackupOrDeadFile(file) {
		return true
	}
	if !opts.IncludeTests && isTestPath(file) {
		return true
	}
	if !opts.IncludeStories && isStoryPath(file) {
		return true
	}
	return false
}

func symbolMatchesListQuery(sym CGPSymbol, q string) bool {
	if strings.Contains(strings.ToLower(sym.Name), q) {
		return true
	}
	if !looksLikePathOrIDQuery(q) {
		return false
	}
	return strings.Contains(strings.ToLower(sym.ID), q) || strings.Contains(strings.ToLower(sym.File), q)
}

func looksLikePathOrIDQuery(q string) bool {
	return strings.Contains(q, "/") || strings.Contains(q, "\\") || strings.Contains(q, ":") || strings.HasPrefix(q, "symbol:")
}

func sortSymbolSummaries(out []CGPSymbolSummary, q string) {
	isBackup := memoizedBackupCheck()
	sort.SliceStable(out, func(i, j int) bool {
		if q != "" {
			left := symbolSearchScore(out[i], q)
			right := symbolSearchScore(out[j], q)
			if left != right {
				return left > right
			}
		}
		// After any query-score ordering, active source outranks backup/dead
		// copies of equal score (see sortedSymbols).
		if bi, bj := isBackup(out[i].File), isBackup(out[j].File); bi != bj {
			return !bi
		}
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].StartLine != out[j].StartLine {
			return out[i].StartLine < out[j].StartLine
		}
		return out[i].Name < out[j].Name
	})
}

// hasIdentifierSuffixMatch reports whether haystack ends with needle on an
// identifier-boundary: either haystack equals needle, or the rune
// immediately before the match is not a letter/digit (e.g. "/", ":", "_",
// ".", "-"). This is the "qualified name suffix" match used to rank queries
// like "file.js:functionName" or "Foo:get" above unrelated substring hits.
//
// Without the boundary check, a short query like "get" would match the
// suffix of "...Target" (which ends in "get") and outrank a real prefix
// match like "getApplicationsWithExpiringConnections" — exactly the
// find-symbol "get" regression where an unrelated "describeMongoTarget"
// topped the results.
func hasIdentifierSuffixMatch(haystack, needle string) bool {
	if needle == "" || !strings.HasSuffix(haystack, needle) {
		return false
	}
	if len(haystack) == len(needle) {
		return true
	}
	prev, _ := utf8.DecodeLastRuneInString(haystack[:len(haystack)-len(needle)])
	return !isSearchLetter(prev) && !isSearchDigit(prev)
}

func symbolSearchScore(sym CGPSymbolSummary, q string) int {
	name := strings.ToLower(sym.Name)
	id := strings.ToLower(sym.ID)
	file := strings.ToLower(sym.File)
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(sym.File)), strings.ToLower(filepath.Ext(sym.File)))
	score := symbolKindSearchWeight(sym.Kind)
	switch {
	case name == q:
		score += 1000
	case base == q:
		score += 850
	case hasIdentifierSuffixMatch(file+":"+name, q):
		score += 800
	case strings.Contains(name, q):
		score += 600
	case strings.Contains(id, q):
		score += 300
	}
	if strings.Contains(file, ".story.") || strings.Contains(file, ".stories.") {
		score -= 250
	}
	if isTestPath(file) {
		score -= 200
	}
	// Backup/dead copies rank below stories and tests — but only by penalty,
	// not a hard sort key, so a query that explicitly names the backup file
	// still finds it via the exact-match boosts above.
	if isBackupOrDeadFile(file) {
		score -= 400
	}
	return score
}

func symbolKindSearchWeight(kind string) int {
	if isTerraformSymbolKind(kind) {
		return 100
	}
	switch kind {
	case "function", "method", "component", "class", "interface", "type", "constant", "getter", "setter", "callback", "vue-prop", "vue-model", "vue-emit", "ttl-shape", "ttl-term":
		return 100
	case "file":
		return 30
	case "template-class", "css-class":
		return 5
	default:
		return 50
	}
}

func TraceSymbol(idx *Index, query string) TraceSymbolResponse {
	return TraceSymbolWithOptions(idx, query, TraceSymbolOptions{Sites: true})
}

// maxAmbiguousTraceDetails bounds how many ambiguous candidates get their
// full caller/callee trace expanded inline (see TraceSymbolWithOptions). A
// genuinely common name (50+ candidates) would make the response enormous
// and defeat the purpose — this only helps the common "2-4 same-named
// methods across files" case and avoids a disambiguate-then-trace sequence.
const maxAmbiguousTraceDetails = 4

// maxAmbiguousTraceCandidates bounds the flat candidate list returned for a
// genuinely common name. A polyglot repo can have dozens of same-named methods
// (for example, many `execute` overrides across HTTP-client engines); returning
// all of them is thousands of tokens the agent cannot act on. Capping the list
// and telling the agent to refine keeps the ambiguous response lean while still
// surfacing enough candidates to disambiguate from.
const maxAmbiguousTraceCandidates = 15

// maxCompactOverloadPossibleCallers keeps a compact class-qualified overload
// trace actionable without expanding every unresolved same-name call in a
// large repository. The entries are shared by the overload family (not
// repeated once per candidate) and ranked by source-path proximity.
const maxCompactOverloadPossibleCallers = 16

func TraceSymbolWithOptions(idx *Index, query string, opts TraceSymbolOptions) TraceSymbolResponse {
	resp := TraceSymbolResponse{Status: "not_found", Query: query, Callers: []CGPSymbolSummary{}, Callees: []CGPSymbolSummary{}}
	matches := findSymbols(idx, query)
	if len(matches) > 1 {
		resp.Status = "ambiguous"
		if opts.Compact {
			sorted := sortedSymbols(matches)
			resp.Candidates = make([]CGPSymbolSummary, 0, len(sorted))
			for _, match := range sorted {
				resp.Candidates = append(resp.Candidates, summarizeSymbolCandidate(match))
			}
		} else {
			resp.Candidates = summarizeSymbols(matches)
		}
		if len(resp.Candidates) > maxAmbiguousTraceCandidates {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("%d candidates matched %q; showing the first %d — disambiguate by re-querying with \"<file>:<name>\" from one candidate", len(resp.Candidates), query, maxAmbiguousTraceCandidates))
			resp.Candidates = resp.Candidates[:maxAmbiguousTraceCandidates]
		}
		if len(matches) <= maxAmbiguousTraceDetails {
			// One immutable generation for every candidate keeps all details
			// internally consistent if a watcher publishes an edit meanwhile.
			snap := idx.symbolGraphSnapshot()
			batch := buildSymbolTraceBatch(snap, matches, true)
			resp.CandidateDetails = make([]TraceSymbolCandidateDetail, 0, len(matches))
			for _, m := range matches {
				detail := traceSymbolFromBatch(m, snap, opts, batch)
				summary := summarizeSymbol(m)
				if opts.Compact {
					summary = summarizeSymbolCandidate(m)
				}
				resp.CandidateDetails = append(resp.CandidateDetails, TraceSymbolCandidateDetail{
					Symbol:           &summary,
					Callers:          detail.Callers,
					PossibleCallers:  detail.PossibleCallers,
					PossibleCount:    detail.PossibleCount,
					PossibleSites:    detail.PossibleSites,
					CallerConfidence: detail.CallerConfidence,
				})
			}
			if opts.Compact && isOverloadFamily(matches) {
				resp.PossibleCallers, resp.PossibleCount = compactOverloadPossibleCallers(snap, batch, matches, opts)
				if resp.PossibleCount > len(resp.PossibleCallers) {
					resp.Warnings = append(resp.Warnings, fmt.Sprintf("%d unresolved same-name call site(s) may target this overload family; showing the %d source-nearest possible callers without promoting them to resolved graph edges", resp.PossibleCount, len(resp.PossibleCallers)))
				}
			}
		} else {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("too many candidates (%d) to expand inline — disambiguate by re-querying with \"<file>:<name>\" from one candidate", len(matches)))
		}
		return resp
	}
	if len(matches) == 0 {
		return resp
	}
	single := traceSymbolFromSnapshot(matches[0], idx.symbolGraphSnapshot(), opts)
	single.Query = query
	return single
}

func isOverloadFamily(matches []CGPSymbol) bool {
	if len(matches) < 2 || matches[0].ParentID == "" {
		return false
	}
	for _, match := range matches[1:] {
		if match.Name != matches[0].Name || match.ParentID != matches[0].ParentID || match.File != matches[0].File {
			return false
		}
	}
	return true
}

func compactOverloadPossibleCallers(snap symbolGraphSnapshot, batch symbolTraceBatch, matches []CGPSymbol, opts TraceSymbolOptions) ([]CGPSymbolSummary, int) {
	targetFile := matches[0].File
	seenEdges := map[int]bool{}
	seenCallers := map[string]bool{}
	callers := make([]CGPSymbol, 0, maxCompactOverloadPossibleCallers)
	possibleSites := 0
	for _, match := range matches {
		for _, edgeIndex := range batch.possibleIndexes[match.ID] {
			if seenEdges[edgeIndex] {
				continue
			}
			seenEdges[edgeIndex] = true
			edge := snap.edgeAt(edgeIndex)
			caller, ok := snap.Symbols[edge.From]
			if !ok || (opts.ExcludeTests && isTestCaller(caller)) {
				continue
			}
			possibleSites++
			if !seenCallers[caller.ID] {
				seenCallers[caller.ID] = true
				callers = append(callers, caller)
			}
		}
	}
	sort.Slice(callers, func(i, j int) bool {
		left := commonPathPrefixSegments(targetFile, callers[i].File)
		right := commonPathPrefixSegments(targetFile, callers[j].File)
		if left != right {
			return left > right
		}
		if callers[i].File != callers[j].File {
			return callers[i].File < callers[j].File
		}
		if callers[i].StartLine != callers[j].StartLine {
			return callers[i].StartLine < callers[j].StartLine
		}
		return callers[i].Name < callers[j].Name
	})
	if len(callers) > maxCompactOverloadPossibleCallers {
		callers = callers[:maxCompactOverloadPossibleCallers]
	}
	out := make([]CGPSymbolSummary, 0, len(callers))
	for _, caller := range callers {
		out = append(out, summarizeSymbolCompact(caller))
	}
	return out, possibleSites
}

func commonPathPrefixSegments(left, right string) int {
	leftParts := strings.FieldsFunc(filepath.ToSlash(left), func(r rune) bool { return r == '/' })
	rightParts := strings.FieldsFunc(filepath.ToSlash(right), func(r rune) bool { return r == '/' })
	count := 0
	for count < len(leftParts)-1 && count < len(rightParts)-1 && leftParts[count] == rightParts[count] {
		count++
	}
	return count
}

// traceSymbolFromSnapshot is TraceSymbolWithOptions' single-symbol core,
// factored out so the ambiguous-candidate-expansion path can reuse one
// immutable generation across every candidate.
func traceSymbolFromSnapshot(sym CGPSymbol, snap symbolGraphSnapshot, opts TraceSymbolOptions) TraceSymbolResponse {
	return traceSymbolFromBatch(sym, snap, opts, buildSymbolTraceBatch(snap, []CGPSymbol{sym}, true))
}

// symbolTraceBatch indexes only edges relevant to the requested symbols. It
// deliberately remains request-local: retaining a second full adjacency index
// beside SymbolEdges would raise steady-state memory substantially on large
// repositories, while a single linear pass is cheap and keeps watcher updates
// atomic with the snapshot used by the request.
type symbolTraceBatch struct {
	edgeIndexes     map[string][]int
	possibleIndexes map[string][]int
	degrees         map[string]int
}

func buildSymbolTraceBatch(snap symbolGraphSnapshot, targets []CGPSymbol, includePossible bool) symbolTraceBatch {
	return buildSymbolTraceBatchWithDegrees(snap, targets, includePossible, false)
}

func buildSymbolTraceBatchWithDegrees(snap symbolGraphSnapshot, targets []CGPSymbol, includePossible, collectDegrees bool) symbolTraceBatch {
	batch := symbolTraceBatch{
		edgeIndexes:     make(map[string][]int, len(targets)),
		possibleIndexes: make(map[string][]int, len(targets)),
	}
	if collectDegrees {
		batch.degrees = map[string]int{}
	}
	targetIDs := make(map[string]bool, len(targets))
	possibleTargets := map[string]map[string][]string{}
	for _, target := range targets {
		if target.ID == "" || targetIDs[target.ID] {
			continue
		}
		targetIDs[target.ID] = true
		batch.edgeIndexes[target.ID] = nil
		if !includePossible {
			continue
		}
		family := languageFamily(target.Language)
		if possibleTargets[family] == nil {
			possibleTargets[family] = map[string][]string{}
		}
		possibleTargets[family][target.Name] = append(possibleTargets[family][target.Name], target.ID)
	}
	snap.forEachEdge(func(i int, edge CGPEdge) bool {
		if collectDegrees && edge.Type == "calls" && edge.Confidence != ConfUnresolved {
			batch.degrees[edge.From]++
			batch.degrees[edge.To]++
		}
		if targetIDs[edge.From] {
			batch.edgeIndexes[edge.From] = append(batch.edgeIndexes[edge.From], i)
		}
		if edge.To != edge.From && targetIDs[edge.To] {
			batch.edgeIndexes[edge.To] = append(batch.edgeIndexes[edge.To], i)
		}
		if !includePossible {
			return true
		}
		name := unresolvedCallTargetName(edge)
		if name == "" {
			return true
		}
		caller, ok := snap.Symbols[edge.From]
		if !ok || caller.Kind == "file" {
			return true
		}
		for _, targetID := range possibleTargets[languageFamily(caller.Language)][name] {
			batch.possibleIndexes[targetID] = append(batch.possibleIndexes[targetID], i)
		}
		return true
	})
	return batch
}

func traceSymbolFromBatch(sym CGPSymbol, snap symbolGraphSnapshot, opts TraceSymbolOptions, batch symbolTraceBatch) TraceSymbolResponse {
	resp := TraceSymbolResponse{Status: "found", Callers: []CGPSymbolSummary{}, Callees: []CGPSymbolSummary{}}
	if opts.Compact {
		// The queried symbol's own Docstring is the one field Compact
		// doesn't drop outright (it's the actual answer to what was
		// asked about), but an unbounded docstring is still real cost —
		// trimmed to its first sentence rather than removed, see
		// firstSentenceOrLimit's doc comment. sym is already a by-value
		// copy (this function's own parameter), so mutating it here
		// can't affect idx's stored symbol.
		sym.Docstring = firstSentenceOrLimit(sym.Docstring)
		// MarshalJSON renders Symbol via compactMainSymbolJSON instead of
		// CGPSymbol's full shape when this is set — see its doc comment
		// for why id/language/startColumn/endLine/endColumn/exported/
		// parentId are worth dropping here too, on top of the trimmed
		// docstring above.
		resp.compactMainSymbol = true
	}
	resp.Symbol = &sym
	symbolMap := snap.Symbols

	summarize := summarizeSymbol
	if opts.Compact {
		summarize = summarizeSymbolCompact
		// CallerSites duplicates, at the individual-call-site level, the
		// same who-calls-this information Callers already gives per
		// distinct caller symbol — real, additional cost for a query
		// whose whole point is "the leanest possible identify+locate
		// view." A caller wanting both gets the contradiction resolved in
		// compact's favor: Compact's CLI/MCP default (Sites: true)
		// otherwise meant `-compact` alone never actually dropped this,
		// the single largest remaining cost after id/language were
		// removed from each caller/callee entry.
		opts.Sites = false
	}

	seenCallers := map[string]bool{}
	seenCallees := map[string]bool{}
	testGroups := map[string]*CGPSymbolSummary{}
	possibleSeen := map[string]bool{}
	possibleSites := 0
	for _, edgeIndex := range batch.possibleIndexes[sym.ID] {
		edge := snap.edgeAt(edgeIndex)
		caller, ok := symbolMap[edge.From]
		if !ok {
			continue
		}
		isTest := isTestCaller(caller)
		if opts.ExcludeTests && isTest {
			continue
		}
		if !opts.Compact && !possibleSeen[caller.ID] {
			resp.PossibleCallers = append(resp.PossibleCallers, summarize(caller))
			possibleSeen[caller.ID] = true
		}
		if !opts.Compact && opts.Sites {
			site := callSite(edge, caller)
			site.Confidence = ConfUnresolved
			resp.PossibleSites = append(resp.PossibleSites, site)
		}
		possibleSites++
	}
	if possibleSites > 0 {
		resp.PossibleCount = possibleSites
		resp.Warnings = append(resp.Warnings, fmt.Sprintf("%d unresolved same-name call site(s) may target this symbol; possible callers are not promoted to resolved graph edges", possibleSites))
	}
	for _, edgeIndex := range batch.edgeIndexes[sym.ID] {
		edge := snap.edgeAt(edgeIndex)
		if opts.WithEdges {
			resp.Edges = append(resp.Edges, edge)
		}
		if edge.Type == "imports" {
			resp.Imports = append(resp.Imports, edge)
			continue
		}
		if traceSymbolEdgeType(edge.Type) && edge.From == sym.ID {
			if target, ok := symbolMap[edge.To]; ok {
				if !seenCallees[target.ID] {
					resp.Callees = append(resp.Callees, summarize(target))
					seenCallees[target.ID] = true
				}
			}
		}
		if traceSymbolEdgeType(edge.Type) && edge.To == sym.ID {
			if caller, ok := symbolMap[edge.From]; ok {
				isTest := isTestCaller(caller)
				if opts.Sites && !(opts.ExcludeTests && isTest) {
					resp.CallerSites = append(resp.CallerSites, callSite(edge, caller))
					addConfidence(&resp.CallerConfidence, edge.Confidence)
				}
				if opts.ExcludeTests && isTest {
					continue
				}
				if isTest && !opts.IncludeTestDetails {
					addTestCallerGroup(testGroups, caller, edge, opts.Compact)
					continue
				}
				if !seenCallers[caller.ID] {
					resp.Callers = append(resp.Callers, summarize(caller))
					seenCallers[caller.ID] = true
				}
			}
		}
		if edge.Type == "uses-css-class" {
			if edge.From == sym.ID {
				if target, ok := symbolMap[edge.To]; ok {
					if !seenCallees[target.ID] {
						resp.Callees = append(resp.Callees, summarize(target))
						seenCallees[target.ID] = true
					}
				}
			}
			if edge.To == sym.ID {
				if caller, ok := symbolMap[edge.From]; ok {
					if opts.Sites {
						resp.CallerSites = append(resp.CallerSites, callSite(edge, caller))
						addConfidence(&resp.CallerConfidence, edge.Confidence)
					}
					if !seenCallers[caller.ID] {
						resp.Callers = append(resp.Callers, summarize(caller))
						seenCallers[caller.ID] = true
					}
				}
			}
		}
	}
	for _, group := range sortedTestCallerGroups(testGroups) {
		resp.Callers = append(resp.Callers, *group)
	}
	sort.Slice(resp.PossibleCallers, func(i, j int) bool {
		if resp.PossibleCallers[i].File != resp.PossibleCallers[j].File {
			return resp.PossibleCallers[i].File < resp.PossibleCallers[j].File
		}
		return resp.PossibleCallers[i].StartLine < resp.PossibleCallers[j].StartLine
	})
	sort.Slice(resp.PossibleSites, func(i, j int) bool {
		if resp.PossibleSites[i].File != resp.PossibleSites[j].File {
			return resp.PossibleSites[i].File < resp.PossibleSites[j].File
		}
		return resp.PossibleSites[i].Line < resp.PossibleSites[j].Line
	})
	sortTraceSymbolResponse(&resp)
	return resp
}

func traceSymbolEdgeType(edgeType string) bool {
	switch edgeType {
	case "calls", terraformDependencyEdge, "renders-component", "passes-prop", "binds-model", "listens-event", "handles-route", "calls-http-route":
		return true
	default:
		return false
	}
}

func callSite(edge CGPEdge, caller CGPSymbol) CGPCallSite {
	return CGPCallSite{
		File:       edge.Evidence.File,
		Line:       edge.Evidence.StartLine,
		Column:     edge.Evidence.StartColumn,
		Raw:        edge.Evidence.Raw,
		Caller:     caller.Name,
		Confidence: edge.Confidence,
	}
}

func addConfidence(summary *CGPConfidenceSummary, confidence string) {
	switch confidence {
	case ConfExact:
		summary.Exact++
	case ConfScoped:
		summary.Scoped++
	case ConfUnresolved:
		summary.Unresolved++
	default:
		summary.Heuristic++
	}
}

func isTestCaller(sym CGPSymbol) bool {
	file := filepath.ToSlash(sym.File)
	switch {
	case isTestPath(file):
		return true
	case sym.Kind == "callback" && looksLikeTestCallbackName(sym.Name):
		return true
	default:
		return false
	}
}

func isTestPath(file string) bool {
	file = strings.ToLower(filepath.ToSlash(file))
	segmented := "/" + strings.Trim(file, "/") + "/"
	for _, segment := range strings.Split(strings.Trim(file, "/"), "/") {
		if strings.HasPrefix(segment, "test-") ||
			strings.HasSuffix(segment, "-test") ||
			strings.HasSuffix(segment, "-tests") ||
			strings.Contains(segment, "-test-") {
			return true
		}
	}
	switch {
	case strings.Contains(segmented, "/test/"),
		strings.Contains(segmented, "/tests/"),
		strings.Contains(segmented, "/__tests__/"),
		strings.Contains(segmented, "/testfixtures/"),
		strings.Contains(segmented, "/testdata/"):
		return true
	case hasAnySuffix(file, ".test.ts", ".test.tsx", ".test.js", ".test.jsx", ".spec.ts", ".spec.tsx", ".spec.js", ".spec.jsx"):
		return true
	case hasAnySuffix(file,
		"_test.go", "_test.rs", "_test.py", "_spec.rb",
		".test.cs", ".spec.cs"):
		return true
	case strings.HasPrefix(filepath.Base(file), "test_") && strings.HasSuffix(file, ".py"):
		return true
	default:
		return false
	}
}

func isStoryPath(file string) bool {
	file = filepath.ToSlash(file)
	return strings.Contains(file, ".story.") || strings.Contains(file, ".stories.") || hasAnySuffix(file, ".story.vue", ".stories.vue", ".story.tsx", ".stories.tsx")
}

// backupFileNameRE matches filenames that look like ad-hoc backup/dead
// copies left in a repo, e.g. "class_backup_07-10-2024.js",
// "utils.js.bak", "config.old.json", "App.tsx.orig", "Foo copy.ts", or
// editor swap files ending in "~". These are rarely meaningful search
// results and crowd out real source files in ranked output.
var backupFileNameRE = regexp.MustCompile(`(?i)([._-](backup|bak|old|orig)([._-]?\d{1,2}[-_]\d{1,2}[-_]\d{2,4})?|[._-]\d{1,2}[-_]\d{1,2}[-_]\d{4}[._-](backup|bak|old|copy)| copy(\s*\d*)?)(\.[a-z0-9]+)?$`)

// isBackupOrDeadFile reports whether file looks like a backup/dead copy
// (see backupFileNameRE) rather than active source. It is checked
// unconditionally by search-code and inspect-exact so stale snapshot files
// don't consume result/context-slice budgets, and by shouldExcludeNoisyFile
// so -source-only views (list-symbols, repo-map) drop them too.
func isBackupOrDeadFile(file string) bool {
	base := filepath.Base(filepath.ToSlash(file))
	if strings.HasSuffix(base, "~") {
		return true
	}
	return backupFileNameRE.MatchString(base)
}

func hasAnySuffix(s string, suffixes ...string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(s, suffix) {
			return true
		}
	}
	return false
}

func looksLikeTestCallbackName(name string) bool {
	for _, prefix := range []string{"describe:", "it:", "test:", "beforeEach", "afterEach", "beforeAll", "afterAll"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// addTestCallerGroup builds (or updates) the synthetic "test callbacks"
// summary row for one file's grouped test callers. This is a separate
// construction path from summarize/summarizeSymbolCompact — found while
// regression-testing the compact-mode token fix: compact never reached
// here, so grouped test-caller rows kept Language even in compact mode
// while every other caller/callee entry had it dropped. compact mirrors
// summarizeSymbolCompact's own rule (no Language; this type never had an
// ID to begin with, so there's nothing to drop on that front).
func addTestCallerGroup(groups map[string]*CGPSymbolSummary, caller CGPSymbol, edge CGPEdge, compact bool) {
	group := groups[caller.File]
	if group == nil {
		group = &CGPSymbolSummary{
			Name:      "test callbacks",
			Kind:      "test-callback-group",
			File:      caller.File,
			StartLine: edge.Evidence.StartLine,
		}
		if !compact {
			group.Language = caller.Language
		}
		groups[caller.File] = group
	}
	group.Count++
	if group.StartLine <= 0 || (edge.Evidence.StartLine > 0 && edge.Evidence.StartLine < group.StartLine) {
		group.StartLine = edge.Evidence.StartLine
	}
	if compact {
		if group.Count > 1 {
			group.Truncated = true
		}
		return
	}
	if edge.Evidence.StartLine > 0 && !containsInt(group.Lines, edge.Evidence.StartLine) {
		group.Lines = append(group.Lines, edge.Evidence.StartLine)
		sort.Ints(group.Lines)
	}
	if len(group.NamesPreview) < 2 && caller.Name != "" && !containsStringValue(group.NamesPreview, caller.Name) {
		group.NamesPreview = append(group.NamesPreview, caller.Name)
	}
	if group.Count > len(group.NamesPreview) || len(group.Lines) > len(group.NamesPreview) {
		group.Truncated = true
	}
}

func sortedTestCallerGroups(groups map[string]*CGPSymbolSummary) []*CGPSymbolSummary {
	out := make([]*CGPSymbolSummary, 0, len(groups))
	for _, group := range groups {
		out = append(out, group)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].File < out[j].File
	})
	return out
}

func containsInt(values []int, want int) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsStringValue(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func sortTraceSymbolResponse(resp *TraceSymbolResponse) {
	sort.Slice(resp.Callers, func(i, j int) bool {
		if resp.Callers[i].File != resp.Callers[j].File {
			return resp.Callers[i].File < resp.Callers[j].File
		}
		if resp.Callers[i].StartLine != resp.Callers[j].StartLine {
			return resp.Callers[i].StartLine < resp.Callers[j].StartLine
		}
		return resp.Callers[i].Name < resp.Callers[j].Name
	})
	sort.Slice(resp.Callees, func(i, j int) bool {
		if resp.Callees[i].File != resp.Callees[j].File {
			return resp.Callees[i].File < resp.Callees[j].File
		}
		if resp.Callees[i].StartLine != resp.Callees[j].StartLine {
			return resp.Callees[i].StartLine < resp.Callees[j].StartLine
		}
		return resp.Callees[i].Name < resp.Callees[j].Name
	})
	sort.Slice(resp.CallerSites, func(i, j int) bool {
		if resp.CallerSites[i].File != resp.CallerSites[j].File {
			return resp.CallerSites[i].File < resp.CallerSites[j].File
		}
		if resp.CallerSites[i].Line != resp.CallerSites[j].Line {
			return resp.CallerSites[i].Line < resp.CallerSites[j].Line
		}
		return resp.CallerSites[i].Column < resp.CallerSites[j].Column
	})
	sort.Slice(resp.Edges, func(i, j int) bool {
		return resp.Edges[i].ID < resp.Edges[j].ID
	})
}

func findSymbols(idx *Index, query string) []CGPSymbol {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil
	}
	// Production indexes publish an immutable graph generation after build or
	// load. Resolve against that same generation used by trace/impact so a
	// watcher rebake cannot pair a target from a half-built live graph with
	// edges from the previous generation.
	if published := idx.publishedSymbolGraph.Load(); published != nil {
		return findSymbolsInGraph(*published, q)
	}
	qualifiedQuery := strings.Contains(q, ".") && !strings.ContainsAny(q, "/\\:")
	idx.mu.Lock()
	if sym, ok := idx.Symbols[q]; ok {
		idx.mu.Unlock()
		return []CGPSymbol{sym}
	}
	if qualifiedQuery {
		// Check only same-named children before walking ParentID. This avoids
		// copying the full symbol map into a second lookup map for a query that
		// normally has only one or two overload candidates.
		childName := q[strings.LastIndexByte(q, '.')+1:]
		var qualified []CGPSymbol
		for _, sym := range idx.Symbols {
			if sym.Name == childName && symbolQualifiedName(sym, idx.Symbols) == q {
				qualified = append(qualified, sym)
			}
		}
		idx.mu.Unlock()
		if len(qualified) > 0 {
			return sortedSymbolsByQuery(qualified, strings.ToLower(q))
		}
	} else {
		idx.mu.Unlock()
	}

	// Fast path: a bare symbol name (no ":" — the only way the slow path's
	// "file:name" exact-match clause below could ever match, so a
	// colon-free query can only ever match by Name, the exact set
	// idx.symbolsByName already indexes) hits idx.symbolsByName in O(1)
	// instead of copying and linearly scanning every symbol in the repo.
	// Profiling a real trace_symbol call on a 19,000-symbol repo found
	// this full-repo copy-then-scan (plus a lowercased substring check
	// against every symbol's name, even after an exact match already
	// existed) as the single largest cost in the call — a concrete, fixable
	// cause rather than assuming JSON/response
	// shaping was the only lever left. Falls through to the original
	// full-scan path unchanged for anything this can't prove identical to
	// the slow path: a "file:name"-qualified query, a substring/fuzzy
	// query, or a bare name idx.symbolsByName has no entry for (which
	// still needs the fuzzy fallback below).
	if !strings.Contains(q, ":") {
		idx.ensureFileSymbolIndex()
		idx.mu.Lock()
		byName := idx.symbolsByName[q]
		exact := append([]CGPSymbol(nil), byName...)
		idx.mu.Unlock()
		if len(exact) > 0 {
			return sortedSymbolsByQuery(exact, strings.ToLower(q))
		}
	}

	idx.mu.Lock()
	syms := make([]CGPSymbol, 0, len(idx.Symbols))
	for _, sym := range idx.Symbols {
		syms = append(syms, sym)
	}
	idx.mu.Unlock()

	var exact []CGPSymbol
	var fuzzy []CGPSymbol
	for _, sym := range syms {
		if sym.Name == q || sym.File+":"+sym.Name == q {
			exact = append(exact, sym)
			continue
		}
		if strings.Contains(strings.ToLower(sym.Name), strings.ToLower(q)) {
			fuzzy = append(fuzzy, sym)
		}
	}
	if len(exact) > 0 {
		return sortedSymbolsByQuery(exact, strings.ToLower(q))
	}
	return sortedSymbolsByQuery(fuzzy, strings.ToLower(q))
}

func findSymbolsInGraph(snap symbolGraphSnapshot, q string) []CGPSymbol {
	if sym, ok := snap.Symbols[q]; ok {
		return []CGPSymbol{sym}
	}
	lowerQuery := strings.ToLower(q)
	qualifiedQuery := strings.Contains(q, ".") && !strings.ContainsAny(q, "/\\:")
	if qualifiedQuery {
		childName := q[strings.LastIndexByte(q, '.')+1:]
		var qualified []CGPSymbol
		for _, id := range snap.SymbolsByName[childName] {
			sym, ok := snap.Symbols[id]
			if ok && symbolQualifiedName(sym, snap.Symbols) == q {
				qualified = append(qualified, sym)
			}
		}
		if len(qualified) > 0 {
			return sortedSymbolsByQuery(qualified, lowerQuery)
		}
	} else if !strings.Contains(q, ":") {
		ids := snap.SymbolsByName[q]
		exact := make([]CGPSymbol, 0, len(ids))
		for _, id := range ids {
			if sym, ok := snap.Symbols[id]; ok {
				exact = append(exact, sym)
			}
		}
		if len(exact) > 0 {
			return sortedSymbolsByQuery(exact, lowerQuery)
		}
	}

	var exact []CGPSymbol
	var fuzzy []CGPSymbol
	for _, sym := range snap.Symbols {
		if sym.Name == q || sym.File+":"+sym.Name == q {
			exact = append(exact, sym)
			continue
		}
		if strings.Contains(strings.ToLower(sym.Name), lowerQuery) {
			fuzzy = append(fuzzy, sym)
		}
	}
	if len(exact) > 0 {
		return sortedSymbolsByQuery(exact, lowerQuery)
	}
	return sortedSymbolsByQuery(fuzzy, lowerQuery)
}

const maxSymbolQualificationDepth = 25

// symbolQualifiedName builds the shortest useful declaration-qualified name
// from ParentID links. File parents are intentionally omitted: file-qualified
// lookup already has the unambiguous "file:name" syntax, while callers asking
// for Class.method should not have to include a source path.
func symbolQualifiedName(sym CGPSymbol, byID map[string]CGPSymbol) string {
	parts := []string{sym.Name}
	parentID := sym.ParentID
	for depth := 0; parentID != "" && depth < maxSymbolQualificationDepth; depth++ {
		parent, ok := byID[parentID]
		if !ok || parent.Kind == "file" {
			break
		}
		parts = append(parts, parent.Name)
		parentID = parent.ParentID
	}
	for left, right := 0, len(parts)-1; left < right; left, right = left+1, right-1 {
		parts[left], parts[right] = parts[right], parts[left]
	}
	return strings.Join(parts, ".")
}

func summarizeSymbol(sym CGPSymbol) CGPSymbolSummary {
	return CGPSymbolSummary{
		ID:                  sym.ID,
		Name:                sym.Name,
		Kind:                sym.Kind,
		Language:            sym.Language,
		File:                sym.File,
		StartLine:           sym.StartLine,
		Signature:           sym.Signature,
		Docstring:           sym.Docstring,
		ReturnTypes:         append([]string(nil), sym.ReturnTypes...),
		Complexity:          sym.Complexity,
		LoopDepth:           sym.LoopDepth,
		TransitiveLoopDepth: sym.TransitiveLoopDepth,
		LinearScanInLoop:    sym.LinearScanInLoop,
		AllocInLoop:         sym.AllocInLoop,
		RecursionInLoop:     sym.RecursionInLoop,
	}
}

// summarizeSymbolCompact is summarizeSymbol's lean counterpart: only the
// fields needed to identify a symbol and locate it in source (name, kind,
// file, startLine) — no Signature/Docstring/ReturnTypes/hot-path fields,
// the dominant per-entry cost once a trace has more than a handful of
// callers/callees. Used by TraceSymbolOptions.Compact, matching the same
// lean caller/callee shape inspect_symbol's format=node already uses
// (compactNodeSymbolSummary in inspect.go, includeDetails=false).
//
// ID and Language are deliberately also dropped here, not just the
// detail/hot-path fields: response audits found that ID's full
// qualified-path form (e.g. "symbol:kotlin:method:<full-file-path>:
// Class.method") was, on a real multi-thousand-file repo, the single
// largest per-entry cost in compact output — and unlike Signature/
// Docstring, it isn't optional ballast, it's pure duplication: every
// caller/callee already carries File+StartLine+Name, which is exactly
// what re-querying trace_symbol or fetch_context needs; nothing downstream
// requires re-deriving the literal ID string from a caller/callee entry.
// Language is the same story — almost always inferable from File's
// extension, and on the rare genuine cross-language call, the file
// extension carries that information anyway.
func summarizeSymbolCompact(sym CGPSymbol) CGPSymbolSummary {
	return CGPSymbolSummary{
		Name:      sym.Name,
		Kind:      sym.Kind,
		File:      sym.File,
		StartLine: sym.StartLine,
	}
}

// summarizeSymbolCandidate is the compact ambiguous-candidate shape. ID is
// deliberately dropped: it embeds the full file path a second time (the same
// string already sent in File), and re-querying with "<file>:<name>" resolves
// exactly via findSymbols' file:name clause — same round-trip count as
// selecting by id, at ~35-45 fewer tokens per candidate. Signature is retained
// to distinguish same-file overloads (the one case file:name stays ambiguous,
// which a re-query then auto-expands inline via maxAmbiguousTraceDetails).
func summarizeSymbolCandidate(sym CGPSymbol) CGPSymbolSummary {
	return CGPSymbolSummary{
		Name:      sym.Name,
		Kind:      sym.Kind,
		File:      sym.File,
		StartLine: sym.StartLine,
		Signature: sym.Signature,
	}
}

// compactMainSymbolJSON is the wire shape TraceSymbolResponse/ImpactResponse
// render their own queried-symbol Symbol field as when Compact is set (see
// each type's compactMainSymbol field and MarshalJSON below) — kept
// completely separate from CGPSymbol/CGPSymbolSummary's own JSON tags
// rather than adding omitempty to those shared types' ID/StartColumn/
// EndLine/EndColumn/Exported/ParentID fields, which are populated for
// every other consumer and would carry needless risk for a change scoped
// to exactly two call sites. Confidence is CGPSymbol-only (ImpactResponse's
// Symbol is a CGPSymbolSummary, which has no such field); omitempty drops
// it cleanly when absent.
//
// Signature/Docstring are kept (the actual answer to what was asked
// about — Docstring already trimmed to its first sentence by the caller
// before this struct is built, see firstSentenceOrLimit); Name/Kind/File/
// StartLine are kept (identify+locate, the same floor every other compact
// entry keeps); ID/Language/StartColumn/EndLine/EndColumn/Exported/
// ParentID are dropped — found while closing more of the remaining
// known-symbol-lookup token cost because none of these fields are needed by
// compact node/caller output.
type compactMainSymbolJSON struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	File       string `json:"file"`
	StartLine  int    `json:"startLine"`
	Signature  string `json:"signature,omitempty"`
	Docstring  string `json:"docstring,omitempty"`
	Confidence string `json:"confidence,omitempty"`
}

// MarshalJSON renders Symbol via compactMainSymbolJSON instead of
// CGPSymbol's full shape when compactMainSymbol is set (only ever true
// when built by traceSymbolFromSnapshot with TraceSymbolOptions.Compact —
// the default, non-compact path is byte-for-byte unaffected by this
// method existing at all, since it falls straight through to the
// type-aliased default marshaling below). Uses the standard Go
// "type alias + embed with an overriding field" idiom so every other
// field keeps its existing tag-driven behavior unchanged; only Symbol's
// rendering is intercepted.
func (resp TraceSymbolResponse) MarshalJSON() ([]byte, error) {
	type alias TraceSymbolResponse
	if !resp.compactMainSymbol || resp.Symbol == nil {
		return json.Marshal(alias(resp))
	}
	lean := compactMainSymbolJSON{
		Name:       resp.Symbol.Name,
		Kind:       resp.Symbol.Kind,
		File:       resp.Symbol.File,
		StartLine:  resp.Symbol.StartLine,
		Signature:  resp.Symbol.Signature,
		Docstring:  resp.Symbol.Docstring,
		Confidence: resp.Symbol.Confidence,
	}
	return json.Marshal(struct {
		alias
		Symbol *compactMainSymbolJSON `json:"symbol,omitempty"`
	}{alias: alias(resp), Symbol: &lean})
}

// compactDocstringMaxLen bounds firstSentenceOrLimit's output when a
// docstring has no early sentence break (a single long run-on paragraph,
// or a language convention with no sentence-terminating periods at all).
const compactDocstringMaxLen = 160

// firstSentenceOrLimit trims a docstring to its first sentence (the actual
// "what does this do" summary almost every doc-comment convention leads
// with), falling back to a hard length cap if no early sentence break is
// found. Used for the queried symbol's own Docstring in Compact mode
// (trace_symbol, impact) — the one field deliberately *not* dropped
// outright, since it's the actual answer to what was asked about, not
// incidental per-entry ballast like a caller's docstring. Docstrings often pad a one-sentence
// summary with boilerplate that adds bulk without value for an agent
// reading code: "@param" tags, "[Report a problem](url)" links, "you can
// learn more at [...]" pointers — none of that survives a first-sentence
// cut, which is a general, convention-agnostic rule rather than a pattern
// match tied to one doc generator's specific boilerplate.
//
// The sentence break must be a ". " (or a trailing ".") after at least
// minSentenceLen characters, so a stray abbreviation near the very start
// ("e.g. " as the first two words) doesn't trigger a near-empty result.
func firstSentenceOrLimit(s string) string {
	const minSentenceLen = 12
	if s == "" {
		return s
	}
	if idx := strings.Index(s, ". "); idx >= minSentenceLen {
		return s[:idx+1]
	}
	if strings.HasSuffix(s, ".") && len(s) >= minSentenceLen {
		return s
	}
	if len(s) <= compactDocstringMaxLen {
		return s
	}
	cut := compactDocstringMaxLen
	for cut > 0 && s[cut] != ' ' {
		cut--
	}
	if cut == 0 {
		cut = compactDocstringMaxLen
	}
	return strings.TrimRight(s[:cut], " ") + "…"
}

func summarizeSymbols(symbols []CGPSymbol) []CGPSymbolSummary {
	out := make([]CGPSymbolSummary, 0, len(symbols))
	for _, sym := range sortedSymbols(symbols) {
		out = append(out, summarizeSymbol(sym))
	}
	return out
}

func sortedSymbols(symbols []CGPSymbol) []CGPSymbol {
	isBackup := memoizedBackupCheck()
	sort.Slice(symbols, func(i, j int) bool {
		// Backup/dead copies sort after active source so an ambiguous-name
		// candidate list does not lead with a stale snapshot.
		if bi, bj := isBackup(symbols[i].File), isBackup(symbols[j].File); bi != bj {
			return !bi
		}
		if symbols[i].File != symbols[j].File {
			return symbols[i].File < symbols[j].File
		}
		if symbols[i].StartLine != symbols[j].StartLine {
			return symbols[i].StartLine < symbols[j].StartLine
		}
		return symbols[i].Name < symbols[j].Name
	})
	return symbols
}

// memoizedBackupCheck returns an isBackupOrDeadFile wrapper that caches per
// file path — sort comparators call it O(n log n) times over few distinct
// files, and the underlying check is a regex.
func memoizedBackupCheck() func(string) bool {
	cache := map[string]bool{}
	return func(file string) bool {
		v, ok := cache[file]
		if !ok {
			v = isBackupOrDeadFile(file)
			cache[file] = v
		}
		return v
	}
}

func sortedSymbolsByQuery(symbols []CGPSymbol, q string) []CGPSymbol {
	sort.SliceStable(symbols, func(i, j int) bool {
		if q != "" {
			left := symbolSearchScore(summarizeSymbol(symbols[i]), q)
			right := symbolSearchScore(summarizeSymbol(symbols[j]), q)
			if left != right {
				return left > right
			}
		}
		if symbols[i].File != symbols[j].File {
			return symbols[i].File < symbols[j].File
		}
		if symbols[i].StartLine != symbols[j].StartLine {
			return symbols[i].StartLine < symbols[j].StartLine
		}
		return symbols[i].Name < symbols[j].Name
	})
	return symbols
}

// containingSymbolFast uses a per-file precomputed list, sorted by StartLine,
// to attribute a call site to its enclosing symbol in O(log n) rather than
// O(symbols).
func (idx *Index) containingSymbolFast(file string, line int) CGPSymbol {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	syms := idx.symbolsByFile[file]
	if len(syms) == 0 {
		return CGPSymbol{}
	}
	// Largest StartLine <= line. Then walk backwards while EndLine >= line so
	// the most-deeply-nested enclosing symbol wins (e.g. a method inside a
	// class wins over the class itself).
	lo, hi := 0, len(syms)
	for lo < hi {
		mid := (lo + hi) / 2
		if syms[mid].StartLine <= line {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	var best CGPSymbol
	for i := lo - 1; i >= 0; i-- {
		s := syms[i]
		if s.Kind == "file" {
			continue
		}
		if s.EndLine == 0 || (s.StartLine <= line && line <= s.EndLine) {
			if best.ID == "" || s.StartLine >= best.StartLine {
				best = s
			}
		}
	}
	return best
}

func (idx *Index) componentSymbolForFile(file string) CGPSymbol {
	idx.ensureFileSymbolIndex()
	idx.mu.Lock()
	defer idx.mu.Unlock()
	var out CGPSymbol
	for _, sym := range idx.symbolsByFile[file] {
		if sym.Kind != "component" {
			continue
		}
		if out.ID == "" || sym.StartLine < out.StartLine {
			out = sym
		}
	}
	return out
}

// ensureFileSymbolIndex (re)builds the per-file, per-name, and per-parent
// symbol indexes from idx.Symbols in a single pass. Cheap if already built
// (a single mutex round-trip); safe to call from any number of goroutines
// before or during the parallel relation-scanning phase. These indexes back
// resolveSymbolCall, findClassByNameLocked, findMethodInClassLocked,
// resolveSuperCall, componentSymbolForFile, findVueAPISymbolForComponent, and
// linkTemplateClassUsages, turning what would otherwise be O(len(idx.Symbols))
// scans per call/edge into O(1) map lookups — essential for large codebases
// where Symbols can hold tens or hundreds of thousands of entries and these
// lookups happen once per call edge.
func (idx *Index) ensureFileSymbolIndex() {
	idx.mu.Lock()
	if idx.symbolsByFile == nil {
		idx.symbolsByFile = map[string][]CGPSymbol{}
	}
	if idx.symbolIndexBuilt {
		idx.mu.Unlock()
		return
	}
	symbols := make([]CGPSymbol, 0, len(idx.Symbols))
	for _, sym := range idx.Symbols {
		symbols = append(symbols, sym)
	}
	idx.mu.Unlock()
	// idx.Symbols is a Go map, so its iteration order changes between builds
	// and processes. Several resolvers intentionally choose the first matching
	// child when the language grammar does not provide enough signature detail
	// to distinguish overloads. Build every derived lookup from stable symbol
	// ID order so those conservative choices, emitted edges, and architecture
	// scores are reproducible for identical source.
	sort.Slice(symbols, func(i, j int) bool {
		return symbols[i].ID < symbols[j].ID
	})

	// The O(len(Symbols)) build (and the per-file sort) runs without holding
	// idx.mu. It used to run entirely under the lock; on a CPU-contended
	// machine, a `mamari serve --watch` rebake calls invalidate+ensure on
	// every single file edit, and a slow scheduler tick during that
	// full-repo loop would block every other goroutine waiting on idx.mu —
	// including concurrent MCP query handling — for the full duration.
	// Mutex profiling confirmed this as the source of query-latency spikes
	// under load
	// (GC pauses and per-file cost were both ruled out first). Mirrors the
	// same lock-minimizing shape already used by ensureCodeSearchIndex.
	symbolsByFile := map[string][]CGPSymbol{}
	symbolsByName := map[string][]CGPSymbol{}
	childrenByParent := map[string][]CGPSymbol{}
	for _, sym := range symbols {
		symbolsByFile[sym.File] = append(symbolsByFile[sym.File], sym)
		symbolsByName[sym.Name] = append(symbolsByName[sym.Name], sym)
		if sym.ParentID != "" {
			childrenByParent[sym.ParentID] = append(childrenByParent[sym.ParentID], sym)
		}
	}
	for f, list := range symbolsByFile {
		sort.Slice(list, func(i, j int) bool {
			if list[i].StartLine != list[j].StartLine {
				return list[i].StartLine < list[j].StartLine
			}
			return list[i].EndLine > list[j].EndLine
		})
		symbolsByFile[f] = list
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.symbolIndexBuilt {
		// Someone else finished building (or invalidated+rebuilt) while we
		// were off-lock — keep theirs rather than overwrite with a snapshot
		// that may already be stale.
		return
	}
	idx.symbolsByFile = symbolsByFile
	idx.symbolsByName = symbolsByName
	idx.childrenByParent = childrenByParent
	idx.symbolIndexBuilt = true
}

func (idx *Index) invalidateFileSymbolIndex() {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.symbolsByFile = nil
	idx.symbolsByName = nil
	idx.childrenByParent = nil
	idx.symbolIndexBuilt = false
	idx.orderedSymbolIDs = nil
}

// languageFamily groups language tags that freely call into each other
// within this codebase's existing resolution model: JS/TS/Vue are one
// interoperable ecosystem (a .js file can call into a .ts module's export;
// a Vue SFC's own File-level "vue" tag never reflects its <script> block's
// "javascript"/"typescript" symbol tags, since the file isn't one
// language). Every other language is its own family. Used by
// resolveSymbolCall to scope bare-name candidate search without
// reintroducing false cross-language ambiguity for this one pre-existing,
// intentionally-blurred boundary.
func languageFamily(lang string) string {
	switch lang {
	case "javascript", "typescript", "vue":
		return "js"
	default:
		return lang
	}
}

// resolveSymbolCall maps a callee expression ("foo", "obj.foo", "this.bar")
// to a target symbol ID, classifying the strength of the match. Callers use
// the confidence/reason to decide whether the edge is trustworthy and, when
// it is unresolved, why.
//
//   - ConfExact: unique same-file declaration of the bare name, called by
//     its bare name directly (e.g. `helper()`) — strong, structural evidence.
//   - ConfScoped: unique cross-file declaration of the bare name, called by
//     its bare name directly (single global candidate) — strong evidence.
//   - ConfHeuristic: either multiple candidates with no scope hint to pick
//     between them, OR a unique same-file/global name match for a DOTTED
//     call on an unknown receiver (e.g. `obj.helper()` where `obj`'s type
//     isn't known — every other dotted-call resolution path, this/super/
//     require-bound/return-bound, already failed by the time this function
//     is reached for a dotted call, so the receiver really is unknown here).
//     A name match in that case is a coincidence-based guess, not structural
//     evidence: nothing says `obj` is actually an instance of whatever
//     declares `helper`. We still return the
//     candidate's id (recall matters — this fallback is what resolves the
//     common "imported singleton instance" pattern correctly), just at a
//     confidence level that's honest about it being a guess.
//   - ConfUnresolved: nothing matched, with a reason code that explains why.
//
// The function never picks an arbitrary winner among ambiguous candidates;
// returning an unresolved target keeps caller graphs honest.
func resolveSymbolCall(idx *Index, file, call string) (target, confidence, reason string) {
	name := call
	dotted := strings.Contains(name, ".")
	if dotted {
		parts := strings.Split(name, ".")
		name = parts[len(parts)-1]
	}
	idx.ensureFileSymbolIndex()
	// A call expression can only directly target a symbol in its own
	// language family (modulo FFI, which mamari doesn't attempt to resolve
	// here) — scoping the candidate search to lang prevents an unrelated
	// symbol that merely shares a name in a genuinely different language
	// from making an otherwise-unique name look ambiguous. This matters more
	// as more languages are indexed in the same repo: a name unique within
	// one language must not become falsely "ambiguous" just because some
	// other language's code happens to reuse it. Comparison is by
	// languageFamily, not exact string equality: JS/TS files routinely call
	// into each other (a .js file importing a .ts module's export), and a
	// Vue SFC's File-level "vue" tag doesn't reflect its <script> block's
	// own "javascript"/"typescript" symbol tags — none of that is a
	// cross-language call in the sense this filter cares about.
	lang := languageFamily(idx.languageFor(file))
	idx.mu.Lock()
	defer idx.mu.Unlock()
	var sameFile []CGPSymbol
	var global []CGPSymbol
	for _, sym := range idx.symbolsByName[name] {
		if sym.Kind == "file" || languageFamily(sym.Language) != lang {
			continue
		}
		// Vue API metadata (defineProps/defineEmits/defineModel entries)
		// describes a component's public surface; none of it is callable.
		// Without this exclusion a member call like `loading.close()`
		// bare-name matched a same-file emit-event string 'close'
		// (defineEmits(['close'])) — a confident edge into a non-function.
		if sym.Kind == "vue-prop" || sym.Kind == "vue-emit" || sym.Kind == "vue-model" {
			continue
		}
		if sym.File == file {
			sameFile = append(sameFile, sym)
		}
		global = append(global, sym)
	}
	if len(sameFile) == 1 {
		if dotted {
			return sameFile[0].ID, ConfHeuristic, ""
		}
		return sameFile[0].ID, ConfExact, ""
	}
	if len(sameFile) > 1 {
		// Multiple declarations in the same file (overloads, shadowed names).
		return "unresolved:" + call, ConfUnresolved, ReasonAmbiguousName
	}
	if len(global) == 1 {
		if dotted {
			return global[0].ID, ConfHeuristic, ""
		}
		return global[0].ID, ConfScoped, ""
	}
	if len(global) > 1 {
		return "unresolved:" + call, ConfUnresolved, ReasonAmbiguousName
	}
	if dotted {
		return "unresolved:" + call, ConfUnresolved, ReasonDynamicReceiver
	}
	return "unresolved:" + call, ConfUnresolved, ReasonMissingImport
}

// jsRequireBindingFiles maps each local name bound by an import/require in
// `file` (e.g. `const { notificationService } = require('../services/x')`,
// or `import { x } from './y'`) to the indexed file the spec resolves to.
// Call resolution uses this to follow `localName.method(...)` to the right
// file even when `method` alone is ambiguous repo-wide — JS/TS has no
// `idx.varTypes` equivalent (unlike Go/Java/C#), so this is the only signal
// available for cross-file method-call resolution on imported bindings.
func jsRequireBindingFiles(idx *Index, file string, imports []ScannedImport) map[string]string {
	out := map[string]string{}
	for _, imp := range imports {
		targetFile := resolveImportSpecToIndexedFile(idx, file, imp.Spec)
		if targetFile == "" {
			continue
		}
		for _, binding := range imp.Bindings {
			if binding.Local == "" {
				continue
			}
			out[binding.Local] = targetFile
		}
	}
	return out
}

type jsLocalReturnAssignment struct {
	scopeID    string
	name       string
	targetFile string
	pos        int
}

type jsRawCallAssignment struct {
	scopeID string
	name    string
	callee  string
	pos     int
}

// jsReturnFileInference connects JS/TS patterns that are common in service
// singletons/repositories but invisible to a name-only resolver:
//
//	this.emailHandler = require("./emailHandler")
//	return this.emailHandler
//	const emailHandler = this.getEmailHandler()
//	emailHandler.sendEmail(...)
//
//	const repo = new UserRepository()      // UserRepository imported
//	repo.findById(...)
//	function getRepo() { return new UserRepository() }
//	getRepo().findById(...)
//
// The function returns scoped local-variable bindings whose value came from
// either a method with a known import/require-backed return file, or a local
// `new ClassName(...)` instantiation where ClassName is itself an
// imported/required binding. The main call loop can then resolve
// `local.method()` within that file, avoiding a repo-wide ambiguous
// method-name search.
//
// Deliberately out of scope (not a general type-flow engine): property-chain
// receivers (`obj.nested.field.method()`) and conditional assignment
// (`x = cond ? new A() : new B()`).
func jsReturnFileInference(idx *Index, file, body string, baseOffset int, fullContent string, requireBindings map[string]string) []jsLocalReturnAssignment {
	tokens := TokenizeJS(body)
	if len(tokens) == 0 {
		return nil
	}
	starts := lineStarts(fullContent)
	fieldFiles := map[string]string{}
	// Factory helpers keyed by *name*, file-wide: a concise-arrow helper
	// (`const build = () => new Service()`) declared inside one test callback
	// is called by bare name from sibling callbacks, which scope-keyed
	// tracking cannot connect. Same-name helpers with different targets
	// poison the entry ("" = refuse to guess).
	arrowHelperFiles := map[string]string{}
	var rawAssignments []jsRawCallAssignment
	var newInstanceAssignments []jsLocalReturnAssignment

	for i := 0; i < len(tokens); i++ {
		if name, className, ok := jsConciseArrowHelperAt(tokens, i); ok {
			if targetFile := requireBindings[className]; targetFile != "" {
				if existing, seen := arrowHelperFiles[name]; seen && existing != targetFile {
					arrowHelperFiles[name] = ""
				} else if !seen {
					arrowHelperFiles[name] = targetFile
				}
			}
		}
		if field, spec, ok := jsThisFieldRequireAssignment(tokens, i); ok {
			targetFile := resolveImportSpecToIndexedFile(idx, file, spec)
			if targetFile == "" {
				continue
			}
			line, _ := offsetToLineCol(starts, baseOffset+tokens[i].Start)
			from := idx.containingSymbolFast(file, line)
			if from.ID == "" {
				continue
			}
			fieldFiles[jsScopedNameKey(from.ID, field)] = targetFile
			if classID := enclosingClassID(idx, from); classID != "" {
				fieldFiles[jsScopedNameKey(classID, field)] = targetFile
			}
			continue
		}
		if field, className, ok := jsThisFieldNewAssignment(tokens, i); ok {
			// `this.repo = new Repo()` — record the type the same way every
			// tree-sitter language's "self.attr = ClassName(...)" rule does
			// (idx.varTypes, scoped to the enclosing class so any sibling
			// method's `this.repo.find()` can use it too), since plain JS
			// classes have no separate typed field declaration to fall
			// back on the way a TypeScript class would.
			line, _ := offsetToLineCol(starts, baseOffset+tokens[i].Start)
			from := idx.containingSymbolFast(file, line)
			classID := enclosingClassID(idx, from)
			if classID == "" {
				continue
			}
			idx.mu.Lock()
			if idx.varTypes == nil {
				idx.varTypes = map[string]map[string]string{}
			}
			if idx.varTypes[classID] == nil {
				idx.varTypes[classID] = map[string]string{}
			}
			idx.varTypes[classID][field] = className
			idx.mu.Unlock()
			continue
		}
		if assignment, ok := jsNewInstanceAssignmentAt(tokens, i); ok {
			targetFile := requireBindings[assignment.className]
			if targetFile == "" {
				continue
			}
			line, _ := offsetToLineCol(starts, baseOffset+assignment.pos)
			from := idx.containingSymbolFast(file, line)
			if from.ID == "" {
				continue
			}
			newInstanceAssignments = append(newInstanceAssignments, jsLocalReturnAssignment{
				scopeID:    from.ID,
				name:       assignment.name,
				targetFile: targetFile,
				pos:        assignment.pos,
			})
			continue
		}
		if assignment, ok := jsCallAssignmentAt(tokens, i); ok {
			line, _ := offsetToLineCol(starts, baseOffset+assignment.pos)
			from := idx.containingSymbolFast(file, line)
			if from.ID == "" {
				continue
			}
			assignment.scopeID = from.ID
			rawAssignments = append(rawAssignments, assignment)
		}
	}

	methodReturnFiles := map[string]string{}
	for i := 0; i < len(tokens); i++ {
		exprIdx := -1
		switch {
		case tokens[i].Kind == TokKeyword && tokens[i].Value == "return":
			exprIdx = i + 1
		case tokens[i].Kind == TokPunct && tokens[i].Value == "=>":
			// A concise arrow body IS its return expression:
			//   const buildService = (overrides = {}) => new Service({...})
			// carries the same return-type evidence as `return new Service()`
			// but has no `return` token, so before this case a local built
			// via such a helper (`const svc = buildService()`) never
			// resolved its method calls when reached only through a concise-arrow
			// test helper. Block bodies (`=> { ... }`) are skipped: their
			// `return` statements are handled by the case above.
			next := i + 1
			for next < len(tokens) && (tokens[next].Kind == TokComment || tokens[next].Kind == TokLineComment) {
				next++
			}
			if next < len(tokens) && !(tokens[next].Kind == TokPunct && tokens[next].Value == "{") {
				exprIdx = next
			}
		}
		if exprIdx < 0 {
			continue
		}
		line, _ := offsetToLineCol(starts, baseOffset+tokens[i].Start)
		from := idx.containingSymbolFast(file, line)
		if from.ID == "" {
			continue
		}
		if targetFile := jsReturnTargetFile(idx, file, tokens, exprIdx, from, fieldFiles, requireBindings, newInstanceAssignments, tokens[i].Start); targetFile != "" {
			methodReturnFiles[from.ID] = targetFile
		}
	}

	// TypeScript return-type annotations: `function getRepo(): UserRepo {}` or
	// `getRepo(): Promise<UserRepo> {}`. A value assigned from calling such a
	// function carries the annotated (Promise/Array-unwrapped) type, so
	// `const r = getRepo(); r.find()` resolves — the declared-type complement
	// to the return-*body* inference above. Only fills a function that has no
	// body-derived return file yet, so concrete `return new X()` evidence wins.
	for i := 0; i < len(tokens); i++ {
		if !(tokens[i].Kind == TokPunct && tokens[i].Value == ")") {
			continue
		}
		typeName, ok := jsReturnTypeAnnotationAt(tokens, i)
		if !ok || typeName == "" {
			continue
		}
		targetFile := jsTypeTargetFile(idx, file, typeName, requireBindings)
		if targetFile == "" {
			continue
		}
		line, _ := offsetToLineCol(starts, baseOffset+tokens[i].Start)
		from := idx.containingSymbolFast(file, line)
		if from.ID == "" || !isJSTSFunctionLikeKind(from.Kind) {
			continue
		}
		if _, exists := methodReturnFiles[from.ID]; !exists {
			methodReturnFiles[from.ID] = targetFile
		}
	}

	out := make([]jsLocalReturnAssignment, 0, len(rawAssignments)+len(newInstanceAssignments))
	out = append(out, newInstanceAssignments...)

	if (len(methodReturnFiles) > 0 || len(arrowHelperFiles) > 0) && len(rawAssignments) > 0 {
		sort.SliceStable(rawAssignments, func(i, j int) bool { return rawAssignments[i].pos < rawAssignments[j].pos })
		for _, assignment := range rawAssignments {
			if targetFile, ok := arrowHelperFiles[assignment.callee]; ok && targetFile != "" {
				out = append(out, jsLocalReturnAssignment{
					scopeID:    assignment.scopeID,
					name:       assignment.name,
					targetFile: targetFile,
					pos:        assignment.pos,
				})
				continue
			}
			from := CGPSymbol{ID: assignment.scopeID, File: file}
			idx.mu.Lock()
			if sym, ok := idx.Symbols[assignment.scopeID]; ok {
				from = sym
			}
			idx.mu.Unlock()
			target, confidence, _ := resolveJSTSAssignmentCallee(idx, file, from, assignment.callee)
			if confidence != ConfExact && confidence != ConfScoped {
				continue
			}
			targetFile := methodReturnFiles[target]
			if targetFile == "" {
				continue
			}
			out = append(out, jsLocalReturnAssignment{
				scopeID:    assignment.scopeID,
				name:       assignment.name,
				targetFile: targetFile,
				pos:        assignment.pos,
			})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func jsAssignedReturnFile(assignments []jsLocalReturnAssignment, scopeID, name string, beforePos int) string {
	var found jsLocalReturnAssignment
	for _, assignment := range assignments {
		if assignment.scopeID != scopeID || assignment.name != name || assignment.pos >= beforePos {
			continue
		}
		if found.targetFile == "" || assignment.pos > found.pos {
			found = assignment
		}
	}
	return found.targetFile
}

func jsReceiverTargetFile(idx *Index, assignments []jsLocalReturnAssignment, from CGPSymbol, receiver string, beforePos int) string {
	if target := jsAssignedReturnFile(assignments, from.ID, receiver, beforePos); target != "" {
		return target
	}
	if strings.HasPrefix(receiver, "this.") {
		if classID := enclosingClassID(idx, from); classID != "" {
			return jsAssignedReturnFile(assignments, classID, receiver, beforePos)
		}
	}
	return ""
}

// jsTypedBindingInference recovers TypeScript's explicit receiver types and
// projects them onto the same scoped "value came from this indexed file"
// representation used by jsReturnFileInference. It covers annotated params,
// locals, class fields, constructor parameter-properties, and simple aliases
// (`this.repo = repo`, `const alias = repo`). A type is trusted only when it
// is anchored to an actual import/namespace binding or a same-file class or
// interface; unanchored names never become confident call edges.
func jsTypedBindingInference(idx *Index, file, body string, baseOffset int, fullContent string, requireBindings map[string]string) []jsLocalReturnAssignment {
	tokens := TokenizeJS(body)
	if len(tokens) == 0 {
		return nil
	}
	starts := lineStarts(fullContent)
	var out []jsLocalReturnAssignment

	for i := 0; i < len(tokens); i++ {
		if !jsIdentLike(tokens[i]) {
			continue
		}
		typeName, ok := jsTypeAnnotationAt(tokens, i)
		if !ok {
			continue
		}
		targetFile := jsTypeTargetFile(idx, file, typeName, requireBindings)
		if targetFile == "" {
			continue
		}
		line, _ := offsetToLineCol(starts, baseOffset+tokens[i].Start)
		from := idx.containingSymbolFast(file, line)
		if from.ID == "" {
			continue
		}
		isParam, open := jsIsParameterBinding(tokens, i)
		isDecl := jsIsTypedVariableDeclaration(tokens, i)
		if !isParam && !isDecl && from.Kind != "class" {
			continue
		}
		out = append(out, jsLocalReturnAssignment{scopeID: from.ID, name: tokens[i].Value, targetFile: targetFile, pos: tokens[i].Start})
		if isParam && jsParameterPropertyModifier(tokens, open, i) {
			if classID := enclosingClassID(idx, from); classID != "" {
				out = append(out, jsLocalReturnAssignment{scopeID: classID, name: "this." + tokens[i].Value, targetFile: targetFile, pos: tokens[i].Start})
			}
		} else if from.Kind == "class" && !isParam {
			out = append(out, jsLocalReturnAssignment{scopeID: from.ID, name: "this." + tokens[i].Value, targetFile: targetFile, pos: tokens[i].Start})
		}
	}

	// Propagate explicit aliases in source order. The fixed-point rounds cover
	// short chains while remaining bounded on pathological assignments.
	for pass := 0; pass < 3; pass++ {
		changed := false
		for i := 0; i < len(tokens); i++ {
			lhs, rhs, pos, ok := jsSimpleAliasAssignmentAt(tokens, i)
			if !ok {
				continue
			}
			line, _ := offsetToLineCol(starts, baseOffset+pos)
			from := idx.containingSymbolFast(file, line)
			if from.ID == "" {
				continue
			}
			targetFile := jsReceiverTargetFile(idx, out, from, rhs, pos)
			if targetFile == "" {
				continue
			}
			scopeID := from.ID
			if strings.HasPrefix(lhs, "this.") {
				scopeID = enclosingClassID(idx, from)
			}
			if scopeID == "" || jsAssignedReturnFile(out, scopeID, lhs, pos+1) == targetFile {
				continue
			}
			out = append(out, jsLocalReturnAssignment{scopeID: scopeID, name: lhs, targetFile: targetFile, pos: pos})
			changed = true
		}
		if !changed {
			break
		}
	}
	return out
}

func jsTypeAnnotationAt(tokens []Token, nameIdx int) (string, bool) {
	j := jsSkipTrivia(tokens, nameIdx+1)
	if jsTokenPunct(tokens, j, "?") {
		j = jsSkipTrivia(tokens, j+1)
	}
	if !jsTokenPunct(tokens, j, ":") {
		return "", false
	}
	j = jsSkipTrivia(tokens, j+1)
	if j >= len(tokens) || !jsIdentLike(tokens[j]) {
		return "", false
	}
	return tokens[j].Value, true
}

// jsReturnTypeAnnotationAt reports the return type when tokens[i] is the `)`
// closing a function/arrow parameter list immediately followed by
// `: <type> {` or `: <type> =>`. It returns the unwrapped root type name (or
// "" for union/intersection/ambiguous types, which must not be guessed).
func jsReturnTypeAnnotationAt(tokens []Token, i int) (string, bool) {
	j := i + 1
	if j >= len(tokens) || !(tokens[j].Kind == TokPunct && tokens[j].Value == ":") {
		return "", false
	}
	j++
	start := j
	angle := 0
	for j < len(tokens) && j-start <= 40 {
		t := tokens[j]
		if t.Kind == TokPunct {
			switch t.Value {
			case "<":
				angle++
			case ">":
				if angle > 0 {
					angle--
				}
			case ">>":
				angle -= 2
				if angle < 0 {
					angle = 0
				}
			case "{", "=>":
				if angle == 0 {
					return jsExtractTypeRoot(tokens, start, j), true
				}
			case "(", ")", ";", ",", "=":
				if angle == 0 {
					return "", false
				}
			}
		}
		j++
	}
	return "", false
}

// jsExtractTypeRoot pulls the meaningful type identifier from a return-type
// token span: `UserRepo` → UserRepo, `Promise<UserRepo>`/`Array<UserRepo>` →
// UserRepo. Union/intersection types and multi-identifier types without a
// known wrapper return "" (ambiguous — don't guess).
func jsExtractTypeRoot(tokens []Token, start, end int) string {
	var idents []string
	for k := start; k < end; k++ {
		if tokens[k].Kind == TokIdent || tokens[k].Kind == TokKeyword {
			idents = append(idents, tokens[k].Value)
		} else if tokens[k].Kind == TokPunct && (tokens[k].Value == "|" || tokens[k].Value == "&") {
			return ""
		}
	}
	if len(idents) == 0 {
		return ""
	}
	switch idents[0] {
	case "Promise", "Array", "Readonly", "Awaited", "ReadonlyArray":
		if len(idents) >= 2 {
			return idents[1]
		}
		return ""
	}
	if len(idents) == 1 {
		return idents[0]
	}
	return ""
}

func jsTypeTargetFile(idx *Index, file, typeName string, requireBindings map[string]string) string {
	if target := requireBindings[typeName]; target != "" {
		return target
	}
	idx.ensureFileSymbolIndex()
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for _, sym := range idx.symbolsByName[typeName] {
		if sym.File == file && (sym.Kind == "class" || sym.Kind == "interface" || sym.Kind == "type") {
			return file
		}
	}
	return ""
}

func jsIsTypedVariableDeclaration(tokens []Token, nameIdx int) bool {
	for j := jsSkipTriviaBack(tokens, nameIdx-1); j >= 0; j = jsSkipTriviaBack(tokens, j-1) {
		if tokens[j].Kind == TokKeyword {
			switch tokens[j].Value {
			case "const", "let", "var", "private", "public", "protected", "readonly", "declare", "static":
				return true
			}
		}
		if tokens[j].Kind == TokPunct && (tokens[j].Value == ";" || tokens[j].Value == "{" || tokens[j].Value == "}") {
			break
		}
		if jsIdentLike(tokens[j]) {
			break
		}
	}
	return false
}

func jsIsParameterBinding(tokens []Token, nameIdx int) (bool, int) {
	depth := 0
	open := -1
	for j := nameIdx - 1; j >= 0; j-- {
		if tokens[j].Kind != TokPunct {
			continue
		}
		switch tokens[j].Value {
		case ")":
			depth++
		case "(":
			if depth == 0 {
				open = j
				j = -1
			} else {
				depth--
			}
		case "{", "}":
			if depth == 0 {
				return false, -1
			}
		}
	}
	if open < 0 {
		return false, -1
	}
	for j := open + 1; j < nameIdx; j++ {
		if jsTokenPunct(tokens, j, "{") || jsTokenPunct(tokens, j, "}") {
			return false, -1
		}
	}
	close := jsMatchingParenToken(tokens, open)
	if close < 0 {
		return false, -1
	}
	j := jsSkipTrivia(tokens, close+1)
	if jsTokenPunct(tokens, j, ":") {
		j++
		for j < len(tokens) && !jsTokenPunct(tokens, j, "{") && !jsTokenPunct(tokens, j, "=>") && !jsTokenPunct(tokens, j, ";") {
			j++
		}
	}
	return jsTokenPunct(tokens, j, "{") || jsTokenPunct(tokens, j, "=>"), open
}

func jsMatchingParenToken(tokens []Token, open int) int {
	depth := 0
	for i := open; i < len(tokens); i++ {
		if jsTokenPunct(tokens, i, "(") {
			depth++
		} else if jsTokenPunct(tokens, i, ")") {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func jsParameterPropertyModifier(tokens []Token, open, nameIdx int) bool {
	for i := open + 1; i < nameIdx; i++ {
		if tokens[i].Kind != TokKeyword {
			continue
		}
		switch tokens[i].Value {
		case "private", "public", "protected", "readonly":
			return true
		}
	}
	return false
}

func jsSimpleAliasAssignmentAt(tokens []Token, i int) (lhs, rhs string, pos int, ok bool) {
	if !jsTokenPunct(tokens, i, "=") {
		return "", "", 0, false
	}
	left := jsSkipTriviaBack(tokens, i-1)
	if left < 0 || !jsIdentLike(tokens[left]) {
		return "", "", 0, false
	}
	lhs = tokens[left].Value
	if dot := jsSkipTriviaBack(tokens, left-1); jsTokenPunct(tokens, dot, ".") {
		root := jsSkipTriviaBack(tokens, dot-1)
		if root < 0 || tokens[root].Value != "this" {
			return "", "", 0, false
		}
		lhs = "this." + lhs
	}
	right := jsSkipTrivia(tokens, i+1)
	if right >= len(tokens) || !jsIdentLike(tokens[right]) {
		return "", "", 0, false
	}
	rhs = tokens[right].Value
	if tokens[right].Value == "this" {
		dot := jsSkipTrivia(tokens, right+1)
		field := jsSkipTrivia(tokens, dot+1)
		if !jsTokenPunct(tokens, dot, ".") || field >= len(tokens) || !jsIdentLike(tokens[field]) {
			return "", "", 0, false
		}
		rhs = "this." + tokens[field].Value
		right = field
	}
	next := jsSkipTrivia(tokens, right+1)
	if jsTokenPunct(tokens, next, "(") || jsTokenPunct(tokens, next, ".") {
		return "", "", 0, false
	}
	return lhs, rhs, tokens[i].Start, true
}

func jsScopedNameKey(scopeID, name string) string {
	return scopeID + "\x00" + name
}

func jsThisFieldRequireAssignment(tokens []Token, i int) (field, spec string, ok bool) {
	j := jsSkipTrivia(tokens, i)
	if j >= len(tokens) || tokens[j].Value != "this" {
		return "", "", false
	}
	j = jsSkipTrivia(tokens, j+1)
	if !jsTokenPunct(tokens, j, ".") {
		return "", "", false
	}
	j = jsSkipTrivia(tokens, j+1)
	if j >= len(tokens) || !jsIdentLike(tokens[j]) {
		return "", "", false
	}
	field = tokens[j].Value
	j = jsSkipTrivia(tokens, j+1)
	if !jsTokenPunct(tokens, j, "=") {
		return "", "", false
	}
	j = jsSkipTrivia(tokens, j+1)
	spec, ok = jsRequireSpecAt(tokens, j)
	return field, spec, ok
}

// jsThisFieldNewAssignment matches `this.field = new ClassName(...)` at
// tokens[i], the plain-JS-class analogue of every other language's
// "self.attr = ClassName(...)" instance-attribute typing rule (Python's
// "self.", Ruby's "@", PHP's "$this->", etc.) — found completely missing
// for JS specifically (TypeScript classes typically declare an explicit
// `private repo: Repo;` field instead, which a separate existing mechanism
// already handles; plain JS constructor-only assignment had nothing at
// all). Reuses jsNewInstanceClassAt for the RHS, the same matcher
// jsNewInstanceAssignmentAt's local-variable case already uses.
func jsThisFieldNewAssignment(tokens []Token, i int) (field, className string, ok bool) {
	j := jsSkipTrivia(tokens, i)
	if j >= len(tokens) || tokens[j].Value != "this" {
		return "", "", false
	}
	j = jsSkipTrivia(tokens, j+1)
	if !jsTokenPunct(tokens, j, ".") {
		return "", "", false
	}
	j = jsSkipTrivia(tokens, j+1)
	if j >= len(tokens) || !jsIdentLike(tokens[j]) {
		return "", "", false
	}
	field = tokens[j].Value
	j = jsSkipTrivia(tokens, j+1)
	if !jsTokenPunct(tokens, j, "=") {
		return "", "", false
	}
	j = jsSkipTrivia(tokens, j+1)
	className, ok = jsNewInstanceClassAt(tokens, j)
	return field, className, ok
}

func jsCallAssignmentAt(tokens []Token, i int) (jsRawCallAssignment, bool) {
	j := jsSkipTrivia(tokens, i)
	if j >= len(tokens) {
		return jsRawCallAssignment{}, false
	}
	if tokens[j].Kind == TokKeyword && (tokens[j].Value == "const" || tokens[j].Value == "let" || tokens[j].Value == "var") {
		nameIdx := jsSkipTrivia(tokens, j+1)
		if nameIdx >= len(tokens) || !jsIdentLike(tokens[nameIdx]) {
			return jsRawCallAssignment{}, false
		}
		name := tokens[nameIdx].Value
		eq := jsFindAssignmentEquals(tokens, nameIdx+1)
		if eq < 0 {
			return jsRawCallAssignment{}, false
		}
		callee, ok := jsDottedCallAt(tokens, jsSkipAwait(tokens, eq+1))
		if !ok {
			return jsRawCallAssignment{}, false
		}
		return jsRawCallAssignment{name: name, callee: callee, pos: tokens[nameIdx].Start}, true
	}

	if !jsIdentLike(tokens[j]) {
		return jsRawCallAssignment{}, false
	}
	if j > 0 && jsTokenPunct(tokens, jsSkipTriviaBack(tokens, j-1), ".") {
		return jsRawCallAssignment{}, false
	}
	eq := jsSkipTrivia(tokens, j+1)
	if !jsTokenPunct(tokens, eq, "=") {
		return jsRawCallAssignment{}, false
	}
	callee, ok := jsDottedCallAt(tokens, jsSkipAwait(tokens, eq+1))
	if !ok {
		return jsRawCallAssignment{}, false
	}
	return jsRawCallAssignment{name: tokens[j].Value, callee: callee, pos: tokens[j].Start}, true
}

func jsFindAssignmentEquals(tokens []Token, from int) int {
	depth := 0
	for j := from; j < len(tokens); j++ {
		t := tokens[j]
		if t.Kind == TokComment || t.Kind == TokLineComment {
			continue
		}
		if t.Kind != TokPunct {
			continue
		}
		switch t.Value {
		case "(", "[", "{", "<":
			depth++
		case ")", "]", "}", ">":
			if depth > 0 {
				depth--
			}
		case "=":
			if depth == 0 {
				return j
			}
		case ";", ",":
			if depth == 0 {
				return -1
			}
		}
	}
	return -1
}

// jsReturnTargetFile decides what file (if any) a `return <expr>` at
// tokens[from:] resolves to. newInstanceAssignments and beforePos add the
// `new ClassName(...)` patterns (direct return, and one-hop "return x" where
// x was locally bound via `new` before this return statement) — see
// jsReturnFileInference.
func jsReturnTargetFile(idx *Index, file string, tokens []Token, from int, scope CGPSymbol, fieldFiles, requireBindings map[string]string, newInstanceAssignments []jsLocalReturnAssignment, beforePos int) string {
	j := jsSkipAwait(tokens, from)
	if spec, ok := jsRequireSpecAt(tokens, j); ok {
		return resolveImportSpecToIndexedFile(idx, file, spec)
	}
	if className, ok := jsNewInstanceClassAt(tokens, j); ok {
		return requireBindings[className]
	}
	if j < len(tokens) && tokens[j].Value == "this" {
		dot := jsSkipTrivia(tokens, j+1)
		fieldIdx := jsSkipTrivia(tokens, dot+1)
		if jsTokenPunct(tokens, dot, ".") && fieldIdx < len(tokens) && jsIdentLike(tokens[fieldIdx]) {
			field := tokens[fieldIdx].Value
			if target := fieldFiles[jsScopedNameKey(scope.ID, field)]; target != "" {
				return target
			}
			if classID := enclosingClassID(idx, scope); classID != "" {
				return fieldFiles[jsScopedNameKey(classID, field)]
			}
		}
		return ""
	}
	if j < len(tokens) && jsIdentLike(tokens[j]) {
		name := tokens[j].Value
		if target := requireBindings[name]; target != "" {
			return target
		}
		return jsAssignedReturnFile(newInstanceAssignments, scope.ID, name, beforePos)
	}
	return ""
}

func resolveJSTSAssignmentCallee(idx *Index, file string, from CGPSymbol, callee string) (target, confidence, reason string) {
	if dot := strings.LastIndexByte(callee, '.'); dot >= 0 {
		root := callee[:dot]
		method := callee[dot+1:]
		switch root {
		case "this":
			return resolveScopedCall(idx, file, from, method)
		case "super":
			return resolveSuperCall(idx, file, from, method)
		}
	}
	return resolveSymbolCall(idx, file, callee)
}

func jsRequireSpecAt(tokens []Token, i int) (string, bool) {
	j := jsSkipTrivia(tokens, i)
	if j >= len(tokens) || tokens[j].Kind != TokIdent || tokens[j].Value != "require" {
		return "", false
	}
	j = jsSkipTrivia(tokens, j+1)
	if !jsTokenPunct(tokens, j, "(") {
		return "", false
	}
	j = jsSkipTrivia(tokens, j+1)
	if j >= len(tokens) || tokens[j].Kind != TokString {
		return "", false
	}
	spec := tokens[j].Value
	j = jsSkipTrivia(tokens, j+1)
	if !jsTokenPunct(tokens, j, ")") {
		return "", false
	}
	return spec, true
}

func jsDottedCallAt(tokens []Token, i int) (string, bool) {
	j := jsSkipTrivia(tokens, i)
	if j >= len(tokens) || !jsIdentLike(tokens[j]) {
		return "", false
	}
	parts := []string{tokens[j].Value}
	j = jsSkipTrivia(tokens, j+1)
	for jsTokenPunct(tokens, j, ".") {
		j = jsSkipTrivia(tokens, j+1)
		if j >= len(tokens) || !jsIdentLike(tokens[j]) {
			return "", false
		}
		parts = append(parts, tokens[j].Value)
		j = jsSkipTrivia(tokens, j+1)
	}
	if !jsTokenPunct(tokens, j, "(") {
		return "", false
	}
	return strings.Join(parts, "."), true
}

// jsNewInstanceClassAt matches `new ClassName(` at tokens[i] (after skipping
// trivia), returning the constructed class's identifier name. An optional TS
// generic type-argument span between the class name and the call's opening
// paren (`new Box<string>(...)`) is skipped. Dotted constructor expressions
// (`new ns.ClassName()`) are intentionally not matched — namespace imports
// are excluded from jsRequireBindingFiles too, so there is nothing to
// resolve them against.
func jsNewInstanceClassAt(tokens []Token, i int) (string, bool) {
	j := jsSkipTrivia(tokens, i)
	if j >= len(tokens) || tokens[j].Kind != TokKeyword || tokens[j].Value != "new" {
		return "", false
	}
	j = jsSkipTrivia(tokens, j+1)
	if j >= len(tokens) || !jsIdentLike(tokens[j]) {
		return "", false
	}
	className := tokens[j].Value
	j = jsSkipTrivia(tokens, j+1)
	if jsTokenPunct(tokens, j, "<") {
		depth := 1
		j++
		for j < len(tokens) && depth > 0 {
			if jsTokenPunct(tokens, j, "<") {
				depth++
			} else if jsTokenPunct(tokens, j, ">") {
				depth--
			}
			j++
		}
		j = jsSkipTrivia(tokens, j)
	}
	if !jsTokenPunct(tokens, j, "(") {
		return "", false
	}
	return className, true
}

// jsRawNewAssignment is jsNewInstanceAssignmentAt's match result, before the
// constructed class name has been resolved to a file via requireBindings.
type jsRawNewAssignment struct {
	name      string
	className string
	pos       int
}

// jsNewInstanceAssignmentAt matches `const/let/var NAME = new ClassName(...)`
// or bare reassignment `NAME = new ClassName(...)` at tokens[i], mirroring
// jsCallAssignmentAt's two shapes (the same const/let/var-declaration vs.
// bare-reassignment distinction, including its `await`/depth-aware equals
// lookup for the declaration shape).
func jsNewInstanceAssignmentAt(tokens []Token, i int) (jsRawNewAssignment, bool) {
	j := jsSkipTrivia(tokens, i)
	if j >= len(tokens) {
		return jsRawNewAssignment{}, false
	}
	if tokens[j].Kind == TokKeyword && (tokens[j].Value == "const" || tokens[j].Value == "let" || tokens[j].Value == "var") {
		nameIdx := jsSkipTrivia(tokens, j+1)
		if nameIdx >= len(tokens) || !jsIdentLike(tokens[nameIdx]) {
			return jsRawNewAssignment{}, false
		}
		name := tokens[nameIdx].Value
		eq := jsFindAssignmentEquals(tokens, nameIdx+1)
		if eq < 0 {
			return jsRawNewAssignment{}, false
		}
		className, ok := jsNewInstanceClassAt(tokens, jsSkipAwait(tokens, eq+1))
		if !ok {
			return jsRawNewAssignment{}, false
		}
		return jsRawNewAssignment{name: name, className: className, pos: tokens[nameIdx].Start}, true
	}

	if !jsIdentLike(tokens[j]) {
		return jsRawNewAssignment{}, false
	}
	if j > 0 && jsTokenPunct(tokens, jsSkipTriviaBack(tokens, j-1), ".") {
		return jsRawNewAssignment{}, false
	}
	eq := jsSkipTrivia(tokens, j+1)
	if !jsTokenPunct(tokens, eq, "=") {
		return jsRawNewAssignment{}, false
	}
	className, ok := jsNewInstanceClassAt(tokens, jsSkipAwait(tokens, eq+1))
	if !ok {
		return jsRawNewAssignment{}, false
	}
	return jsRawNewAssignment{name: tokens[j].Value, className: className, pos: tokens[j].Start}, true
}

func jsSkipAwait(tokens []Token, i int) int {
	j := jsSkipTrivia(tokens, i)
	if j < len(tokens) && tokens[j].Kind == TokKeyword && tokens[j].Value == "await" {
		return jsSkipTrivia(tokens, j+1)
	}
	return j
}

// jsConciseArrowHelperAt recognizes a factory helper declared as a concise
// arrow returning a construction:
//
//	const buildService = (overrides = {}) => new ReconciliationCheckService({...})
//
// and returns its name and the constructed class. Unlike
// jsNewInstanceAssignmentAt (which types the *assigned local*), this types
// the *helper by name*, so `const svc = buildService()` resolves even when
// helper and caller live in different nested scopes (describe/test callbacks)
// where scope-keyed methodReturnFiles cannot connect them.
func jsConciseArrowHelperAt(tokens []Token, i int) (name, className string, ok bool) {
	j := jsSkipTrivia(tokens, i)
	if j >= len(tokens) {
		return "", "", false
	}
	if tokens[j].Kind == TokKeyword && (tokens[j].Value == "const" || tokens[j].Value == "let" || tokens[j].Value == "var") {
		j = jsSkipTrivia(tokens, j+1)
	}
	if j >= len(tokens) || !jsIdentLike(tokens[j]) {
		return "", "", false
	}
	nameIdx := j
	eq := jsSkipTrivia(tokens, j+1)
	if !jsTokenPunct(tokens, eq, "=") {
		return "", "", false
	}
	j = jsSkipTrivia(tokens, eq+1)
	if j < len(tokens) && tokens[j].Kind == TokKeyword && tokens[j].Value == "async" {
		j = jsSkipTrivia(tokens, j+1)
	}
	if !jsTokenPunct(tokens, j, "(") {
		return "", "", false
	}
	depth := 0
	for ; j < len(tokens); j++ {
		if tokens[j].Kind != TokPunct {
			continue
		}
		switch tokens[j].Value {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				goto params_done
			}
		}
	}
	return "", "", false
params_done:
	j = jsSkipTrivia(tokens, j+1)
	if !jsTokenPunct(tokens, j, "=>") {
		return "", "", false
	}
	className, ok = jsNewInstanceClassAt(tokens, jsSkipAwait(tokens, j+1))
	if !ok {
		return "", "", false
	}
	return tokens[nameIdx].Value, className, true
}

func jsSkipTrivia(tokens []Token, i int) int {
	for i < len(tokens) && (tokens[i].Kind == TokComment || tokens[i].Kind == TokLineComment) {
		i++
	}
	return i
}

func jsSkipTriviaBack(tokens []Token, i int) int {
	for i >= 0 && (tokens[i].Kind == TokComment || tokens[i].Kind == TokLineComment) {
		i--
	}
	return i
}

func jsTokenPunct(tokens []Token, i int, value string) bool {
	return i >= 0 && i < len(tokens) && tokens[i].Kind == TokPunct && tokens[i].Value == value
}

func jsIdentLike(tok Token) bool {
	if tok.Kind == TokIdent {
		return true
	}
	if tok.Kind != TokKeyword {
		return false
	}
	return contextualIdentKeyword(tok.Value) || tok.Value == "this" || tok.Value == "super"
}

// resolveImportBoundCall looks up `name` scoped to a single known file
// (typically the resolved target of a require()/import binding). Unlike
// resolveSymbolCall, ambiguity is judged only within that one file, so a
// name that's ambiguous repo-wide (e.g. two classes in different files both
// define `sendEmail`) still resolves cleanly once we know which file the
// receiver variable points to.
func resolveImportBoundCall(idx *Index, targetFile, name string) (target, confidence, reason string, ok bool) {
	idx.ensureFileSymbolIndex()
	idx.mu.Lock()
	defer idx.mu.Unlock()
	var matches []CGPSymbol
	for _, sym := range idx.symbolsByName[name] {
		if sym.Kind == "file" || sym.File != targetFile {
			continue
		}
		matches = append(matches, sym)
	}
	if len(matches) == 1 {
		return matches[0].ID, ConfScoped, "", true
	}
	if len(matches) > 1 {
		return "unresolved:" + targetFile + "#" + name, ConfUnresolved, ReasonAmbiguousName, true
	}
	return "", "", "", false
}

// resolveImportBoundBareCall resolves a bare call on a local name bound by an
// import/require to the declaration inside the file the import resolves to.
// Named (possibly renamed) imports look up the *imported* name; default-style
// bindings (Imported == Local per jsImportBindingTargets) additionally fall
// back to the target file's recorded bare default export, so a renamed
// default import (`const run = require('./job')` where job.js does
// `module.exports = runJob`) still resolves. Deliberately no sole-declaration
// guess beyond that: a `calls` edge asserts control flow, so not-found means
// "let the caller fall through", never "pick something plausible".
func resolveImportBoundBareCall(idx *Index, binding jsImportBindingTarget) (target, confidence, reason string, ok bool) {
	if t, c, r, found := resolveImportBoundCall(idx, binding.File, binding.Imported); found {
		return t, c, r, true
	}
	if !binding.IsDefault {
		return "", "", "", false
	}
	if exportName := idx.jsDefaultExportName(binding.File); exportName != "" && exportName != binding.Imported {
		if t, c, r, found := resolveImportBoundCall(idx, binding.File, exportName); found {
			return t, c, r, true
		}
	}
	return "", "", "", false
}

// resolveImportBoundConstructor resolves a `new LocalName()` call through
// LocalName's imported file. It prefers a class with the same declaration
// name, then accepts the file's sole class for default/CommonJS imports whose
// local binding was renamed by the importer.
func resolveImportBoundConstructor(idx *Index, targetFile, name string) (target, confidence, reason string, ok bool) {
	idx.ensureFileSymbolIndex()
	idx.mu.Lock()
	defer idx.mu.Unlock()
	var named []CGPSymbol
	var classes []CGPSymbol
	for _, sym := range idx.Symbols {
		if sym.File != targetFile || sym.Kind != "class" {
			continue
		}
		classes = append(classes, sym)
		if sym.Name == name {
			named = append(named, sym)
		}
	}
	if len(named) == 1 {
		return named[0].ID, ConfScoped, "", true
	}
	if len(named) > 1 {
		return "unresolved:" + targetFile + "#" + name, ConfUnresolved, ReasonAmbiguousName, true
	}
	if len(classes) == 1 {
		return classes[0].ID, ConfScoped, "", true
	}
	if len(classes) > 1 {
		return "unresolved:" + targetFile + "#" + name, ConfUnresolved, ReasonAmbiguousName, true
	}
	return "", "", "", false
}

func fileSymbolID(file string) string {
	return "symbol:file:" + filepath.ToSlash(file)
}

// stableSymbolID produces an ID that does not encode the symbol's line
// number, so a function that simply moves up or down in the file keeps the
// same ID across rebakes. Collisions on the same qualified name in the same
// file (e.g. TS function overloads) are disambiguated by appending #N.
func stableSymbolID(language, kind, file, qualified string, idx *Index) string {
	base := fmt.Sprintf("symbol:%s:%s:%s:%s", language, kind, filepath.ToSlash(file), qualified)
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.symbolSeen == nil {
		idx.symbolSeen = map[string]bool{}
	}
	if !idx.symbolSeen[base] {
		return base
	}
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s#%d", base, n)
		if !idx.symbolSeen[candidate] {
			return candidate
		}
	}
}

func ttlTermSymbolID(termID, file string, line int) string {
	// TTL term sites are intentionally per-location: the same term appears at
	// many line numbers and each is its own evidence point.
	return fmt.Sprintf("symbol:ttl:term:%s:%s:%d", termID, filepath.ToSlash(file), line)
}

func clampOffset(content string, offset int) int {
	if offset > len(content) {
		return len(content)
	}
	if offset < 0 {
		return 0
	}
	return offset
}

func signatureLine(content string, starts []int, line int) string {
	if line < 1 || line > len(starts) {
		return ""
	}
	begin := starts[line-1]
	end := len(content)
	if line < len(starts) {
		end = starts[line] - 1
	}
	return strings.TrimSpace(content[begin:end])
}

// vueComponentName reads the component name from `defineOptions({ name: '...' })`
// or `export default { name: '...' }`. It deliberately does NOT match a bare
// `name:` literal anywhere in the file, which the previous regex did.
func vueComponentName(content string) string {
	for _, block := range scriptBlockRe.FindAllStringSubmatchIndex(content, -1) {
		body := content[block[2]:block[3]]
		masked := MaskStringsAndComments(body)
		if name := vueComponentNameFromMasked(body, masked); name != "" {
			return name
		}
	}
	return ""
}

func vueComponentNameFromMasked(body, masked string) string {
	for _, marker := range []string{"defineOptions(", "export default {"} {
		idx := strings.Index(masked, marker)
		if idx < 0 {
			continue
		}
		// Walk forward to the matching `)` or `}`. Inside the body, find a
		// top-level `name:` followed by a string literal in the *original*.
		end := matchingClose(masked, idx+len(marker)-1)
		if end <= idx {
			continue
		}
		segment := body[idx:end]
		key := "name:"
		for offset := 0; offset < len(segment); {
			relative := strings.Index(segment[offset:], key)
			if relative < 0 {
				break
			}
			k := offset + relative
			rest := segment[k+len(key):]
			rest = strings.TrimLeft(rest, " \t")
			if len(rest) >= 2 && (rest[0] == '\'' || rest[0] == '"') {
				quote := rest[0]
				if close := strings.IndexByte(rest[1:], quote); close >= 0 {
					return rest[1 : 1+close]
				}
			}
			offset = k + len(key)
		}
	}
	return ""
}

// matchingClose returns the index in `masked` of the bracket that closes the
// one at `open`. Brackets inside strings/comments don't appear in `masked`
// because they were replaced with spaces.
func matchingClose(masked string, open int) int {
	if open < 0 || open >= len(masked) {
		return -1
	}
	openCh := masked[open]
	closeCh := byte(')')
	if openCh == '{' {
		closeCh = '}'
	} else if openCh == '[' {
		closeCh = ']'
	}
	depth := 0
	for i := open; i < len(masked); i++ {
		switch masked[i] {
		case openCh:
			depth++
		case closeCh:
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// maskPythonStringsAndComments replaces `# comments`, `'..'`, `".."`, `”'..”'`,
// and `"""..."""` with spaces of equal length, preserving newlines. Any line
// that survives the mask is structural Python code.
func maskPythonStringsAndComments(src string) string {
	if src == "" {
		return src
	}
	buf := []byte(src)
	i := 0
	for i < len(buf) {
		c := buf[i]
		switch {
		case c == '#':
			for i < len(buf) && buf[i] != '\n' {
				buf[i] = ' '
				i++
			}
		case (c == '\'' || c == '"') && i+2 < len(buf) && buf[i+1] == c && buf[i+2] == c:
			quote := c
			buf[i] = ' '
			buf[i+1] = ' '
			buf[i+2] = ' '
			i += 3
			for i+2 < len(buf) {
				if buf[i] == quote && buf[i+1] == quote && buf[i+2] == quote {
					buf[i] = ' '
					buf[i+1] = ' '
					buf[i+2] = ' '
					i += 3
					break
				}
				if buf[i] != '\n' {
					buf[i] = ' '
				}
				i++
			}
		case c == '\'' || c == '"':
			quote := c
			buf[i] = ' '
			i++
			for i < len(buf) && buf[i] != quote {
				if buf[i] == '\\' && i+1 < len(buf) {
					buf[i] = ' '
					if buf[i+1] != '\n' {
						buf[i+1] = ' '
					}
					i += 2
					continue
				}
				if buf[i] == '\n' {
					break
				}
				buf[i] = ' '
				i++
			}
			if i < len(buf) && buf[i] == quote {
				buf[i] = ' '
				i++
			}
		default:
			i++
		}
	}
	return string(buf)
}

func sortCGP(idx *Index) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	sort.Slice(idx.SymbolEdges, func(i, j int) bool { return idx.SymbolEdges[i].ID < idx.SymbolEdges[j].ID })
}

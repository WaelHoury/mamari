package mamari

import "strings"

// ScannedRef is an identifier used as a *value* rather than called: passed as
// an argument (`router.post('/x', handler)`), stored in an object-literal
// property (`{ jobFunction: CAISExportJob }`), registered as a callback
// (`addEventListener('change', onChange)`), returned from a composable, or
// assigned to a variable. These uses produce no `calls` edge, so before this
// pass a function invoked exclusively through such registration looked
// completely unreferenced to dead-code analysis — measured at 9 of 12 sampled
// false positives on a real Express/Vue monorepo (route handlers, cron job
// configs and event listeners).
type ScannedRef struct {
	Name  string
	Start int
}

// jsRefSkipNames are identifiers that can never resolve to a project symbol
// worth a reference edge. Keywords (this, true, typeof, ...) never reach the
// scanner because the tokenizer classifies them as TokKeyword; these are the
// ident-shaped globals it cannot.
var jsRefSkipNames = map[string]bool{
	"undefined": true, "NaN": true, "Infinity": true, "arguments": true,
	"require": true, "module": true, "exports": true, "globalThis": true,
}

// jsRefSkipPrevKeywords are keywords that, immediately before an identifier,
// mark it as a declaration/binding name rather than a value use.
var jsRefSkipPrevKeywords = map[string]bool{
	"function": true, "class": true, "const": true, "let": true, "var": true,
	"new": true, "get": true, "set": true, "import": true, "interface": true,
	"enum": true, "namespace": true,
}

// scanJSValueRefs walks the token stream of a JS/TS source slice and returns
// identifiers in value position. It is deliberately conservative in both
// directions:
//
//   - It never reports an identifier that is a declaration name, an object
//     key, a member-access segment (`obj.name`), a call (those are already
//     ScannedCalls), an assignment target, or part of an import/export
//     statement — so a reference edge, once resolved, means a real use.
//   - Export sites (`module.exports = {...}`, `exports.x = ...`,
//     `export default x`, `export { a, b }`) are masked entirely: exporting a
//     symbol is not *using* it. Without this, no CommonJS-exported function
//     could ever be reported dead, because its own export statement would
//     keep it alive.
//
// Positions it may miss (ternary branches, values inside a masked export
// object) fail toward the status quo — fewer reference edges — never toward a
// false edge. Resolution (resolveJSValueRef) is the second gate: a ref only
// becomes an edge when the name maps to exactly one same-file or
// import-bound declaration.
func scanJSValueRefs(src string, res ScanResult) []ScannedRef {
	tokens := significantJSTokens(TokenizeJS(src))
	if len(tokens) == 0 {
		return nil
	}

	masked := jsRefMaskedRanges(src, tokens, res)
	defNames := make(map[int]bool, len(res.Symbols))
	for _, sym := range res.Symbols {
		defNames[sym.NameStart] = true
	}

	var out []ScannedRef
	for i, tok := range tokens {
		if tok.Kind != TokIdent {
			continue
		}
		if jsRefSkipNames[tok.Value] || defNames[tok.Start] || jsRefOffsetMasked(masked, tok.Start) {
			continue
		}
		if prev := jsRefSignificantAt(tokens, i-1); prev != nil {
			if prev.Kind == TokPunct && (prev.Value == "." || prev.Value == "?.") {
				continue // member-access segment, not a bare value
			}
			if prev.Kind == TokKeyword && jsRefSkipPrevKeywords[prev.Value] {
				continue // declaration/binding name
			}
		}
		if next := jsRefSignificantAt(tokens, i+1); next != nil && next.Kind == TokPunct {
			switch next.Value {
			case "(":
				continue // call — already a ScannedCall
			case "=", "=>", "++", "--":
				continue // assignment target / arrow param / mutation
			case ":":
				// Object key (`{ handler: fn }`) or TS annotation — not a
				// value use. The one value position followed by ':' is a
				// ternary's then-branch (`c ? a : b`), recognizable by the
				// '?' before it.
				if prev := jsRefSignificantAt(tokens, i-1); prev == nil || prev.Value != "?" {
					continue
				}
			}
		}
		out = append(out, ScannedRef{Name: tok.Value, Start: tok.Start})
	}
	return out
}

// scannedDynamicImport is a `import('spec')` expression site with a
// string/template literal spec.
type scannedDynamicImport struct {
	Spec  string
	Start int
}

// scanJSDynamicImports walks the whole token stream for
// `import ( <string|template> )` expressions. It runs over the full body
// rather than the top-level parser because dynamic imports nest arbitrarily
// deep inside route tables (`const routes = [{ component: () => import('./X.vue') }]`)
// where a statement-level check never reaches them. Only literal specs are
// captured (a computed `import(path)` cannot be resolved to an indexed file).
func scanJSDynamicImports(src string) []scannedDynamicImport {
	tokens := significantJSTokens(TokenizeJS(src))
	var out []scannedDynamicImport
	for i := 0; i+3 < len(tokens); i++ {
		if !(tokens[i].Kind == TokKeyword && tokens[i].Value == "import") {
			continue
		}
		open, spec, closeParen := tokens[i+1], tokens[i+2], tokens[i+3]
		if open.Kind != TokPunct || open.Value != "(" {
			continue
		}
		if spec.Kind != TokString && spec.Kind != TokTemplate {
			continue
		}
		if closeParen.Kind != TokPunct || closeParen.Value != ")" {
			continue
		}
		// TokString/TokTemplate values are already unquoted by the tokenizer
		// (same value consumeString uses for static import specs).
		out = append(out, scannedDynamicImport{Spec: spec.Value, Start: tokens[i].Start})
	}
	return out
}

// scanJSDefaultExport returns the identifier a module default-exports, when
// the export is a bare re-export of a named declaration:
//
//	module.exports = aggregateLoansInfoJob
//	exports.default = aggregateLoansInfoJob
//	export default aggregateLoansInfoJob
//
// It returns "" for object/class/function/call/member RHS forms
// (`module.exports = {...}`, `= new X()`, `= factory()`, `= a.b`,
// `export default function () {}`), which are not a simple symbol re-export.
// The importer's local binding name is often unrelated to this exported
// identifier (`const balanceCheckForLoans = require('./balanceCheckForLoans')`
// where the file's function is `loansBalanceCheck`), so recording it lets a
// renamed default import resolve to the real declaration rather than guessing.
func scanJSDefaultExport(src string) string {
	tokens := significantJSTokens(TokenizeJS(src))
	isBareEnd := func(idx int) bool {
		if idx >= len(tokens) {
			return true
		}
		t := tokens[idx]
		return t.Kind == TokPunct && (t.Value == ";" || t.Value == "}")
	}
	for i := 0; i+2 < len(tokens); i++ {
		t := tokens[i]
		// export default IDENT
		if t.Kind == TokKeyword && t.Value == "export" &&
			jsRefKeywordAt(tokens, i+1, "default") {
			name := tokens[i+2]
			if name.Kind == TokIdent && isBareEnd(i+3) {
				return name.Value
			}
			continue
		}
		// module.exports = IDENT  /  exports.default = IDENT
		if t.Kind == TokIdent && (t.Value == "module" || t.Value == "exports") {
			j := i
			if t.Value == "module" {
				if !(jsRefPunctAt(tokens, i+1, ".") && jsRefIdentAt(tokens, i+2, "exports")) {
					continue
				}
				j = i + 2
			} else {
				// bare `exports` — only accept `exports.default =`
				if !(jsRefPunctAt(tokens, i+1, ".") && jsRefIdentAt(tokens, i+2, "default")) {
					continue
				}
				j = i + 2
			}
			// optional `.default`
			if jsRefPunctAt(tokens, j+1, ".") && jsRefIdentAt(tokens, j+2, "default") {
				j += 2
			}
			if jsRefPunctAt(tokens, j+1, "=") {
				name := jsRefSignificantAt(tokens, j+2)
				if name != nil && name.Kind == TokIdent && isBareEnd(j+3) {
					return name.Value
				}
			}
		}
	}
	return ""
}

// jsRefSignificantAt returns the token at idx, or nil when out of range.
// Tokens are pre-filtered by significantJSTokens, so neighbors are already
// comment-free.
func jsRefSignificantAt(tokens []Token, idx int) *Token {
	if idx < 0 || idx >= len(tokens) {
		return nil
	}
	return &tokens[idx]
}

// jsRefMaskedRanges collects [start,end) byte ranges whose identifiers must
// not count as value references: import/export statements and CommonJS/ES
// export sites.
func jsRefMaskedRanges(src string, tokens []Token, res ScanResult) [][2]int {
	var ranges [][2]int
	for _, imp := range res.Imports {
		ranges = append(ranges, [2]int{imp.Start, imp.End})
	}

	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		switch {
		// module.exports = <RHS>  /  module.exports.x = <RHS>
		case tok.Kind == TokIdent && tok.Value == "module" &&
			jsRefPunctAt(tokens, i+1, ".") && jsRefIdentAt(tokens, i+2, "exports"):
			if end, ok := jsRefExportAssignmentEnd(tokens, i+3); ok {
				ranges = append(ranges, [2]int{tok.Start, end})
			}
		// exports.x = <RHS>
		case tok.Kind == TokIdent && tok.Value == "exports" &&
			jsRefPunctAt(tokens, i+1, ".") &&
			!(i > 0 && tokens[i-1].Kind == TokPunct && (tokens[i-1].Value == "." || tokens[i-1].Value == "?.")):
			if end, ok := jsRefExportAssignmentEnd(tokens, i+2); ok {
				ranges = append(ranges, [2]int{tok.Start, end})
			}
		// export default <Ident>  /  export { a, b }   (re-exports with
		// `from` are already in res.Imports). `export default function...`
		// and `export const ...` declare real code whose body must stay
		// scannable, so only the bare-identifier and brace-list forms mask.
		case tok.Kind == TokKeyword && tok.Value == "export":
			if jsRefKeywordAt(tokens, i+1, "default") {
				if id := jsRefSignificantAt(tokens, i+2); id != nil && id.Kind == TokIdent {
					if after := jsRefSignificantAt(tokens, i+3); after == nil ||
						(after.Kind == TokPunct && (after.Value == ";" || after.Value == "}")) ||
						after.Kind == TokKeyword {
						ranges = append(ranges, [2]int{tok.Start, id.End})
					}
				}
			} else if jsRefPunctAt(tokens, i+1, "{") {
				if close := jsRefMatchingBrace(tokens, i+1); close >= 0 {
					ranges = append(ranges, [2]int{tok.Start, tokens[close].End})
				}
			}
		}
	}
	_ = src
	return ranges
}

// jsRefExportAssignmentEnd finds the end offset of an export assignment's
// right-hand side, given the token index where the walk should continue
// (just past `module.exports` / `exports`). It expects an optional
// `.property` chain, then `=`; a brace-literal RHS masks through its
// matching brace, any other RHS masks through the end of the statement.
// Compound-assignment or non-assignment continuations return !ok so ordinary
// reads of `module.exports` (e.g. spreading it) are not masked.
func jsRefExportAssignmentEnd(tokens []Token, i int) (int, bool) {
	for jsRefPunctAt(tokens, i, ".") {
		next := jsRefSignificantAt(tokens, i+1)
		if next == nil || (next.Kind != TokIdent && next.Kind != TokKeyword) {
			return 0, false
		}
		i += 2
	}
	if !jsRefPunctAt(tokens, i, "=") {
		return 0, false
	}
	rhs := jsRefSignificantAt(tokens, i+1)
	if rhs == nil {
		return 0, false
	}
	if rhs.Kind == TokPunct && rhs.Value == "{" {
		if close := jsRefMatchingBrace(tokens, i+1); close >= 0 {
			return tokens[close].End, true
		}
		return 0, false
	}
	// Non-brace RHS: mask to the statement's `;` (or the last token before
	// nesting closes / input ends), tracking bracket depth so a `;` inside
	// a nested arrow body does not end the statement early.
	depth := 0
	end := rhs.End
	for j := i + 1; j < len(tokens); j++ {
		t := tokens[j]
		if t.Kind == TokPunct {
			switch t.Value {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				if depth == 0 {
					return end, true
				}
				depth--
			case ";":
				if depth == 0 {
					return t.End, true
				}
			}
		}
		end = t.End
	}
	return end, true
}

func jsRefPunctAt(tokens []Token, idx int, value string) bool {
	t := jsRefSignificantAt(tokens, idx)
	return t != nil && t.Kind == TokPunct && t.Value == value
}

func jsRefIdentAt(tokens []Token, idx int, value string) bool {
	t := jsRefSignificantAt(tokens, idx)
	return t != nil && t.Kind == TokIdent && t.Value == value
}

func jsRefKeywordAt(tokens []Token, idx int, value string) bool {
	t := jsRefSignificantAt(tokens, idx)
	return t != nil && t.Kind == TokKeyword && t.Value == value
}

func jsRefMatchingBrace(tokens []Token, openIdx int) int {
	depth := 0
	for j := openIdx; j < len(tokens); j++ {
		if tokens[j].Kind != TokPunct {
			continue
		}
		switch tokens[j].Value {
		case "{":
			depth++
		case "}":
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return -1
}

func jsRefOffsetMasked(ranges [][2]int, offset int) bool {
	for _, r := range ranges {
		if offset >= r[0] && offset < r[1] {
			return true
		}
	}
	return false
}

// jsImportBindingTarget records where an import binding's value comes from:
// the indexed file the spec resolved to, and the *imported* name (which
// differs from the local binding on renamed imports such as
// `const { a: b } = require('./x')` or `import { a as b } from './x'`).
type jsImportBindingTarget struct {
	File      string
	Imported  string
	IsDefault bool
}

// jsImportBindingTargets is the reference-resolution analogue of
// jsRequireBindingFiles: it keeps the imported name alongside the target file
// so a renamed binding resolves to the declaration it actually aliases.
// Default-style bindings (`const X = require('./x')`, `import X from './x'`)
// record Imported as "default", which names nothing in the target file — for
// those the local name is the best lookup key, with a sole-declaration
// fallback in the resolver for renamed importers.
func jsImportBindingTargets(idx *Index, file string, imports []ScannedImport) map[string]jsImportBindingTarget {
	out := map[string]jsImportBindingTarget{}
	for _, imp := range imports {
		targetFile := resolveImportSpecToIndexedFile(idx, file, imp.Spec)
		if targetFile == "" {
			continue
		}
		for _, binding := range imp.Bindings {
			if binding.Local == "" {
				continue
			}
			imported := binding.Imported
			if imported == "" || binding.IsDefault || imported == "default" {
				imported = binding.Local
			}
			out[binding.Local] = jsImportBindingTarget{
				File: targetFile, Imported: imported,
				IsDefault: binding.IsDefault || binding.Imported == "default" || binding.Imported == "",
			}
		}
	}
	return out
}

// recordJSDefaultExport stores a file's bare default-export identifier (if
// any) so renamed default imports can resolve to it. Called during symbol
// scan, before any relations pass reads it. Watch-mode rebake clears the
// entry in dropFile.
func recordJSDefaultExport(idx *Index, file, content string) {
	name := scanJSDefaultExport(content)
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.jsDefaultExports == nil {
		idx.jsDefaultExports = map[string]string{}
	}
	if name == "" {
		delete(idx.jsDefaultExports, file)
		return
	}
	idx.jsDefaultExports[file] = name
}

// jsDefaultExportName returns the recorded bare default-export identifier of
// file (see recordJSDefaultExport), or "" — a mutex-guarded read for callers
// outside this file's already-locked paths.
func (idx *Index) jsDefaultExportName(file string) string {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return idx.jsDefaultExports[file]
}

// jsValueRefTargetKinds are the declaration kinds a value reference may
// resolve to. Methods are excluded: a bare identifier cannot name a method
// without its receiver, so a bare-name match against a method would be the
// same class of name-collision guessing the call resolver avoids.
var jsValueRefTargetKinds = map[string]bool{
	"function":  true,
	"class":     true,
	"component": true,
	"constant":  true,
}

// resolveJSValueRef resolves a value-position identifier to at most one
// declaration: a unique same-file match (exact) or a unique match inside the
// file its import binding resolves to (scoped). Anything ambiguous or
// unmatched resolves to nothing — value references are additive evidence for
// liveness, so an unresolved ref is silence, not an `unresolved:` edge; the
// call graph's honesty accounting is not diluted with identifier noise.
func resolveJSValueRef(idx *Index, file, name string, bindings map[string]jsImportBindingTarget) (target, confidence string) {
	idx.ensureFileSymbolIndex()
	// An import binding wins over same-file lookup: `const X = require('./X')`
	// declares a same-file *constant* named X that would otherwise absorb
	// every reference to the binding — the shadow, not the declaration the
	// value actually aliases.
	if binding, ok := bindings[name]; ok {
		idx.mu.Lock()
		var bound []CGPSymbol
		for _, sym := range idx.symbolsByName[binding.Imported] {
			if !jsValueRefTargetKinds[sym.Kind] || sym.File != binding.File {
				continue
			}
			bound = append(bound, sym)
		}
		// A default/CommonJS binding renamed by the importer names nothing
		// in the target file under the local name. Resolve it precisely to
		// the file's recorded default export (`module.exports = X`), which is
		// correct even when the file also declares unexported helper functions.
		// Fall back to a sole top-level
		// declaration only when no default export was recorded.
		if len(bound) == 0 && binding.IsDefault {
			if exportName := idx.jsDefaultExports[binding.File]; exportName != "" {
				for _, sym := range idx.symbolsByName[exportName] {
					if sym.File == binding.File && jsValueRefTargetKinds[sym.Kind] {
						bound = append(bound, sym)
					}
				}
			}
		}
		if len(bound) == 0 && binding.IsDefault {
			// "Top-level" = not nested in another symbol; but top-level JS
			// functions carry ParentID == the file symbol, so the test is
			// "no parent, or a file parent", NOT "empty ParentID".
			for _, sym := range idx.Symbols {
				if sym.File != binding.File || !jsValueRefTargetKinds[sym.Kind] {
					continue
				}
				if sym.ParentID != "" && !strings.HasPrefix(sym.ParentID, "symbol:file:") {
					continue
				}
				bound = append(bound, sym)
				if len(bound) > 1 {
					break
				}
			}
		}
		idx.mu.Unlock()
		if len(bound) == 1 {
			return bound[0].ID, ConfScoped
		}
		return "", ""
	}
	lang := languageFamily(idx.languageFor(file))
	idx.mu.Lock()
	var sameFile []CGPSymbol
	for _, sym := range idx.symbolsByName[name] {
		if !jsValueRefTargetKinds[sym.Kind] || languageFamily(sym.Language) != lang {
			continue
		}
		if sym.File == file {
			sameFile = append(sameFile, sym)
		}
	}
	idx.mu.Unlock()
	if len(sameFile) == 1 {
		return sameFile[0].ID, ConfExact
	}
	return "", ""
}

// emitJSValueRefEdges converts value-position identifier references into
// `references-symbol` edges (the same edge type SCIP ingestion uses for
// compiler-reported references). Consumers that reason about execution
// (impact, hot paths, test reachability) filter on Type == "calls" and are
// untouched; dead-code counts every non-unresolved edge as usage, which is
// exactly the intended effect: a function registered as a route handler, job
// config, or event callback is *used*, and must not be reported dead.
func emitJSValueRefEdges(idx *Index, file, body string, baseOffset int, fullContent string, res ScanResult) {
	refs := scanJSValueRefs(body, res)
	if len(refs) == 0 {
		return
	}
	bindings := jsImportBindingTargets(idx, file, res.Imports)
	fileStarts := lineStarts(fullContent)
	seen := map[string]bool{}
	for _, ref := range refs {
		target, confidence := resolveJSValueRef(idx, file, ref.Name, bindings)
		if target == "" {
			continue
		}
		line, col := offsetToLineCol(fileStarts, baseOffset+ref.Start)
		from := idx.containingSymbolFast(file, line)
		fromID := from.ID
		if fromID == "" {
			fromID = fileSymbolID(file)
		}
		if fromID == target {
			continue // self-reference (e.g. a recursive setTimeout) is not external evidence of use
		}
		key := fromID + "\x00" + target
		if seen[key] {
			continue
		}
		seen[key] = true
		idx.AddCGPEdge(fromID, target, "references-symbol", confidence, Location{
			File: file, StartLine: line, StartColumn: col,
			EndLine: line, EndColumn: col + len(ref.Name),
			Kind: "value-ref", Raw: ref.Name,
		})
	}
}

// jsValueRefsInExpression resolves bare identifiers inside a Vue template
// expression (`v-if="canEdit && isNearExpiry(d)"`, `:disabled="isLocked"`)
// against the component's own script symbols and reports them as
// references-symbol edges from the component. Calls inside the expression are
// handled separately by emitVueTemplateExpressionCalls; this covers the
// identifiers that are *read*, which previously left script members used
// only by the template looking dead.
func jsValueRefsInExpression(idx *Index, file, fromID, expr string, starts []int, offset int) {
	res := ParseJS(expr)
	refs := scanJSValueRefs(expr, res)
	if len(refs) == 0 {
		return
	}
	seen := map[string]bool{}
	for _, ref := range refs {
		if strings.TrimSpace(ref.Name) == "" {
			continue
		}
		target, confidence := resolveJSValueRef(idx, file, ref.Name, nil)
		if target == "" || target == fromID || seen[target] {
			continue
		}
		seen[target] = true
		line, col := offsetToLineCol(starts, offset+ref.Start)
		idx.AddCGPEdge(fromID, target, "references-symbol", confidence, Location{
			File: file, StartLine: line, StartColumn: col,
			EndLine: line, EndColumn: col + len(ref.Name),
			Kind: "value-ref", Raw: ref.Name,
		})
	}
}

package mamari

import (
	"fmt"
	"strings"
)

// ScannedSymbol is a structural declaration recovered from JS/TS source.
type ScannedSymbol struct {
	Kind      string // function | class | method | getter | setter | interface | type | enum | constant
	Name      string
	Start     int // byte offset of the first character of the declaration (incl. modifiers)
	End       int // byte offset just past the closing brace or terminator
	NameStart int // byte offset of the identifier itself
	Exported  bool
	Default   bool
	Static    bool
	Private   bool
	Async     bool
	Parent    string // owning class name for methods
}

// ScannedImport is an `import ...` or re-exporting `export ... from '...'` statement.
type ScannedImport struct {
	Spec     string
	Bindings []ScannedImportBinding
	Start    int
	End      int
	IsExport bool
}

type ScannedImportBinding struct {
	Imported    string
	Local       string
	IsNamespace bool
	IsDefault   bool
}

// ScannedCall is a function/method invocation site. Callee is the dotted path
// at the call (`foo`, `obj.foo`, `obj.bar.baz`) — preserving full chain depth.
type ScannedCall struct {
	Callee      string
	Start       int
	End         int
	Constructor bool // true for `new Callee(...)`
}

type ScanResult struct {
	Symbols     []ScannedSymbol
	Imports     []ScannedImport
	Calls       []ScannedCall
	Diagnostics []ScanDiagnostic
}

// ScanDiagnostic flags a region the parser could not handle cleanly. The
// parser keeps running past the issue, so a diagnostic is advisory: symbols
// and calls may still be valid, but agents should treat the file as suspect.
type ScanDiagnostic struct {
	Code    string // e.g. "unbalanced_braces".
	Message string
}

// ParseJS produces structural information from a JS/TS source string. The
// implementation walks a token stream from TokenizeJS, so strings, comments,
// templates, and regex literals are guaranteed to be skipped — the regex
// pathologies that affected the previous scanner (false call edges from
// strings/comments, missing class methods, broken end-line detection on
// generic constraints) do not apply here.
func ParseJS(src string) ScanResult {
	p := &jsParser{src: src, tokens: TokenizeJS(src)}
	p.run()
	if p.minBraceDepth < 0 {
		p.result.Diagnostics = append(p.result.Diagnostics, ScanDiagnostic{
			Code:    "unbalanced_braces",
			Message: "closing brace without matching open",
		})
	} else if p.braceDepth != 0 {
		p.result.Diagnostics = append(p.result.Diagnostics, ScanDiagnostic{
			Code:    "unbalanced_braces",
			Message: "input ended with open braces",
		})
	}
	return p.result
}

type jsParser struct {
	src            string
	tokens         []Token
	i              int
	braceDepth     int
	minBraceDepth  int
	classes        []classFrame
	objectLiterals []objectLiteralFrame
	result         ScanResult
}

type classFrame struct {
	name       string
	braceDepth int // depth at which class body is live
	symbolIdx  int // index into result.Symbols for the class
}

// objectLiteralFrame mirrors classFrame, but for an object literal assigned
// via `module.exports = {...}` / `exports.x = {...}` / `const x = {...}`.
// It exists so the main run() loop dispatches to parseObjectLiteralMember
// for each property — the same way it already dispatches to
// parseClassMember inside a class body — instead of using a separate,
// self-contained scanning loop. A self-contained loop cannot call
// maybeRecordCall() on the tokens a property's function-like value leaves
// unconsumed (see skipFunctionExpressionValue), so calls inside an
// object-literal property's body would vanish even after the value itself
// got a symbol.
type objectLiteralFrame struct {
	parent     string // attached to each property's emitted symbol for a readable qualified name
	braceDepth int    // depth at which the object literal's body is live
	symbolIdx  int    // index into result.Symbols to finalize End for, or -1 if none (e.g. `module.exports = {...}` emits no backing symbol)
}

func (p *jsParser) run() {
	for p.i < len(p.tokens) {
		t := p.tokens[p.i]

		// Comments are filtered by TokenizeJS into TokComment / TokLineComment;
		// they're harmless here but we skip them to keep the cursor moving.
		if t.Kind == TokComment || t.Kind == TokLineComment {
			p.i++
			continue
		}
		if t.Kind == TokTemplate {
			p.recordTemplateCalls(t)
			p.i++
			continue
		}

		if t.Kind == TokPunct && t.End-t.Start == 1 {
			switch p.src[t.Start] {
			case '{':
				p.braceDepth++
				p.i++
				continue
			case '}':
				if len(p.classes) > 0 {
					top := p.classes[len(p.classes)-1]
					if p.braceDepth == top.braceDepth {
						p.result.Symbols[top.symbolIdx].End = t.End
						p.classes = p.classes[:len(p.classes)-1]
					}
				}
				p.popObjectLiteralFrame(t.End)
				p.braceDepth--
				if p.braceDepth < p.minBraceDepth {
					p.minBraceDepth = p.braceDepth
				}
				p.i++
				continue
			}
		}

		// Decide what we're parsing based on scope. parseTopLevelDecl is only
		// safe at the actual module top level — running it inside function
		// bodies would consume `const x = expr()` as a top-level decl and
		// the call inside `expr()` would be lost.
		switch {
		case p.inClassBody():
			if p.parseClassMember() {
				continue
			}
		case p.inObjectLiteralBody():
			if p.parseObjectLiteralMember(p.objectLiterals[len(p.objectLiterals)-1].parent) {
				continue
			}
		case p.braceDepth == 0:
			if p.parseImportOrExport() {
				continue
			}
			if p.parseTopLevelDecl() {
				continue
			}
		}

		p.maybeRecordCall()
		p.i++
	}
}

func (p *jsParser) inClassBody() bool {
	if len(p.classes) == 0 {
		return false
	}
	return p.braceDepth == p.classes[len(p.classes)-1].braceDepth
}

func (p *jsParser) inObjectLiteralBody() bool {
	if len(p.objectLiterals) == 0 {
		return false
	}
	return p.braceDepth == p.objectLiterals[len(p.objectLiterals)-1].braceDepth
}

// popObjectLiteralFrame finalizes and pops the top object-literal frame if
// the "}" just encountered closes it (braceDepth still reflects the depth
// inside the body, mirroring how the class-frame pop above runs before
// braceDepth is decremented).
func (p *jsParser) popObjectLiteralFrame(braceEnd int) {
	if len(p.objectLiterals) == 0 {
		return
	}
	top := p.objectLiterals[len(p.objectLiterals)-1]
	if p.braceDepth != top.braceDepth {
		return
	}
	if top.symbolIdx >= 0 {
		p.result.Symbols[top.symbolIdx].End = braceEnd
	}
	p.objectLiterals = p.objectLiterals[:len(p.objectLiterals)-1]
}

func (p *jsParser) peek(offset int) *Token {
	idx := p.i + offset
	for idx < len(p.tokens) {
		k := p.tokens[idx].Kind
		if k == TokComment || k == TokLineComment {
			idx++
			offset++
			continue
		}
		return &p.tokens[idx]
	}
	return nil
}

func (p *jsParser) consume() *Token {
	for p.i < len(p.tokens) {
		t := &p.tokens[p.i]
		p.i++
		if t.Kind == TokComment || t.Kind == TokLineComment {
			continue
		}
		if t.Kind == TokPunct && t.End-t.Start == 1 {
			switch p.src[t.Start] {
			case '{':
				p.braceDepth++
			case '}':
				if len(p.classes) > 0 {
					top := p.classes[len(p.classes)-1]
					if p.braceDepth == top.braceDepth {
						p.result.Symbols[top.symbolIdx].End = t.End
						p.classes = p.classes[:len(p.classes)-1]
					}
				}
				p.popObjectLiteralFrame(t.End)
				p.braceDepth--
				if p.braceDepth < p.minBraceDepth {
					p.minBraceDepth = p.braceDepth
				}
			}
		}
		return t
	}
	return nil
}

func (p *jsParser) matchKeyword(kw string) bool {
	t := p.peek(0)
	if t == nil || t.Kind != TokKeyword || t.Value != kw {
		return false
	}
	p.consume()
	return true
}

func (p *jsParser) matchPunct(value string) bool {
	t := p.peek(0)
	if t == nil || t.Kind != TokPunct || t.Value != value {
		return false
	}
	p.consume()
	return true
}

func (p *jsParser) peekKeyword(kw string) bool {
	t := p.peek(0)
	return t != nil && t.Kind == TokKeyword && t.Value == kw
}

func (p *jsParser) peekPunct(value string) bool {
	t := p.peek(0)
	return t != nil && t.Kind == TokPunct && t.Value == value
}

func contextualIdentKeyword(value string) bool {
	switch value {
	case "from", "of", "as", "get", "set", "async", "type", "namespace", "is", "keyof":
		return true
	}
	return false
}

func callIdentToken(t *Token) bool {
	if t == nil {
		return false
	}
	if t.Kind == TokIdent {
		return true
	}
	if t.Kind != TokKeyword {
		return false
	}
	return contextualIdentKeyword(t.Value) || t.Value == "this" || t.Value == "super"
}

// parseImportOrExport handles `import`, `import type`, `import * as`, and
// `export ... from '...'` re-exports. Returns true if it consumed something.
func (p *jsParser) parseImportOrExport() bool {
	save := p.i
	saveDepth := p.braceDepth

	if p.peekKeyword("import") {
		// `import('x')` is a dynamic import expression — left to
		// scanJSDynamicImports, which walks the whole body (dynamic imports
		// commonly nest deep inside route tables like
		// `const routes = [{ component: () => import('./X.vue') }]`, where a
		// top-level-only check never reaches them).
		next := p.peek(1)
		if next != nil && next.Kind == TokPunct && next.Value == "(" {
			return false
		}
		startTok := p.peek(0)
		p.consume()
		stmt := ScannedImport{Start: startTok.Start}
		stmt = p.parseImportClause(stmt)
		if stmt.Spec != "" {
			stmt.End = p.lastConsumedEnd(startTok.End)
			p.result.Imports = append(p.result.Imports, stmt)
			p.consumeStatementTerminator()
			return true
		}
		// Roll back if we couldn't parse a usable import.
		p.i = save
		p.braceDepth = saveDepth
		return false
	}

	if p.peekKeyword("export") {
		// We only treat `export ... from '...'` as an import edge here.
		// `export {decl}` (no `from`) and `export default X` flow through the
		// top-level decl path so the underlying decl is registered as exported.
		first := p.peek(1)
		if first == nil {
			return false
		}
		// `export * from '...'` or `export { ... } from '...'`
		if first.Kind == TokPunct && (first.Value == "*" || first.Value == "{") {
			startTok := p.peek(0)
			p.consume() // export
			stmt := ScannedImport{Start: startTok.Start, IsExport: true}
			if p.matchPunct("*") {
				if p.peekKeyword("as") {
					p.consume()
					if name := p.peek(0); name != nil && (name.Kind == TokIdent || name.Kind == TokKeyword) {
						stmt.Bindings = append(stmt.Bindings, ScannedImportBinding{Local: name.Value, IsNamespace: true})
						p.consume()
					}
				} else {
					stmt.Bindings = append(stmt.Bindings, ScannedImportBinding{IsNamespace: true})
				}
			} else if p.matchPunct("{") {
				p.parseNamedBindings(&stmt)
			}
			if !p.matchKeyword("from") {
				p.i = save
				p.braceDepth = saveDepth
				return false
			}
			if spec := p.consumeString(); spec != "" {
				stmt.Spec = spec
				stmt.End = p.lastConsumedEnd(startTok.End)
				p.result.Imports = append(p.result.Imports, stmt)
				p.consumeStatementTerminator()
				return true
			}
			p.i = save
			p.braceDepth = saveDepth
			return false
		}
	}
	return false
}

// parseImportClause assumes `import` was already consumed.
func (p *jsParser) parseImportClause(stmt ScannedImport) ScannedImport {
	// `import type ...` — keep parsing, treat as a normal import for graph purposes.
	if p.peekKeyword("type") {
		p.consume()
	}

	// `import 'module'`
	if t := p.peek(0); t != nil && t.Kind == TokString {
		stmt.Spec = t.Value
		p.consume()
		return stmt
	}

	// Default binding: `import X` or `import X, ...`
	if t := p.peek(0); t != nil && (t.Kind == TokIdent || (t.Kind == TokKeyword && contextualIdentKeyword(t.Value))) {
		stmt.Bindings = append(stmt.Bindings, ScannedImportBinding{Imported: "default", Local: t.Value, IsDefault: true})
		p.consume()
		if !p.matchPunct(",") {
			if !p.matchKeyword("from") {
				return ScannedImport{}
			}
			if spec := p.consumeString(); spec != "" {
				stmt.Spec = spec
				return stmt
			}
			return ScannedImport{}
		}
	}

	// `* as NS`
	if p.matchPunct("*") {
		if !p.matchKeyword("as") {
			return ScannedImport{}
		}
		t := p.peek(0)
		if t == nil || (t.Kind != TokIdent && t.Kind != TokKeyword) {
			return ScannedImport{}
		}
		stmt.Bindings = append(stmt.Bindings, ScannedImportBinding{Local: t.Value, IsNamespace: true})
		p.consume()
	} else if p.matchPunct("{") {
		p.parseNamedBindings(&stmt)
	}

	if !p.matchKeyword("from") {
		return ScannedImport{}
	}
	spec := p.consumeString()
	if spec == "" {
		return ScannedImport{}
	}
	stmt.Spec = spec
	return stmt
}

func (p *jsParser) parseNamedBindings(stmt *ScannedImport) {
	for !p.peekPunct("}") {
		// Optional `type` modifier on individual binding.
		if p.peekKeyword("type") {
			p.consume()
		}
		t := p.peek(0)
		if t == nil {
			return
		}
		imported := t.Value
		local := imported
		p.consume()
		if p.peekKeyword("as") {
			p.consume()
			if next := p.peek(0); next != nil && (next.Kind == TokIdent || next.Kind == TokKeyword) {
				local = next.Value
				p.consume()
			}
		}
		stmt.Bindings = append(stmt.Bindings, ScannedImportBinding{Imported: imported, Local: local})
		if !p.matchPunct(",") {
			break
		}
	}
	p.matchPunct("}")
}

// parseDestructuringBindings parses the inside of an object destructuring
// pattern (`a, b: c, d = 5, ...rest`), starting just after the opening `{`
// and stopping at (without consuming) the closing `}`. Renaming uses `:`
// (object destructuring), not `as` (which is import-clause syntax) — this is
// otherwise the same shape as parseNamedBindings.
func (p *jsParser) parseDestructuringBindings() ([]ScannedImportBinding, bool) {
	var bindings []ScannedImportBinding
	for !p.peekPunct("}") {
		if p.matchPunct("...") {
			if t := p.peek(0); t != nil && (t.Kind == TokIdent || t.Kind == TokKeyword) {
				p.consume()
			}
			if !p.matchPunct(",") {
				break
			}
			continue
		}
		t := p.peek(0)
		if t == nil || (t.Kind != TokIdent && t.Kind != TokKeyword) {
			return nil, false
		}
		imported := t.Value
		local := imported
		p.consume()
		if p.matchPunct(":") {
			next := p.peek(0)
			if next == nil || (next.Kind != TokIdent && next.Kind != TokKeyword) {
				return nil, false
			}
			local = next.Value
			p.consume()
		}
		bindings = append(bindings, ScannedImportBinding{Imported: imported, Local: local})
		if p.peekPunct("=") {
			p.consume()
			p.skipExpression() // consumes a trailing ',' itself, or stops before '}'
			continue
		}
		if !p.matchPunct(",") {
			break
		}
	}
	return bindings, true
}

// tryParseRequireCall recognizes `require('spec')` at the current position
// and, on success, consumes it and returns the spec. On failure it leaves
// the cursor untouched.
func (p *jsParser) tryParseRequireCall() (string, bool) {
	save := p.i
	t := p.peek(0)
	if t == nil || t.Kind != TokIdent || t.Value != "require" {
		return "", false
	}
	next := p.peek(1)
	if next == nil || next.Kind != TokPunct || next.Value != "(" {
		return "", false
	}
	p.consume() // require
	p.consume() // (
	spec := p.consumeString()
	if spec == "" || !p.matchPunct(")") {
		p.i = save
		return "", false
	}
	return spec, true
}

func (p *jsParser) consumeString() string {
	t := p.peek(0)
	if t == nil || (t.Kind != TokString && t.Kind != TokTemplate) {
		return ""
	}
	value := t.Value
	p.consume()
	return value
}

func (p *jsParser) consumeStatementTerminator() {
	if p.peekPunct(";") {
		p.consume()
	}
}

func (p *jsParser) lastConsumedEnd(fallback int) int {
	if p.i == 0 {
		return fallback
	}
	for j := p.i - 1; j >= 0; j-- {
		k := p.tokens[j].Kind
		if k == TokComment || k == TokLineComment {
			continue
		}
		return p.tokens[j].End
	}
	return fallback
}

// parseTopLevelDecl tries to recognize a top-level declaration starting at
// p.i. On success the relevant tokens are consumed and a symbol is emitted.
func (p *jsParser) parseTopLevelDecl() bool {
	save := p.i
	saveDepth := p.braceDepth

	startTok := p.peek(0)
	if startTok == nil {
		return false
	}
	startOffset := startTok.Start

	exported := p.matchKeyword("export")
	defaultExport := false
	if exported {
		defaultExport = p.matchKeyword("default")
	}

	// Re-export form `export { foo }` (no `from`) is consumed defensively
	// here: we skip past the close brace so identifiers inside don't look
	// like calls.
	if exported && !defaultExport && p.peekPunct("{") {
		p.skipMatchedBraces()
		p.consumeStatementTerminator()
		return true
	}

	// Optional declaration modifiers.
	p.matchKeyword("declare")
	p.matchKeyword("abstract")
	isAsync := p.matchKeyword("async")

	if p.peekKeyword("function") {
		p.consume()
		p.matchPunct("*") // generator
		name := ""
		nameStart := -1
		if t := p.peek(0); t != nil && (t.Kind == TokIdent || t.Kind == TokKeyword) {
			name = t.Value
			nameStart = t.Start
			p.consume()
		} else if defaultExport {
			name = "default"
		} else {
			p.i = save
			p.braceDepth = saveDepth
			return false
		}
		end := p.skipFunctionLikeBody()
		p.emit(ScannedSymbol{
			Kind: "function", Name: name, Start: startOffset, End: end,
			NameStart: nameStart, Exported: exported, Default: defaultExport,
			Async: isAsync,
		})
		return true
	}

	if p.peekKeyword("class") {
		p.consume()
		name := ""
		nameStart := -1
		if t := p.peek(0); t != nil && (t.Kind == TokIdent || t.Kind == TokKeyword) {
			name = t.Value
			nameStart = t.Start
			p.consume()
		} else if defaultExport {
			name = "default"
		} else {
			p.i = save
			p.braceDepth = saveDepth
			return false
		}
		// Skip generic params, extends/implements clauses up to the body brace.
		p.skipUntilBrace()
		// We are now positioned AT the `{`. Push class frame using the depth
		// we'll be at AFTER consuming `{`.
		idx := len(p.result.Symbols)
		p.emit(ScannedSymbol{
			Kind: "class", Name: name, Start: startOffset, End: 0,
			NameStart: nameStart, Exported: exported, Default: defaultExport,
		})
		// Consume the brace; this increments braceDepth.
		p.consume()
		p.classes = append(p.classes, classFrame{name: name, braceDepth: p.braceDepth, symbolIdx: idx})
		return true
	}

	if p.peekKeyword("interface") {
		p.consume()
		t := p.peek(0)
		if t == nil || (t.Kind != TokIdent && t.Kind != TokKeyword) {
			p.i = save
			p.braceDepth = saveDepth
			return false
		}
		name := t.Value
		nameStart := t.Start
		p.consume()
		end := p.skipBalancedBody()
		p.emit(ScannedSymbol{
			Kind: "interface", Name: name, Start: startOffset, End: end,
			NameStart: nameStart, Exported: exported,
		})
		return true
	}

	if p.peekKeyword("type") {
		p.consume()
		t := p.peek(0)
		if t == nil || (t.Kind != TokIdent && t.Kind != TokKeyword) {
			p.i = save
			p.braceDepth = saveDepth
			return false
		}
		name := t.Value
		nameStart := t.Start
		p.consume()
		end := p.skipUntilTopLevelTerminator()
		p.emit(ScannedSymbol{
			Kind: "type", Name: name, Start: startOffset, End: end,
			NameStart: nameStart, Exported: exported,
		})
		return true
	}

	if p.peekKeyword("enum") {
		p.consume()
		t := p.peek(0)
		if t == nil || (t.Kind != TokIdent && t.Kind != TokKeyword) {
			p.i = save
			p.braceDepth = saveDepth
			return false
		}
		name := t.Value
		nameStart := t.Start
		p.consume()
		end := p.skipBalancedBody()
		p.emit(ScannedSymbol{
			Kind: "enum", Name: name, Start: startOffset, End: end,
			NameStart: nameStart, Exported: exported,
		})
		return true
	}

	if p.peekKeyword("const") || p.peekKeyword("let") || p.peekKeyword("var") {
		p.consume()

		// CommonJS destructured require: `const { a, b: c } = require('spec')`.
		// Recorded as a ScannedImport (like an ES named import) so call
		// resolution can later map `a.method()`/`c.method()` back to the
		// required file, the same way `import { a } from 'spec'` would.
		if p.peekPunct("{") {
			p.consume() // '{'
			bindings, ok := p.parseDestructuringBindings()
			if !ok || !p.matchPunct("}") || !p.peekPunct("=") {
				p.i = save
				p.braceDepth = saveDepth
				return false
			}
			p.consume() // '='
			if spec, isRequire := p.tryParseRequireCall(); isRequire {
				// require(...) is already fully consumed — only an optional
				// trailing ';' remains, not a whole statement to skip to.
				// skipUntilTopLevelTerminator would over-consume into the
				// next declaration on semicolon-less lines (no ASI handling
				// for const/function/class, only export/import).
				end := p.lastConsumedEnd(0)
				p.consumeStatementTerminator()
				p.result.Imports = append(p.result.Imports, ScannedImport{
					Spec: spec, Bindings: bindings, Start: startOffset, End: end,
				})
				return true
			}
			// RHS isn't require(...) — not an import-like binding we can
			// resolve to a file, but still consume the value and record any
			// calls inside it (e.g. destructuring a function-call result).
			valueStart := p.lastConsumedEnd(0)
			if t := p.peek(0); t != nil {
				valueStart = t.Start
			}
			end := p.skipTopLevelValue()
			p.recordCallsInRange(valueStart, end)
			return true
		}

		t := p.peek(0)
		if t == nil || (t.Kind != TokIdent && t.Kind != TokKeyword) {
			p.i = save
			p.braceDepth = saveDepth
			return false
		}
		name := t.Value
		nameStart := t.Start
		p.consume()
		p.skipTypeAnnotation()
		if !p.peekPunct("=") {
			p.i = save
			p.braceDepth = saveDepth
			return false
		}
		// Consume `=` and decide whether the RHS is a function-like.
		p.consume()
		if p.looksLikeFunctionExpression() {
			end, _ := p.skipFunctionExpressionValue()
			p.emit(ScannedSymbol{
				Kind: "function", Name: name, Start: startOffset, End: end,
				NameStart: nameStart, Exported: exported, Async: isAsync,
			})
			return true
		}
		// Object literal: `const handlers = { foo: () => {...}, bar() {...} }`.
		// Scan its properties for function-valued ones (see
		// enterObjectLiteralBody) instead of falling back to the generic
		// "record as a constant" handling below — without this, an
		// anonymous arrow/function property had no symbol at all and any
		// call inside it vanished. The constant symbol for
		// `name` itself is emitted with a placeholder End that
		// popObjectLiteralFrame fills in once the matching "}" is reached
		// (the same pattern the "class" branch above uses).
		if p.peekPunct("{") {
			idx := len(p.result.Symbols)
			p.emit(ScannedSymbol{
				Kind: "constant", Name: name, Start: startOffset, End: 0,
				NameStart: nameStart, Exported: exported,
			})
			p.enterObjectLiteralBody(name, idx)
			return true
		}
		// Bare CommonJS require: `const emailHandler = require('spec')`.
		// Recorded as both a constant (existing behavior) and a default-style
		// ScannedImport binding so `emailHandler.method()` call sites can
		// resolve cross-file to whatever 'spec' exports (e.g. a singleton
		// `module.exports = new EmailHandler()`).
		if spec, isRequire := p.tryParseRequireCall(); isRequire {
			// See the destructured-require branch above for why this isn't
			// skipUntilTopLevelTerminator: the call is already fully
			// consumed, so over-skipping on a semicolon-less line would eat
			// the next declaration(s) too.
			end := p.lastConsumedEnd(0)
			p.consumeStatementTerminator()
			p.result.Imports = append(p.result.Imports, ScannedImport{
				Spec: spec,
				Bindings: []ScannedImportBinding{
					{Imported: "default", Local: name, IsDefault: true},
				},
				Start: startOffset, End: end,
			})
			p.emit(ScannedSymbol{
				Kind: "constant", Name: name, Start: startOffset, End: end,
				NameStart: nameStart, Exported: exported,
			})
			return true
		}
		// Not function-like — record as a constant. We still emit it because
		// dotted `Foo.x` calls might resolve through it later.
		valueStart := p.lastConsumedEnd(0)
		if t := p.peek(0); t != nil {
			valueStart = t.Start
		}
		end := p.skipTopLevelValue()
		p.recordCallsInRange(valueStart, end)
		p.emit(ScannedSymbol{
			Kind: "constant", Name: name, Start: startOffset, End: end,
			NameStart: nameStart, Exported: exported,
		})
		return true
	}

	// Bare `export default expression` (no decl keyword). We synthesize a
	// "default" function symbol when the RHS looks like a function.
	if exported && defaultExport {
		if p.looksLikeFunctionExpression() {
			end, _ := p.skipFunctionExpressionValue()
			p.emit(ScannedSymbol{
				Kind: "function", Name: "default", Start: startOffset, End: end,
				Exported: true, Default: true, Async: isAsync,
			})
			return true
		}
		if p.peekPunct("{") {
			p.enterObjectLiteralBody("default", -1)
			return true
		}
		// Plain default export — skip to terminator.
		p.skipUntilTopLevelTerminator()
		return true
	}

	if exported {
		// Unrecognized export form — don't roll back, just drop the export
		// keyword we consumed so we don't loop forever.
		return true
	}

	// Bare top-level assignment to a dotted target — `module.exports = ...`,
	// `exports.foo = ...` — whose value is function-like or an object
	// literal. Assignments aren't declarations, so they were never routed
	// through the const/let/var handling above: an anonymous arrow/function
	// value (or an object literal's function-valued properties) had no
	// symbol at all, and any call inside it vanished completely (not even
	// recorded as unresolved) because containingSymbolFast found nothing to
	// attribute it to.
	//
	// Anything else (require() re-exports, `module.exports = new X()`,
	// bare identifiers/literals, ...) is intentionally left to the existing
	// generic call-scanning walk — tryParseDottedAssignmentTarget rolls back
	// fully on failure, and the two checks below roll back too if the value
	// isn't one of the two shapes this branch exists to handle, so this is
	// strictly additive: nothing that worked before changes.
	if name, nameStart, ok := p.tryParseDottedAssignmentTarget(); ok {
		if p.looksLikeFunctionExpression() {
			if p.isNamedFunctionExpression() {
				// `module.exports = function doWork(...) {...}` — already
				// handled correctly without this branch: the run loop
				// retries parseTopLevelDecl at every token, so once it
				// reaches the `function` keyword itself (regardless of
				// what assigned it), the dedicated `function` branch above
				// fires and uses the function's own name. Roll back so
				// that happens, instead of emitting a less-specific symbol
				// named after the assignment target ("exports"/"foo").
				p.i = save
				p.braceDepth = saveDepth
				return false
			}
			end, _ := p.skipFunctionExpressionValue()
			p.emit(ScannedSymbol{
				Kind: "function", Name: name, Start: startOffset, End: end,
				NameStart: nameStart,
			})
			return true
		}
		if p.peekPunct("{") {
			p.enterObjectLiteralBody(name, -1) // no backing symbol for "exports"/"module.exports" itself
			return true
		}
		p.i = save
		p.braceDepth = saveDepth
		return false
	}

	p.i = save
	p.braceDepth = saveDepth
	return false
}

// parseClassMember handles methods (regular, async, static, generator),
// getters/setters, and private fields/methods. Returns true if consumed.
func (p *jsParser) parseClassMember() bool {
	if len(p.classes) == 0 {
		return false
	}
	className := p.classes[len(p.classes)-1].name
	save := p.i
	saveDepth := p.braceDepth

	startTok := p.peek(0)
	if startTok == nil {
		return false
	}
	startOffset := startTok.Start

	// Skip decorators: `@Decorator(args)` or `@Decorator`.
	for p.peekPunct("@") {
		p.consume()
		// Decorator name (possibly dotted).
		for {
			if t := p.peek(0); t != nil && (t.Kind == TokIdent || t.Kind == TokKeyword) {
				p.consume()
				if p.matchPunct(".") {
					continue
				}
			}
			break
		}
		if p.peekPunct("(") {
			p.skipBalancedParens()
		}
	}

	// Modifiers — order can vary.
	isStatic := false
	isAsync := false
	kind := "method"
	for {
		switch {
		case p.peekKeyword("static"):
			isStatic = true
			p.consume()
		case p.peekKeyword("public"), p.peekKeyword("protected"), p.peekKeyword("readonly"), p.peekKeyword("abstract"), p.peekKeyword("declare"):
			p.consume()
		case p.peekKeyword("private"):
			p.consume()
		case p.peekKeyword("async"):
			isAsync = true
			p.consume()
		default:
			goto modsDone
		}
	}
modsDone:

	// Generator `*` before name.
	p.matchPunct("*")

	// Getter / setter.
	if p.peekKeyword("get") {
		// Could be a method literally named "get" — disambiguate: if next is
		// an ident or string, this is a getter declaration.
		next := p.peek(1)
		if next != nil && (next.Kind == TokIdent || next.Kind == TokKeyword || next.Kind == TokString) {
			p.consume()
			kind = "getter"
		}
	} else if p.peekKeyword("set") {
		next := p.peek(1)
		if next != nil && (next.Kind == TokIdent || next.Kind == TokKeyword || next.Kind == TokString) {
			p.consume()
			kind = "setter"
		}
	}

	// Member name. Accept ident, keyword (e.g. method literally named
	// "delete"), private `#name`, or quoted name.
	name := ""
	nameStart := -1
	if t := p.peek(0); t != nil {
		switch t.Kind {
		case TokIdent, TokKeyword:
			name = t.Value
			nameStart = t.Start
			p.consume()
		case TokString:
			name = t.Value
			nameStart = t.Start
			p.consume()
		case TokPunct:
			if t.Value == "[" {
				// Computed property name: `[expr]`. Skip the brackets and
				// emit a synthetic name.
				p.skipBalancedBrackets()
				name = "[computed]"
				nameStart = t.Start
			}
		}
	}
	if name == "" {
		p.i = save
		p.braceDepth = saveDepth
		return false
	}

	// Field declaration vs method: a method has `(` next.
	if !p.peekPunct("(") {
		// Treat as a field — skip to `;` or newline. We don't emit field
		// symbols today (no value to agents looking for callable units).
		p.skipUntilSemicolonAtClassDepth()
		return true
	}

	end := p.skipFunctionLikeBody()
	private := strings.HasPrefix(name, "#")
	p.emit(ScannedSymbol{
		Kind: kind, Name: name, Start: startOffset, End: end,
		NameStart: nameStart, Static: isStatic, Async: isAsync,
		Private: private, Parent: className,
	})
	return true
}

func (p *jsParser) emit(sym ScannedSymbol) {
	p.result.Symbols = append(p.result.Symbols, sym)
}

// enterObjectLiteralBody consumes an object literal's opening "{" and
// pushes an objectLiteralFrame so the main run() loop dispatches each
// property to parseObjectLiteralMember from here on (the same way it
// already dispatches to parseClassMember inside a class body), until the
// matching "}" pops the frame. symbolIdx is the index into result.Symbols
// of a backing symbol whose End should be finalized when the frame pops
// (mirroring how a class's own symbol gets its End filled in when its body
// closes), or -1 if there is no such symbol (e.g. `module.exports = {...}`
// has nothing backing "exports" itself).
//
// This (replacing an earlier, simpler self-contained scanning loop) exists
// because object literals assigned via `module.exports = {...}` /
// `exports.x = {...}` / `const x = {...}` previously produced no symbol at
// all for an anonymous function-valued property (`foo: () => {...}`,
// `foo() {...}`) — only a *named* `function foo() {...}` expression
// happened to be picked up, because parseTopLevelDecl is retried on every
// token at brace depth 0 and a `function` keyword matches regardless of
// what assigned it. An anonymous arrow/function value has no such luck, so
// containingSymbolFast found nothing for any call inside it, and the call
// itself vanished entirely (not even recorded as unresolved). A
// self-contained scanning loop fixed the missing symbol but NOT this: it
// cannot call maybeRecordCall() on the tokens a property's function-like
// value leaves unconsumed (see skipFunctionExpressionValue), so calls
// inside the property's body still vanished. Integrating into run()'s own
// dispatch — exactly like class bodies already work — fixes both.
func (p *jsParser) enterObjectLiteralBody(parent string, symbolIdx int) {
	p.consume() // '{', increments braceDepth
	p.objectLiterals = append(p.objectLiterals, objectLiteralFrame{
		parent: parent, braceDepth: p.braceDepth, symbolIdx: symbolIdx,
	})
}

// scanRangeForCalls advances the cursor token-by-token up to (not
// including) the first token whose Start >= endOffset, calling
// maybeRecordCall at each step exactly the way run()'s own fallback path
// does (p.maybeRecordCall(); p.i++ — maybeRecordCall may itself advance
// p.i further on a matched dotted chain, which is why the order matters).
// Used only for a concise arrow body's expression tokens inside an object
// literal property, where we deliberately do NOT want run()'s dispatch
// switch to run (it would wrongly try to parse the expression's own
// tokens as the next property — see parseObjectLiteralMember).
func (p *jsParser) scanRangeForCalls(endOffset int) {
	for {
		t := p.peek(0)
		if t == nil || t.Start >= endOffset {
			return
		}
		p.maybeRecordCall()
		p.i++
	}
}

// parseObjectLiteralMember recognizes one property of an object literal and
// emits a symbol for it if (and only if) its value is function-like:
//
//	name: function(...) {...}      name: async function(...) {...}
//	name: (...) => ...              name: async (...) => ...
//	name(...) {...}                 async name(...) {...}
//	get name() {...}                set name(v) {...}
//
// Properties whose value is NOT function-like (numbers, strings, nested
// objects, call expressions, ...), shorthand properties (`{ x }`), spreads
// (`...expr`), and computed keys (`[expr]: ...`) are skipped without
// emitting a symbol — but any calls inside a non-function value are still
// recorded via recordCallsInRange, exactly as a top-level non-function
// `const x = ...` already does. Called once per token by run()'s dispatch
// while inObjectLiteralBody() — see enterObjectLiteralBody — so it also
// handles a bare "," between properties itself (returning true) and must
// never assume it owns a multi-token loop the way parseObjectLiteralMember
// no longer does; the matching "}" is intercepted by run()/consume()
// before this is ever called with it.
//
// parent only affects the emitted symbol's qualified name (see
// emitJSTSSymbolsScoped: `sym.Parent` is resolved to a real ParentID only
// for Kind "method"/"getter"/"setter" AND only when a "class" symbol with
// a matching name was emitted in the same scan — which never happens for an
// object literal, since there is no separate class-like symbol backing
// `module.exports` or a `const` object). It still gives properties a more
// readable, collision-resistant qualified name (e.g. "exports.doWork"
// instead of a bare "doWork" that could collide with an unrelated
// same-named top-level function).
func (p *jsParser) parseObjectLiteralMember(parent string) bool {
	// The separator between properties — run()'s dispatch calls us per
	// token while inObjectLiteralBody(), so we (not a pre-filtering loop)
	// see commas directly.
	if p.matchPunct(",") {
		return true
	}

	startTok := p.peek(0)
	if startTok == nil {
		return false
	}
	startOffset := startTok.Start

	// Spread (`...expr`) or computed key (`[expr]: ...`) — nothing to name;
	// just account for any calls inside the value and move on.
	if p.peekPunct("...") || p.peekPunct("[") {
		if p.peekPunct("...") {
			p.consume()
		} else {
			p.skipBalancedBrackets()
			if !p.matchPunct(":") {
				return true // computed shorthand method `[expr]() {}` — skip its body below via the generic path
			}
		}
		valueStart := p.lastConsumedEnd(0)
		if t := p.peek(0); t != nil {
			valueStart = t.Start
		}
		end := p.skipExpression()
		p.recordCallsInRange(valueStart, end)
		return true
	}

	isAsync := false
	if p.peekKeyword("async") {
		isAsync = true
		p.consume()
	}
	p.matchPunct("*") // generator — recorded as a plain function; the
	// distinction isn't meaningful for call resolution/dead-code purposes.

	kind := "function"
	if p.peekKeyword("get") {
		if next := p.peek(1); next != nil && (next.Kind == TokIdent || next.Kind == TokKeyword || next.Kind == TokString) {
			p.consume()
			kind = "getter"
		}
	} else if p.peekKeyword("set") {
		if next := p.peek(1); next != nil && (next.Kind == TokIdent || next.Kind == TokKeyword || next.Kind == TokString) {
			p.consume()
			kind = "setter"
		}
	}

	name := ""
	nameStart := -1
	switch t := p.peek(0); {
	case t == nil:
		return false
	case t.Kind == TokIdent || t.Kind == TokKeyword:
		name = t.Value
		nameStart = t.Start
		p.consume()
	case t.Kind == TokString || t.Kind == TokNumber:
		name = t.Value
		nameStart = t.Start
		p.consume()
	default:
		return false
	}

	// Method shorthand: `name(...) {...}`.
	if p.peekPunct("(") {
		end := p.skipFunctionLikeBody()
		p.emit(ScannedSymbol{
			Kind: kind, Name: name, Start: startOffset, End: end,
			NameStart: nameStart, Async: isAsync, Parent: parent,
		})
		return true
	}

	if !p.matchPunct(":") {
		// Shorthand property `{ x }` — nothing to scan.
		return true
	}
	if p.peekPunct("{") {
		childParent := name
		if parent != "" {
			childParent = parent + "." + name
		}
		p.enterObjectLiteralBody(childParent, -1)
		return true
	}
	if p.looksLikeFunctionExpression() {
		end, concise := p.skipFunctionExpressionValue()
		p.emit(ScannedSymbol{
			Kind: "function", Name: name, Start: startOffset, End: end,
			NameStart: nameStart, Async: isAsync, Parent: parent,
		})
		// A braced body (`name: () => { ... }`) left its contents
		// unconsumed for "normal traversal" — safe here, because entering
		// "{" increments braceDepth past this object literal's own frame
		// depth, so run()'s dispatch naturally stops treating those tokens
		// as new properties (inObjectLiteralBody() only matches the exact
		// frame depth), and run()'s own brace-tracking correctly pairs that
		// "{" with its "}". A CONCISE body (`name: () => expr`, no braces)
		// has no such depth change to rely on — left alone, run()'s
		// dispatch would wrongly try to parse the expression's own tokens
		// as the NEXT property. Walk it explicitly instead, ONLY for the
		// concise case: calling scanRangeForCalls on a BRACED body's
		// contents would bypass run()'s brace-depth bookkeeping for that
		// body's own "{"/"}", permanently desyncing braceDepth (and with
		// it inObjectLiteralBody) for the rest of the file.
		if concise {
			p.scanRangeForCalls(end)
		}
		return true
	}
	// Non-function value — record any calls inside it without emitting a
	// symbol, matching today's existing behavior for non-function
	// const/let/var values.
	valueStart := p.lastConsumedEnd(0)
	if t := p.peek(0); t != nil {
		valueStart = t.Start
	}
	end := p.skipExpression()
	p.recordCallsInRange(valueStart, end)
	return true
}

// tryParseDottedAssignmentTarget recognizes a bare top-level assignment
// target `IDENT ("." IDENT)+ =` (e.g. `module.exports =`, `exports.foo =`)
// — deliberately requiring at least one "." so a plain top-level `x = ...`
// reassignment of some other declaration is never affected — and reports
// the chain's last identifier (the most descriptive name available: "foo"
// for "exports.foo", "exports" for "module.exports") plus its position.
// Does not consume anything on failure.
func (p *jsParser) tryParseDottedAssignmentTarget() (name string, nameStart int, ok bool) {
	save := p.i
	t := p.peek(0)
	if t == nil || (t.Kind != TokIdent && t.Kind != TokKeyword) {
		return "", 0, false
	}
	p.consume()
	dots := 0
	for p.peekPunct(".") {
		p.consume()
		nt := p.peek(0)
		if nt == nil || (nt.Kind != TokIdent && nt.Kind != TokKeyword) {
			p.i = save
			return "", 0, false
		}
		name = nt.Value
		nameStart = nt.Start
		p.consume()
		dots++
	}
	if dots == 0 || !p.peekPunct("=") {
		p.i = save
		return "", 0, false
	}
	p.consume() // '='
	return name, nameStart, true
}

// isNamedFunctionExpression reports whether the upcoming tokens are
// `function NAME(`, `async function NAME(`, or `function* NAME(` — a named
// function expression, as opposed to an anonymous `function(...) {}` or an
// arrow function (which never has a "function" keyword at all, so this
// correctly returns false for those too).
func (p *jsParser) isNamedFunctionExpression() bool {
	i := 0
	if t := p.peek(i); t != nil && t.Kind == TokKeyword && t.Value == "async" {
		i++
	}
	t := p.peek(i)
	if t == nil || t.Kind != TokKeyword || t.Value != "function" {
		return false
	}
	i++
	if t := p.peek(i); t != nil && t.Kind == TokPunct && t.Value == "*" {
		i++
	}
	nt := p.peek(i)
	return nt != nil && (nt.Kind == TokIdent || nt.Kind == TokKeyword)
}

// looksLikeFunctionExpression peeks at p.i to decide whether the upcoming
// expression is `function ...`, `async function ...`, `(...) =>`, `async (...) =>`,
// or bare `id =>`.
func (p *jsParser) looksLikeFunctionExpression() bool {
	t := p.peek(0)
	if t == nil {
		return false
	}
	if t.Kind == TokKeyword && t.Value == "function" {
		return true
	}
	if t.Kind == TokKeyword && t.Value == "async" {
		next := p.peek(1)
		if next == nil {
			return false
		}
		if next.Kind == TokKeyword && next.Value == "function" {
			return true
		}
		if next.Kind == TokPunct && next.Value == "(" {
			return p.lookaheadArrow(1)
		}
		if next.Kind == TokIdent {
			after := p.peek(2)
			if after != nil && after.Kind == TokPunct && after.Value == "=>" {
				return true
			}
		}
		return false
	}
	if t.Kind == TokPunct && t.Value == "(" {
		return p.lookaheadArrow(0)
	}
	if t.Kind == TokIdent {
		next := p.peek(1)
		if next != nil && next.Kind == TokPunct && next.Value == "=>" {
			return true
		}
	}
	return false
}

// lookaheadArrow scans forward from p.i+offset to see whether a parenthesized
// parameter list is followed by `=>`. We balance parens/brackets/braces but
// reject if we hit a statement terminator first.
func (p *jsParser) lookaheadArrow(offset int) bool {
	idx := p.i + offset
	for idx < len(p.tokens) && (p.tokens[idx].Kind == TokComment || p.tokens[idx].Kind == TokLineComment) {
		idx++
	}
	if idx >= len(p.tokens) {
		return false
	}
	if p.tokens[idx].Kind != TokPunct || p.tokens[idx].Value != "(" {
		return false
	}
	depth := 0
	for ; idx < len(p.tokens); idx++ {
		t := p.tokens[idx]
		if t.Kind != TokPunct {
			continue
		}
		switch t.Value {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			depth--
			if depth == 0 {
				// Look for `=>` after a possible return-type annotation.
				j := idx + 1
				for j < len(p.tokens) {
					tt := p.tokens[j]
					if tt.Kind == TokComment || tt.Kind == TokLineComment {
						j++
						continue
					}
					if tt.Kind == TokPunct && tt.Value == "=>" {
						return true
					}
					if tt.Kind == TokPunct && tt.Value == ":" {
						// Skip return type until we hit `=>`, `;`, or newline-equivalent.
						j = p.skipReturnType(j + 1)
						continue
					}
					return false
				}
				return false
			}
		case ";":
			if depth == 0 {
				return false
			}
		}
	}
	return false
}

func (p *jsParser) skipReturnType(from int) int {
	depth := 0
	for j := from; j < len(p.tokens); j++ {
		t := p.tokens[j]
		if t.Kind != TokPunct {
			continue
		}
		switch t.Value {
		case "(", "[", "{", "<":
			depth++
		case ")", "]", "}", ">":
			depth--
			if depth < 0 {
				return j
			}
		case ">>":
			// The tokenizer glues adjacent `>` characters into one token
			// (so a real `>>` right-shift operator doesn't toggle
			// regex-allowed state — see jstoken.go's punct()), so a nested
			// generic close like `Pick<T, Exclude<K, J>>` arrives as a
			// single ">>" token, not two ">" tokens. Treating it as a
			// single close (the bug this fixes) leaves depth permanently
			// off by one for the rest of the file: every subsequent `;`
			// looks like it's still nested inside the unclosed generic,
			// so this function — and the five others with the identical
			// ">"-only case below it, all fixed the same way — never finds
			// the type alias's real end, silently swallowing everything
			// after it. Confirmed against a real, ordinary generic type
			// alias (`Exclude<keyof T, K>>`-shaped nesting) in production
			// TypeScript, not a contrived example. ">=", ">>=", ">>>=" are
			// real comparison/compound-assignment operators despite
			// sharing a ">" prefix and correctly fall through to the
			// default case (no effect on depth).
			depth -= 2
			if depth < 0 {
				return j
			}
		case ">>>":
			depth -= 3
			if depth < 0 {
				return j
			}
		case ";":
			if depth == 0 {
				return j
			}
		case "=>":
			if depth == 0 {
				return j
			}
		}
	}
	return len(p.tokens)
}

// skipExpression consumes tokens until the end of the current expression
// statement, balancing all bracket forms. Returns the End offset of the last
// consumed token.
func (p *jsParser) skipExpression() int {
	depth := 0
	lastEnd := p.lastConsumedEnd(0)
	for p.i < len(p.tokens) {
		t := p.peek(0)
		if t == nil {
			break
		}
		if t.Kind == TokPunct {
			switch t.Value {
			case "(", "[", "{":
				depth++
				lastEnd = t.End
				p.consume()
				continue
			case ")", "]", "}":
				if depth == 0 {
					return lastEnd
				}
				depth--
				lastEnd = t.End
				p.consume()
				if depth == 0 {
					// ASI: once the outermost bracket group closes, the
					// expression is over unless the next token (after a
					// newline) could plausibly continue it (e.g. `.then(...)`
					// chaining, a binary/ternary operator, or a call/index
					// applied to an IIFE result). See jsExpressionContinues.
					next := p.peek(0)
					if next == nil || (!jsExpressionContinues(next) && strings.Contains(p.src[lastEnd:next.Start], "\n")) {
						return lastEnd
					}
				}
				continue
			case ";", ",":
				if depth == 0 {
					p.consume()
					return lastEnd
				}
			}
		}
		lastEnd = t.End
		p.consume()
		if depth == 0 {
			// Arrow functions in Vue <script setup> are commonly written
			// without semicolons. Once a newline separates the current
			// expression from another top-level declaration, leave that
			// declaration visible to the main parser instead of swallowing the
			// rest of the script as the previous initializer.
			if next := p.peek(0); startsTopLevelDeclaration(next) && strings.Contains(p.src[lastEnd:next.Start], "\n") {
				return lastEnd
			}
		}
	}
	return lastEnd
}

// skipTopLevelValue is like skipExpression but stops when the initial bracket
// group closes (depth returns to 0 via }, ], or )). This handles the ASI case
// where a const value like { a: 'b' } has no trailing semicolon — the next
// export/function declaration must remain visible to the run loop.
func (p *jsParser) skipTopLevelValue() int {
	depth := 0
	lastEnd := p.lastConsumedEnd(0)
	for p.i < len(p.tokens) {
		t := p.peek(0)
		if t == nil {
			break
		}
		if t.Kind == TokPunct {
			switch t.Value {
			case "(", "[", "{":
				depth++
				lastEnd = t.End
				p.consume()
				continue
			case ")", "]", "}":
				if depth == 0 {
					return lastEnd
				}
				depth--
				lastEnd = t.End
				p.consume()
				if depth == 0 {
					return lastEnd
				}
				continue
			case ";", ",":
				if depth == 0 {
					p.consume()
					return lastEnd
				}
			}
		}
		lastEnd = t.End
		p.consume()
		if depth == 0 {
			if next := p.peek(0); startsTopLevelDeclaration(next) && strings.Contains(p.src[lastEnd:next.Start], "\n") {
				return lastEnd
			}
		}
	}
	return lastEnd
}

// skipFunctionExpressionValue consumes the signature of a function-like
// VALUE — `function(...) {...}`, `async function(...) {...}`, `(...) =>
// {...}`, `async (...) => {...}`, `ident => {...}`, and the concise-body
// arrow forms (`(...) => expr`) — and returns the value's end offset.
// Callers must have already confirmed looksLikeFunctionExpression().
//
// Critically, for a braced body this does NOT consume the body's contents
// (mirroring skipFunctionLikeBody, used for NAMED function declarations):
// it consumes only through the opening "{" and returns, leaving the body
// for the normal run() loop to walk token-by-token so calls inside are
// discovered by maybeRecordCall exactly the way they already are inside a
// named function declaration's body. Before this fix, every caller used
// skipExpression here, which consumes the ENTIRE value (signature AND
// body) via its own self-contained loop — so the run() loop never saw any
// of the body's tokens and every call inside a function-like VALUE
// (`const x = () => { ... }`, an object-literal property, an export
// assignment) silently vanished. This is more consequential than the
// originally-reported missing-symbol issue, since it affected every
// top-level arrow/function-expression value already in production, not
// just the newly-added object-literal/export-assignment recognizers.
//
// For a concise arrow body (no braces, e.g. `() => foo()`), there is no
// "{" to enter — the body is already a single expression directly visible
// to the normal run loop once we stop here, so its end is computed via a
// non-consuming lookahead (peekExpressionEnd) instead of being consumed.
// skipFunctionExpressionValue's second return value reports whether the
// body was concise (no braces — its tokens are left completely unconsumed,
// including the leading token, for whoever called this to decide how to
// proceed) as opposed to braced (only the opening "{" is consumed; the
// braced body's contents are left for the NORMAL run() loop, which will
// correctly perform its own brace-depth bookkeeping as it walks them).
// Callers must not treat these two cases the same way — see
// parseObjectLiteralMember, where conflating them once caused braceDepth to
// be permanently corrupted (scanRangeForCalls bypasses run()'s own
// brace-tracking, so calling it on a braced body's contents desynced
// braceDepth for the rest of the file).
func (p *jsParser) skipFunctionExpressionValue() (end int, concise bool) {
	p.matchKeyword("async")
	if p.peekKeyword("function") {
		p.consume()
		p.matchPunct("*") // generator
		if t := p.peek(0); t != nil && (t.Kind == TokIdent || t.Kind == TokKeyword) {
			p.consume() // optional name
		}
		return p.skipFunctionLikeBody(), false
	}
	// Arrow form: `(...) =>` or `ident =>`.
	if p.peekPunct("(") {
		p.skipBalancedParens()
	} else if t := p.peek(0); t != nil && (t.Kind == TokIdent || t.Kind == TokKeyword) {
		p.consume()
	}
	p.skipReturnTypeInline()
	if !p.matchPunct("=>") {
		// Shouldn't happen given looksLikeFunctionExpression already
		// verified this shape, but never get stuck.
		return p.lastConsumedEnd(0), false
	}
	if p.peekPunct("{") {
		bodyEnd := p.peekMatchedBraceEnd(p.i)
		p.consume() // enter the body, increments braceDepth
		return bodyEnd, false
	}
	return p.peekExpressionEnd(), true
}

// peekExpressionEnd is a non-consuming lookahead version of skipExpression,
// used only to compute a concise arrow body's end offset for symbol
// metadata. The tokens themselves are deliberately left unconsumed so the
// normal run loop walks them — see skipFunctionExpressionValue.
func (p *jsParser) peekExpressionEnd() int {
	depth := 0
	idx := p.i
	lastEnd := p.lastConsumedEnd(0)
	for idx < len(p.tokens) {
		t := p.tokens[idx]
		if t.Kind == TokComment || t.Kind == TokLineComment {
			idx++
			continue
		}
		if t.Kind == TokPunct {
			switch t.Value {
			case "(", "[", "{":
				depth++
				lastEnd = t.End
				idx++
				continue
			case ")", "]", "}":
				if depth == 0 {
					return lastEnd
				}
				depth--
				lastEnd = t.End
				idx++
				if depth == 0 {
					next := p.peek(idx - p.i)
					if next == nil || (!jsExpressionContinues(next) && strings.Contains(p.src[lastEnd:next.Start], "\n")) {
						return lastEnd
					}
				}
				continue
			case ";", ",":
				if depth == 0 {
					return lastEnd
				}
			}
		}
		lastEnd = t.End
		idx++
		if depth == 0 {
			next := p.peek(idx - p.i)
			if startsTopLevelDeclaration(next) && next != nil && strings.Contains(p.src[lastEnd:next.Start], "\n") {
				return lastEnd
			}
		}
	}
	return lastEnd
}

func (p *jsParser) skipFunctionLikeBody() int {
	if p.peekPunct("(") {
		p.skipBalancedParens()
	} else {
		return p.lastConsumedEnd(0)
	}
	p.skipReturnTypeInline()
	if p.peekPunct("{") {
		end := p.peekMatchedBraceEnd(p.i)
		p.consume() // enter the body, increments braceDepth
		return end
	}
	// Abstract / overload form: skip to `;`.
	for p.i < len(p.tokens) {
		t := p.peek(0)
		if t == nil {
			break
		}
		if t.Kind == TokPunct && t.Value == ";" {
			end := t.End
			p.consume()
			return end
		}
		p.consume()
	}
	return p.lastConsumedEnd(0)
}

// peekMatchedBraceEnd returns the byte-offset End of the `}` that matches
// the next `{` at or after fromIdx, without advancing the cursor.
func (p *jsParser) peekMatchedBraceEnd(fromIdx int) int {
	depth := 0
	seen := false
	for j := fromIdx; j < len(p.tokens); j++ {
		t := p.tokens[j]
		if t.Kind != TokPunct || t.End-t.Start != 1 {
			continue
		}
		switch p.src[t.Start] {
		case '{':
			depth++
			seen = true
		case '}':
			depth--
			if seen && depth == 0 {
				return t.End
			}
		}
	}
	if len(p.tokens) == 0 {
		return 0
	}
	return p.tokens[len(p.tokens)-1].End
}

func (p *jsParser) skipReturnTypeInline() {
	if !p.peekPunct(":") {
		return
	}
	p.consume()
	depth := 0
	for p.i < len(p.tokens) {
		t := p.peek(0)
		if t == nil {
			return
		}
		if t.Kind == TokPunct {
			switch t.Value {
			case "(", "[", "<", "{":
				if t.Value == "{" && depth == 0 {
					return
				}
				depth++
			case ")", "]", ">", "}":
				if depth == 0 {
					return
				}
				depth--
			case ">>":
				// See skipReturnType's identical case for the full
				// rationale (the tokenizer glues a nested generic close
				// like `Foo<Bar<Baz>>` into one ">>" token, not two ">").
				if depth == 0 {
					return
				}
				if depth >= 2 {
					depth -= 2
				} else {
					depth = 0
				}
			case ">>>":
				if depth == 0 {
					return
				}
				if depth >= 3 {
					depth -= 3
				} else {
					depth = 0
				}
			case "=>":
				if depth == 0 {
					p.consume()
					continue
				}
			case ";", ",":
				if depth == 0 {
					return
				}
			}
		}
		p.consume()
	}
}

// skipBalancedBody assumes the next token is `{` and consumes through the
// matching `}`. Returns the End offset of the closing brace.
func (p *jsParser) skipBalancedBody() int {
	if !p.peekPunct("{") {
		return p.lastConsumedEnd(0)
	}
	depth := 0
	for p.i < len(p.tokens) {
		t := p.peek(0)
		if t == nil {
			break
		}
		if t.Kind == TokPunct {
			switch t.Value {
			case "{":
				depth++
				p.consume()
				continue
			case "}":
				depth--
				end := t.End
				p.consume()
				if depth == 0 {
					return end
				}
				continue
			}
		}
		p.consume()
	}
	return p.lastConsumedEnd(0)
}

func (p *jsParser) skipBalancedParens() {
	if !p.peekPunct("(") {
		return
	}
	depth := 0
	for p.i < len(p.tokens) {
		t := p.peek(0)
		if t == nil {
			break
		}
		if t.Kind == TokPunct {
			switch t.Value {
			case "(":
				depth++
			case ")":
				depth--
				p.consume()
				if depth == 0 {
					return
				}
				continue
			}
		}
		p.consume()
	}
}

func (p *jsParser) skipBalancedBrackets() {
	if !p.peekPunct("[") {
		return
	}
	depth := 0
	for p.i < len(p.tokens) {
		t := p.peek(0)
		if t == nil {
			break
		}
		if t.Kind == TokPunct {
			switch t.Value {
			case "[":
				depth++
			case "]":
				depth--
				p.consume()
				if depth == 0 {
					return
				}
				continue
			}
		}
		p.consume()
	}
}

func (p *jsParser) skipMatchedBraces() {
	if !p.peekPunct("{") {
		return
	}
	depth := 0
	for p.i < len(p.tokens) {
		t := p.peek(0)
		if t == nil {
			break
		}
		if t.Kind == TokPunct {
			switch t.Value {
			case "{":
				depth++
			case "}":
				depth--
				p.consume()
				if depth == 0 {
					return
				}
				continue
			}
		}
		p.consume()
	}
}

// skipUntilBrace consumes tokens up to (but not including) the next top-level
// `{`. Used to traverse generic params, extends/implements clauses, return
// types, etc., before a class or function body opens.
func (p *jsParser) skipUntilBrace() {
	depth := 0
	for p.i < len(p.tokens) {
		t := p.peek(0)
		if t == nil {
			return
		}
		if t.Kind == TokPunct {
			switch t.Value {
			case "(", "[", "<":
				depth++
			case ")", "]", ">":
				if depth > 0 {
					depth--
				}
			case ">>":
				// See skipReturnType's identical case for the full
				// rationale (the tokenizer glues a nested generic close
				// like `Foo<Bar<Baz>>` into one ">>" token, not two ">").
				if depth >= 2 {
					depth -= 2
				} else {
					depth = 0
				}
			case ">>>":
				if depth >= 3 {
					depth -= 3
				} else {
					depth = 0
				}
			case "{":
				if depth == 0 {
					return
				}
				depth++
			case "}":
				if depth > 0 {
					depth--
				}
			}
		}
		p.consume()
	}
}

// skipUntilTopLevelTerminator consumes tokens until reaching `;` or a newline
// at depth 0. Used for `type X = ...;` aliases.
func (p *jsParser) skipUntilTopLevelTerminator() int {
	depth := 0
	lastEnd := p.lastConsumedEnd(0)
	for p.i < len(p.tokens) {
		t := p.peek(0)
		if t == nil {
			return lastEnd
		}
		if t.Kind == TokPunct {
			switch t.Value {
			case "(", "[", "{", "<":
				depth++
			case ")", "]", "}", ">":
				if depth > 0 {
					depth--
				}
			case ">>":
				// See skipReturnType's identical case for the full
				// rationale: the tokenizer glues a nested generic close
				// like `Pick<T, Exclude<K, J>>` into a single ">>" token,
				// not two ">" tokens. This is the function that actually
				// hit the bug in production: `type Except<T, K extends
				// keyof T> = Pick<T, Exclude<keyof T, K>>;` never found
				// its terminating ";" at depth 0, so the type alias's End
				// silently swallowed the rest of the file — confirmed via
				// a real 3,465-line TypeScript file that yielded only 4
				// symbols total because of this exact line.
				if depth >= 2 {
					depth -= 2
				} else {
					depth = 0
				}
			case ">>>":
				if depth >= 3 {
					depth -= 3
				} else {
					depth = 0
				}
			case ";":
				if depth == 0 {
					end := t.End
					p.consume()
					return end
				}
			}
		}
		// ASI: stop before a new top-level declaration at depth 0.
		if depth == 0 && t.Kind == TokKeyword {
			switch t.Value {
			case "export", "import":
				return lastEnd
			}
		}
		lastEnd = t.End
		p.consume()
	}
	return lastEnd
}

// jsExpressionContinuationPunct lists punctuation that, when it's the first
// token on the line after an expression's outermost bracket closes,
// indicates the expression continues onto that line (method chaining,
// binary/ternary operators, etc.). Anything else ends the expression there —
// this is JS's automatic-semicolon-insertion behavior for statements like:
//
//	const f = () => {
//	  ...
//	}
//	other()
//
// where `other()` is its own statement, not part of f's initializer. Without
// this check, skipExpression would swallow `other()` (and everything after
// it) into f's range once depth returned to 0 on the closing `}`, because
// `other` is a plain identifier and startsTopLevelDeclaration only recognizes
// declaration keywords.
var jsExpressionContinuationPunct = map[string]bool{
	".": true, "?.": true, "(": true, "[": true,
	"+": true, "-": true, "*": true, "/": true, "%": true, "**": true,
	"&&": true, "||": true, "??": true, "?": true, ":": true,
	"=>": true, ",": true,
	"=": true, "==": true, "===": true, "!=": true, "!==": true,
	"<": true, ">": true, "<=": true, ">=": true,
	"&": true, "|": true, "^": true, "<<": true, ">>": true, ">>>": true,
	"+=": true, "-=": true, "*=": true, "/=": true,
	"&&=": true, "||=": true, "??=": true,
}

// jsExpressionContinues reports whether next continues the current
// expression across a line break (see jsExpressionContinuationPunct).
func jsExpressionContinues(next *Token) bool {
	if next == nil {
		return false
	}
	if next.Kind == TokPunct {
		return jsExpressionContinuationPunct[next.Value]
	}
	if next.Kind == TokKeyword {
		switch next.Value {
		case "instanceof", "in", "as":
			return true
		}
	}
	return false
}

func startsTopLevelDeclaration(t *Token) bool {
	if t == nil || t.Kind != TokKeyword {
		return false
	}
	switch t.Value {
	case "export", "import", "const", "let", "var", "function", "class", "interface", "type", "enum", "declare", "abstract", "async":
		return true
	}
	return false
}

func (p *jsParser) skipUntilSemicolonAtClassDepth() {
	depth := 0
	for p.i < len(p.tokens) {
		t := p.peek(0)
		if t == nil {
			return
		}
		if t.Kind == TokPunct {
			switch t.Value {
			case ";":
				if depth == 0 {
					p.consume()
					return
				}
			case "}":
				if depth == 0 && p.inClassBody() {
					return
				}
				if depth > 0 {
					depth--
				}
			case "(", "[", "{":
				depth++
			case ")", "]":
				if depth > 0 {
					depth--
				}
			}
		}
		lastEnd := t.End
		p.consume()
		if depth == 0 {
			if next := p.peek(0); classMemberStartToken(next) && strings.Contains(p.src[lastEnd:next.Start], "\n") {
				return
			}
		}
	}
}

func classMemberStartToken(t *Token) bool {
	if t == nil {
		return false
	}
	switch t.Kind {
	case TokIdent, TokString:
		return true
	case TokKeyword:
		switch t.Value {
		case "static", "public", "protected", "private", "readonly", "abstract", "declare", "async", "get", "set":
			return true
		}
	case TokPunct:
		switch t.Value {
		case "@", "#", "[", "*":
			return true
		}
	}
	return false
}

func (p *jsParser) skipTypeAnnotation() {
	if !p.peekPunct(":") {
		return
	}
	p.consume()
	depth := 0
	for p.i < len(p.tokens) {
		t := p.peek(0)
		if t == nil {
			return
		}
		if t.Kind == TokPunct {
			switch t.Value {
			case "(", "[", "<", "{":
				depth++
			case ")", "]", ">", "}":
				if depth == 0 {
					return
				}
				depth--
			case ">>":
				// See skipReturnType's identical case for the full
				// rationale (the tokenizer glues a nested generic close
				// like `Foo<Bar<Baz>>` into one ">>" token, not two ">").
				if depth == 0 {
					return
				}
				if depth >= 2 {
					depth -= 2
				} else {
					depth = 0
				}
			case ">>>":
				if depth == 0 {
					return
				}
				if depth >= 3 {
					depth -= 3
				} else {
					depth = 0
				}
			case "=":
				if depth == 0 {
					return
				}
			case ";", ",":
				if depth == 0 {
					return
				}
			}
		}
		p.consume()
	}
}

// maybeRecordCall examines the cursor for a call site of the form
// `IDENT(...)` or `IDENT.IDENT(...)` (full chain depth) and records it.
// It must NOT be invoked when we are sitting on a declaration keyword we want
// to handle structurally — that's why the run loop tries decl parsing first.
func (p *jsParser) maybeRecordCall() {
	t := p.peek(0)
	if t == nil {
		return
	}
	// `new` followed by an identifier is also a call we record.
	if t.Kind == TokKeyword && t.Value == "new" {
		// Let the next iteration see the constructor identifier as a call.
		return
	}
	if !callIdentToken(t) {
		return
	}
	// Quick reject: keyword starts of declarations / control flow that we
	// already handle elsewhere.
	if t.Kind == TokKeyword {
		switch t.Value {
		case "function", "class", "interface", "type", "enum", "import", "export",
			"if", "for", "while", "switch", "catch", "return", "throw", "typeof",
			"new", "delete", "await", "yield", "in", "of", "instanceof", "void":
			return
		}
	}

	// Build dotted callee: ident (. ident)* — stopping at the first `(`.
	// The member separator may be `.` or the optional-chaining operator `?.`
	// (a single glued TokPunct), so `a?.b.foo()` yields callee "a.b.foo" with
	// the receiver intact — otherwise the receiver segment is lost and the
	// call wrongly takes the bare-name resolution path (dropping import-bound
	// and this/super disambiguation). Optional chaining is pervasive in modern
	// TS/Vue, exactly this engine's corpus.
	startIdx := p.i
	parts := []string{t.Value}
	j := 1
	for {
		next := p.peek(j)
		if next == nil {
			return
		}
		if next.Kind == TokPunct && (next.Value == "." || next.Value == "?.") {
			after := p.peek(j + 1)
			if after == nil {
				return
			}
			if after.Kind != TokIdent && after.Kind != TokKeyword {
				return
			}
			parts = append(parts, after.Value)
			j += 2
			continue
		}
		if next.Kind == TokPunct && next.Value == "(" {
			callee := strings.Join(parts, ".")
			// Look back past comment tokens for the `new` keyword: a comment
			// between `new` and the constructor name must not hide construction.
			constructor := false
			for pi := startIdx - 1; pi >= 0; pi-- {
				pk := p.tokens[pi].Kind
				if pk == TokComment || pk == TokLineComment {
					continue
				}
				constructor = p.tokens[pi].Kind == TokKeyword && p.tokens[pi].Value == "new"
				break
			}
			p.result.Calls = append(p.result.Calls, ScannedCall{
				Callee:      callee,
				Start:       p.tokens[startIdx].Start,
				End:         next.End,
				Constructor: constructor,
			})
			openTokIdx := p.i + j
			if testFrameworkCallNames[callee] {
				p.recordTestCallbacks(callee, openTokIdx)
			} else if p.braceDepth == 0 {
				p.recordTopLevelCallbackSymbols(callee, openTokIdx)
			}
			// Advance the cursor to land on the "(" token (the run loop
			// does one more p.i++ after we return) instead of leaving it on
			// the identifier we started from. Without this, the run loop's
			// normal token-by-token walk would revisit every identifier
			// after a "." in the chain we just recorded (e.g. "execute" in
			// "strategy.execute(") as if it were its own, unrelated,
			// standalone bare call — fabricating a second ScannedCall
			// ("execute(...)") for the same call expression. That phantom
			// entry then resolves independently downstream and can produce
			// a confident but wrong edge whenever the bare suffix happens
			// to collide with an unrelated same-named symbol elsewhere in
			// the file/repo.
			// Chained calls like `promise.then(x).catch(y)` are unaffected:
			// this only skips the tokens already consumed as part of THIS
			// chain, not anything from the matching "(" onward, so the
			// normal walk still discovers ".catch(" as its own fresh call
			// once it reaches it.
			p.i += j - 1
		}
		return
	}
}

// testFrameworkCallNames are calls whose function-expression arguments are
// the recognized "callback" symbols. Calls inside those bodies should be
// attributed to the callback rather than the enclosing file. The set is
// intentionally narrow — we only want the test-author intent of "this is a
// nested execution unit", not arbitrary higher-order functions.
var testFrameworkCallNames = map[string]bool{
	"it":         true,
	"test":       true,
	"describe":   true,
	"context":    true,
	"suite":      true,
	"beforeEach": true,
	"afterEach":  true,
	"beforeAll":  true,
	"afterAll":   true,
	"before":     true,
	"after":      true,
	"specify":    true,
}

// recordTestCallbacks finds function-expression arguments of a recognized
// test-framework call and emits ScannedSymbol entries for them. The symbol
// span covers the body so containingSymbolFast attributes nested calls to
// the callback. Naming uses the leading string literal arg when present
// (e.g. `it("renders", () => ...)` → name "it: renders") so callers/agents
// can disambiguate sibling tests.
func (p *jsParser) recordTestCallbacks(callee string, openParenIdx int) {
	if openParenIdx >= len(p.tokens) {
		return
	}
	closeIdx := p.findMatchingParen(openParenIdx)
	if closeIdx < 0 {
		return
	}
	args := p.argRanges(openParenIdx+1, closeIdx)
	testName := ""
	for argIdx, arg := range args {
		// First argument: prefer a string literal as the test name. We do
		// not emit a callback symbol for it.
		if argIdx == 0 {
			if arg.start <= arg.end {
				first := p.tokens[arg.start]
				if first.Kind == TokString {
					testName = first.Value
				}
			}
		}
		bodyStart, bodyEnd, ok := p.detectFunctionBody(arg.start, arg.end)
		if !ok {
			continue
		}
		name := callee
		if testName != "" {
			name = callee + ": " + testName
		}
		p.result.Symbols = append(p.result.Symbols, ScannedSymbol{
			Kind:      "callback",
			Name:      name,
			Start:     bodyStart,
			End:       bodyEnd,
			NameStart: bodyStart,
			Parent:    "",
		})
	}
}

// recordTopLevelCallbackSymbols emits a synthetic "callback" symbol for each
// function-expression/arrow-function argument with a block body, for a call
// made directly at module top level (braceDepth == 0, the caller's
// condition). Without this, calls made inside such callbacks have no
// enclosing symbol and are silently dropped from the call graph — e.g.
//
//	axiosInstance.interceptors.response.use(res => res, async (error) => {
//	  await useAuth()  // dropped: no enclosing symbol without this fix
//	})
//
// or `items.forEach(item => process(item))` at file scope. Test-framework
// calls (it/describe/...) are handled separately by recordTestCallbacks with
// friendlier "it: <name>"-style naming.
func (p *jsParser) recordTopLevelCallbackSymbols(callee string, openParenIdx int) {
	if openParenIdx >= len(p.tokens) {
		return
	}
	closeIdx := p.findMatchingParen(openParenIdx)
	if closeIdx < 0 {
		return
	}
	args := p.argRanges(openParenIdx+1, closeIdx)
	n := 0
	for _, arg := range args {
		bodyStart, bodyEnd, ok := p.detectFunctionBody(arg.start, arg.end)
		if !ok || bodyEnd-bodyStart < 4 {
			continue
		}
		n++
		name := callee + "(callback)"
		if len(args) > 1 {
			name = fmt.Sprintf("%s(callback #%d)", callee, n)
		}
		p.result.Symbols = append(p.result.Symbols, ScannedSymbol{
			Kind:      "callback",
			Name:      name,
			Start:     bodyStart,
			End:       bodyEnd,
			NameStart: bodyStart,
			Parent:    "",
		})
	}
}

type tokenRange struct{ start, end int } // inclusive

// argRanges returns the token-index ranges of each top-level call argument
// between (exclusive) openIdx and closeIdx. Commas at the call's own depth
// separate args; nested parens/braces/brackets are treated as opaque.
func (p *jsParser) argRanges(start, closeIdx int) []tokenRange {
	var args []tokenRange
	depth := 0
	cur := start
	for i := start; i < closeIdx; i++ {
		tok := p.tokens[i]
		if tok.Kind == TokPunct {
			switch tok.Value {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
			case ",":
				if depth == 0 {
					args = append(args, tokenRange{start: cur, end: i - 1})
					cur = i + 1
				}
			}
		}
	}
	if cur <= closeIdx-1 {
		args = append(args, tokenRange{start: cur, end: closeIdx - 1})
	}
	// Skip trivia at the start of each arg so callers see real tokens.
	for i := range args {
		args[i].start = p.skipTrivia(args[i].start, args[i].end)
	}
	return args
}

func (p *jsParser) skipTrivia(start, end int) int {
	for start <= end {
		k := p.tokens[start].Kind
		if k == TokComment || k == TokLineComment {
			start++
			continue
		}
		break
	}
	return start
}

// detectFunctionBody examines a token range that is a single call argument
// and returns the byte span of the function body if the argument is an
// arrow function or function expression. Concise arrow bodies (without
// braces) are not emitted as callback symbols — they are too small to host
// nested call sites worth attributing.
func (p *jsParser) detectFunctionBody(argStart, argEnd int) (int, int, bool) {
	if argStart > argEnd {
		return 0, 0, false
	}
	i := argStart

	// Optional `async`.
	if i <= argEnd {
		t := p.tokens[i]
		if t.Kind == TokKeyword && t.Value == "async" {
			i++
			i = p.skipTrivia(i, argEnd)
		}
	}
	if i > argEnd {
		return 0, 0, false
	}
	first := p.tokens[i]

	// `function` expression: optional name, then `(...)`, then `{...}`.
	if first.Kind == TokKeyword && first.Value == "function" {
		// Walk to the next `{` at depth 0; treat that as the body open.
		bodyOpen := p.findPunctAtDepth(i+1, argEnd, "{")
		if bodyOpen < 0 {
			return 0, 0, false
		}
		bodyClose := p.findMatchingBrace(bodyOpen)
		if bodyClose < 0 || bodyClose > argEnd {
			return 0, 0, false
		}
		return p.tokens[bodyOpen].Start, p.tokens[bodyClose].End, true
	}

	// Arrow function: either `IDENT => ...` or `(...) => ...`.
	arrowIdx := -1
	if first.Kind == TokIdent || (first.Kind == TokKeyword && contextualIdentKeyword(first.Value)) {
		// `x =>` — single param without parens.
		next := i + 1
		next = p.skipTrivia(next, argEnd)
		if next <= argEnd && p.tokens[next].Kind == TokPunct && p.tokens[next].Value == "=>" {
			arrowIdx = next
		}
	} else if first.Kind == TokPunct && first.Value == "(" {
		closeParen := p.findMatchingParen(i)
		if closeParen < 0 || closeParen > argEnd {
			return 0, 0, false
		}
		next := p.skipTrivia(closeParen+1, argEnd)
		// Optional return-type annotation `(): Foo => body` — skip up to `=>`.
		if next <= argEnd && p.tokens[next].Kind == TokPunct && p.tokens[next].Value == ":" {
			next = p.findArrowAfterReturnType(next+1, argEnd)
		}
		if next > 0 && next <= argEnd && p.tokens[next].Kind == TokPunct && p.tokens[next].Value == "=>" {
			arrowIdx = next
		}
	}
	if arrowIdx < 0 {
		return 0, 0, false
	}
	bodyStart := p.skipTrivia(arrowIdx+1, argEnd)
	if bodyStart > argEnd {
		return 0, 0, false
	}
	body := p.tokens[bodyStart]
	if body.Kind == TokPunct && body.Value == "{" {
		bodyClose := p.findMatchingBrace(bodyStart)
		if bodyClose < 0 || bodyClose > argEnd {
			return 0, 0, false
		}
		return body.Start, p.tokens[bodyClose].End, true
	}
	// Concise arrow body — not large enough to matter for attribution.
	return 0, 0, false
}

// findArrowAfterReturnType walks past a TS-style return-type annotation
// after a parameter list and returns the token index of the `=>`. Returns
// -1 if not found before end. Treats `<…>`, `(…)`, `[…]`, `{…}` as opaque.
func (p *jsParser) findArrowAfterReturnType(start, end int) int {
	depth := 0
	for i := start; i <= end; i++ {
		tok := p.tokens[i]
		if tok.Kind != TokPunct {
			continue
		}
		switch tok.Value {
		case "(", "[", "{", "<":
			depth++
		case ")", "]", "}", ">":
			if depth > 0 {
				depth--
			}
		case ">>":
			// See skipReturnType's identical case for the full rationale
			// (the tokenizer glues a nested generic close like
			// `Foo<Bar<Baz>>` into one ">>" token, not two ">").
			if depth >= 2 {
				depth -= 2
			} else {
				depth = 0
			}
		case ">>>":
			if depth >= 3 {
				depth -= 3
			} else {
				depth = 0
			}
		case "=>":
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// findMatchingParen returns the token index of the `)` that closes the `(`
// at openIdx, or -1 if unmatched. Trivia tokens are ignored.
func (p *jsParser) findMatchingParen(openIdx int) int {
	if openIdx < 0 || openIdx >= len(p.tokens) {
		return -1
	}
	depth := 0
	for i := openIdx; i < len(p.tokens); i++ {
		tok := p.tokens[i]
		if tok.Kind != TokPunct {
			continue
		}
		switch tok.Value {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func (p *jsParser) findMatchingBrace(openIdx int) int {
	if openIdx < 0 || openIdx >= len(p.tokens) {
		return -1
	}
	depth := 0
	for i := openIdx; i < len(p.tokens); i++ {
		tok := p.tokens[i]
		if tok.Kind != TokPunct {
			continue
		}
		switch tok.Value {
		case "{":
			depth++
		case "}":
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func (p *jsParser) findPunctAtDepth(start, end int, target string) int {
	depth := 0
	for i := start; i <= end && i < len(p.tokens); i++ {
		tok := p.tokens[i]
		if tok.Kind != TokPunct {
			continue
		}
		switch tok.Value {
		case "(", "[", "{":
			if tok.Value == target && depth == 0 {
				return i
			}
			depth++
		case ")", "]", "}":
			depth--
		default:
			if tok.Value == target && depth == 0 {
				return i
			}
		}
	}
	return -1
}

func (p *jsParser) recordTemplateCalls(t Token) {
	raw := p.src[t.Start:t.End]
	for _, r := range templateExpressionRanges(raw) {
		expr := raw[r[0]:r[1]]
		res := ParseJS(expr)
		for _, call := range res.Calls {
			p.result.Calls = append(p.result.Calls, ScannedCall{
				Callee: call.Callee,
				Start:  t.Start + r[0] + call.Start,
				End:    t.Start + r[0] + call.End,
			})
		}
	}
}

func (p *jsParser) recordCallsInRange(start, end int) {
	start = clampRangeOffset(start, len(p.src))
	end = clampRangeOffset(end, len(p.src))
	if end <= start {
		return
	}
	res := ParseJS(p.src[start:end])
	for _, call := range res.Calls {
		p.result.Calls = append(p.result.Calls, ScannedCall{
			Callee: call.Callee,
			Start:  start + call.Start,
			End:    start + call.End,
		})
	}
}

func clampRangeOffset(offset, size int) int {
	if offset < 0 {
		return 0
	}
	if offset > size {
		return size
	}
	return offset
}

func templateExpressionRanges(raw string) [][2]int {
	var ranges [][2]int
	for i := 0; i+1 < len(raw); i++ {
		if raw[i] == '\\' {
			i++
			continue
		}
		if raw[i] != '$' || raw[i+1] != '{' {
			continue
		}
		start := i + 2
		depth := 1
		j := start
	expression:
		for j < len(raw) && depth > 0 {
			switch raw[j] {
			case '\'', '"':
				j = skipQuotedJS(raw, j)
				continue
			case '`':
				j = skipTemplateJS(raw, j)
				continue
			case '/':
				if j+1 < len(raw) && raw[j+1] == '/' {
					j += 2
					for j < len(raw) && raw[j] != '\n' {
						j++
					}
					continue
				}
				if j+1 < len(raw) && raw[j+1] == '*' {
					j += 2
					for j+1 < len(raw) && !(raw[j] == '*' && raw[j+1] == '/') {
						j++
					}
					if j+1 < len(raw) {
						j += 2
					}
					continue
				}
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					ranges = append(ranges, [2]int{start, j})
					i = j
					break expression
				}
			}
			j++
		}
	}
	return ranges
}

func skipQuotedJS(raw string, i int) int {
	quote := raw[i]
	i++
	for i < len(raw) {
		if raw[i] == '\\' {
			i += 2
			continue
		}
		if raw[i] == quote {
			return i + 1
		}
		i++
	}
	return i
}

func skipTemplateJS(raw string, i int) int {
	i++
	for i < len(raw) {
		if raw[i] == '\\' {
			i += 2
			continue
		}
		if raw[i] == '`' {
			return i + 1
		}
		i++
	}
	return i
}

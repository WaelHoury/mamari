package mamari

import "strings"

// Token represents a lexical unit in JS/TS source. Token.Start/End are byte
// offsets into the original input. Code tokens have a populated Value when
// they form an identifier; string/template/regex tokens carry their literal
// text in Value (without delimiters where useful).
type Token struct {
	Kind  TokenKind
	Start int
	End   int
	Value string
}

type TokenKind int

const (
	TokIdent TokenKind = iota
	TokKeyword
	TokNumber
	TokString
	TokTemplate
	TokRegex
	TokPunct
	TokComment
	TokLineComment
)

// jsKeywords are reserved words we treat differently from identifiers when
// scanning structural elements like classes, functions, types, etc.
var jsKeywords = map[string]bool{
	"abstract": true, "any": true, "as": true, "async": true, "await": true,
	"boolean": true, "break": true, "case": true, "catch": true, "class": true,
	"const": true, "continue": true, "debugger": true, "declare": true,
	"default": true, "delete": true, "do": true, "else": true, "enum": true,
	"export": true, "extends": true, "false": true, "finally": true, "for": true,
	"from": true, "function": true, "get": true, "if": true, "implements": true,
	"import": true, "in": true, "instanceof": true, "interface": true, "is": true,
	"keyof": true, "let": true, "namespace": true, "new": true, "null": true,
	"number": true, "of": true, "private": true, "protected": true, "public": true,
	"readonly": true, "return": true, "set": true, "static": true, "string": true,
	"super": true, "switch": true, "this": true, "throw": true, "true": true,
	"try": true, "type": true, "typeof": true, "undefined": true, "var": true,
	"void": true, "while": true, "with": true, "yield": true,
}

// TokenizeJS produces a flat token stream for JavaScript or TypeScript source.
// It correctly handles:
//   - // line comments and /* block */ comments (incl. JSDoc)
//   - 'single', "double", and `template` strings (with ${...} interpolation
//     containing arbitrary nested code, recursively)
//   - regex literals disambiguated by preceding-token context
//   - numeric literals (incl. 0x/0b/0o, bigint suffix)
//   - punctuation and identifiers
//
// The tokenizer is deliberately tolerant: malformed input is reported as the
// best-effort token stream up to the error and then EOF — never panics.
func TokenizeJS(src string) []Token {
	t := &jsTokenizer{src: src}
	for !t.eof() {
		t.next()
	}
	return t.tokens
}

type jsTokenizer struct {
	src    string
	i      int
	tokens []Token
}

func (t *jsTokenizer) eof() bool { return t.i >= len(t.src) }

func (t *jsTokenizer) peek(offset int) byte {
	if t.i+offset >= len(t.src) {
		return 0
	}
	return t.src[t.i+offset]
}

func (t *jsTokenizer) emit(kind TokenKind, start, end int, value string) {
	t.tokens = append(t.tokens, Token{Kind: kind, Start: start, End: end, Value: value})
}

func (t *jsTokenizer) lastSignificant() *Token {
	for i := len(t.tokens) - 1; i >= 0; i-- {
		k := t.tokens[i].Kind
		if k == TokComment || k == TokLineComment {
			continue
		}
		return &t.tokens[i]
	}
	return nil
}

// regexAllowed returns true when a `/` at the current position should start a
// regex literal rather than a division operator. We approximate the rule used
// by every JS parser: regex is allowed at the start of input, after most
// punctuation, and after specific keywords. After an identifier, number, ),
// ], }, or template-string value, a `/` is division.
func (t *jsTokenizer) regexAllowed() bool {
	prev := t.lastSignificant()
	if prev == nil {
		return true
	}
	switch prev.Kind {
	case TokIdent, TokNumber, TokString, TokTemplate, TokRegex:
		return false
	case TokKeyword:
		switch prev.Value {
		case "this", "true", "false", "null", "undefined", "super":
			return false
		}
		return true
	case TokPunct:
		// `)`, `]`, `}` close primary expressions and disable regex.
		if prev.End-prev.Start == 1 {
			switch t.src[prev.Start] {
			case ')', ']', '}':
				return false
			}
		}
		return true
	}
	return true
}

func (t *jsTokenizer) next() {
	t.skipWhitespace()
	if t.eof() {
		return
	}
	c := t.src[t.i]
	switch {
	case c == '/' && t.peek(1) == '/':
		t.lineComment()
	case c == '/' && t.peek(1) == '*':
		t.blockComment()
	case c == '/' && t.regexAllowed():
		t.regex()
	case c == '\'' || c == '"':
		t.stringLiteral(c)
	case c == '`':
		t.templateLiteral()
	case isDigit(c) || (c == '.' && isDigit(t.peek(1))):
		t.number()
	case isIdentStart(c):
		t.identOrKeyword()
	case c == '#' && isIdentStart(t.peek(1)):
		// Private class field name like `#foo`. Treat the whole thing as an
		// identifier token so downstream code can recognize private members.
		start := t.i
		t.i++
		for !t.eof() && isIdentPart(t.src[t.i]) {
			t.i++
		}
		t.emit(TokIdent, start, t.i, t.src[start:t.i])
	default:
		t.punct()
	}
}

func (t *jsTokenizer) skipWhitespace() {
	for !t.eof() {
		c := t.src[t.i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			t.i++
			continue
		}
		break
	}
}

func (t *jsTokenizer) lineComment() {
	start := t.i
	t.i += 2
	for !t.eof() && t.src[t.i] != '\n' {
		t.i++
	}
	t.emit(TokLineComment, start, t.i, "")
}

func (t *jsTokenizer) blockComment() {
	start := t.i
	t.i += 2
	for !t.eof() {
		if t.src[t.i] == '*' && t.peek(1) == '/' {
			t.i += 2
			break
		}
		t.i++
	}
	t.emit(TokComment, start, t.i, "")
}

func (t *jsTokenizer) stringLiteral(quote byte) {
	start := t.i
	t.i++
	var b strings.Builder
	for !t.eof() {
		c := t.src[t.i]
		if c == '\\' && t.i+1 < len(t.src) {
			b.WriteByte(t.src[t.i+1])
			t.i += 2
			continue
		}
		if c == quote {
			t.i++
			break
		}
		if c == '\n' {
			// Unterminated string; bail out to avoid runaway tokens.
			break
		}
		b.WriteByte(c)
		t.i++
	}
	t.emit(TokString, start, t.i, b.String())
}

func (t *jsTokenizer) templateLiteral() {
	start := t.i
	t.i++
	var b strings.Builder
	for !t.eof() {
		c := t.src[t.i]
		if c == '\\' && t.i+1 < len(t.src) {
			b.WriteByte(t.src[t.i+1])
			t.i += 2
			continue
		}
		if c == '`' {
			t.i++
			break
		}
		if c == '$' && t.peek(1) == '{' {
			t.skipTemplateInterp(&b)
			continue
		}
		b.WriteByte(c)
		t.i++
	}
	t.emit(TokTemplate, start, t.i, b.String())
}

// skipTemplateInterp consumes a template interpolation `${ ... }` (from the
// `$` through the matching `}`) starting at t.i, appending the raw bytes to b
// when b is non-nil. It correctly skips nested strings, regex literals,
// comments, nested templates, and balanced braces so the matching `}` is
// found even when the interpolation contains a regex whose body holds quotes
// or braces (e.g. `${x.replace(/"/g, '""')}`). Before this, a regex inside an
// interpolation desynced quote tracking and the lexer ran to EOF, dropping
// every token — and therefore every call edge — after it (measured: a single
// such template swallowed ~170 lines of a real 3,200-line Vue component).
func (t *jsTokenizer) skipTemplateInterp(b *strings.Builder) {
	write := func(c byte) {
		if b != nil {
			b.WriteByte(c)
		}
	}
	writeStr := func(s string) {
		if b != nil {
			b.WriteString(s)
		}
	}
	writeStr("${")
	t.i += 2
	depth := 1
	var lastSig byte // last significant byte, for regex-vs-division detection
	for !t.eof() && depth > 0 {
		c := t.src[t.i]
		switch {
		case c == '{':
			depth++
			write(c)
			t.i++
			lastSig = c
		case c == '}':
			depth--
			write(c)
			t.i++
			if depth == 0 {
				return
			}
			lastSig = c
		case c == '\'' || c == '"':
			q := c
			write(c)
			t.i++
			for !t.eof() && t.src[t.i] != q {
				if t.src[t.i] == '\\' && t.i+1 < len(t.src) {
					write(t.src[t.i])
					write(t.src[t.i+1])
					t.i += 2
					continue
				}
				if t.src[t.i] == '\n' {
					break
				}
				write(t.src[t.i])
				t.i++
			}
			if !t.eof() && t.src[t.i] == q {
				write(q)
				t.i++
			}
			lastSig = q
		case c == '`':
			writeStr(t.scanNestedTemplate())
			lastSig = '`'
		case c == '/' && t.peek(1) == '/':
			for !t.eof() && t.src[t.i] != '\n' {
				write(t.src[t.i])
				t.i++
			}
		case c == '/' && t.peek(1) == '*':
			writeStr("/*")
			t.i += 2
			for !t.eof() && !(t.src[t.i] == '*' && t.peek(1) == '/') {
				write(t.src[t.i])
				t.i++
			}
			if !t.eof() {
				writeStr("*/")
				t.i += 2
			}
		case c == '/' && templateInterpRegexAllowed(lastSig):
			write('/')
			t.i++
			inClass := false
			for !t.eof() {
				ch := t.src[t.i]
				if ch == '\\' && t.i+1 < len(t.src) {
					write(ch)
					write(t.src[t.i+1])
					t.i += 2
					continue
				}
				if ch == '[' {
					inClass = true
				} else if ch == ']' {
					inClass = false
				} else if ch == '/' && !inClass {
					write('/')
					t.i++
					break
				} else if ch == '\n' {
					break
				}
				write(ch)
				t.i++
			}
			lastSig = 'x' // a regex literal is a value; division position follows
		default:
			write(c)
			t.i++
			if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				lastSig = c
			}
		}
	}
}

// templateInterpRegexAllowed reports whether a `/` following prev (the last
// significant byte scanned in a template interpolation) begins a regex
// literal rather than division. Mirrors the main tokenizer's regexAllowed
// heuristic at the byte level: a regex can begin an expression (after an
// operator or opening punctuator, or at the start) but not after a value
// (identifier, number, `)`, `]`, quote, backtick, or member `.`).
func templateInterpRegexAllowed(prev byte) bool {
	switch {
	case prev == 0:
		return true
	case (prev >= 'a' && prev <= 'z') || (prev >= 'A' && prev <= 'Z') ||
		(prev >= '0' && prev <= '9') || prev == '_' || prev == '$':
		return false
	case prev == ')' || prev == ']' || prev == '`' || prev == '\'' ||
		prev == '"' || prev == '.':
		return false
	default:
		return true
	}
}

func (t *jsTokenizer) scanNestedTemplate() string {
	if t.eof() || t.src[t.i] != '`' {
		return ""
	}
	start := t.i
	t.i++
	for !t.eof() {
		c := t.src[t.i]
		if c == '\\' && t.i+1 < len(t.src) {
			t.i += 2
			continue
		}
		if c == '`' {
			t.i++
			return t.src[start:t.i]
		}
		if c == '$' && t.peek(1) == '{' {
			t.skipTemplateInterp(nil)
			continue
		}
		t.i++
	}
	return t.src[start:t.i]
}

func (t *jsTokenizer) regex() {
	start := t.i
	t.i++
	inClass := false
	for !t.eof() {
		c := t.src[t.i]
		if c == '\\' && t.i+1 < len(t.src) {
			t.i += 2
			continue
		}
		if c == '[' {
			inClass = true
		} else if c == ']' {
			inClass = false
		} else if c == '/' && !inClass {
			t.i++
			break
		} else if c == '\n' {
			break
		}
		t.i++
	}
	for !t.eof() && isIdentPart(t.src[t.i]) {
		t.i++
	}
	t.emit(TokRegex, start, t.i, t.src[start:t.i])
}

func (t *jsTokenizer) number() {
	start := t.i
	if t.src[t.i] == '0' && t.i+1 < len(t.src) {
		switch t.src[t.i+1] {
		case 'x', 'X', 'b', 'B', 'o', 'O':
			t.i += 2
			for !t.eof() && (isHexDigit(t.src[t.i]) || t.src[t.i] == '_') {
				t.i++
			}
			if !t.eof() && t.src[t.i] == 'n' {
				t.i++
			}
			t.emit(TokNumber, start, t.i, t.src[start:t.i])
			return
		}
	}
	for !t.eof() && (isDigit(t.src[t.i]) || t.src[t.i] == '_') {
		t.i++
	}
	if !t.eof() && t.src[t.i] == '.' {
		t.i++
		for !t.eof() && (isDigit(t.src[t.i]) || t.src[t.i] == '_') {
			t.i++
		}
	}
	if !t.eof() && (t.src[t.i] == 'e' || t.src[t.i] == 'E') {
		t.i++
		if !t.eof() && (t.src[t.i] == '+' || t.src[t.i] == '-') {
			t.i++
		}
		for !t.eof() && isDigit(t.src[t.i]) {
			t.i++
		}
	}
	if !t.eof() && t.src[t.i] == 'n' {
		t.i++
	}
	t.emit(TokNumber, start, t.i, t.src[start:t.i])
}

func (t *jsTokenizer) identOrKeyword() {
	start := t.i
	t.i++
	for !t.eof() && isIdentPart(t.src[t.i]) {
		t.i++
	}
	value := t.src[start:t.i]
	if jsKeywords[value] {
		t.emit(TokKeyword, start, t.i, value)
		return
	}
	t.emit(TokIdent, start, t.i, value)
}

func (t *jsTokenizer) punct() {
	start := t.i
	c := t.src[t.i]
	t.i++
	// Multi-char operators we want to keep glued so they don't accidentally
	// re-enable regex parsing. The set is conservative; extra unrolling is
	// fine because we only look at this for `regexAllowed`.
	switch c {
	case '=', '!', '<', '>':
		if !t.eof() && t.src[t.i] == '=' {
			t.i++
			if !t.eof() && t.src[t.i] == '=' {
				t.i++
			}
		} else if c == '=' && !t.eof() && t.src[t.i] == '>' {
			t.i++
		} else if (c == '<' || c == '>') && !t.eof() && t.src[t.i] == c {
			t.i++
		}
	case '&', '|':
		if !t.eof() && t.src[t.i] == c {
			t.i++
		}
	case '+', '-':
		if !t.eof() && (t.src[t.i] == c || t.src[t.i] == '=') {
			t.i++
		}
	case '?':
		if !t.eof() && (t.src[t.i] == '.' || t.src[t.i] == '?') {
			t.i++
		}
	case '*':
		if !t.eof() && t.src[t.i] == '*' {
			t.i++
		}
	}
	t.emit(TokPunct, start, t.i, t.src[start:t.i])
}

func isHexDigit(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c == '$'
}
func isIdentPart(c byte) bool { return isIdentStart(c) || isDigit(c) }

// MaskStringsAndComments returns a copy of src where every string, template,
// regex, line comment, and block comment is replaced with ASCII spaces of
// equal length (newlines preserved). The output has the same byte length and
// line layout as the input, so any line/column derived from offsets in the
// masked string maps 1:1 to the original. This is the primary defense against
// regex scanners (e.g. call detection) treating prose or docstrings as code.
func MaskStringsAndComments(src string) string {
	if src == "" {
		return src
	}
	tokens := TokenizeJS(src)
	buf := []byte(src)
	for _, tok := range tokens {
		switch tok.Kind {
		case TokString, TokTemplate, TokRegex, TokComment, TokLineComment:
			for i := tok.Start; i < tok.End && i < len(buf); i++ {
				if buf[i] != '\n' {
					buf[i] = ' '
				}
			}
		}
	}
	return string(buf)
}

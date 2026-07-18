package treesitter

import (
	"regexp"
	"strings"
)

// cppThreadSafetyAnnotations names Clang's documented thread-safety-analysis
// attribute macros (https://clang.llvm.org/docs/ThreadSafetyAnalysis.html,
// conventionally defined in a project's own thread_annotations.h and used
// identically by every project that adopts the convention — Abseil,
// Chromium, gRPC, LevelDB among them). They're real, valid, extremely
// common modern C++ — but since tree-sitter never runs the C preprocessor,
// it sees the literal macro-call token (e.g. `GUARDED_BY(mu)`) sitting in a
// declarator position no C++ grammar production expects, producing a parse
// error for the entire surrounding declaration. This is a fixed, finite,
// well-known list (a de facto standard, not project-specific), unlike
// export-macro names below.
var cppThreadSafetyAnnotations = []string{
	"GUARDED_BY", "PT_GUARDED_BY", "ACQUIRED_AFTER", "ACQUIRED_BEFORE",
	"EXCLUSIVE_LOCKS_REQUIRED", "SHARED_LOCKS_REQUIRED", "LOCKS_EXCLUDED",
	"LOCK_RETURNED", "LOCKABLE", "SCOPED_LOCKABLE",
	"EXCLUSIVE_LOCK_FUNCTION", "SHARED_LOCK_FUNCTION",
	"ASSERT_EXCLUSIVE_LOCK", "ASSERT_SHARED_LOCK",
	"EXCLUSIVE_TRYLOCK_FUNCTION", "SHARED_TRYLOCK_FUNCTION",
	"UNLOCK_FUNCTION", "NO_THREAD_SAFETY_ANALYSIS",
}

var cppThreadSafetyPattern = regexp.MustCompile(`\b(` + strings.Join(cppThreadSafetyAnnotations, "|") + `)\b`)

// cppExportMacroPattern matches library export/visibility macros by their
// near-universal real-world naming convention (`LEVELDB_EXPORT`,
// `ABSL_EXPORT`, `MYLIB_API`, ...) rather than a blanket "any all-caps
// identifier" rule — deliberately narrower than that to avoid stripping
// legitimate all-caps typedefs real C++ code does use (Windows API's
// DWORD/HANDLE/LPCWSTR among them), which don't end in one of these
// suffixes.
var cppExportMacroPattern = regexp.MustCompile(`\b[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)*_(?:EXPORT|API|PUBLIC|VISIBLE)\b`)

// stripCppMacroAnnotations returns a same-length copy of content with known
// Clang thread-safety annotations and library export/visibility macros
// blanked out (replaced with spaces, newlines preserved) so tree-sitter-cpp
// can parse the surrounding declaration. Byte length and line numbers are
// always preserved — callers needing the real source text (signatures,
// docstrings) read it from the original, unmodified content independently;
// this transformed copy exists only to feed the parser.
//
// Found via real-world verification: indexing `google/leveldb` flagged
// 33 of 134 files (24.6%) as parse failures, almost entirely from these two
// macro shapes — not from anything exotic. A pointer-to-member-function
// declaration (`void (Benchmark::*method)(ThreadState*)`) and a couple of
// test-only macro DSLs (GoogleMock's `MATCHER(...)  {...}`, `<cinttypes>`
// format macros used in adjacent string-literal concatenation) account for
// the remainder and are deliberately not handled here — genuinely different,
// narrower problems that don't share this fix's safe, well-known-name-based
// detection strategy.
func stripCppMacroAnnotations(content []byte) []byte {
	out := make([]byte, len(content))
	copy(out, content)

	for _, loc := range cppThreadSafetyPattern.FindAllIndex(content, -1) {
		if isOnPreprocessorLine(content, loc[0]) {
			continue
		}
		blankRange(out, loc[0], extendOverParenArgs(content, loc[1]))
	}

	for _, loc := range cppExportMacroPattern.FindAllIndex(content, -1) {
		if isOnPreprocessorLine(content, loc[0]) {
			continue
		}
		if precedesIdentifier(content, loc[1]) {
			blankRange(out, loc[0], loc[1])
		}
	}

	return out
}

func blankRange(buf []byte, start, end int) {
	for i := start; i < end && i < len(buf); i++ {
		if buf[i] != '\n' {
			buf[i] = ' '
		}
	}
}

// extendOverParenArgs returns the end of a `(...)` argument list
// immediately following position end (skipping only spaces/tabs first), or
// end unchanged if no such argument list follows — covers both
// `GUARDED_BY(mu)` and the bare (default-constructed) `LOCKABLE` shape.
func extendOverParenArgs(buf []byte, end int) int {
	i := end
	for i < len(buf) && (buf[i] == ' ' || buf[i] == '\t') {
		i++
	}
	if i >= len(buf) || buf[i] != '(' {
		return end
	}
	depth := 0
	for ; i < len(buf); i++ {
		switch buf[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return len(buf)
}

// isOnPreprocessorLine reports whether pos lies on a line whose first
// non-whitespace character is `#` — i.e. a preprocessor directive line.
// These macro names are only ever meaningfully *used* (as an annotation
// to strip) in ordinary C++ declarator positions; the only place they
// legitimately appear on a `#`-line is their own `#define`/`#ifdef`/
// `#ifndef`/`#undef`/`#if defined(...)` declaration — found via a real
// regression while testing this fix: blanking the macro name out of its
// own `#define NAME(args) body` line corrupts the directive itself
// (shifting `body` into the name position), breaking files that
// previously parsed fine. Skipping every preprocessor-directive line
// uniformly avoids this without needing to enumerate every directive
// keyword that can precede the name.
func isOnPreprocessorLine(buf []byte, pos int) bool {
	lineStart := pos
	for lineStart > 0 && buf[lineStart-1] != '\n' {
		lineStart--
	}
	i := lineStart
	for i < len(buf) && (buf[i] == ' ' || buf[i] == '\t') {
		i++
	}
	return i < len(buf) && buf[i] == '#'
}

// precedesIdentifier reports whether pos, after skipping spaces/tabs, is
// immediately followed by an identifier-start character — the signal that
// distinguishes `LEVELDB_EXPORT Status DestroyDB(...)`/`class LEVELDB_EXPORT
// Name` (an export macro directly preceding the real declarator) from any
// other use of a same-shaped name (end of file, followed by punctuation,
// etc.), where blanking would be wrong.
func precedesIdentifier(buf []byte, pos int) bool {
	i := pos
	for i < len(buf) && (buf[i] == ' ' || buf[i] == '\t') {
		i++
	}
	if i >= len(buf) {
		return false
	}
	b := buf[i]
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

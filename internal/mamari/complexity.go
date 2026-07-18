package mamari

import (
	"regexp"
	"strings"
)

// complexityDecisionRe matches the branch/loop/boolean-operator tokens counted
// toward a symbol's approximate cyclomatic complexity. It intentionally omits
// the ternary "?" operator and optional-chaining "?." since they are
// indistinguishable from TypeScript optional-parameter/property syntax without
// a full parser, and a conservative (slightly low) score is preferable to a
// noisy one.
var complexityDecisionRe = regexp.MustCompile(`\b(?:if|elif|else if|for|while|case|catch|except)\b|&&|\|\||\?\?`)

// isComplexityKind reports whether sym.Kind is a symbol kind that
// annotateComplexity computes a cyclomatic-complexity score for. Other kinds
// (classes, imports, TTL terms, etc.) keep Complexity at its zero value.
func isComplexityKind(kind string) bool {
	switch kind {
	case "function", "method", "callback", "getter", "setter":
		return true
	default:
		return false
	}
}

// complexityForRange returns an approximate cyclomatic-complexity score for
// the 1-indexed inclusive line range [start, end] of lines. The score starts
// at 1 (a single straight-line path) and gains one for every decision point
// matched by complexityDecisionRe.
func complexityForRange(lines []string, start, end int) int {
	score := 1
	if start < 1 {
		start = 1
	}
	if end > len(lines) {
		end = len(lines)
	}
	for i := start - 1; i < end; i++ {
		if i < 0 || i >= len(lines) {
			continue
		}
		score += len(complexityDecisionRe.FindAllString(lines[i], -1))
	}
	return score
}

// annotateComplexityLines computes and stores an approximate
// cyclomatic-complexity score on every function/method/callback/getter/setter
// symbol in file. It
// relies on idx.symbolsByFile (via ensureFileSymbolIndex) for the symbol list,
// so it must be called after that index has been built for the current pass.
// maskedSourceLines masks strings/comments (python-aware) and splits into
// lines — the shared preprocessing for the three Phase 5.5 annotators
// (complexity, hot-path, shape hash). Computing it once and passing the
// []string into the annotate*Lines variants avoids masking + splitting the
// same file three times per index build (a measurable indexing cost).
func maskedSourceLines(idx *Index, file, content string) []string {
	var masked string
	if idx.languageFor(file) == "python" {
		masked = maskPythonStringsAndComments(content)
	} else {
		masked = MaskStringsAndComments(content)
	}
	return strings.Split(masked, "\n")
}

func annotateComplexityLines(idx *Index, file string, lines []string) {
	idx.ensureFileSymbolIndex()

	idx.mu.Lock()
	syms := append([]CGPSymbol(nil), idx.symbolsByFile[file]...)
	idx.mu.Unlock()
	if len(syms) == 0 {
		return
	}

	for _, sym := range syms {
		if !isComplexityKind(sym.Kind) {
			continue
		}
		score := complexityForRange(lines, sym.StartLine, sym.EndLine)

		idx.mu.Lock()
		if existing, ok := idx.Symbols[sym.ID]; ok {
			existing.Complexity = score
			idx.Symbols[sym.ID] = existing
		}
		idx.mu.Unlock()
	}
}

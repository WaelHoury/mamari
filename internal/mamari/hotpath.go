package mamari

import (
	"regexp"
	"strings"
)

// hotPathLoopKeywordRe and friends are masked-text heuristics, the same
// philosophy as complexityDecisionRe in complexity.go: a conservative,
// slightly-approximate signal computed uniformly across every language
// Mamari indexes (tree-sitter-backed or not) is preferable to an exact
// per-language AST walk that would only cover the four tree-sitter
// grammars and leave JS/TS/heuristic-fallback languages with nothing.
var (
	hotPathLoopKeywordRe = regexp.MustCompile(`\b(for|while)\b`)
	hotPathPyLoopStartRe = regexp.MustCompile(`^(for|while)\b`)
	hotPathLinearScanRe  = regexp.MustCompile(`\.(find|indexOf|includes|contains|index)\s*\(`)
	hotPathAllocRe       = regexp.MustCompile(`\bnew\s+[A-Za-z_]|\.(append|push|add)\s*\(`)
)

// maxTransitiveLoopDepthHops bounds propagateTransitiveLoopDepth's relaxation
// passes, so a deep or cyclic call graph can't make index builds unbounded.
const maxTransitiveLoopDepthHops = 6

// hotPathMetrics is annotateHotPathSignalsLines' per-symbol result, mirrored onto
// the matching CGPSymbol fields (see types.go).
type hotPathMetrics struct {
	LoopDepth        int
	LinearScanInLoop int
	AllocInLoop      int
	RecursionInLoop  bool
}

// annotateHotPathSignalsLines computes loop-nesting and loop-body hot-path
// signals for every function/method/callback/getter/setter symbol in file,
// alongside annotateComplexity's cyclomatic score. Must run after the file
// symbol index is available (same precondition as annotateComplexity); the
// two are typically called back to back.
func annotateHotPathSignalsLines(idx *Index, file string, lines []string) {
	idx.ensureFileSymbolIndex()

	idx.mu.Lock()
	syms := append([]CGPSymbol(nil), idx.symbolsByFile[file]...)
	idx.mu.Unlock()
	if len(syms) == 0 {
		return
	}

	lang := idx.languageFor(file)
	python := lang == "python"
	ruby := lang == "ruby"

	for _, sym := range syms {
		if !isComplexityKind(sym.Kind) {
			continue
		}
		var metrics hotPathMetrics
		switch {
		case python:
			metrics = hotPathForRangePython(lines, sym.StartLine, sym.EndLine, sym.Name)
		case ruby:
			metrics = hotPathForRangeRuby(lines, sym.StartLine, sym.EndLine, sym.Name)
		default:
			metrics = hotPathForRangeBraces(lines, sym.StartLine, sym.EndLine, sym.Name)
		}

		idx.mu.Lock()
		if existing, ok := idx.Symbols[sym.ID]; ok {
			existing.LoopDepth = metrics.LoopDepth
			existing.LinearScanInLoop = metrics.LinearScanInLoop
			existing.AllocInLoop = metrics.AllocInLoop
			existing.RecursionInLoop = metrics.RecursionInLoop
			idx.Symbols[sym.ID] = existing
		}
		idx.mu.Unlock()
	}
}

// hotPathForRangeBraces scans a brace-delimited language's masked source
// for the 1-indexed inclusive line range [start, end], tracking brace
// nesting to find loop bodies (any `{` immediately following a line that
// mentions `for`/`while`) and counting linear-scan/alloc/self-call evidence
// found while inside one. It is a text heuristic, not a parser: a loop
// keyword and its opening brace separated by an intervening unrelated `{`
// (e.g. an object literal in a `for` condition) will under-count rather
// than over-count, matching complexityForRange's existing conservative bias.
func hotPathForRangeBraces(lines []string, start, end int, symbolName string) hotPathMetrics {
	var m hotPathMetrics
	if start < 1 {
		start = 1
	}
	if end > len(lines) {
		end = len(lines)
	}
	// Compiled lazily on the first loop-body line only: most symbols have no
	// loop at all, so this per-symbol regex was pure waste when eagerly built.
	var selfCallRe *regexp.Regexp
	selfCallReCompiled := false

	depth := 0
	loopDepth := 0
	loopFrame := map[int]bool{}
	pendingLoop := false

	for i := start - 1; i < end; i++ {
		if i < 0 || i >= len(lines) {
			continue
		}
		line := lines[i]
		if hotPathLoopKeywordRe.MatchString(line) {
			pendingLoop = true
		}
		if loopDepth > 0 {
			m.LinearScanInLoop += len(hotPathLinearScanRe.FindAllString(line, -1))
			m.AllocInLoop += len(hotPathAllocRe.FindAllString(line, -1))
			if symbolName != "" {
				if !selfCallReCompiled {
					selfCallRe = regexp.MustCompile(`\b` + regexp.QuoteMeta(symbolName) + `\s*\(`)
					selfCallReCompiled = true
				}
				if selfCallRe.MatchString(line) {
					m.RecursionInLoop = true
				}
			}
		}
		for _, ch := range line {
			switch ch {
			case '{':
				depth++
				if pendingLoop {
					loopFrame[depth] = true
					loopDepth++
					if loopDepth > m.LoopDepth {
						m.LoopDepth = loopDepth
					}
					pendingLoop = false
				}
			case '}':
				if loopFrame[depth] {
					loopDepth--
					delete(loopFrame, depth)
				}
				if depth > 0 {
					depth--
				}
			}
		}
		if pendingLoop && !strings.Contains(line, "{") {
			// A brace-less one-statement loop body (or the brace is on a
			// later line) — drop the pending flag so it can't leak onto an
			// unrelated brace several lines down.
			pendingLoop = false
		}
	}
	return m
}

// hotPathForRangeRuby is hotPathForRangeBraces' "end"-keyword-based
// counterpart, found necessary by a precision audit: Ruby has no braces at
// all for control-flow blocks (`while`/`for`/iterator `do...end` blocks all
// close with a bare `end`), so the brace-counting version's loopDepth could
// never advance past zero — every Ruby file's hot-path signals were
// silently always zero, regardless of how deeply nested its real loops
// were. Reuses heuristic.go's rubyOpenRe/rubyEndRe for "end"-matched depth
// tracking (rubyOpenRe already treats a block-opening `do`/`do |x|` as
// requiring a matching `end`, the same construct every iterator method
// like `.each`/`.map` uses) and additionally treats literal `while`/
// `until`/`for` lines as loop starts. Non-loop block openers
// (`class`/`module`/`def`/`if`/`case`/`begin`) still consume a depth level
// so nested loops inside them are tracked at the right depth, but are not
// themselves counted as loops.
var hotPathRubyLoopStartRe = regexp.MustCompile(`^(?:for|while|until)\b|\bdo(?:\s*\|[^|]*\|)?\s*$`)

func hotPathForRangeRuby(lines []string, start, end int, symbolName string) hotPathMetrics {
	var m hotPathMetrics
	if start < 1 {
		start = 1
	}
	if end > len(lines) {
		end = len(lines)
	}
	// Compiled lazily on the first loop-body line only: most symbols have no
	// loop at all, so this per-symbol regex was pure waste when eagerly built.
	var selfCallRe *regexp.Regexp
	selfCallReCompiled := false

	depth := 0
	loopDepth := 0
	loopFrame := map[int]bool{}

	for i := start - 1; i < end; i++ {
		if i < 0 || i >= len(lines) {
			continue
		}
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if loopDepth > 0 {
			m.LinearScanInLoop += len(hotPathLinearScanRe.FindAllString(line, -1))
			m.AllocInLoop += len(hotPathAllocRe.FindAllString(line, -1))
			if symbolName != "" {
				if !selfCallReCompiled {
					selfCallRe = regexp.MustCompile(`\b` + regexp.QuoteMeta(symbolName) + `\s*\(`)
					selfCallReCompiled = true
				}
				if selfCallRe.MatchString(line) {
					m.RecursionInLoop = true
				}
			}
		}

		switch {
		case rubyEndRe.MatchString(trimmed):
			if depth > 0 {
				if loopFrame[depth] {
					loopDepth--
					delete(loopFrame, depth)
				}
				depth--
			}
		case rubyOpenRe.MatchString(trimmed):
			depth++
			if hotPathRubyLoopStartRe.MatchString(trimmed) {
				loopFrame[depth] = true
				loopDepth++
				if loopDepth > m.LoopDepth {
					m.LoopDepth = loopDepth
				}
			}
		}
	}
	return m
}

// hotPathForRangePython is hotPathForRangeBraces' indentation-based
// counterpart: a loop opens at a `for`/`while` line's indentation column and
// closes at the next line indented at or below that column.
func hotPathForRangePython(lines []string, start, end int, symbolName string) hotPathMetrics {
	var m hotPathMetrics
	if start < 1 {
		start = 1
	}
	if end > len(lines) {
		end = len(lines)
	}
	// Compiled lazily on the first loop-body line only: most symbols have no
	// loop at all, so this per-symbol regex was pure waste when eagerly built.
	var selfCallRe *regexp.Regexp
	selfCallReCompiled := false

	var loopIndents []int
	for i := start - 1; i < end; i++ {
		if i < 0 || i >= len(lines) {
			continue
		}
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))

		for len(loopIndents) > 0 && indent <= loopIndents[len(loopIndents)-1] {
			loopIndents = loopIndents[:len(loopIndents)-1]
		}

		if len(loopIndents) > 0 {
			m.LinearScanInLoop += len(hotPathLinearScanRe.FindAllString(line, -1))
			m.AllocInLoop += len(hotPathAllocRe.FindAllString(line, -1))
			if symbolName != "" {
				if !selfCallReCompiled {
					selfCallRe = regexp.MustCompile(`\b` + regexp.QuoteMeta(symbolName) + `\s*\(`)
					selfCallReCompiled = true
				}
				if selfCallRe.MatchString(line) {
					m.RecursionInLoop = true
				}
			}
		}

		if hotPathPyLoopStartRe.MatchString(trimmed) {
			loopIndents = append(loopIndents, indent)
			if len(loopIndents) > m.LoopDepth {
				m.LoopDepth = len(loopIndents)
			}
		}
	}
	return m
}

// propagateTransitiveLoopDepth computes, for every symbol, the maximum
// LoopDepth reachable by following "calls" edges outward, capped at
// maxTransitiveLoopDepthHops relaxation passes — a hint that a
// shallow-looking function may still sit on a hot path because something it
// calls (possibly several hops away) loops deeply. Must run after CGP edges
// are built (ScanCGPRelations) and after annotateHotPathSignalsLines has set each
// symbol's own LoopDepth.
//
// Uses bounded Bellman-Ford-style relaxation instead of per-symbol recursive
// DFS: O(hops * edges), naturally bounded and cycle-safe (a cycle simply
// stops changing once every member shares the cycle's max depth), versus a
// recursive walk that would need its own per-root cycle/memo bookkeeping to
// avoid blowing up on highly recursive or highly-connected call graphs.
func propagateTransitiveLoopDepth(idx *Index) {
	idx.mu.Lock()
	type callEdge struct{ from, to string }
	calls := make([]callEdge, 0, len(idx.SymbolEdges))
	for _, e := range idx.SymbolEdges {
		if e.Type == "calls" {
			calls = append(calls, callEdge{e.From, e.To})
		}
	}
	depth := make(map[string]int, len(idx.Symbols))
	for id, sym := range idx.Symbols {
		depth[id] = sym.LoopDepth
	}
	idx.mu.Unlock()

	for hop := 0; hop < maxTransitiveLoopDepthHops; hop++ {
		changed := false
		for _, e := range calls {
			if depth[e.to] > depth[e.from] {
				depth[e.from] = depth[e.to]
				changed = true
			}
		}
		if !changed {
			break
		}
	}

	idx.mu.Lock()
	for id, v := range depth {
		if sym, ok := idx.Symbols[id]; ok && sym.TransitiveLoopDepth != v {
			sym.TransitiveLoopDepth = v
			idx.Symbols[id] = sym
		}
	}
	idx.mu.Unlock()
}

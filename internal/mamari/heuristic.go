package mamari

import (
	"regexp"
	"strings"
)

// heuristicLanguages lists languages mamari does not parse structurally but
// still scans with emitHeuristicSymbols/emitHeuristicImports — a lightweight,
// regex/brace-based "skeleton" extractor covering top-level
// function/class/interface/struct declarations and import lines. Every
// symbol and edge produced this way is tagged ConfHeuristic; combined with
// the file's Parser ("heuristic-fallback"), callers can identify and
// down-weight this data relative to mamari's structural parsers.
var heuristicLanguages = []string{}

// maxHeuristicBodyLines bounds how far emitHeuristicSymbols scans past a
// declaration line when looking for the end of its body, keeping indexing
// fast even on files with unbalanced braces.
const maxHeuristicBodyLines = 5000

// heuristicDecl is one top-level-declaration pattern for a language. re must
// have exactly one capturing group: the declared name.
type heuristicDecl struct {
	re   *regexp.Regexp
	kind string
}

// heuristicLangSpec describes how to skeleton-scan one fallback language.
type heuristicLangSpec struct {
	decls   []heuristicDecl
	imports []*regexp.Regexp // exactly one capturing group: the import spec
	// rubyStyle selects "end"-keyword body matching instead of brace counting.
	rubyStyle bool
}

// heuristicSpecs is now empty: every language mamari recognizes has a real
// structural parser (tree-sitter, the bespoke JS/TS parser, or the generic
// tree-sitter engine). The map, emitHeuristicSymbols/emitHeuristicImports,
// and the heuristicDecl/heuristicLangSpec types stay as the fallback
// mechanism for a genuinely new, not-yet-integrated language landing in
// cgp.go's `default:` case — not dead code, just currently unexercised.
var heuristicSpecs = map[string]heuristicLangSpec{}

// rubyOpenRe matches Ruby lines that open a nested block requiring a matching
// "end" (besides the class/module/def line itself, which is counted as the
// opening block by rubyBodyEnd's initial depth of 1).
var rubyOpenRe = regexp.MustCompile(`^(?:class|module|def)\b|^(?:if|unless|while|until|case|begin|for)\b|\bdo(?:\s*\|[^|]*\|)?\s*$`)
var rubyEndRe = regexp.MustCompile(`^end\b`)

// emitHeuristicSymbols scans content for top-level declarations matching
// language's heuristicLangSpec and registers each as a CGPSymbol with
// Confidence: ConfHeuristic. Languages with no spec are left untouched (no
// symbols beyond the file symbol added in ScanCGPSymbols).
func emitHeuristicSymbols(idx *Index, file, language, content, fileSymID string) {
	spec, ok := heuristicSpecs[language]
	if !ok || len(spec.decls) == 0 {
		return
	}
	lines := strings.Split(content, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if line == "" || (line[0] == ' ' || line[0] == '\t') {
			continue // top-level declarations only: no leading whitespace
		}
		for _, decl := range spec.decls {
			m := decl.re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			name := m[1]
			startLine := i + 1
			var endLine int
			if spec.rubyStyle {
				endLine = rubyBodyEnd(lines, startLine)
			} else {
				endLine = braceBodyEnd(lines, startLine)
			}
			id := stableSymbolID(language, decl.kind, file, name, idx)
			idx.AddCGPSymbol(CGPSymbol{
				ID:          id,
				Name:        name,
				Kind:        decl.kind,
				Language:    language,
				File:        file,
				StartLine:   startLine,
				StartColumn: 1,
				EndLine:     endLine,
				EndColumn:   len(rawLineString(lines, endLine)) + 1,
				Signature:   strings.TrimSpace(line),
				Exported:    true,
				ParentID:    fileSymID,
				Confidence:  ConfHeuristic,
			})
			break // first matching pattern wins
		}
	}
}

// emitHeuristicImports scans content for import/use/require-style lines
// matching language's heuristicLangSpec and adds an "imports" edge from the
// file symbol to "module:<spec>", with Confidence: ConfHeuristic.
func emitHeuristicImports(idx *Index, file, language, content string) {
	spec, ok := heuristicSpecs[language]
	if !ok || len(spec.imports) == 0 {
		return
	}
	starts := lineStarts(content)
	lines := strings.Split(content, "\n")
	fromID := fileSymbolID(file)
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		for _, re := range spec.imports {
			m := re.FindStringSubmatch(trimmed)
			if m == nil {
				continue
			}
			specName := strings.TrimSpace(m[1])
			if specName == "" {
				continue
			}
			lineNo := i + 1
			col := strings.Index(line, m[1]) + 1
			if col < 1 {
				col = 1
			}
			_ = starts
			idx.AddCGPEdge(fromID, "module:"+specName, "imports", ConfHeuristic, Location{
				File: file, StartLine: lineNo, StartColumn: col, EndLine: lineNo, EndColumn: col + len(m[1]), Kind: "import", Raw: specName,
			})
			break
		}
	}
}

// braceBodyEnd returns the 1-based line on which the brace opened at or after
// startLine first balances back to zero, counted naively (no string/comment
// masking — adequate for heuristic confidence). If no closing brace is found
// within maxHeuristicBodyLines, the scan limit is returned; if no opening
// brace is found at all, startLine is returned (single-line declaration).
func braceBodyEnd(lines []string, startLine int) int {
	limit := len(lines)
	if limit > startLine+maxHeuristicBodyLines {
		limit = startLine + maxHeuristicBodyLines
	}
	depth := 0
	opened := false
	for l := startLine; l <= limit; l++ {
		for _, ch := range lines[l-1] {
			switch ch {
			case '{':
				depth++
				opened = true
			case '}':
				depth--
				if opened && depth <= 0 {
					return l
				}
			}
		}
	}
	if !opened {
		return startLine
	}
	return limit
}

// rubyBodyEnd returns the 1-based line of the "end" keyword that closes the
// block opened at startLine, tracking nested class/module/def/do/if-style
// openers. If no matching "end" is found within maxHeuristicBodyLines, the
// scan limit is returned.
func rubyBodyEnd(lines []string, startLine int) int {
	limit := len(lines)
	if limit > startLine+maxHeuristicBodyLines {
		limit = startLine + maxHeuristicBodyLines
	}
	depth := 1
	for l := startLine + 1; l <= limit; l++ {
		trimmed := strings.TrimSpace(lines[l-1])
		switch {
		case rubyEndRe.MatchString(trimmed):
			depth--
			if depth == 0 {
				return l
			}
		case rubyOpenRe.MatchString(trimmed):
			depth++
		}
	}
	return limit
}

// rawLineString returns line (1-based) from lines with no trailing newline,
// or "" if out of range.
func rawLineString(lines []string, line int) string {
	if line < 1 || line > len(lines) {
		return ""
	}
	return strings.TrimRight(lines[line-1], "\r")
}

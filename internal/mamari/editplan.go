package mamari

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// identifierRe matches a single identifier token valid across mamari's
// supported languages (allows JS's leading '$').
var identifierRe = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

// maxRenameDefSearchLines bounds how many lines past a symbol's StartLine are
// scanned for its name when locating the definition-site edit (handles
// multi-line signatures and leading decorators without unbounded scans).
const maxRenameDefSearchLines = 20

// identChar reports whether b can appear inside an identifier across
// mamari's supported languages (letters, digits, underscore, and JS's '$').
func identChar(b byte) bool {
	return b == '_' || b == '$' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// firstIdentifierIndex returns the 0-based byte offset of the first
// whole-word occurrence of name in s, or -1 if none.
func firstIdentifierIndex(s, name string) int {
	for i := 0; i+len(name) <= len(s); i++ {
		if s[i:i+len(name)] != name {
			continue
		}
		if i > 0 && identChar(s[i-1]) {
			continue
		}
		if i+len(name) < len(s) && identChar(s[i+len(name)]) {
			continue
		}
		return i
	}
	return -1
}

// lastIdentifierIndex returns the 0-based byte offset of the last whole-word
// occurrence of name in s, or -1 if none. Used to locate "method" inside a
// call's raw text such as "receiver.method".
func lastIdentifierIndex(s, name string) int {
	for i := len(s) - len(name); i >= 0; i-- {
		if s[i:i+len(name)] != name {
			continue
		}
		if i > 0 && identChar(s[i-1]) {
			continue
		}
		if i+len(name) < len(s) && identChar(s[i+len(name)]) {
			continue
		}
		return i
	}
	return -1
}

// readSourceLines reads file (relative to the indexed repo root), splitting
// it into lines that retain their trailing newline so joined ranges
// reproduce the original bytes exactly.
func readSourceLines(idx *Index, file string) ([]string, error) {
	idx.mu.Lock()
	root := idx.Repo.Root
	idx.mu.Unlock()
	data, err := readRepoFile(root, file)
	if err != nil {
		return nil, err
	}
	return strings.SplitAfter(string(data), "\n"), nil
}

// findNameInRange scans lines startLine..endLine (capped at
// maxRenameDefSearchLines) of file for the first whole-word occurrence of
// name, returning its 1-based line and column.
func findNameInRange(idx *Index, file, name string, startLine, endLine int) (line, col int, ok bool) {
	lines, err := readSourceLines(idx, file)
	if err != nil {
		return 0, 0, false
	}
	limit := endLine
	if limit > startLine+maxRenameDefSearchLines {
		limit = startLine + maxRenameDefSearchLines
	}
	if limit > len(lines) {
		limit = len(lines)
	}
	for l := startLine; l <= limit; l++ {
		text := rawLine(lines, l)
		if i := firstIdentifierIndex(text, name); i >= 0 {
			return l, i + 1, true
		}
	}
	return 0, 0, false
}

// RenameSymbol produces an edit plan that renames every occurrence of a
// symbol's name: its definition and every call/reference site that resolves
// to it. It does not modify any files — callers review and apply Edits
// themselves.
//
// Edits derived from edges with confidence weaker than ConfScoped are
// included but flagged via Warnings, since heuristic/unresolved resolution
// can occasionally point at a same-named symbol elsewhere.
func RenameSymbol(idx *Index, query, newName string) (EditPlanResponse, error) {
	resp := EditPlanResponse{Status: "not_found", Operation: "rename_symbol", Query: query, Edits: []FileEdit{}}
	if !identifierRe.MatchString(newName) {
		return resp, fmt.Errorf("newName %q is not a valid identifier", newName)
	}
	matches := findSymbols(idx, query)
	if len(matches) > 1 {
		resp.Status = "ambiguous"
		resp.Candidates = summarizeSymbols(matches)
		return resp, nil
	}
	if len(matches) == 0 {
		return resp, nil
	}
	target := matches[0]
	if target.Name == newName {
		return resp, fmt.Errorf("symbol %q already has the name %q", target.Name, newName)
	}
	summary := summarizeSymbol(target)
	resp.Symbol = &summary

	type editLoc struct {
		file             string
		line, col, width int
		confidence       string
	}
	var locs []editLoc
	seen := map[string]bool{}

	if line, col, ok := findNameInRange(idx, target.File, target.Name, target.StartLine, target.EndLine); ok {
		seen[fmt.Sprintf("%s:%d:%d", target.File, line, col)] = true
		locs = append(locs, editLoc{target.File, line, col, len(target.Name), ConfExact})
	}

	snap := idx.snapshot()
	snap.forEachEdge(func(_ int, e CGPEdge) bool {
		if e.To != target.ID || e.Evidence.Raw == "" {
			return true
		}
		i := lastIdentifierIndex(e.Evidence.Raw, target.Name)
		if i < 0 {
			return true
		}
		line := e.Evidence.StartLine
		col := e.Evidence.StartColumn + i
		key := fmt.Sprintf("%s:%d:%d", e.Evidence.File, line, col)
		if seen[key] {
			return true
		}
		seen[key] = true
		locs = append(locs, editLoc{e.Evidence.File, line, col, len(target.Name), e.Confidence})
		return true
	})

	sort.Slice(locs, func(i, j int) bool {
		if locs[i].file != locs[j].file {
			return locs[i].file < locs[j].file
		}
		if locs[i].line != locs[j].line {
			return locs[i].line < locs[j].line
		}
		return locs[i].col < locs[j].col
	})

	fileLines := map[string][]string{}
	weakEdits := 0
	for _, l := range locs {
		lines, ok := fileLines[l.file]
		if !ok {
			var err error
			lines, err = readSourceLines(idx, l.file)
			if err != nil {
				continue
			}
			fileLines[l.file] = lines
		}
		text := rawLine(lines, l.line)
		if l.col < 1 || l.col-1+l.width > len(text) {
			continue
		}
		old := text[l.col-1 : l.col-1+l.width]
		if old != target.Name {
			continue
		}
		if l.confidence != ConfExact && l.confidence != ConfScoped {
			weakEdits++
		}
		resp.Edits = append(resp.Edits, FileEdit{
			File:        l.file,
			StartLine:   l.line,
			StartColumn: l.col,
			EndLine:     l.line,
			EndColumn:   l.col + l.width,
			OldText:     old,
			NewText:     newName,
			Confidence:  l.confidence,
		})
	}

	files := map[string]bool{}
	for _, e := range resp.Edits {
		files[e.File] = true
	}
	resp.FilesAffected = len(files)
	if weakEdits > 0 {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf("%d edit(s) come from heuristic or unresolved references and should be reviewed before applying", weakEdits))
	}
	resp.Status = "ok"
	return resp, nil
}

// ReplaceSymbolBody produces an edit plan that replaces a symbol's entire
// source range (from its first line to its last, inclusive) with newBody. A
// trailing newline is appended to newBody if missing so the file's line
// structure stays intact.
func ReplaceSymbolBody(idx *Index, query, newBody string) (EditPlanResponse, error) {
	resp := EditPlanResponse{Status: "not_found", Operation: "replace_symbol_body", Query: query, Edits: []FileEdit{}}
	matches := findSymbols(idx, query)
	if len(matches) > 1 {
		resp.Status = "ambiguous"
		resp.Candidates = summarizeSymbols(matches)
		return resp, nil
	}
	if len(matches) == 0 {
		return resp, nil
	}
	target := matches[0]
	summary := summarizeSymbol(target)
	resp.Symbol = &summary

	lines, err := readSourceLines(idx, target.File)
	if err != nil {
		return resp, fmt.Errorf("read source for %s: %w", target.File, err)
	}
	if target.EndLine > len(lines) {
		return resp, fmt.Errorf("symbol range exceeds file length")
	}
	oldText := strings.Join(lines[target.StartLine-1:target.EndLine], "")

	if !strings.HasSuffix(newBody, "\n") {
		newBody += "\n"
	}
	resp.Edits = []FileEdit{{
		File:        target.File,
		StartLine:   target.StartLine,
		StartColumn: 1,
		EndLine:     target.EndLine + 1,
		EndColumn:   1,
		OldText:     oldText,
		NewText:     newBody,
	}}
	resp.FilesAffected = 1
	resp.Status = "ok"
	return resp, nil
}

// InsertAfterSymbol produces an edit plan that inserts text as new lines
// immediately after a symbol's source range. A trailing newline is appended
// to text if missing.
func InsertAfterSymbol(idx *Index, query, text string) (EditPlanResponse, error) {
	resp := EditPlanResponse{Status: "not_found", Operation: "insert_after_symbol", Query: query, Edits: []FileEdit{}}
	matches := findSymbols(idx, query)
	if len(matches) > 1 {
		resp.Status = "ambiguous"
		resp.Candidates = summarizeSymbols(matches)
		return resp, nil
	}
	if len(matches) == 0 {
		return resp, nil
	}
	target := matches[0]
	summary := summarizeSymbol(target)
	resp.Symbol = &summary

	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	insertLine := target.EndLine + 1
	resp.Edits = []FileEdit{{
		File:        target.File,
		StartLine:   insertLine,
		StartColumn: 1,
		EndLine:     insertLine,
		EndColumn:   1,
		OldText:     "",
		NewText:     text,
	}}
	resp.FilesAffected = 1
	resp.Status = "ok"
	return resp, nil
}

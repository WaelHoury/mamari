package mamari

import (
	"path/filepath"
	"sort"
	"strings"
)

// maxOutlineSymbols bounds the total number of nodes (across the whole tree)
// returned by FileOutline, so a single enormous generated file cannot flood
// the response.
const maxOutlineSymbols = 1000

// maxOutlineDepth bounds recursion depth when building the symbol tree. Real
// nesting (class -> method -> nested function) is at most a handful of
// levels deep; this is a defensive cap against any unexpected ParentID cycle.
const maxOutlineDepth = 25

// FileOutline returns a repo-relative file's symbol tree (nesting via
// ParentID) with signatures and metadata only — no source text. It is a
// cheap "what's in this file" step before fetch_context, especially useful
// for large files where list_symbols returns a flat, unordered list.
func FileOutline(idx *Index, file string, opts FileOutlineOptions) FileOutlineResponse {
	rel := normalizeOutlinePath(file)
	resp := FileOutlineResponse{Status: "not_found", File: rel, Symbols: []OutlineSymbol{}}

	idx.ensureFileSymbolIndex()
	idx.mu.Lock()
	syms := append([]CGPSymbol(nil), idx.symbolsByFile[rel]...)
	children := make(map[string][]CGPSymbol, len(idx.childrenByParent))
	for k, v := range idx.childrenByParent {
		children[k] = append([]CGPSymbol(nil), v...)
	}
	_, fileKnown := idx.Files[rel]
	idx.mu.Unlock()

	if len(syms) == 0 && !fileKnown {
		return resp
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = maxOutlineSymbols
	}

	count := 0
	truncated := false
	var build func(sym CGPSymbol, depth int) OutlineSymbol
	build = func(sym CGPSymbol, depth int) OutlineSymbol {
		out := OutlineSymbol{
			ID:         sym.ID,
			Name:       sym.Name,
			Kind:       sym.Kind,
			StartLine:  sym.StartLine,
			EndLine:    sym.EndLine,
			Signature:  sym.Signature,
			Exported:   sym.Exported,
			Complexity: sym.Complexity,
		}
		if depth >= maxOutlineDepth {
			return out
		}
		for _, child := range children[sym.ID] {
			if count >= limit {
				truncated = true
				break
			}
			count++
			out.Children = append(out.Children, build(child, depth+1))
		}
		return out
	}

	fileSymID := fileSymbolID(rel)
	var roots []CGPSymbol
	for _, sym := range syms {
		if sym.Kind == "file" || (sym.ParentID != "" && sym.ParentID != fileSymID) {
			continue
		}
		roots = append(roots, sym)
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].StartLine < roots[j].StartLine })

	out := make([]OutlineSymbol, 0, len(roots))
	for _, sym := range roots {
		if count >= limit {
			truncated = true
			break
		}
		count++
		out = append(out, build(sym, 0))
	}

	resp.Status = "ok"
	resp.Total = count
	resp.Limit = limit
	resp.Truncated = truncated
	resp.Symbols = out
	return resp
}

func normalizeOutlinePath(file string) string {
	rel := filepath.ToSlash(filepath.Clean(file))
	rel = strings.TrimPrefix(rel, "./")
	return rel
}

package mamari

import "sort"

// maxDiffIndexEntries bounds each section (symbolsAdded/Removed/Changed,
// edgesAdded/Removed) of DiffIndex's response, so diffing two large indexes
// cannot produce an unbounded payload.
const maxDiffIndexEntries = 1000

// DiffIndex computes a structural diff between two CGP index snapshots —
// typically a "before" index (e.g. merge-base) and an "after" index (e.g.
// HEAD) — without re-running the whole graph by hand. It compares symbols by
// ID (added/removed/changed) and edges by ID (added/removed).
//
// Symbol IDs are derived from (language, kind, file, qualified name) via
// stableSymbolID, so a symbol that is renamed or moved to a different file
// appears as one removal plus one addition rather than a "change" — this is
// intentional: renames/moves are exactly the kind of structural shift a PR
// reviewer wants surfaced, not hidden behind a same-ID "change."
func DiffIndex(base, head *Index) DiffIndexResponse {
	baseSnap := base.snapshot()
	headSnap := head.snapshot()

	resp := DiffIndexResponse{
		Status:         "ok",
		SymbolsAdded:   []CGPSymbolSummary{},
		SymbolsRemoved: []CGPSymbolSummary{},
		SymbolsChanged: []SymbolChange{},
		EdgesAdded:     []CGPEdge{},
		EdgesRemoved:   []CGPEdge{},
	}

	var added, removed []CGPSymbolSummary
	var changed []SymbolChange
	for id, headSym := range headSnap.Symbols {
		baseSym, ok := baseSnap.Symbols[id]
		if !ok {
			added = append(added, summarizeSymbol(headSym))
			continue
		}
		if fields := diffSymbolFields(baseSym, headSym); len(fields) > 0 {
			changed = append(changed, SymbolChange{
				ID:     id,
				Old:    summarizeSymbol(baseSym),
				New:    summarizeSymbol(headSym),
				Fields: fields,
			})
		}
	}
	for id, baseSym := range baseSnap.Symbols {
		if _, ok := headSnap.Symbols[id]; !ok {
			removed = append(removed, summarizeSymbol(baseSym))
		}
	}

	sortSymbolSummaries(added, "")
	sortSymbolSummaries(removed, "")
	sort.Slice(changed, func(i, j int) bool {
		if changed[i].New.File != changed[j].New.File {
			return changed[i].New.File < changed[j].New.File
		}
		return changed[i].New.Name < changed[j].New.Name
	})

	resp.Summary.SymbolsAdded = len(added)
	resp.Summary.SymbolsRemoved = len(removed)
	resp.Summary.SymbolsChanged = len(changed)
	resp.SymbolsAdded = capSummaries(added, maxDiffIndexEntries)
	resp.SymbolsRemoved = capSummaries(removed, maxDiffIndexEntries)
	if len(changed) > maxDiffIndexEntries {
		changed = changed[:maxDiffIndexEntries]
	}
	resp.SymbolsChanged = changed

	baseEdges := make(map[string]CGPEdge, baseSnap.edgeCount())
	baseSnap.forEachEdge(func(index int, e CGPEdge) bool {
		e = baseSnap.edgeAtWithID(index)
		baseEdges[e.ID] = e
		return true
	})
	headEdges := make(map[string]CGPEdge, headSnap.edgeCount())
	headSnap.forEachEdge(func(index int, e CGPEdge) bool {
		e = headSnap.edgeAtWithID(index)
		headEdges[e.ID] = e
		return true
	})
	var edgesAdded, edgesRemoved []CGPEdge
	for id, e := range headEdges {
		if _, ok := baseEdges[id]; !ok {
			edgesAdded = append(edgesAdded, e)
		}
	}
	for id, e := range baseEdges {
		if _, ok := headEdges[id]; !ok {
			edgesRemoved = append(edgesRemoved, e)
		}
	}
	sort.Slice(edgesAdded, func(i, j int) bool { return edgesAdded[i].ID < edgesAdded[j].ID })
	sort.Slice(edgesRemoved, func(i, j int) bool { return edgesRemoved[i].ID < edgesRemoved[j].ID })

	resp.Summary.EdgesAdded = len(edgesAdded)
	resp.Summary.EdgesRemoved = len(edgesRemoved)
	if len(edgesAdded) > maxDiffIndexEntries {
		edgesAdded = edgesAdded[:maxDiffIndexEntries]
	}
	if len(edgesRemoved) > maxDiffIndexEntries {
		edgesRemoved = edgesRemoved[:maxDiffIndexEntries]
	}
	resp.EdgesAdded = edgesAdded
	resp.EdgesRemoved = edgesRemoved

	if resp.SymbolsAdded == nil {
		resp.SymbolsAdded = []CGPSymbolSummary{}
	}
	if resp.SymbolsRemoved == nil {
		resp.SymbolsRemoved = []CGPSymbolSummary{}
	}
	if resp.SymbolsChanged == nil {
		resp.SymbolsChanged = []SymbolChange{}
	}
	if resp.EdgesAdded == nil {
		resp.EdgesAdded = []CGPEdge{}
	}
	if resp.EdgesRemoved == nil {
		resp.EdgesRemoved = []CGPEdge{}
	}
	return resp
}

// diffSymbolFields returns the names of top-level CGPSymbol attributes that
// differ between old and new, excluding ID (compared by caller).
func diffSymbolFields(old, new CGPSymbol) []string {
	var fields []string
	if old.Name != new.Name {
		fields = append(fields, "name")
	}
	if old.Kind != new.Kind {
		fields = append(fields, "kind")
	}
	if old.File != new.File {
		fields = append(fields, "file")
	}
	if old.StartLine != new.StartLine || old.StartColumn != new.StartColumn {
		fields = append(fields, "startLine")
	}
	if old.EndLine != new.EndLine || old.EndColumn != new.EndColumn {
		fields = append(fields, "endLine")
	}
	if old.Signature != new.Signature {
		fields = append(fields, "signature")
	}
	if old.Exported != new.Exported {
		fields = append(fields, "exported")
	}
	if old.ParentID != new.ParentID {
		fields = append(fields, "parentId")
	}
	if old.Confidence != new.Confidence {
		fields = append(fields, "confidence")
	}
	if old.Complexity != new.Complexity {
		fields = append(fields, "complexity")
	}
	return fields
}

func capSummaries(in []CGPSymbolSummary, limit int) []CGPSymbolSummary {
	if len(in) > limit {
		return in[:limit]
	}
	return in
}

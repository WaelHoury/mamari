package mamari

import (
	"fmt"
	"math"
)

// compactSymbolEdge is the read-only representation used by long-running MCP
// and UI processes. CGPEdge is convenient at the API boundary, but its string
// headers and native-width integers cost 176 bytes per edge on 64-bit systems.
// This fixed-width record costs 48 bytes and refers to a shared string table.
type compactSymbolEdge struct {
	from, to, edgeType, confidence uint32
	reason, file, kind, raw        uint32
	startLine, startColumn         int32
	endLine, endColumn             int32
}

type compactSymbolEdgeStore struct {
	strings      []string
	edges        []compactSymbolEdge
	exceptionIDs map[uint32]string
}

func canonicalCGPEdgeID(from, to, edgeType string, loc Location) string {
	return fmt.Sprintf("cgpedge:%s:%s:%s:%s:%d:%d", edgeType, from, to, loc.File, loc.StartLine, loc.StartColumn)
}

func newCompactSymbolEdgeStore(edges []CGPEdge) *compactSymbolEdgeStore {
	if len(edges) == 0 {
		return &compactSymbolEdgeStore{strings: []string{""}}
	}
	if uint64(len(edges))*8+1 > math.MaxUint32 {
		return nil
	}
	store := &compactSymbolEdgeStore{strings: []string{""}, edges: make([]compactSymbolEdge, len(edges))}
	ids := map[string]uint32{"": 0}
	intern := func(value string) (uint32, bool) {
		if id, ok := ids[value]; ok {
			return id, true
		}
		if len(store.strings) == math.MaxUint32 {
			return 0, false
		}
		id := uint32(len(store.strings))
		ids[value] = id
		store.strings = append(store.strings, value)
		return id, true
	}
	toInt32 := func(value int) (int32, bool) {
		if value < math.MinInt32 || value > math.MaxInt32 {
			return 0, false
		}
		return int32(value), true
	}
	for i, edge := range edges {
		compact := compactSymbolEdge{}
		var ok bool
		if compact.from, ok = intern(edge.From); !ok {
			return nil
		}
		if compact.to, ok = intern(edge.To); !ok {
			return nil
		}
		if compact.edgeType, ok = intern(edge.Type); !ok {
			return nil
		}
		if compact.confidence, ok = intern(edge.Confidence); !ok {
			return nil
		}
		if compact.reason, ok = intern(edge.UnresolvedReason); !ok {
			return nil
		}
		if compact.file, ok = intern(edge.Evidence.File); !ok {
			return nil
		}
		if compact.kind, ok = intern(edge.Evidence.Kind); !ok {
			return nil
		}
		if compact.raw, ok = intern(edge.Evidence.Raw); !ok {
			return nil
		}
		if compact.startLine, ok = toInt32(edge.Evidence.StartLine); !ok {
			return nil
		}
		if compact.startColumn, ok = toInt32(edge.Evidence.StartColumn); !ok {
			return nil
		}
		if compact.endLine, ok = toInt32(edge.Evidence.EndLine); !ok {
			return nil
		}
		if compact.endColumn, ok = toInt32(edge.Evidence.EndColumn); !ok {
			return nil
		}
		store.edges[i] = compact
		if edge.ID != "" && edge.ID != canonicalCGPEdgeID(edge.From, edge.To, edge.Type, edge.Evidence) {
			if store.exceptionIDs == nil {
				store.exceptionIDs = make(map[uint32]string)
			}
			store.exceptionIDs[uint32(i)] = edge.ID
		}
	}
	return store
}

func (store *compactSymbolEdgeStore) len() int {
	if store == nil {
		return 0
	}
	return len(store.edges)
}

func (store *compactSymbolEdgeStore) edgeAt(index int, includeID bool) CGPEdge {
	if store == nil || index < 0 || index >= len(store.edges) {
		return CGPEdge{}
	}
	compact := store.edges[index]
	edge := CGPEdge{
		From:             store.strings[compact.from],
		To:               store.strings[compact.to],
		Type:             store.strings[compact.edgeType],
		Confidence:       store.strings[compact.confidence],
		UnresolvedReason: store.strings[compact.reason],
		Evidence: Location{
			File:        store.strings[compact.file],
			StartLine:   int(compact.startLine),
			StartColumn: int(compact.startColumn),
			EndLine:     int(compact.endLine),
			EndColumn:   int(compact.endColumn),
			Kind:        store.strings[compact.kind],
			Raw:         store.strings[compact.raw],
		},
	}
	if includeID {
		if id, ok := store.exceptionIDs[uint32(index)]; ok {
			edge.ID = id
		} else {
			edge.ID = canonicalCGPEdgeID(edge.From, edge.To, edge.Type, edge.Evidence)
		}
	}
	return edge
}

func (store *compactSymbolEdgeStore) materialize(includeIDs bool) []CGPEdge {
	if store == nil {
		return nil
	}
	edges := make([]CGPEdge, len(store.edges))
	for i := range edges {
		edges[i] = store.edgeAt(i, includeIDs)
	}
	return edges
}

func (snap symbolGraphSnapshot) edgeCount() int {
	if snap.CompactSymbolEdges != nil {
		return snap.CompactSymbolEdges.len()
	}
	return len(snap.SymbolEdges)
}

func (snap symbolGraphSnapshot) edgeAt(index int) CGPEdge {
	if snap.CompactSymbolEdges != nil {
		return snap.CompactSymbolEdges.edgeAt(index, false)
	}
	if index < 0 || index >= len(snap.SymbolEdges) {
		return CGPEdge{}
	}
	return snap.SymbolEdges[index]
}

func (snap symbolGraphSnapshot) edgeAtWithID(index int) CGPEdge {
	if snap.CompactSymbolEdges != nil {
		return snap.CompactSymbolEdges.edgeAt(index, true)
	}
	if index < 0 || index >= len(snap.SymbolEdges) {
		return CGPEdge{}
	}
	return snap.SymbolEdges[index]
}

// forEachEdge avoids materializing the expanded graph. Returning false from
// visit stops iteration early.
func (snap symbolGraphSnapshot) forEachEdge(visit func(index int, edge CGPEdge) bool) {
	if snap.CompactSymbolEdges != nil {
		for i := range snap.CompactSymbolEdges.edges {
			if !visit(i, snap.CompactSymbolEdges.edgeAt(i, false)) {
				return
			}
		}
		return
	}
	for i, edge := range snap.SymbolEdges {
		if !visit(i, edge) {
			return
		}
	}
}

func (snap indexSnapshot) edgeCount() int {
	if snap.compactSymbolEdges != nil {
		return snap.compactSymbolEdges.len()
	}
	return len(snap.SymbolEdges)
}

func (snap indexSnapshot) edgeAtWithID(index int) CGPEdge {
	if snap.compactSymbolEdges != nil {
		return snap.compactSymbolEdges.edgeAt(index, true)
	}
	if index < 0 || index >= len(snap.SymbolEdges) {
		return CGPEdge{}
	}
	return snap.SymbolEdges[index]
}

func (snap indexSnapshot) forEachEdge(visit func(index int, edge CGPEdge) bool) {
	if snap.compactSymbolEdges != nil {
		for i := range snap.compactSymbolEdges.edges {
			if !visit(i, snap.compactSymbolEdges.edgeAt(i, false)) {
				return
			}
		}
		return
	}
	for i, edge := range snap.SymbolEdges {
		if !visit(i, edge) {
			return
		}
	}
}

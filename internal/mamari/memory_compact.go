package mamari

// compactLoadedIndexStrings canonicalizes high-repetition strings decoded
// from the persisted index. encoding/gob allocates a fresh backing string for
// every occurrence, so a large call graph otherwise retains thousands of
// copies of the same symbol IDs, file paths, edge types, and confidence
// labels. The values are immutable and string equality is value-based; sharing
// backing storage therefore changes no observable behavior.
//
// Call this before initRuntimeLocked builds dedup and lookup maps so those
// maps inherit the canonical strings instead of retaining decoded duplicates.
func compactLoadedIndexStrings(idx *Index) {
	if idx == nil {
		return
	}
	canonical := make(map[string]string, len(idx.Symbols)*2+len(idx.Files)*4)
	intern := func(value string) string {
		if value == "" {
			return ""
		}
		if existing, ok := canonical[value]; ok {
			return existing
		}
		canonical[value] = value
		return value
	}
	compactLocation := func(loc *Location) {
		loc.File = intern(loc.File)
		loc.Kind = intern(loc.Kind)
	}

	idx.Repo.GitCommit = intern(idx.Repo.GitCommit)

	files := make(map[string]File, len(idx.Files))
	for key, file := range idx.Files {
		key = intern(key)
		file.ID = intern(file.ID)
		file.Path = intern(file.Path)
		file.Language = intern(file.Language)
		file.Parser = intern(file.Parser)
		file.ParseStatus = intern(file.ParseStatus)
		files[key] = file
	}
	idx.Files = files

	symbols := make(map[string]CGPSymbol, len(idx.Symbols))
	for key, symbol := range idx.Symbols {
		key = intern(key)
		symbol.ID = intern(symbol.ID)
		symbol.Name = intern(symbol.Name)
		symbol.Kind = intern(symbol.Kind)
		symbol.Language = intern(symbol.Language)
		symbol.File = intern(symbol.File)
		symbol.ReceiverType = intern(symbol.ReceiverType)
		symbol.ParentID = intern(symbol.ParentID)
		symbol.Confidence = intern(symbol.Confidence)
		symbol.SCIPSymbol = intern(symbol.SCIPSymbol)
		for i := range symbol.ReturnTypes {
			symbol.ReturnTypes[i] = intern(symbol.ReturnTypes[i])
		}
		symbols[key] = symbol
	}
	idx.Symbols = symbols
	idx.orderedSymbolIDs = nil

	for i := range idx.SymbolEdges {
		edge := &idx.SymbolEdges[i]
		edge.From = intern(edge.From)
		edge.To = intern(edge.To)
		edge.Type = intern(edge.Type)
		edge.Confidence = intern(edge.Confidence)
		edge.UnresolvedReason = intern(edge.UnresolvedReason)
		compactLocation(&edge.Evidence)
	}
	for i := range idx.References {
		ref := &idx.References[i]
		ref.TermID = intern(ref.TermID)
		ref.Term = intern(ref.Term)
		ref.IRI = intern(ref.IRI)
		ref.File = intern(ref.File)
		ref.Confidence = intern(ref.Confidence)
		ref.Kind = intern(ref.Kind)
	}
	for i := range idx.Edges {
		edge := &idx.Edges[i]
		edge.From = intern(edge.From)
		edge.To = intern(edge.To)
		edge.Type = intern(edge.Type)
		edge.Confidence = intern(edge.Confidence)
		compactLocation(&edge.Evidence)
	}
	for i := range idx.DynamicIRICalls {
		call := &idx.DynamicIRICalls[i]
		call.File = intern(call.File)
		call.Callee = intern(call.Callee)
	}
}

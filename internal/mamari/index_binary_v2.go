package mamari

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
)

// The v2 index is a deterministic, reflection-free binary encoding. Every
// distinct string is stored once in a sorted table and records refer to it by
// integer ID. Collection lengths are written before their contents so the
// decoder can allocate each map/slice exactly once.
var indexBinaryMagicV2 = []byte("mamari-index-v2\n")

const indexBinaryChecksumSize = sha256.Size

type indexStringTable struct {
	ids     map[string]uint64
	ordered []string // ID 0 is always the empty string.
}

func buildIndexStringTable(snapshot indexSnapshot) indexStringTable {
	unique := make(map[string]struct{}, len(snapshot.Symbols)*3+len(snapshot.SymbolEdges)*2+len(snapshot.Files)*4)
	add := func(values ...string) {
		for _, value := range values {
			if value != "" {
				unique[value] = struct{}{}
			}
		}
	}
	addLocation := func(loc Location) { add(loc.File, loc.Kind, loc.Raw) }
	addLiteral := func(lit Literal) {
		add(lit.Predicate, lit.Value, lit.Lang, lit.ShapeID)
		addLocation(lit.Location)
	}

	add(snapshot.Repo.Root, snapshot.Repo.IndexedAt, snapshot.Repo.GitCommit)
	for key, file := range snapshot.Files {
		add(key, file.ID, file.Path, file.Language, file.SHA256, file.Parser, file.ParseStatus, file.ParseError)
		for prefix, iri := range file.Prefixes {
			add(prefix, iri)
		}
	}
	for key, prefix := range snapshot.Prefixes {
		add(key, prefix.Prefix, prefix.IRI)
		addLocation(prefix.Location)
	}
	for key, term := range snapshot.Terms {
		add(key, term.ID, term.Term, term.IRI, term.Prefix, term.LocalName)
		for _, loc := range term.Locations {
			addLocation(loc)
		}
	}
	for key, shape := range snapshot.Shapes {
		add(key, shape.ID, shape.TermID, shape.Term, shape.IRI)
		addLocation(shape.Location)
		for _, links := range [][]ShapeLink{shape.TargetClasses, shape.Paths, shape.Nodes, shape.Predicates} {
			for _, link := range links {
				add(link.Predicate, link.Term, link.IRI)
				addLocation(link.Location)
			}
		}
		for _, branch := range shape.Branches {
			add(branch.Kind, branch.Name, branch.Datatype, branch.DatatypeIRI, branch.Pattern, branch.Path, branch.PathIRI)
			addLocation(branch.Location)
		}
		for _, lit := range shape.Names {
			addLiteral(lit)
		}
		for _, loc := range shape.Unsupported {
			addLocation(loc)
		}
	}
	for _, ref := range snapshot.References {
		add(ref.ID, ref.TermID, ref.Term, ref.IRI, ref.File, ref.Confidence, ref.Kind, ref.Context)
	}
	for _, edge := range snapshot.Edges {
		add(edge.ID, edge.From, edge.To, edge.Type, edge.Confidence)
		addLocation(edge.Evidence)
	}
	for _, call := range snapshot.DynamicIRICalls {
		add(call.File, call.Callee, call.Snippet)
	}
	for key, symbol := range snapshot.Symbols {
		add(key, symbol.ID, symbol.Name, symbol.Kind, symbol.Language, symbol.File, symbol.Signature, symbol.Docstring,
			symbol.ReceiverType, symbol.ParentID, symbol.Confidence, symbol.SCIPSymbol, symbol.ShapeHash)
		add(symbol.ReturnTypes...)
	}
	for _, edge := range snapshot.SymbolEdges {
		if edge.ID != canonicalCGPEdgeID(edge.From, edge.To, edge.Type, edge.Evidence) {
			add(edge.ID)
		}
		add(edge.From, edge.To, edge.Type, edge.Confidence, edge.UnresolvedReason)
		addLocation(edge.Evidence)
	}

	ordered := make([]string, 0, len(unique)+1)
	ordered = append(ordered, "")
	for value := range unique {
		ordered = append(ordered, value)
	}
	sort.Strings(ordered[1:])
	ids := make(map[string]uint64, len(ordered))
	for id, value := range ordered {
		ids[value] = uint64(id)
	}
	return indexStringTable{ids: ids, ordered: ordered}
}

type indexBinaryWriter struct {
	buf     bytes.Buffer
	table   indexStringTable
	err     error
	scratch [binary.MaxVarintLen64]byte
}

func (w *indexBinaryWriter) u64(value uint64) {
	if w.err != nil {
		return
	}
	n := binary.PutUvarint(w.scratch[:], value)
	_, w.err = w.buf.Write(w.scratch[:n])
}

func (w *indexBinaryWriter) integer(value int) {
	if w.err != nil {
		return
	}
	n := binary.PutVarint(w.scratch[:], int64(value))
	_, w.err = w.buf.Write(w.scratch[:n])
}

func (w *indexBinaryWriter) boolean(value bool) {
	if value {
		w.u64(1)
	} else {
		w.u64(0)
	}
}

func (w *indexBinaryWriter) stringID(value string) {
	id, ok := w.table.ids[value]
	if !ok {
		w.err = fmt.Errorf("v2 index encoder: string table is missing %q", value)
		return
	}
	w.u64(id)
}

func (w *indexBinaryWriter) count(length int) { w.u64(uint64(length)) }

func (w *indexBinaryWriter) location(loc Location) {
	w.stringID(loc.File)
	w.integer(loc.StartLine)
	w.integer(loc.StartColumn)
	w.integer(loc.EndLine)
	w.integer(loc.EndColumn)
	w.stringID(loc.Kind)
	w.stringID(loc.Raw)
}

func (w *indexBinaryWriter) literal(lit Literal) {
	w.stringID(lit.Predicate)
	w.stringID(lit.Value)
	w.stringID(lit.Lang)
	w.location(lit.Location)
	w.stringID(lit.ShapeID)
}

func sortedIndexMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func marshalBinaryIndexV2(snapshot indexSnapshot) ([]byte, error) {
	table := buildIndexStringTable(snapshot)
	w := &indexBinaryWriter{table: table}

	// The string section stores lengths followed by one contiguous blob. On
	// decode, every string can therefore share the blob's backing allocation.
	w.count(len(table.ordered) - 1)
	for _, value := range table.ordered[1:] {
		w.count(len(value))
	}
	for _, value := range table.ordered[1:] {
		if w.err == nil {
			_, w.err = w.buf.WriteString(value)
		}
	}

	w.integer(snapshot.SchemaVersion)
	w.stringID(snapshot.Repo.Root)
	w.stringID(snapshot.Repo.IndexedAt)
	w.stringID(snapshot.Repo.GitCommit)

	w.count(len(snapshot.Files))
	for _, key := range sortedIndexMapKeys(snapshot.Files) {
		file := snapshot.Files[key]
		w.stringID(key)
		w.stringID(file.ID)
		w.stringID(file.Path)
		w.stringID(file.Language)
		w.stringID(file.SHA256)
		w.integer(file.LineCount)
		w.count(len(file.Prefixes))
		for _, prefix := range sortedIndexMapKeys(file.Prefixes) {
			w.stringID(prefix)
			w.stringID(file.Prefixes[prefix])
		}
		w.stringID(file.Parser)
		w.stringID(file.ParseStatus)
		w.stringID(file.ParseError)
	}

	w.count(len(snapshot.Prefixes))
	for _, key := range sortedIndexMapKeys(snapshot.Prefixes) {
		prefix := snapshot.Prefixes[key]
		w.stringID(key)
		w.stringID(prefix.Prefix)
		w.stringID(prefix.IRI)
		w.location(prefix.Location)
	}

	w.count(len(snapshot.Terms))
	for _, key := range sortedIndexMapKeys(snapshot.Terms) {
		term := snapshot.Terms[key]
		w.stringID(key)
		w.stringID(term.ID)
		w.stringID(term.Term)
		w.stringID(term.IRI)
		w.stringID(term.Prefix)
		w.stringID(term.LocalName)
		w.count(len(term.Locations))
		for _, loc := range term.Locations {
			w.location(loc)
		}
	}

	w.count(len(snapshot.Shapes))
	for _, key := range sortedIndexMapKeys(snapshot.Shapes) {
		shape := snapshot.Shapes[key]
		w.stringID(key)
		w.stringID(shape.ID)
		w.stringID(shape.TermID)
		w.stringID(shape.Term)
		w.stringID(shape.IRI)
		w.location(shape.Location)
		for _, links := range [][]ShapeLink{shape.TargetClasses, shape.Paths, shape.Nodes, shape.Predicates} {
			w.count(len(links))
			for _, link := range links {
				w.stringID(link.Predicate)
				w.stringID(link.Term)
				w.stringID(link.IRI)
				w.location(link.Location)
			}
		}
		w.count(len(shape.Branches))
		for _, branch := range shape.Branches {
			w.stringID(branch.Kind)
			w.stringID(branch.Name)
			w.stringID(branch.Datatype)
			w.stringID(branch.DatatypeIRI)
			w.stringID(branch.Pattern)
			w.stringID(branch.Path)
			w.stringID(branch.PathIRI)
			w.location(branch.Location)
		}
		w.count(len(shape.Names))
		for _, lit := range shape.Names {
			w.literal(lit)
		}
		w.count(len(shape.Unsupported))
		for _, loc := range shape.Unsupported {
			w.location(loc)
		}
	}

	w.count(len(snapshot.References))
	for _, ref := range snapshot.References {
		w.stringID(ref.ID)
		w.stringID(ref.TermID)
		w.stringID(ref.Term)
		w.stringID(ref.IRI)
		w.stringID(ref.File)
		w.integer(ref.StartLine)
		w.integer(ref.StartColumn)
		w.integer(ref.EndLine)
		w.integer(ref.EndColumn)
		w.stringID(ref.Confidence)
		w.stringID(ref.Kind)
		w.stringID(ref.Context)
	}

	w.count(len(snapshot.Edges))
	for _, edge := range snapshot.Edges {
		w.stringID(edge.ID)
		w.stringID(edge.From)
		w.stringID(edge.To)
		w.stringID(edge.Type)
		w.stringID(edge.Confidence)
		w.location(edge.Evidence)
	}

	w.count(len(snapshot.DynamicIRICalls))
	for _, call := range snapshot.DynamicIRICalls {
		w.stringID(call.File)
		w.integer(call.Line)
		w.integer(call.Column)
		w.stringID(call.Callee)
		w.stringID(call.Snippet)
	}

	w.count(len(snapshot.Symbols))
	for _, key := range sortedIndexMapKeys(snapshot.Symbols) {
		symbol := snapshot.Symbols[key]
		w.stringID(key)
		w.stringID(symbol.ID)
		w.stringID(symbol.Name)
		w.stringID(symbol.Kind)
		w.stringID(symbol.Language)
		w.stringID(symbol.File)
		w.integer(symbol.StartLine)
		w.integer(symbol.StartColumn)
		w.integer(symbol.EndLine)
		w.integer(symbol.EndColumn)
		w.stringID(symbol.Signature)
		w.stringID(symbol.Docstring)
		w.count(len(symbol.ReturnTypes))
		for _, returnType := range symbol.ReturnTypes {
			w.stringID(returnType)
		}
		w.stringID(symbol.ReceiverType)
		w.boolean(symbol.Exported)
		w.stringID(symbol.ParentID)
		w.stringID(symbol.Confidence)
		w.stringID(symbol.SCIPSymbol)
		w.integer(symbol.Complexity)
		w.integer(symbol.LoopDepth)
		w.integer(symbol.TransitiveLoopDepth)
		w.integer(symbol.LinearScanInLoop)
		w.integer(symbol.AllocInLoop)
		w.boolean(symbol.RecursionInLoop)
		w.stringID(symbol.ShapeHash)
	}

	w.count(len(snapshot.SymbolEdges))
	for _, edge := range snapshot.SymbolEdges {
		canonicalID := edge.ID == canonicalCGPEdgeID(edge.From, edge.To, edge.Type, edge.Evidence)
		w.boolean(canonicalID)
		if !canonicalID {
			w.stringID(edge.ID)
		}
		w.stringID(edge.From)
		w.stringID(edge.To)
		w.stringID(edge.Type)
		w.stringID(edge.Confidence)
		w.stringID(edge.UnresolvedReason)
		w.location(edge.Evidence)
	}
	if w.err != nil {
		return nil, w.err
	}
	payload := w.buf.Bytes()
	checksum := sha256.Sum256(payload)
	data := make([]byte, 0, len(indexBinaryMagicV2)+len(payload)+len(checksum))
	data = append(data, indexBinaryMagicV2...)
	data = append(data, payload...)
	data = append(data, checksum[:]...)
	return data, nil
}

type indexBinaryReader struct {
	data    []byte
	pos     int
	strings []string
	err     error
}

func (r *indexBinaryReader) u64() uint64 {
	if r.err != nil {
		return 0
	}
	value, n := binary.Uvarint(r.data[r.pos:])
	if n <= 0 {
		r.err = fmt.Errorf("v2 index: invalid unsigned integer at byte %d", r.pos)
		return 0
	}
	r.pos += n
	return value
}

func (r *indexBinaryReader) integer() int {
	if r.err != nil {
		return 0
	}
	value, n := binary.Varint(r.data[r.pos:])
	if n <= 0 {
		r.err = fmt.Errorf("v2 index: invalid integer at byte %d", r.pos)
		return 0
	}
	r.pos += n
	converted := int(value)
	if int64(converted) != value {
		r.err = fmt.Errorf("v2 index: integer %d overflows int", value)
		return 0
	}
	return converted
}

func (r *indexBinaryReader) count(label string) int {
	value := r.u64()
	if r.err != nil {
		return 0
	}
	// Every encoded collection element consumes at least one byte. This
	// bounds allocations before trusting a file-controlled count.
	if value > uint64(len(r.data)-r.pos) || uint64(int(value)) != value {
		r.err = fmt.Errorf("v2 index: invalid %s count %d", label, value)
		return 0
	}
	return int(value)
}

func (r *indexBinaryReader) stringID() string {
	id := r.u64()
	if r.err != nil {
		return ""
	}
	if id >= uint64(len(r.strings)) {
		r.err = fmt.Errorf("v2 index: string ID %d is out of range", id)
		return ""
	}
	return r.strings[id]
}

func (r *indexBinaryReader) boolean() bool {
	value := r.u64()
	if value > 1 && r.err == nil {
		r.err = fmt.Errorf("v2 index: invalid boolean %d", value)
	}
	return value == 1
}

func (r *indexBinaryReader) location() Location {
	return Location{
		File: r.stringID(), StartLine: r.integer(), StartColumn: r.integer(),
		EndLine: r.integer(), EndColumn: r.integer(), Kind: r.stringID(), Raw: r.stringID(),
	}
}

func (r *indexBinaryReader) literal() Literal {
	return Literal{
		Predicate: r.stringID(), Value: r.stringID(), Lang: r.stringID(),
		Location: r.location(), ShapeID: r.stringID(),
	}
}

func (r *indexBinaryReader) stringTable() {
	count := r.count("string table")
	lengths := make([]int, count)
	total := 0
	for i := range lengths {
		lengths[i] = r.count("string length")
		if lengths[i] <= 0 && r.err == nil {
			r.err = fmt.Errorf("v2 index: empty string table entry %d", i+1)
		}
		if lengths[i] > len(r.data)-r.pos-total {
			r.err = fmt.Errorf("v2 index: string table exceeds payload")
			break
		}
		total += lengths[i]
	}
	if r.err != nil {
		return
	}
	blob := string(r.data[r.pos : r.pos+total])
	r.pos += total
	r.strings = make([]string, count+1)
	offset := 0
	for i, length := range lengths {
		r.strings[i+1] = blob[offset : offset+length]
		offset += length
	}
}

func unmarshalBinaryIndexV2(data []byte) (*Index, error) {
	if len(data) < len(indexBinaryMagicV2)+indexBinaryChecksumSize {
		return nil, fmt.Errorf("v2 index: file is truncated")
	}
	payloadEnd := len(data) - indexBinaryChecksumSize
	payload := data[len(indexBinaryMagicV2):payloadEnd]
	want := data[payloadEnd:]
	got := sha256.Sum256(payload)
	if !bytes.Equal(got[:], want) {
		return nil, fmt.Errorf("v2 index: checksum mismatch")
	}
	r := &indexBinaryReader{data: payload}
	r.stringTable()
	snapshot := indexSnapshot{}
	snapshot.SchemaVersion = r.integer()
	snapshot.Repo = RepoInfo{Root: r.stringID(), IndexedAt: r.stringID(), GitCommit: r.stringID()}

	fileCount := r.count("files")
	snapshot.Files = make(map[string]File, fileCount)
	for i := 0; i < fileCount; i++ {
		key := r.stringID()
		file := File{ID: r.stringID(), Path: r.stringID(), Language: r.stringID(), SHA256: r.stringID(), LineCount: r.integer()}
		prefixCount := r.count("file prefixes")
		if prefixCount > 0 {
			file.Prefixes = make(map[string]string, prefixCount)
			for j := 0; j < prefixCount; j++ {
				file.Prefixes[r.stringID()] = r.stringID()
			}
		}
		file.Parser, file.ParseStatus, file.ParseError = r.stringID(), r.stringID(), r.stringID()
		snapshot.Files[key] = file
	}

	prefixCount := r.count("prefixes")
	snapshot.Prefixes = make(map[string]Prefix, prefixCount)
	for i := 0; i < prefixCount; i++ {
		key := r.stringID()
		snapshot.Prefixes[key] = Prefix{Prefix: r.stringID(), IRI: r.stringID(), Location: r.location()}
	}

	termCount := r.count("terms")
	snapshot.Terms = make(map[string]Term, termCount)
	for i := 0; i < termCount; i++ {
		key := r.stringID()
		term := Term{ID: r.stringID(), Term: r.stringID(), IRI: r.stringID(), Prefix: r.stringID(), LocalName: r.stringID()}
		locationCount := r.count("term locations")
		term.Locations = make([]Location, locationCount)
		for j := range term.Locations {
			term.Locations[j] = r.location()
		}
		snapshot.Terms[key] = term
	}

	shapeCount := r.count("shapes")
	snapshot.Shapes = make(map[string]Shape, shapeCount)
	for i := 0; i < shapeCount; i++ {
		key := r.stringID()
		shape := Shape{ID: r.stringID(), TermID: r.stringID(), Term: r.stringID(), IRI: r.stringID(), Location: r.location()}
		linkTargets := []*[]ShapeLink{&shape.TargetClasses, &shape.Paths, &shape.Nodes, &shape.Predicates}
		for _, target := range linkTargets {
			linkCount := r.count("shape links")
			*target = make([]ShapeLink, linkCount)
			for j := range *target {
				(*target)[j] = ShapeLink{Predicate: r.stringID(), Term: r.stringID(), IRI: r.stringID(), Location: r.location()}
			}
		}
		branchCount := r.count("shape branches")
		shape.Branches = make([]Branch, branchCount)
		for j := range shape.Branches {
			shape.Branches[j] = Branch{
				Kind: r.stringID(), Name: r.stringID(), Datatype: r.stringID(), DatatypeIRI: r.stringID(),
				Pattern: r.stringID(), Path: r.stringID(), PathIRI: r.stringID(), Location: r.location(),
			}
		}
		nameCount := r.count("shape names")
		shape.Names = make([]Literal, nameCount)
		for j := range shape.Names {
			shape.Names[j] = r.literal()
		}
		unsupportedCount := r.count("unsupported locations")
		shape.Unsupported = make([]Location, unsupportedCount)
		for j := range shape.Unsupported {
			shape.Unsupported[j] = r.location()
		}
		snapshot.Shapes[key] = shape
	}

	referenceCount := r.count("references")
	snapshot.References = make([]Reference, referenceCount)
	for i := range snapshot.References {
		snapshot.References[i] = Reference{
			ID: r.stringID(), TermID: r.stringID(), Term: r.stringID(), IRI: r.stringID(), File: r.stringID(),
			StartLine: r.integer(), StartColumn: r.integer(), EndLine: r.integer(), EndColumn: r.integer(),
			Confidence: r.stringID(), Kind: r.stringID(), Context: r.stringID(),
		}
	}

	edgeCount := r.count("RDF edges")
	snapshot.Edges = make([]Edge, edgeCount)
	for i := range snapshot.Edges {
		snapshot.Edges[i] = Edge{
			ID: r.stringID(), From: r.stringID(), To: r.stringID(), Type: r.stringID(),
			Confidence: r.stringID(), Evidence: r.location(),
		}
	}

	dynamicCount := r.count("dynamic IRI calls")
	snapshot.DynamicIRICalls = make([]DynamicIRICall, dynamicCount)
	for i := range snapshot.DynamicIRICalls {
		snapshot.DynamicIRICalls[i] = DynamicIRICall{
			File: r.stringID(), Line: r.integer(), Column: r.integer(), Callee: r.stringID(), Snippet: r.stringID(),
		}
	}

	symbolCount := r.count("symbols")
	snapshot.Symbols = make(map[string]CGPSymbol, symbolCount)
	for i := 0; i < symbolCount; i++ {
		key := r.stringID()
		symbol := CGPSymbol{
			ID: r.stringID(), Name: r.stringID(), Kind: r.stringID(), Language: r.stringID(), File: r.stringID(),
			StartLine: r.integer(), StartColumn: r.integer(), EndLine: r.integer(), EndColumn: r.integer(),
			Signature: r.stringID(), Docstring: r.stringID(),
		}
		returnCount := r.count("symbol return types")
		if returnCount > 0 {
			symbol.ReturnTypes = make([]string, returnCount)
			for j := range symbol.ReturnTypes {
				symbol.ReturnTypes[j] = r.stringID()
			}
		}
		symbol.ReceiverType = r.stringID()
		symbol.Exported = r.boolean()
		symbol.ParentID = r.stringID()
		symbol.Confidence = r.stringID()
		symbol.SCIPSymbol = r.stringID()
		symbol.Complexity = r.integer()
		symbol.LoopDepth = r.integer()
		symbol.TransitiveLoopDepth = r.integer()
		symbol.LinearScanInLoop = r.integer()
		symbol.AllocInLoop = r.integer()
		symbol.RecursionInLoop = r.boolean()
		symbol.ShapeHash = r.stringID()
		snapshot.Symbols[key] = symbol
	}

	symbolEdgeCount := r.count("symbol edges")
	snapshot.SymbolEdges = make([]CGPEdge, symbolEdgeCount)
	for i := range snapshot.SymbolEdges {
		canonicalID := r.boolean()
		customID := ""
		if !canonicalID {
			customID = r.stringID()
		}
		edge := CGPEdge{
			ID: customID, From: r.stringID(), To: r.stringID(), Type: r.stringID(),
			Confidence: r.stringID(), UnresolvedReason: r.stringID(), Evidence: r.location(),
		}
		if canonicalID {
			edge.ID = canonicalCGPEdgeID(edge.From, edge.To, edge.Type, edge.Evidence)
		}
		snapshot.SymbolEdges[i] = edge
	}
	if r.err != nil {
		return nil, r.err
	}
	if r.pos != len(r.data) {
		return nil, fmt.Errorf("v2 index: %d trailing payload bytes", len(r.data)-r.pos)
	}
	idx := &Index{
		SchemaVersion: snapshot.SchemaVersion, Repo: snapshot.Repo, Files: snapshot.Files,
		Prefixes: snapshot.Prefixes, Terms: snapshot.Terms, Shapes: snapshot.Shapes,
		References: snapshot.References, Edges: snapshot.Edges, DynamicIRICalls: snapshot.DynamicIRICalls,
		Symbols: snapshot.Symbols, SymbolEdges: snapshot.SymbolEdges, indexStringsInterned: true,
	}
	if idx.SchemaVersion != SchemaVersion {
		return nil, unsupportedIndexSchemaError(idx.SchemaVersion)
	}
	return idx, nil
}

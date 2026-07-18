package mamari

import (
	"bytes"
	"encoding/gob"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	semanticDimensions    = 256
	semanticSparseEntries = 8
	semanticWindow        = 5
	semanticMaxDocTokens  = 512
	semanticMaxOccurrence = 512
	defaultSemanticLimit  = 10
)

var semanticSidecarMagic = []byte("mamari-semantic-v5\n")

type semanticIndex struct {
	Dimensions     int
	FileHashes     map[string]string
	TokenVectors   map[string][]int8
	ConceptVectors map[string][]int8
	Nodes          []semanticNode
}

type semanticNode struct {
	SymbolID string
	Vector   []int8
	Concepts []string
}

type semanticDocument struct {
	Symbol   CGPSymbol
	Tokens   []string
	Concepts []string
}

type persistedSemanticIndex struct {
	SchemaVersion int
	Index         semanticIndex
}

// SemanticQuery performs dependency-free vector retrieval over symbol-level
// code embeddings. The index combines IDF-weighted metadata/source tokens,
// corpus co-occurrence random indexing, software-concept canonicalization,
// and one call-graph diffusion pass. Vectors are int8-quantized and persisted
// beside the main index after their first build.
func SemanticQuery(idx *Index, query string, opts SemanticQueryOptions) SemanticQueryResponse {
	query = strings.TrimSpace(query)
	resp := SemanticQueryResponse{
		Status: "not_found", Query: query, Model: "mamari-ri-graph-v5",
		Dimensions: semanticDimensions, Hits: []SemanticQueryHit{},
	}
	if query == "" {
		resp.Status = "invalid"
		resp.Warnings = append(resp.Warnings, "empty query")
		return resp
	}
	terms := semanticTokens(query)
	if len(terms) == 0 {
		resp.Status = "invalid"
		resp.Warnings = append(resp.Warnings, "query has no semantic terms")
		return resp
	}
	if opts.Limit <= 0 {
		opts.Limit = defaultSemanticLimit
	}
	if opts.MinScore <= 0 {
		opts.MinScore = 0.40
	}

	sem := idx.ensureSemanticIndex()
	if sem == nil || len(sem.Nodes) == 0 {
		resp.Warnings = append(resp.Warnings, "semantic index is empty")
		return resp
	}
	resp.Dimensions = sem.Dimensions
	qv := semanticQueryVector(sem, terms)
	if len(qv) == 0 {
		resp.Warnings = append(resp.Warnings, "query produced a zero vector")
		return resp
	}

	snap := idx.snapshot()
	for _, node := range sem.Nodes {
		sym, ok := snap.Symbols[node.SymbolID]
		if !ok || !semanticResultKind(sym.Kind) {
			continue
		}
		listOpts := ListSymbolsOptions{SourceOnly: opts.SourceOnly, IncludeTests: opts.IncludeTests, IncludeStories: opts.IncludeStories}
		if shouldExcludeNoisyFile(sym.File, listOpts) {
			continue
		}
		matched := semanticMatchedConcepts(terms, node.Concepts)
		coverage := float64(len(matched)) / float64(maxInt(1, semanticUniqueConceptCount(terms)))
		// Blend latent cosine with explicit concept coverage. Pure averaged
		// cosine can rank a generic one-word match (e.g. `user`) above a
		// symbol matching two rarer query concepts (`send` + `alert`).
		score := semanticCosineI8(qv, node.Vector)*0.72 + coverage*0.28
		if score < opts.MinScore {
			continue
		}
		resp.Hits = append(resp.Hits, SemanticQueryHit{
			Symbol: summarizeSymbol(sym), Score: math.Round(score*10000) / 10000,
			Terms: matched,
		})
	}
	sort.SliceStable(resp.Hits, func(i, j int) bool {
		if resp.Hits[i].Score != resp.Hits[j].Score {
			return resp.Hits[i].Score > resp.Hits[j].Score
		}
		return resp.Hits[i].Symbol.ID < resp.Hits[j].Symbol.ID
	})
	resp.Total = len(resp.Hits)
	if len(resp.Hits) > opts.Limit {
		resp.Hits = resp.Hits[:opts.Limit]
		resp.Truncated = true
	}
	if len(resp.Hits) > 0 {
		resp.Status = "ok"
	}
	return resp
}

func (idx *Index) ensureSemanticIndex() *semanticIndex {
	idx.mu.Lock()
	if idx.semanticIndex != nil {
		sem := idx.semanticIndex
		idx.mu.Unlock()
		return sem
	}
	idx.mu.Unlock()

	idx.semanticBuildMu.Lock()
	defer idx.semanticBuildMu.Unlock()
	idx.mu.Lock()
	if idx.semanticIndex != nil {
		sem := idx.semanticIndex
		idx.mu.Unlock()
		return sem
	}
	path := idx.semanticSidecarPath
	idx.mu.Unlock()
	if path != "" {
		if sem := loadSemanticSidecar(idx, path); sem != nil {
			idx.mu.Lock()
			idx.semanticIndex = sem
			idx.mu.Unlock()
			return sem
		}
	}
	sem := buildSemanticIndex(idx)
	for !semanticHashesMatch(idx, sem.FileHashes) {
		// A watcher committed an edit while vectors were being built. Never
		// publish the stale generation; rebuild against the new snapshot.
		sem = buildSemanticIndex(idx)
	}
	idx.mu.Lock()
	idx.semanticIndex = sem
	idx.mu.Unlock()
	if path != "" {
		_ = saveSemanticSidecar(idx, path)
	}
	return sem
}

func (idx *Index) invalidateSemanticIndex() {
	idx.mu.Lock()
	idx.semanticIndex = nil
	idx.mu.Unlock()
}

func buildSemanticIndex(idx *Index) *semanticIndex {
	snap := idx.snapshot()
	docs := semanticDocuments(snap)
	sem := &semanticIndex{
		Dimensions: semanticDimensions, FileHashes: semanticFileHashes(snap),
		TokenVectors: map[string][]int8{}, ConceptVectors: map[string][]int8{},
	}
	if len(docs) == 0 {
		return sem
	}

	docFreq := map[string]int{}
	tokenFreq := map[string]int{}
	vocab := map[string]bool{}
	for i := range docs {
		seen := map[string]bool{}
		for _, token := range docs[i].Tokens {
			vocab[token] = true
			tokenFreq[token]++
			if !seen[token] {
				docFreq[token]++
				seen[token] = true
			}
		}
	}

	floatTokens := make(map[string][]float32, len(vocab))
	for token := range vocab {
		vec := make([]float32, semanticDimensions)
		semanticAddBaseVector(vec, semanticCanonical(token), 1)
		floatTokens[token] = vec
	}
	occurrences := map[string]int{}
	for _, doc := range docs {
		for i, token := range doc.Tokens {
			if occurrences[token] >= semanticMaxOccurrence {
				continue
			}
			occurrences[token]++
			processedOccurrences := tokenFreq[token]
			if processedOccurrences > semanticMaxOccurrence {
				processedOccurrences = semanticMaxOccurrence
			}
			// Bound the TOTAL context mass contributed to a token, rather
			// than adding a fixed amount per occurrence. The latter makes
			// frequent code words converge toward one common direction and
			// yields meaningless ~0.95 cosine scores across most of a repo.
			contextScale := float32(0.6 / float64(maxInt(1, processedOccurrences*semanticWindow*2)))
			start, end := i-semanticWindow, i+semanticWindow+1
			if start < 0 {
				start = 0
			}
			if end > len(doc.Tokens) {
				end = len(doc.Tokens)
			}
			for j := start; j < end; j++ {
				if j != i {
					semanticAddBaseVector(floatTokens[token], semanticCanonical(doc.Tokens[j]), contextScale)
				}
			}
		}
	}
	for token, vec := range floatTokens {
		semanticNormalize(vec)
		sem.TokenVectors[token] = semanticQuantize(vec)
	}

	conceptSums := map[string][]float32{}
	conceptCounts := map[string]int{}
	for token, vec := range floatTokens {
		concept := semanticCanonical(token)
		if conceptSums[concept] == nil {
			conceptSums[concept] = make([]float32, semanticDimensions)
		}
		semanticAddDense(conceptSums[concept], vec, 1)
		conceptCounts[concept]++
	}
	for concept, vec := range conceptSums {
		semanticNormalize(vec)
		sem.ConceptVectors[concept] = semanticQuantize(vec)
	}

	nodeFloat := make(map[string][]float32, len(docs))
	for _, doc := range docs {
		vec := make([]float32, semanticDimensions)
		counts := map[string]int{}
		for _, token := range doc.Tokens {
			counts[token]++
		}
		for token, count := range counts {
			idf := math.Log(1 + float64(len(docs))/float64(1+docFreq[token]))
			weight := float32(idf * (1 + math.Log(float64(count))))
			semanticAddI8(vec, sem.TokenVectors[token], weight)
		}
		semanticNormalize(vec)
		nodeFloat[doc.Symbol.ID] = vec
	}

	neighbors := semanticNeighbors(snap)
	for _, doc := range docs {
		base := nodeFloat[doc.Symbol.ID]
		blended := append([]float32(nil), base...)
		for _, neighbor := range neighbors[doc.Symbol.ID] {
			if nv := nodeFloat[neighbor]; nv != nil {
				semanticAddDense(blended, nv, 0.18/float32(maxInt(1, len(neighbors[doc.Symbol.ID]))))
			}
		}
		semanticNormalize(blended)
		sem.Nodes = append(sem.Nodes, semanticNode{SymbolID: doc.Symbol.ID, Vector: semanticQuantize(blended), Concepts: doc.Concepts})
	}
	sort.Slice(sem.Nodes, func(i, j int) bool { return sem.Nodes[i].SymbolID < sem.Nodes[j].SymbolID })
	return sem
}

func semanticDocuments(snap indexSnapshot) []semanticDocument {
	ids := make([]string, 0, len(snap.Symbols))
	for id, sym := range snap.Symbols {
		if sym.Kind != "file" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	fileLines := map[string][]string{}
	neighbors := semanticNeighbors(snap)
	docs := make([]semanticDocument, 0, len(ids))
	for _, id := range ids {
		sym := snap.Symbols[id]
		identityParts := []string{sym.Name, sym.Kind, sym.Language, sym.File, sym.Signature, sym.Docstring}
		identityParts = append(identityParts, sym.ReturnTypes...)
		parts := append([]string(nil), identityParts...)
		lines, ok := fileLines[sym.File]
		if !ok {
			data, err := os.ReadFile(filepath.Join(snap.Repo.Root, filepath.FromSlash(sym.File)))
			if err == nil {
				lines = strings.Split(string(data), "\n")
			}
			fileLines[sym.File] = lines
		}
		if len(lines) > 0 {
			start, end := sym.StartLine-1, sym.EndLine
			if start < 0 {
				start = 0
			}
			if end > len(lines) {
				end = len(lines)
			}
			if end > start {
				parts = append(parts, strings.Join(lines[start:end], "\n"))
			}
		}
		for _, neighborID := range neighbors[id] {
			if neighbor, ok := snap.Symbols[neighborID]; ok {
				parts = append(parts, neighbor.Name, neighbor.Kind, neighbor.Signature)
			}
		}
		tokens := semanticTokens(strings.Join(parts, " "))
		if len(tokens) > semanticMaxDocTokens {
			tokens = tokens[:semanticMaxDocTokens]
		}
		// Coverage explanations deliberately use declaration identity only,
		// not every incidental word in a large function body. Body tokens
		// still shape the latent vector, but cannot manufacture a perfect
		// explicit-concept match for an unrelated symbol.
		conceptSet := map[string]bool{}
		for _, token := range semanticTokens(strings.Join(identityParts, " ")) {
			conceptSet[semanticCanonical(token)] = true
		}
		concepts := make([]string, 0, len(conceptSet))
		for concept := range conceptSet {
			concepts = append(concepts, concept)
		}
		sort.Strings(concepts)
		docs = append(docs, semanticDocument{Symbol: sym, Tokens: tokens, Concepts: concepts})
	}
	return docs
}

func semanticNeighbors(snap indexSnapshot) map[string][]string {
	out := map[string][]string{}
	seen := map[string]map[string]bool{}
	snap.forEachEdge(func(_ int, edge CGPEdge) bool {
		if edge.Confidence == ConfUnresolved || snap.Symbols[edge.From].ID == "" || snap.Symbols[edge.To].ID == "" {
			return true
		}
		if seen[edge.From] == nil {
			seen[edge.From] = map[string]bool{}
		}
		if seen[edge.To] == nil {
			seen[edge.To] = map[string]bool{}
		}
		if !seen[edge.From][edge.To] {
			out[edge.From] = append(out[edge.From], edge.To)
			seen[edge.From][edge.To] = true
		}
		if !seen[edge.To][edge.From] {
			out[edge.To] = append(out[edge.To], edge.From)
			seen[edge.To][edge.From] = true
		}
		return true
	})
	return out
}

func semanticTokens(text string) []string {
	raw := searchTokens(text)
	out := make([]string, 0, len(raw))
	for _, token := range raw {
		token = searchStem(token)
		if token == "" || len(token) < 2 || searchStopWords[token] {
			continue
		}
		out = append(out, token)
	}
	return out
}

var semanticCanonicalTerms = buildSemanticCanonicalTerms()

func buildSemanticCanonicalTerms() map[string]string {
	groups := [][]string{
		{"send", "publish", "post", "emit", "dispatch", "transmit", "deliver", "notify"},
		{"receive", "consume", "listen", "subscribe", "handle"},
		{"auth", "authenticate", "authentication", "login", "signin", "credential"},
		{"authorize", "authorization", "permission", "access", "privilege", "right", "policy"},
		{"validate", "verify", "check", "assert", "ensure"},
		{"create", "add", "insert", "new", "allocate"},
		{"update", "modify", "change", "edit", "patch"},
		{"delete", "remove", "destroy", "drop", "purge"},
		{"read", "get", "fetch", "load", "retrieve", "find", "lookup", "query"},
		{"write", "save", "store", "persist", "commit"},
		{"error", "exception", "failure", "fault", "panic"},
		{"message", "event", "notification", "signal"},
		{"alert", "warning", "alarm", "critical"},
		{"request", "input", "command"},
		{"response", "output", "result", "reply"},
		{"database", "db", "repository", "repo", "storage"},
		{"cache", "memoize", "buffer"},
		{"queue", "job", "task", "work"},
		{"route", "endpoint", "handler", "controller"},
		{"service", "manager", "provider"},
		{"config", "configuration", "setting", "option"},
		{"start", "initialize", "init", "open", "connect"},
		{"stop", "shutdown", "close", "disconnect", "dispose"},
		{"parse", "decode", "deserialize", "unmarshal"},
		{"format", "encode", "serialize", "marshal"},
		{"encrypt", "cipher", "secure", "hash"},
		{"decrypt", "decipher"},
		{"test", "spec", "assertion", "fixture"},
		{"log", "logging", "trace", "audit", "telemetry"},
		{"user", "account", "member", "customer"},
		{"document", "file", "resource", "asset"},
		{"list", "array", "collection", "slice", "vector"},
		{"map", "dictionary", "object", "record"},
		{"async", "concurrent", "parallel", "background"},
		{"retry", "repeat", "requeue", "backoff"},
		{"limit", "throttle", "rate", "quota"},
		{"search", "discover", "match", "resolve"},
	}
	out := map[string]string{}
	for _, group := range groups {
		canonical := group[0]
		for _, term := range group {
			out[term] = canonical
		}
	}
	return out
}

func semanticCanonical(token string) string {
	if canonical := semanticCanonicalTerms[token]; canonical != "" {
		return canonical
	}
	return token
}

func semanticAddBaseVector(dst []float32, token string, scale float32) {
	seed := semanticHash(token)
	for i := 0; i < semanticSparseEntries; i++ {
		seed += 0x9e3779b97f4a7c15
		z := seed
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		z ^= z >> 31
		pos := int(z % uint64(len(dst)))
		if z&1 == 0 {
			dst[pos] += scale
		} else {
			dst[pos] -= scale
		}
	}
}

func semanticHash(s string) uint64 {
	const offset = uint64(14695981039346656037)
	const prime = uint64(1099511628211)
	h := offset
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

func semanticNormalize(v []float32) {
	var mag float64
	for _, n := range v {
		mag += float64(n * n)
	}
	if mag == 0 {
		return
	}
	inv := float32(1 / math.Sqrt(mag))
	for i := range v {
		v[i] *= inv
	}
}

func semanticAddDense(dst, src []float32, scale float32) {
	for i := range dst {
		dst[i] += src[i] * scale
	}
}

func semanticAddI8(dst []float32, src []int8, scale float32) {
	for i := range dst {
		dst[i] += float32(src[i]) / 127 * scale
	}
}

func semanticQuantize(v []float32) []int8 {
	out := make([]int8, len(v))
	for i, n := range v {
		q := math.Round(float64(n * 127))
		if q > 127 {
			q = 127
		} else if q < -127 {
			q = -127
		}
		out[i] = int8(q)
	}
	return out
}

func semanticQueryVector(sem *semanticIndex, terms []string) []int8 {
	vec := make([]float32, sem.Dimensions)
	for _, term := range terms {
		if tv := sem.TokenVectors[term]; len(tv) == sem.Dimensions {
			semanticAddI8(vec, tv, 1)
			continue
		}
		if cv := sem.ConceptVectors[semanticCanonical(term)]; len(cv) == sem.Dimensions {
			semanticAddI8(vec, cv, 1)
			continue
		}
		semanticAddBaseVector(vec, semanticCanonical(term), 1)
	}
	semanticNormalize(vec)
	return semanticQuantize(vec)
}

func semanticCosineI8(a, b []int8) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, ma, mb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		ma += x * x
		mb += y * y
	}
	if ma == 0 || mb == 0 {
		return 0
	}
	return dot / math.Sqrt(ma*mb)
}

func semanticMatchedConcepts(terms, concepts []string) []string {
	set := make(map[string]bool, len(concepts))
	for _, concept := range concepts {
		set[concept] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, term := range terms {
		concept := semanticCanonical(term)
		if set[concept] && !seen[concept] {
			out = append(out, concept)
			seen[concept] = true
		}
	}
	sort.Strings(out)
	return out
}

func semanticUniqueConceptCount(terms []string) int {
	seen := map[string]bool{}
	for _, term := range terms {
		seen[semanticCanonical(term)] = true
	}
	return len(seen)
}

func semanticResultKind(kind string) bool {
	switch kind {
	case "function", "method", "class", "component", "callback", "getter", "setter", "constructor", "http-route", "http-endpoint", "ttl-shape", "ttl-term":
		return true
	default:
		return false
	}
}

func semanticFileHashes(snap indexSnapshot) map[string]string {
	out := map[string]string{}
	for path, file := range snap.Files {
		if searchableCodeLanguage(file.Language) {
			out[path] = file.SHA256
		}
	}
	return out
}

func saveSemanticSidecar(idx *Index, path string) error {
	idx.mu.Lock()
	sem := idx.semanticIndex
	idx.mu.Unlock()
	if sem == nil {
		return nil
	}
	var buf bytes.Buffer
	buf.Write(semanticSidecarMagic)
	if err := gob.NewEncoder(&buf).Encode(persistedSemanticIndex{SchemaVersion: SchemaVersion, Index: *sem}); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func loadSemanticSidecar(idx *Index, path string) *semanticIndex {
	data, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(data, semanticSidecarMagic) {
		return nil
	}
	var payload persistedSemanticIndex
	if gob.NewDecoder(bytes.NewReader(data[len(semanticSidecarMagic):])).Decode(&payload) != nil || payload.SchemaVersion != SchemaVersion {
		return nil
	}
	if payload.Index.Dimensions != semanticDimensions || !semanticHashesMatch(idx, payload.Index.FileHashes) {
		return nil
	}
	return &payload.Index
}

func semanticHashesMatch(idx *Index, hashes map[string]string) bool {
	snap := idx.snapshot()
	want := semanticFileHashes(snap)
	if len(want) != len(hashes) {
		return false
	}
	for path, hash := range want {
		if hashes[path] != hash {
			return false
		}
	}
	return true
}

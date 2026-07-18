package mamari

import (
	"sort"
	"strings"
)

const compactResultLimit = 50

func TraceTerm(idx *Index, query string, opts ...QueryOptions) TraceResponse {
	o := mergeOpts(opts)
	resolved := resolveQuery(idx, query)
	resp := TraceResponse{Status: resolved.status, Query: query, Candidates: []TermSummary{}, TTLUsages: []Location{}, CodeReferences: []Reference{}, Edges: []Edge{}}
	if len(resolved.candidates) > 0 {
		resp.Candidates = summaries(resolved.candidates)
	}
	if len(resolved.warnings) > 0 {
		resp.Warnings = append(resp.Warnings, resolved.warnings...)
	}
	if resolved.status == "ambiguous" || len(resolved.terms) == 0 {
		return resp
	}
	term := resolved.terms[0]
	summary := summarize(term)
	resp.Term = &summary
	relatedTerms := []Term{term}
	if term.IRI != "" {
		relatedTerms = idx.termsByIRI(term.IRI)
	}
	for _, related := range relatedTerms {
		resp.TTLUsages = append(resp.TTLUsages, related.Locations...)
	}
	resp.TTLUsages = dedupeLocations(resp.TTLUsages)
	snap := idx.snapshot()
	for _, ref := range snap.References {
		if ref.TermID == term.ID || (term.IRI != "" && ref.IRI == term.IRI) {
			if !o.IncludeWeak && ref.Confidence == "weak" {
				continue
			}
			resp.CodeReferences = append(resp.CodeReferences, ref)
		}
	}
	for _, edge := range snap.Edges {
		if edge.From == term.ID || edge.To == term.ID || strings.Contains(edge.To, term.ID) {
			resp.Edges = append(resp.Edges, edge)
		}
	}
	if len(resp.TTLUsages) == 0 && len(resp.CodeReferences) == 0 && len(resp.Edges) == 0 {
		resp.Status = "not_found"
	} else {
		resp.Status = "found"
	}
	return resp
}

func mergeOpts(opts []QueryOptions) QueryOptions {
	var o QueryOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	return o
}

func TraceTermCompact(idx *Index, query string, opts ...QueryOptions) TraceCompactResponse {
	trace := TraceTerm(idx, query, opts...)
	resp := TraceCompactResponse{
		Status:              trace.Status,
		Query:               trace.Query,
		Term:                trace.Term,
		Candidates:          trace.Candidates,
		TTLUsageCount:       len(trace.TTLUsages),
		CodeReferenceCount:  len(trace.CodeReferences),
		EdgeCount:           len(trace.Edges),
		DynamicIRICallCount: idx.dynamicIRICallCount(),
		TTLUsages:           make([]LocationBrief, 0, len(trace.TTLUsages)),
		CodeReferences:      make([]ReferenceBrief, 0, len(trace.CodeReferences)),
		Warnings:            trace.Warnings,
	}
	for _, loc := range trace.TTLUsages {
		if len(resp.TTLUsages) >= compactResultLimit {
			break
		}
		resp.TTLUsages = append(resp.TTLUsages, LocationBrief{File: loc.File, StartLine: loc.StartLine, StartColumn: loc.StartColumn, Kind: loc.Kind})
	}
	for _, ref := range trace.CodeReferences {
		if len(resp.CodeReferences) >= compactResultLimit {
			break
		}
		resp.CodeReferences = append(resp.CodeReferences, ReferenceBrief{File: ref.File, StartLine: ref.StartLine, StartColumn: ref.StartColumn, Confidence: ref.Confidence, Kind: ref.Kind})
	}
	return resp
}

func TraceTermGroupedCompact(idx *Index, query string, opts ...QueryOptions) TraceGroupedCompactResponse {
	trace := TraceTerm(idx, query, opts...)
	resp := TraceGroupedCompactResponse{
		Status:              trace.Status,
		Query:               trace.Query,
		Term:                trace.Term,
		Candidates:          trace.Candidates,
		TTLUsageCount:       len(trace.TTLUsages),
		CodeReferenceCount:  len(trace.CodeReferences),
		EdgeCount:           len(trace.Edges),
		DynamicIRICallCount: idx.dynamicIRICallCount(),
		TTLUsages:           map[string][]GroupedLocation{},
		CodeReferences:      map[string][]GroupedReference{},
		Warnings:            trace.Warnings,
	}
	for _, loc := range trace.TTLUsages {
		if groupedCount(resp.TTLUsages) >= compactResultLimit {
			break
		}
		resp.TTLUsages[loc.File] = append(resp.TTLUsages[loc.File], GroupedLocation{Line: loc.StartLine, Column: loc.StartColumn, Kind: loc.Kind})
	}
	for _, ref := range trace.CodeReferences {
		if groupedCount(resp.CodeReferences) >= compactResultLimit {
			break
		}
		resp.CodeReferences[ref.File] = append(resp.CodeReferences[ref.File], GroupedReference{Line: ref.StartLine, Column: ref.StartColumn, Confidence: ref.Confidence, Kind: ref.Kind})
	}
	return resp
}

func groupedCount[T any](m map[string][]T) int {
	n := 0
	for _, values := range m {
		n += len(values)
	}
	return n
}

// FindReferences resolves query against the RDF term graph first. If no RDF
// term matches, it falls back to the CGP symbol graph (the same resolver
// trace-symbol uses) so identifiers that only exist as code symbols (e.g.
// "useAuth") still return useful results instead of "not_found".
func FindReferences(idx *Index, query string, opts ...QueryOptions) FindReferencesResponse {
	trace := TraceTerm(idx, query, opts...)
	resp := FindReferencesResponse{
		Status:     trace.Status,
		Query:      trace.Query,
		Term:       trace.Term,
		Candidates: trace.Candidates,
		References: trace.CodeReferences,
		Warnings:   trace.Warnings,
	}
	if resp.Status != "not_found" {
		return resp
	}
	matches := findSymbols(idx, query)
	switch {
	case len(matches) == 0:
		return resp
	case len(matches) > 1:
		resp.Status = "ambiguous"
		resp.SymbolCandidates = summarizeSymbols(matches)
		// Reuse trace-symbol's inline candidate-detail expansion instead of
		// forcing the caller to disambiguate and re-query once per
		// candidate — see TraceSymbolWithOptions/maxAmbiguousTraceDetails.
		ambiguousTrace := TraceSymbolWithOptions(idx, query, TraceSymbolOptions{Sites: true})
		resp.SymbolCandidateDetails = ambiguousTrace.CandidateDetails
		return resp
	}
	symTrace := TraceSymbolWithOptions(idx, query, TraceSymbolOptions{Sites: true})
	resp.Status = "found"
	if symTrace.Symbol != nil {
		summary := summarizeSymbol(*symTrace.Symbol)
		resp.Symbol = &summary
	}
	resp.Callers = symTrace.Callers
	resp.Callees = symTrace.Callees
	for _, site := range symTrace.CallerSites {
		resp.References = append(resp.References, Reference{
			File:        site.File,
			StartLine:   site.Line,
			StartColumn: site.Column,
			Confidence:  site.Confidence,
			Kind:        "call",
			Context:     site.Raw,
		})
	}
	resp.Warnings = append(resp.Warnings, "resolved via CGP symbol graph (no matching RDF term)")
	return resp
}

func SearchLiteral(idx *Index, query, lang string) SearchLiteralResponse {
	resp := SearchLiteralResponse{Status: "ok", Query: query, Lang: lang, Hits: []Literal{}}
	if query == "" {
		return resp
	}
	if err := idx.ensureLiteralsLoaded(); err != nil {
		resp.Status = "error"
		return resp
	}
	idx.mu.Lock()
	literals := append([]Literal(nil), idx.Literals...)
	idx.mu.Unlock()
	needle := strings.ToLower(query)
	for _, lit := range literals {
		if lang != "" && lit.Lang != lang {
			continue
		}
		if !strings.Contains(strings.ToLower(lit.Value), needle) {
			continue
		}
		resp.Hits = append(resp.Hits, lit)
	}
	sortSearchLiteralHits(resp.Hits, needle)
	return resp
}

// sortSearchLiteralHits ranks literal matches by relevance instead of plain
// file:line order. SearchLiteral matches a single substring against literal
// values rather than scoring multiple weighted terms against documents, so
// the BM25 term-frequency/IDF model used by SearchCode doesn't directly
// apply here — the relevant analog is: exact-value matches first, then
// closer (shorter) matches over incidental substring hits inside long
// values, then non-test evidence before test fixtures, with file:line as a
// stable final tiebreaker.
func sortSearchLiteralHits(hits []Literal, needleLower string) {
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		aExact := strings.ToLower(a.Value) == needleLower
		bExact := strings.ToLower(b.Value) == needleLower
		if aExact != bExact {
			return aExact
		}
		if len(a.Value) != len(b.Value) {
			return len(a.Value) < len(b.Value)
		}
		aTest, bTest := isTestPath(a.Location.File), isTestPath(b.Location.File)
		if aTest != bTest {
			return !aTest
		}
		if a.Location.File != b.Location.File {
			return a.Location.File < b.Location.File
		}
		return a.Location.StartLine < b.Location.StartLine
	})
}

func FindContainingShape(idx *Index, query string) FindContainingShapeResponse {
	resp := FindContainingShapeResponse{Status: "ok", Query: query, Containers: []Shape{}}
	q := strings.TrimSpace(query)
	if q == "" {
		return resp
	}
	iri := ""
	if strings.HasPrefix(q, "http://") || strings.HasPrefix(q, "https://") {
		iri = q
	} else if prefix, local := splitTerm(q); prefix != "" && local != "" {
		if base, ok := idx.prefixIRI(prefix); ok {
			iri = base + local
		}
	}
	matches := func(term, termIRI string) bool {
		if term == q {
			return true
		}
		if iri != "" && termIRI == iri {
			return true
		}
		return false
	}
	seen := map[string]bool{}
	snap := idx.snapshot()
	for _, shape := range snap.Shapes {
		matched := false
		if matches(shape.Term, shape.IRI) {
			matched = true
		}
		if !matched {
			for _, p := range shape.Paths {
				if matches(p.Term, p.IRI) {
					matched = true
					break
				}
			}
		}
		if !matched {
			for _, n := range shape.Nodes {
				if matches(n.Term, n.IRI) {
					matched = true
					break
				}
			}
		}
		if !matched {
			for _, t := range shape.TargetClasses {
				if matches(t.Term, t.IRI) {
					matched = true
					break
				}
			}
		}
		if !matched {
			for _, p := range shape.Predicates {
				if p.Predicate == q || matches(p.Term, p.IRI) {
					matched = true
					break
				}
				if iri != "" {
					if pIRI := idx.ResolveTerm(p.Predicate); pIRI == iri {
						matched = true
						break
					}
				}
			}
		}
		if !matched {
			for _, b := range shape.Branches {
				if matches(b.Datatype, b.DatatypeIRI) || matches(b.Path, b.PathIRI) {
					matched = true
					break
				}
			}
		}
		if matched && !seen[shape.ID] {
			seen[shape.ID] = true
			resp.Containers = append(resp.Containers, shape)
		}
	}
	sort.Slice(resp.Containers, func(i, j int) bool {
		if resp.Containers[i].Location.File != resp.Containers[j].Location.File {
			return resp.Containers[i].Location.File < resp.Containers[j].Location.File
		}
		return resp.Containers[i].Location.StartLine < resp.Containers[j].Location.StartLine
	})
	return resp
}

func ListDynamicIRIs(idx *Index, file string) ListDynamicIRIsResponse {
	resp := ListDynamicIRIsResponse{Status: "ok", File: file, Calls: []DynamicIRICall{}}
	idx.mu.Lock()
	calls := append([]DynamicIRICall(nil), idx.DynamicIRICalls...)
	idx.mu.Unlock()
	if file == "" {
		resp.Calls = append(resp.Calls, calls...)
		return resp
	}
	for _, c := range calls {
		if c.File == file {
			resp.Calls = append(resp.Calls, c)
		}
	}
	return resp
}

func ListTerms(idx *Index, prefix string) ListTermsResponse {
	var terms []Term
	snap := idx.snapshot()
	for _, term := range snap.Terms {
		if prefix == "" || term.Prefix == prefix {
			terms = append(terms, term)
		}
	}
	sort.Slice(terms, func(i, j int) bool { return terms[i].Term < terms[j].Term })
	return ListTermsResponse{Status: "ok", Prefix: prefix, Terms: summaries(terms)}
}

type resolvedQuery struct {
	status     string
	terms      []Term
	candidates []Term
	warnings   []string
}

func resolveQuery(idx *Index, query string) resolvedQuery {
	query = strings.TrimSpace(query)
	if query == "" {
		return resolvedQuery{status: "invalid"}
	}
	if strings.HasPrefix(query, "http://") || strings.HasPrefix(query, "https://") {
		terms := idx.termsByIRI(query)
		if len(terms) == 0 {
			if compact := idx.CompactIRI(query); compact != "" {
				prefix, local := splitTerm(compact)
				return resolvedQuery{status: "not_found", terms: []Term{{ID: "term:" + compact, Term: compact, IRI: query, Prefix: prefix, LocalName: local}}}
			}
			return resolvedQuery{status: "not_found"}
		}
		if len(terms) > 1 {
			ranked, warnings := rankTermsByIRIShape(terms)
			return resolvedQuery{status: "ambiguous", candidates: ranked, warnings: warnings}
		}
		return resolvedQuery{status: "found", terms: terms}
	}
	if prefix, local := splitTerm(query); prefix != "" && local != "" {
		terms := idx.termsByTerm(query)
		if len(terms) > 1 {
			ranked, warnings := rankTermsByIRIShape(terms)
			return resolvedQuery{status: "ambiguous", candidates: ranked, warnings: warnings}
		}
		if len(terms) == 1 {
			return resolvedQuery{status: "found", terms: terms}
		}
		id := "term:" + query
		if base, ok := idx.prefixIRI(prefix); ok {
			return resolvedQuery{status: "not_found", terms: []Term{{ID: id, Term: query, IRI: base + local, Prefix: prefix, LocalName: local}}}
		}
		return resolvedQuery{status: "not_found"}
	}
	terms := idx.termsByLocal(query)
	if len(terms) == 0 {
		return resolvedQuery{status: "not_found"}
	}
	if len(terms) > 1 {
		ranked, warnings := rankTermsByIRIShape(terms)
		return resolvedQuery{status: "ambiguous", candidates: ranked, warnings: warnings}
	}
	return resolvedQuery{status: "found", terms: terms}
}

// rankTermsByIRIShape orders an ambiguous candidate list so well-formed IRIs
// (where the local name is separated from the namespace by '#', '/', or ':')
// appear before malformed ones (where the local name flush-concatenates onto
// the namespace, e.g. "http://example.org/ns/customhideIf"). When at least one
// malformed candidate is present, a warning is returned so the agent can see
// that one prefix declaration in the repo looks broken. Status remains
// "ambiguous" — we don't auto-pick — but the agent can rely on element 0
// being the canonical choice.
func rankTermsByIRIShape(terms []Term) ([]Term, []string) {
	if len(terms) <= 1 {
		return terms, nil
	}
	ranked := make([]Term, len(terms))
	copy(ranked, terms)
	sort.SliceStable(ranked, func(i, j int) bool {
		li := isWellFormedIRI(ranked[i].IRI, ranked[i].LocalName)
		lj := isWellFormedIRI(ranked[j].IRI, ranked[j].LocalName)
		if li != lj {
			return li
		}
		return ranked[i].IRI < ranked[j].IRI
	})
	var warnings []string
	for _, t := range ranked {
		if !isWellFormedIRI(t.IRI, t.LocalName) {
			warnings = append(warnings, "ambiguous candidate "+t.IRI+" looks like a missing-separator prefix declaration; canonical "+wellFormedSiblingIRI(ranked, t)+" is preferred")
		}
	}
	if len(warnings) == 0 {
		return ranked, nil
	}
	return ranked, warnings
}

// isWellFormedIRI returns true when the IRI separates its local name from the
// namespace with '#', '/', or ':'. Returns true for IRIs whose local name is
// empty or absent — we only flag the specific case where the local name
// concatenates flush onto a namespace base.
func isWellFormedIRI(iri, local string) bool {
	if local == "" || iri == "" {
		return true
	}
	if !strings.HasSuffix(iri, local) {
		return true
	}
	base := iri[:len(iri)-len(local)]
	if base == "" {
		return true
	}
	last := base[len(base)-1]
	return last == '#' || last == '/' || last == ':'
}

func wellFormedSiblingIRI(terms []Term, malformed Term) string {
	for _, t := range terms {
		if t.LocalName == malformed.LocalName && isWellFormedIRI(t.IRI, t.LocalName) {
			return t.IRI
		}
	}
	return malformed.IRI
}

func dedupeLocations(locations []Location) []Location {
	seen := map[string]bool{}
	out := make([]Location, 0, len(locations))
	for _, loc := range locations {
		key := loc.File + ":" + intKey(loc.StartLine) + ":" + intKey(loc.StartColumn)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, loc)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].StartLine != out[j].StartLine {
			return out[i].StartLine < out[j].StartLine
		}
		return out[i].StartColumn < out[j].StartColumn
	})
	return out
}

func intKey(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	return string(buf[i:])
}

func summarize(term Term) TermSummary {
	return TermSummary{ID: term.ID, Term: term.Term, IRI: term.IRI, Prefix: term.Prefix, LocalName: term.LocalName}
}

func summaries(terms []Term) []TermSummary {
	out := make([]TermSummary, 0, len(terms))
	for _, term := range terms {
		out = append(out, summarize(term))
	}
	return out
}

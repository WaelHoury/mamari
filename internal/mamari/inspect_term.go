package mamari

import (
	"sort"
	"strings"
)

const (
	defaultInspectTermBudgetTokens = 1200
	defaultInspectTermContextLines = 6
	defaultInspectTermLimit        = 8
)

// InspectTerm is the RDF/source bridge workflow. It keeps term discovery
// separate from symbol inspection, but packages the common agent sequence:
// trace grouped term refs, surface likely implementation identifiers, then
// return budgeted source slices around the strongest code evidence.
func InspectTerm(idx *Index, query string, opts InspectTermOptions) (InspectTermResponse, error) {
	if opts.BudgetTokens <= 0 {
		opts.BudgetTokens = defaultInspectTermBudgetTokens
	}
	if opts.ContextLines <= 0 {
		opts.ContextLines = defaultInspectTermContextLines
	}
	if opts.Limit <= 0 {
		opts.Limit = defaultInspectTermLimit
	}
	mode := normalizeMode(opts.Mode)
	resp := InspectTermResponse{Status: "not_found", Query: query}

	trace := TraceTermGroupedCompact(idx, query, QueryOptions{IncludeWeak: opts.IncludeWeak})
	resp.Trace = trace
	resp.Term = trace.Term
	resp.Candidates = trace.Candidates
	if trace.Status != "found" {
		resp.Status = trace.Status
		resp.Warnings = append(resp.Warnings, trace.Warnings...)
		return resp, nil
	}
	resp.Status = "ok"
	resp.Warnings = append(resp.Warnings, trace.Warnings...)

	term := ""
	local := ""
	prefix := ""
	if trace.Term != nil {
		term = trace.Term.Term
		local = trace.Term.LocalName
		prefix = trace.Term.Prefix
	}
	implHits := termImplementationHits(idx, prefix, local, opts)
	resp.Implementation = implHits

	contextQueries := inspectTermContextQueries(trace, implHits, opts.Limit)
	if len(contextQueries) == 0 {
		resp.Warnings = append(resp.Warnings, "no implementation context candidates found for "+term)
		return resp, nil
	}
	context, err := fetchContextForQueries(idx, contextQueries, FetchContextOptions{
		BudgetTokens: opts.BudgetTokens,
		ContextLines: opts.ContextLines,
		Mode:         mode,
	})
	if err != nil {
		return resp, err
	}
	resp.Context = context
	resp.EstimatedTokens = context.EstimatedTokens
	return resp, nil
}

func termImplementationHits(idx *Index, prefix, local string, opts InspectTermOptions) []SearchCodeHit {
	idents := derivedTermIdentifiers(prefix, local)
	if len(idents) == 0 {
		return nil
	}
	idx.ensureFileSymbolIndex()
	files := idx.ensureCodeSearchIndex()
	var hits []SearchCodeHit
	for _, file := range files {
		if file.language == "ttl" || shouldExcludeNoisyFile(file.file, ListSymbolsOptions{}) {
			continue
		}
		for lineIndex, line := range file.lines {
			lineNumber := lineIndex + 1
			lineText := file.lineText(line)
			matches := exactIdentifierMatches(lineText, idents)
			if len(matches) == 0 {
				continue
			}
			score := 500 + len(matches)*120
			if isSearchDefinitionLine(file, line, lineNumber) {
				score += 180
			}
			text := lineText
			hits = append(hits, SearchCodeHit{
				File:            file.file,
				StartLine:       lineNumber,
				EndLine:         lineNumber,
				FocusLine:       lineNumber,
				Score:           score,
				EstimatedTokens: EstimateTokens(text),
				MatchedExact:    matches,
				Symbols:         dereferenceSymbolSummaries(file.lineSymbols(line, nil)),
				Text:            text,
			})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		if hits[i].File != hits[j].File {
			return hits[i].File < hits[j].File
		}
		return hits[i].FocusLine < hits[j].FocusLine
	})
	if len(hits) > opts.Limit {
		hits = hits[:opts.Limit]
	}
	return hits
}

func derivedTermIdentifiers(prefix, local string) []string {
	if local == "" {
		return nil
	}
	localIdent := sanitizeIdentifierPart(local)
	if localIdent == "" {
		return nil
	}
	prefixes := []string{sanitizeIdentifierPart(prefix)}
	if alias := semanticPrefixAlias(prefix); alias != "" && alias != prefixes[0] {
		prefixes = append(prefixes, alias)
	}
	seen := map[string]bool{}
	add := func(s string, out *[]string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		*out = append(*out, s)
	}
	var out []string
	if len(localIdent) > 2 && !searchStopWords[strings.ToLower(localIdent)] {
		add(localIdent, &out)
		add(lowerFirst(localIdent), &out)
	}
	for _, p := range prefixes {
		if p == "" {
			continue
		}
		add(p+upperFirst(localIdent), &out)
		add(strings.ToUpper(p)+"_"+strings.ToUpper(localIdent), &out)
	}
	return out
}

func exactIdentifierMatches(text string, idents []string) []string {
	var out []string
	for _, ident := range idents {
		if ident == "" || !strings.Contains(text, ident) {
			continue
		}
		out = append(out, ident)
	}
	return out
}

func sanitizeIdentifierPart(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	upperNext := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			if upperNext {
				b.WriteString(strings.ToUpper(string(r)))
				upperNext = false
			} else {
				b.WriteRune(r)
			}
			continue
		}
		upperNext = true
	}
	return strings.Trim(b.String(), "_")
}

func semanticPrefixAlias(prefix string) string {
	switch strings.ToLower(prefix) {
	case "sh":
		return "shacl"
	case "rdf":
		return "rdf"
	case "rdfs":
		return "rdfs"
	case "xsd":
		return "xsd"
	case "dct", "dcterms":
		return "dcterms"
	case "dcat":
		return "dcat"
	case "dcatap":
		return "dcatap"
	default:
		return sanitizeIdentifierPart(prefix)
	}
}

func upperFirst(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func lowerFirst(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func inspectTermContextQueries(trace TraceGroupedCompactResponse, hits []SearchCodeHit, limit int) []string {
	seen := map[string]bool{}
	var out []string
	add := func(file string, line int) {
		if file == "" || line <= 0 || len(out) >= limit {
			return
		}
		q := file + ":" + intString(line)
		if seen[q] {
			return
		}
		seen[q] = true
		out = append(out, q)
	}
	var files []string
	for file := range trace.CodeReferences {
		files = append(files, file)
	}
	sort.Strings(files)
	for _, file := range files {
		refs := append([]GroupedReference(nil), trace.CodeReferences[file]...)
		sort.SliceStable(refs, func(i, j int) bool {
			if refs[i].Line != refs[j].Line {
				return refs[i].Line < refs[j].Line
			}
			return refs[i].Column < refs[j].Column
		})
		for _, ref := range refs {
			add(file, ref.Line)
		}
	}
	for _, hit := range hits {
		add(hit.File, hit.FocusLine)
	}
	return out
}

func fetchContextForQueries(idx *Index, queries []string, opts FetchContextOptions) (FetchContextResponse, error) {
	resp := FetchContextResponse{
		Status:       "not_found",
		Query:        strings.Join(queries, ","),
		BudgetTokens: opts.BudgetTokens,
		Slices:       []ContextSlice{},
	}
	seen := map[string]bool{}
	for _, q := range queries {
		if resp.EstimatedTokens >= opts.BudgetTokens {
			resp.Truncated = true
			break
		}
		next, err := FetchContext(idx, q, FetchContextOptions{
			BudgetTokens: opts.BudgetTokens - resp.EstimatedTokens,
			ContextLines: opts.ContextLines,
			Mode:         opts.Mode,
		})
		if err != nil {
			return resp, err
		}
		resp.Warnings = append(resp.Warnings, next.Warnings...)
		for _, slice := range next.Slices {
			key := sliceKey(slice)
			if seen[key] {
				continue
			}
			seen[key] = true
			resp.Slices = append(resp.Slices, slice)
			resp.EstimatedTokens += slice.EstimatedTokens
			if resp.EstimatedTokens >= opts.BudgetTokens {
				resp.Truncated = true
				break
			}
		}
	}
	if len(resp.Slices) > 0 {
		resp.Status = "ok"
	}
	return resp, nil
}

func intString(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	n := v
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

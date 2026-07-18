package mamari

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	defaultInspectSymbolNodeBudget = 900
	defaultInspectSymbolNodeLines  = 140
)

type inspectSymbolNodeJSON struct {
	Status           string                    `json:"status"`
	Query            string                    `json:"query,omitempty"`
	Symbol           *nodeSymbolSummaryJSON    `json:"symbol,omitempty"`
	Candidates       []nodeSymbolSummaryJSON   `json:"candidates,omitempty"`
	CandidateDetails []nodeCandidateDetailJSON `json:"candidateDetails,omitempty"`
	Source           string                    `json:"source,omitempty"`
	Callers          []nodeSymbolSummaryJSON   `json:"callers,omitempty"`
	Callees          []nodeSymbolSummaryJSON   `json:"callees,omitempty"`
	CallerSites      []nodeCallSiteJSON        `json:"callerSites,omitempty"`
	Tests            []nodeSymbolSummaryJSON   `json:"tests,omitempty"`
	EstimatedTokens  int                       `json:"estimatedTokens"`
	Truncated        bool                      `json:"truncated,omitempty"`
	Warnings         []string                  `json:"warnings,omitempty"`
}

// nodeCandidateDetailJSON is the compact "node" rendering of
// TraceSymbolCandidateDetail — mirrors the token-conscious summaries used
// elsewhere in this file rather than the fuller CGPSymbolSummary shape.
type nodeCandidateDetailJSON struct {
	Symbol           *nodeSymbolSummaryJSON  `json:"symbol,omitempty"`
	Callers          []nodeSymbolSummaryJSON `json:"callers,omitempty"`
	CallerConfidence CGPConfidenceSummary    `json:"callerConfidence"`
}

func compactNodeCandidateDetails(in []TraceSymbolCandidateDetail) []nodeCandidateDetailJSON {
	if len(in) == 0 {
		return nil
	}
	out := make([]nodeCandidateDetailJSON, 0, len(in))
	for _, detail := range in {
		item := nodeCandidateDetailJSON{
			Callers:          compactNodeSymbolSummaries(detail.Callers, false),
			CallerConfidence: detail.CallerConfidence,
		}
		if detail.Symbol != nil {
			sym := compactNodeSymbolSummary(*detail.Symbol, true)
			item.Symbol = &sym
		}
		out = append(out, item)
	}
	return out
}

type nodeSymbolSummaryJSON struct {
	ID           string   `json:"id,omitempty"`
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	File         string   `json:"file"`
	StartLine    int      `json:"startLine"`
	Signature    string   `json:"signature,omitempty"`
	Docstring    string   `json:"docstring,omitempty"`
	ReturnTypes  []string `json:"returnTypes,omitempty"`
	Complexity   int      `json:"complexity,omitempty"`
	Count        int      `json:"count,omitempty"`
	Lines        []int    `json:"lines,omitempty"`
	NamesPreview []string `json:"namesPreview,omitempty"`
	Truncated    bool     `json:"truncated,omitempty"`
}

type nodeCallSiteJSON struct {
	File       string `json:"file"`
	Line       int    `json:"line"`
	Raw        string `json:"raw,omitempty"`
	Caller     string `json:"caller,omitempty"`
	Confidence string `json:"confidence,omitempty"`
}

func (resp InspectSymbolNodeResponse) MarshalJSON() ([]byte, error) {
	out := inspectSymbolNodeJSON{
		Status:           resp.Status,
		Query:            resp.Query,
		Candidates:       compactNodeSymbolSummaries(resp.Candidates, true),
		CandidateDetails: compactNodeCandidateDetails(resp.CandidateDetails),
		Source:           resp.Source,
		Callers:          compactNodeSymbolSummaries(resp.Callers, false),
		Callees:          compactNodeSymbolSummaries(resp.Callees, false),
		CallerSites:      compactNodeCallSites(resp.CallerSites),
		Tests:            compactNodeSymbolSummaries(resp.Tests, false),
		EstimatedTokens:  resp.EstimatedTokens,
		Truncated:        resp.Truncated,
		Warnings:         resp.Warnings,
	}
	if resp.Symbol != nil {
		sym := compactNodeSymbolSummary(*resp.Symbol, true)
		out.Symbol = &sym
	}
	return json.Marshal(out)
}

func compactNodeSymbolSummaries(in []CGPSymbolSummary, includeDetails bool) []nodeSymbolSummaryJSON {
	if len(in) == 0 {
		return nil
	}
	out := make([]nodeSymbolSummaryJSON, 0, len(in))
	for _, sym := range in {
		out = append(out, compactNodeSymbolSummary(sym, includeDetails))
	}
	return out
}

func compactNodeSymbolSummary(sym CGPSymbolSummary, includeDetails bool) nodeSymbolSummaryJSON {
	out := nodeSymbolSummaryJSON{
		Name:      sym.Name,
		Kind:      sym.Kind,
		File:      sym.File,
		StartLine: sym.StartLine,
		Count:     sym.Count,
		Truncated: sym.Truncated,
	}
	if includeDetails {
		out.ID = sym.ID
		out.Signature = sym.Signature
		out.Docstring = sym.Docstring
		out.ReturnTypes = append([]string(nil), sym.ReturnTypes...)
		out.Complexity = sym.Complexity
		out.Lines = append([]int(nil), sym.Lines...)
		out.NamesPreview = append([]string(nil), sym.NamesPreview...)
	}
	return out
}

func compactNodeCallSites(in []CGPCallSite) []nodeCallSiteJSON {
	if len(in) == 0 {
		return nil
	}
	out := make([]nodeCallSiteJSON, 0, len(in))
	for _, site := range in {
		out = append(out, nodeCallSiteJSON{
			File:       site.File,
			Line:       site.Line,
			Raw:        site.Raw,
			Caller:     site.Caller,
			Confidence: site.Confidence,
		})
	}
	return out
}

func InspectSymbol(idx *Index, query string, opts InspectSymbolOptions) (InspectSymbolResponse, error) {
	resp := InspectSymbolResponse{Status: "not_found", Query: query}
	matches := findSymbols(idx, query)
	if len(matches) == 0 {
		return resp, nil
	}
	if len(matches) > 1 {
		resp.Status = "ambiguous"
		resp.Candidates = summarizeSymbols(matches)
		resp.Warnings = append(resp.Warnings, "query matched multiple symbols; pass a symbol id, file:name, kind-filtered query, or exact id")
		// Reuse trace-symbol's inline candidate-detail expansion instead of
		// forcing the caller to disambiguate and re-query once per
		// candidate — see TraceSymbolWithOptions/maxAmbiguousTraceDetails.
		ambiguousTrace := TraceSymbolWithOptions(idx, query, TraceSymbolOptions{Sites: true})
		resp.CandidateDetails = ambiguousTrace.CandidateDetails
		return resp, nil
	}

	sym := matches[0]
	resp.Status = "ok"
	resp.Symbol = &sym
	trace := TraceSymbolWithOptions(idx, sym.ID, TraceSymbolOptions{
		Sites:              true,
		WithEdges:          opts.WithEdges,
		IncludeTestDetails: opts.IncludeTestDetails,
		ExcludeTests:       !opts.IncludeTests,
	})
	resp.Trace = trace

	context, err := FetchContext(idx, sym.ID, FetchContextOptions{
		BudgetTokens:   opts.BudgetTokens,
		ContextLines:   opts.ContextLines,
		IncludeCallers: true,
		IncludeCallees: true,
		Mode:           opts.Mode,
	})
	if err != nil {
		return resp, err
	}
	resp.Context = context
	resp.EstimatedTokens = context.EstimatedTokens
	resp.Frontend = frontendEdgesForSymbol(idx, sym.ID)
	if notes, err := ListNotes(idx.Repo.Root, sym.ID); err == nil && len(notes.Notes) > 0 {
		resp.Notes = notes.Notes
	}
	return resp, nil
}

func InspectSymbolNode(idx *Index, query string, opts InspectSymbolNodeOptions) (InspectSymbolNodeResponse, error) {
	resp := InspectSymbolNodeResponse{Status: "not_found", Query: query}
	matches := findSymbols(idx, query)
	if len(matches) == 0 {
		return resp, nil
	}
	if len(matches) > 1 {
		resp.Status = "ambiguous"
		resp.Candidates = summarizeSymbols(matches)
		resp.Warnings = append(resp.Warnings, "query matched multiple symbols; pass a symbol id, file:name, kind-filtered query, or exact id")
		// Reuse trace-symbol's inline candidate-detail expansion instead of
		// forcing the caller to disambiguate and re-query once per
		// candidate — see TraceSymbolWithOptions/maxAmbiguousTraceDetails.
		// Compact: true is a pure internal efficiency win here, not a
		// response-shape change — InspectSymbolNodeResponse's own
		// MarshalJSON (compactNodeCandidateDetails/compactNodeSymbolSummary)
		// already drops Signature/Docstring/etc from non-detail entries
		// regardless, so this just avoids building and discarding that data.
		ambiguousTrace := TraceSymbolWithOptions(idx, query, TraceSymbolOptions{Sites: true, Compact: true})
		resp.CandidateDetails = ambiguousTrace.CandidateDetails
		return resp, nil
	}
	if opts.BudgetTokens <= 0 {
		opts.BudgetTokens = defaultInspectSymbolNodeBudget
	}
	if opts.SourceLines <= 0 {
		opts.SourceLines = defaultInspectSymbolNodeLines
	}

	sym := matches[0]
	summary := summarizeSymbol(sym)
	resp.Status = "ok"
	resp.Symbol = &summary
	resp.Docstring = sym.Docstring
	resp.ReturnTypes = append([]string(nil), sym.ReturnTypes...)

	// Compact: true here too — see the ambiguous-candidates call above for
	// why this changes nothing about the actual JSON output.
	trace := TraceSymbolWithOptions(idx, sym.ID, TraceSymbolOptions{
		Sites:              true,
		IncludeTestDetails: opts.IncludeTestDetails,
		ExcludeTests:       !opts.IncludeTests,
		Compact:            true,
	})
	for _, caller := range trace.Callers {
		if caller.Kind == "test-callback-group" || isTestPath(caller.File) {
			resp.Tests = append(resp.Tests, caller)
			continue
		}
		resp.Callers = append(resp.Callers, caller)
	}
	resp.Callees = trace.Callees
	resp.CallerSites = trace.CallerSites
	source, truncated, err := compactSymbolSource(idx, sym, opts.SourceLines, opts.BudgetTokens)
	if err != nil {
		resp.Warnings = append(resp.Warnings, err.Error())
	} else {
		resp.Source = source
		resp.Truncated = truncated
	}
	resp.EstimatedTokens = EstimateTokens(inspectSymbolNodeTokenText(resp))
	return resp, nil
}

func compactSymbolSource(idx *Index, sym CGPSymbol, maxLines, budgetTokens int) (string, bool, error) {
	if sym.File == "" || sym.StartLine <= 0 || sym.EndLine < sym.StartLine {
		return "", false, nil
	}
	idx.mu.Lock()
	root := idx.Repo.Root
	idx.mu.Unlock()
	data, err := os.ReadFile(filepath.Join(root, sym.File))
	if err != nil {
		return "", false, err
	}
	lines := strings.SplitAfter(string(data), "\n")
	start := sym.StartLine
	end := sym.EndLine
	if start > len(lines) {
		return "", false, nil
	}
	if end > len(lines) {
		end = len(lines)
	}
	truncated := false
	if maxLines > 0 && end-start+1 > maxLines {
		end = start + maxLines - 1
		truncated = true
	}
	source := strings.Join(lines[start-1:end], "")
	for budgetTokens > 0 && EstimateTokens(source) > budgetTokens && end > start {
		end--
		truncated = true
		source = strings.Join(lines[start-1:end], "")
	}
	if truncated {
		source = strings.TrimRight(source, "\r\n") + "\n// ... truncated\n"
	}
	return source, truncated, nil
}

func inspectSymbolNodeTokenText(resp InspectSymbolNodeResponse) string {
	var b strings.Builder
	if resp.Symbol != nil {
		b.WriteString(resp.Symbol.Signature)
		b.WriteByte('\n')
		b.WriteString(resp.Symbol.Docstring)
		b.WriteByte('\n')
	}
	b.WriteString(resp.Source)
	for _, caller := range resp.Callers {
		b.WriteString(caller.File)
		b.WriteByte(':')
		b.WriteString(caller.Name)
		b.WriteByte('\n')
	}
	for _, callee := range resp.Callees {
		b.WriteString(callee.File)
		b.WriteByte(':')
		b.WriteString(callee.Name)
		b.WriteByte('\n')
	}
	for _, test := range resp.Tests {
		b.WriteString(test.File)
		b.WriteByte(':')
		b.WriteString(test.Name)
		b.WriteByte('\n')
	}
	return b.String()
}

func frontendEdgesForSymbol(idx *Index, symbolID string) []CGPEdge {
	snap := idx.symbolGraphSnapshot() // reads only SymbolEdges
	var out []CGPEdge
	snap.forEachEdge(func(index int, edge CGPEdge) bool {
		if edge.From != symbolID && edge.To != symbolID {
			return true
		}
		switch edge.Type {
		case "renders-component", "passes-prop", "binds-model", "listens-event", "handles-route", "calls-http-route":
			edge = snap.edgeAtWithID(index)
			out = append(out, edge)
		}
		return true
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Evidence.File != out[j].Evidence.File {
			return out[i].Evidence.File < out[j].Evidence.File
		}
		if out[i].Evidence.StartLine != out[j].Evidence.StartLine {
			return out[i].Evidence.StartLine < out[j].Evidence.StartLine
		}
		if out[i].Evidence.StartColumn != out[j].Evidence.StartColumn {
			return out[i].Evidence.StartColumn < out[j].Evidence.StartColumn
		}
		return out[i].ID < out[j].ID
	})
	return out
}

package mamari

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	defaultInspectFlowLimit        = 6
	defaultInspectFlowBudgetTokens = 1800
	defaultInspectFlowSearchBudget = 700
	defaultInspectFlowContextLines = 8
	defaultInspectFlowSearchLines  = 1
	defaultInspectFlowSymbolLines  = 48
)

// InspectFlow is a deterministic code-investigation workflow for vague
// behavior questions. It keeps the LLM out of the low-value orchestration loop:
// discover focused evidence, trace unambiguous symbols, then fetch all source
// context through one merged budgeted read.
func InspectFlow(idx *Index, query string, opts InspectFlowOptions) (InspectFlowResponse, error) {
	if opts.Limit <= 0 {
		opts.Limit = defaultInspectFlowLimit
	}
	if opts.BudgetTokens <= 0 {
		opts.BudgetTokens = defaultInspectFlowBudgetTokens
	}
	if opts.SearchBudgetTokens <= 0 {
		opts.SearchBudgetTokens = defaultInspectFlowSearchBudget
	}
	if opts.ContextLines <= 0 {
		opts.ContextLines = defaultInspectFlowContextLines
	}
	if opts.SearchContextLines <= 0 {
		opts.SearchContextLines = defaultInspectFlowSearchLines
	}
	mode := normalizeMode(opts.Mode)

	resp := InspectFlowResponse{Status: "not_found", Query: query}
	search := SearchCode(idx, query, SearchCodeOptions{
		Limit:             opts.Limit,
		BudgetTokens:      opts.SearchBudgetTokens,
		ContextLines:      opts.SearchContextLines,
		SourceOnly:        opts.SourceOnly,
		IncludeTests:      opts.IncludeTests,
		IncludeStories:    opts.IncludeStories,
		PreferDefinitions: true,
		Mode:              ModeEvidence,
		// inspectFlowAnchors below needs each hit's symbol ID to build
		// follow-up trace_symbol queries — SearchCode's default compact
		// projection drops it. The final response still only carries this
		// detail when the caller explicitly sets IncludeSearchSymbols
		// (stripSearchHitSymbols nils it out otherwise), so this has no
		// effect on inspect_flow/explore's default token cost.
		SymbolDetail: true,
	})
	resp.EstimatedTokens += search.EstimatedTokens
	if search.Status != "ok" {
		if !opts.IncludeSearchSymbols {
			stripSearchHitSymbols(&search)
		}
		resp.Search = search
		resp.Status = search.Status
		resp.Warnings = append(resp.Warnings, search.Warnings...)
		if search.Status == "" {
			resp.Status = "not_found"
		}
		return resp, nil
	}

	contextQueries, symbolQueries := inspectFlowAnchors(search.Hits, opts.Limit)
	var graphSnap symbolGraphSnapshot
	var flowSymbols []CGPSymbol
	if len(symbolQueries) > 0 || shouldExpandLifecycleQuery(searchQueryRawTermSet(query)) {
		graphSnap = idx.symbolGraphSnapshot()
	}
	for _, symQuery := range symbolQueries {
		sym, ok := graphSnap.Symbols[symQuery]
		if !ok {
			continue
		}
		flowSymbols = append(flowSymbols, sym)
	}
	lifecycleQuery := shouldExpandLifecycleQuery(searchQueryRawTermSet(query))
	traceBatch := buildSymbolTraceBatchWithDegrees(graphSnap, flowSymbols, opts.IncludeTraces, lifecycleQuery)
	contextQueries = appendLifecycleSupplementalContext(query, contextQueries, opts, graphSnap, traceBatch.degrees)
	// A broad natural-language ranker can legitimately be dominated by a
	// frequently referenced type even when the caller also supplied another
	// exact identifier. Add a few definition anchors for explicitly cased
	// symbol names whose files are not represented yet. These are placed ahead
	// of heuristic/search anchors so budget shaping retains the evidence the
	// caller named verbatim.
	explicitAnchors := inspectFlowExplicitSymbolAnchors(query, contextQueries, opts, graphSnap)
	for i := len(explicitAnchors) - 1; i >= 0; i-- {
		contextQueries = prependContextQuery(contextQueries, explicitAnchors[i].ID)
	}
	for _, sym := range flowSymbols {
		trace := traceSymbolFromBatch(sym, graphSnap, TraceSymbolOptions{
			Sites:        true,
			ExcludeTests: !opts.IncludeTests,
		}, traceBatch)
		trace.Query = sym.ID
		if opts.IncludeTraces {
			resp.Traces = append(resp.Traces, trace)
		}
		if trace.Symbol != nil {
			resp.Symbols = appendUniqueSummary(resp.Symbols, summarizeSymbol(*trace.Symbol))
		}
		for _, site := range trace.CallerSites {
			if len(contextQueries) >= opts.Limit*3 {
				break
			}
			if opts.SourceOnly && searchSupportFilePenalty(site.File, false) > 0 {
				continue
			}
			contextQueries = append(contextQueries, fmt.Sprintf("%s:%d", site.File, site.Line))
		}
	}
	resp.Symbols = sortedSummaries(resp.Symbols)
	if !opts.IncludeSearchSymbols {
		stripSearchHitSymbols(&search)
	}
	resp.Search = search

	contextBudget := opts.BudgetTokens - resp.EstimatedTokens
	if contextBudget < opts.BudgetTokens/3 {
		contextBudget = opts.BudgetTokens / 3
	}
	var sharedGraph *symbolGraphSnapshot
	if len(graphSnap.Symbols) > 0 {
		sharedGraph = &graphSnap
	}
	ctx, err := fetchContextManyWithSymbolGraph(idx, contextQueries, FetchContextOptions{
		BudgetTokens:          contextBudget,
		ContextLines:          opts.ContextLines,
		IncludeCallers:        opts.IncludeCallers,
		IncludeCallees:        opts.IncludeCallees,
		SuppressImports:       true,
		MaxSymbolContextLines: defaultInspectFlowSymbolLines,
		Mode:                  mode,
	}, sharedGraph)
	if err != nil {
		return resp, err
	}
	resp.Context = ctx
	resp.EstimatedTokens += ctx.EstimatedTokens
	resp.Warnings = append(resp.Warnings, ctx.Warnings...)
	if ctx.Status == "ok" {
		resp.Status = "ok"
	} else if resp.Status == "not_found" {
		resp.Status = ctx.Status
	}
	if ctx.Truncated || search.Truncated {
		resp.Warnings = append(resp.Warnings, "result truncated by token or hit budget")
	}
	fitInspectFlowResponse(&resp, opts.BudgetTokens)
	return resp, nil
}

func inspectFlowExplicitSymbolAnchors(query string, contextQueries []string, opts InspectFlowOptions, snap symbolGraphSnapshot) []CGPSymbol {
	tokens := inspectFlowIdentifierTokens(query)
	if len(tokens) == 0 || len(snap.Symbols) == 0 {
		return nil
	}
	tokenOrder := make(map[string]int, len(tokens))
	for i, token := range tokens {
		tokenOrder[token] = i
	}
	representedFiles := map[string]bool{}
	for _, contextQuery := range contextQueries {
		if sym, ok := snap.Symbols[contextQuery]; ok {
			representedFiles[sym.File] = true
			continue
		}
		if file, _, ok := splitInspectFlowFileLine(contextQuery); ok {
			representedFiles[file] = true
		}
	}
	type explicitCandidate struct {
		symbol CGPSymbol
		score  int
		names  map[string]bool
	}
	representedNames := map[string]bool{}
	for _, sym := range snap.Symbols {
		if _, named := tokenOrder[sym.Name]; named && representedFiles[sym.File] {
			representedNames[sym.Name] = true
		}
	}
	byFile := map[string]explicitCandidate{}
	for _, sym := range snap.Symbols {
		position, named := tokenOrder[sym.Name]
		if !named || representedNames[sym.Name] || sym.File == "" || sym.StartLine <= 0 || representedFiles[sym.File] || sym.Kind == "file" {
			continue
		}
		if opts.SourceOnly && shouldExcludeNoisyFile(sym.File, ListSymbolsOptions{IncludeTests: opts.IncludeTests, IncludeStories: opts.IncludeStories}) {
			continue
		}
		score := explicitFlowSymbolKindScore(sym.Kind) + minInt(len(sym.Name), 40)*2 - position
		if sym.Exported {
			score += 10
		}
		current, ok := byFile[sym.File]
		if !ok {
			current.names = map[string]bool{}
		}
		current.names[sym.Name] = true
		if !ok || score > current.score || (score == current.score && sym.StartLine < current.symbol.StartLine) {
			current.symbol = sym
			current.score = score
		}
		byFile[sym.File] = current
	}
	candidates := make([]explicitCandidate, 0, len(byFile))
	for _, candidate := range byFile {
		candidate.score += (len(candidate.names) - 1) * 250
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		if candidates[i].symbol.File != candidates[j].symbol.File {
			return candidates[i].symbol.File < candidates[j].symbol.File
		}
		return candidates[i].symbol.StartLine < candidates[j].symbol.StartLine
	})
	limit := opts.Limit / 2
	if limit < 1 {
		limit = 1
	}
	if limit > 4 {
		limit = 4
	}
	out := make([]CGPSymbol, 0, minInt(len(candidates), limit))
	coveredNames := representedNames
	for _, candidate := range candidates {
		addsEvidence := false
		for name := range candidate.names {
			if !coveredNames[name] {
				addsEvidence = true
				break
			}
		}
		if !addsEvidence {
			continue
		}
		out = append(out, candidate.symbol)
		for name := range candidate.names {
			coveredNames[name] = true
		}
		if len(out) >= limit {
			break
		}
	}
	return out
}

func inspectFlowIdentifierTokens(query string) []string {
	parts := strings.FieldsFunc(query, func(r rune) bool {
		return !isSearchLetter(r) && !isSearchDigit(r) && r != '_' && r != '$'
	})
	seen := map[string]bool{}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) < 3 || seen[part] {
			continue
		}
		explicit := strings.ContainsAny(part, "_$")
		for _, r := range part {
			if isSearchUpper(r) {
				explicit = true
				break
			}
		}
		if !explicit {
			continue
		}
		seen[part] = true
		out = append(out, part)
	}
	return out
}

func explicitFlowSymbolKindScore(kind string) int {
	switch kind {
	case "function", "method", "callback", "getter", "setter", "component", "http-route":
		return 500
	case "class", "interface", "type", "constant":
		return 300
	default:
		return 100
	}
}

// fitInspectFlowResponse enforces InspectFlowOptions.BudgetTokens against the
// complete serialized packet, not only source-text slices. Search and context
// builders use content budgets internally, but duplicated symbol metadata and
// JSON structure can otherwise make a nominal 1,800-token response several
// times larger on broad framework queries.
func fitInspectFlowResponse(resp *InspectFlowResponse, budget int) {
	if resp == nil || budget <= 0 {
		return
	}
	// EstimateTokens uses the portable four-characters-per-token estimate.
	// JSON keys and source punctuation are denser under cl100k-style
	// tokenizers, so reserve 12.5% rather than returning a packet that only
	// fits under the optimistic estimator.
	serializedBudget := budget * 7 / 8
	if serializedBudget <= 0 {
		serializedBudget = budget
	}
	// On success, drop the three redundant query echoes (top-level, search
	// sub-object, and context sub-object): the caller knows its own query, and
	// competitors don't echo it either. On the not_found/error path keep the
	// query in the context sub-object so a failed flow stays self-describing.
	if resp.Status == "ok" {
		resp.Query = ""
		resp.Search.Query = ""
		resp.Context.Query = ""
	} else {
		resp.Context.Query = resp.Query
	}
	// These summaries duplicate resp.Symbols and the slice locations in an
	// inspect-flow packet. fetch_context keeps them when called directly.
	resp.Context.Target = nil
	resp.Context.Targets = nil
	recountInspectFlowContext(resp)
	if inspectFlowSerializedTokens(resp) <= serializedBudget {
		resp.EstimatedTokens = inspectFlowSerializedTokens(resp)
		return
	}

	truncated := false
	for i := range resp.Symbols {
		if resp.Symbols[i].Docstring != "" {
			resp.Symbols[i].Docstring = ""
			truncated = true
		}
	}
	// Source slices are the most directly usable evidence in a flow packet.
	// Shed duplicated symbol detail and long ranked tails before deleting a
	// distinct source file the workflow already paid to discover.
	if inspectFlowSerializedTokens(resp) > serializedBudget {
		for i := range resp.Symbols {
			if resp.Symbols[i].Signature != "" || len(resp.Symbols[i].ReturnTypes) > 0 {
				resp.Symbols[i].Signature = ""
				resp.Symbols[i].ReturnTypes = nil
				truncated = true
			}
		}
	}
	const minimumFlowMetadataItems = 4
	for inspectFlowSerializedTokens(resp) > serializedBudget && len(resp.Symbols) > minimumFlowMetadataItems {
		resp.Symbols = resp.Symbols[:len(resp.Symbols)-1]
		truncated = true
	}
	for inspectFlowSerializedTokens(resp) > serializedBudget && len(resp.Search.Hits) > minimumFlowMetadataItems {
		resp.Search.Hits = resp.Search.Hits[:len(resp.Search.Hits)-1]
		resp.Search.Truncated = true
		truncated = true
	}
	for inspectFlowSerializedTokens(resp) > serializedBudget && len(resp.Context.Slices) > 1 {
		removeInspectFlowContextSlice(resp)
		truncated = true
	}
	for inspectFlowSerializedTokens(resp) > serializedBudget && len(resp.Traces) > 0 {
		resp.Traces = resp.Traces[:len(resp.Traces)-1]
		truncated = true
	}
	for inspectFlowSerializedTokens(resp) > serializedBudget && len(resp.Search.Hits) > 1 {
		resp.Search.Hits = resp.Search.Hits[:len(resp.Search.Hits)-1]
		resp.Search.Truncated = true
		truncated = true
	}
	for inspectFlowSerializedTokens(resp) > serializedBudget && len(resp.Symbols) > 1 {
		resp.Symbols = resp.Symbols[:len(resp.Symbols)-1]
		truncated = true
	}
	if inspectFlowSerializedTokens(resp) > serializedBudget && len(resp.Context.Slices) == 1 {
		slice := &resp.Context.Slices[0]
		line, lineNo := evidenceLine(slice)
		slice.Text = line
		slice.StartLine = lineNo
		slice.EndLine = lineNo
		slice.EstimatedTokens = EstimateTokens(line)
		truncated = true
	}
	for inspectFlowSerializedTokens(resp) > serializedBudget && len(resp.Search.Hits) > 0 {
		hit := &resp.Search.Hits[len(resp.Search.Hits)-1]
		if hit.Text == "" {
			resp.Search.Hits = resp.Search.Hits[:len(resp.Search.Hits)-1]
			continue
		}
		hit.Text = firstLine(hit.Text)
		hit.EstimatedTokens = EstimateTokens(hit.Text)
		truncated = true
		if inspectFlowSerializedTokens(resp) > serializedBudget {
			resp.Search.Hits = resp.Search.Hits[:len(resp.Search.Hits)-1]
		}
	}

	if truncated {
		resp.Context.Truncated = true
		resp.Search.Truncated = resp.Search.Truncated || len(resp.Search.Hits) < resp.Search.Limit
		resp.Warnings = appendUniqueString(resp.Warnings, "result shaped to fit total serialized token budget")
	}
	recountInspectFlowContext(resp)
	resp.EstimatedTokens = inspectFlowSerializedTokens(resp)
}

func inspectFlowSerializedTokens(resp *InspectFlowResponse) int {
	if resp == nil {
		return 0
	}
	copy := *resp
	copy.EstimatedTokens = 0
	data, err := json.Marshal(copy)
	if err != nil {
		return 0
	}
	estimate := EstimateTokens(string(data))
	copy.EstimatedTokens = estimate
	data, err = json.Marshal(copy)
	if err != nil {
		return estimate
	}
	return EstimateTokens(string(data))
}

func removeInspectFlowContextSlice(resp *InspectFlowResponse) {
	counts := map[string]int{}
	for _, slice := range resp.Context.Slices {
		counts[slice.File]++
	}
	remove := -1
	for i := len(resp.Context.Slices) - 1; i >= 0; i-- {
		if counts[resp.Context.Slices[i].File] > 1 {
			remove = i
			break
		}
	}
	if remove < 0 {
		remove = len(resp.Context.Slices) - 1
	}
	resp.Context.Slices = append(resp.Context.Slices[:remove], resp.Context.Slices[remove+1:]...)
	recountInspectFlowContext(resp)
}

func recountInspectFlowContext(resp *InspectFlowResponse) {
	total := 0
	for _, slice := range resp.Context.Slices {
		total += slice.EstimatedTokens
	}
	resp.Context.EstimatedTokens = total
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func stripSearchHitSymbols(search *SearchCodeResponse) {
	for i := range search.Hits {
		search.Hits[i].Symbols = nil
	}
}

func inspectFlowAnchors(hits []SearchCodeHit, limit int) ([]string, []string) {
	if limit <= 0 {
		limit = defaultInspectFlowLimit
	}
	var contextQueries []string
	var symbolQueries []string
	seenContext := map[string]bool{}
	seenSymbols := map[string]bool{}
	for _, hit := range hits {
		if len(contextQueries) >= limit*2 {
			break
		}
		line := hit.FocusLine
		if line <= 0 {
			line = hit.StartLine
		}
		if line > 0 {
			q := fmt.Sprintf("%s:%d", hit.File, line)
			if !seenContext[q] {
				contextQueries = append(contextQueries, q)
				seenContext[q] = true
			}
		}
		if len(symbolQueries) < limit {
			if sym, ok := preferredFlowSymbol(hit.Symbols); ok && sym.ID != "" && !seenSymbols[sym.ID] {
				seenSymbols[sym.ID] = true
				symbolQueries = append(symbolQueries, sym.ID)
				if !seenContext[sym.ID] {
					contextQueries = append(contextQueries, sym.ID)
					seenContext[sym.ID] = true
				}
			}
		}
	}
	return contextQueries, symbolQueries
}

func appendLifecycleSupplementalContext(query string, contextQueries []string, opts InspectFlowOptions, snap symbolGraphSnapshot, degree map[string]int) []string {
	if len(contextQueries) >= opts.Limit*2+1 {
		return contextQueries
	}
	rawSet := searchQueryRawTermSet(query)
	if !shouldExpandLifecycleQuery(rawSet) {
		return contextQueries
	}
	expandedSet := map[string]bool{}
	for _, term := range searchQueryTerms(query) {
		expandedSet[term] = true
	}
	seenFiles := map[string]bool{}
	seenQueries := map[string]bool{}
	for _, q := range contextQueries {
		seenQueries[q] = true
		if file, _, ok := splitInspectFlowFileLine(q); ok {
			seenFiles[file] = true
		}
	}
	type scoredLifecycleSymbol struct {
		symbol CGPSymbol
		score  int
	}
	candidates := make([]scoredLifecycleSymbol, 0, minInt(len(snap.Symbols), 256))
	for _, sym := range snap.Symbols {
		score := lifecycleSupplementalSymbolScore(sym, rawSet, expandedSet, degree[sym.ID])
		if score > 0 {
			candidates = append(candidates, scoredLifecycleSymbol{symbol: sym, score: score})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		if candidates[i].symbol.File != candidates[j].symbol.File {
			return candidates[i].symbol.File < candidates[j].symbol.File
		}
		return candidates[i].symbol.StartLine < candidates[j].symbol.StartLine
	})
	// Anchor the strongest implementation definition repo-wide rather than
	// restricting this pass to files the lexical hit list already admitted;
	// otherwise a documentation-heavy wrapper can define the candidate set
	// that is supposed to correct it.
	primaryFile := ""
	for _, candidate := range candidates {
		sym := candidate.symbol
		if opts.SourceOnly && shouldExcludeNoisyFile(sym.File, ListSymbolsOptions{IncludeTests: opts.IncludeTests, IncludeStories: opts.IncludeStories}) {
			continue
		}
		contextQueries = prependContextQuery(contextQueries, fmt.Sprintf("%s:%d", sym.File, sym.StartLine))
		primaryFile = sym.File
		seenFiles[sym.File] = true
		break
	}
	// Add the strongest complementary file below. This keeps the main
	// implementation first while still giving broad queries one adjacent
	// phase/context boundary when the budget permits.
	for _, candidate := range candidates {
		sym := candidate.symbol
		if sym.File == primaryFile {
			continue
		}
		if opts.SourceOnly && shouldExcludeNoisyFile(sym.File, ListSymbolsOptions{IncludeTests: opts.IncludeTests, IncludeStories: opts.IncludeStories}) {
			continue
		}
		if seenFiles[sym.File] {
			continue
		}
		q := fmt.Sprintf("%s:%d", sym.File, sym.StartLine)
		if seenQueries[q] {
			continue
		}
		return append(contextQueries, q)
	}
	return contextQueries
}

func searchQueryRawTermSet(query string) map[string]bool {
	out := map[string]bool{}
	for _, token := range searchTokens(query) {
		if searchStopWords[token] {
			continue
		}
		out[searchStem(token)] = true
	}
	return out
}

func prependContextQuery(queries []string, want string) []string {
	out := make([]string, 0, len(queries)+1)
	out = append(out, want)
	for _, query := range queries {
		if query != want {
			out = append(out, query)
		}
	}
	return out
}

func splitInspectFlowFileLine(query string) (string, int, bool) {
	i := strings.LastIndex(query, ":")
	if i <= 0 || i == len(query)-1 {
		return "", 0, false
	}
	line, err := strconv.Atoi(query[i+1:])
	if err != nil || line <= 0 {
		return "", 0, false
	}
	return query[:i], line, true
}

func lifecycleSupplementalSymbolScore(sym CGPSymbol, rawTerms, expandedTerms map[string]bool, graphDegree int) int {
	switch sym.Kind {
	case "function", "method", "class", "interface":
	default:
		return 0
	}
	if sym.File == "" || sym.StartLine <= 0 || isTestPath(sym.File) || isExampleOrDemoPath(sym.File) {
		return 0
	}

	nameTerms := stemmedSearchTermSet(sym.Name)
	signatureTerms := stemmedSearchTermSet(sym.Signature)
	pathTerms := stemmedSearchTermSet(filepath.ToSlash(sym.File))
	rawMatches, expandedMatches := 0, 0
	score := 0
	for term := range expandedTerms {
		weight := 0
		switch {
		case nameTerms[term]:
			weight = 180
		case signatureTerms[term]:
			weight = 90
		case pathTerms[term]:
			weight = 45
		}
		if weight == 0 {
			continue
		}
		expandedMatches++
		if rawTerms[term] {
			rawMatches++
			weight *= 2
		}
		score += weight
	}
	intentBonus := lifecycleIntentPhaseBonus(rawTerms, nameTerms, signatureTerms)
	if rawMatches == 0 && expandedMatches < 2 && intentBonus == 0 {
		return 0
	}
	score += intentBonus
	score += minInt(graphDegree, 12) * 12
	switch sym.Kind {
	case "function", "method":
		score += 100
	case "class", "interface":
		score += 30
	}
	if sym.Exported {
		score += 10
	}
	return score
}

func lifecycleIntentPhaseBonus(rawTerms, nameTerms, signatureTerms map[string]bool) int {
	has := func(terms ...string) bool {
		for _, term := range terms {
			if nameTerms[term] || signatureTerms[term] {
				return true
			}
		}
		return false
	}
	score := 0
	if rawTerms["teardown"] && has("teardown", "cleanup", "close", "pop", "push") {
		// Teardown boundaries are often named by the operation (pop/close)
		// rather than the user's lifecycle word, so compensate for the raw
		// lexical match that dispatch-style methods naturally receive.
		score += 1000
	}
	if rawTerms["before"] && has("before", "preprocess", "prepare", "setup") {
		score += 320
	}
	if rawTerms["after"] && has("after", "process", "finalize", "response") {
		score += 320
	}
	if (rawTerms["exception"] || rawTerms["error"]) && has("exception", "error", "recover", "rescue") {
		score += 320
	}
	if rawTerms["dispatch"] && has("dispatch", "route", "select") {
		score += 320
	}
	return score
}

func stemmedSearchTermSet(text string) map[string]bool {
	out := map[string]bool{}
	for _, token := range searchTokens(text) {
		token = searchStem(token)
		if token != "" && !searchStopWords[token] {
			out[token] = true
		}
	}
	return out
}

func preferredFlowSymbol(symbols []CGPSymbolSummary) (CGPSymbolSummary, bool) {
	if len(symbols) == 0 {
		return CGPSymbolSummary{}, false
	}
	for _, kind := range []string{"function", "method", "callback", "getter", "setter", "http-route", "component"} {
		for _, sym := range symbols {
			if sym.Kind == kind {
				return sym, true
			}
		}
	}
	for _, sym := range symbols {
		if sym.Kind != "class" && sym.Kind != "interface" && sym.Kind != "type" && sym.Kind != "file" {
			return sym, true
		}
	}
	return symbols[0], true
}

func appendUniqueSummary(out []CGPSymbolSummary, sym CGPSymbolSummary) []CGPSymbolSummary {
	for _, existing := range out {
		if existing.ID == sym.ID {
			return out
		}
	}
	return append(out, sym)
}

func sortedSummaries(in []CGPSymbolSummary) []CGPSymbolSummary {
	out := append([]CGPSymbolSummary(nil), in...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].StartLine != out[j].StartLine {
			return out[i].StartLine < out[j].StartLine
		}
		return out[i].Name < out[j].Name
	})
	return out
}

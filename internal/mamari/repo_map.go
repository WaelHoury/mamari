package mamari

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
)

const (
	defaultRepoMapBudgetTokens = 1200
	defaultRepoMapLimit        = 40
	repoMapDamping             = 0.85
	repoMapTolerance           = 1e-6
	repoMapMaxIterations       = 100

	// repoMapStrongMentionWeight is used for mentions that uniquely identify
	// a file or symbol: the full `-mentioned` entry as given, and the
	// basename (sans extension) of any path-like entry. These are specific
	// enough that they should dominate ranking.
	repoMapStrongMentionWeight = 12.0
	// repoMapWeakMentionWeight is used for camelCase/snake_case sub-tokens of
	// a `-mentioned` entry and for terms extracted from a natural-language
	// query. These tokens are individually common (e.g. "auth", "token") so
	// they get a much smaller per-match weight; relevance comes from a file
	// or symbol matching several of them, or matching a strong mention too.
	repoMapWeakMentionWeight = 2.0
	// repoMapMinSubTokenLen filters out very short camelCase/path fragments
	// (e.g. "js", "ts", "use", "src") that would otherwise match almost every
	// file in the repo and dilute personalization.
	repoMapMinSubTokenLen = 5
)

type repoMapGraph struct {
	files    []string
	index    map[string]int
	out      map[int]map[int]float64
	typed    map[int]map[int]map[string]repoMapEdgeStats
	inbound  map[int]int
	outbound map[int]int
}

type repoMapEdgeStats struct {
	edges  int
	weight float64
}

type repoMapCacheKey struct {
	budgetTokens        int
	limit               int
	mentioned           string
	query               string
	sourceOnly          bool
	includeTests        bool
	includeArchitecture bool
}

type repoMapCacheEntry struct {
	version  uint64
	response RepoMapResponse
}

// RepoMap builds a budgeted source-of-truth map from the CGP symbol graph.
// Edges point from a referrer file to a definer file, then weighted PageRank
// is redistributed to symbols so broad agent workflows can start with a
// compact ranked map instead of exploratory source reads.
func RepoMap(idx *Index, opts RepoMapOptions) RepoMapResponse {
	if opts.BudgetTokens <= 0 {
		opts.BudgetTokens = defaultRepoMapBudgetTokens
	}
	if opts.Limit <= 0 {
		opts.Limit = defaultRepoMapLimit
	}
	cacheKey := repoMapCacheKey{
		budgetTokens:        opts.BudgetTokens,
		limit:               opts.Limit,
		mentioned:           strings.Join(opts.Mentioned, "\x00"),
		query:               opts.Query,
		sourceOnly:          opts.SourceOnly,
		includeTests:        opts.IncludeTests,
		includeArchitecture: opts.IncludeArchitecture,
	}
	cacheVersion, cached, ok := idx.loadRepoMapResult(cacheKey)
	if ok {
		return cached
	}
	mentioned := buildRepoMapMentions(opts.Mentioned, opts.Query)
	resp := RepoMapResponse{
		Status:          "not_found",
		Query:           opts.Query,
		BudgetTokens:    opts.BudgetTokens,
		Files:           []RepoMapFile{},
		Symbols:         []RepoMapSymbol{},
		EstimatedTokens: 24,
	}

	snap := idx.snapshot()
	graph := buildRepoMapGraph(snap, mentioned, opts.IncludeArchitecture)
	if len(graph.files) == 0 {
		resp.Warnings = append(resp.Warnings, "no file graph edges available")
		return resp
	}
	ranks := personalizedPageRank(graph, mentioned, snap)
	coChange := idx.ensureCoChangeGraph()
	fileRows := repoMapFiles(snap, graph, ranks, mentioned, opts, coChange)
	symbolRows := repoMapSymbols(snap, ranks, mentioned, opts)
	sortRepoMapFiles(fileRows)
	sortRepoMapSymbols(symbolRows)
	if opts.IncludeArchitecture {
		architectureBudget := opts.BudgetTokens / 2
		if architectureBudget > 900 {
			architectureBudget = 900
		}
		if architectureBudget < 160 {
			architectureBudget = opts.BudgetTokens
		}
		architecture := buildRepoArchitecture(snap, graph, ranks, opts, architectureBudget)
		resp.Architecture = &architecture
		resp.EstimatedTokens += architecture.EstimatedTokens
	}

	for _, row := range fileRows {
		if len(resp.Files) >= opts.Limit || resp.EstimatedTokens+row.EstimatedTokens > opts.BudgetTokens {
			resp.Truncated = true
			break
		}
		resp.Files = append(resp.Files, row)
		resp.EstimatedTokens += row.EstimatedTokens
	}
	for _, row := range symbolRows {
		if len(resp.Symbols) >= opts.Limit || resp.EstimatedTokens+row.EstimatedTokens > opts.BudgetTokens {
			resp.Truncated = true
			break
		}
		resp.Symbols = append(resp.Symbols, row)
		resp.EstimatedTokens += row.EstimatedTokens
	}
	if len(resp.Files) > 0 || len(resp.Symbols) > 0 {
		resp.Status = "ok"
	}
	idx.storeRepoMapResult(cacheKey, cacheVersion, resp)
	return resp
}

func (idx *Index) loadRepoMapResult(key repoMapCacheKey) (uint64, RepoMapResponse, bool) {
	idx.repoMapCacheActive.Store(true)
	version := idx.repoMapCacheVersion.Load()
	idx.repoMapResultsMu.Lock()
	entry, ok := idx.repoMapResults[key]
	idx.repoMapResultsMu.Unlock()
	if !ok || entry.version != version {
		return version, RepoMapResponse{}, false
	}
	return version, cloneRepoMapResponse(entry.response), true
}

func (idx *Index) storeRepoMapResult(key repoMapCacheKey, version uint64, response RepoMapResponse) {
	if !idx.repoMapCacheActive.Load() || idx.repoMapCacheVersion.Load() != version {
		return
	}
	idx.repoMapResultsMu.Lock()
	defer idx.repoMapResultsMu.Unlock()
	if !idx.repoMapCacheActive.Load() || idx.repoMapCacheVersion.Load() != version {
		return
	}
	if idx.repoMapResults == nil {
		idx.repoMapResults = make(map[repoMapCacheKey]repoMapCacheEntry)
	}
	if len(idx.repoMapResults) >= 32 {
		clear(idx.repoMapResults)
	}
	idx.repoMapResults[key] = repoMapCacheEntry{
		version:  version,
		response: cloneRepoMapResponse(response),
	}
}

func (idx *Index) invalidateRepoMapCache() {
	if !idx.repoMapCacheActive.CompareAndSwap(true, false) {
		return
	}
	idx.repoMapCacheVersion.Add(1)
	idx.repoMapResultsMu.Lock()
	clear(idx.repoMapResults)
	idx.repoMapResultsMu.Unlock()
}

func cloneRepoMapResponse(response RepoMapResponse) RepoMapResponse {
	data, err := json.Marshal(response)
	if err != nil {
		return response
	}
	var clone RepoMapResponse
	if err := json.Unmarshal(data, &clone); err != nil {
		return response
	}
	return clone
}

func buildRepoMapGraph(snap indexSnapshot, mentioned repoMapMentions, includeTypedEdges bool) repoMapGraph {
	g := repoMapGraph{
		index:    map[string]int{},
		out:      map[int]map[int]float64{},
		inbound:  map[int]int{},
		outbound: map[int]int{},
	}
	if includeTypedEdges {
		g.typed = map[int]map[int]map[string]repoMapEdgeStats{}
	}
	addFile := func(file string) int {
		if n, ok := g.index[file]; ok {
			return n
		}
		n := len(g.files)
		g.index[file] = n
		g.files = append(g.files, file)
		return n
	}
	defCount := repoMapDefinitionCounts(snap.Symbols)
	snap.forEachEdge(func(_ int, edge CGPEdge) bool {
		from, okFrom := snap.Symbols[edge.From]
		to, okTo := snap.Symbols[edge.To]
		if !okFrom || !okTo || from.File == "" || to.File == "" || from.File == to.File {
			return true
		}
		if edge.Type != "calls" && edge.Type != terraformDependencyEdge && edge.Type != "imports" && edge.Type != "renders-component" && edge.Type != "passes-prop" && edge.Type != "binds-model" && edge.Type != "listens-event" && edge.Type != "handles-route" && edge.Type != "calls-http-route" && edge.Type != "references-symbol" && edge.Type != "scip-imports" {
			return true
		}
		src := addFile(from.File)
		dst := addFile(to.File)
		if g.out[src] == nil {
			g.out[src] = map[int]float64{}
		}
		if includeTypedEdges {
			if g.typed[src] == nil {
				g.typed[src] = map[int]map[string]repoMapEdgeStats{}
			}
			if g.typed[src][dst] == nil {
				g.typed[src][dst] = map[string]repoMapEdgeStats{}
			}
		}
		weight := repoMapIdentifierWeight(to.Name, mentioned, defCount[to.Name])
		if edge.Type == "imports" || edge.Type == "scip-imports" {
			weight *= 0.8
		}
		g.out[src][dst] += weight
		if includeTypedEdges {
			stats := g.typed[src][dst][edge.Type]
			stats.edges++
			stats.weight += weight
			g.typed[src][dst][edge.Type] = stats
		}
		g.outbound[src]++
		g.inbound[dst]++
		return true
	})
	for _, sym := range repoMapSortedSymbols(snap.Symbols) {
		if sym.File == "" || sym.Kind == "file" {
			continue
		}
		if len(mentioned) > 0 && repoMapMatchedMentions(sym, mentioned) == nil {
			continue
		}
		addFile(sym.File)
	}
	return g
}

func personalizedPageRank(g repoMapGraph, mentioned repoMapMentions, snap indexSnapshot) map[string]float64 {
	n := len(g.files)
	if n == 0 {
		return nil
	}
	personal := make([]float64, n)
	if len(mentioned) > 0 {
		for _, sym := range repoMapSortedSymbols(snap.Symbols) {
			matches := repoMapMatchedMentions(sym, mentioned)
			if len(matches) == 0 {
				continue
			}
			if idx, ok := g.index[sym.File]; ok {
				personal[idx] += repoMapTermSetWeight(matches, mentioned)
			}
		}
	}
	sumPersonal := 0.0
	for _, v := range personal {
		sumPersonal += v
	}
	if sumPersonal == 0 {
		for i := range personal {
			personal[i] = 1 / float64(n)
		}
	} else {
		for i := range personal {
			personal[i] /= sumPersonal
		}
	}

	rank := make([]float64, n)
	for i := range rank {
		rank[i] = 1 / float64(n)
	}
	// The per-source destination order is immutable across iterations, so sort
	// each source's destinations ONCE up front rather than re-sorting them in
	// every one of the up-to-repoMapMaxIterations passes. Same order → identical
	// float summation → byte-identical PageRank (zero golden-snapshot drift).
	sortedDstsBySrc := make([][]int, n)
	for src := 0; src < n; src++ {
		if len(g.out[src]) > 0 {
			sortedDstsBySrc[src] = sortedRepoMapDestinations(g.out[src])
		}
	}
	for iter := 0; iter < repoMapMaxIterations; iter++ {
		next := make([]float64, n)
		for i := range next {
			next[i] = (1 - repoMapDamping) * personal[i]
		}
		dangling := 0.0
		for src := 0; src < n; src++ {
			edges := g.out[src]
			if len(edges) == 0 {
				dangling += rank[src]
				continue
			}
			total := 0.0
			destinations := sortedDstsBySrc[src]
			for _, dst := range destinations {
				total += edges[dst]
			}
			if total == 0 {
				dangling += rank[src]
				continue
			}
			for _, dst := range destinations {
				w := edges[dst]
				next[dst] += repoMapDamping * rank[src] * (w / total)
			}
		}
		if dangling > 0 {
			for i := range next {
				next[i] += repoMapDamping * dangling * personal[i]
			}
		}
		delta := 0.0
		for i := range rank {
			delta += math.Abs(next[i] - rank[i])
		}
		rank = next
		if delta < repoMapTolerance {
			break
		}
	}
	out := map[string]float64{}
	for file, idx := range g.index {
		out[file] = rank[idx]
	}
	return out
}

// maxRepoMapCoChangeFiles bounds how many co-changed file names are attached
// to each RepoMapFile entry, keeping the repo map compact and token-budget
// friendly.
const maxRepoMapCoChangeFiles = 3

func repoMapFiles(snap indexSnapshot, g repoMapGraph, ranks map[string]float64, mentioned repoMapMentions, opts RepoMapOptions, coChange map[string][]CoChangeEntry) []RepoMapFile {
	var out []RepoMapFile
	// Compute mention matches for every file in one symbol pass. Calling
	// repoMapFileMentionMatches separately for each graph file made this
	// O(files*symbols*mentions); on large monorepos, a single map query could
	// spend minutes repeating the same full symbol-map scan thousands of times.
	mentionMatchesByFile := repoMapFileMentionMatchesAll(snap.Symbols, mentioned)
	for _, file := range g.files {
		if opts.SourceOnly && shouldExcludeNoisyFile(file, ListSymbolsOptions{IncludeTests: opts.IncludeTests}) {
			continue
		}
		info := snap.Files[file]
		matches := mentionMatchesByFile[file]
		score := int(ranks[file]*1_000_000) + int(repoMapTermSetWeight(matches, mentioned)*10_000) + g.inbound[g.index[file]]*20 + g.outbound[g.index[file]]*5
		row := RepoMapFile{
			File:           file,
			Language:       info.Language,
			Rank:           ranks[file],
			Score:          score,
			Inbound:        g.inbound[g.index[file]],
			Outbound:       g.outbound[g.index[file]],
			MatchedMention: matches,
			CoChangedFiles: coChangedFileNames(coChange[file], maxRepoMapCoChangeFiles),
		}
		row.EstimatedTokens = estimateStructuredTokens(row)
		out = append(out, row)
	}
	return out
}

// coChangedFileNames extracts up to limit file names from entries, preserving
// their existing frequency order.
func coChangedFileNames(entries []CoChangeEntry, limit int) []string {
	if len(entries) == 0 {
		return nil
	}
	if len(entries) > limit {
		entries = entries[:limit]
	}
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.File
	}
	return out
}

func repoMapSymbols(snap indexSnapshot, ranks map[string]float64, mentioned repoMapMentions, opts RepoMapOptions) []RepoMapSymbol {
	defCount := repoMapDefinitionCounts(snap.Symbols)
	var out []RepoMapSymbol
	for _, sym := range snap.Symbols {
		if sym.Kind == "file" || sym.File == "" {
			continue
		}
		if opts.SourceOnly && shouldExcludeNoisyFile(sym.File, ListSymbolsOptions{IncludeTests: opts.IncludeTests}) {
			continue
		}
		fileRank := ranks[sym.File]
		matches := repoMapMatchedMentions(sym, mentioned)
		score := int(fileRank*1_000_000) + int(repoMapIdentifierWeight(sym.Name, mentioned, defCount[sym.Name])*100)
		if len(matches) > 0 {
			score += int(repoMapTermSetWeight(matches, mentioned) * 50_000)
		}
		summary := summarizeSymbol(sym)
		summary.Score = score
		row := RepoMapSymbol{
			CGPSymbolSummary: summary,
			FileRank:         fileRank,
			MatchedMention:   matches,
		}
		row.EstimatedTokens = estimateStructuredTokens(row)
		out = append(out, row)
	}
	return out
}

// repoMapMentions maps a normalized lowercase term (a full `-mentioned`
// entry, a path basename, a camelCase/snake_case sub-token, or a term
// extracted from a natural-language query) to the personalization weight a
// single match contributes. See repoMapStrongMentionWeight /
// repoMapWeakMentionWeight for the rationale behind the two tiers.
type repoMapMentions map[string]float64

// buildRepoMapMentions merges explicit `-mentioned` entries (identifiers,
// paths, or terms) with significant terms extracted from a natural-language
// query into a single weighted mention set used to personalize ranking.
//
// Explicit mentions get a strong weight for the full string and any
// path/extension basename (these are specific enough to dominate ranking),
// and a weak weight for camelCase/snake_case sub-tokens of length >=
// repoMapMinSubTokenLen (short fragments like "js"/"use"/"src" are dropped
// entirely — they match too much of the codebase and would dilute
// personalization rather than focus it). Query terms go through the same
// stopword/stemming pipeline as search-code and get the weak weight, since a
// natural-language query is usually several individually-common words whose
// combination is the actual signal.
func buildRepoMapMentions(mentioned []string, query string) repoMapMentions {
	out := repoMapMentions{}
	add := func(term string, weight float64) {
		term = strings.TrimSpace(term)
		if term == "" {
			return
		}
		if existing, ok := out[term]; !ok || weight > existing {
			out[term] = weight
		}
	}
	for _, value := range mentioned {
		for _, raw := range strings.Split(value, ",") {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			part := strings.ToLower(raw)
			add(part, repoMapStrongMentionWeight)
			base := part
			if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
				base = base[i+1:]
			}
			if i := strings.LastIndex(base, "."); i > 0 {
				add(base[:i], repoMapStrongMentionWeight)
			} else if base != part {
				add(base, repoMapStrongMentionWeight)
			}
			// Tokenize the original-case string so camelCase boundaries
			// (e.g. "previewEnvelopeDocuments" -> preview/envelope/documents)
			// are preserved; searchTokens lowercases internally.
			for _, tok := range searchTokens(raw) {
				if len(tok) >= repoMapMinSubTokenLen && !searchStopWords[tok] {
					add(tok, repoMapWeakMentionWeight)
				}
			}
		}
	}
	for _, term := range searchQueryTerms(query) {
		if len(term) < 2 {
			continue
		}
		add(term, repoMapWeakMentionWeight)
	}
	return out
}

func repoMapMatchedMentions(sym CGPSymbol, mentioned repoMapMentions) []string {
	if len(mentioned) == 0 {
		return nil
	}
	text := strings.ToLower(sym.Name + " " + sym.Signature + " " + sym.File + " " + sym.SCIPSymbol)
	var out []string
	for mention := range mentioned {
		if mention == "" {
			continue
		}
		if strings.Contains(text, mention) {
			out = append(out, mention)
		}
	}
	sort.Strings(out)
	return out
}

// repoMapTermSetWeight sums the personalization weight of each distinct
// matched term, so a file/symbol matching one strong mention plus a couple
// weak query terms ranks above one matching only the weak terms.
func repoMapTermSetWeight(terms []string, mentioned repoMapMentions) float64 {
	total := 0.0
	for _, term := range terms {
		total += mentioned[term]
	}
	return total
}

func repoMapFileMentionMatches(symbols map[string]CGPSymbol, file string, mentioned repoMapMentions) []string {
	return repoMapFileMentionMatchesAll(symbols, mentioned)[file]
}

// repoMapFileMentionMatchesAll groups matched mentions for every file in one
// pass over symbols. The returned slices are sorted for deterministic ranking
// and response snapshots.
func repoMapFileMentionMatchesAll(symbols map[string]CGPSymbol, mentioned repoMapMentions) map[string][]string {
	if len(mentioned) == 0 {
		return nil
	}
	seenByFile := map[string]map[string]bool{}
	for _, sym := range symbols {
		if sym.File == "" {
			continue
		}
		for _, m := range repoMapMatchedMentions(sym, mentioned) {
			seen := seenByFile[sym.File]
			if seen == nil {
				seen = map[string]bool{}
				seenByFile[sym.File] = seen
			}
			seen[m] = true
		}
	}
	out := make(map[string][]string, len(seenByFile))
	for file, seen := range seenByFile {
		matches := make([]string, 0, len(seen))
		for mention := range seen {
			matches = append(matches, mention)
		}
		sort.Strings(matches)
		out[file] = matches
	}
	return out
}

func repoMapIdentifierWeight(name string, mentioned repoMapMentions, definitionCount int) float64 {
	weight := 1.0
	lower := strings.ToLower(name)
	if w, ok := mentioned[lower]; ok {
		weight *= 1 + w
	}
	if isLikelyExactIdentifier(name) || strings.Contains(name, "-") && len(name) >= 8 {
		weight *= 10
	}
	if strings.HasPrefix(name, "_") {
		weight *= 0.1
	}
	if definitionCount > 5 {
		weight *= 0.1
	}
	return weight
}

func repoMapDefinitionCounts(symbols map[string]CGPSymbol) map[string]int {
	out := map[string]int{}
	for _, sym := range symbols {
		if sym.Kind == "file" || sym.Name == "" {
			continue
		}
		out[sym.Name]++
	}
	return out
}

func repoMapSortedSymbols(symbols map[string]CGPSymbol) []CGPSymbol {
	out := make([]CGPSymbol, 0, len(symbols))
	for _, symbol := range symbols {
		out = append(out, symbol)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].StartLine != out[j].StartLine {
			return out[i].StartLine < out[j].StartLine
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func sortedRepoMapDestinations(edges map[int]float64) []int {
	out := make([]int, 0, len(edges))
	for destination := range edges {
		out = append(out, destination)
	}
	sort.Ints(out)
	return out
}

func estimateStructuredTokens(value any) int {
	data, err := json.Marshal(value)
	if err != nil {
		return 1
	}
	estimate := EstimateTokens(string(data))
	// JSON property names and punctuation tokenize a little worse than plain
	// source text under cl100k/o200k. Keep a 15% safety margin so the public
	// budget remains a ceiling in real agent tokenizers, not only chars/4.
	return (estimate*120 + 99) / 100
}

func sortRepoMapFiles(rows []RepoMapFile) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Score != rows[j].Score {
			return rows[i].Score > rows[j].Score
		}
		return rows[i].File < rows[j].File
	})
}

func sortRepoMapSymbols(rows []RepoMapSymbol) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Score != rows[j].Score {
			return rows[i].Score > rows[j].Score
		}
		if rows[i].File != rows[j].File {
			return rows[i].File < rows[j].File
		}
		if rows[i].StartLine != rows[j].StartLine {
			return rows[i].StartLine < rows[j].StartLine
		}
		return rows[i].Name < rows[j].Name
	})
}

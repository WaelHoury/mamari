package mamari

import (
	"embed"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

//go:embed webui.html
var graphUIAssets embed.FS

type GraphUINode struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Language   string `json:"language"`
	File       string `json:"file"`
	StartLine  int    `json:"startLine"`
	Signature  string `json:"signature,omitempty"`
	Confidence string `json:"confidence"`
	Degree     int    `json:"degree"`
	// Aggregate-view fields (view=packages|files): how much the node rolls up.
	SymbolCount int `json:"symbolCount,omitempty"`
	FileCount   int `json:"fileCount,omitempty"`
	// Health-overlay fields (overlay=health). Booleans describe a symbol
	// node; counts describe an aggregate node.
	Dead           bool `json:"dead,omitempty"`
	Untested       bool `json:"untested,omitempty"`
	HotPath        bool `json:"hotPath,omitempty"`
	HighComplexity bool `json:"highComplexity,omitempty"`
	DeadCount      int  `json:"deadCount,omitempty"`
	UntestedCount  int  `json:"untestedCount,omitempty"`
	HotPathCount   int  `json:"hotPathCount,omitempty"`
}

type GraphUIEdge struct {
	ID         string `json:"id"`
	From       string `json:"from"`
	To         string `json:"to"`
	Type       string `json:"type"`
	Confidence string `json:"confidence"`
	File       string `json:"file,omitempty"`
	Line       int    `json:"line,omitempty"`
	// Count is the number of underlying symbol edges an aggregate edge rolls
	// up (view=packages|files). Zero for plain symbol edges.
	Count int `json:"count,omitempty"`
}

type GraphUIStats struct {
	Files      int `json:"files"`
	Symbols    int `json:"symbols"`
	Edges      int `json:"edges"`
	Unresolved int `json:"unresolved"`
}

type GraphUIResponse struct {
	Repo      RepoInfo      `json:"repo"`
	Stats     GraphUIStats  `json:"stats"`
	Nodes     []GraphUINode `json:"nodes"`
	Edges     []GraphUIEdge `json:"edges"`
	Kinds     []string      `json:"kinds"`
	Languages []string      `json:"languages"`
	EdgeTypes []string      `json:"edgeTypes"`
	Truncated bool          `json:"truncated,omitempty"`
}

// NewGraphUIHandler returns a dependency-free local graph explorer. The
// handler intentionally exposes only bounded graph packets and symbol reads;
// it never accepts writes or serves arbitrary filesystem paths.
func NewGraphUIHandler(idx *Index) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, err := graphUIAssets.ReadFile("webui.html")
		if err != nil {
			http.Error(w, "graph explorer unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		_, _ = w.Write(data)
	})
	mux.HandleFunc("GET /api/graph", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit := boundedInt(q.Get("limit"), 260, 20, 1000)
		overlay := q.Get("overlay") == "health"
		var resp GraphUIResponse
		switch q.Get("view") {
		case "packages":
			resp = buildGraphUIAggregateResponse(idx, aggregateByPackage, q.Get("focus"), q.Get("edge"), limit, overlay)
		case "files":
			resp = buildGraphUIAggregateResponse(idx, aggregateByFile, q.Get("focus"), q.Get("edge"), limit, overlay)
		default: // symbols — the original behavior; `file` narrows to one file for drill-down.
			resp = buildGraphUIResponse(idx, q.Get("q"), q.Get("kind"), q.Get("language"), q.Get("edge"), q.Get("file"), limit)
			if overlay {
				decorateGraphUIHealth(idx, &resp)
			}
		}
		writeGraphUIJSON(w, resp)
	})
	mux.HandleFunc("GET /api/symbol", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" || len(id) > 2048 {
			http.Error(w, "id is required", http.StatusBadRequest)
			return
		}
		resp, err := InspectSymbolNode(idx, id, InspectSymbolNodeOptions{BudgetTokens: 1400, SourceLines: 180, IncludeTests: true})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeGraphUIJSON(w, resp)
	})
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func buildGraphUIResponse(idx *Index, query, kind, language, edgeType, fileFilter string, limit int) GraphUIResponse {
	snap := idx.snapshot()
	query = strings.ToLower(strings.TrimSpace(query))
	kind = strings.TrimSpace(kind)
	language = strings.TrimSpace(language)
	edgeType = strings.TrimSpace(edgeType)
	fileFilter = strings.TrimSpace(fileFilter)

	degree := make(map[string]int, len(snap.Symbols))
	var eligibleEdges []CGPEdge
	edgeTypes := map[string]bool{}
	snap.forEachEdge(func(_ int, edge CGPEdge) bool {
		edgeTypes[edge.Type] = true
		if edgeType != "" && edge.Type != edgeType {
			return true
		}
		eligibleEdges = append(eligibleEdges, edge)
		degree[edge.From]++
		degree[edge.To]++
		return true
	})

	kinds, languages := map[string]bool{}, map[string]bool{}
	all := make([]CGPSymbol, 0, len(snap.Symbols))
	for _, symbol := range snap.Symbols {
		kinds[symbol.Kind] = true
		languages[symbol.Language] = true
		if kind != "" && symbol.Kind != kind || language != "" && symbol.Language != language {
			continue
		}
		if fileFilter != "" {
			// Drill-down from the files view: everything declared in this one
			// file (including kinds the architecture view hides), no
			// name-match requirement.
			if symbol.File != fileFilter || symbol.Kind == "file" {
				continue
			}
		} else if query != "" {
			haystack := strings.ToLower(symbol.Name + " " + symbol.File + " " + symbol.Signature + " " + symbol.Kind)
			if !strings.Contains(haystack, query) {
				continue
			}
		} else if kind == "" && language == "" && !graphUIDefaultKind(symbol.Kind) {
			continue
		}
		all = append(all, symbol)
	}
	sort.Slice(all, func(i, j int) bool {
		di, dj := degree[all[i].ID], degree[all[j].ID]
		if di != dj {
			return di > dj
		}
		if all[i].Kind != all[j].Kind {
			return all[i].Kind < all[j].Kind
		}
		if all[i].File != all[j].File {
			return all[i].File < all[j].File
		}
		return all[i].ID < all[j].ID
	})
	truncated := len(all) > limit
	if len(all) > limit {
		all = all[:limit]
	}
	selected := make(map[string]bool, limit)
	for _, symbol := range all {
		selected[symbol.ID] = true
	}

	// A focused search (or a single-file drill-down) is much more useful with
	// immediate graph context. Add one-hop neighbors while respecting the same
	// hard node ceiling — for a file view this is exactly "how does this file
	// connect outward".
	if (query != "" || fileFilter != "") && len(selected) < limit {
		for _, edge := range eligibleEdges {
			if len(selected) >= limit {
				break
			}
			var neighbor string
			switch {
			case selected[edge.From] && !selected[edge.To]:
				neighbor = edge.To
			case selected[edge.To] && !selected[edge.From]:
				neighbor = edge.From
			}
			if neighbor == "" {
				continue
			}
			symbol, ok := snap.Symbols[neighbor]
			if !ok || kind != "" && symbol.Kind != kind || language != "" && symbol.Language != language {
				continue
			}
			selected[neighbor] = true
			all = append(all, symbol)
		}
	}

	nodes := make([]GraphUINode, 0, len(all))
	for _, symbol := range all {
		nodes = append(nodes, GraphUINode{
			ID: symbol.ID, Name: symbol.Name, Kind: symbol.Kind, Language: symbol.Language,
			File: symbol.File, StartLine: symbol.StartLine, Signature: symbol.Signature,
			Confidence: symbol.Confidence, Degree: degree[symbol.ID],
		})
	}
	var edges []GraphUIEdge
	unresolved := 0
	snap.forEachEdge(func(index int, edge CGPEdge) bool {
		if edge.Confidence == ConfUnresolved {
			unresolved++
		}
		if edgeType != "" && edge.Type != edgeType || !selected[edge.From] || !selected[edge.To] {
			return true
		}
		edge = snap.edgeAtWithID(index)
		edges = append(edges, GraphUIEdge{ID: edge.ID, From: edge.From, To: edge.To, Type: edge.Type, Confidence: edge.Confidence, File: edge.Evidence.File, Line: edge.Evidence.StartLine})
		return true
	})
	sort.Slice(edges, func(i, j int) bool { return edges[i].ID < edges[j].ID })
	return GraphUIResponse{
		Repo: snap.Repo, Stats: GraphUIStats{Files: len(snap.Files), Symbols: len(snap.Symbols), Edges: snap.edgeCount(), Unresolved: unresolved},
		Nodes: nodes, Edges: edges, Kinds: sortedSet(kinds), Languages: sortedSet(languages), EdgeTypes: sortedSet(edgeTypes), Truncated: truncated,
	}
}

type graphUIAggregateLevel int

const (
	aggregateByPackage graphUIAggregateLevel = iota
	aggregateByFile
)

// buildGraphUIAggregateResponse renders the hierarchical views: one node per
// directory (view=packages) or per file (view=files), with symbol edges
// rolled up into weighted aggregate edges between them. This is the top-down
// "how is the codebase connected?" picture; clicking down drills
// package → files → symbols. focus narrows the files view to one directory.
func buildGraphUIAggregateResponse(idx *Index, level graphUIAggregateLevel, focus, edgeType string, limit int, overlay bool) GraphUIResponse {
	snap := idx.snapshot()
	edgeType = strings.TrimSpace(edgeType)
	focus = strings.TrimSpace(strings.TrimSuffix(focus, "/"))

	keyOf := func(file string) string {
		if level == aggregateByFile {
			return file
		}
		dir := file
		if i := strings.LastIndexByte(file, '/'); i >= 0 {
			dir = file[:i]
		} else {
			dir = "."
		}
		return dir
	}
	inFocus := func(file string) bool {
		if focus == "" {
			return true
		}
		// "." is the root package (files with no directory) — prefix matching
		// can never hit it because root files carry no "./" prefix.
		if focus == "." {
			return !strings.Contains(file, "/")
		}
		return file == focus || strings.HasPrefix(file, focus+"/")
	}

	var deadSet, untestedSet map[string]bool
	if overlay {
		deadSet, untestedSet = graphUIHealthSets(idx)
	}

	type agg struct {
		key        string
		files      map[string]bool
		symbols    int
		langCounts map[string]int // dominant language chosen deterministically below
		dead       int
		untested   int
		hotPath    int
	}
	groups := map[string]*agg{}
	symbolKey := map[string]string{} // symbol ID -> aggregate key
	kindsSeen, languagesSeen := map[string]bool{}, map[string]bool{}
	for _, sym := range snap.Symbols {
		kindsSeen[sym.Kind] = true
		languagesSeen[sym.Language] = true
		if sym.Kind == "file" || sym.File == "" || !inFocus(sym.File) {
			continue
		}
		key := keyOf(sym.File)
		g := groups[key]
		if g == nil {
			g = &agg{key: key, files: map[string]bool{}, langCounts: map[string]int{}}
			groups[key] = g
		}
		g.files[sym.File] = true
		g.symbols++
		if sym.Language != "" {
			g.langCounts[sym.Language]++
		}
		symbolKey[sym.ID] = key
		if overlay {
			if deadSet[sym.ID] {
				g.dead++
			}
			if untestedSet[sym.ID] {
				g.untested++
			}
			if sym.TransitiveLoopDepth >= reportHotPathLoopMin || sym.LinearScanInLoop > 0 {
				g.hotPath++
			}
		}
	}
	// Dominant language per group, deterministically (max count, then
	// lexicographic) — the previous last-seen-under-map-iteration value made
	// byte output nondeterministic across identical requests and could label
	// a Vue package "css".
	dominantLang := func(counts map[string]int) string {
		best, bestN := "", -1
		for lang, n := range counts {
			if n > bestN || (n == bestN && lang < best) {
				best, bestN = lang, n
			}
		}
		return best
	}

	// Roll symbol edges up to aggregate edges between distinct groups.
	type aggEdge struct {
		from, to   string
		edgeType   string
		count      int
		confidence string
	}
	confRank := func(c string) int {
		switch c {
		case ConfExact:
			return 3
		case ConfScoped:
			return 2
		case ConfHeuristic:
			return 1
		default:
			return 0
		}
	}
	edgeAgg := map[string]*aggEdge{}
	edgeTypesSeen := map[string]bool{}
	degree := map[string]int{}
	unresolved := 0
	snap.forEachEdge(func(_ int, edge CGPEdge) bool {
		if edge.Confidence == ConfUnresolved {
			unresolved++
		}
		edgeTypesSeen[edge.Type] = true
		if edgeType != "" && edge.Type != edgeType {
			return true
		}
		var fromKey, toKey string
		if edge.Type == "imports" && strings.HasPrefix(edge.To, "module:") {
			// Imports are file-level edges (file symbol -> "module:<spec>"),
			// which the symbol-based mapping below can never resolve — yet
			// package-level import dependencies are THE canonical
			// architecture signal. Roll them up by resolving the spec to an
			// indexed file.
			fromSym, ok := snap.Symbols[edge.From]
			if !ok || fromSym.File == "" || !inFocus(fromSym.File) {
				return true
			}
			resolved := resolveImportSpecToIndexedFile(idx, fromSym.File, strings.TrimPrefix(edge.To, "module:"))
			if resolved == "" || !inFocus(resolved) {
				return true
			}
			fromKey, toKey = keyOf(fromSym.File), keyOf(resolved)
		} else {
			var okF, okT bool
			fromKey, okF = symbolKey[edge.From]
			toKey, okT = symbolKey[edge.To]
			if !okF || !okT {
				return true
			}
		}
		if fromKey == toKey || groups[fromKey] == nil || groups[toKey] == nil {
			return true
		}
		id := edge.Type + "\x00" + fromKey + "\x00" + toKey
		e := edgeAgg[id]
		if e == nil {
			e = &aggEdge{from: fromKey, to: toKey, edgeType: edge.Type}
			edgeAgg[id] = e
			degree[fromKey]++
			degree[toKey]++
		}
		e.count++
		if confRank(edge.Confidence) > confRank(e.confidence) {
			e.confidence = edge.Confidence
		}
		return true
	})

	ordered := make([]*agg, 0, len(groups))
	for _, g := range groups {
		ordered = append(ordered, g)
	}
	sort.Slice(ordered, func(i, j int) bool {
		di, dj := degree[ordered[i].key], degree[ordered[j].key]
		if di != dj {
			return di > dj
		}
		if ordered[i].symbols != ordered[j].symbols {
			return ordered[i].symbols > ordered[j].symbols
		}
		return ordered[i].key < ordered[j].key
	})
	truncated := len(ordered) > limit
	if len(ordered) > limit {
		ordered = ordered[:limit]
	}
	selected := map[string]bool{}
	for _, g := range ordered {
		selected[g.key] = true
	}

	prefix := "dir:"
	kindName := "package"
	if level == aggregateByFile {
		prefix = "file:"
		kindName = "file"
	}
	nodes := make([]GraphUINode, 0, len(ordered))
	for _, g := range ordered {
		name := g.key
		if i := strings.LastIndexByte(name, '/'); i >= 0 && level == aggregateByFile {
			name = name[i+1:]
		}
		nodes = append(nodes, GraphUINode{
			ID: prefix + g.key, Name: name, Kind: kindName, Language: dominantLang(g.langCounts),
			File: g.key, Degree: degree[g.key],
			SymbolCount: g.symbols, FileCount: len(g.files),
			DeadCount: g.dead, UntestedCount: g.untested, HotPathCount: g.hotPath,
		})
	}

	edges := make([]GraphUIEdge, 0, len(edgeAgg))
	for _, e := range edgeAgg {
		if !selected[e.from] || !selected[e.to] {
			continue
		}
		edges = append(edges, GraphUIEdge{
			ID:   "agg:" + e.edgeType + ":" + e.from + "->" + e.to,
			From: prefix + e.from, To: prefix + e.to,
			Type: e.edgeType, Confidence: e.confidence, Count: e.count,
		})
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].ID < edges[j].ID })

	return GraphUIResponse{
		Repo: snap.Repo, Stats: GraphUIStats{Files: len(snap.Files), Symbols: len(snap.Symbols), Edges: snap.edgeCount(), Unresolved: unresolved},
		Nodes: nodes, Edges: edges, Kinds: sortedSet(kindsSeen), Languages: sortedSet(languagesSeen), EdgeTypes: sortedSet(edgeTypesSeen), Truncated: truncated,
	}
}

// graphUIHealthSets computes the dead and untested symbol-ID sets using the
// same shipped flows the CLI exposes, so the overlay never disagrees with
// `mamari dead-code` / `untested_symbols`.
func graphUIHealthSets(idx *Index) (dead, untested map[string]bool) {
	dead, untested = map[string]bool{}, map[string]bool{}
	dc := DeadCode(idx, DeadCodeOptions{Limit: 1 << 20})
	for _, s := range dc.Symbols {
		if s.ID != "" {
			dead[s.ID] = true
		}
	}
	ut := UntestedSymbols(idx, UntestedSymbolsOptions{Limit: 1 << 20})
	for _, s := range ut.Symbols {
		if s.ID != "" {
			untested[s.ID] = true
		}
	}
	return dead, untested
}

// decorateGraphUIHealth marks symbol nodes with the health overlay flags.
func decorateGraphUIHealth(idx *Index, resp *GraphUIResponse) {
	deadSet, untestedSet := graphUIHealthSets(idx)
	snap := idx.symbolGraphSnapshot()
	for i := range resp.Nodes {
		n := &resp.Nodes[i]
		n.Dead = deadSet[n.ID]
		n.Untested = untestedSet[n.ID]
		if sym, ok := snap.Symbols[n.ID]; ok {
			n.HotPath = sym.TransitiveLoopDepth >= reportHotPathLoopMin || sym.LinearScanInLoop > 0
			n.HighComplexity = sym.Complexity >= reportHighComplexity
		}
	}
}

func graphUIDefaultKind(kind string) bool {
	switch kind {
	case "class", "interface", "function", "component", "http-route", "k8s-resource", "kustomization", "docker-stage", "ttl-shape":
		return true
	default:
		return false
	}
}

func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for value := range set {
		if value != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func boundedInt(raw string, fallback, min, max int) int {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

func writeGraphUIJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(value)
}

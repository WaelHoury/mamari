package mamari

import (
	"math"
	"path"
	"sort"
	"strconv"
	"strings"
)

const (
	maxArchitectureLanguages   = 16
	maxArchitecturePackages    = 12
	maxArchitectureEntryPoints = 8
	maxArchitectureRoutes      = 8
	maxArchitectureHotspots    = 8
	maxArchitectureCommunities = 8
	maxArchitectureBoundaries  = 8
	maxCommunityPreviewFiles   = 6
	maxPackagePreviewFiles     = 3
)

// buildRepoArchitecture turns the same graph used by RepoMap's personalized
// PageRank into a compact architecture packet. It deliberately derives every
// field from indexed evidence; no directory-name-only "layer" claims are
// emitted because those are attractive but unreliable on unfamiliar repos.
func buildRepoArchitecture(snap indexSnapshot, graph repoMapGraph, ranks map[string]float64, opts RepoMapOptions, budget int) RepoArchitecture {
	communities, membership := repoArchitectureCommunities(snap, graph, ranks, opts)
	architecture := RepoArchitecture{
		Languages:   repoArchitectureLanguages(snap, opts),
		Packages:    repoArchitecturePackages(snap, ranks, opts),
		EntryPoints: repoArchitectureEntryPoints(snap, ranks, opts),
		Routes:      repoArchitectureRoutes(snap, ranks, opts),
		Hotspots:    repoArchitectureHotspots(snap, graph, ranks, opts),
		Communities: communities,
		Boundaries:  repoArchitectureBoundaries(graph, membership),
	}
	trimRepoArchitecture(&architecture, budget)
	return architecture
}

func repoArchitectureFileAllowed(file string, opts RepoMapOptions) bool {
	return !opts.SourceOnly || !shouldExcludeNoisyFile(file, ListSymbolsOptions{IncludeTests: opts.IncludeTests})
}

func repoArchitectureLanguages(snap indexSnapshot, opts RepoMapOptions) []RepoLanguageSummary {
	counts := map[string]*RepoLanguageSummary{}
	for file, meta := range snap.Files {
		if !repoArchitectureFileAllowed(file, opts) {
			continue
		}
		language := meta.Language
		if language == "" {
			language = "unknown"
		}
		row := counts[language]
		if row == nil {
			row = &RepoLanguageSummary{Language: language}
			counts[language] = row
		}
		row.Files++
	}
	for _, symbol := range snap.Symbols {
		if symbol.File == "" || !repoArchitectureFileAllowed(symbol.File, opts) {
			continue
		}
		language := symbol.Language
		if language == "" {
			language = snap.Files[symbol.File].Language
		}
		if language == "" {
			language = "unknown"
		}
		row := counts[language]
		if row == nil {
			row = &RepoLanguageSummary{Language: language}
			counts[language] = row
		}
		row.Symbols++
	}
	rows := make([]RepoLanguageSummary, 0, len(counts))
	for _, row := range counts {
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Files != rows[j].Files {
			return rows[i].Files > rows[j].Files
		}
		return rows[i].Language < rows[j].Language
	})
	if len(rows) > maxArchitectureLanguages {
		rows = rows[:maxArchitectureLanguages]
	}
	return rows
}

func repoArchitecturePackageName(file string) string {
	dir := path.Dir(strings.TrimPrefix(strings.ReplaceAll(file, "\\", "/"), "./"))
	if dir == "." || dir == "" {
		return "(root)"
	}
	return dir
}

func repoArchitecturePackages(snap indexSnapshot, ranks map[string]float64, opts RepoMapOptions) []RepoPackageSummary {
	type packageBuild struct {
		row   RepoPackageSummary
		files []string
	}
	builds := map[string]*packageBuild{}
	for _, file := range repoArchitectureSortedFiles(snap.Files) {
		if !repoArchitectureFileAllowed(file, opts) {
			continue
		}
		name := repoArchitecturePackageName(file)
		build := builds[name]
		if build == nil {
			build = &packageBuild{row: RepoPackageSummary{Package: name}}
			builds[name] = build
		}
		build.row.Files++
		build.row.Rank += ranks[file]
		build.files = append(build.files, file)
	}
	for _, symbol := range snap.Symbols {
		if symbol.File == "" || !repoArchitectureFileAllowed(symbol.File, opts) {
			continue
		}
		if build := builds[repoArchitecturePackageName(symbol.File)]; build != nil {
			build.row.Symbols++
		}
	}
	rows := make([]RepoPackageSummary, 0, len(builds))
	for _, build := range builds {
		sort.Slice(build.files, func(i, j int) bool {
			if ranks[build.files[i]] != ranks[build.files[j]] {
				return ranks[build.files[i]] > ranks[build.files[j]]
			}
			return build.files[i] < build.files[j]
		})
		if len(build.files) > maxPackagePreviewFiles {
			build.files = build.files[:maxPackagePreviewFiles]
		}
		build.row.TopFiles = build.files
		rows = append(rows, build.row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Rank != rows[j].Rank {
			return rows[i].Rank > rows[j].Rank
		}
		if rows[i].Files != rows[j].Files {
			return rows[i].Files > rows[j].Files
		}
		return rows[i].Package < rows[j].Package
	})
	if len(rows) > maxArchitecturePackages {
		rows = rows[:maxArchitecturePackages]
	}
	return rows
}

func repoArchitectureEntryPoints(snap indexSnapshot, ranks map[string]float64, opts RepoMapOptions) []RepoEntryPoint {
	var rows []RepoEntryPoint
	for _, symbol := range snap.Symbols {
		if symbol.File == "" || !repoArchitectureFileAllowed(symbol.File, opts) {
			continue
		}
		reason := repoArchitectureEntryPointReason(symbol)
		if reason == "" {
			continue
		}
		rows = append(rows, RepoEntryPoint{CGPSymbolSummary: summarizeSymbol(symbol), Reason: reason})
	}
	sort.Slice(rows, func(i, j int) bool {
		left, right := ranks[rows[i].File], ranks[rows[j].File]
		if left != right {
			return left > right
		}
		if rows[i].File != rows[j].File {
			return rows[i].File < rows[j].File
		}
		return rows[i].StartLine < rows[j].StartLine
	})
	if len(rows) > maxArchitectureEntryPoints {
		rows = rows[:maxArchitectureEntryPoints]
	}
	return rows
}

func repoArchitectureEntryPointReason(symbol CGPSymbol) string {
	name := strings.ToLower(strings.TrimSpace(symbol.Name))
	switch name {
	case "main", "__main__":
		return "conventional main"
	case "init":
		return "runtime initializer"
	case "bootstrap", "startserver", "runserver", "createapp":
		return "startup convention"
	}
	return ""
}

func repoArchitectureRoutes(snap indexSnapshot, ranks map[string]float64, opts RepoMapOptions) []CGPSymbolSummary {
	var rows []CGPSymbolSummary
	for _, symbol := range snap.Symbols {
		if symbol.Kind != "http-route" || !repoArchitectureFileAllowed(symbol.File, opts) {
			continue
		}
		rows = append(rows, summarizeSymbol(symbol))
	}
	sort.Slice(rows, func(i, j int) bool {
		if ranks[rows[i].File] != ranks[rows[j].File] {
			return ranks[rows[i].File] > ranks[rows[j].File]
		}
		if rows[i].File != rows[j].File {
			return rows[i].File < rows[j].File
		}
		return rows[i].StartLine < rows[j].StartLine
	})
	if len(rows) > maxArchitectureRoutes {
		rows = rows[:maxArchitectureRoutes]
	}
	return rows
}

func repoArchitectureHotspots(snap indexSnapshot, graph repoMapGraph, ranks map[string]float64, opts RepoMapOptions) []RepoHotspot {
	complexity := map[string]int{}
	for _, symbol := range snap.Symbols {
		if symbol.File != "" {
			complexity[symbol.File] += symbol.Complexity
		}
	}
	rows := make([]RepoHotspot, 0, len(graph.files))
	for _, file := range graph.files {
		if !repoArchitectureFileAllowed(file, opts) {
			continue
		}
		node := graph.index[file]
		score := int(ranks[file]*1_000_000) + graph.inbound[node]*30 + graph.outbound[node]*10 + complexity[file]*20
		rows = append(rows, RepoHotspot{
			File: file, Score: score, Rank: ranks[file], Inbound: graph.inbound[node],
			Outbound: graph.outbound[node], Complexity: complexity[file],
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Score != rows[j].Score {
			return rows[i].Score > rows[j].Score
		}
		return rows[i].File < rows[j].File
	})
	if len(rows) > maxArchitectureHotspots {
		rows = rows[:maxArchitectureHotspots]
	}
	return rows
}

// repoArchitectureCommunities performs the local-moving phase of weighted
// modularity optimization (the core of Louvain) on an undirected projection
// of the file dependency graph. File iteration and tie-breaking are sorted so
// community IDs and previews remain deterministic across identical indexes.
func repoArchitectureCommunities(snap indexSnapshot, graph repoMapGraph, ranks map[string]float64, opts RepoMapOptions) ([]RepoCommunity, map[int]int) {
	n := len(graph.files)
	if n == 0 {
		return nil, nil
	}
	adj := make([]map[int]float64, n)
	for src := 0; src < n; src++ {
		if !repoArchitectureFileAllowed(graph.files[src], opts) {
			continue
		}
		edges := graph.out[src]
		for _, dst := range sortedRepoMapDestinations(edges) {
			weight := edges[dst]
			if src == dst || weight <= 0 || !repoArchitectureFileAllowed(graph.files[dst], opts) {
				continue
			}
			if adj[src] == nil {
				adj[src] = map[int]float64{}
			}
			if adj[dst] == nil {
				adj[dst] = map[int]float64{}
			}
			adj[src][dst] += weight
			adj[dst][src] += weight
		}
	}
	degree := make([]float64, n)
	totalDegree := 0.0
	for node := range adj {
		for _, neighbor := range sortedRepoMapDestinations(adj[node]) {
			degree[node] += adj[node][neighbor]
		}
		totalDegree += degree[node]
	}
	community := make([]int, n)
	totals := make([]float64, n)
	for node := range community {
		community[node] = node
		totals[node] = degree[node]
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool { return graph.files[order[i]] < graph.files[order[j]] })
	if totalDegree > 0 {
		for pass := 0; pass < 100; pass++ {
			moved := false
			for _, node := range order {
				current := community[node]
				weights := map[int]float64{}
				for _, neighbor := range sortedRepoMapDestinations(adj[node]) {
					weight := adj[node][neighbor]
					weights[community[neighbor]] += weight
				}
				totals[current] -= degree[node]
				best, bestGain := current, 0.0
				candidates := make([]int, 0, len(weights)+1)
				seen := map[int]bool{current: true}
				candidates = append(candidates, current)
				for candidate := range weights {
					if !seen[candidate] {
						seen[candidate] = true
						candidates = append(candidates, candidate)
					}
				}
				sort.Ints(candidates)
				for _, candidate := range candidates {
					gain := weights[candidate] - totals[candidate]*degree[node]/totalDegree
					if gain > bestGain+1e-12 || math.Abs(gain-bestGain) <= 1e-12 && candidate < best {
						best, bestGain = candidate, gain
					}
				}
				community[node] = best
				totals[best] += degree[node]
				if best != current {
					moved = true
				}
			}
			if !moved {
				break
			}
		}
	}
	community = repoArchitectureRefineConnectedCommunities(community, adj, order)

	type communityBuild struct {
		oldID          int
		files          []string
		rank           float64
		internalWeight float64
		externalWeight float64
		edgeTypes      map[string]*RepoCommunityEdgeType
	}
	builds := map[int]*communityBuild{}
	for node, oldID := range community {
		file := graph.files[node]
		if !repoArchitectureFileAllowed(file, opts) {
			continue
		}
		build := builds[oldID]
		if build == nil {
			build = &communityBuild{oldID: oldID, edgeTypes: map[string]*RepoCommunityEdgeType{}}
			builds[oldID] = build
		}
		build.files = append(build.files, file)
		build.rank += ranks[file]
	}
	for src := 0; src < n; src++ {
		edges := graph.out[src]
		for _, dst := range sortedRepoMapDestinations(edges) {
			weight := edges[dst]
			left, right := builds[community[src]], builds[community[dst]]
			if left == nil || right == nil {
				continue
			}
			if left == right {
				left.internalWeight += weight
			} else {
				left.externalWeight += weight
				right.externalWeight += weight
			}
			for edgeType, stats := range graph.typed[src][dst] {
				if left == right {
					row := repoArchitectureCommunityEdgeType(left.edgeTypes, edgeType)
					row.InternalEdges += stats.edges
					row.InternalWeight += stats.weight
				} else {
					leftRow := repoArchitectureCommunityEdgeType(left.edgeTypes, edgeType)
					leftRow.ExternalEdges += stats.edges
					leftRow.ExternalWeight += stats.weight
					rightRow := repoArchitectureCommunityEdgeType(right.edgeTypes, edgeType)
					rightRow.ExternalEdges += stats.edges
					rightRow.ExternalWeight += stats.weight
				}
			}
		}
	}
	ordered := make([]*communityBuild, 0, len(builds))
	for _, build := range builds {
		sort.Strings(build.files)
		ordered = append(ordered, build)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].rank != ordered[j].rank {
			return ordered[i].rank > ordered[j].rank
		}
		if len(ordered[i].files) != len(ordered[j].files) {
			return len(ordered[i].files) > len(ordered[j].files)
		}
		return ordered[i].files[0] < ordered[j].files[0]
	})
	oldToNew := map[int]int{}
	fileCommunity := make(map[string]int, len(community))
	for node, oldID := range community {
		fileCommunity[graph.files[node]] = oldID
	}
	communitySymbols := map[int][]CGPSymbol{}
	for _, symbol := range repoMapSortedSymbols(snap.Symbols) {
		oldID, ok := fileCommunity[symbol.File]
		if !ok || symbol.Name == "" || symbol.Kind == "file" || symbol.Kind == "import" {
			continue
		}
		communitySymbols[oldID] = append(communitySymbols[oldID], symbol)
	}
	rows := make([]RepoCommunity, 0, len(ordered))
	for i, build := range ordered {
		id := i + 1
		oldToNew[build.oldID] = id
		sort.Slice(build.files, func(i, j int) bool {
			if ranks[build.files[i]] != ranks[build.files[j]] {
				return ranks[build.files[i]] > ranks[build.files[j]]
			}
			return build.files[i] < build.files[j]
		})
		preview := append([]string(nil), build.files...)
		if len(preview) > maxCommunityPreviewFiles {
			preview = preview[:maxCommunityPreviewFiles]
		}
		rows = append(rows, RepoCommunity{
			ID: id, Name: repoArchitectureCommunityName(build.files), FileCount: len(build.files), Files: preview,
			Packages:       repoArchitectureCommunityPackages(build.files),
			TopSymbols:     repoArchitectureTopSymbolNames(communitySymbols[build.oldID], 3),
			EdgeTypes:      repoArchitectureSortedCommunityEdgeTypes(build.edgeTypes),
			Cohesion:       repoArchitectureCohesion(build.internalWeight, build.externalWeight),
			InternalWeight: build.internalWeight, ExternalWeight: build.externalWeight,
		})
	}
	if len(rows) > maxArchitectureCommunities {
		rows = rows[:maxArchitectureCommunities]
	}
	membership := make(map[int]int, len(community))
	for node, oldID := range community {
		membership[node] = oldToNew[oldID]
	}
	return rows, membership
}

func repoArchitectureCommunityEdgeType(rows map[string]*RepoCommunityEdgeType, edgeType string) *RepoCommunityEdgeType {
	row := rows[edgeType]
	if row == nil {
		row = &RepoCommunityEdgeType{Type: edgeType}
		rows[edgeType] = row
	}
	return row
}

func repoArchitectureSortedCommunityEdgeTypes(rows map[string]*RepoCommunityEdgeType) []RepoCommunityEdgeType {
	out := make([]RepoCommunityEdgeType, 0, len(rows))
	for _, row := range rows {
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool {
		left := out[i].InternalWeight + out[i].ExternalWeight
		right := out[j].InternalWeight + out[j].ExternalWeight
		if left != right {
			return left > right
		}
		return out[i].Type < out[j].Type
	})
	return out
}

// repoArchitectureRefineConnectedCommunities applies the connectivity part of
// Leiden-style refinement: a reported community must be connected in the
// undirected file graph. It does not claim full Leiden optimization, but it
// prevents a modularity-local optimum from presenting disconnected modules as
// one architectural unit.
func repoArchitectureRefineConnectedCommunities(community []int, adj []map[int]float64, order []int) []int {
	if len(community) == 0 {
		return community
	}
	refined := append([]int(nil), community...)
	nextID := 0
	groups := map[int][]int{}
	for _, node := range order {
		groups[community[node]] = append(groups[community[node]], node)
		if community[node] >= nextID {
			nextID = community[node] + 1
		}
	}
	groupIDs := make([]int, 0, len(groups))
	for id := range groups {
		groupIDs = append(groupIDs, id)
	}
	sort.Ints(groupIDs)
	visited := make([]bool, len(community))
	for _, id := range groupIDs {
		component := 0
		for _, start := range groups[id] {
			if visited[start] {
				continue
			}
			assignedID := id
			if component > 0 {
				assignedID = nextID
				nextID++
			}
			component++
			visited[start] = true
			queue := []int{start}
			for len(queue) > 0 {
				node := queue[0]
				queue = queue[1:]
				refined[node] = assignedID
				for _, neighbor := range sortedRepoMapDestinations(adj[node]) {
					if adj[node][neighbor] <= 0 || visited[neighbor] || community[neighbor] != id {
						continue
					}
					visited[neighbor] = true
					queue = append(queue, neighbor)
				}
			}
		}
	}
	return refined
}

func repoArchitectureCohesion(internal, external float64) float64 {
	total := internal + external
	if total <= 0 {
		return 0
	}
	return internal / total
}

func repoArchitectureCommunityPackages(files []string) []string {
	counts := map[string]int{}
	for _, file := range files {
		counts[repoArchitecturePackageName(file)]++
	}
	type packageCount struct {
		name  string
		count int
	}
	rows := make([]packageCount, 0, len(counts))
	for name, count := range counts {
		rows = append(rows, packageCount{name: name, count: count})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].count != rows[j].count {
			return rows[i].count > rows[j].count
		}
		return rows[i].name < rows[j].name
	})
	if len(rows) > 3 {
		rows = rows[:3]
	}
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = row.name
	}
	return out
}

func repoArchitectureTopSymbolNames(symbols []CGPSymbol, limit int) []string {
	sort.SliceStable(symbols, func(i, j int) bool {
		left := symbols[i].Complexity + repoArchitectureRepresentativeKindWeight(symbols[i].Kind)
		right := symbols[j].Complexity + repoArchitectureRepresentativeKindWeight(symbols[j].Kind)
		if left != right {
			return left > right
		}
		if symbols[i].File != symbols[j].File {
			return symbols[i].File < symbols[j].File
		}
		return symbols[i].StartLine < symbols[j].StartLine
	})
	seen := map[string]bool{}
	var out []string
	for _, symbol := range symbols {
		if seen[symbol.Name] {
			continue
		}
		seen[symbol.Name] = true
		out = append(out, symbol.Name)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func repoArchitectureRepresentativeKindWeight(kind string) int {
	if isTerraformSymbolKind(kind) {
		return 20
	}
	switch kind {
	case "http-route":
		return 40
	case "class", "interface", "component":
		return 20
	case "function", "method":
		return 10
	default:
		return 0
	}
}

func repoArchitectureCommunityName(files []string) string {
	if len(files) == 0 {
		return "community"
	}
	counts := map[string]int{}
	for _, file := range files {
		counts[repoArchitecturePackageName(file)]++
	}
	type packageCount struct {
		name  string
		count int
	}
	packages := make([]packageCount, 0, len(counts))
	for name, count := range counts {
		packages = append(packages, packageCount{name: name, count: count})
	}
	sort.Slice(packages, func(i, j int) bool {
		if packages[i].count != packages[j].count {
			return packages[i].count > packages[j].count
		}
		return packages[i].name < packages[j].name
	})
	stems := make([]string, 0, 2)
	for _, file := range files {
		base := path.Base(file)
		stem := strings.TrimSuffix(base, path.Ext(base))
		if stem == "" || containsStringValue(stems, stem) {
			continue
		}
		stems = append(stems, stem)
		if len(stems) == 2 {
			break
		}
	}
	label := strings.Join(stems, " + ")
	if len(packages) == 1 || packages[0].count*2 >= len(files) {
		if label == "" {
			return packages[0].name
		}
		return packages[0].name + ": " + label
	}
	if label != "" {
		return label
	}
	return packages[0].name + " +" + strconv.Itoa(len(packages)-1)
}

func repoArchitectureBoundaries(graph repoMapGraph, membership map[int]int) []RepoBoundary {
	type boundaryKey struct{ from, to int }
	type boundaryBuild struct {
		row       RepoBoundary
		edgeTypes map[string]RepoEdgeTypeSummary
	}
	builds := map[boundaryKey]*boundaryBuild{}
	for src := 0; src < len(graph.files); src++ {
		edges := graph.out[src]
		from := membership[src]
		for _, dst := range sortedRepoMapDestinations(edges) {
			weight := edges[dst]
			to := membership[dst]
			if from == 0 || to == 0 || from == to || from > maxArchitectureCommunities || to > maxArchitectureCommunities {
				continue
			}
			key := boundaryKey{from: from, to: to}
			build := builds[key]
			if build == nil {
				build = &boundaryBuild{
					row:       RepoBoundary{FromCommunity: from, ToCommunity: to},
					edgeTypes: map[string]RepoEdgeTypeSummary{},
				}
				builds[key] = build
			}
			build.row.Weight += weight
			for edgeType, stats := range graph.typed[src][dst] {
				build.row.Edges += stats.edges
				row := build.edgeTypes[edgeType]
				row.Type = edgeType
				row.Edges += stats.edges
				row.Weight += stats.weight
				build.edgeTypes[edgeType] = row
			}
		}
	}
	rows := make([]RepoBoundary, 0, len(builds))
	for _, build := range builds {
		for _, row := range build.edgeTypes {
			build.row.EdgeTypes = append(build.row.EdgeTypes, row)
		}
		sort.Slice(build.row.EdgeTypes, func(i, j int) bool {
			if build.row.EdgeTypes[i].Weight != build.row.EdgeTypes[j].Weight {
				return build.row.EdgeTypes[i].Weight > build.row.EdgeTypes[j].Weight
			}
			return build.row.EdgeTypes[i].Type < build.row.EdgeTypes[j].Type
		})
		rows = append(rows, build.row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Weight != rows[j].Weight {
			return rows[i].Weight > rows[j].Weight
		}
		if rows[i].FromCommunity != rows[j].FromCommunity {
			return rows[i].FromCommunity < rows[j].FromCommunity
		}
		return rows[i].ToCommunity < rows[j].ToCommunity
	})
	if len(rows) > maxArchitectureBoundaries {
		rows = rows[:maxArchitectureBoundaries]
	}
	return rows
}

func trimRepoArchitecture(architecture *RepoArchitecture, budget int) {
	if budget <= 0 {
		budget = 300
	}
	estimate := func() int {
		copy := *architecture
		copy.EstimatedTokens = 0
		return estimateStructuredTokens(copy)
	}
	for estimate() > budget {
		architecture.Truncated = true
		switch {
		case len(architecture.Boundaries) > 0:
			architecture.Boundaries = architecture.Boundaries[:len(architecture.Boundaries)-1]
		case len(architecture.Packages) > 3:
			architecture.Packages = architecture.Packages[:len(architecture.Packages)-1]
		case len(architecture.Hotspots) > 3:
			architecture.Hotspots = architecture.Hotspots[:len(architecture.Hotspots)-1]
		case len(architecture.Routes) > 3:
			architecture.Routes = architecture.Routes[:len(architecture.Routes)-1]
		case len(architecture.EntryPoints) > 3:
			architecture.EntryPoints = architecture.EntryPoints[:len(architecture.EntryPoints)-1]
		case trimRepoArchitectureCommunityDetail(architecture):
		case len(architecture.Communities) > 2:
			if len(architecture.Packages) > 1 {
				architecture.Packages = architecture.Packages[:len(architecture.Packages)-1]
			} else if len(architecture.Hotspots) > 1 {
				architecture.Hotspots = architecture.Hotspots[:len(architecture.Hotspots)-1]
			} else if len(architecture.EntryPoints) > 1 {
				architecture.EntryPoints = architecture.EntryPoints[:len(architecture.EntryPoints)-1]
			} else if len(architecture.Routes) > 1 {
				architecture.Routes = architecture.Routes[:len(architecture.Routes)-1]
			} else if len(architecture.Communities) > 4 {
				architecture.Communities = architecture.Communities[:len(architecture.Communities)-1]
			} else {
				architecture.Communities = architecture.Communities[:len(architecture.Communities)-1]
			}
		case len(architecture.Languages) > 3:
			architecture.Languages = architecture.Languages[:len(architecture.Languages)-1]
		default:
			architecture.EstimatedTokens = estimate()
			return
		}
	}
	architecture.EstimatedTokens = estimate()
}

func trimRepoArchitectureCommunityDetail(architecture *RepoArchitecture) bool {
	communities := architecture.Communities
	for i := len(communities) - 1; i >= 0; i-- {
		if len(communities[i].EdgeTypes) > 1 {
			communities[i].EdgeTypes = communities[i].EdgeTypes[:len(communities[i].EdgeTypes)-1]
			return true
		}
	}
	for i := len(communities) - 1; i >= 3; i-- {
		if len(communities[i].EdgeTypes) > 0 {
			communities[i].EdgeTypes = nil
			return true
		}
	}
	for i := len(communities) - 1; i >= 0; i-- {
		if len(communities[i].TopSymbols) > 1 {
			communities[i].TopSymbols = communities[i].TopSymbols[:len(communities[i].TopSymbols)-1]
			return true
		}
	}
	for i := len(communities) - 1; i >= 0; i-- {
		if len(communities[i].Files) > 3 {
			communities[i].Files = communities[i].Files[:len(communities[i].Files)-1]
			return true
		}
	}
	for i := len(communities) - 1; i >= 0; i-- {
		if len(communities[i].Packages) > 1 {
			communities[i].Packages = communities[i].Packages[:len(communities[i].Packages)-1]
			return true
		}
	}
	for i := len(communities) - 1; i >= 4; i-- {
		if len(communities[i].TopSymbols) > 0 {
			communities[i].TopSymbols = nil
			return true
		}
		if len(communities[i].Files) > 2 {
			communities[i].Files = communities[i].Files[:len(communities[i].Files)-1]
			return true
		}
	}
	return false
}

func repoArchitectureSortedFiles(files map[string]File) []string {
	out := make([]string, 0, len(files))
	for file := range files {
		out = append(out, file)
	}
	sort.Strings(out)
	return out
}

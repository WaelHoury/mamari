package mamari

import (
	"sort"
	"strings"
)

// crossRepoNodeSep separates a repo name from a file path inside a
// cross-repo graph node name ("repoName" + sep + "relFile"). Mirrors the
// ":" convention already used for synthetic keys elsewhere (event:<name>,
// module:<spec>).
const crossRepoNodeSep = ":"

func crossRepoNodeName(repo, file string) string {
	return repo + crossRepoNodeSep + file
}

// crossRepoEdgeWeight is the weight given to a cross-repo coupling edge in
// the combined graph fed to community detection — large enough to pull
// genuinely coupled repos into the same community, but not so large that a
// single cross-repo call dominates over a repo's own internal call graph.
const crossRepoEdgeWeight = 5.0

// CrossRepoArchitectureResponse is CrossRepoArchitecture's result: every
// cross-repo coupling edge found, plus communities/boundaries computed over
// the combined multi-repo graph (the primary index's own repo_map community
// detection, generalized to span repo boundaries instead of being confined
// to one repo's files).
type CrossRepoArchitectureResponse struct {
	Status      string          `json:"status"`
	Repos       []string        `json:"repos"`
	Edges       []CrossRepoEdge `json:"edges"`
	Communities []RepoCommunity `json:"communities"`
	Boundaries  []RepoBoundary  `json:"boundaries"`
	Warnings    []string        `json:"warnings,omitempty"`
}

// CrossRepoArchitecture computes cross-repo coupling edges (HTTP + event,
// see FindAllCrossRepoEdges) and runs the same Louvain-style community
// detection repo_map's single-repo architecture view uses
// (repoArchitectureCommunities), but over a combined graph spanning the
// primary index and every linked repo. A community whose RepoCommunity.Repos
// has more than one entry is a real cross-repo architectural boundary that
// a single-repo view cannot see.
//
// Deliberately reuses repoArchitectureCommunities/repoArchitectureBoundaries
// unmodified rather than generalizing their signatures: a synthetic combined
// indexSnapshot (symbols with repo-prefixed File fields, to match the
// combined graph's repo-prefixed node names) and a combined repoMapGraph are
// built here instead, so the well-tested single-repo code path is completely
// untouched.
func CrossRepoArchitecture(idx *Index, linked []LinkedRepo, opts RepoMapOptions) CrossRepoArchitectureResponse {
	resp := CrossRepoArchitectureResponse{Status: "ok"}
	repos := append([]LinkedRepo{{Name: repoName(idx), Index: idx}}, linked...)
	for _, r := range repos {
		name := r.Name
		if name == "" {
			name = repoName(r.Index)
		}
		resp.Repos = append(resp.Repos, name)
	}
	if len(repos) < 2 {
		resp.Warnings = append(resp.Warnings, "no linked repos — cross-repo architecture needs at least one repo loaded via `mamari serve --link`")
	}

	resp.Edges = FindAllCrossRepoEdges(idx, linked)

	type namedRepoGraph struct {
		name  string
		graph repoMapGraph
		snap  indexSnapshot
	}
	graphs := make([]namedRepoGraph, 0, len(repos))
	for _, r := range repos {
		name := r.Name
		if name == "" {
			name = repoName(r.Index)
		}
		snap := r.Index.snapshot()
		graphs = append(graphs, namedRepoGraph{name: name, graph: buildRepoMapGraph(snap, nil, true), snap: snap})
	}

	combined := repoMapGraph{
		index: map[string]int{},
		out:   map[int]map[int]float64{},
		typed: map[int]map[int]map[string]repoMapEdgeStats{},
	}
	combinedSymbols := map[string]CGPSymbol{}
	addNode := func(name string) int {
		if n, ok := combined.index[name]; ok {
			return n
		}
		n := len(combined.files)
		combined.index[name] = n
		combined.files = append(combined.files, name)
		return n
	}
	for _, g := range graphs {
		for _, file := range g.graph.files {
			addNode(crossRepoNodeName(g.name, file))
		}
		for src, edges := range g.graph.out {
			srcNode := addNode(crossRepoNodeName(g.name, g.graph.files[src]))
			for dst, weight := range edges {
				dstNode := addNode(crossRepoNodeName(g.name, g.graph.files[dst]))
				addRepoMapWeightedEdge(&combined, srcNode, dstNode, weight, g.graph.typed[src][dst])
			}
		}
		for id, sym := range g.snap.Symbols {
			cp := sym
			cp.File = crossRepoNodeName(g.name, sym.File)
			combinedSymbols[g.name+"#"+id] = cp
		}
	}
	for _, ce := range resp.Edges {
		srcNode := addNode(crossRepoNodeName(ce.FromRepo, ce.From.File))
		dstNode := addNode(crossRepoNodeName(ce.ToRepo, ce.To.File))
		edgeType := "cross-repo-" + ce.Kind
		addRepoMapWeightedEdge(&combined, srcNode, dstNode, crossRepoEdgeWeight, map[string]repoMapEdgeStats{
			edgeType: {edges: 1, weight: crossRepoEdgeWeight},
		})
	}

	if len(combined.files) == 0 {
		return resp
	}
	combinedSnap := indexSnapshot{Symbols: combinedSymbols}
	ranks := personalizedPageRank(combined, nil, combinedSnap)
	communities, membership := repoArchitectureCommunities(combinedSnap, combined, ranks, opts)
	annotateCommunityRepos(communities, membership, combined.files)
	resp.Communities = communities
	resp.Boundaries = repoArchitectureBoundaries(combined, membership)
	return resp
}

// addRepoMapWeightedEdge adds (or accumulates into) one weighted, optionally
// typed edge in g. Shared by the per-repo edge-copying and cross-repo-edge
// insertion steps in CrossRepoArchitecture.
func addRepoMapWeightedEdge(g *repoMapGraph, src, dst int, weight float64, typed map[string]repoMapEdgeStats) {
	if g.out[src] == nil {
		g.out[src] = map[int]float64{}
	}
	g.out[src][dst] += weight
	if len(typed) == 0 {
		return
	}
	if g.typed[src] == nil {
		g.typed[src] = map[int]map[string]repoMapEdgeStats{}
	}
	if g.typed[src][dst] == nil {
		g.typed[src][dst] = map[string]repoMapEdgeStats{}
	}
	for edgeType, stats := range typed {
		existing := g.typed[src][dst][edgeType]
		existing.edges += stats.edges
		existing.weight += stats.weight
		g.typed[src][dst][edgeType] = existing
	}
}

// annotateCommunityRepos sets each community's Repos field (the distinct set
// of repos its member files span) by walking membership/files directly —
// repoArchitectureCommunities truncates Files to a preview, so Repos cannot
// be derived from the response rows alone.
func annotateCommunityRepos(communities []RepoCommunity, membership map[int]int, files []string) {
	if len(communities) == 0 {
		return
	}
	byID := make(map[int]*RepoCommunity, len(communities))
	reposSeen := make(map[int]map[string]bool, len(communities))
	for i := range communities {
		byID[communities[i].ID] = &communities[i]
		reposSeen[communities[i].ID] = map[string]bool{}
	}
	for node, communityID := range membership {
		if node < 0 || node >= len(files) {
			continue
		}
		seen, ok := reposSeen[communityID]
		if !ok {
			continue
		}
		repo, _, found := strings.Cut(files[node], crossRepoNodeSep)
		if !found {
			continue
		}
		seen[repo] = true
	}
	for id, seen := range reposSeen {
		community := byID[id]
		if community == nil {
			continue
		}
		repos := make([]string, 0, len(seen))
		for repo := range seen {
			repos = append(repos, repo)
		}
		sort.Strings(repos)
		community.Repos = repos
	}
}

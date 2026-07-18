package mamari

import (
	"path/filepath"
	"sort"
	"strings"
)

// LinkedRepo is a secondary, read-only index loaded alongside the primary
// index so tools like find_route can resolve references across repo
// boundaries (e.g. a frontend repo's HTTP client calls resolving to route
// handlers defined in a separate backend repo's index). Linked indexes are
// never written to.
type LinkedRepo struct {
	Name  string
	Index *Index
}

// RouteMatch is one http-route (handler) or http-endpoint (caller) symbol —
// or, when Kind is "event", one listens-event (handler) or emits-event
// (caller) site — found while resolving a route/event query, tagged with
// which repo it came from.
type RouteMatch struct {
	Repo   string           `json:"repo"`
	Symbol CGPSymbolSummary `json:"symbol"`
	// Kind is "http" (the default, for backward compatibility with existing
	// callers) or "event".
	Kind string `json:"kind,omitempty"`
}

// CrossRepoEdge is one cross-repo coupling signal found by
// FindAllCrossRepoEdges: an HTTP call resolving to a route handler in
// another linked repo (Kind "http"), or a matching emit/listen pair on the
// same event name across repos (Kind "event").
type CrossRepoEdge struct {
	FromRepo   string           `json:"fromRepo"`
	ToRepo     string           `json:"toRepo"`
	From       CGPSymbolSummary `json:"from"`
	To         CGPSymbolSummary `json:"to"`
	Kind       string           `json:"kind"`
	Confidence string           `json:"confidence"`
}

// FindRouteResponse reports where an HTTP route is handled and called,
// across the primary index and any linked repos.
type FindRouteResponse struct {
	Status   string       `json:"status"`
	Query    string       `json:"query"`
	Method   string       `json:"method,omitempty"`
	Path     string       `json:"path,omitempty"`
	Handlers []RouteMatch `json:"handlers"`
	Callers  []RouteMatch `json:"callers"`
}

// LinkedRepoInfo summarizes one repo (primary or linked) for list_linked_repos.
type LinkedRepoInfo struct {
	Name    string `json:"name"`
	Root    string `json:"root"`
	Primary bool   `json:"primary"`
	Files   int    `json:"files"`
	Symbols int    `json:"symbols"`
}

// ListLinkedReposResponse lists the primary index plus every linked repo
// available to cross-repo tools such as find_route.
type ListLinkedReposResponse struct {
	Status string           `json:"status"`
	Repos  []LinkedRepoInfo `json:"repos"`
}

// repoName derives a short, stable label for a repo from its root path —
// used to tag matches so agents can tell which repo a result came from.
func repoName(idx *Index) string {
	idx.mu.Lock()
	root := idx.Repo.Root
	idx.mu.Unlock()
	if root == "" {
		return "."
	}
	return filepath.Base(filepath.Clean(root))
}

// ListLinkedRepos reports the primary index and every linked repo, so agents
// can discover what cross-repo context find_route can search.
func ListLinkedRepos(idx *Index, linked []LinkedRepo) ListLinkedReposResponse {
	resp := ListLinkedReposResponse{Status: "ok", Repos: []LinkedRepoInfo{}}
	resp.Repos = append(resp.Repos, linkedRepoInfo(LinkedRepo{Name: repoName(idx), Index: idx}, true))
	for _, lr := range linked {
		resp.Repos = append(resp.Repos, linkedRepoInfo(lr, false))
	}
	return resp
}

func linkedRepoInfo(lr LinkedRepo, primary bool) LinkedRepoInfo {
	name := lr.Name
	if name == "" {
		name = repoName(lr.Index)
	}
	lr.Index.mu.Lock()
	root := lr.Index.Repo.Root
	files := len(lr.Index.Files)
	symbols := len(lr.Index.Symbols)
	lr.Index.mu.Unlock()
	return LinkedRepoInfo{
		Name:    name,
		Root:    root,
		Primary: primary,
		Files:   files,
		Symbols: symbols,
	}
}

// FindRoute resolves an HTTP route (e.g. "GET /users/:id" or "/users/:id")
// to its handler(s) ("http-route" symbols) and caller(s) ("http-endpoint"
// symbols) across the primary index and any linked repos. Route paths are
// normalized the same way framework.go's route resolution does, so
// "/users/:id", "/users/${id}", and "/users/123" all match each other.
//
// If query doesn't parse as "METHOD /path", it is tried as a bare event
// name instead (see findRouteByEventName) — this is backward compatible:
// every query that previously parsed as an HTTP route still behaves
// identically, since the event fallback only runs when parseRouteQuery
// fails.
func FindRoute(idx *Index, linked []LinkedRepo, query string) FindRouteResponse {
	resp := FindRouteResponse{Status: "not_found", Query: query, Handlers: []RouteMatch{}, Callers: []RouteMatch{}}
	method, path, ok := parseRouteQuery(query)
	if !ok {
		return findRouteByEventName(idx, linked, query)
	}
	resp.Method = method
	resp.Path = path
	want := normalizeHTTPRoutePath(path)
	if want == "" {
		return resp
	}

	repos := append([]LinkedRepo{{Name: repoName(idx), Index: idx}}, linked...)
	for _, repo := range repos {
		name := repo.Name
		if name == "" {
			name = repoName(repo.Index)
		}
		snap := repo.Index.snapshot()
		for _, sym := range snap.Symbols {
			switch sym.Kind {
			case "http-route", "http-endpoint":
			default:
				continue
			}
			routeMethod, _, ok := splitHTTPRouteName(sym.Name)
			if !ok || routeMethod != method || !httpRouteNameMatchesPathPhrase(sym.Name, path) {
				continue
			}
			match := RouteMatch{Repo: name, Symbol: summarizeSymbol(sym), Kind: "http"}
			if sym.Kind == "http-route" {
				resp.Handlers = append(resp.Handlers, match)
			} else {
				resp.Callers = append(resp.Callers, match)
			}
		}
	}

	sortRouteMatches(resp.Handlers)
	sortRouteMatches(resp.Callers)
	if len(resp.Handlers) > 0 || len(resp.Callers) > 0 {
		resp.Status = "ok"
	}
	return resp
}

// findRouteByEventName resolves query as a bare event name (the same key
// space TraceEvent/ListEvents use, "event:<name>" with the prefix optional)
// against listens-event ("handler") and emits-event ("caller") sites across
// the primary index and any linked repos. Listens map to Handlers and emits
// map to Callers, mirroring HTTP's "caller triggers, handler handles"
// relationship: an emitter triggers the event, a listener handles it.
func findRouteByEventName(idx *Index, linked []LinkedRepo, query string) FindRouteResponse {
	resp := FindRouteResponse{Status: "not_found", Query: query, Handlers: []RouteMatch{}, Callers: []RouteMatch{}}
	key := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(query), eventTargetPrefix))
	if key == "" {
		return resp
	}
	// "<dynamic>" is the reserved key for computed/unknown event names; it is
	// never a real event, and matching it would wrongly join every
	// dynamically-named emit/listen edge across all repos (mirrors the same
	// guard in findCrossRepoEventEdges).
	if key == "<dynamic>" {
		return resp
	}
	target := eventTargetPrefix + key

	repos := append([]LinkedRepo{{Name: repoName(idx), Index: idx}}, linked...)
	for _, repo := range repos {
		name := repo.Name
		if name == "" {
			name = repoName(repo.Index)
		}
		snap := repo.Index.snapshot()
		snap.forEachEdge(func(_ int, edge CGPEdge) bool {
			if !strings.EqualFold(edge.To, target) {
				return true
			}
			sym, ok := snap.Symbols[edge.From]
			if !ok {
				return true
			}
			match := RouteMatch{Repo: name, Symbol: summarizeSymbol(sym), Kind: "event"}
			switch edge.Type {
			case EdgeListensEvent:
				resp.Handlers = append(resp.Handlers, match)
			case EdgeEmitsEvent:
				resp.Callers = append(resp.Callers, match)
			}
			return true
		})
	}

	sortRouteMatches(resp.Handlers)
	sortRouteMatches(resp.Callers)
	if len(resp.Handlers) > 0 || len(resp.Callers) > 0 {
		resp.Status = "ok"
	}
	return resp
}

// FindAllCrossRepoEdges finds every HTTP and event-based coupling across
// the full repo set (primary + every linked repo, not just primary-vs-each-
// linked) — used by CrossRepoArchitecture to see the whole cross-repo graph
// at once instead of answering one route/event query at a time like
// FindRoute does.
func FindAllCrossRepoEdges(idx *Index, linked []LinkedRepo) []CrossRepoEdge {
	repos := append([]LinkedRepo{{Name: repoName(idx), Index: idx}}, linked...)
	var out []CrossRepoEdge
	out = append(out, findCrossRepoHTTPEdges(repos)...)
	out = append(out, findCrossRepoEventEdges(repos)...)
	sortCrossRepoEdges(out)
	return out
}

func findCrossRepoHTTPEdges(repos []LinkedRepo) []CrossRepoEdge {
	type routeSite struct {
		repo   string
		method string
		path   string
		sym    CGPSymbol
	}
	var handlers, callers []routeSite
	for _, repo := range repos {
		name := repo.Name
		if name == "" {
			name = repoName(repo.Index)
		}
		snap := repo.Index.snapshot()
		for _, sym := range snap.Symbols {
			method, path, ok := splitHTTPRouteName(sym.Name)
			if !ok {
				continue
			}
			switch sym.Kind {
			case "http-route":
				handlers = append(handlers, routeSite{repo: name, method: method, path: path, sym: sym})
			case "http-endpoint":
				callers = append(callers, routeSite{repo: name, method: method, path: path, sym: sym})
			}
		}
	}
	var out []CrossRepoEdge
	for _, caller := range callers {
		for _, handler := range handlers {
			if caller.repo == handler.repo || caller.method != handler.method {
				continue
			}
			if !httpRouteNameMatchesPathPhrase(handler.sym.Name, caller.path) {
				continue
			}
			out = append(out, CrossRepoEdge{
				FromRepo: caller.repo, ToRepo: handler.repo,
				From: summarizeSymbol(caller.sym), To: summarizeSymbol(handler.sym),
				Kind: "http", Confidence: ConfScoped,
			})
		}
	}
	return out
}

func findCrossRepoEventEdges(repos []LinkedRepo) []CrossRepoEdge {
	type eventSite struct {
		repo string
		name string
		sym  CGPSymbol
	}
	var emits, listens []eventSite
	for _, repo := range repos {
		name := repo.Name
		if name == "" {
			name = repoName(repo.Index)
		}
		snap := repo.Index.snapshot()
		snap.forEachEdge(func(_ int, edge CGPEdge) bool {
			if !strings.HasPrefix(edge.To, eventTargetPrefix) {
				return true
			}
			key := strings.TrimPrefix(edge.To, eventTargetPrefix)
			if key == "" || key == "<dynamic>" {
				return true
			}
			sym, ok := snap.Symbols[edge.From]
			if !ok {
				return true
			}
			site := eventSite{repo: name, name: key, sym: sym}
			switch edge.Type {
			case EdgeEmitsEvent:
				emits = append(emits, site)
			case EdgeListensEvent:
				listens = append(listens, site)
			}
			return true
		})
	}
	var out []CrossRepoEdge
	for _, emit := range emits {
		for _, listen := range listens {
			if emit.repo == listen.repo || !strings.EqualFold(emit.name, listen.name) {
				continue
			}
			out = append(out, CrossRepoEdge{
				FromRepo: emit.repo, ToRepo: listen.repo,
				From: summarizeSymbol(emit.sym), To: summarizeSymbol(listen.sym),
				Kind: "event", Confidence: ConfScoped,
			})
		}
	}
	return out
}

func sortCrossRepoEdges(edges []CrossRepoEdge) {
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].FromRepo != edges[j].FromRepo {
			return edges[i].FromRepo < edges[j].FromRepo
		}
		if edges[i].From.File != edges[j].From.File {
			return edges[i].From.File < edges[j].From.File
		}
		if edges[i].From.StartLine != edges[j].From.StartLine {
			return edges[i].From.StartLine < edges[j].From.StartLine
		}
		return edges[i].ToRepo < edges[j].ToRepo
	})
}

func sortRouteMatches(matches []RouteMatch) {
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Repo != matches[j].Repo {
			return matches[i].Repo < matches[j].Repo
		}
		if matches[i].Symbol.File != matches[j].Symbol.File {
			return matches[i].Symbol.File < matches[j].Symbol.File
		}
		return matches[i].Symbol.StartLine < matches[j].Symbol.StartLine
	})
}

// parseRouteQuery splits a route query into an HTTP method and path. The
// method defaults to GET when the query is a bare path (e.g. "/users/:id").
func parseRouteQuery(query string) (method, path string, ok bool) {
	query = strings.TrimSpace(query)
	if query == "" {
		return "", "", false
	}
	if parts := strings.SplitN(query, " ", 2); len(parts) == 2 && httpMethods[strings.ToLower(parts[0])] {
		return strings.ToUpper(parts[0]), strings.TrimSpace(parts[1]), true
	}
	if strings.HasPrefix(query, "/") {
		return "GET", query, true
	}
	return "", "", false
}

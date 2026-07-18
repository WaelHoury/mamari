package mamari

import "testing"

func TestFindRouteAcrossLinkedRepos(t *testing.T) {
	backendRoot := t.TempDir()
	write(t, backendRoot, "src/routes.ts", `app.get('/users/:id', getUser)
function getUser(req, res) {
  res.json({})
}
`)
	backend, err := BuildIndex(backendRoot)
	if err != nil {
		t.Fatal(err)
	}

	frontendRoot := t.TempDir()
	write(t, frontendRoot, "src/api.ts", "export function loadUser(id: string) {\n  return axios.get(`/users/${id}`)\n}\n")
	frontend, err := BuildIndex(frontendRoot)
	if err != nil {
		t.Fatal(err)
	}

	// Within the frontend's own index, the route is unresolved (no backend
	// handler present) — find_route with no linked repos should report only
	// the caller-side http-endpoint.
	resp := FindRoute(frontend, nil, "GET /users/:id")
	if resp.Status != "ok" {
		t.Fatalf("expected ok status with caller-only match, got %#v", resp)
	}
	if len(resp.Handlers) != 0 {
		t.Fatalf("expected no handlers without a linked backend repo, got %#v", resp.Handlers)
	}
	if len(resp.Callers) != 1 {
		t.Fatalf("expected one caller, got %#v", resp.Callers)
	}

	// Linking the backend repo should surface its handler too.
	linked := []LinkedRepo{{Name: "backend", Index: backend}}
	resp = FindRoute(frontend, linked, "GET /users/:id")
	if resp.Status != "ok" {
		t.Fatalf("expected ok status, got %#v", resp)
	}
	if len(resp.Handlers) != 1 {
		t.Fatalf("expected one handler from the linked backend repo, got %#v", resp.Handlers)
	}
	if resp.Handlers[0].Repo != "backend" {
		t.Fatalf("expected handler tagged with repo 'backend', got %q", resp.Handlers[0].Repo)
	}
	if resp.Handlers[0].Symbol.Kind != "http-route" {
		t.Fatalf("expected http-route handler symbol, got %#v", resp.Handlers[0].Symbol)
	}
	if len(resp.Callers) != 1 || resp.Callers[0].Repo != repoName(frontend) {
		t.Fatalf("expected one caller from the primary (frontend) repo, got %#v", resp.Callers)
	}

	// A concrete-valued path segment ("123") should still resolve to the
	// parameterized route handler ("/users/:id"), per FindRoute's docstring.
	resp = FindRoute(frontend, linked, "GET /users/123")
	if resp.Status != "ok" || len(resp.Handlers) != 1 {
		t.Fatalf("expected /users/123 to match /users/:id handler, got %#v", resp)
	}

	// No match for an unrelated route.
	resp = FindRoute(frontend, linked, "GET /unrelated")
	if resp.Status != "not_found" {
		t.Fatalf("expected not_found for an unrelated route, got %#v", resp)
	}

	// list_linked_repos reports the primary repo and the linked backend.
	list := ListLinkedRepos(frontend, linked)
	if list.Status != "ok" || len(list.Repos) != 2 {
		t.Fatalf("expected 2 repos listed, got %#v", list)
	}
	if !list.Repos[0].Primary || list.Repos[1].Primary {
		t.Fatalf("expected first repo to be marked primary, got %#v", list.Repos)
	}
	if list.Repos[1].Name != "backend" {
		t.Fatalf("expected linked repo named 'backend', got %q", list.Repos[1].Name)
	}
}

// TestFindAllCrossRepoEdgesAcrossThreeRepos covers pairwise-across-the-full-
// set matching (not just primary-vs-each-linked): an HTTP edge between
// frontend and backend, plus an event edge between backend and worker
// (neither of which is the primary repo) must both be found.
func TestFindAllCrossRepoEdgesAcrossThreeRepos(t *testing.T) {
	frontendRoot := t.TempDir()
	write(t, frontendRoot, "src/api.ts", "export function loadUser(id) {\n  return axios.get(`/users/${id}`)\n}\n")
	frontend, err := BuildIndex(frontendRoot)
	if err != nil {
		t.Fatal(err)
	}

	backendRoot := t.TempDir()
	write(t, backendRoot, "src/routes.js", `app.get('/users/:id', getUser)
function getUser(req, res) {
  res.json({})
}
function onUserFetched() {
  bus.emit('user.fetched', {})
}
`)
	backend, err := BuildIndex(backendRoot)
	if err != nil {
		t.Fatal(err)
	}

	workerRoot := t.TempDir()
	write(t, workerRoot, "src/listener.js", `function setup() {
  bus.on('user.fetched', handleUserFetched)
}
function handleUserFetched() {
  return 1
}
`)
	worker, err := BuildIndex(workerRoot)
	if err != nil {
		t.Fatal(err)
	}

	linked := []LinkedRepo{{Name: "backend", Index: backend}, {Name: "worker", Index: worker}}
	edges := FindAllCrossRepoEdges(frontend, linked)

	hasEdge := func(fromRepo, toRepo, kind string) bool {
		for _, e := range edges {
			if e.FromRepo == fromRepo && e.ToRepo == toRepo && e.Kind == kind {
				return true
			}
		}
		return false
	}
	if !hasEdge(repoName(frontend), "backend", "http") {
		t.Fatalf("expected an http cross-repo edge from the primary (frontend) repo to backend, got %#v", edges)
	}
	if !hasEdge("backend", "worker", "event") {
		t.Fatalf("expected an event cross-repo edge from backend to worker (neither of which is the primary repo), got %#v", edges)
	}
}

// TestCrossRepoArchitectureReportsMultiRepoCommunity covers
// CrossRepoArchitecture: a community whose members are connected only by
// cross-repo edges must report Repos with every spanned repo, while a
// completely disconnected repo gets its own single-repo community.
func TestCrossRepoArchitectureReportsMultiRepoCommunity(t *testing.T) {
	frontendRoot := t.TempDir()
	write(t, frontendRoot, "src/api.ts", "export function loadUser(id) {\n  return axios.get(`/users/${id}`)\n}\n")
	frontend, err := BuildIndex(frontendRoot)
	if err != nil {
		t.Fatal(err)
	}

	backendRoot := t.TempDir()
	write(t, backendRoot, "src/routes.js", `app.get('/users/:id', getUser)
function getUser(req, res) {
  bus.emit('user.fetched', {})
}
`)
	backend, err := BuildIndex(backendRoot)
	if err != nil {
		t.Fatal(err)
	}

	workerRoot := t.TempDir()
	write(t, workerRoot, "src/listener.js", `function setup() {
  bus.on('user.fetched', handleUserFetched)
}
function handleUserFetched() {
  return 1
}
`)
	worker, err := BuildIndex(workerRoot)
	if err != nil {
		t.Fatal(err)
	}

	isolatedRoot := t.TempDir()
	write(t, isolatedRoot, "src/standalone.js", "function standalone() {\n  return 1\n}\n")
	isolated, err := BuildIndex(isolatedRoot)
	if err != nil {
		t.Fatal(err)
	}

	linked := []LinkedRepo{
		{Name: "backend", Index: backend},
		{Name: "worker", Index: worker},
		{Name: "isolated", Index: isolated},
	}
	resp := CrossRepoArchitecture(frontend, linked, RepoMapOptions{})
	if resp.Status != "ok" {
		t.Fatalf("expected ok status, got %#v", resp)
	}
	if len(resp.Repos) != 4 {
		t.Fatalf("expected 4 repos listed, got %#v", resp.Repos)
	}
	if len(resp.Edges) == 0 {
		t.Fatalf("expected at least the http and event cross-repo edges, got none")
	}

	var multiRepoCommunity, isolatedCommunity *RepoCommunity
	for i := range resp.Communities {
		c := &resp.Communities[i]
		if len(c.Repos) > 1 {
			multiRepoCommunity = c
		}
		for _, r := range c.Repos {
			if r == "isolated" {
				isolatedCommunity = c
			}
		}
	}
	if multiRepoCommunity == nil {
		t.Fatalf("expected at least one community spanning more than one repo, got %#v", resp.Communities)
	}
	frontendName := repoName(frontend)
	for _, want := range []string{frontendName, "backend", "worker"} {
		found := false
		for _, r := range multiRepoCommunity.Repos {
			if r == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected the multi-repo community to include %q, got %#v", want, multiRepoCommunity.Repos)
		}
	}
	if isolatedCommunity == nil {
		t.Fatalf("expected the disconnected 'isolated' repo to appear in some community, got %#v", resp.Communities)
	}
	if len(isolatedCommunity.Repos) != 1 {
		t.Fatalf("expected the disconnected 'isolated' repo's community to span exactly 1 repo (no cross-repo edges connect to it), got %#v", isolatedCommunity.Repos)
	}
}

func TestParseRouteQuery(t *testing.T) {
	cases := []struct {
		in     string
		method string
		path   string
		ok     bool
	}{
		{"GET /users/:id", "GET", "/users/:id", true},
		{"post /users", "POST", "/users", true},
		{"/users/:id", "GET", "/users/:id", true},
		{"", "", "", false},
		{"not-a-route", "", "", false},
	}
	for _, c := range cases {
		method, path, ok := parseRouteQuery(c.in)
		if method != c.method || path != c.path || ok != c.ok {
			t.Errorf("parseRouteQuery(%q) = (%q, %q, %v), want (%q, %q, %v)", c.in, method, path, ok, c.method, c.path, c.ok)
		}
	}
}

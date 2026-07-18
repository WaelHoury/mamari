package mamari

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGraphUIHandlerServesBoundedGraphAndSymbolContext(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/app.ts", `export function saveOrder() { validateOrder() }
export function validateOrder() { return true }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewGraphUIHandler(idx)

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `data-testid="graph-search"`) {
		t.Fatalf("UI page response = %d %q", page.Code, page.Body.String())
	}
	if page.Header().Get("Content-Security-Policy") == "" || page.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("missing browser security headers: %#v", page.Header())
	}

	graph := httptest.NewRecorder()
	handler.ServeHTTP(graph, httptest.NewRequest(http.MethodGet, "/api/graph?q=saveOrder&limit=20", nil))
	if graph.Code != http.StatusOK {
		t.Fatalf("graph response status = %d: %s", graph.Code, graph.Body.String())
	}
	var response GraphUIResponse
	if err := json.Unmarshal(graph.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Nodes) == 0 || len(response.Nodes) > 20 || response.Stats.Symbols == 0 {
		t.Fatalf("unexpected graph packet: %#v", response)
	}
	var saveID string
	for _, node := range response.Nodes {
		if node.Name == "saveOrder" {
			saveID = node.ID
		}
	}
	if saveID == "" {
		t.Fatalf("saveOrder missing from focused graph: %#v", response.Nodes)
	}

	symbol := httptest.NewRecorder()
	handler.ServeHTTP(symbol, httptest.NewRequest(http.MethodGet, "/api/symbol?id="+saveID, nil))
	if symbol.Code != http.StatusOK || !strings.Contains(symbol.Body.String(), "validateOrder") {
		t.Fatalf("symbol response = %d %s", symbol.Code, symbol.Body.String())
	}
}

func TestGraphUIRejectsMissingSymbolIDAndUnknownPaths(t *testing.T) {
	idx, err := BuildIndex(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler := NewGraphUIHandler(idx)
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/api/symbol", nil))
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("missing id status = %d", missing.Code)
	}
	unknown := httptest.NewRecorder()
	handler.ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/not-found", nil))
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown path status = %d", unknown.Code)
	}
}

func TestGraphUIAggregateViewsAndOverlay(t *testing.T) {
	root := t.TempDir()
	write(t, root, "pkg/a.js", `function alpha() { return beta() }
module.exports = { alpha }
`)
	write(t, root, "lib/b.js", `function beta() { return 1 }
function orphanInLib() { return 2 }
module.exports = { beta }
`)
	write(t, root, "pkg/a.test.js", `const { alpha } = require('../pkg/a')
test('a', () => alpha())
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewGraphUIHandler(idx)

	get := func(url string) GraphUIResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s -> %d: %s", url, rec.Code, rec.Body.String())
		}
		var resp GraphUIResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: bad json: %v", url, err)
		}
		return resp
	}

	// Packages view: one node per directory, dir: IDs, aggregate counts.
	pkgs := get("/api/graph?view=packages")
	dirs := map[string]GraphUINode{}
	for _, n := range pkgs.Nodes {
		if !strings.HasPrefix(n.ID, "dir:") {
			t.Fatalf("packages view node without dir: prefix: %+v", n)
		}
		dirs[n.File] = n
	}
	if dirs["pkg"].SymbolCount == 0 || dirs["lib"].SymbolCount == 0 {
		t.Fatalf("expected pkg and lib aggregate nodes, got %+v", pkgs.Nodes)
	}
	// The cross-package call alpha->beta must appear as an aggregate edge.
	foundCross := false
	for _, e := range pkgs.Edges {
		if e.Type == "calls" && e.Count >= 1 && strings.HasPrefix(e.From, "dir:") {
			foundCross = true
		}
	}
	if !foundCross {
		t.Fatalf("expected an aggregate calls edge between packages, got %+v", pkgs.Edges)
	}

	// Files view focused on one dir: only files under it.
	files := get("/api/graph?view=files&focus=pkg")
	for _, n := range files.Nodes {
		if !strings.HasPrefix(n.File, "pkg") {
			t.Fatalf("files view with focus=pkg leaked %q", n.File)
		}
	}

	// Symbol drill-down by file: in-file symbols + one-hop neighbors.
	syms := get("/api/graph?file=lib/b.js")
	inFile, neighbors := 0, 0
	for _, n := range syms.Nodes {
		if n.File == "lib/b.js" {
			inFile++
		} else {
			neighbors++
		}
	}
	if inFile == 0 {
		t.Fatalf("file drill-down returned no in-file symbols: %+v", syms.Nodes)
	}
	if neighbors == 0 {
		t.Fatalf("file drill-down should include one-hop neighbors (alpha calls beta)")
	}

	// Health overlay marks the orphan dead and flags untested symbols.
	overlaid := get("/api/graph?file=lib/b.js&overlay=health")
	sawDead := false
	for _, n := range overlaid.Nodes {
		if n.Name == "orphanInLib" && n.Dead {
			sawDead = true
		}
	}
	if !sawDead {
		t.Fatalf("overlay should mark orphanInLib dead: %+v", overlaid.Nodes)
	}

	// Default view (no params) must not carry aggregate/overlay fields.
	def := get("/api/graph?limit=20")
	for _, n := range def.Nodes {
		if n.SymbolCount != 0 || n.Dead || n.UntestedCount != 0 {
			t.Fatalf("default payload gained new nonzero fields: %+v", n)
		}
	}
}

// Aggregate views must be byte-deterministic (dominant language is computed,
// not last-seen under map iteration), imports must roll up as package edges,
// and focus=. must reach root-level files.
func TestGraphUIAggregateDeterminismImportsAndRootFocus(t *testing.T) {
	root := t.TempDir()
	write(t, root, "rootfile.js", `function rootHelper() { return 1 }
module.exports = { rootHelper }
`)
	write(t, root, "app/main.js", `const { util } = require('../lib/util')
function main() { return util() }
module.exports = { main }
`)
	write(t, root, "lib/util.js", `export function util() { return 2 }`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewGraphUIHandler(idx)
	get := func(url string) string {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s -> %d", url, rec.Code)
		}
		return rec.Body.String()
	}
	// Byte determinism across repeated identical requests.
	first := get("/api/graph?view=packages")
	for i := 0; i < 5; i++ {
		if got := get("/api/graph?view=packages"); got != first {
			t.Fatalf("aggregate view output nondeterministic on request %d", i+2)
		}
	}
	// Imports roll up: app -> lib exists as an aggregate imports edge.
	var resp GraphUIResponse
	if err := json.Unmarshal([]byte(get("/api/graph?view=packages&edge=imports")), &resp); err != nil {
		t.Fatal(err)
	}
	foundImport := false
	for _, e := range resp.Edges {
		if e.Type == "imports" && e.From == "dir:app" && e.To == "dir:lib" {
			foundImport = true
		}
	}
	if !foundImport {
		t.Fatalf("expected aggregate imports edge dir:app -> dir:lib, got %+v", resp.Edges)
	}
	// Root package (focus=.) reaches root-level files.
	if err := json.Unmarshal([]byte(get("/api/graph?view=files&focus=.")), &resp); err != nil {
		t.Fatal(err)
	}
	sawRoot := false
	for _, n := range resp.Nodes {
		if n.File == "rootfile.js" {
			sawRoot = true
		}
		if strings.Contains(n.File, "/") {
			t.Fatalf("focus=. leaked non-root file %q", n.File)
		}
	}
	if !sawRoot {
		t.Fatalf("focus=. must include rootfile.js, got %+v", resp.Nodes)
	}
}

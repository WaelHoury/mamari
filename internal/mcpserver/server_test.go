package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/waelhoury/mamari/internal/mamari"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// writeFile writes content to root/rel, creating parent directories as needed.
func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixtureIndex builds a small TS index with a helper/caller pair, used by
// the edit_symbol and manage_notes dispatch tests.
func fixtureIndex(t *testing.T) *mamari.Index {
	root := t.TempDir()
	writeFile(t, root, "src/util.ts", `export function helper(): number {
  return 1
}

export function caller(): number {
  return helper() + helper()
}
`)
	idx, err := mamari.BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

// symbolID returns the id of the first symbol with the given name, failing
// the test if none is found.
func symbolID(t *testing.T, idx *mamari.Index, name string) string {
	t.Helper()
	for id, sym := range idx.Symbols {
		if sym.Name == name {
			return id
		}
	}
	t.Fatalf("no symbol named %q in index", name)
	return ""
}

// startClient builds an in-process MCP client against idx/linked with the
// full tool surface registered (ServeOptions.FullToolset: true), initializes
// it, and registers cleanup. Existing dispatch tests want every tool
// reachable regardless of index content, so they use this helper; tests of
// the gating behavior itself use startClientWithOptions directly.
func startClient(t *testing.T, idx *mamari.Index, linked []mamari.LinkedRepo) *client.Client {
	t.Helper()
	return startClientWithOptions(t, idx, linked, ServeOptions{FullToolset: true})
}

// startClientWithOptions is startClient with caller-controlled ServeOptions,
// for tests that need to exercise the default (non-full) tool gating.
func startClientWithOptions(t *testing.T, idx *mamari.Index, linked []mamari.LinkedRepo, opts ServeOptions) *client.Client {
	t.Helper()
	s := newMCPServer(idx, linked, opts)
	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("client.Start: %v", err)
	}
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "test-client", Version: "1.0.0"}
	if _, err := c.Initialize(context.Background(), initReq); err != nil {
		t.Fatalf("client.Initialize: %v", err)
	}
	return c
}

func TestInitializeReportsConfiguredServerVersion(t *testing.T) {
	s := newMCPServer(fixtureIndex(t), nil, ServeOptions{ServerVersion: "v9.8.7"})
	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "test-client", Version: "1.0.0"}
	result, err := c.Initialize(context.Background(), initReq)
	if err != nil {
		t.Fatal(err)
	}
	if result.ServerInfo.Version != "v9.8.7" {
		t.Fatalf("initialize server version=%q, want v9.8.7", result.ServerInfo.Version)
	}
}

// callTool invokes name with arguments and returns the result text and
// whether the call was an error result.
func callTool(t *testing.T, c *client.Client, name string, arguments map[string]any) (string, bool) {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = arguments
	result, err := c.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	if len(result.Content) == 0 {
		t.Fatalf("CallTool(%s): empty content", name)
	}
	text, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("CallTool(%s): expected text content, got %#v", name, result.Content[0])
	}
	return text.Text, result.IsError
}

func TestTraceTermFormatDispatch(t *testing.T) {
	idx := fixtureIndex(t)
	c := startClient(t, idx, nil)

	// format=full (the default) includes the raw "edges" field.
	text, isErr := callTool(t, c, "trace_term", map[string]any{"term": "dcterms:identifier"})
	if isErr {
		t.Fatalf("format=full returned error: %s", text)
	}
	var full map[string]any
	if err := json.Unmarshal([]byte(text), &full); err != nil {
		t.Fatalf("unmarshal full response: %v", err)
	}
	if _, ok := full["edges"]; !ok {
		t.Fatalf("format=full response missing %q field: %s", "edges", text)
	}

	// format=compact omits "edges" but includes "ttlUsageCount", with
	// "ttlUsages" as a flat array.
	text, isErr = callTool(t, c, "trace_term", map[string]any{"term": "dcterms:identifier", "format": "compact"})
	if isErr {
		t.Fatalf("format=compact returned error: %s", text)
	}
	var compact map[string]any
	if err := json.Unmarshal([]byte(text), &compact); err != nil {
		t.Fatalf("unmarshal compact response: %v", err)
	}
	if _, ok := compact["edges"]; ok {
		t.Fatalf("format=compact response unexpectedly has %q field: %s", "edges", text)
	}
	if _, ok := compact["ttlUsageCount"]; !ok {
		t.Fatalf("format=compact response missing %q field: %s", "ttlUsageCount", text)
	}
	if _, ok := compact["ttlUsages"].([]any); !ok {
		t.Fatalf("format=compact response expected ttlUsages to be an array: %s", text)
	}

	// format=grouped also has "ttlUsageCount", but "ttlUsages" is a map
	// keyed by file rather than a flat array.
	text, isErr = callTool(t, c, "trace_term", map[string]any{"term": "dcterms:identifier", "format": "grouped"})
	if isErr {
		t.Fatalf("format=grouped returned error: %s", text)
	}
	var grouped map[string]any
	if err := json.Unmarshal([]byte(text), &grouped); err != nil {
		t.Fatalf("unmarshal grouped response: %v", err)
	}
	if _, ok := grouped["ttlUsageCount"]; !ok {
		t.Fatalf("format=grouped response missing %q field: %s", "ttlUsageCount", text)
	}
	if ttlUsages, ok := grouped["ttlUsages"]; !ok {
		t.Fatalf("format=grouped response missing %q field: %s", "ttlUsages", text)
	} else if _, isMap := ttlUsages.(map[string]any); !isMap && ttlUsages != nil {
		t.Fatalf("format=grouped response expected ttlUsages to be a map, got %T: %s", ttlUsages, text)
	}

	// An invalid format is rejected with a descriptive error.
	text, isErr = callTool(t, c, "trace_term", map[string]any{"term": "dcterms:identifier", "format": "bogus"})
	if !isErr {
		t.Fatalf("format=bogus expected an error result, got: %s", text)
	}
	if want := `format must be one of full|compact|grouped, got "bogus"`; text != want {
		t.Fatalf("format=bogus error = %q, want %q", text, want)
	}
}

func TestDoctorDispatchBoundsParseFailureExamples(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 12; i++ {
		writeFile(t, root, "src/broken-"+strconv.Itoa(i)+".ts", "function broken() { if (true) { return 1 }\n")
	}
	idx, err := mamari.BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if total := mamari.Doctor(idx).ParseFailureTotal; total < 12 {
		t.Fatalf("fixture produced %d parse failures, want at least 12", total)
	}

	named := startClient(t, idx, nil)
	text, isError := callTool(t, named, "doctor", map[string]any{"parse_failure_limit": 3})
	if isError {
		t.Fatalf("named doctor returned an error: %s", text)
	}
	var namedReport mamari.DoctorReport
	if err := json.Unmarshal([]byte(text), &namedReport); err != nil {
		t.Fatal(err)
	}
	if len(namedReport.ParseFailures) != 3 || !namedReport.ParseFailuresTruncated {
		t.Fatalf("named doctor did not enforce limit: %#v", namedReport)
	}
	if namedReport.ParseFailureTotal < 12 {
		t.Fatalf("named doctor lost total count: %#v", namedReport)
	}

	slim := startClientWithOptions(t, idx, nil, ServeOptions{})
	text, isError = callTool(t, slim, "mamari", map[string]any{
		"action":    "doctor",
		"args_json": `{"parse_failure_limit":4}`,
	})
	if isError {
		t.Fatalf("slim doctor returned an error: %s", text)
	}
	var slimReport mamari.DoctorReport
	if err := json.Unmarshal([]byte(text), &slimReport); err != nil {
		t.Fatal(err)
	}
	if len(slimReport.ParseFailures) != 4 || !slimReport.ParseFailuresTruncated {
		t.Fatalf("slim doctor did not enforce limit: %#v", slimReport)
	}
	if slimReport.ParseFailureTotal != namedReport.ParseFailureTotal {
		t.Fatalf("doctor surfaces disagree on total: named=%d slim=%d", namedReport.ParseFailureTotal, slimReport.ParseFailureTotal)
	}
}

func TestEditSymbolDispatch(t *testing.T) {
	idx := fixtureIndex(t)
	c := startClient(t, idx, nil)

	// operation=rename requires new_name.
	text, isErr := callTool(t, c, "edit_symbol", map[string]any{"query": "helper", "operation": "rename"})
	if !isErr || text != "operation=rename requires new_name" {
		t.Fatalf("rename without new_name: isErr=%v text=%q", isErr, text)
	}

	// operation=rename with new_name succeeds and reports both call sites.
	text, isErr = callTool(t, c, "edit_symbol", map[string]any{"query": "helper", "operation": "rename", "new_name": "helperRenamed"})
	if isErr {
		t.Fatalf("rename returned error: %s", text)
	}
	var renamed mamari.EditPlanResponse
	if err := json.Unmarshal([]byte(text), &renamed); err != nil {
		t.Fatalf("unmarshal rename response: %v", err)
	}
	if renamed.Status != "ok" {
		t.Fatalf("rename status = %q, want ok: %s", renamed.Status, text)
	}
	if renamed.FilesAffected != 1 {
		t.Fatalf("rename FilesAffected = %d, want 1: %s", renamed.FilesAffected, text)
	}

	// operation=replace_body requires new_body.
	text, isErr = callTool(t, c, "edit_symbol", map[string]any{"query": "helper", "operation": "replace_body"})
	if !isErr || text != "operation=replace_body requires new_body" {
		t.Fatalf("replace_body without new_body: isErr=%v text=%q", isErr, text)
	}

	// operation=insert_after requires text.
	text, isErr = callTool(t, c, "edit_symbol", map[string]any{"query": "helper", "operation": "insert_after"})
	if !isErr || text != "operation=insert_after requires text" {
		t.Fatalf("insert_after without text: isErr=%v text=%q", isErr, text)
	}

	// An invalid operation is rejected with a descriptive error.
	text, isErr = callTool(t, c, "edit_symbol", map[string]any{"query": "helper", "operation": "bogus"})
	if !isErr {
		t.Fatalf("operation=bogus expected an error result, got: %s", text)
	}
	if want := `operation must be one of rename|replace_body|insert_after, got "bogus"`; text != want {
		t.Fatalf("operation=bogus error = %q, want %q", text, want)
	}
}

func TestManageNotesDispatch(t *testing.T) {
	idx := fixtureIndex(t)
	c := startClient(t, idx, nil)
	helperID := symbolID(t, idx, "helper")

	// action=add requires symbol_id and text.
	text, isErr := callTool(t, c, "manage_notes", map[string]any{"action": "add"})
	if !isErr || text != "action=add requires symbol_id and text" {
		t.Fatalf("add without symbol_id/text: isErr=%v text=%q", isErr, text)
	}

	// action=add with valid symbol_id and text succeeds.
	text, isErr = callTool(t, c, "manage_notes", map[string]any{"action": "add", "symbol_id": helperID, "text": "known race condition, see issue #123"})
	if isErr {
		t.Fatalf("add returned error: %s", text)
	}
	var added mamari.AddNoteResponse
	if err := json.Unmarshal([]byte(text), &added); err != nil {
		t.Fatalf("unmarshal add response: %v", err)
	}
	if added.Status != "ok" {
		t.Fatalf("add status = %q, want ok: %s", added.Status, text)
	}

	// action=list returns the note just added.
	text, isErr = callTool(t, c, "manage_notes", map[string]any{"action": "list", "symbol_id": helperID})
	if isErr {
		t.Fatalf("list returned error: %s", text)
	}
	var listed mamari.ListNotesResponse
	if err := json.Unmarshal([]byte(text), &listed); err != nil {
		t.Fatalf("unmarshal list response: %v", err)
	}
	if listed.Status != "ok" || listed.Total != 1 {
		t.Fatalf("list = %#v, want one note: %s", listed, text)
	}

	// action=remove requires id.
	text, isErr = callTool(t, c, "manage_notes", map[string]any{"action": "remove"})
	if !isErr {
		t.Fatalf("remove without id expected an error result, got: %s", text)
	}

	// action=remove with id succeeds.
	text, isErr = callTool(t, c, "manage_notes", map[string]any{"action": "remove", "id": float64(listed.Notes[0].ID)})
	if isErr {
		t.Fatalf("remove returned error: %s", text)
	}
	var removed mamari.RemoveNoteResponse
	if err := json.Unmarshal([]byte(text), &removed); err != nil {
		t.Fatalf("unmarshal remove response: %v", err)
	}
	if removed.Status != "ok" || !removed.Removed {
		t.Fatalf("remove = %#v, want removed: %s", removed, text)
	}

	// An invalid action is rejected with a descriptive error.
	text, isErr = callTool(t, c, "manage_notes", map[string]any{"action": "bogus"})
	if !isErr {
		t.Fatalf("action=bogus expected an error result, got: %s", text)
	}
	if want := `action must be one of add|list|remove, got "bogus"`; text != want {
		t.Fatalf("action=bogus error = %q, want %q", text, want)
	}
}

func TestManageADRDispatch(t *testing.T) {
	idx := fixtureIndex(t)
	c := startClient(t, idx, nil)

	// action=update requires title and content.
	text, isErr := callTool(t, c, "manage_adr", map[string]any{"action": "update", "title": "auth"})
	if !isErr || text != "action=update requires title and content" {
		t.Fatalf("update without content: isErr=%v text=%q", isErr, text)
	}

	// action=update with both succeeds.
	text, isErr = callTool(t, c, "manage_adr", map[string]any{"action": "update", "title": "auth-strategy", "content": "JWT with refresh tokens."})
	if isErr {
		t.Fatalf("update returned error: %s", text)
	}
	var updated mamari.ADRSectionResponse
	if err := json.Unmarshal([]byte(text), &updated); err != nil {
		t.Fatalf("unmarshal update response: %v", err)
	}
	if updated.Status != "ok" || updated.Section.Title != "auth-strategy" {
		t.Fatalf("update = %#v, want ok with the section: %s", updated, text)
	}

	// action=get (case-insensitive) returns the section with content.
	text, isErr = callTool(t, c, "manage_adr", map[string]any{"action": "get", "title": "Auth-Strategy"})
	if isErr {
		t.Fatalf("get returned error: %s", text)
	}
	var got mamari.ADRGetResponse
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("unmarshal get response: %v", err)
	}
	if got.Status != "ok" || len(got.Sections) != 1 || got.Sections[0].Content != "JWT with refresh tokens." {
		t.Fatalf("get = %#v, want the section with content: %s", got, text)
	}

	// action=list omits content.
	text, isErr = callTool(t, c, "manage_adr", map[string]any{"action": "list"})
	if isErr {
		t.Fatalf("list returned error: %s", text)
	}
	var listed mamari.ADRListResponse
	if err := json.Unmarshal([]byte(text), &listed); err != nil {
		t.Fatalf("unmarshal list response: %v", err)
	}
	if listed.Total != 1 || listed.Sections[0].Content != "" {
		t.Fatalf("list = %#v, want 1 section with no content: %s", listed, text)
	}

	// action=remove requires title.
	text, isErr = callTool(t, c, "manage_adr", map[string]any{"action": "remove"})
	if !isErr || text != "action=remove requires title" {
		t.Fatalf("remove without title: isErr=%v text=%q", isErr, text)
	}

	// action=remove with title succeeds.
	text, isErr = callTool(t, c, "manage_adr", map[string]any{"action": "remove", "title": "auth-strategy"})
	if isErr {
		t.Fatalf("remove returned error: %s", text)
	}
	var removed mamari.ADRRemoveResponse
	if err := json.Unmarshal([]byte(text), &removed); err != nil {
		t.Fatalf("unmarshal remove response: %v", err)
	}
	if removed.Status != "ok" || !removed.Removed {
		t.Fatalf("remove = %#v, want removed: %s", removed, text)
	}

	// An invalid action is rejected with a descriptive error.
	text, isErr = callTool(t, c, "manage_adr", map[string]any{"action": "bogus"})
	if !isErr {
		t.Fatalf("action=bogus expected an error result, got: %s", text)
	}
	if want := `action must be one of get|list|update|remove, got "bogus"`; text != want {
		t.Fatalf("action=bogus error = %q, want %q", text, want)
	}
}

// TestFindRouteAndCrossRepoArchitectureDispatch covers find_route's event
// fallback (a query that doesn't parse as an HTTP route is tried as a bare
// event name) and the new cross_repo_architecture tool, through the MCP
// dispatch layer rather than calling the Go functions directly.
func TestFindRouteAndCrossRepoArchitectureDispatch(t *testing.T) {
	backendRoot := t.TempDir()
	writeFile(t, backendRoot, "src/routes.js", `app.get('/users/:id', getUser)
function getUser(req, res) {
  bus.emit('user.fetched', {})
}
`)
	backend, err := mamari.BuildIndex(backendRoot)
	if err != nil {
		t.Fatal(err)
	}

	frontendRoot := t.TempDir()
	writeFile(t, frontendRoot, "src/api.ts", "export function loadUser(id) {\n  return axios.get(`/users/${id}`)\n}\n")
	writeFile(t, frontendRoot, "src/listener.js", `function setup() {
  bus.on('user.fetched', handleUserFetched)
}
function handleUserFetched() {
  return 1
}
`)
	frontend, err := mamari.BuildIndex(frontendRoot)
	if err != nil {
		t.Fatal(err)
	}

	linked := []mamari.LinkedRepo{{Name: "backend", Index: backend}}
	c := startClient(t, frontend, linked)

	// find_route with an HTTP-shaped query behaves as before.
	text, isErr := callTool(t, c, "find_route", map[string]any{"query": "GET /users/:id"})
	if isErr {
		t.Fatalf("find_route (http) returned error: %s", text)
	}
	var httpResp mamari.FindRouteResponse
	if err := json.Unmarshal([]byte(text), &httpResp); err != nil {
		t.Fatalf("unmarshal find_route response: %v", err)
	}
	if httpResp.Status != "ok" || len(httpResp.Handlers) != 1 || httpResp.Handlers[0].Kind != "http" {
		t.Fatalf("expected one http handler, got %#v: %s", httpResp, text)
	}

	// find_route with a bare event name falls back to event matching: the
	// frontend's bus.on listener is the "handler", the backend's bus.emit
	// (in a linked, non-primary repo) is the "caller".
	text, isErr = callTool(t, c, "find_route", map[string]any{"query": "user.fetched"})
	if isErr {
		t.Fatalf("find_route (event) returned error: %s", text)
	}
	var eventResp mamari.FindRouteResponse
	if err := json.Unmarshal([]byte(text), &eventResp); err != nil {
		t.Fatalf("unmarshal find_route event response: %v", err)
	}
	if eventResp.Status != "ok" || len(eventResp.Handlers) != 1 || eventResp.Handlers[0].Kind != "event" {
		t.Fatalf("expected one event handler (listener), got %#v: %s", eventResp, text)
	}
	if len(eventResp.Callers) != 1 || eventResp.Callers[0].Repo != "backend" {
		t.Fatalf("expected one event caller (emitter) from the linked backend repo, got %#v: %s", eventResp, text)
	}

	// cross_repo_architecture surfaces both the http and event cross-repo
	// edges between the primary (frontend) and linked backend repo.
	text, isErr = callTool(t, c, "cross_repo_architecture", map[string]any{})
	if isErr {
		t.Fatalf("cross_repo_architecture returned error: %s", text)
	}
	var arch mamari.CrossRepoArchitectureResponse
	if err := json.Unmarshal([]byte(text), &arch); err != nil {
		t.Fatalf("unmarshal cross_repo_architecture response: %v", err)
	}
	if arch.Status != "ok" || len(arch.Repos) != 2 {
		t.Fatalf("expected ok status with 2 repos, got %#v", arch)
	}
	hasKind := func(kind string) bool {
		for _, e := range arch.Edges {
			if e.Kind == kind {
				return true
			}
		}
		return false
	}
	if !hasKind("http") || !hasKind("event") {
		t.Fatalf("expected both http and event cross-repo edges, got %#v", arch.Edges)
	}
}

func TestQueryGraphDispatch(t *testing.T) {
	idx := fixtureIndex(t)
	c := startClient(t, idx, nil)

	text, isErr := callTool(t, c, "query_graph", map[string]any{
		"query": "MATCH (a:function)-[:calls]->(b:function) WHERE a.name = 'caller' RETURN a.name, b.name",
	})
	if isErr {
		t.Fatalf("query_graph returned error: %s", text)
	}
	var resp mamari.QueryGraphLiteResponse
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("unmarshal query_graph response: %v", err)
	}
	// caller() calls helper() twice, so two distinct call edges (call sites)
	// both match — Cypher MATCH semantics don't dedupe relationships.
	if resp.Status != "ok" || resp.Total != 2 {
		t.Fatalf("expected two caller->helper rows (one per call site), got %#v: %s", resp, text)
	}
	if resp.Rows[0]["a.name"] != "caller" || resp.Rows[0]["b.name"] != "helper" {
		t.Fatalf("expected caller->helper, got %#v", resp.Rows[0])
	}

	// max_rows is respected.
	text, isErr = callTool(t, c, "query_graph", map[string]any{
		"query":    "MATCH (f:function) RETURN f.name",
		"max_rows": float64(1),
	})
	if isErr {
		t.Fatalf("query_graph with max_rows returned error: %s", text)
	}
	var capped mamari.QueryGraphLiteResponse
	if err := json.Unmarshal([]byte(text), &capped); err != nil {
		t.Fatalf("unmarshal capped response: %v", err)
	}
	if len(capped.Rows) != 1 || !capped.Truncated {
		t.Fatalf("expected max_rows=1 to truncate to exactly 1 row, got %#v: %s", capped, text)
	}

	// A malformed query is rejected without erroring the tool call.
	text, isErr = callTool(t, c, "query_graph", map[string]any{"query": "NOT A QUERY"})
	if isErr {
		t.Fatalf("malformed query should not return a tool-call error, got: %s", text)
	}
	var invalid mamari.QueryGraphLiteResponse
	if err := json.Unmarshal([]byte(text), &invalid); err != nil {
		t.Fatalf("unmarshal invalid response: %v", err)
	}
	if invalid.Status != "invalid" || len(invalid.Warnings) == 0 {
		t.Fatalf("expected status=invalid with a warning for a malformed query, got %#v", invalid)
	}
}

// mutatingMCPTools names every registered tool with a genuine, real
// filesystem side effect: manage_notes (.mamari/notes.json) and manage_adr
// (.mamari/adr.json) are the only two — everything else, including
// edit_symbol despite its name, only computes a result (edit_symbol
// returns an edit *plan*; mamari never writes source files itself, see
// CLAUDE.md). Every other tool must report readOnlyHint=true.
var mutatingMCPTools = map[string]bool{
	"manage_notes": true,
	"manage_adr":   true,
}

// TestToolAnnotationsMatchActualSideEffects is a regression guard for a
// real, repo-wide MCP-protocol-level finding: every one of mamari's 34
// tools was registered with no explicit annotation options at all, so
// every single one silently inherited mcp-go's conservative library
// default of readOnlyHint=false/destructiveHint=true/openWorldHint=true —
// including pure CGP-graph queries like doctor, list_terms, and
// trace_symbol that have zero side effects. This is exactly the kind of
// thing an MCP client uses to decide whether to prompt for confirmation
// or auto-approve a call; marking every read as "destructive" actively
// misrepresents a tool surface whose entire design is "read-mostly" (see
// CLAUDE.md). Fixed by setting readOnlyHint/destructiveHint/
// idempotentHint/openWorldHint explicitly per tool. This test calls the
// real MCP ListTools RPC (not just reading the Go source) so it would
// catch a future tool registered without annotations, or an existing one
// whose annotation silently regresses back to the library default.
func TestToolAnnotationsMatchActualSideEffects(t *testing.T) {
	idx := fixtureIndex(t)
	c := startClient(t, idx, nil)
	res, err := c.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) == 0 {
		t.Fatal("expected at least one registered tool")
	}
	for _, tool := range res.Tools {
		ann := tool.Annotations
		wantReadOnly := !mutatingMCPTools[tool.Name]
		if ann.ReadOnlyHint == nil {
			t.Errorf("%s: ReadOnlyHint is unset (inherits the library's misleading default) — every tool must set it explicitly", tool.Name)
			continue
		}
		if *ann.ReadOnlyHint != wantReadOnly {
			t.Errorf("%s: ReadOnlyHint=%v, want %v", tool.Name, *ann.ReadOnlyHint, wantReadOnly)
		}
		if ann.DestructiveHint == nil || *ann.DestructiveHint != mutatingMCPTools[tool.Name] {
			t.Errorf("%s: DestructiveHint=%v, want %v", tool.Name, ann.DestructiveHint, mutatingMCPTools[tool.Name])
		}
		// Every mamari tool operates on a local, already-built index or
		// local sidecar files — none of them reach out to an open-ended
		// external system (network APIs, web search), so OpenWorldHint
		// must always be false, mutating tools included.
		if ann.OpenWorldHint == nil || *ann.OpenWorldHint {
			t.Errorf("%s: OpenWorldHint=%v, want false (mamari is entirely local)", tool.Name, ann.OpenWorldHint)
		}
	}
}

// ttlTools, eventTools, linkTools, watchTool, and adminTools are the tool
// names gated by hasTTL, hasEvents, hasLinked, hasWatch, and full
// respectively in newMCPServer — kept here, next to the gating tests, so a
// future change to which tools belong to which gate has one obvious place
// to update both the gate and the test that checks it.
var (
	ttlTools    = []string{"trace_term", "inspect_term", "list_terms", "search_literal", "find_containing_shape", "list_dynamic_iris"}
	eventTools  = []string{"trace_event", "list_events"}
	linkTools   = []string{"list_linked_repos", "cross_repo_architecture"}
	watchTool   = []string{"changed_since"}
	adminTools  = []string{"manage_notes", "manage_adr", "diff_index"}
	coreSamples = []string{"search_code", "trace_symbol", "impact", "repo_map", "find_route", "query_graph"}
)

// listToolNames returns the set of tool names the server currently exposes.
func listToolNames(t *testing.T, c *client.Client) map[string]bool {
	t.Helper()
	res, err := c.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool, len(res.Tools))
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	return names
}

func assertPresent(t *testing.T, names map[string]bool, want []string) {
	t.Helper()
	for _, n := range want {
		if !names[n] {
			t.Errorf("expected tool %q to be registered, but it was not. Registered: %v", n, names)
		}
	}
}

func assertAbsent(t *testing.T, names map[string]bool, want []string) {
	t.Helper()
	for _, n := range want {
		if names[n] {
			t.Errorf("expected tool %q to NOT be registered, but it was", n)
		}
	}
}

func TestSlimToolsetDefaultExposesOnlyPrimaryRouter(t *testing.T) {
	idx := fixtureIndex(t)
	c := startClientWithOptions(t, idx, nil, ServeOptions{})
	names := listToolNames(t, c)

	if len(names) != 1 || !names["mamari"] {
		t.Fatalf("default slim toolset should expose only the primary mamari tool, got %v", names)
	}

	res, err := c.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(res.Tools)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) >= 1100 {
		t.Fatalf("slim tools/list schema exceeded its 1100-byte regression ceiling, got %d bytes: %s", len(payload), payload)
	}

	text, isErr := callTool(t, c, "mamari", map[string]any{
		"action":    "trace",
		"query":     "helper",
		"args_json": `{"compact":true}`,
	})
	if isErr {
		t.Fatalf("primary router trace returned error: %s", text)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("unmarshal primary router response: %v", err)
	}
	if resp["status"] != "found" {
		t.Fatalf("primary router trace status=%v response=%s", resp["status"], text)
	}
}

func TestPrimaryExactDefaultsToEvidenceOnly(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/routes.ts", `export function previewEnvelopeDocuments() {
  return "application/pdf"
}
`)
	idx, err := mamari.BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	c := startClientWithOptions(t, idx, nil, ServeOptions{})
	text, isErr := callTool(t, c, "mamari", map[string]any{
		"action": "exact",
		"query":  "previewEnvelopeDocuments application/pdf",
	})
	if isErr {
		t.Fatalf("primary exact returned error: %s", text)
	}
	var resp mamari.InspectExactResponse
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "ok" || len(resp.Clusters) == 0 {
		t.Fatalf("expected exact evidence clusters, got %s", text)
	}
	for _, cluster := range resp.Clusters {
		if cluster.Source != "" {
			t.Fatalf("default exact action should omit full source, got %#v", cluster)
		}
	}
}

func TestPrimarySearchDefaultsToFocusedEvidence(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/duplicate.ts", `const before = true
export function preventDuplicateStart() {
  return before
}
`)
	idx, err := mamari.BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	c := startClientWithOptions(t, idx, nil, ServeOptions{})

	text, isErr := callTool(t, c, "mamari", map[string]any{
		"action": "search",
		"query":  "duplicate",
	})
	if isErr {
		t.Fatalf("primary search returned error: %s", text)
	}
	var focused mamari.SearchCodeResponse
	if err := json.Unmarshal([]byte(text), &focused); err != nil {
		t.Fatal(err)
	}
	if focused.Status != "ok" || focused.Mode != mamari.ModeEvidence || len(focused.Hits) == 0 {
		t.Fatalf("expected focused evidence hits, got %s", text)
	}
	for _, hit := range focused.Hits {
		// The lean wire shape omits endLine/focusLine when they carry no
		// information beyond startLine, so a single focused line serializes
		// with endLine absent (0 after unmarshal). A present endLine must still
		// equal startLine.
		if (hit.EndLine != 0 && hit.EndLine != hit.StartLine) || strings.Count(strings.TrimSuffix(hit.Text, "\n"), "\n") != 0 {
			t.Fatalf("default slim search should return one focused line, got %#v", hit)
		}
	}

	text, isErr = callTool(t, c, "mamari", map[string]any{
		"action":    "search",
		"query":     "duplicate",
		"args_json": `{"mode":"context"}`,
	})
	if isErr {
		t.Fatalf("context search returned error: %s", text)
	}
	var contextual mamari.SearchCodeResponse
	if err := json.Unmarshal([]byte(text), &contextual); err != nil {
		t.Fatal(err)
	}
	if contextual.Mode != mamari.ModeContext || len(contextual.Hits) == 0 ||
		contextual.Hits[0].StartLine == contextual.Hits[0].EndLine {
		t.Fatalf("explicit context mode should retain surrounding lines, got %s", text)
	}
}

func TestSearchCodeDispatchEnforcesSerializedBudget(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 16; i++ {
		rel := filepath.Join("src", "feature-with-a-deliberately-long-name-"+strconv.Itoa(i), "handler.ts")
		writeFile(t, root, rel, "export function serializedBudgetAnchor"+strconv.Itoa(i)+"() { return 1 }\n")
	}
	idx, err := mamari.BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	c := startClient(t, idx, nil)

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{name: "search_code", args: map[string]any{
			"query": "serialized budget anchor", "limit": 16, "budget": 300, "mode": "evidence",
		}},
		{name: "mamari", args: map[string]any{
			"action": "search", "query": "serialized budget anchor",
			"args_json": `{"limit":16,"budget":300}`,
		}},
	} {
		text, isErr := callTool(t, c, tc.name, tc.args)
		if isErr {
			t.Fatalf("%s returned error: %s", tc.name, text)
		}
		var resp mamari.SearchCodeResponse
		if err := json.Unmarshal([]byte(text), &resp); err != nil {
			t.Fatal(err)
		}
		if got := mamari.EstimateTokens(text); got > 300 {
			t.Fatalf("%s serialized response uses %d estimated tokens, want <= 300: %s", tc.name, got, text)
		}
		if len(resp.Hits) >= 16 || !resp.Truncated || len(resp.Warnings) == 0 {
			t.Fatalf("%s did not report serialized-budget shaping: %#v", tc.name, resp)
		}
	}
}

func TestPrimaryGraphDefaultsToLosslessCompactTable(t *testing.T) {
	idx := fixtureIndex(t)
	c := startClientWithOptions(t, idx, nil, ServeOptions{})
	query := `MATCH (a:function)-[:calls]->(b:function) RETURN a.name, b.name LIMIT 5`

	text, isErr := callTool(t, c, "mamari", map[string]any{
		"action": "graph",
		"query":  query,
	})
	if isErr {
		t.Fatalf("primary graph returned error: %s", text)
	}
	var compact map[string]any
	if err := json.Unmarshal([]byte(text), &compact); err != nil {
		t.Fatal(err)
	}
	if _, ok := compact["status"]; ok {
		t.Fatalf("successful compact graph response should imply status: %s", text)
	}
	columns, ok := compact["columns"].([]any)
	if !ok || len(columns) != 2 {
		t.Fatalf("expected two compact columns, got %s", text)
	}
	rows, ok := compact["rows"].([]any)
	if !ok || len(rows) == 0 {
		t.Fatalf("expected compact rows, got %s", text)
	}
	if _, ok := rows[0].([]any); !ok {
		t.Fatalf("compact rows should be value arrays, got %T in %s", rows[0], text)
	}

	text, isErr = callTool(t, c, "mamari", map[string]any{
		"action":    "graph",
		"query":     query,
		"args_json": `{"compact":false}`,
	})
	if isErr {
		t.Fatalf("full graph returned error: %s", text)
	}
	var full map[string]any
	if err := json.Unmarshal([]byte(text), &full); err != nil {
		t.Fatal(err)
	}
	if full["status"] != "ok" {
		t.Fatalf("full graph response lost status: %s", text)
	}
	fullRows, ok := full["rows"].([]any)
	if !ok || len(fullRows) == 0 {
		t.Fatalf("expected full rows, got %s", text)
	}
	if _, ok := fullRows[0].(map[string]any); !ok {
		t.Fatalf("compact=false should keep map rows, got %T in %s", fullRows[0], text)
	}
}

// TestToolGatingAdaptiveHidesInapplicableTools verifies that a plain repo with no TTL
// content, no event-bus edges, no --link, and no --watch should not pay the
// tools/list token cost of TTL/event/cross-repo/admin tools it can never use.
func TestToolGatingAdaptiveHidesInapplicableTools(t *testing.T) {
	idx := fixtureIndex(t) // plain TS, no TTL/events
	c := startClientWithOptions(t, idx, nil, ServeOptions{Toolset: "adaptive"})
	names := listToolNames(t, c)

	assertAbsent(t, names, ttlTools)
	assertAbsent(t, names, eventTools)
	assertAbsent(t, names, linkTools)
	assertAbsent(t, names, watchTool)
	assertAbsent(t, names, adminTools)
	assertPresent(t, names, []string{"mamari"})
	assertPresent(t, names, coreSamples)
}

// TestToolGatingFullToolsetExposesEverything checks the FullToolset escape
// hatch: even with none of the underlying conditions met, every gated tool
// must still be reachable when explicitly requested.
func TestToolGatingFullToolsetExposesEverything(t *testing.T) {
	idx := fixtureIndex(t)
	c := startClientWithOptions(t, idx, nil, ServeOptions{FullToolset: true})
	names := listToolNames(t, c)

	assertPresent(t, names, ttlTools)
	assertPresent(t, names, eventTools)
	assertPresent(t, names, linkTools)
	assertPresent(t, names, watchTool)
	assertPresent(t, names, adminTools)
	assertPresent(t, names, coreSamples)
}

// TestToolGatingTTLContentEnablesTTLTools checks that a repo with real
// RDF/TTL content gets the TTL tools without needing FullToolset, while
// still not getting the unrelated event/link/watch/admin tools.
func TestToolGatingTTLContentEnablesTTLTools(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "shapes/main.ttl", `@prefix dcterms: <http://purl.org/dc/terms/> .
@prefix dcatap: <http://data.europa.eu/r5r/> .
@prefix sh: <http://www.w3.org/ns/shacl#> .

dcatap:Dataset_Shape
  a sh:NodeShape ;
  sh:path dcterms:identifier .
`)
	idx, err := mamari.BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	c := startClientWithOptions(t, idx, nil, ServeOptions{Toolset: "adaptive"})
	names := listToolNames(t, c)

	assertPresent(t, names, []string{"mamari"})
	assertPresent(t, names, ttlTools)
	assertAbsent(t, names, eventTools)
	assertAbsent(t, names, linkTools)
	assertAbsent(t, names, watchTool)
	assertAbsent(t, names, adminTools)
}

// TestToolGatingEventEdgesEnableEventTools checks that a repo with real
// event-bus emit/listen call sites gets trace_event/list_events without
// needing FullToolset.
func TestToolGatingEventEdgesEnableEventTools(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/bus.js", `function setup() {
  bus.on('user.created', handleUserCreated)
}
function handleUserCreated() {
  return 1
}
function fire() {
  bus.emit('user.created', {})
}
`)
	idx, err := mamari.BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	c := startClientWithOptions(t, idx, nil, ServeOptions{Toolset: "adaptive"})
	names := listToolNames(t, c)

	assertPresent(t, names, []string{"mamari"})
	assertPresent(t, names, eventTools)
	assertAbsent(t, names, ttlTools)
	assertAbsent(t, names, linkTools)
	assertAbsent(t, names, watchTool)
	assertAbsent(t, names, adminTools)
}

// TestToolGatingLinkedReposEnablesCrossRepoTools checks that passing a
// non-empty linked-repo list gets list_linked_repos/cross_repo_architecture
// without needing FullToolset.
func TestToolGatingLinkedReposEnablesCrossRepoTools(t *testing.T) {
	idx := fixtureIndex(t)
	linked := []mamari.LinkedRepo{{Name: "other", Index: fixtureIndex(t)}}
	c := startClientWithOptions(t, idx, linked, ServeOptions{Toolset: "adaptive"})
	names := listToolNames(t, c)

	assertPresent(t, names, []string{"mamari"})
	assertPresent(t, names, linkTools)
	assertAbsent(t, names, ttlTools)
	assertAbsent(t, names, eventTools)
	assertAbsent(t, names, watchTool)
	assertAbsent(t, names, adminTools)
}

// TestToolGatingWatchEnablesChangedSince checks that ServeOptions.Watch gets
// changed_since without needing FullToolset.
func TestToolGatingWatchEnablesChangedSince(t *testing.T) {
	idx := fixtureIndex(t)
	c := startClientWithOptions(t, idx, nil, ServeOptions{Toolset: "adaptive", Watch: true})
	names := listToolNames(t, c)

	assertPresent(t, names, []string{"mamari"})
	assertPresent(t, names, watchTool)
	assertAbsent(t, names, ttlTools)
	assertAbsent(t, names, eventTools)
	assertAbsent(t, names, linkTools)
	assertAbsent(t, names, adminTools)
}

func TestAdaptiveWatchRefreshesCapabilityTools(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/util.ts", "export const value = 1\n")
	idx, err := mamari.BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	opts := ServeOptions{Toolset: "adaptive", Watch: true}
	s := newMCPServer(idx, nil, opts)
	c, err := client.NewInProcessClient(s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "test-client", Version: "1.0.0"}
	initResult, err := c.Initialize(context.Background(), initReq)
	if err != nil {
		t.Fatal(err)
	}
	if initResult.Capabilities.Tools == nil || !initResult.Capabilities.Tools.ListChanged {
		t.Fatalf("adaptive watch server must advertise tools.listChanged, got %#v", initResult.Capabilities.Tools)
	}
	assertAbsent(t, listToolNames(t, c), ttlTools)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rebaked := make(chan struct{}, 2)
	go func() {
		_ = mamari.Watch(ctx, idx, mamari.WatchOptions{
			Debounce: 10 * time.Millisecond,
			OnRebake: func(updated, removed []string) {
				refreshAdaptiveTools(s, idx, nil, opts)
				rebaked <- struct{}{}
			},
		})
	}()
	time.Sleep(50 * time.Millisecond)

	writeFile(t, root, "shapes/main.ttl", `@prefix sh: <http://www.w3.org/ns/shacl#> .
@prefix ex: <https://example.test/> .
ex:ThingShape a sh:NodeShape .
`)
	waitForRebake(t, rebaked)
	assertPresent(t, listToolNames(t, c), ttlTools)

	if err := os.Remove(filepath.Join(root, "shapes/main.ttl")); err != nil {
		t.Fatal(err)
	}
	waitForRebake(t, rebaked)
	assertAbsent(t, listToolNames(t, c), ttlTools)
}

func waitForRebake(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for watch rebake")
	}
}

func TestAutomaticServerMemoryLimitScalesWithAllIndexes(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first.index")
	second := filepath.Join(root, "second.index")
	if err := os.WriteFile(first, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := automaticServerMemoryLimit([]string{first, second}), defaultServerMemoryFloor+3072; got != want {
		t.Fatalf("automatic memory limit=%d, want %d", got, want)
	}
	if got := automaticServerMemoryLimit([]string{filepath.Join(root, "missing")}); got != defaultServerMemoryFloor {
		t.Fatalf("missing indexes should leave the floor unchanged, got %d", got)
	}
	v2 := filepath.Join(root, "v2.index")
	v2Data := append([]byte("mamari-index-v2\n"), make([]byte, 4096)...)
	if err := os.WriteFile(v2, v2Data, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := automaticServerMemoryLimit([]string{v2}), defaultServerMemoryFloor+int64(len(v2Data))*v2IndexMemoryMultiplier; got != want {
		t.Fatalf("v2 automatic memory limit=%d, want %d", got, want)
	}
}

func TestApplyServerMemoryLimitHonorsModesAndRestores(t *testing.T) {
	const (
		baseline = int64(512 << 20)
		explicit = int64(256 << 20)
	)
	original := debug.SetMemoryLimit(baseline)
	t.Cleanup(func() {
		debug.SetMemoryLimit(original)
	})

	restore := applyServerMemoryLimit("unused.index", nil, explicit)
	if previous := debug.SetMemoryLimit(explicit); previous != explicit {
		t.Fatalf("explicit memory limit=%d, want %d", previous, explicit)
	}
	restore()
	if previous := debug.SetMemoryLimit(baseline); previous != baseline {
		t.Fatalf("restored memory limit=%d, want %d", previous, baseline)
	}

	t.Setenv("GOMEMLIMIT", "128MiB")
	restore = applyServerMemoryLimit("unused.index", nil, -1)
	if previous := debug.SetMemoryLimit(baseline); previous != baseline {
		t.Fatalf("automatic mode replaced existing GOMEMLIMIT: got %d want %d", previous, baseline)
	}
	restore()

	restore = applyServerMemoryLimit("unused.index", nil, 0)
	if previous := debug.SetMemoryLimit(baseline); previous != baseline {
		t.Fatalf("disabled automatic limit changed runtime limit: got %d want %d", previous, baseline)
	}
	restore()

	restore = applyServerMemoryLimit("unused.index", nil, explicit)
	if previous := debug.SetMemoryLimit(explicit); previous != explicit {
		t.Fatalf("explicit limit should override GOMEMLIMIT: got %d want %d", previous, explicit)
	}
	restore()
}

// TestSearchCodeSymbolDetailDispatch is the MCP-transport-level counterpart
// to mamari_test.go's TestSearchCodeSymbolDetailDefaultsCompact: confirms
// the real "symbol_detail" request param (not just the Go API option) is
// wired through both the named search_code tool and the slim router's
// "search" action, on both sides of the default.
func TestSearchCodeSymbolDetailDispatch(t *testing.T) {
	idx := fixtureIndex(t)
	c := startClient(t, idx, nil)

	text, isErr := callTool(t, c, "search_code", map[string]any{"query": "helper"})
	if isErr {
		t.Fatalf("search_code returned error: %s", text)
	}
	var compact mamari.SearchCodeResponse
	if err := json.Unmarshal([]byte(text), &compact); err != nil {
		t.Fatalf("unmarshal search_code response: %v", err)
	}
	if len(compact.Hits) == 0 || len(compact.Hits[0].Symbols) == 0 {
		t.Fatalf("expected at least one hit with a symbol, got %#v", compact)
	}
	if compact.Hits[0].Symbols[0].ID != "" || compact.Hits[0].Symbols[0].Signature != "" {
		t.Fatalf("default search_code dispatch should drop id/signature, got %#v", compact.Hits[0].Symbols[0])
	}

	text, isErr = callTool(t, c, "search_code", map[string]any{"query": "helper", "symbol_detail": true})
	if isErr {
		t.Fatalf("search_code with symbol_detail=true returned error: %s", text)
	}
	var detailed mamari.SearchCodeResponse
	if err := json.Unmarshal([]byte(text), &detailed); err != nil {
		t.Fatalf("unmarshal search_code response: %v", err)
	}
	if len(detailed.Hits) == 0 || len(detailed.Hits[0].Symbols) == 0 {
		t.Fatalf("expected at least one hit with a symbol, got %#v", detailed)
	}
	if detailed.Hits[0].Symbols[0].ID == "" || detailed.Hits[0].Symbols[0].Signature == "" {
		t.Fatalf("symbol_detail=true dispatch should keep id/signature, got %#v", detailed.Hits[0].Symbols[0])
	}

	// Slim router's "search" action defaults to the same compact symbol
	// projection.
	text, isErr = callTool(t, c, "mamari", map[string]any{"action": "search", "query": "helper"})
	if isErr {
		t.Fatalf("mamari action=search returned error: %s", text)
	}
	var slim mamari.SearchCodeResponse
	if err := json.Unmarshal([]byte(text), &slim); err != nil {
		t.Fatalf("unmarshal mamari action=search response: %v", err)
	}
	if len(slim.Hits) == 0 || len(slim.Hits[0].Symbols) == 0 {
		t.Fatalf("expected at least one hit with a symbol, got %#v", slim)
	}
	if slim.Hits[0].Symbols[0].ID != "" || slim.Hits[0].Symbols[0].Signature != "" {
		t.Fatalf("slim router's default search action should drop id/signature, got %#v", slim.Hits[0].Symbols[0])
	}
}

// The review action documents the base ref as the `query` param, but an agent
// passing args_json {"base": ...} previously got a silent review against HEAD
// — a wrong-scope result that looks like success. Both spellings must reach
// ReviewOptions.Base, with query winning when both are set.
func TestSlimRouterReviewAcceptsBaseFromArgsJSON(t *testing.T) {
	idx := fixtureIndex(t)
	slim := startClientWithOptions(t, idx, nil, ServeOptions{})

	text, _ := callTool(t, slim, "mamari", map[string]any{
		"action":    "review",
		"args_json": `{"base":"args-base"}`,
	})
	if !strings.Contains(text, `"base":"args-base"`) {
		t.Fatalf("args_json base was dropped: %s", text)
	}

	text, _ = callTool(t, slim, "mamari", map[string]any{
		"action":    "review",
		"query":     "query-base",
		"args_json": `{"base":"args-base"}`,
	})
	if !strings.Contains(text, `"base":"query-base"`) {
		t.Fatalf("query param must win over args_json base: %s", text)
	}
}

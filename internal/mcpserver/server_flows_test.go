package mcpserver

import (
	"encoding/json"
	"testing"

	"github.com/waelhoury/mamari/internal/mamari"
)

// The slim router must dispatch the new review and dead_code actions and return
// well-formed, non-error results (review degrades to not_git outside a repo).
func TestPrimaryRouterDispatchesReviewAndDeadCode(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.js", "export function handler() { return 1 }\n")
	writeFile(t, root, "b.js", "export function handler() { return 2 }\n")
	writeFile(t, root, "c.js", "function invoke(x) { return handler(x) }\n")
	idx, err := mamari.BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	c := startClientWithOptions(t, idx, nil, ServeOptions{})

	// dead_code: exported+uncertain default on in the router; expect a well-
	// formed response that holds handler back rather than asserting it dead.
	text, isErr := callTool(t, c, "mamari", map[string]any{
		"action":    "dead_code",
		"args_json": `{"include_exported": true}`,
	})
	if isErr {
		t.Fatalf("dead_code dispatch errored: %s", text)
	}
	var dc mamari.DeadCodeResponse
	if err := json.Unmarshal([]byte(text), &dc); err != nil {
		t.Fatalf("dead_code response not JSON: %v\n%s", err, text)
	}
	if dc.Status != "ok" {
		t.Fatalf("dead_code status = %s", dc.Status)
	}
	for _, s := range dc.Symbols {
		if s.Name == "handler" {
			t.Fatalf("router dead_code asserted an ambiguously-reached symbol dead: %+v", dc.Symbols)
		}
	}

	// review: temp dir is not a git repo, so it must degrade gracefully, not error.
	text, isErr = callTool(t, c, "mamari", map[string]any{"action": "review"})
	if isErr {
		t.Fatalf("review dispatch errored: %s", text)
	}
	var rv mamari.ReviewResponse
	if err := json.Unmarshal([]byte(text), &rv); err != nil {
		t.Fatalf("review response not JSON: %v\n%s", err, text)
	}
	if rv.Status == "ok" {
		t.Fatalf("review must not report ok without a git diff, got %+v", rv)
	}
	if rv.Status != "not_git" && rv.Status != "no_changes" {
		t.Fatalf("expected graceful review status, got %s", rv.Status)
	}
}

package mamari

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// TestQueryGraphLiteSingleHopWhereOrderLimit covers the core scoped-query
// grammar: MATCH single hop, WHERE numeric comparison, RETURN multiple
// fields, ORDER BY DESC, and LIMIT.
func TestQueryGraphLiteSingleHopWhereOrderLimit(t *testing.T) {
	root := t.TempDir()
	write(t, root, "hot.js", `function deep() {
  for (let i = 0; i < 10; i++) {
    for (let j = 0; j < 10; j++) {
      noop()
    }
  }
}

function noop() {}

function shallow() {
  return 1
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	resp := QueryGraphLite(idx, `MATCH (f:function) WHERE f.transitiveloopdepth >= 1 RETURN f.name, f.transitiveloopdepth ORDER BY f.transitiveloopdepth DESC LIMIT 10`, QueryGraphLiteOptions{})
	if resp.Status != "ok" {
		t.Fatalf("expected ok status, got %#v", resp)
	}
	if len(resp.Rows) != 1 {
		t.Fatalf("expected 1 row (only deep() loops), got %#v", resp.Rows)
	}
	if resp.Rows[0]["f.name"] != "deep" {
		t.Fatalf("expected deep() to be the match, got %#v", resp.Rows)
	}
	if resp.Rows[0]["f.transitiveloopdepth"] != 2 {
		t.Fatalf("expected deep()'s transitiveLoopDepth=2, got %#v", resp.Rows[0])
	}
}

// TestQueryGraphLiteTwoHopCalls covers the two-hop
// MATCH (a)-[:CALLS]->(b) shape.
func TestQueryGraphLiteTwoHopCalls(t *testing.T) {
	root := t.TempDir()
	write(t, root, "calls.js", `function outer() {
  inner()
}

function inner() {
  return 1
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	resp := QueryGraphLite(idx, `MATCH (a:function)-[:calls]->(b:function) WHERE a.name = 'outer' RETURN a.name, b.name`, QueryGraphLiteOptions{})
	if resp.Status != "ok" {
		t.Fatalf("expected ok status, got %#v", resp)
	}
	if len(resp.Rows) != 1 {
		t.Fatalf("expected 1 row (outer calls inner), got %#v", resp.Rows)
	}
	if resp.Rows[0]["a.name"] != "outer" || resp.Rows[0]["b.name"] != "inner" {
		t.Fatalf("expected outer->inner, got %#v", resp.Rows[0])
	}
}

// TestQueryGraphLiteInvalidQueryReportsStatusInvalid covers the error path:
// a malformed query must come back as status="invalid" with a warning, not
// a panic or a silent empty result.
func TestQueryGraphLiteInvalidQueryReportsStatusInvalid(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.js", "function f() {}\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	resp := QueryGraphLite(idx, `SELECT * FROM symbols`, QueryGraphLiteOptions{})
	if resp.Status != "invalid" {
		t.Fatalf("expected invalid status for a non-MATCH query, got %#v", resp)
	}
	if len(resp.Warnings) == 0 {
		t.Fatalf("expected a warning explaining the parse error")
	}
	if resp.Query != `SELECT * FROM symbols` {
		t.Fatalf("expected the invalid query to be echoed back so the caller can see exactly what failed to parse, got %#v", resp.Query)
	}
}

// TestQueryGraphLiteSuccessOmitsQueryEcho guards a measured token-cost
// fix: a successful response no longer echoes the query string back at
// all (the caller already has it, and a Cypher-lite statement can be long
// relative to the typically small, row-shaped result it produces). The invalid-query
// path (covered above) is unaffected and deliberately keeps the echo.
func TestQueryGraphLiteSuccessOmitsQueryEcho(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.js", "function f() {}\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	resp := QueryGraphLite(idx, `MATCH (f:function) RETURN f.name`, QueryGraphLiteOptions{})
	if resp.Status != "ok" {
		t.Fatalf("expected ok status, got %#v", resp)
	}
	if resp.Query != "" {
		t.Fatalf("expected a successful response to omit the query echo, got %#v", resp.Query)
	}

	encoded, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["query"]; ok {
		t.Fatalf("expected the \"query\" key to disappear entirely (omitempty) on success, got %v", decoded)
	}
}

func TestCompactQueryGraphLiteResponseIsLosslessAndColumnOrdered(t *testing.T) {
	root := t.TempDir()
	write(t, root, "flow.js", `function finish() {}
function middle() { finish() }
function start() { middle() }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	query := `MATCH (a:function)-[:calls]->(b:function) RETURN a.name, b.name ORDER BY a.name LIMIT 5`
	resp := QueryGraphLite(idx, query, QueryGraphLiteOptions{})
	if resp.Status != "ok" || len(resp.Rows) == 0 {
		t.Fatalf("expected graph rows, got %#v", resp)
	}
	table := CompactQueryGraphLiteResponse(resp)
	if got, want := table.Columns, []string{"a.name", "b.name"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("columns=%v, want %v", got, want)
	}
	if table.Status != "" {
		t.Fatalf("successful compact response should imply status, got %q", table.Status)
	}
	if table.Total != resp.Total || table.Truncated != resp.Truncated || len(table.Rows) != len(resp.Rows) {
		t.Fatalf("compact metadata changed: table=%#v response=%#v", table, resp)
	}
	for i, values := range table.Rows {
		for j, column := range table.Columns {
			if !reflect.DeepEqual(values[j], resp.Rows[i][column]) {
				t.Fatalf("row %d column %q changed: got %#v want %#v", i, column, values[j], resp.Rows[i][column])
			}
		}
	}

	fullJSON, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	compactJSON, err := json.Marshal(table)
	if err != nil {
		t.Fatal(err)
	}
	if len(compactJSON) >= len(fullJSON) {
		t.Fatalf("compact table did not reduce payload: compact=%d full=%d", len(compactJSON), len(fullJSON))
	}
}

func TestQueryGraphLiteWithoutOrderByIsDeterministic(t *testing.T) {
	root := t.TempDir()
	write(t, root, "flow.js", `function finish() {}
function middle() { finish() }
function start() { middle(); finish() }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	query := `MATCH (a:function)-[:calls]->(b:function) RETURN a.name, b.name LIMIT 5`
	var baseline []byte
	for i := 0; i < 50; i++ {
		resp := QueryGraphLite(idx, query, QueryGraphLiteOptions{})
		encoded, err := json.Marshal(resp.Rows)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			baseline = encoded
			continue
		}
		if !bytes.Equal(encoded, baseline) {
			t.Fatalf("unordered query changed between calls:\nfirst %s\nlater %s", baseline, encoded)
		}
	}
}

func TestQueryGraphLiteRefreshesDeterministicOrderAfterSymbolInsertion(t *testing.T) {
	root := t.TempDir()
	write(t, root, "flow.js", "function existing() {}\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	query := `MATCH (f:function) RETURN f.name`
	first := QueryGraphLite(idx, query, QueryGraphLiteOptions{})
	if first.Total != 1 {
		t.Fatalf("initial total=%d, want 1: %#v", first.Total, first.Rows)
	}

	added := idx.AddCGPSymbol(CGPSymbol{
		ID:        "function:added.js:1:added",
		Name:      "added",
		Kind:      "function",
		Language:  "javascript",
		File:      "added.js",
		StartLine: 1,
		EndLine:   1,
	})
	if added.ID == "" {
		t.Fatal("failed to add test symbol")
	}

	second := QueryGraphLite(idx, query, QueryGraphLiteOptions{})
	if second.Total != 2 {
		t.Fatalf("post-insertion total=%d, want 2: %#v", second.Total, second.Rows)
	}
	names := []any{second.Rows[0]["f.name"], second.Rows[1]["f.name"]}
	if want := []any{"added", "existing"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("post-insertion order=%v, want %v", names, want)
	}
}

// TestQueryGraphLiteRespectsHardRowCeiling covers truncation: a broad query
// over more symbols than the hard ceiling must report Truncated=true and
// cap Rows, while Total still reflects the full match count.
func TestQueryGraphLiteRespectsHardRowCeiling(t *testing.T) {
	root := t.TempDir()
	var b []byte
	for i := 0; i < 5005; i++ {
		b = append(b, []byte("function f"+itoaForTest(i)+"() { return 1 }\n")...)
	}
	write(t, root, "many.js", string(b))
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	resp := QueryGraphLite(idx, `MATCH (f:function) RETURN f.name`, QueryGraphLiteOptions{})
	if !resp.Truncated {
		t.Fatalf("expected Truncated=true for >5000 matches, got %#v total=%d rows=%d", resp.Status, resp.Total, len(resp.Rows))
	}
	if resp.Total < 5005 {
		t.Fatalf("expected Total to reflect the full match count (>=5005), got %d", resp.Total)
	}
	if len(resp.Rows) != cypherLiteHardRowCeiling {
		t.Fatalf("expected exactly %d rows after truncation, got %d", cypherLiteHardRowCeiling, len(resp.Rows))
	}
}

// TestQueryGraphLiteCountStar covers a global aggregate with no GROUP BY
// fields: COUNT(*) over all matching rows.
func TestQueryGraphLiteCountStar(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.js", "function f1() {}\nfunction f2() {}\nfunction f3() {}\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := QueryGraphLite(idx, `MATCH (f:function) RETURN COUNT(*)`, QueryGraphLiteOptions{})
	if resp.Status != "ok" || len(resp.Rows) != 1 {
		t.Fatalf("expected exactly 1 aggregate row, got %#v", resp)
	}
	if resp.Rows[0]["COUNT(*)"] != 3 {
		t.Fatalf("expected COUNT(*)=3, got %#v", resp.Rows[0])
	}
}

// TestQueryGraphLiteCountStarOverZeroMatches covers the SQL/Cypher
// convention that an aggregate-only RETURN over zero matching rows still
// returns one row with COUNT=0, not zero rows.
func TestQueryGraphLiteCountStarOverZeroMatches(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.js", "function f1() {}\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := QueryGraphLite(idx, `MATCH (f:class) RETURN COUNT(*)`, QueryGraphLiteOptions{})
	if resp.Status != "ok" || len(resp.Rows) != 1 {
		t.Fatalf("expected exactly 1 aggregate row even with zero matches, got %#v", resp)
	}
	if resp.Rows[0]["COUNT(*)"] != 0 {
		t.Fatalf("expected COUNT(*)=0, got %#v", resp.Rows[0])
	}
}

// TestQueryGraphLiteGroupByWithCountAndSum covers GROUP BY (the implicit
// grouping by non-aggregate RETURN fields) combined with both COUNT and
// SUM, ordered by the aggregate.
func TestQueryGraphLiteGroupByWithCountAndSum(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.js", `function aFn1() {
  if (x) {}
}
function aFn2() {
  if (x) {}
  if (y) {}
}
`)
	write(t, root, "b.js", `function bFn1() {
  return 1
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := QueryGraphLite(idx, `MATCH (f:function) RETURN f.file, COUNT(*), SUM(f.complexity) ORDER BY COUNT(*) DESC`, QueryGraphLiteOptions{})
	if resp.Status != "ok" {
		t.Fatalf("expected ok status, got %#v", resp)
	}
	if len(resp.Rows) != 2 {
		t.Fatalf("expected 2 groups (one per file), got %#v", resp.Rows)
	}
	if resp.Rows[0]["f.file"] != "a.js" || resp.Rows[0]["COUNT(*)"] != 2 {
		t.Fatalf("expected a.js (2 functions) first when ordered by COUNT(*) DESC, got %#v", resp.Rows[0])
	}
	if resp.Rows[1]["f.file"] != "b.js" || resp.Rows[1]["COUNT(*)"] != 1 {
		t.Fatalf("expected b.js (1 function) second, got %#v", resp.Rows[1])
	}
	sumA, ok := resp.Rows[0]["SUM(f.complexity)"].(int)
	if !ok || sumA < 3 {
		t.Fatalf("expected SUM(f.complexity) for a.js to be the sum across both functions (>=3), got %#v", resp.Rows[0])
	}
}

func TestQueryGraphLiteMinMaxAvgAndCollect(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.js", `function alpha() { return 1 }
function beta() { if (x) return 2; return 1 }
function gamma() { if (x) return 3; if (y) return 2; return 1 }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := QueryGraphLite(idx, `MATCH (f:function) RETURN MIN(f.complexity), MAX(f.complexity), AVG(f.complexity), COLLECT(f.name)`, QueryGraphLiteOptions{})
	if resp.Status != "ok" || len(resp.Rows) != 1 {
		t.Fatalf("expected one aggregate row, got %#v", resp)
	}
	row := resp.Rows[0]
	if row["MIN(f.complexity)"] != 1 || row["MAX(f.complexity)"] != 3 || row["AVG(f.complexity)"] != float64(2) {
		t.Fatalf("unexpected numeric aggregates: %#v", row)
	}
	names, ok := row["COLLECT(f.name)"].([]any)
	if !ok || len(names) != 3 {
		t.Fatalf("expected three collected names, got %#v", row["COLLECT(f.name)"])
	}
}

func TestQueryGraphLiteEmptyExtendedAggregates(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.js", "function alpha() {}\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := QueryGraphLite(idx, `MATCH (f:class) RETURN MIN(f.name), MAX(f.name), AVG(f.complexity), COLLECT(f.name)`, QueryGraphLiteOptions{})
	if resp.Status != "ok" || len(resp.Rows) != 1 {
		t.Fatalf("expected one aggregate row over zero matches, got %#v", resp)
	}
	row := resp.Rows[0]
	if row["MIN(f.name)"] != nil || row["MAX(f.name)"] != nil || row["AVG(f.complexity)"] != nil {
		t.Fatalf("expected nil MIN/MAX/AVG over zero matches, got %#v", row)
	}
	if values, ok := row["COLLECT(f.name)"].([]any); !ok || len(values) != 0 {
		t.Fatalf("expected empty COLLECT over zero matches, got %#v", row["COLLECT(f.name)"])
	}
}

func itoaForTest(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// TestQueryGraphLiteThreeNodeChain covers the new multi-hop chain
// extension: `(a)-[:T]->(b)-[:T]->(c)`, three+ nodes in one pattern.
// Before this fix the
// parser only ever accepted a single optional hop (two nodes max).
func TestQueryGraphLiteThreeNodeChain(t *testing.T) {
	root := t.TempDir()
	write(t, root, "chain.js", `function a() { b() }
function b() { c() }
function c() { return 1 }
function unrelated() {}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := QueryGraphLite(idx, `MATCH (x:function)-[:calls]->(y:function)-[:calls]->(z:function) RETURN x.name, y.name, z.name`, QueryGraphLiteOptions{})
	if resp.Status != "ok" {
		t.Fatalf("expected ok status, got %#v", resp)
	}
	if len(resp.Rows) != 1 {
		t.Fatalf("expected exactly one a->b->c chain, got %#v", resp.Rows)
	}
	row := resp.Rows[0]
	if row["x.name"] != "a" || row["y.name"] != "b" || row["z.name"] != "c" {
		t.Fatalf("expected a->b->c, got %#v", row)
	}
}

// TestQueryGraphLiteVariableLengthRangeFindsTransitiveCallers covers the
// other named gap: `-[:CALLS*1..3]->`, a bounded variable-length hop.
// a->b->c->d->e is a 4-hop chain; `*1..3` from `a` must reach b/c/d but
// not e (4 hops away, outside the range).
func TestQueryGraphLiteVariableLengthRangeFindsTransitiveCallers(t *testing.T) {
	root := t.TempDir()
	write(t, root, "chain.js", `function a() { b() }
function b() { c() }
function c() { d() }
function d() { e() }
function e() { return 1 }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := QueryGraphLite(idx, `MATCH (x:function)-[:calls*1..3]->(y:function) WHERE x.name = 'a' RETURN y.name ORDER BY y.name ASC`, QueryGraphLiteOptions{})
	if resp.Status != "ok" {
		t.Fatalf("expected ok status, got %#v", resp)
	}
	var names []string
	for _, row := range resp.Rows {
		names = append(names, row["y.name"].(string))
	}
	if len(names) != 3 || names[0] != "b" || names[1] != "c" || names[2] != "d" {
		t.Fatalf("expected exactly [b, c, d] within 1..3 hops of a, got %v", names)
	}
}

// TestQueryGraphLiteUnboundedVariableLengthIsCapped covers bare `*`
// (unbounded) — must reach every transitively-callable function (here,
// within the 5-hop chain, all of it) without requiring an explicit upper
// bound, while still being internally capped (cypherLiteMaxHops) rather
// than truly unbounded.
func TestQueryGraphLiteUnboundedVariableLengthIsCapped(t *testing.T) {
	root := t.TempDir()
	write(t, root, "chain.js", `function a() { b() }
function b() { c() }
function c() { d() }
function d() { e() }
function e() { return 1 }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := QueryGraphLite(idx, `MATCH (x:function)-[:calls*]->(y:function) WHERE x.name = 'a' RETURN y.name ORDER BY y.name ASC`, QueryGraphLiteOptions{})
	if resp.Status != "ok" {
		t.Fatalf("expected ok status, got %#v", resp)
	}
	var names []string
	for _, row := range resp.Rows {
		names = append(names, row["y.name"].(string))
	}
	if len(names) != 4 || names[0] != "b" || names[1] != "c" || names[2] != "d" || names[3] != "e" {
		t.Fatalf("expected all 4 transitively-reachable functions [b,c,d,e], got %v", names)
	}
}

// TestQueryGraphLiteExactHopCount covers `*N` (exact hop count, no
// range): only nodes reachable at precisely N hops, not fewer.
func TestQueryGraphLiteExactHopCount(t *testing.T) {
	root := t.TempDir()
	write(t, root, "chain.js", `function a() { b() }
function b() { c() }
function c() { return 1 }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := QueryGraphLite(idx, `MATCH (x:function)-[:calls*2]->(y:function) WHERE x.name = 'a' RETURN y.name`, QueryGraphLiteOptions{})
	if resp.Status != "ok" {
		t.Fatalf("expected ok status, got %#v", resp)
	}
	if len(resp.Rows) != 1 || resp.Rows[0]["y.name"] != "c" {
		t.Fatalf("expected exactly one row (c, at exactly 2 hops from a), got %#v", resp.Rows)
	}
}

// TestQueryGraphLiteHopCountClampedToHardCap covers the safety bound: a
// query explicitly requesting more hops than cypherLiteMaxHops must not
// error, crash, or hang — it's silently clamped, the same policy
// QueryGraphLite already applies to an over-large max_rows.
func TestQueryGraphLiteHopCountClampedToHardCap(t *testing.T) {
	root := t.TempDir()
	write(t, root, "chain.js", `function a() { b() }
function b() { return 1 }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := QueryGraphLite(idx, `MATCH (x:function)-[:calls*1..999]->(y:function) WHERE x.name = 'a' RETURN y.name`, QueryGraphLiteOptions{})
	if resp.Status != "ok" {
		t.Fatalf("expected ok status (clamped, not rejected), got %#v", resp)
	}
	if len(resp.Rows) != 1 || resp.Rows[0]["y.name"] != "b" {
		t.Fatalf("expected exactly one row (b), got %#v", resp.Rows)
	}
}

// TestQueryGraphLiteDuplicateEdgesProduceOneRowEach is a regression guard
// for a real bug caught while building the variable-length-hop feature:
// the first implementation of the fixed (non-variable-length) single-hop
// case routed through the same reachability/BFS path as true
// variable-length hops, whose visited-node-set semantics silently
// deduplicate by node pair — collapsing two distinct call sites between
// the same two functions into one row, where real Cypher (and this
// grammar's own pre-existing behavior) returns one row per matching
// relationship/edge. Caught by TestQueryGraphDispatch in
// internal/mcpserver, which already covered this exact shape; this adds
// a dedicated unit-level test alongside it.
func TestQueryGraphLiteDuplicateEdgesProduceOneRowEach(t *testing.T) {
	root := t.TempDir()
	write(t, root, "dup.js", `function caller() {
  helper()
  helper()
}
function helper() { return 1 }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := QueryGraphLite(idx, `MATCH (a:function)-[:calls]->(b:function) WHERE a.name = 'caller' RETURN a.name, b.name`, QueryGraphLiteOptions{})
	if resp.Status != "ok" {
		t.Fatalf("expected ok status, got %#v", resp)
	}
	if len(resp.Rows) != 2 {
		t.Fatalf("expected two rows (one per call site), got %#v", resp.Rows)
	}
}

// TestQueryGraphLiteRangeWithNoLowerBound covers `*..N` (omitted lower
// bound, defaulting to 1) — a genuinely distinct parser code path from
// `*N..M`: the tokenizer's number scan greedily swallows a digit-prefixed
// ".." into one token (e.g. "1..3"), but a leading ".." with no preceding
// digit tokenizes as two separate "." punct tokens instead, handled by a
// different branch in parseHopQuantifier.
func TestQueryGraphLiteRangeWithNoLowerBound(t *testing.T) {
	root := t.TempDir()
	write(t, root, "chain.js", `function a() { b() }
function b() { c() }
function c() { return 1 }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := QueryGraphLite(idx, `MATCH (x:function)-[:calls*..2]->(y:function) WHERE x.name = 'a' RETURN y.name ORDER BY y.name ASC`, QueryGraphLiteOptions{})
	if resp.Status != "ok" {
		t.Fatalf("expected ok status, got %#v", resp)
	}
	var names []string
	for _, row := range resp.Rows {
		names = append(names, row["y.name"].(string))
	}
	if len(names) != 2 || names[0] != "b" || names[1] != "c" {
		t.Fatalf("expected [b, c] within *..2 hops of a, got %v", names)
	}
}

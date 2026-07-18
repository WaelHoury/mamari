package mamari

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// QueryGraphLite executes a deliberately restricted, Cypher-like query
// language against the CGP symbol/edge graph:
//
//	MATCH (a:Label)[-[:EDGE_TYPE[*[minHops][..[maxHops]]]]->(b:Label)]*
//	[WHERE <var>.<field> <op> <value> [AND <var>.<field> <op> <value>]*]
//	RETURN <var>.<field>|COUNT(*)|<aggregate>(<var>.<field>) [, ...]
//	[ORDER BY <var>.<field>|<aggregate>(...) [ASC|DESC]]
//	[LIMIT n]
//
// This is intentionally not full Cypher: simple property comparisons only,
// with no OPTIONAL MATCH/UNION/WITH/subqueries. It does support chained multi-hop patterns
// (`(a)-[:CALLS]->(b)-[:CALLS]->(c)`, arbitrarily many segments, not just
// one edge) and variable-length hops (`(a)-[:CALLS*1..3]->(b)`, `*` for
// unbounded — internally capped at cypherLiteMaxHops for safety, the same
// kind of bound propagateTransitiveLoopDepth already uses for its own
// transitive-reachability computation). It supports COUNT, SUM, AVG, MIN,
// MAX, and COLLECT with implicit grouping. It exists to answer ad-hoc
// multi-hop/property/aggregate questions (e.g. hot-path hotspots from
// hotpath.go's loop-depth signals, or "how many functions per file exceed
// complexity N") that the fixed-shape tools don't cover, without growing the
// MCP tool surface into a query-language reimplementation project.
func QueryGraphLite(idx *Index, query string, opts QueryGraphLiteOptions) QueryGraphLiteResponse {
	resp := QueryGraphLiteResponse{Status: "ok"}
	stmt, err := parseCypherLite(query)
	if err != nil {
		// Query is only echoed back on the error path, where seeing the
		// exact string that failed to parse is genuinely useful (e.g. the
		// response is logged or shown without the original request handy)
		// — see QueryGraphLiteResponse's doc comment for why a successful
		// response drops it instead: the caller already has it, and unlike
		// most mamari tools' query/symbol-name argument, a Cypher-lite
		// statement can be long relative to a typically small, row-shaped
		// result, found while auditing real responses for the
		// "echo nothing the caller doesn't already need" pass this fix
		// belongs to.
		resp.Status = "invalid"
		resp.Query = query
		resp.Warnings = append(resp.Warnings, err.Error())
		return resp
	}

	limit := opts.MaxRows
	if limit <= 0 || limit > cypherLiteHardRowCeiling {
		limit = cypherLiteHardRowCeiling
	}

	snap := idx.orderedSymbolGraphSnapshot()
	rows := evalCypherLite(snap, stmt)
	resp.Columns = make([]string, len(stmt.ret))
	for i, field := range stmt.ret {
		resp.Columns[i] = cypherLiteFieldKey(field)
	}

	var outputRows []map[string]any
	if cypherLiteReturnHasAggregate(stmt.ret) {
		// RETURN/ORDER BY/LIMIT all operate on the post-aggregation rows —
		// "ORDER BY COUNT(*) DESC LIMIT 5" ranks groups, not raw matches.
		outputRows = aggregateCypherLiteRows(rows, stmt.ret)
		if stmt.orderBy != nil {
			sortAggregatedRows(outputRows, *stmt.orderBy)
		}
	} else {
		if stmt.orderBy != nil {
			sortCypherLiteRows(rows, *stmt.orderBy)
		}
		outputRows = make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			outputRows = append(outputRows, projectCypherLiteRow(r, stmt.ret))
		}
	}

	resp.Total = len(outputRows)
	if len(outputRows) > limit {
		outputRows = outputRows[:limit]
		resp.Truncated = true
	}
	if stmt.limit != nil && len(outputRows) > *stmt.limit {
		outputRows = outputRows[:*stmt.limit]
	}
	resp.Rows = outputRows
	return resp
}

// CompactQueryGraphLiteResponse converts map-per-row graph results into a
// columnar table without changing values, ordering, totals, or truncation.
// Repeating three RETURN keys across five rows can cost as much as the values
// themselves; the slim MCP route therefore uses this projection by default.
func CompactQueryGraphLiteResponse(resp QueryGraphLiteResponse) QueryGraphLiteTableResponse {
	table := QueryGraphLiteTableResponse{
		Query:     resp.Query,
		Columns:   append([]string(nil), resp.Columns...),
		Rows:      make([][]any, len(resp.Rows)),
		Total:     resp.Total,
		Truncated: resp.Truncated,
		Warnings:  append([]string(nil), resp.Warnings...),
	}
	if resp.Status != "ok" {
		table.Status = resp.Status
	}
	for i, row := range resp.Rows {
		values := make([]any, len(table.Columns))
		for j, column := range table.Columns {
			values[j] = row[column]
		}
		table.Rows[i] = values
	}
	return table
}

// cypherLiteHardRowCeiling mirrors query_graph's documented hard cap — bounds
// response size on a broad/unfiltered query regardless of caller-supplied
// MaxRows.
const cypherLiteHardRowCeiling = 5000

// --- AST -------------------------------------------------------------------

type cypherLiteNodePattern struct {
	Var   string
	Label string // CGP Kind to match, case-insensitive; "" = any kind
}

// cypherLiteHop is one `-[:TYPE[*min..max]]->(node)` segment of a MATCH
// chain. MinHops/MaxHops are both 1 for an ordinary fixed single-edge hop
// (the only shape this grammar supported before variable-length patterns);
// MaxHops > MinHops (or the cypherLiteMaxHops sentinel for unbounded `*`)
// means "reachable via 1 or more EdgeType edges, between MinHops and
// MaxHops steps inclusive" — ordinary Cypher variable-length-path
// semantics, evaluated as bounded BFS rather than true shortest-path
// search (a node reachable by more than one path length within range is
// still only bound once per row, matching Cypher's per-path semantics
// closely enough for this grammar's purpose: "is B reachable from A within
// N hops", not "enumerate every distinct path").
type cypherLiteHop struct {
	EdgeType string // case-insensitive
	Node     cypherLiteNodePattern
	MinHops  int
	MaxHops  int
}

// cypherLiteStatement is a MATCH chain: Nodes[0] is the pattern's starting
// node; Hops[i] describes the edge+target node connecting Nodes[i] to
// Nodes[i+1]. len(Nodes) == len(Hops)+1 always (Nodes has at least one
// element; Hops may be empty for a single bare node pattern).
type cypherLiteStatement struct {
	nodes   []cypherLiteNodePattern
	hops    []cypherLiteHop
	where   []cypherLitePredicate
	ret     []cypherLiteField
	orderBy *cypherLiteOrderBy
	limit   *int
}

// cypherLiteMaxHops hard-caps any variable-length hop's MaxHops, including
// unbounded `*` — bounds the worst-case BFS work per row the same way
// hotpath.go's maxTransitiveLoopDepthHops bounds its own transitive
// reachability computation. An explicit `*N..M` requesting more than this
// is silently clamped, not rejected — matching how QueryGraphLite already
// clamps MaxRows against cypherLiteHardRowCeiling rather than erroring.
const cypherLiteMaxHops = 10

type cypherLitePredicate struct {
	Var   string
	Field string
	Op    string // = != > >= < <= CONTAINS IN
	Value string
}

type cypherLiteField struct {
	Var   string
	Field string
	// Aggregate is "" (a plain property reference) or a supported aggregate
	// function name. When
	// set and Var == "*" (COUNT(*) only), Field is unused.
	Aggregate string
}

type cypherLiteOrderBy struct {
	Field cypherLiteField
	Desc  bool
}

type cypherLiteRow struct {
	vars map[string]CGPSymbol
}

// --- Tokenizer ---------------------------------------------------------

type cypherLiteToken struct {
	kind string // ident, string, number, punct, eof
	text string
}

func tokenizeCypherLite(query string) ([]cypherLiteToken, error) {
	var tokens []cypherLiteToken
	runes := []rune(query)
	i := 0
	for i < len(runes) {
		c := runes[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '\'' || c == '"':
			quote := c
			j := i + 1
			var sb strings.Builder
			for j < len(runes) && runes[j] != quote {
				sb.WriteRune(runes[j])
				j++
			}
			if j >= len(runes) {
				return nil, fmt.Errorf("unterminated string literal")
			}
			tokens = append(tokens, cypherLiteToken{kind: "string", text: sb.String()})
			i = j + 1
		case c == '(' || c == ')' || c == '[' || c == ']' || c == ',' || c == '.' || c == ':' || c == '*':
			tokens = append(tokens, cypherLiteToken{kind: "punct", text: string(c)})
			i++
		case c == '-':
			if i+1 < len(runes) && runes[i+1] == '>' {
				tokens = append(tokens, cypherLiteToken{kind: "punct", text: "->"})
				i += 2
			} else {
				tokens = append(tokens, cypherLiteToken{kind: "punct", text: "-"})
				i++
			}
		case c == '=' || c == '!' || c == '>' || c == '<':
			j := i + 1
			if j < len(runes) && runes[j] == '=' {
				tokens = append(tokens, cypherLiteToken{kind: "punct", text: string(c) + "="})
				i += 2
			} else {
				tokens = append(tokens, cypherLiteToken{kind: "punct", text: string(c)})
				i++
			}
		case isCypherLiteIdentStart(c):
			j := i
			for j < len(runes) && isCypherLiteIdentPart(runes[j]) {
				j++
			}
			tokens = append(tokens, cypherLiteToken{kind: "ident", text: string(runes[i:j])})
			i = j
		case c >= '0' && c <= '9':
			j := i
			for j < len(runes) && (runes[j] >= '0' && runes[j] <= '9' || runes[j] == '.' || runes[j] == '-') {
				j++
			}
			tokens = append(tokens, cypherLiteToken{kind: "number", text: string(runes[i:j])})
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q at position %d", c, i)
		}
	}
	tokens = append(tokens, cypherLiteToken{kind: "eof"})
	return tokens, nil
}

func isCypherLiteIdentStart(c rune) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isCypherLiteIdentPart(c rune) bool {
	return isCypherLiteIdentStart(c) || (c >= '0' && c <= '9')
}

// --- Parser --------------------------------------------------------------

type cypherLiteParser struct {
	tokens []cypherLiteToken
	pos    int
}

func parseCypherLite(query string) (*cypherLiteStatement, error) {
	tokens, err := tokenizeCypherLite(query)
	if err != nil {
		return nil, err
	}
	p := &cypherLiteParser{tokens: tokens}
	stmt, err := p.parseStatement()
	if err != nil {
		return nil, err
	}
	if !p.atEOF() {
		return nil, fmt.Errorf("unexpected trailing input near %q", p.peek().text)
	}
	return stmt, nil
}

func (p *cypherLiteParser) peek() cypherLiteToken { return p.tokens[p.pos] }
func (p *cypherLiteParser) atEOF() bool           { return p.peek().kind == "eof" }

func (p *cypherLiteParser) next() cypherLiteToken {
	t := p.tokens[p.pos]
	if p.pos < len(p.tokens)-1 {
		p.pos++
	}
	return t
}

func (p *cypherLiteParser) expectKeyword(kw string) error {
	t := p.next()
	if t.kind != "ident" || !strings.EqualFold(t.text, kw) {
		return fmt.Errorf("expected %s, got %q", kw, t.text)
	}
	return nil
}

func (p *cypherLiteParser) expectPunct(s string) error {
	t := p.next()
	if t.kind != "punct" || t.text != s {
		return fmt.Errorf("expected %q, got %q", s, t.text)
	}
	return nil
}

func (p *cypherLiteParser) peekIsKeyword(kw string) bool {
	t := p.peek()
	return t.kind == "ident" && strings.EqualFold(t.text, kw)
}

func (p *cypherLiteParser) parseStatement() (*cypherLiteStatement, error) {
	if err := p.expectKeyword("MATCH"); err != nil {
		return nil, err
	}
	first, err := p.parseNodePattern()
	if err != nil {
		return nil, err
	}
	stmt := &cypherLiteStatement{nodes: []cypherLiteNodePattern{first}}

	for p.peek().kind == "punct" && p.peek().text == "-" {
		p.next()
		if err := p.expectPunct("["); err != nil {
			return nil, err
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		t := p.next()
		if t.kind != "ident" {
			return nil, fmt.Errorf("expected edge type, got %q", t.text)
		}
		minHops, maxHops, err := p.parseHopQuantifier()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct("]"); err != nil {
			return nil, err
		}
		if err := p.expectPunct("->"); err != nil {
			return nil, err
		}
		node, err := p.parseNodePattern()
		if err != nil {
			return nil, err
		}
		stmt.hops = append(stmt.hops, cypherLiteHop{EdgeType: t.text, Node: node, MinHops: minHops, MaxHops: maxHops})
		stmt.nodes = append(stmt.nodes, node)
	}

	if p.peekIsKeyword("WHERE") {
		p.next()
		preds, err := p.parseWhere()
		if err != nil {
			return nil, err
		}
		stmt.where = preds
	}

	if err := p.expectKeyword("RETURN"); err != nil {
		return nil, err
	}
	ret, err := p.parseReturn()
	if err != nil {
		return nil, err
	}
	stmt.ret = ret

	if p.peekIsKeyword("ORDER") {
		p.next()
		if err := p.expectKeyword("BY"); err != nil {
			return nil, err
		}
		field, err := p.parseReturnField()
		if err != nil {
			return nil, err
		}
		desc := false
		if p.peekIsKeyword("DESC") {
			p.next()
			desc = true
		} else if p.peekIsKeyword("ASC") {
			p.next()
		}
		stmt.orderBy = &cypherLiteOrderBy{Field: field, Desc: desc}
	}

	if p.peekIsKeyword("LIMIT") {
		p.next()
		t := p.next()
		if t.kind != "number" {
			return nil, fmt.Errorf("expected a number after LIMIT, got %q", t.text)
		}
		n, err := strconv.Atoi(t.text)
		if err != nil {
			return nil, fmt.Errorf("invalid LIMIT %q", t.text)
		}
		stmt.limit = &n
	}

	return stmt, nil
}

func (p *cypherLiteParser) parseNodePattern() (cypherLiteNodePattern, error) {
	if err := p.expectPunct("("); err != nil {
		return cypherLiteNodePattern{}, err
	}
	t := p.next()
	if t.kind != "ident" {
		return cypherLiteNodePattern{}, fmt.Errorf("expected a variable name, got %q", t.text)
	}
	pat := cypherLiteNodePattern{Var: t.text}
	if p.peek().kind == "punct" && p.peek().text == ":" {
		p.next()
		lt := p.next()
		if lt.kind != "ident" {
			return cypherLiteNodePattern{}, fmt.Errorf("expected a label after ':', got %q", lt.text)
		}
		pat.Label = lt.text
	}
	if err := p.expectPunct(")"); err != nil {
		return cypherLiteNodePattern{}, err
	}
	return pat, nil
}

// parseHopQuantifier parses an optional `*[min][..[max]]` variable-length
// quantifier immediately following an edge type inside `[:TYPE...]`.
// Returns (1, 1) — an ordinary fixed single hop — when no `*` is present
// at all, preserving this grammar's original single-hop-only behavior
// exactly for every query that doesn't use this new syntax.
//
// The tokenizer (see tokenizeCypherLite) scans a number greedily through
// any following '.', so a digit-prefixed range like "1..3" or "2.." (no
// upper bound) arrives as a single "number" token whose text must be
// split on ".." manually; a range with no lower bound ("..3") has no
// leading digit, so it instead arrives as two separate "." punct tokens
// followed by a number — both shapes are handled below.
func (p *cypherLiteParser) parseHopQuantifier() (minHops, maxHops int, err error) {
	if !(p.peek().kind == "punct" && p.peek().text == "*") {
		return 1, 1, nil
	}
	p.next() // consume '*'

	if p.peek().kind == "number" {
		text := p.next().text
		if dot := strings.Index(text, ".."); dot >= 0 {
			left, right := text[:dot], text[dot+2:]
			min, err := strconv.Atoi(left)
			if err != nil {
				return 0, 0, fmt.Errorf("invalid hop range %q", text)
			}
			if right == "" {
				return clampCypherLiteHops(min), cypherLiteMaxHops, nil
			}
			max, err := strconv.Atoi(right)
			if err != nil {
				return 0, 0, fmt.Errorf("invalid hop range %q", text)
			}
			return clampCypherLiteHops(min), clampCypherLiteHops(max), nil
		}
		n, err := strconv.Atoi(text)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid hop count %q", text)
		}
		return clampCypherLiteHops(n), clampCypherLiteHops(n), nil
	}

	if p.peek().kind == "punct" && p.peek().text == "." {
		p.next()
		if !(p.peek().kind == "punct" && p.peek().text == ".") {
			return 0, 0, fmt.Errorf("expected '..' in hop quantifier")
		}
		p.next()
		if p.peek().kind == "number" {
			text := p.next().text
			max, err := strconv.Atoi(text)
			if err != nil {
				return 0, 0, fmt.Errorf("invalid hop count %q", text)
			}
			return 1, clampCypherLiteHops(max), nil
		}
		return 1, cypherLiteMaxHops, nil
	}

	// Bare '*' with nothing else following (immediately "]") — unbounded,
	// capped at cypherLiteMaxHops.
	return 1, cypherLiteMaxHops, nil
}

func clampCypherLiteHops(n int) int {
	if n < 1 {
		return 1
	}
	if n > cypherLiteMaxHops {
		return cypherLiteMaxHops
	}
	return n
}

func (p *cypherLiteParser) parseField() (cypherLiteField, error) {
	t := p.next()
	if t.kind != "ident" {
		return cypherLiteField{}, fmt.Errorf("expected a variable, got %q", t.text)
	}
	if err := p.expectPunct("."); err != nil {
		return cypherLiteField{}, err
	}
	f := p.next()
	if f.kind != "ident" {
		return cypherLiteField{}, fmt.Errorf("expected a field name, got %q", f.text)
	}
	return cypherLiteField{Var: t.text, Field: f.text}, nil
}

// parseReturnField parses one RETURN/ORDER BY item: either a plain
// "var.field" (delegated to parseField) or a supported aggregate call.
func (p *cypherLiteParser) parseReturnField() (cypherLiteField, error) {
	nextIsOpenParen := p.pos+1 < len(p.tokens) && p.tokens[p.pos+1].kind == "punct" && p.tokens[p.pos+1].text == "("
	isAggregate := p.peekIsKeyword("COUNT") || p.peekIsKeyword("SUM") || p.peekIsKeyword("AVG") ||
		p.peekIsKeyword("MIN") || p.peekIsKeyword("MAX") || p.peekIsKeyword("COLLECT")
	if isAggregate && nextIsOpenParen {
		fn := strings.ToUpper(p.next().text)
		p.next() // '('
		if fn == "COUNT" && p.peek().kind == "punct" && p.peek().text == "*" {
			p.next()
			if err := p.expectPunct(")"); err != nil {
				return cypherLiteField{}, err
			}
			return cypherLiteField{Var: "*", Aggregate: fn}, nil
		}
		inner, err := p.parseField()
		if err != nil {
			return cypherLiteField{}, err
		}
		if err := p.expectPunct(")"); err != nil {
			return cypherLiteField{}, err
		}
		inner.Aggregate = fn
		return inner, nil
	}
	return p.parseField()
}

var cypherLiteOps = []string{">=", "<=", "!=", "=", ">", "<"}

func (p *cypherLiteParser) parseWhere() ([]cypherLitePredicate, error) {
	var preds []cypherLitePredicate
	for {
		field, err := p.parseField()
		if err != nil {
			return nil, err
		}
		var op string
		if p.peekIsKeyword("CONTAINS") {
			p.next()
			op = "CONTAINS"
		} else if p.peekIsKeyword("IN") {
			p.next()
			op = "IN"
		} else {
			t := p.next()
			matched := false
			for _, candidate := range cypherLiteOps {
				if t.kind == "punct" && t.text == candidate {
					op = candidate
					matched = true
					break
				}
			}
			if !matched {
				return nil, fmt.Errorf("expected a comparison operator, got %q", t.text)
			}
		}
		valTok := p.next()
		if valTok.kind != "string" && valTok.kind != "number" && valTok.kind != "ident" {
			return nil, fmt.Errorf("expected a value, got %q", valTok.text)
		}
		preds = append(preds, cypherLitePredicate{Var: field.Var, Field: field.Field, Op: op, Value: valTok.text})
		if p.peekIsKeyword("AND") {
			p.next()
			continue
		}
		break
	}
	return preds, nil
}

func (p *cypherLiteParser) parseReturn() ([]cypherLiteField, error) {
	var fields []cypherLiteField
	for {
		f, err := p.parseReturnField()
		if err != nil {
			return nil, err
		}
		fields = append(fields, f)
		if p.peek().kind == "punct" && p.peek().text == "," {
			p.next()
			continue
		}
		break
	}
	return fields, nil
}

// --- Execution -----------------------------------------------------------

func cypherLiteSymbolMatchesLabel(sym CGPSymbol, label string) bool {
	return label == "" || strings.EqualFold(sym.Kind, label)
}

// cypherLiteRowExpansionCeiling bounds intermediate row counts *during*
// chain expansion (before WHERE/LIMIT are applied), distinct from
// QueryGraphLite's post-execution cypherLiteHardRowCeiling — a query like
// `MATCH (a)-[:CALLS*]->(b)-[:CALLS*]->(c)` against a large, densely
// connected graph could otherwise multiply out to an enormous intermediate
// row count before the final WHERE/LIMIT ever gets a chance to cut it
// down. Generous (4x the final ceiling) since later WHERE/ORDER BY/LIMIT
// stages still need real data to work with, but still bounded.
const cypherLiteRowExpansionCeiling = cypherLiteHardRowCeiling * 4

// cypherLiteAdjacency maps a from-symbol-ID to every to-symbol-ID reachable
// via exactly one edge of one specific (already-filtered) edge type.
type cypherLiteAdjacency map[string][]string

func buildCypherLiteAdjacency(snap symbolGraphSnapshot, edgeType string) cypherLiteAdjacency {
	adj := cypherLiteAdjacency{}
	snap.forEachEdge(func(_ int, edge CGPEdge) bool {
		if !strings.EqualFold(edge.Type, edgeType) {
			return true
		}
		adj[edge.From] = append(adj[edge.From], edge.To)
		return true
	})
	return adj
}

// evalCypherLite matches stmt's full node/hop chain against snap, starting
// from every symbol matching the first node pattern's label and advancing
// across each hop in order — a fixed hop (MinHops==MaxHops==1, every plain
// `-[:TYPE]->` pattern, including every pre-existing single/two-node
// query) is one adjacency lookup per row; a variable-length hop
// (`-[:TYPE*min..max]->`) is a bounded BFS per row via cypherLiteReachable.
// WHERE is applied once at the end, after every chain variable is bound —
// equivalent to applying it incrementally (it's a pure per-row predicate
// with no side effects, so evaluation order doesn't change the result),
// and simpler to reason about for an arbitrary-length chain.
func evalCypherLite(snap symbolGraphSnapshot, stmt *cypherLiteStatement) []cypherLiteRow {
	rows := make([]cypherLiteRow, 0)
	for _, id := range snap.OrderedSymbolIDs {
		sym := snap.Symbols[id]
		if !cypherLiteSymbolMatchesLabel(sym, stmt.nodes[0].Label) {
			continue
		}
		rows = append(rows, cypherLiteRow{vars: map[string]CGPSymbol{stmt.nodes[0].Var: sym}})
	}

	for i, hop := range stmt.hops {
		fromVar := stmt.nodes[i].Var
		adj := buildCypherLiteAdjacency(snap, hop.EdgeType)
		rows = expandCypherLiteHop(snap, rows, fromVar, hop, adj)
		if len(rows) > cypherLiteRowExpansionCeiling {
			rows = rows[:cypherLiteRowExpansionCeiling]
		}
	}

	out := make([]cypherLiteRow, 0, len(rows))
	for _, row := range rows {
		if cypherLiteRowMatchesWhere(row, stmt.where) {
			out = append(out, row)
		}
	}
	return out
}

// expandCypherLiteHop advances every row in rows across one hop, producing
// one new row per (row, reachable-node) pair — each new row carries every
// variable already bound in the input row plus the hop's own target
// variable, so a 3+ node chain accumulates bindings correctly across
// successive calls.
func expandCypherLiteHop(snap symbolGraphSnapshot, rows []cypherLiteRow, fromVar string, hop cypherLiteHop, adj cypherLiteAdjacency) []cypherLiteRow {
	var out []cypherLiteRow
	for _, row := range rows {
		from, ok := row.vars[fromVar]
		if !ok {
			continue
		}
		var targets []string
		if hop.MinHops == 1 && hop.MaxHops == 1 {
			// Ordinary fixed single hop — the only shape this grammar
			// supported before variable-length patterns, and the one real
			// Cypher always returns one row per matching *relationship*
			// for, not deduplicated by node pair: two distinct edges
			// between the same two symbols (e.g. two separate call sites)
			// must produce two separate rows. adj already preserves that
			// multiplicity (buildCypherLiteAdjacency appends every
			// matching edge, duplicates included); cypherLiteReachable's
			// visited-set semantics below are deliberately the opposite
			// (one row per *node*, regardless of how many edges/paths
			// reach it) — correct for true variable-length reachability,
			// but would silently collapse this common single-hop case and
			// was caught doing exactly that by the existing MCP dispatch
			// test before this special case was added.
			targets = adj[from.ID]
		} else {
			targets = cypherLiteReachable(from.ID, adj, hop.MinHops, hop.MaxHops)
		}
		for _, toID := range targets {
			to, ok := snap.Symbols[toID]
			if !ok || !cypherLiteSymbolMatchesLabel(to, hop.Node.Label) {
				continue
			}
			newVars := make(map[string]CGPSymbol, len(row.vars)+1)
			for k, v := range row.vars {
				newVars[k] = v
			}
			newVars[hop.Node.Var] = to
			out = append(out, cypherLiteRow{vars: newVars})
			if len(out) >= cypherLiteRowExpansionCeiling {
				return out
			}
		}
	}
	return out
}

// cypherLiteReachable returns every distinct symbol ID reachable from
// startID via adj, at a *shortest-path* depth between minHops and maxHops
// inclusive (BFS, depth-bounded) — the same shortest-path-style notion of
// "reachable within N hops" hotpath.go's propagateTransitiveLoopDepth
// already uses for its own bounded transitive computation, deliberately
// chosen over enumerating every distinct path (which a real Cypher engine
// can do but which is combinatorially expensive on a dense graph): a node
// is included if *a* path of length in [minHops,maxHops] reaches it, found
// via the first (shortest) discovery, not every possible path length to
// it. A node is visited (and can appear in the result) at most once.
func cypherLiteReachable(startID string, adj cypherLiteAdjacency, minHops, maxHops int) []string {
	type frontierEntry struct {
		id    string
		depth int
	}
	visited := map[string]bool{startID: true}
	var result []string
	frontier := []frontierEntry{{id: startID, depth: 0}}
	for len(frontier) > 0 && frontier[0].depth < maxHops {
		var next []frontierEntry
		for _, f := range frontier {
			for _, toID := range adj[f.id] {
				if visited[toID] {
					continue
				}
				visited[toID] = true
				depth := f.depth + 1
				if depth >= minHops && depth <= maxHops {
					result = append(result, toID)
				}
				if depth < maxHops {
					next = append(next, frontierEntry{id: toID, depth: depth})
				}
			}
		}
		frontier = next
	}
	return result
}

func cypherLiteRowMatchesWhere(row cypherLiteRow, preds []cypherLitePredicate) bool {
	for _, pred := range preds {
		sym, ok := row.vars[pred.Var]
		if !ok {
			return false
		}
		if !cypherLitePredicateMatches(sym, pred) {
			return false
		}
	}
	return true
}

// cypherLiteSymbolField resolves a property name against a CGPSymbol via an
// explicit accessor switch rather than reflection — keeps the supported
// field set small, fast, and auditable in one place.
func cypherLiteSymbolField(sym CGPSymbol, field string) (any, bool) {
	switch strings.ToLower(field) {
	case "id":
		return sym.ID, true
	case "name":
		return sym.Name, true
	case "kind", "label":
		return sym.Kind, true
	case "language":
		return sym.Language, true
	case "file":
		return sym.File, true
	case "startline":
		return sym.StartLine, true
	case "endline":
		return sym.EndLine, true
	case "signature":
		return sym.Signature, true
	case "confidence":
		return sym.Confidence, true
	case "exported":
		return sym.Exported, true
	case "complexity":
		return sym.Complexity, true
	case "loopdepth", "loop_depth":
		return sym.LoopDepth, true
	case "transitiveloopdepth", "transitive_loop_depth":
		return sym.TransitiveLoopDepth, true
	case "linearscaninloop", "linear_scan_in_loop":
		return sym.LinearScanInLoop, true
	case "allocinloop", "alloc_in_loop":
		return sym.AllocInLoop, true
	case "recursioninloop", "recursion_in_loop":
		return sym.RecursionInLoop, true
	default:
		return nil, false
	}
}

func cypherLitePredicateMatches(sym CGPSymbol, pred cypherLitePredicate) bool {
	value, ok := cypherLiteSymbolField(sym, pred.Field)
	if !ok {
		return false
	}
	switch v := value.(type) {
	case string:
		return cypherLiteCompareString(v, pred.Op, pred.Value)
	case int:
		return cypherLiteCompareNumber(float64(v), pred.Op, pred.Value)
	case bool:
		return cypherLiteCompareBool(v, pred.Op, pred.Value)
	default:
		return false
	}
}

func cypherLiteCompareString(actual, op, want string) bool {
	switch op {
	case "=":
		return strings.EqualFold(actual, want)
	case "!=":
		return !strings.EqualFold(actual, want)
	case "CONTAINS":
		return strings.Contains(strings.ToLower(actual), strings.ToLower(want))
	case "IN":
		for _, item := range strings.Split(want, "|") {
			if strings.EqualFold(actual, strings.TrimSpace(item)) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func cypherLiteCompareNumber(actual float64, op, wantStr string) bool {
	want, err := strconv.ParseFloat(wantStr, 64)
	if err != nil {
		return false
	}
	switch op {
	case "=":
		return actual == want
	case "!=":
		return actual != want
	case ">":
		return actual > want
	case ">=":
		return actual >= want
	case "<":
		return actual < want
	case "<=":
		return actual <= want
	default:
		return false
	}
}

func cypherLiteCompareBool(actual bool, op, wantStr string) bool {
	want := strings.EqualFold(wantStr, "true")
	switch op {
	case "=":
		return actual == want
	case "!=":
		return actual != want
	default:
		return false
	}
}

func sortCypherLiteRows(rows []cypherLiteRow, order cypherLiteOrderBy) {
	sort.SliceStable(rows, func(i, j int) bool {
		vi, oki := cypherLiteSymbolField(rows[i].vars[order.Field.Var], order.Field.Field)
		vj, okj := cypherLiteSymbolField(rows[j].vars[order.Field.Var], order.Field.Field)
		if !oki || !okj {
			return false
		}
		less := cypherLiteValueLess(vi, vj)
		if order.Desc {
			return cypherLiteValueLess(vj, vi)
		}
		return less
	})
}

func cypherLiteValueLess(a, b any) bool {
	switch av := a.(type) {
	case string:
		if bv, ok := b.(string); ok {
			return av < bv
		}
	case int:
		if bv, ok := b.(int); ok {
			return av < bv
		}
	case float64:
		if bv, ok := b.(float64); ok {
			return av < bv
		}
	case bool:
		if bv, ok := b.(bool); ok {
			return !av && bv
		}
	}
	return false
}

// cypherLiteFieldKey is the RETURN/output column name for f — "var.field"
// for a plain reference or "AGGREGATE(var.field)" for an aggregate, matching
// ordinary Cypher's default column naming.
func cypherLiteFieldKey(f cypherLiteField) string {
	if f.Aggregate == "" {
		return f.Var + "." + f.Field
	}
	if f.Var == "*" {
		return f.Aggregate + "(*)"
	}
	return f.Aggregate + "(" + f.Var + "." + f.Field + ")"
}

func cypherLiteReturnHasAggregate(fields []cypherLiteField) bool {
	for _, f := range fields {
		if f.Aggregate != "" {
			return true
		}
	}
	return false
}

func projectCypherLiteRow(row cypherLiteRow, fields []cypherLiteField) map[string]any {
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		key := cypherLiteFieldKey(f)
		sym, ok := row.vars[f.Var]
		if !ok {
			out[key] = nil
			continue
		}
		if v, ok := cypherLiteSymbolField(sym, f.Field); ok {
			out[key] = v
		} else {
			out[key] = nil
		}
	}
	return out
}

// aggregateCypherLiteRows groups rows by the non-aggregate RETURN fields
// (implicit GROUP BY, the same rule ordinary Cypher/SQL use: any
// non-aggregated return expression becomes a group key) and computes each
// aggregate field per group. A RETURN with only aggregate fields and no
// plain fields produces exactly one group (a single global aggregate),
// including a single zero-valued row when rows is empty — matching
// standard SQL/Cypher aggregate-only semantics.
func aggregateCypherLiteRows(rows []cypherLiteRow, ret []cypherLiteField) []map[string]any {
	var groupFields, aggFields []cypherLiteField
	for _, f := range ret {
		if f.Aggregate == "" {
			groupFields = append(groupFields, f)
		} else {
			aggFields = append(aggFields, f)
		}
	}

	type group struct {
		rows []cypherLiteRow
	}
	groups := map[string]*group{}
	var order []string
	for _, row := range rows {
		keyParts := make([]string, len(groupFields))
		for i, gf := range groupFields {
			v, _ := cypherLiteSymbolField(row.vars[gf.Var], gf.Field)
			keyParts[i] = fmt.Sprintf("%v", v)
		}
		key := strings.Join(keyParts, "\x1f")
		g := groups[key]
		if g == nil {
			g = &group{}
			groups[key] = g
			order = append(order, key)
		}
		g.rows = append(g.rows, row)
	}
	if len(rows) == 0 && len(groupFields) == 0 {
		groups[""] = &group{}
		order = append(order, "")
	}

	sort.Strings(order) // deterministic before any ORDER BY clause is applied
	out := make([]map[string]any, 0, len(order))
	for _, key := range order {
		g := groups[key]
		row := make(map[string]any, len(ret))
		for _, gf := range groupFields {
			var v any
			if len(g.rows) > 0 {
				v, _ = cypherLiteSymbolField(g.rows[0].vars[gf.Var], gf.Field)
			}
			row[cypherLiteFieldKey(gf)] = v
		}
		for _, af := range aggFields {
			row[cypherLiteFieldKey(af)] = computeCypherLiteAggregate(af, g.rows)
		}
		out = append(out, row)
	}
	return out
}

// computeCypherLiteAggregate evaluates one aggregate field over a group's
// rows. COUNT(*) and COUNT(var[.field]) are equivalent here: every matched
// row has a real, non-null symbol bound to every MATCH variable (there is
// no concept of a null/missing match in this grammar), so a per-field null
// check would never exclude anything — both simply count rows in the group.
func computeCypherLiteAggregate(f cypherLiteField, rows []cypherLiteRow) any {
	switch f.Aggregate {
	case "COUNT":
		return len(rows)
	case "SUM":
		total, count := cypherLiteNumericValues(f, rows)
		if count == 0 {
			return 0
		}
		return int(total)
	case "AVG":
		total, count := cypherLiteNumericValues(f, rows)
		if count == 0 {
			return nil
		}
		return total / float64(count)
	case "MIN", "MAX":
		var best any
		for _, row := range rows {
			sym, ok := row.vars[f.Var]
			if !ok {
				continue
			}
			v, ok := cypherLiteSymbolField(sym, f.Field)
			if !ok {
				continue
			}
			if best == nil {
				best = v
				continue
			}
			less := cypherLiteValueLess(v, best)
			if (f.Aggregate == "MIN" && less) || (f.Aggregate == "MAX" && cypherLiteValueLess(best, v)) {
				best = v
			}
		}
		return best
	case "COLLECT":
		values := make([]any, 0, len(rows))
		for _, row := range rows {
			sym, ok := row.vars[f.Var]
			if !ok {
				continue
			}
			if v, ok := cypherLiteSymbolField(sym, f.Field); ok {
				values = append(values, v)
			}
		}
		return values
	default:
		return nil
	}
}

func cypherLiteNumericValues(f cypherLiteField, rows []cypherLiteRow) (float64, int) {
	total := 0.0
	count := 0
	for _, row := range rows {
		sym, ok := row.vars[f.Var]
		if !ok {
			continue
		}
		v, ok := cypherLiteSymbolField(sym, f.Field)
		if !ok {
			continue
		}
		n, ok := v.(int)
		if !ok {
			continue
		}
		total += float64(n)
		count++
	}
	return total, count
}

func sortAggregatedRows(rows []map[string]any, order cypherLiteOrderBy) {
	key := cypherLiteFieldKey(order.Field)
	sort.SliceStable(rows, func(i, j int) bool {
		less := cypherLiteValueLess(rows[i][key], rows[j][key])
		if order.Desc {
			return cypherLiteValueLess(rows[j][key], rows[i][key])
		}
		return less
	})
}

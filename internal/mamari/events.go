package mamari

import (
	"regexp"
	"sort"
	"strings"
)

// Phase 4 — event-flow tracing. We model `bus.emit('FOO', payload)` and
// `bus.on('FOO', cb)` as edges in the same SymbolEdges graph used by call
// resolution, with a synthetic target ID `event:FOO`. The pattern is the
// same one Sourcegraph uses for module imports (`module:lodash`): the
// "to" side of the edge is a symbolic key the agent can fan out from. A
// `trace-event` query then resolves the key back to its emit + listen
// sites by walking the edges in O(edges) — no second index needed.
//
// Method allowlist is deliberately narrow. emit / on / once / off are the
// canonical Node EventEmitter / mitt / tiny-emitter / Vue $emit-style
// surface. Adding `dispatch` or `trigger` would also catch Redux/jQuery
// patterns but those carry far more false positives (e.g. `dispatch` in
// Redux is used with action *objects*, not string event names) so we
// keep them out and let consumers extend later.

// eventEdgeType taxonomy — identical naming to the call/import edge types
// so the existing edge listing UIs render them without code changes.
const (
	EdgeEmitsEvent    = "emits-event"
	EdgeListensEvent  = "listens-event"
	EdgeRemovesEvent  = "removes-event"
	eventTargetPrefix = "event:"
)

var eventCallRe = regexp.MustCompile(
	`\b([A-Za-z_$][A-Za-z0-9_$]*(?:\.[A-Za-z_$][A-Za-z0-9_$]*)*)\.(emit|on|once|off)\s*\(`,
)

// emitEventEdges scans a JS/TS source slice for `*.emit / .on / .once / .off`
// invocations and records them as SymbolEdges keyed on the resolved event
// name. The slice is masked first so a `// foo.emit(...)` comment or a
// string literal containing `bus.on(` does not create false edges.
//
// `from` attribution uses containingSymbolFast on the call line — the same
// algorithm call edges use — so an emit inside a nested function is
// attributed to that nested function (Phase 3 made this work for
// composable-local helpers).
//
// `confidence` reflects how certain we are about the event key:
//   - exact: string-literal first arg ("FOO") — directly comparable across sites.
//   - scoped: dotted identifier first arg (APP_EVENTS.FOO) — comparable
//     when both sides use the same constant path.
//   - unresolved: dynamic first arg (variable, expression) — recorded with
//     reason runtime_value so the agent knows to fall back to fetch_source.
func emitEventEdges(idx *Index, file, content string, fileStarts []int, baseOffset int) {
	if content == "" {
		return
	}
	masked := MaskStringsAndComments(content)
	for _, m := range eventCallRe.FindAllStringSubmatchIndex(masked, -1) {
		if len(m) < 6 {
			continue
		}
		receiver := content[m[2]:m[3]]
		method := content[m[4]:m[5]]
		callStart := m[0]
		argStart := m[1] // first byte after `(`
		key, keyConf, keyReason := extractEventKey(content, argStart)
		startLine, startCol := offsetToLineCol(fileStarts, baseOffset+callStart)
		from := idx.containingSymbolFast(file, startLine)
		if from.ID == "" {
			from = CGPSymbol{ID: fileSymbolID(file)}
		}
		edgeType := eventEdgeTypeFor(method)
		if edgeType == "" {
			continue
		}
		target := eventTargetPrefix + key
		if key == "" {
			target = eventTargetPrefix + "<dynamic>"
		}
		raw := receiver + "." + method + "(" + key + ")"
		idx.AddCGPEdgeWithReason(
			from.ID, target, edgeType, keyConf, keyReason,
			Location{
				File: file, StartLine: startLine, StartColumn: startCol,
				EndLine: startLine, EndColumn: startCol + len(raw),
				Kind: "event-call", Raw: raw,
			},
		)
	}
}

func eventEdgeTypeFor(method string) string {
	switch method {
	case "emit":
		return EdgeEmitsEvent
	case "on", "once":
		return EdgeListensEvent
	case "off":
		return EdgeRemovesEvent
	}
	return ""
}

// extractEventKey parses the first argument of an event call starting at
// argStart (the byte after `(`). Returns the resolved event key, the
// confidence to record on the edge, and an optional unresolved reason. We
// do not attempt full expression resolution — string literals and dotted
// identifier paths are the only forms we recognize.
func extractEventKey(content string, argStart int) (string, string, string) {
	for argStart < len(content) {
		c := content[argStart]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			argStart++
			continue
		}
		break
	}
	if argStart >= len(content) {
		return "", ConfUnresolved, ReasonRuntimeValue
	}
	ch := content[argStart]
	if ch == '\'' || ch == '"' || ch == '`' {
		end := strings.IndexByte(content[argStart+1:], ch)
		if end < 0 {
			return "", ConfUnresolved, ReasonRuntimeValue
		}
		key := content[argStart+1 : argStart+1+end]
		if key == "" {
			return "", ConfUnresolved, ReasonRuntimeValue
		}
		return key, ConfExact, ""
	}
	if !isEventIdentStartByte(ch) {
		return "", ConfUnresolved, ReasonRuntimeValue
	}
	i := argStart
	for i < len(content) {
		c := content[i]
		if isEventIdentStartByte(c) || (c >= '0' && c <= '9') || c == '.' {
			i++
			continue
		}
		break
	}
	key := strings.TrimRight(content[argStart:i], ".")
	if key == "" {
		return "", ConfUnresolved, ReasonRuntimeValue
	}
	return key, ConfScoped, ""
}

func isEventIdentStartByte(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' || c == '$'
}

// TraceEvent finds every emit / listen / remove site recorded for an event
// key. The query may be the bare key ("FOO_BAR"), the qualified target
// ("event:FOO_BAR"), or the dotted identifier path ("APP_EVENTS.FOO_BAR").
// All three forms resolve to the same edge set when the indexed code uses
// matching syntax on both sides; if one site uses a string literal and the
// other a constant, they will appear as two distinct event keys and the
// caller can issue both queries to bridge the gap.
func TraceEvent(idx *Index, query string) TraceEventResponse {
	resp := TraceEventResponse{
		Status:  "not_found",
		Query:   query,
		Emits:   []EventSite{},
		Listens: []EventSite{},
		Removes: []EventSite{},
	}
	q := strings.TrimSpace(query)
	if q == "" {
		resp.Status = "invalid"
		resp.Warnings = append(resp.Warnings, "empty event query")
		return resp
	}
	key := strings.TrimPrefix(q, eventTargetPrefix)
	target := eventTargetPrefix + key
	resp.Event = key
	snap := idx.snapshot()
	addSite := func(slice *[]EventSite, edge CGPEdge) {
		fromName := edge.From
		fromKind := ""
		if sym, ok := snap.Symbols[edge.From]; ok {
			fromName = sym.Name
			fromKind = sym.Kind
		}
		*slice = append(*slice, EventSite{
			SymbolID:   edge.From,
			SymbolName: fromName,
			SymbolKind: fromKind,
			Confidence: edge.Confidence,
			Reason:     edge.UnresolvedReason,
			Location:   edge.Evidence,
			Raw:        edge.Evidence.Raw,
		})
	}
	snap.forEachEdge(func(_ int, edge CGPEdge) bool {
		if edge.To != target {
			return true
		}
		switch edge.Type {
		case EdgeEmitsEvent:
			addSite(&resp.Emits, edge)
		case EdgeListensEvent:
			addSite(&resp.Listens, edge)
		case EdgeRemovesEvent:
			addSite(&resp.Removes, edge)
		}
		return true
	})
	sortEventSites(resp.Emits)
	sortEventSites(resp.Listens)
	sortEventSites(resp.Removes)
	if len(resp.Emits) == 0 && len(resp.Listens) == 0 && len(resp.Removes) == 0 {
		// Surface candidate keys when the exact form is missing — this is
		// the place where a string-literal emit and a constant-path listen
		// fail to pair, and seeing both candidates lets the agent retry.
		resp.Candidates = listEventCandidates(snap, key)
		if len(resp.Candidates) > 0 {
			resp.Status = "not_found"
			resp.Warnings = append(resp.Warnings, "exact key not found; see candidates with similar suffixes")
		}
		return resp
	}
	resp.Status = "found"
	return resp
}

func sortEventSites(sites []EventSite) {
	sort.SliceStable(sites, func(i, j int) bool {
		if sites[i].Location.File != sites[j].Location.File {
			return sites[i].Location.File < sites[j].Location.File
		}
		if sites[i].Location.StartLine != sites[j].Location.StartLine {
			return sites[i].Location.StartLine < sites[j].Location.StartLine
		}
		return sites[i].Location.StartColumn < sites[j].Location.StartColumn
	})
}

// listEventCandidates returns event keys whose suffix matches the requested
// key — useful when the query uses the bare local part ("FOO_BAR") and the
// indexed sites use the qualified path ("APP_EVENTS.FOO_BAR"), or vice
// versa. Sorted by frequency descending so the most active key surfaces first.
func listEventCandidates(snap indexSnapshot, key string) []EventCandidate {
	if key == "" {
		return nil
	}
	counts := map[string]int{}
	kinds := map[string]map[string]int{}
	snap.forEachEdge(func(_ int, edge CGPEdge) bool {
		if !strings.HasPrefix(edge.To, eventTargetPrefix) {
			return true
		}
		k := strings.TrimPrefix(edge.To, eventTargetPrefix)
		if k == key || k == "" {
			return true
		}
		if !strings.EqualFold(k, key) && !strings.HasSuffix(k, "."+key) && !strings.HasSuffix(key, "."+k) && !strings.Contains(strings.ToLower(k), strings.ToLower(key)) {
			return true
		}
		counts[k]++
		if kinds[k] == nil {
			kinds[k] = map[string]int{}
		}
		kinds[k][edge.Type]++
		return true
	})
	if len(counts) == 0 {
		return nil
	}
	out := make([]EventCandidate, 0, len(counts))
	for k, c := range counts {
		out = append(out, EventCandidate{
			Event:       k,
			TotalSites:  c,
			EmitCount:   kinds[k][EdgeEmitsEvent],
			ListenCount: kinds[k][EdgeListensEvent],
			RemoveCount: kinds[k][EdgeRemovesEvent],
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TotalSites != out[j].TotalSites {
			return out[i].TotalSites > out[j].TotalSites
		}
		return out[i].Event < out[j].Event
	})
	return out
}

// ListEvents returns every event key seen in the index together with site
// counts. Use it for discovery — agents can pick a key and call TraceEvent.
func ListEvents(idx *Index) ListEventsResponse {
	resp := ListEventsResponse{Status: "ok", Events: []EventCandidate{}}
	snap := idx.snapshot()
	counts := map[string]int{}
	kinds := map[string]map[string]int{}
	snap.forEachEdge(func(_ int, edge CGPEdge) bool {
		if !strings.HasPrefix(edge.To, eventTargetPrefix) {
			return true
		}
		k := strings.TrimPrefix(edge.To, eventTargetPrefix)
		counts[k]++
		if kinds[k] == nil {
			kinds[k] = map[string]int{}
		}
		kinds[k][edge.Type]++
		return true
	})
	for k, c := range counts {
		resp.Events = append(resp.Events, EventCandidate{
			Event:       k,
			TotalSites:  c,
			EmitCount:   kinds[k][EdgeEmitsEvent],
			ListenCount: kinds[k][EdgeListensEvent],
			RemoveCount: kinds[k][EdgeRemovesEvent],
		})
	}
	sort.SliceStable(resp.Events, func(i, j int) bool {
		if resp.Events[i].TotalSites != resp.Events[j].TotalSites {
			return resp.Events[i].TotalSites > resp.Events[j].TotalSites
		}
		return resp.Events[i].Event < resp.Events[j].Event
	})
	return resp
}

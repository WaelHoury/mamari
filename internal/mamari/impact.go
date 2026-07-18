package mamari

import (
	"encoding/json"
	"sort"
)

const (
	defaultImpactDepth = 2
	maxImpactSymbols   = 500 // hard cap so a hub function does not flood the response.
)

// Impact returns the reverse caller/dependent closure of a symbol, layer by
// layer. Executable languages follow calls; Terraform/OpenTofu additionally
// follows declarative depends-on edges.
//
// For each visited symbol, PathConfidence reports the *weakest* confidence
// along the chain from the target up to that symbol — a single heuristic
// hop downgrades the whole path. This makes it cheap to filter for "only
// strong impact": keep ConfExact and ConfScoped, drop the rest.
//
// Self edges and re-entry are deduped so cyclic call graphs don't loop.
//
// Impact is a thin, backward-compatible wrapper over ImpactWithOptions
// (Compact: false) — every existing caller keeps today's full-detail output
// unchanged.
func Impact(idx *Index, query string, depth int) ImpactResponse {
	return ImpactWithOptions(idx, query, ImpactOptions{Depth: depth})
}

// ImpactWithOptions is Impact with an opt-in Compact mode, mirroring
// TraceSymbolWithOptions' own Compact option and for the same reason: a
// symbol with a wide blast radius previously had every layer entry carry
// its full Signature/Docstring/ReturnTypes/hot-path/ID/Language fields —
// the same dominant per-entry cost that made trace_symbol expensive before
// its own compact fix, found by auditing every other CGPSymbolSummary-
// returning tool for the same gap once trace_symbol's was fixed. The
// queried target's own Symbol keeps full detail regardless of Compact —
// that is the actual answer to what the agent asked about, not incidental
// per-entry ballast, the same distinction TraceSymbolWithOptions draws for
// its own top-level Symbol.
func ImpactWithOptions(idx *Index, query string, opts ImpactOptions) ImpactResponse {
	depth := opts.Depth
	if depth <= 0 {
		depth = defaultImpactDepth
	}
	resp := ImpactResponse{
		Status: "not_found",
		Query:  query,
		Depth:  depth,
		Layers: []ImpactLayer{},
	}
	matches := findSymbols(idx, query)
	if len(matches) > 1 {
		resp.Status = "ambiguous"
		resp.Candidates = summarizeSymbols(matches)
		// A very common name (e.g. 136 methods named "write") would otherwise
		// dump every candidate — thousands of tokens the caller can't act on.
		// Cap to the same bound trace_symbol uses and flag the truncation.
		if len(resp.Candidates) > maxAmbiguousTraceCandidates {
			resp.Candidates = resp.Candidates[:maxAmbiguousTraceCandidates]
			resp.Truncated = true
		}
		return resp
	}
	if len(matches) == 0 {
		return resp
	}
	target := matches[0]
	summary := summarizeSymbol(target)
	if opts.Compact {
		// The queried target's own Docstring is the one field Compact
		// doesn't drop outright (it's the actual answer to what was asked
		// about), but an unbounded docstring is still real cost — trimmed
		// to its first sentence rather than removed, mirroring
		// TraceSymbolWithOptions' identical treatment of its own
		// top-level Symbol. See firstSentenceOrLimit's doc comment.
		summary.Docstring = firstSentenceOrLimit(summary.Docstring)
		// MarshalJSON renders Symbol via compactMainSymbolJSON instead of
		// CGPSymbolSummary's full shape when this is set — mirrors
		// TraceSymbolResponse's identical field/mechanism (cgp.go).
		resp.compactMainSymbol = true
	}
	resp.Symbol = &summary
	resp.Status = "found"

	summarizeImpact := summarizeSymbol
	if opts.Compact {
		summarizeImpact = summarizeSymbolCompact
	}
	if co := idx.CoChangedFiles(target.File, 0); len(co) > 0 {
		resp.CoChangedFiles = co
	}

	// Impact retains one immutable Symbols/SymbolEdges generation for the BFS;
	// watcher updates can publish concurrently without changing this request.
	snap := idx.symbolGraphSnapshot()
	callers := buildReverseImpactIndexGraph(snap)

	// BFS layer by layer. We track per-symbol best-path-confidence so a
	// later, weaker route does not overwrite an earlier strong one.
	type entry struct {
		conf   string
		reason string
	}
	visited := map[string]entry{target.ID: {conf: ConfExact}}
	frontier := []string{target.ID}

	for d := 1; d <= depth; d++ {
		var nextFrontier []string
		layer := ImpactLayer{Depth: d}
		for _, id := range frontier {
			parentEntry := visited[id]
			for _, edge := range callers[id] {
				if edge.From == target.ID {
					// Direct call from target back to self is noise.
					continue
				}
				caller, ok := snap.Symbols[edge.From]
				if !ok || caller.Kind == "file" {
					continue
				}
				edgeConf := edge.Confidence
				edgeReason := edge.UnresolvedReason
				combinedConf, combinedReason := weakerConfidence(parentEntry.conf, parentEntry.reason, edgeConf, edgeReason)
				prev, seen := visited[caller.ID]
				if seen && !confidenceImproves(prev.conf, combinedConf) {
					continue
				}
				visited[caller.ID] = entry{conf: combinedConf, reason: combinedReason}
				if !seen {
					layer.Symbols = append(layer.Symbols, ImpactSymbol{
						CGPSymbolSummary: summarizeImpact(caller),
						PathConfidence:   combinedConf,
						PathReason:       combinedReason,
					})
					nextFrontier = append(nextFrontier, caller.ID)
				}
			}
		}
		sort.Slice(layer.Symbols, func(i, j int) bool {
			if layer.Symbols[i].File != layer.Symbols[j].File {
				return layer.Symbols[i].File < layer.Symbols[j].File
			}
			return layer.Symbols[i].StartLine < layer.Symbols[j].StartLine
		})
		if len(layer.Symbols) > 0 {
			resp.Layers = append(resp.Layers, layer)
			resp.Total += len(layer.Symbols)
		}
		if resp.Total >= maxImpactSymbols {
			resp.Truncated = true
			break
		}
		if len(nextFrontier) == 0 {
			break
		}
		frontier = nextFrontier
	}
	return resp
}

func buildReverseImpactIndexGraph(snap symbolGraphSnapshot) map[string][]CGPEdge {
	out := map[string][]CGPEdge{}
	snap.forEachEdge(func(_ int, edge CGPEdge) bool {
		if edge.Type == "calls" || edge.Type == terraformDependencyEdge {
			out[edge.To] = append(out[edge.To], edge)
		}
		return true
	})
	return out
}

func buildReverseCallIndexSnapshot(snap indexSnapshot) map[string][]CGPEdge {
	out := map[string][]CGPEdge{}
	snap.forEachEdge(func(_ int, edge CGPEdge) bool {
		addReverseCallEdge(out, edge)
		return true
	})
	return out
}

func addReverseCallEdge(out map[string][]CGPEdge, edge CGPEdge) {
	if edge.Type == "calls" {
		out[edge.To] = append(out[edge.To], edge)
	}
}

// confidenceRank ranks confidences from strongest (1) to weakest (4). Used by
// path aggregation: weaker rank wins the path.
func confidenceRank(c string) int {
	switch c {
	case ConfExact:
		return 1
	case ConfScoped:
		return 2
	case ConfHeuristic:
		return 3
	case ConfUnresolved:
		return 4
	default:
		return 3
	}
}

func weakerConfidence(parentConf, parentReason, edgeConf, edgeReason string) (string, string) {
	if confidenceRank(edgeConf) > confidenceRank(parentConf) {
		return edgeConf, edgeReason
	}
	if confidenceRank(parentConf) > confidenceRank(edgeConf) {
		return parentConf, parentReason
	}
	// Equal rank — prefer keeping the parent's reason (oldest signal wins).
	if parentReason != "" {
		return parentConf, parentReason
	}
	return edgeConf, edgeReason
}

func confidenceImproves(prev, candidate string) bool {
	return confidenceRank(candidate) < confidenceRank(prev)
}

// MarshalJSON renders Symbol via compactMainSymbolJSON instead of
// CGPSymbolSummary's full shape when compactMainSymbol is set — mirrors
// TraceSymbolResponse.MarshalJSON (cgp.go) exactly; see
// compactMainSymbolJSON's doc comment there. The default, non-compact path
// is byte-for-byte unaffected by this method existing at all, since it
// falls straight through to the type-aliased default marshaling below.
func (resp ImpactResponse) MarshalJSON() ([]byte, error) {
	type alias ImpactResponse
	if !resp.compactMainSymbol || resp.Symbol == nil {
		return json.Marshal(alias(resp))
	}
	lean := compactMainSymbolJSON{
		Name:      resp.Symbol.Name,
		Kind:      resp.Symbol.Kind,
		File:      resp.Symbol.File,
		StartLine: resp.Symbol.StartLine,
		Signature: resp.Symbol.Signature,
		Docstring: resp.Symbol.Docstring,
		// CGPSymbolSummary (unlike CGPSymbol) has no Confidence field —
		// Confidence stays zero-valued and omitempty drops it.
	}
	return json.Marshal(struct {
		alias
		Symbol *compactMainSymbolJSON `json:"symbol,omitempty"`
	}{alias: alias(resp), Symbol: &lean})
}

package mamari

import "fmt"

// maxDeadCodeSymbols bounds the response size for DeadCode, mirroring the
// maxImpactSymbols cap used by Impact for the same reason: keep the response
// useful and boundable even on very large indexes.
const maxDeadCodeSymbols = 500

// defaultDeadCodeKinds is the conservative default candidate set. It
// deliberately excludes "method"/"callback"/"getter"/"setter" because those
// are frequently invoked via interface satisfaction, framework lifecycle
// hooks, or event callbacks that don't appear as direct "calls" edges.
var defaultDeadCodeKinds = map[string]bool{
	"function":  true,
	"class":     true,
	"interface": true,
	"component": true,
}

// edgeMarksUsage reports whether an edge of this type, from edge.To's
// perspective, counts as evidence that edge.To is used/referenced. Nearly
// every CGP edge type qualifies; the function exists primarily so the intent
// is named and a future edge type can be excluded deliberately if it turns
// out not to indicate real usage.
func edgeMarksUsage(edgeType string) bool {
	switch edgeType {
	case "":
		return false
	default:
		return true
	}
}

// propagateDeadCodeUsageToParents marks a symbol's parent (e.g. the class
// owning a method) as referenced whenever the symbol itself is referenced.
//
// `new ClassName()` never produces an edge into the class symbol — only
// `calls` edges into whatever methods get called on the resulting instance
// do (see jsNewInstanceAssignmentAt/resolveScopedCall and their Go/Java/C#
// equivalents). Without this step, any class used entirely through its own
// instance methods — the overwhelmingly common case — has zero inbound
// edges to the class symbol itself and looks completely unreferenced,
// regardless of how heavily its methods are actually called. This produced
// widespread false positives for classes used through instance methods.
//
// Bounded fixed-point loop (not a single pass) to correctly handle a small
// amount of nesting (e.g. a class nested in a namespace-like construct)
// without assuming a fixed depth or risking an unbounded walk on a
// pathological/cyclic ParentID chain.
func propagateDeadCodeUsageToParents(symbols map[string]CGPSymbol, referenced map[string]bool) {
	for pass := 0; pass < 3; pass++ {
		changed := false
		for _, sym := range symbols {
			if sym.ParentID == "" || referenced[sym.ParentID] || !referenced[sym.ID] {
				continue
			}
			referenced[sym.ParentID] = true
			changed = true
		}
		if !changed {
			break
		}
	}
}

// DeadCode reports symbols that have no inbound CGP edges marking them as
// used, restricted to a conservative set of declaration kinds. It is a
// best-effort heuristic, not a proof of dead code: dynamic dispatch,
// reflection, string-based lookups, and code outside this index can all
// reference a symbol without producing a graph edge. Callers should treat the
// result as "candidates for manual review," not "safe to delete."
func DeadCode(idx *Index, opts DeadCodeOptions) DeadCodeResponse {
	// Symbol-only query: retain one immutable graph generation for the report.
	snap := idx.symbolGraphSnapshot()

	kindSet := defaultDeadCodeKinds
	if len(opts.Kinds) > 0 {
		kindSet = map[string]bool{}
		for _, k := range opts.Kinds {
			if k != "" {
				kindSet[k] = true
			}
		}
	}

	referenced := make(map[string]bool, snap.edgeCount())
	snap.forEachEdge(func(_ int, edge CGPEdge) bool {
		if edge.Confidence == ConfUnresolved {
			return true
		}
		if !edgeMarksUsage(edge.Type) {
			return true
		}
		if _, ok := snap.Symbols[edge.To]; !ok {
			return true
		}
		referenced[edge.To] = true
		return true
	})
	propagateDeadCodeUsageToParents(snap.Symbols, referenced)
	unresolvedNames := unresolvedCallNamesByLanguageGraph(snap, false)

	listOpts := ListSymbolsOptions{SourceOnly: true, IncludeTests: opts.IncludeTests, IncludeStories: opts.IncludeStories}

	var candidates []CGPSymbol
	var uncertain []CGPSymbol
	uncertainSkipped := 0
	for _, sym := range snap.Symbols {
		if !kindSet[sym.Kind] {
			continue
		}
		if !opts.IncludeExported && sym.Exported {
			continue
		}
		if shouldExcludeNoisyFile(sym.File, listOpts) {
			continue
		}
		if referenced[sym.ID] {
			continue
		}
		if hasUnresolvedName(unresolvedNames, sym) {
			uncertainSkipped++
			if opts.IncludeUncertain {
				uncertain = append(uncertain, sym)
			}
			continue
		}
		candidates = append(candidates, sym)
	}

	out := make([]CGPSymbolSummary, 0, len(candidates))
	for _, sym := range candidates {
		out = append(out, summarizeSymbol(sym))
	}
	sortSymbolSummaries(out, "")

	total := len(out)
	limit := opts.Limit
	if limit <= 0 {
		limit = maxDeadCodeSymbols
	}
	truncated := false
	if len(out) > limit {
		out = out[:limit]
		truncated = true
	}
	if out == nil {
		out = []CGPSymbolSummary{}
	}
	resp := DeadCodeResponse{
		Status:           "ok",
		Total:            total,
		Limit:            limit,
		Truncated:        truncated,
		UncertainSkipped: uncertainSkipped,
		Symbols:          out,
	}
	if len(uncertain) > 0 {
		unc := make([]CGPSymbolSummary, 0, len(uncertain))
		for _, sym := range uncertain {
			unc = append(unc, summarizeSymbol(sym))
		}
		sortSymbolSummaries(unc, "")
		if len(unc) > limit {
			unc = unc[:limit]
		}
		resp.Uncertain = unc
	}
	if uncertainSkipped > 0 {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf("%d symbol(s) omitted because unresolved same-name calls may reference them — not asserted dead", uncertainSkipped))
	}
	return resp
}

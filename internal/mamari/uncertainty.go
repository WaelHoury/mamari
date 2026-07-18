package mamari

import "strings"

type possibleUnresolvedCaller struct {
	edge   CGPEdge
	caller CGPSymbol
}

// unresolvedCallTargetName returns the callable suffix encoded in an
// unresolved call edge. It deliberately does not infer a receiver type; it
// only recovers the name that may match one of several real symbols.
func unresolvedCallTargetName(edge CGPEdge) string {
	if edge.Type != "calls" || edge.Confidence != ConfUnresolved {
		return ""
	}
	target := strings.TrimPrefix(edge.To, "unresolved:")
	if hash := strings.LastIndexByte(target, '#'); hash >= 0 {
		target = target[hash+1:]
	}
	target = strings.ReplaceAll(target, "->", ".")
	target = strings.ReplaceAll(target, "::", ".")
	if dot := strings.LastIndexByte(target, '.'); dot >= 0 {
		target = target[dot+1:]
	}
	return strings.TrimSpace(target)
}

func possibleUnresolvedCallersForIndex(snap indexSnapshot, target CGPSymbol) []possibleUnresolvedCaller {
	var out []possibleUnresolvedCaller
	targetFamily := languageFamily(target.Language)
	snap.forEachEdge(func(_ int, edge CGPEdge) bool {
		appendPossibleUnresolvedCaller(&out, snap.Symbols, edge, target, targetFamily)
		return true
	})
	return out
}

func appendPossibleUnresolvedCaller(out *[]possibleUnresolvedCaller, symbols map[string]CGPSymbol, edge CGPEdge, target CGPSymbol, targetFamily string) {
	if unresolvedCallTargetName(edge) != target.Name {
		return
	}
	caller, ok := symbols[edge.From]
	if !ok || caller.Kind == "file" || languageFamily(caller.Language) != targetFamily {
		return
	}
	*out = append(*out, possibleUnresolvedCaller{edge: edge, caller: caller})
}

// unresolvedCallNamesByLanguage groups unresolved call names by the caller's
// language family. Reports such as dead_code can then remain conservative
// without repeatedly scanning every edge for every candidate symbol.
func unresolvedCallNamesByLanguageGraph(snap symbolGraphSnapshot, onlyTests bool) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	snap.forEachEdge(func(_ int, edge CGPEdge) bool {
		addUnresolvedCallName(out, snap.Symbols, edge, onlyTests)
		return true
	})
	return out
}

func unresolvedCallNamesByLanguageIndex(snap indexSnapshot, onlyTests bool) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	snap.forEachEdge(func(_ int, edge CGPEdge) bool {
		addUnresolvedCallName(out, snap.Symbols, edge, onlyTests)
		return true
	})
	return out
}

func addUnresolvedCallName(out map[string]map[string]bool, symbols map[string]CGPSymbol, edge CGPEdge, onlyTests bool) {
	name := unresolvedCallTargetName(edge)
	if name == "" {
		return
	}
	caller, ok := symbols[edge.From]
	if !ok || caller.Kind == "file" || onlyTests && !isTestCaller(caller) {
		return
	}
	family := languageFamily(caller.Language)
	if out[family] == nil {
		out[family] = map[string]bool{}
	}
	out[family][name] = true
}

func hasUnresolvedName(names map[string]map[string]bool, sym CGPSymbol) bool {
	return names[languageFamily(sym.Language)][sym.Name]
}

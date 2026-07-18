package mamari

import (
	"regexp"
	"sort"
	"strings"
)

const defaultInspectExactLimit = 8
const maxExactClassRuleLines = 24
const maxExactTemplateClassUsageLines = 5

var exactRouteHandlerRe = regexp.MustCompile(`(?i)\b(?:app|router|route)\s*\.\s*(get|post|put|patch|delete|head|options)\s*\(\s*['"]([^'"]+)['"]\s*,\s*([A-Za-z_$][A-Za-z0-9_$]*(?:\.[A-Za-z_$][A-Za-z0-9_$]*)?)`)

type exactClusterBuild struct {
	cluster ExactEvidenceCluster
	lines   map[int]*ExactEvidenceLine
	matched map[string]bool
}

// InspectExact returns a compact evidence bundle for queries that already
// contain rare literals such as route paths, MIME types, RDF predicates, or
// long identifiers. It deliberately returns only exact evidence lines unless
// WithSource is set, so agents can answer exact-string tasks without chaining
// search, inspect, and fetch calls.
func InspectExact(idx *Index, query string, opts InspectExactOptions) InspectExactResponse {
	query = strings.TrimSpace(query)
	resp := InspectExactResponse{
		Status:       "not_found",
		Query:        query,
		ExactPhrases: []SearchCodeExactPhrase{},
		Clusters:     []ExactEvidenceCluster{},
	}
	if query == "" {
		resp.Status = "invalid"
		resp.Warnings = append(resp.Warnings, "empty query")
		return resp
	}
	if opts.Limit <= 0 {
		opts.Limit = defaultInspectExactLimit
	}
	if opts.ContextLines < 0 {
		opts.ContextLines = 0
	}

	phrases := extractExactPhrases(query, idx.prefixNamesSnapshot())
	resp.ExactPhrases = exactPhraseSummaries(phrases)
	if len(phrases) == 0 {
		resp.Status = "invalid"
		resp.Warnings = append(resp.Warnings, "query has no exact literals, routes, predicates, MIME types, or long identifiers")
		return resp
	}

	idx.ensureFileSymbolIndex()
	files := idx.ensureCodeSearchIndex()
	stats := inspectExactPhraseStats(files, phrases)
	builders := map[string]*exactClusterBuild{}
	for _, file := range files {
		if isBackupOrDeadFile(file.file) {
			continue
		}
		if opts.SourceOnly && shouldExcludeNoisyFile(file.file, ListSymbolsOptions{IncludeTests: opts.IncludeTests}) {
			continue
		}
		for lineIndex, line := range file.lines {
			lineNumber := lineIndex + 1
			lineText := file.lineText(line)
			matched, exactScore := matchExactPhrases(lineText, line.tokenBloom, phrases)
			if len(matched) == 0 {
				continue
			}
			if inspectExactSingleCommonMatch(matched, phrases, stats) {
				continue
			}
			sym := idx.containingSymbolFast(file.file, lineNumber)
			className := matchedExactClassName(sym, lineText, matched)
			key := file.file
			if className != "" {
				key += "|class|" + className
			} else if sym.ID != "" && sym.Kind != "file" {
				key += "|sym|" + sym.ID
			} else if route, handler, ok := parseExactRouteHandler(lineText); ok {
				key += "|route|" + route + "|" + handler
			} else {
				key += "|file"
			}
			b := builders[key]
			if b == nil {
				b = &exactClusterBuild{
					cluster: ExactEvidenceCluster{File: file.file, Matched: []string{}, Lines: []ExactEvidenceLine{}},
					lines:   map[int]*ExactEvidenceLine{},
					matched: map[string]bool{},
				}
				if sym.ID != "" && sym.Kind != "file" {
					b.cluster.Symbol = sym.Name
					b.cluster.SymbolID = sym.ID
					b.cluster.StartLine = sym.StartLine
				}
				builders[key] = b
			}
			if className != "" {
				b.cluster.Symbol = className
				if b.cluster.SymbolID == "" || sym.Kind == "css-class" {
					b.cluster.SymbolID = sym.ID
					b.cluster.StartLine = sym.StartLine
				}
			}
			if route, handler, ok := parseExactRouteHandler(lineText); ok {
				b.cluster.Route = route
				b.cluster.Handler = handler
				b.cluster.Line = lineNumber
			}
			for _, m := range matched {
				b.matched[m] = true
			}
			addExactEvidenceLine(b, lineNumber, lineText, matched)
			if className != "" {
				if sym.Kind == "css-class" {
					addExactClassRuleLines(b, file, sym)
				} else if lineHasTemplateClassLiteral(lineText, className) {
					addExactTemplateClassUsageLines(b, file, lineNumber)
				} else {
					addExactClassRuleLinesFromLine(b, file, lineNumber, className)
				}
			}
			b.cluster.Score += exactScore
		}
	}
	addInspectExactRouteSymbolMatches(idx, builders, phrases)
	if len(builders) == 0 {
		return resp
	}

	clusters := make([]ExactEvidenceCluster, 0, len(builders))
	for _, b := range builders {
		for literal := range b.matched {
			b.cluster.Matched = append(b.cluster.Matched, literal)
		}
		sort.Strings(b.cluster.Matched)
		for _, line := range b.lines {
			sort.Strings(line.Matched)
			b.cluster.Lines = append(b.cluster.Lines, *line)
		}
		sort.Slice(b.cluster.Lines, func(i, j int) bool { return b.cluster.Lines[i].Line < b.cluster.Lines[j].Line })
		b.cluster.Score += len(b.cluster.Matched) * len(b.cluster.Matched) * 250
		if b.cluster.Symbol != "" {
			b.cluster.Score += 120
			b.cluster.Callers, b.cluster.Callees = compactExactSymbolEdges(idx, b.cluster.SymbolID)
			if exactClusterHasKind(b.cluster.Matched, phrases, "ident") && exactClusterHasKind(b.cluster.Matched, phrases, "mime") {
				b.cluster.Score += 800
			}
			if exactClusterHasKind(b.cluster.Matched, phrases, "ident") && exactClusterHasKind(b.cluster.Matched, phrases, "route") {
				b.cluster.Score += 800
			}
		}
		if b.cluster.Route != "" {
			b.cluster.Score += 160
		}
		if opts.WithSource {
			b.cluster.Source = exactClusterSource(idx, b.cluster)
		} else if opts.ContextLines > 0 {
			b.cluster.Source = exactClusterEvidenceContext(idx, b.cluster, opts.ContextLines)
		}
		b.cluster.EstimatedTokens = EstimateTokens(exactClusterTokenText(b.cluster))
		clusters = append(clusters, b.cluster)
	}
	sort.SliceStable(clusters, func(i, j int) bool {
		if clusters[i].Score != clusters[j].Score {
			return clusters[i].Score > clusters[j].Score
		}
		if len(clusters[i].Matched) != len(clusters[j].Matched) {
			return len(clusters[i].Matched) > len(clusters[j].Matched)
		}
		if clusters[i].File != clusters[j].File {
			return clusters[i].File < clusters[j].File
		}
		return clusters[i].StartLine < clusters[j].StartLine
	})
	if len(clusters) > opts.Limit {
		resp.Truncated = true
		clusters = clusters[:opts.Limit]
	}
	resp.Clusters = clusters
	for _, cluster := range clusters {
		resp.EstimatedTokens += cluster.EstimatedTokens
	}
	resp.Status = "ok"
	return resp
}

func addInspectExactRouteSymbolMatches(idx *Index, builders map[string]*exactClusterBuild, phrases []exactPhrase) {
	var routePhrases []exactPhrase
	for _, phrase := range phrases {
		if phrase.kind == "route" {
			routePhrases = append(routePhrases, phrase)
		}
	}
	if len(routePhrases) == 0 {
		return
	}
	snap := idx.snapshot()
	handlerByRoute := map[string]CGPSymbol{}
	snap.forEachEdge(func(_ int, edge CGPEdge) bool {
		if edge.Type != "handles-route" {
			return true
		}
		if handler, ok := snap.Symbols[edge.To]; ok {
			handlerByRoute[edge.From] = handler
		}
		return true
	})
	for _, sym := range snap.Symbols {
		if sym.Kind != "http-route" {
			continue
		}
		var matched []string
		score := 0
		for _, phrase := range routePhrases {
			if !httpRouteNameMatchesPathPhrase(sym.Name, phrase.literal) {
				continue
			}
			matched = append(matched, phrase.literal)
			score += phrase.weight
		}
		if len(matched) == 0 {
			continue
		}
		key := sym.File + "|sym|" + sym.ID
		b := builders[key]
		if b == nil {
			b = &exactClusterBuild{
				cluster: ExactEvidenceCluster{File: sym.File, Symbol: sym.Name, SymbolID: sym.ID, StartLine: sym.StartLine, Route: sym.Name, Line: sym.StartLine, Matched: []string{}, Lines: []ExactEvidenceLine{}},
				lines:   map[int]*ExactEvidenceLine{},
				matched: map[string]bool{},
			}
			builders[key] = b
		}
		if b.cluster.Symbol == "" {
			b.cluster.Symbol = sym.Name
			b.cluster.SymbolID = sym.ID
			b.cluster.StartLine = sym.StartLine
		}
		if b.cluster.Route == "" {
			b.cluster.Route = sym.Name
			b.cluster.Line = sym.StartLine
		}
		if handler := handlerByRoute[sym.ID]; handler.ID != "" && b.cluster.Handler == "" {
			b.cluster.Handler = handler.Name
		}
		for _, literal := range matched {
			b.matched[literal] = true
		}
		if sym.Signature != "" {
			addExactEvidenceLine(b, sym.StartLine, sym.Signature, matched)
		}
		b.cluster.Score += score + 400
	}
}

type exactPhraseStat struct {
	lines int
	files int
}

func inspectExactPhraseStats(files []codeSearchFile, phrases []exactPhrase) map[string]exactPhraseStat {
	out := map[string]exactPhraseStat{}
	if len(files) == 0 || len(phrases) == 0 {
		return out
	}
	fileSeen := map[string]map[string]bool{}
	for _, file := range files {
		for _, line := range file.lines {
			matched, _ := matchExactPhrases(file.lineText(line), line.tokenBloom, phrases)
			for _, literal := range matched {
				stat := out[literal]
				stat.lines++
				out[literal] = stat
				if fileSeen[literal] == nil {
					fileSeen[literal] = map[string]bool{}
				}
				fileSeen[literal][file.file] = true
			}
		}
	}
	for literal, seen := range fileSeen {
		stat := out[literal]
		stat.files = len(seen)
		out[literal] = stat
	}
	return out
}

func inspectExactSingleCommonMatch(matched []string, phrases []exactPhrase, stats map[string]exactPhraseStat) bool {
	if len(matched) != 1 || len(phrases) <= 1 {
		return false
	}
	literal := matched[0]
	kind := ""
	for _, phrase := range phrases {
		if phrase.literal == literal {
			kind = phrase.kind
			break
		}
	}
	switch kind {
	case "ident", "kebab-ident":
	default:
		return false
	}
	stat := stats[literal]
	return stat.lines > 25 || stat.files > 8
}

func addExactEvidenceLine(b *exactClusterBuild, number int, text string, matched []string) {
	if b == nil || number <= 0 {
		return
	}
	entry := b.lines[number]
	if entry == nil {
		text = strings.TrimRight(text, "\r\n")
		entry = &ExactEvidenceLine{Line: number, Text: strings.TrimSpace(text)}
		b.lines[number] = entry
	}
	entry.Matched = mergeMatchedTerms(entry.Matched, matched)
}

func matchedExactClassName(sym CGPSymbol, lineText string, matched []string) string {
	for _, literal := range matched {
		className := normalizeExactClassLiteral(literal)
		if !isLikelyExactKebabIdentifier(className) {
			continue
		}
		if sym.ID != "" && (sym.Kind == "template-class" || sym.Kind == "css-class") && className == sym.Name {
			return className
		}
		if lineHasExactClassLiteral(lineText, className) {
			return className
		}
	}
	return ""
}

func lineHasExactClassLiteral(lineText, className string) bool {
	if className == "" || lineText == "" {
		return false
	}
	trimmed := strings.TrimSpace(lineText)
	if strings.Contains(trimmed, "."+className) {
		return true
	}
	if lineHasTemplateClassLiteral(trimmed, className) {
		return true
	}
	return false
}

func lineHasTemplateClassLiteral(lineText, className string) bool {
	return strings.Contains(lineText, "class=") && strings.Contains(lineText, className)
}

func addExactClassRuleLines(b *exactClusterBuild, file codeSearchFile, sym CGPSymbol) {
	if sym.Kind != "css-class" || sym.StartLine <= 0 || sym.EndLine < sym.StartLine {
		return
	}
	added := 0
	for lineIndex, line := range file.lines {
		lineNumber := lineIndex + 1
		if lineNumber < sym.StartLine || lineNumber > sym.EndLine {
			continue
		}
		addExactEvidenceLine(b, lineNumber, file.lineText(line), nil)
		added++
		if added >= maxExactClassRuleLines {
			break
		}
	}
}

func addExactTemplateClassUsageLines(b *exactClusterBuild, file codeSearchFile, startLine int) {
	if startLine <= 0 {
		return
	}
	added := 0
	for lineIndex, line := range file.lines {
		lineNumber := lineIndex + 1
		if lineNumber < startLine {
			continue
		}
		lineText := file.lineText(line)
		addExactEvidenceLine(b, lineNumber, lineText, nil)
		added++
		if added >= maxExactTemplateClassUsageLines {
			return
		}
		trimmed := strings.TrimSpace(lineText)
		if added > 1 && strings.HasPrefix(trimmed, "</") {
			return
		}
	}
}

func addExactClassRuleLinesFromLine(b *exactClusterBuild, file codeSearchFile, startLine int, className string) {
	if startLine <= 0 || className == "" {
		return
	}
	started := false
	added := 0
	for lineIndex, line := range file.lines {
		lineNumber := lineIndex + 1
		if lineNumber < startLine {
			continue
		}
		lineText := file.lineText(line)
		if !started {
			if lineNumber != startLine || !strings.Contains(lineText, "{") || !strings.Contains(lineText, "."+className) {
				return
			}
			started = true
		}
		addExactEvidenceLine(b, lineNumber, lineText, nil)
		added++
		if strings.Contains(lineText, "}") || added >= maxExactClassRuleLines {
			return
		}
	}
}

func exactClusterHasKind(matched []string, phrases []exactPhrase, kind string) bool {
	if len(matched) == 0 || len(phrases) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, literal := range matched {
		seen[literal] = true
	}
	for _, phrase := range phrases {
		if phrase.kind == kind && seen[phrase.literal] {
			return true
		}
	}
	return false
}

func parseExactRouteHandler(line string) (route, handler string, ok bool) {
	m := exactRouteHandlerRe.FindStringSubmatch(line)
	if len(m) != 4 {
		return "", "", false
	}
	return strings.ToUpper(m[1]) + " " + m[2], m[3], true
}

func compactExactSymbolEdges(idx *Index, symbolID string) (callers, callees []CGPSymbolSummary) {
	if symbolID == "" {
		return nil, nil
	}
	snap := idx.symbolGraphSnapshot()
	seenCallers := map[string]bool{}
	seenCallees := map[string]bool{}
	snap.forEachEdge(func(_ int, edge CGPEdge) bool {
		if !compactExactEdgeType(edge.Type) {
			return true
		}
		if edge.To == symbolID {
			if sym, ok := snap.Symbols[edge.From]; ok && !seenCallers[sym.ID] {
				seenCallers[sym.ID] = true
				callers = append(callers, summarizeSymbol(sym))
			}
		}
		if edge.From == symbolID {
			if sym, ok := snap.Symbols[edge.To]; ok && !seenCallees[sym.ID] {
				seenCallees[sym.ID] = true
				callees = append(callees, summarizeSymbol(sym))
			}
		}
		return true
	})
	sort.Slice(callers, func(i, j int) bool {
		if callers[i].File != callers[j].File {
			return callers[i].File < callers[j].File
		}
		return callers[i].StartLine < callers[j].StartLine
	})
	sort.Slice(callees, func(i, j int) bool {
		if callees[i].File != callees[j].File {
			return callees[i].File < callees[j].File
		}
		return callees[i].StartLine < callees[j].StartLine
	})
	if len(callers) > 5 {
		callers = callers[:5]
	}
	if len(callees) > 5 {
		callees = callees[:5]
	}
	return callers, callees
}

func compactExactEdgeType(edgeType string) bool {
	switch edgeType {
	case "calls", "renders-component", "passes-prop", "binds-model", "listens-event", "handles-route", "calls-http-route", "uses-css-class":
		return true
	default:
		return false
	}
}

func exactClusterEvidenceContext(idx *Index, cluster ExactEvidenceCluster, contextLines int) string {
	if len(cluster.Lines) == 0 {
		return ""
	}
	ranges := make([][2]int, 0, len(cluster.Lines))
	for _, line := range cluster.Lines {
		ranges = append(ranges, [2]int{line.Line - contextLines, line.Line + contextLines})
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i][0] < ranges[j][0] })
	merged := ranges[:0]
	for _, r := range ranges {
		if r[0] < 1 {
			r[0] = 1
		}
		if len(merged) == 0 || r[0] > merged[len(merged)-1][1]+1 {
			merged = append(merged, r)
			continue
		}
		if r[1] > merged[len(merged)-1][1] {
			merged[len(merged)-1][1] = r[1]
		}
	}
	var b strings.Builder
	for i, r := range merged {
		src, err := FetchSource(idx, cluster.File, r[0], r[1])
		if err != nil {
			continue
		}
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(src.Text)
	}
	return b.String()
}

func exactClusterSource(idx *Index, cluster ExactEvidenceCluster) string {
	if cluster.SymbolID != "" {
		idx.mu.Lock()
		sym := idx.Symbols[cluster.SymbolID]
		idx.mu.Unlock()
		if sym.ID != "" && sym.StartLine > 0 && sym.EndLine >= sym.StartLine {
			if src, err := FetchSource(idx, sym.File, sym.StartLine, sym.EndLine); err == nil {
				return src.Text
			}
		}
	}
	return exactClusterEvidenceContext(idx, cluster, 3)
}

func exactClusterTokenText(cluster ExactEvidenceCluster) string {
	var b strings.Builder
	b.WriteString(cluster.File)
	b.WriteString(cluster.Symbol)
	b.WriteString(cluster.Route)
	b.WriteString(cluster.Handler)
	for _, m := range cluster.Matched {
		b.WriteString(m)
	}
	for _, line := range cluster.Lines {
		b.WriteString(line.Text)
	}
	for _, sym := range cluster.Callers {
		b.WriteString(sym.Name)
		b.WriteString(sym.File)
	}
	for _, sym := range cluster.Callees {
		b.WriteString(sym.Name)
		b.WriteString(sym.File)
	}
	b.WriteString(cluster.Source)
	return b.String()
}

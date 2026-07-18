package mamari

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var (
	routeTemplateParamRe = regexp.MustCompile(`\$\{[^}]+\}`)
	routeColonParamRe    = regexp.MustCompile(`:[A-Za-z_$][A-Za-z0-9_$-]*`)
)

var httpMethods = map[string]bool{
	"get": true, "post": true, "put": true, "patch": true, "delete": true, "head": true, "options": true,
}

var expressReceiverNames = map[string]bool{
	"app": true, "router": true, "route": true,
}

// ScanFrameworkSymbols adds compact framework-level symbols that are more
// useful to agents than raw source slices: currently Express-style HTTP route
// declarations. It is an additive overlay on top of the base JS/TS/Vue symbol
// graph and deliberately preserves the existing schema.
func ScanFrameworkSymbols(idx *Index, file, language, content string) {
	if !frameworkLanguage(language) {
		return
	}
	scanFrameworkBlocks(content, func(body string, baseOffset int) {
		for _, route := range scanExpressRoutes(body) {
			addHTTPRouteSymbol(idx, file, language, content, baseOffset, route)
		}
	})
}

// ScanFrameworkRelations connects framework symbols to code symbols:
// route -> handler and caller -> route for frontend/backend HTTP clients.
func ScanFrameworkRelations(idx *Index, file, language, content string) {
	if !frameworkLanguage(language) {
		return
	}
	fileStarts := lineStarts(content)
	scanFrameworkBlocks(content, func(body string, baseOffset int) {
		for _, route := range scanExpressRoutes(body) {
			line, col := offsetToLineCol(fileStarts, baseOffset+route.start)
			routeID, ok := httpRouteSymbolID(idx, file, route.method, route.path)
			if !ok {
				continue
			}
			target, confidence, reason := resolveSymbolCall(idx, file, route.handler)
			idx.AddCGPEdgeWithReason(routeID, target, "handles-route", confidence, reason, Location{
				File: file, StartLine: line, StartColumn: col,
				EndLine: line, EndColumn: col + len(route.method) + len(route.path) + 1,
				Kind: "http-route", Raw: route.method + " " + route.path + " -> " + route.handler,
			})
		}
		for _, call := range scanHTTPClientCalls(body) {
			line, col := offsetToLineCol(fileStarts, baseOffset+call.start)
			from := idx.containingSymbolFast(file, line)
			if from.ID == "" {
				from = CGPSymbol{ID: fileSymbolID(file)}
			}
			for _, match := range resolveOrAddHTTPEndpointSymbols(idx, file, language, content, baseOffset, call) {
				idx.AddCGPEdge(from.ID, match.id, "calls-http-route", match.confidence, Location{
					File: file, StartLine: line, StartColumn: col,
					EndLine: line, EndColumn: col + len(call.callee),
					Kind: "http-call", Raw: call.method + " " + call.path,
				})
			}
		}
	})
}

func frameworkLanguage(language string) bool {
	return language == "javascript" || language == "typescript" || language == "vue"
}

func scanFrameworkBlocks(content string, scan func(body string, baseOffset int)) {
	if strings.Contains(content, "<script") && strings.Contains(content, "</script>") {
		for _, block := range scriptBlockRe.FindAllStringSubmatchIndex(content, -1) {
			scan(content[block[2]:block[3]], block[2])
		}
		return
	}
	scan(content, 0)
}

type frameworkRoute struct {
	method  string
	path    string
	handler string
	start   int
}

type httpClientCall struct {
	method string
	path   string
	callee string
	start  int
}

func scanExpressRoutes(src string) []frameworkRoute {
	tokens := significantJSTokens(TokenizeJS(src))
	var out []frameworkRoute
	for i, tok := range tokens {
		if !tokenIsPunct(tok, src, "(") {
			continue
		}
		parts, start, ok := dottedCalleeBefore(tokens, i)
		if !ok || len(parts) < 2 {
			continue
		}
		method := strings.ToLower(parts[len(parts)-1])
		if !httpMethods[method] || !looksLikeExpressReceiver(parts) {
			continue
		}
		routeArg := firstArgumentToken(tokens, i)
		if routeArg == nil || (routeArg.Kind != TokString && routeArg.Kind != TokTemplate) || !isLikelyRouteLiteral(routeArg.Value) {
			continue
		}
		handler := lastHandlerArgument(tokens, i)
		if handler == "" {
			continue
		}
		out = append(out, frameworkRoute{
			method:  strings.ToUpper(method),
			path:    routeArg.Value,
			handler: handler,
			start:   start,
		})
	}
	return dedupeFrameworkRoutes(out)
}

func scanHTTPClientCalls(src string) []httpClientCall {
	tokens := significantJSTokens(TokenizeJS(src))
	var out []httpClientCall
	for i, tok := range tokens {
		if !tokenIsPunct(tok, src, "(") {
			continue
		}
		parts, start, ok := dottedCalleeBefore(tokens, i)
		if !ok || len(parts) == 0 {
			continue
		}
		method := ""
		if len(parts) == 1 && parts[0] == "fetch" {
			method = fetchMethod(tokens, i)
		} else {
			last := strings.ToLower(parts[len(parts)-1])
			if !httpMethods[last] || looksLikeExpressReceiver(parts) {
				continue
			}
			method = strings.ToUpper(last)
		}
		routeArg := firstArgumentToken(tokens, i)
		if routeArg == nil || (routeArg.Kind != TokString && routeArg.Kind != TokTemplate) || !isLikelyRouteLiteral(routeArg.Value) {
			continue
		}
		out = append(out, httpClientCall{
			method: method,
			path:   routeArg.Value,
			callee: strings.Join(parts, "."),
			start:  start,
		})
	}
	return dedupeHTTPClientCalls(out)
}

func significantJSTokens(tokens []Token) []Token {
	out := tokens[:0]
	for _, tok := range tokens {
		if tok.Kind == TokComment || tok.Kind == TokLineComment {
			continue
		}
		out = append(out, tok)
	}
	return out
}

func tokenIsPunct(tok Token, src, value string) bool {
	return tok.Kind == TokPunct && tok.Start >= 0 && tok.End <= len(src) && src[tok.Start:tok.End] == value
}

func tokenName(tok Token, src string) string {
	if tok.Value != "" {
		return tok.Value
	}
	if tok.Start >= 0 && tok.End <= len(src) {
		return src[tok.Start:tok.End]
	}
	return ""
}

func dottedCalleeBefore(tokens []Token, openIdx int) ([]string, int, bool) {
	if openIdx <= 0 {
		return nil, 0, false
	}
	i := openIdx - 1
	if tokens[i].Kind != TokIdent && tokens[i].Kind != TokKeyword {
		return nil, 0, false
	}
	parts := []string{tokenName(tokens[i], "")}
	start := tokens[i].Start
	for i >= 2 && tokens[i-1].Kind == TokPunct && tokens[i-1].Value == "." && (tokens[i-2].Kind == TokIdent || tokens[i-2].Kind == TokKeyword) {
		parts = append([]string{tokenName(tokens[i-2], "")}, parts...)
		start = tokens[i-2].Start
		i -= 2
	}
	for _, part := range parts {
		if part == "" {
			return nil, 0, false
		}
	}
	return parts, start, true
}

func looksLikeExpressReceiver(parts []string) bool {
	if len(parts) < 2 {
		return false
	}
	if expressReceiverNames[strings.ToLower(parts[0])] {
		return true
	}
	if len(parts) >= 3 && strings.EqualFold(parts[len(parts)-2], "route") {
		return true
	}
	return false
}

func firstArgumentToken(tokens []Token, openIdx int) *Token {
	for i := openIdx + 1; i < len(tokens); i++ {
		if tokens[i].Kind == TokComment || tokens[i].Kind == TokLineComment {
			continue
		}
		if tokenValue(tokens[i]) == ")" {
			return nil
		}
		return &tokens[i]
	}
	return nil
}

func tokenValue(tok Token) string {
	if tok.Value != "" {
		return tok.Value
	}
	return ""
}

func lastHandlerArgument(tokens []Token, openIdx int) string {
	closeIdx := matchingTokenClose(tokens, openIdx)
	if closeIdx <= openIdx {
		return ""
	}
	var last []string
	for i := openIdx + 1; i < closeIdx; i++ {
		if tokens[i].Kind != TokIdent && tokens[i].Kind != TokKeyword {
			continue
		}
		parts := []string{tokens[i].Value}
		j := i
		for j+2 < closeIdx && tokens[j+1].Kind == TokPunct && tokens[j+1].Value == "." && (tokens[j+2].Kind == TokIdent || tokens[j+2].Kind == TokKeyword) {
			parts = append(parts, tokens[j+2].Value)
			j += 2
		}
		if len(parts) > 0 && !callStopWords[parts[0]] && !httpMethods[strings.ToLower(parts[len(parts)-1])] {
			last = parts
		}
		i = j
	}
	return strings.Join(last, ".")
}

func matchingTokenClose(tokens []Token, openIdx int) int {
	if openIdx < 0 || openIdx >= len(tokens) || tokens[openIdx].Kind != TokPunct || tokens[openIdx].Value != "(" {
		return -1
	}
	depth := 0
	for i := openIdx; i < len(tokens); i++ {
		if tokens[i].Kind != TokPunct {
			continue
		}
		switch tokens[i].Value {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func fetchMethod(tokens []Token, openIdx int) string {
	closeIdx := matchingTokenClose(tokens, openIdx)
	if closeIdx <= openIdx {
		return "GET"
	}
	for i := openIdx + 1; i+2 < closeIdx; i++ {
		if tokens[i].Kind != TokIdent || tokens[i].Value != "method" {
			continue
		}
		if tokens[i+1].Kind != TokPunct || tokens[i+1].Value != ":" || tokens[i+2].Kind != TokString {
			continue
		}
		m := strings.ToUpper(tokens[i+2].Value)
		if httpMethods[strings.ToLower(m)] {
			return m
		}
	}
	return "GET"
}

func addHTTPRouteSymbol(idx *Index, file, language, content string, baseOffset int, route frameworkRoute) {
	starts := lineStarts(content)
	line, col := offsetToLineCol(starts, baseOffset+route.start)
	name := route.method + " " + route.path
	idx.AddCGPSymbol(CGPSymbol{
		ID:          stableSymbolID("http", "http-route", file, name, idx),
		Name:        name,
		Kind:        "http-route",
		Language:    language,
		File:        file,
		StartLine:   line,
		StartColumn: col,
		EndLine:     line,
		EndColumn:   col + len(name),
		Signature:   strings.TrimSpace(signatureLine(content, starts, line)),
		Confidence:  ConfExact,
	})
}

func httpRouteSymbolID(idx *Index, file, method, routePath string) (string, bool) {
	name := method + " " + routePath
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for id, sym := range idx.Symbols {
		if sym.File == file && sym.Kind == "http-route" && sym.Name == name {
			return id, true
		}
	}
	return "", false
}

// httpRouteMatch is one candidate target for a frontend HTTP call. A single
// element carries the resolver's confidence; multiple elements mean the call
// path matched several route declarations exactly and every one is reported
// (as heuristic) rather than silently picking a winner.
type httpRouteMatch struct {
	id         string
	confidence string
}

func resolveOrAddHTTPEndpointSymbols(idx *Index, file, language, content string, baseOffset int, call httpClientCall) []httpRouteMatch {
	if matches := resolveHTTPRouteSymbols(idx, call.method, call.path); len(matches) > 0 {
		return matches
	}
	starts := lineStarts(content)
	line, col := offsetToLineCol(starts, baseOffset+call.start)
	name := call.method + " " + call.path
	sym := idx.AddCGPSymbol(CGPSymbol{
		ID:          stableSymbolID("http", "http-endpoint", file, name, idx),
		Name:        name,
		Kind:        "http-endpoint",
		Language:    language,
		File:        file,
		StartLine:   line,
		StartColumn: col,
		EndLine:     line,
		EndColumn:   col + len(name),
		Signature:   strings.TrimSpace(signatureLine(content, starts, line)),
		Confidence:  ConfHeuristic,
	})
	return []httpRouteMatch{{id: sym.ID, confidence: ConfHeuristic}}
}

func resolveHTTPRouteSymbols(idx *Index, method, path string) []httpRouteMatch {
	want := normalizeHTTPRoutePath(path)
	if want == "" {
		return nil
	}
	type candidate struct {
		id         string
		confidence string
		score      int
		file       string
		line       int
	}
	var candidates []candidate
	idx.mu.Lock()
	for id, sym := range idx.Symbols {
		if sym.Kind != "http-route" {
			continue
		}
		routeMethod, routePath, ok := splitHTTPRouteName(sym.Name)
		if !ok || routeMethod != method {
			continue
		}
		got := normalizeHTTPRoutePath(routePath)
		switch {
		case got == want:
			candidates = append(candidates, candidate{id: id, confidence: ConfExact, score: 1000, file: sym.File, line: sym.StartLine})
		case routePathSuffixMatch(want, got):
			candidates = append(candidates, candidate{id: id, confidence: ConfScoped, score: len(routeSegments(got)), file: sym.File, line: sym.StartLine})
		}
	}
	idx.mu.Unlock()
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		if candidates[i].file != candidates[j].file {
			return candidates[i].file < candidates[j].file
		}
		return candidates[i].line < candidates[j].line
	})
	// Several route declarations match the call path *exactly* — typically a
	// live route file plus an orphaned copy (backup/old revision) declaring
	// the same path. The previous behavior picked candidates[0], i.e. the
	// alphabetically-first file, which sent agents into dead code whenever
	// the stale copy sorted first. An exact tie is genuine ambiguity: report
	// every exact match, each downgraded to heuristic, and let the caller
	// see both leads.
	var exact []httpRouteMatch
	for _, cand := range candidates {
		if cand.confidence == ConfExact {
			exact = append(exact, httpRouteMatch{id: cand.id, confidence: ConfHeuristic})
		}
	}
	if len(exact) == 1 {
		return []httpRouteMatch{{id: exact[0].id, confidence: ConfExact}}
	}
	if len(exact) > 1 {
		return exact
	}
	// Suffix-only matches: a tie between equally-specific prefixes stays
	// unresolved (the caller creates a dangling http-endpoint symbol), as
	// before.
	if len(candidates) > 1 && candidates[0].score == candidates[1].score {
		return nil
	}
	return []httpRouteMatch{{id: candidates[0].id, confidence: candidates[0].confidence}}
}

func splitHTTPRouteName(name string) (method, routePath string, ok bool) {
	parts := strings.SplitN(name, " ", 2)
	if len(parts) != 2 || !httpMethods[strings.ToLower(parts[0])] {
		return "", "", false
	}
	return strings.ToUpper(parts[0]), parts[1], true
}

func normalizeHTTPRoutePath(pathValue string) string {
	pathValue = strings.TrimSpace(pathValue)
	if pathValue == "" {
		return ""
	}
	if strings.HasPrefix(pathValue, "http://") || strings.HasPrefix(pathValue, "https://") {
		if idx := strings.Index(pathValue, "://"); idx >= 0 {
			rest := pathValue[idx+3:]
			if slash := strings.IndexByte(rest, '/'); slash >= 0 {
				pathValue = rest[slash:]
			}
		}
	}
	if q := strings.IndexAny(pathValue, "?#"); q >= 0 {
		pathValue = pathValue[:q]
	}
	pathValue = routeTemplateParamRe.ReplaceAllString(pathValue, ":")
	pathValue = routeColonParamRe.ReplaceAllString(pathValue, ":")
	pathValue = strings.ReplaceAll(pathValue, "//", "/")
	if pathValue != "/" {
		pathValue = strings.TrimRight(pathValue, "/")
	}
	return pathValue
}

func routePathSuffixMatch(callPath, routePath string) bool {
	callSegs := routeSegments(callPath)
	routeSegs := routeSegments(routePath)
	if len(callSegs) == 0 || len(routeSegs) == 0 || len(routeSegs) > len(callSegs) {
		return false
	}
	if len(routeSegs) < 2 {
		return false
	}
	offset := len(callSegs) - len(routeSegs)
	staticMatches := 0
	for i := range routeSegs {
		if routeSegs[i] == ":" || callSegs[offset+i] == ":" {
			continue
		}
		if routeSegs[i] != callSegs[offset+i] {
			return false
		}
		staticMatches++
	}
	return staticMatches > 0
}

func httpRouteNameMatchesPathPhrase(routeName, phrasePath string) bool {
	_, routePath, ok := splitHTTPRouteName(routeName)
	if !ok {
		return false
	}
	want := normalizeHTTPRoutePath(phrasePath)
	got := normalizeHTTPRoutePath(routePath)
	return want != "" && got != "" && (want == got || routePathSuffixMatch(want, got))
}

func routeSegments(pathValue string) []string {
	raw := strings.Split(strings.Trim(pathValue, "/"), "/")
	out := raw[:0]
	for _, seg := range raw {
		if seg == "" {
			continue
		}
		out = append(out, seg)
	}
	return out
}

func dedupeFrameworkRoutes(in []frameworkRoute) []frameworkRoute {
	seen := map[string]bool{}
	out := make([]frameworkRoute, 0, len(in))
	for _, route := range in {
		key := fmt.Sprintf("%s %s %s %d", route.method, route.path, route.handler, route.start)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, route)
	}
	return out
}

func dedupeHTTPClientCalls(in []httpClientCall) []httpClientCall {
	seen := map[string]bool{}
	out := make([]httpClientCall, 0, len(in))
	for _, call := range in {
		key := fmt.Sprintf("%s %s %s %d", call.method, call.path, call.callee, call.start)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, call)
	}
	return out
}

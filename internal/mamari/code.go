package mamari

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

var (
	topConstStringRe = regexp.MustCompile("(?m)^\\s*(?:export\\s+)?const\\s+([A-Za-z_][A-Za-z0-9_]*)\\s*=\\s*(?:\"([^\"]+)\"|'([^']+)'|`([^`]+)`)")
	topConstObjectRe = regexp.MustCompile(`(?ms)^\s*(?:export\s+)?const\s+([A-Za-z_][A-Za-z0-9_]*)\s*=\s*\{(.*?)\}\s*(?:as\s+const)?`)
	objectPropRe     = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*:\s*(?:"([^"]+)"|'([^']+)')`)
	templateNSRe     = regexp.MustCompile(`\$\{\s*([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)?)\s*\}([A-Za-z][A-Za-z0-9_.-]*)`)
	plusNSRe         = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)?)\s*\+\s*(?:"([A-Za-z][A-Za-z0-9_.-]*)"|'([A-Za-z][A-Za-z0-9_.-]*)')`)
	prefixedRe       = regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9_-]*):([A-Za-z][A-Za-z0-9_.-]*)\b`)
	localOnlyRe      = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]*$`)
	scriptBlockRe    = regexp.MustCompile(`(?is)<script\b[^>]*>(.*?)</script>`)
	templateBlockRe  = regexp.MustCompile(`(?is)<template\b[^>]*>(.*?)</template>`)
	styleBlockRe     = regexp.MustCompile(`(?is)<style\b[^>]*>(.*?)</style>`)
	htmlCommentRe    = regexp.MustCompile(`(?s)<!--.*?-->`)
	importNamedRe    = regexp.MustCompile(`(?ms)^\s*import\s+(?:type\s+)?\{\s*([^}]+)\s*\}\s*from\s*['"]([^'"]+)['"]`)
	importStarRe     = regexp.MustCompile(`(?m)^\s*import\s+\*\s+as\s+([A-Za-z_][A-Za-z0-9_]*)\s+from\s*['"]([^'"]+)['"]`)
	namedNodeCallRe  = regexp.MustCompile(`(?:([A-Za-z_][A-Za-z0-9_]*)\s*\.\s*)?\bnamedNode\s*\(`)
)

type importBinding struct {
	imported    string
	local       string
	isNamespace bool
}

type importStmt struct {
	spec     string
	bindings []importBinding
}

type vueBlockRange struct {
	start int
	end   int
}

func ScanCode(idx *Index, file string, content string, namespaces map[string]namespaceEntry) {
	if strings.HasSuffix(file, ".vue") {
		for _, block := range scriptBlockRe.FindAllStringSubmatchIndex(content, -1) {
			scanCodeBlock(idx, file, content, block[2], block[3], namespaces)
		}
		for _, block := range vueTemplateRanges(content) {
			scanTemplateBlock(idx, file, content, block.start, block.end)
		}
		return
	}
	scanCodeBlock(idx, file, content, 0, len(content), namespaces)
}

func vueTemplateRanges(content string) []vueBlockRange {
	lower := strings.ToLower(content)
	var out []vueBlockRange
	searchFrom := 0
	for {
		open := strings.Index(lower[searchFrom:], "<template")
		if open < 0 {
			break
		}
		open += searchFrom
		openEndRel := strings.IndexByte(content[open:], '>')
		if openEndRel < 0 {
			break
		}
		bodyStart := open + openEndRel + 1
		sectionEnd := nextVueTopLevelBlockStart(lower, bodyStart)
		closeRel := strings.LastIndex(lower[bodyStart:sectionEnd], "</template>")
		if closeRel < 0 {
			searchFrom = bodyStart
			continue
		}
		out = append(out, vueBlockRange{start: bodyStart, end: bodyStart + closeRel})
		searchFrom = sectionEnd
	}
	if len(out) > 0 {
		return out
	}
	for _, block := range templateBlockRe.FindAllStringSubmatchIndex(content, -1) {
		out = append(out, vueBlockRange{start: block[2], end: block[3]})
	}
	return out
}

func nextVueTopLevelBlockStart(lower string, from int) int {
	end := len(lower)
	for _, tag := range []string{"<script", "<style"} {
		if idx := strings.Index(lower[from:], tag); idx >= 0 && from+idx < end {
			end = from + idx
		}
	}
	return end
}

func scanTemplateBlock(idx *Index, file, fullContent string, startOffset, endOffset int) {
	starts := lineStarts(fullContent)
	raw := fullContent[startOffset:endOffset]
	masked := maskHTMLComments(raw)
	literals := scanStringLiterals(masked)
	for _, lit := range literals {
		scanLiteralValue(idx, file, fullContent, starts, startOffset, lit)
	}
}

func maskHTMLComments(s string) string {
	return htmlCommentRe.ReplaceAllStringFunc(s, func(match string) string {
		return strings.Repeat(" ", len(match))
	})
}

func scanCodeBlock(idx *Index, file, fullContent string, startOffset, endOffset int, namespaces map[string]namespaceEntry) {
	block := fullContent[startOffset:endOffset]
	if namespaces == nil {
		namespaces = map[string]namespaceEntry{}
	}
	starts := lineStarts(fullContent)
	literals := scanStringLiterals(block)
	for _, lit := range literals {
		scanLiteralValue(idx, file, fullContent, starts, startOffset, lit)
		for _, match := range templateNSRe.FindAllStringSubmatchIndex(lit.value, -1) {
			key := lit.value[match[2]:match[3]]
			local := lit.value[match[4]:match[5]]
			base, ok := namespaces[key]
			if !ok {
				continue
			}
			offset := startOffset + lit.valueStart + match[0]
			line, col := offsetToLineCol(starts, offset)
			addCodeRefByIRIHint(idx, file, fullContent, starts, base.IRI+local, base.PrefixHint, line, col, "heuristic", "namespace-template")
		}
	}
	for _, match := range plusNSRe.FindAllStringSubmatchIndex(block, -1) {
		key := block[match[2]:match[3]]
		local := ""
		if match[4] >= 0 {
			local = block[match[4]:match[5]]
		} else if match[6] >= 0 {
			local = block[match[6]:match[7]]
		}
		base, ok := namespaces[key]
		if !ok {
			continue
		}
		line, col := offsetToLineCol(starts, startOffset+match[0])
		addCodeRefByIRIHint(idx, file, fullContent, starts, base.IRI+local, base.PrefixHint, line, col, "heuristic", "namespace-concat")
	}
}

func fileLocalNamespaces(file, content string) map[string]namespaceEntry {
	if !strings.HasSuffix(file, ".vue") {
		return namespaceTable(content)
	}
	out := map[string]namespaceEntry{}
	for _, block := range scriptBlockRe.FindAllStringSubmatchIndex(content, -1) {
		for k, v := range namespaceTable(content[block[2]:block[3]]) {
			out[k] = v
		}
	}
	return out
}

func collectImports(file, content string) []importStmt {
	if !strings.HasSuffix(file, ".vue") {
		return parseImports(content)
	}
	var out []importStmt
	for _, block := range scriptBlockRe.FindAllStringSubmatchIndex(content, -1) {
		out = append(out, parseImports(content[block[2]:block[3]])...)
	}
	return out
}

func parseImports(content string) []importStmt {
	var out []importStmt
	for _, m := range importNamedRe.FindAllStringSubmatch(content, -1) {
		names, spec := m[1], m[2]
		var bindings []importBinding
		for _, raw := range strings.Split(names, ",") {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			raw = strings.TrimPrefix(raw, "type ")
			raw = strings.TrimSpace(raw)
			parts := strings.Fields(raw)
			switch len(parts) {
			case 1:
				bindings = append(bindings, importBinding{imported: parts[0], local: parts[0]})
			case 3:
				if parts[1] == "as" {
					bindings = append(bindings, importBinding{imported: parts[0], local: parts[2]})
				}
			}
		}
		if len(bindings) > 0 {
			out = append(out, importStmt{spec: spec, bindings: bindings})
		}
	}
	for _, m := range importStarRe.FindAllStringSubmatch(content, -1) {
		out = append(out, importStmt{spec: m[2], bindings: []importBinding{{local: m[1], isNamespace: true}}})
	}
	return out
}

func resolveImportPath(fromFile, spec string, codeFiles map[string]bool) string {
	if !strings.HasPrefix(spec, ".") {
		return ""
	}
	dir := path.Dir(fromFile)
	base := path.Clean(path.Join(dir, spec))
	if codeFiles[base] {
		return base
	}
	for _, ext := range []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".vue"} {
		if codeFiles[base+ext] {
			return base + ext
		}
	}
	for _, ext := range []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"} {
		if codeFiles[base+"/index"+ext] {
			return base + "/index" + ext
		}
	}
	return ""
}

func resolveEffectiveNamespaces(fileLocals map[string]map[string]namespaceEntry, fileImports map[string][]importStmt, codeFiles map[string]bool) map[string]map[string]namespaceEntry {
	out := map[string]map[string]namespaceEntry{}
	for file, locals := range fileLocals {
		eff := map[string]namespaceEntry{}
		for k, v := range locals {
			eff[k] = v
		}
		for _, stmt := range fileImports[file] {
			srcFile := resolveImportPath(file, stmt.spec, codeFiles)
			if srcFile == "" {
				continue
			}
			srcLocals := fileLocals[srcFile]
			if len(srcLocals) == 0 {
				continue
			}
			for _, b := range stmt.bindings {
				if b.isNamespace {
					for k, v := range srcLocals {
						if !strings.Contains(k, ".") {
							eff[b.local+"."+k] = v
						}
					}
					continue
				}
				if v, ok := srcLocals[b.imported]; ok {
					eff[b.local] = v
					continue
				}
				prefix := b.imported + "."
				rename := b.local + "."
				for k, v := range srcLocals {
					if strings.HasPrefix(k, prefix) {
						eff[rename+strings.TrimPrefix(k, prefix)] = v
					}
				}
			}
		}
		out[file] = eff
	}
	return out
}

func ScanDynamicIRICalls(file, content string) []DynamicIRICall {
	if strings.HasSuffix(file, ".vue") {
		var out []DynamicIRICall
		for _, block := range scriptBlockRe.FindAllStringSubmatchIndex(content, -1) {
			out = append(out, scanDynamicIRICallsInBlock(file, content, block[2], block[3])...)
		}
		return out
	}
	return scanDynamicIRICallsInBlock(file, content, 0, len(content))
}

func scanDynamicIRICallsInBlock(file, fullContent string, startOffset, endOffset int) []DynamicIRICall {
	block := fullContent[startOffset:endOffset]
	starts := lineStarts(fullContent)
	var out []DynamicIRICall
	matches := namedNodeCallRe.FindAllStringSubmatchIndex(block, -1)
	for _, m := range matches {
		callStart := m[0]
		openParen := m[1] - 1
		argEnd, ok := findMatchingParen(block, openParen)
		if !ok {
			continue
		}
		argText := strings.TrimSpace(block[openParen+1 : argEnd])
		if argText == "" {
			continue
		}
		if isResolvableNamedNodeArg(argText) {
			continue
		}
		callee := "namedNode"
		if m[2] >= 0 {
			callee = block[m[2]:m[3]] + ".namedNode"
		}
		line, col := offsetToLineCol(starts, startOffset+callStart)
		snippet := argText
		if len(snippet) > 120 {
			snippet = snippet[:120] + "…"
		}
		out = append(out, DynamicIRICall{
			File:    file,
			Line:    line,
			Column:  col,
			Callee:  callee,
			Snippet: snippet,
		})
	}
	return out
}

func findMatchingParen(s string, openIndex int) (int, bool) {
	if openIndex < 0 || openIndex >= len(s) || s[openIndex] != '(' {
		return 0, false
	}
	depth := 0
	i := openIndex
	for i < len(s) {
		c := s[i]
		if c == '\'' || c == '"' || c == '`' {
			quote := c
			i++
			for i < len(s) {
				if s[i] == '\\' && quote != '`' && i+1 < len(s) {
					i += 2
					continue
				}
				if s[i] == quote {
					i++
					break
				}
				i++
			}
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '/' {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < len(s) && s[i+1] == '*' {
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			i += 2
			continue
		}
		if c == '(' {
			depth++
		} else if c == ')' {
			depth--
			if depth == 0 {
				return i, true
			}
		}
		i++
	}
	return 0, false
}

func isPlainStringLiteral(s string) bool {
	if len(s) < 2 {
		return false
	}
	q := s[0]
	if q != '\'' && q != '"' && q != '`' {
		return false
	}
	if s[len(s)-1] != q {
		return false
	}
	if q == '`' && strings.Contains(s, "${") {
		return false
	}
	return true
}

var (
	resolvablePlusRe     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)?\s*\+\s*(?:"[A-Za-z][A-Za-z0-9_.\-]*"|'[A-Za-z][A-Za-z0-9_.\-]*')$`)
	resolvableTemplateRe = regexp.MustCompile("^`\\$\\{\\s*[A-Za-z_][A-Za-z0-9_]*(?:\\.[A-Za-z_][A-Za-z0-9_]*)?\\s*\\}[A-Za-z][A-Za-z0-9_.\\-]*`$")
)

func isResolvableNamedNodeArg(s string) bool {
	if isPlainStringLiteral(s) {
		return true
	}
	if resolvablePlusRe.MatchString(s) {
		return true
	}
	if resolvableTemplateRe.MatchString(s) {
		return true
	}
	return false
}

type namespaceEntry struct {
	IRI        string
	PrefixHint string
}

func namespaceTable(content string) map[string]namespaceEntry {
	out := map[string]namespaceEntry{}
	for _, match := range topConstStringRe.FindAllStringSubmatch(content, -1) {
		name, value := match[1], firstNonEmpty(match[2], match[3], match[4])
		if looksLikeIRIBase(value) {
			out[name] = namespaceEntry{IRI: value}
		}
	}
	for _, match := range topConstObjectRe.FindAllStringSubmatch(content, -1) {
		name, body := match[1], match[2]
		for _, prop := range objectPropRe.FindAllStringSubmatch(body, -1) {
			key, value := prop[1], firstNonEmpty(prop[2], prop[3])
			if looksLikeIRIBase(value) {
				out[name+"."+key] = namespaceEntry{IRI: value, PrefixHint: key}
			}
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func looksLikeIRIBase(value string) bool {
	return strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://")
}

type stringLiteral struct {
	start      int
	end        int
	valueStart int
	valueEnd   int
	quote      byte
	value      string
}

func scanStringLiterals(content string) []stringLiteral {
	var out []stringLiteral
	for i := 0; i < len(content); i++ {
		if content[i] == '/' && i+1 < len(content) && content[i+1] == '/' {
			for i < len(content) && content[i] != '\n' {
				i++
			}
			continue
		}
		if content[i] == '/' && i+1 < len(content) && content[i+1] == '*' {
			i += 2
			for i+1 < len(content) && !(content[i] == '*' && content[i+1] == '/') {
				i++
			}
			i++
			continue
		}
		ch := content[i]
		if ch != '\'' && ch != '"' && ch != '`' {
			continue
		}
		start := i
		quote := ch
		i++
		valueStart := i
		var b strings.Builder
		for i < len(content) {
			if content[i] == '\\' && quote != '`' && i+1 < len(content) {
				b.WriteByte(content[i+1])
				i += 2
				continue
			}
			if content[i] == quote {
				out = append(out, stringLiteral{start: start, end: i + 1, valueStart: valueStart, valueEnd: i, quote: quote, value: b.String()})
				break
			}
			b.WriteByte(content[i])
			i++
		}
	}
	return out
}

func scanLiteralValue(idx *Index, file, content string, starts []int, baseOffset int, lit stringLiteral) {
	seen := map[string]bool{}
	for _, match := range prefixedRe.FindAllStringSubmatchIndex(lit.value, -1) {
		prefix := lit.value[match[2]:match[3]]
		if prefix == "http" || prefix == "https" {
			continue
		}
		termText := lit.value[match[2]:match[5]]
		iri := idx.ResolveTerm(termText)
		if iri == "" {
			continue
		}
		offset := baseOffset + lit.valueStart + match[2]
		line, col := offsetToLineCol(starts, offset)
		key := termText + ":exact"
		if seen[key] {
			continue
		}
		seen[key] = true
		addCodeRef(idx, file, content, starts, termText, iri, line, col, "exact", "literal")
	}
	for _, iri := range idx.knownIRIStrings() {
		pos := strings.Index(lit.value, iri)
		if pos < 0 {
			continue
		}
		line, col := offsetToLineCol(starts, baseOffset+lit.valueStart+pos)
		addCodeRefByIRI(idx, file, content, starts, iri, line, col, "exact", "literal-iri")
	}
	for _, hit := range idx.fullIRIHits(lit.value) {
		line, col := offsetToLineCol(starts, baseOffset+lit.valueStart+hit.offset)
		addCodeRefByIRI(idx, file, content, starts, hit.iri, line, col, "exact", "literal-iri")
	}
	if localOnlyRe.MatchString(lit.value) {
		if !shouldIndexWeakLocal(lit.value) {
			return
		}
		for _, term := range idx.termsByLocal(lit.value) {
			line, col := offsetToLineCol(starts, baseOffset+lit.valueStart)
			addCodeRef(idx, file, content, starts, term.Term, term.IRI, line, col, "weak", "local-literal")
		}
	}
}

func shouldIndexWeakLocal(value string) bool {
	if len(value) < 5 {
		return false
	}
	switch strings.ToLower(value) {
	case "class", "type", "value", "name", "title", "label", "node", "path", "role", "data", "date":
		return false
	default:
		return true
	}
}

type iriHit struct {
	offset int
	iri    string
}

func (idx *Index) fullIRIHits(value string) []iriHit {
	idx.mu.Lock()
	prefixes := make([]Prefix, 0, len(idx.Prefixes))
	for _, p := range idx.Prefixes {
		prefixes = append(prefixes, p)
	}
	idx.mu.Unlock()
	var hits []iriHit
	for _, prefix := range prefixes {
		searchFrom := 0
		for {
			pos := strings.Index(value[searchFrom:], prefix.IRI)
			if pos < 0 {
				break
			}
			pos += searchFrom
			localStart := pos + len(prefix.IRI)
			localEnd := localStart
			for localEnd < len(value) {
				ch := value[localEnd]
				if (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '.' || ch == '-' {
					localEnd++
					continue
				}
				break
			}
			if localEnd > localStart {
				hits = append(hits, iriHit{offset: pos, iri: value[pos:localEnd]})
			}
			searchFrom = localEnd
		}
	}
	return hits
}

func addCodeRefByIRI(idx *Index, file, content string, starts []int, iri string, line, col int, confidence, kind string) {
	addCodeRefByIRIHint(idx, file, content, starts, iri, "", line, col, confidence, kind)
}

func addCodeRefByIRIHint(idx *Index, file, content string, starts []int, iri, prefixHint string, line, col int, confidence, kind string) {
	if prefixHint != "" {
		idx.mu.Lock()
		prefix, ok := idx.Prefixes[prefixHint]
		idx.mu.Unlock()
		if ok && strings.HasPrefix(iri, prefix.IRI) {
			addCodeRef(idx, file, content, starts, prefixHint+":"+strings.TrimPrefix(iri, prefix.IRI), iri, line, col, confidence, kind)
			return
		}
	}
	if compact := idx.CompactIRI(iri); compact != "" {
		addCodeRef(idx, file, content, starts, compact, iri, line, col, confidence, kind)
		return
	}
	terms := idx.termsByIRI(iri)
	if len(terms) > 0 {
		addCodeRef(idx, file, content, starts, terms[0].Term, terms[0].IRI, line, col, confidence, kind)
	}
}

func addCodeRef(idx *Index, file, content string, starts []int, termText, iri string, line, col int, confidence, kind string) {
	term := idx.AddTerm(termText, iri, nil)
	if term.ID == "" {
		return
	}
	context := contextPreview(content, starts, line)
	ref := Reference{
		ID:          fmt.Sprintf("ref:%s:%d:%d:%s", file, line, col, term.Term),
		TermID:      term.ID,
		Term:        term.Term,
		IRI:         term.IRI,
		File:        file,
		StartLine:   line,
		StartColumn: col,
		EndLine:     line,
		EndColumn:   col + len(term.LocalName),
		Confidence:  confidence,
		Kind:        kind,
		Context:     context,
	}
	idx.AddReference(ref)
	idx.AddEdge(ref.ID, term.ID, "references", confidence, Location{File: file, StartLine: line, StartColumn: col, EndLine: line, EndColumn: col + len(term.LocalName), Kind: kind, Raw: context})
}

// All Term lookup helpers below take Index.mu so they remain safe under the
// parallel build pipeline. Callers of these helpers must NOT already hold
// idx.mu.
func (idx *Index) knownIRIStrings() []string {
	idx.mu.Lock()
	terms := idx.codeTermLookupLocked()
	set := make(map[string]bool, len(terms))
	for _, term := range terms {
		if term.IRI != "" {
			set[term.IRI] = true
		}
	}
	idx.mu.Unlock()
	iris := make([]string, 0, len(set))
	for iri := range set {
		iris = append(iris, iri)
	}
	sort.Slice(iris, func(i, j int) bool { return len(iris[i]) > len(iris[j]) })
	return iris
}

func (idx *Index) termsByIRI(iri string) []Term {
	idx.mu.Lock()
	var terms []Term
	for _, term := range idx.codeTermLookupLocked() {
		if term.IRI == iri {
			terms = append(terms, term)
		}
	}
	idx.mu.Unlock()
	sort.Slice(terms, func(i, j int) bool { return terms[i].Term < terms[j].Term })
	return terms
}

func (idx *Index) termsByTerm(termText string) []Term {
	idx.mu.Lock()
	var terms []Term
	for _, term := range idx.codeTermLookupLocked() {
		if term.Term == termText {
			terms = append(terms, term)
		}
	}
	idx.mu.Unlock()
	sort.Slice(terms, func(i, j int) bool {
		if terms[i].IRI != terms[j].IRI {
			return terms[i].IRI < terms[j].IRI
		}
		return terms[i].ID < terms[j].ID
	})
	return terms
}

func (idx *Index) termsByLocal(local string) []Term {
	idx.mu.Lock()
	var terms []Term
	for _, term := range idx.codeTermLookupLocked() {
		if term.LocalName == local {
			terms = append(terms, term)
		}
	}
	idx.mu.Unlock()
	sort.Slice(terms, func(i, j int) bool { return terms[i].Term < terms[j].Term })
	return terms
}

func (idx *Index) beginCodeScanSnapshot() {
	idx.mu.Lock()
	idx.codeScanTerms = make(map[string]Term, len(idx.Terms))
	for id, term := range idx.Terms {
		idx.codeScanTerms[id] = term
	}
	idx.codeScanTermsActive = true
	idx.mu.Unlock()
}

func (idx *Index) endCodeScanSnapshot() {
	idx.mu.Lock()
	idx.codeScanTerms = nil
	idx.codeScanTermsActive = false
	idx.mu.Unlock()
}

func (idx *Index) codeTermLookupLocked() map[string]Term {
	if idx.codeScanTermsActive {
		return idx.codeScanTerms
	}
	return idx.Terms
}

func contextPreview(content string, starts []int, line int) string {
	if line < 1 || line > len(starts) {
		return ""
	}
	start := starts[line-1]
	end := len(content)
	if line < len(starts) {
		end = starts[line] - 1
	}
	raw := strings.TrimSpace(content[start:end])
	raw = strings.Join(strings.Fields(raw), " ")
	if len(raw) > 300 {
		return raw[:300]
	}
	return raw
}

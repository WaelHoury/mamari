package mamari

import (
	"regexp"
	"strings"
)

var (
	prefixRe          = regexp.MustCompile(`(?i)^\s*(?:@prefix|PREFIX)\s+([A-Za-z][A-Za-z0-9_-]*):\s*<([^>]+)>`)
	subjectRe         = regexp.MustCompile(`^\s*((?:[A-Za-z][A-Za-z0-9_-]*:[A-Za-z0-9_.-]+)|(?:<[^>\s]+>))(?:\s|$)`)
	nodeShapeRe       = regexp.MustCompile(`(?:\ba\b|\brdf:type\b)\s+[^.\n;]*\bsh:NodeShape\b`)
	termObjectRe      = regexp.MustCompile(`\b(sh:path|sh:node|sh:targetClass)\s+((?:[A-Za-z][A-Za-z0-9_-]*:[A-Za-z0-9_.-]+)|(?:<[^>\s]+>))`)
	predicateObjectRe = regexp.MustCompile(`(?m)(?:^|[\s;,])([A-Za-z][A-Za-z0-9_-]*:[A-Za-z][A-Za-z0-9_.-]*)\s+((?:[A-Za-z][A-Za-z0-9_-]*:[A-Za-z0-9_.-]+)|(?:<[^>\s]+>))`)
	complexPathRe     = regexp.MustCompile(`\bsh:path\s+(\(|\[)`)
	ttlTokenRe        = regexp.MustCompile(`(?:<[^>\s]+>)|(?:\b[A-Za-z][A-Za-z0-9_-]*:[A-Za-z0-9_.-]+\b)`)
	branchPropRe      = regexp.MustCompile(`\b(sh:name|sh:datatype|sh:pattern|sh:path|sh:class|sh:nodeKind|sh:node)\s+([^;\n\]]+?)(?:\s*[;\]]|\s*$)`)
)

var literalPredicates = map[string]bool{
	"sh:name":             true,
	"sh:message":          true,
	"sh:description":      true,
	"sh:title":            true,
	"rdfs:label":          true,
	"rdfs:comment":        true,
	"dcterms:title":       true,
	"dcterms:description": true,
	"skos:prefLabel":      true,
	"skos:altLabel":       true,
	"skos:definition":     true,
}

func ScanTTL(idx *Index, file string, content string) error {
	lines := strings.SplitAfter(content, "\n")
	localPrefixes := map[string]string{}
	for i, line := range lines {
		if match := prefixRe.FindStringSubmatch(line); match != nil {
			col := strings.Index(line, match[0]) + 1
			loc := Location{File: file, StartLine: i + 1, StartColumn: col, EndLine: i + 1, EndColumn: col + len(match[0]), Kind: "prefix", Raw: strings.TrimSpace(line)}
			localPrefixes[match[1]] = match[2]
			idx.AddPrefix(match[1], match[2], loc)
		}
	}
	if info, ok := idx.Files[file]; ok {
		info.Prefixes = localPrefixes
		idx.Files[file] = info
	}
	if isSHACLishTTL(content) {
		scanTTLTermUsages(idx, file, lines)
	}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		sub := subjectRe.FindStringSubmatchIndex(line)
		if sub == nil || strings.HasPrefix(strings.TrimSpace(line), "@prefix") {
			continue
		}
		subjectRaw := line[sub[2]:sub[3]]
		blockStart := i
		blockEnd := i
		var block strings.Builder
		for ; blockEnd < len(lines); blockEnd++ {
			block.WriteString(lines[blockEnd])
			if strings.HasSuffix(strings.TrimSpace(lines[blockEnd]), ".") {
				break
			}
		}
		lastLine := blockEnd
		if lastLine >= len(lines) {
			lastLine = len(lines) - 1
		}
		if lastLine < blockStart {
			continue
		}
		blockText := block.String()
		if !nodeShapeRe.MatchString(blockText) {
			i = lastLine
			continue
		}
		subjectTerm, subjectIRI := idx.normalizeTTLTokenInFile(file, subjectRaw)
		if subjectTerm == "" {
			i = lastLine
			continue
		}
		loc := Location{File: file, StartLine: blockStart + 1, StartColumn: sub[2] + 1, EndLine: lastLine + 1, EndColumn: len(lines[lastLine]), Kind: "shape", Raw: strings.TrimSpace(subjectRaw)}
		term := idx.AddTerm(subjectTerm, subjectIRI, &loc)
		shape := Shape{ID: "shape:" + subjectTerm + ":" + file + ":" + strings.TrimSpace(subjectRaw), TermID: term.ID, Term: subjectTerm, IRI: subjectIRI, Location: loc}
		matches := termObjectRe.FindAllStringSubmatchIndex(blockText, -1)
		starts := lineStarts(blockText)
		blockLines := strings.SplitAfter(blockText, "\n")
		for _, match := range matches {
			predicate := blockText[match[2]:match[3]]
			token := blockText[match[4]:match[5]]
			lineNo, col := offsetToLineCol(starts, match[4])
			raw := strings.TrimSpace(rawLine(blockLines, lineNo))
			termText, iri := idx.normalizeTTLTokenInFile(file, token)
			if termText == "" {
				continue
			}
			linkLoc := Location{File: file, StartLine: blockStart + lineNo, StartColumn: col, EndLine: blockStart + lineNo, EndColumn: col + len(token), Kind: predicate, Raw: raw}
			linked := idx.AddTerm(termText, iri, &linkLoc)
			link := ShapeLink{Term: termText, IRI: iri, Location: linkLoc}
			switch predicate {
			case "sh:path":
				shape.Paths = append(shape.Paths, link)
				idx.AddEdge(shape.ID, linked.ID, "sh:path", "exact", linkLoc)
			case "sh:node":
				shape.Nodes = append(shape.Nodes, link)
				idx.AddEdge(shape.ID, linked.ID, "sh:node", "exact", linkLoc)
			case "sh:targetClass":
				shape.TargetClasses = append(shape.TargetClasses, link)
				idx.AddEdge(shape.ID, linked.ID, "sh:targetClass", "exact", linkLoc)
			}
		}
		for _, match := range complexPathRe.FindAllStringSubmatchIndex(blockText, -1) {
			lineNo, col := offsetToLineCol(starts, match[0])
			shape.Unsupported = append(shape.Unsupported, Location{File: file, StartLine: blockStart + lineNo, StartColumn: col, EndLine: blockStart + lineNo, EndColumn: col + len("sh:path"), Kind: "unsupported:sh:path", Raw: strings.TrimSpace(rawLine(blockLines, lineNo))})
		}
		shape.Predicates = extractCustomPredicates(idx, file, blockText, blockStart)
		shape.Branches = extractBranches(idx, file, blockText, blockStart, lines)
		for _, br := range shape.Branches {
			if br.Kind == "sh:or" || br.Kind == "sh:xone" {
				idx.AddEdge(shape.ID, "branch:"+shape.ID+":"+br.Kind+":"+intKey(br.Location.StartLine), br.Kind, "exact", br.Location)
			}
		}
		idx.Shapes[shape.ID] = shape
		i = lastLine
	}
	scanLiterals(idx, file, content)
	attachLiteralsToShapes(idx, file)
	return nil
}

type literalHit struct {
	predicate string
	value     string
	lang      string
	line      int
	col       int
}

func scanLiterals(idx *Index, file, content string) {
	hits := collectLiterals(content)
	for _, h := range hits {
		idx.Literals = append(idx.Literals, Literal{
			Predicate: h.predicate,
			Value:     h.value,
			Lang:      h.lang,
			Location: Location{
				File:        file,
				StartLine:   h.line,
				StartColumn: h.col,
				EndLine:     h.line,
				EndColumn:   h.col + len(h.value) + 2,
				Kind:        h.predicate,
				Raw:         h.value,
			},
		})
	}
}

func collectLiterals(content string) []literalHit {
	var out []literalHit
	curPred := ""
	i := 0
	line := 1
	lineStart := 0
	for i < len(content) {
		c := content[i]
		switch {
		case c == '\n':
			line++
			lineStart = i + 1
			i++
		case c == '#':
			for i < len(content) && content[i] != '\n' {
				i++
			}
		case c == '"':
			startCol := i - lineStart + 1
			startLine := line
			value, advance, deltaLines, newLineStart := readTTLString(content, i)
			i = advance
			if deltaLines > 0 {
				line += deltaLines
				lineStart = newLineStart
			}
			lang := ""
			if i < len(content) && content[i] == '@' {
				j := i + 1
				for j < len(content) && (isAlpha(content[j]) || content[j] == '-' || isDigit(content[j])) {
					j++
				}
				lang = content[i+1 : j]
				i = j
			}
			if curPred != "" {
				out = append(out, literalHit{predicate: curPred, value: value, lang: lang, line: startLine, col: startCol})
			}
		case c == ';' || c == '.':
			curPred = ""
			i++
		case c == '[':
			curPred = ""
			i++
		case c == ']':
			curPred = ""
			i++
		case c == ',':
			i++
		case isAlpha(c) || c == '_':
			j := i
			for j < len(content) && (isAlpha(content[j]) || isDigit(content[j]) || content[j] == '_' || content[j] == '-' || content[j] == ':' || content[j] == '.') {
				j++
			}
			tok := content[i:j]
			if literalPredicates[tok] {
				curPred = tok
			}
			i = j
		default:
			i++
		}
	}
	return out
}

func readTTLString(content string, start int) (value string, end int, deltaLines int, newLineStart int) {
	i := start
	if i+2 < len(content) && content[i+1] == '"' && content[i+2] == '"' {
		i += 3
		var b strings.Builder
		for i+2 < len(content) {
			if content[i] == '"' && content[i+1] == '"' && content[i+2] == '"' {
				return b.String(), i + 3, deltaLines, newLineStart
			}
			if content[i] == '\\' && i+1 < len(content) {
				b.WriteByte(unescape(content[i+1]))
				i += 2
				continue
			}
			if content[i] == '\n' {
				deltaLines++
				newLineStart = i + 1
			}
			b.WriteByte(content[i])
			i++
		}
		return b.String(), len(content), deltaLines, newLineStart
	}
	i++
	var b strings.Builder
	for i < len(content) {
		if content[i] == '\\' && i+1 < len(content) {
			b.WriteByte(unescape(content[i+1]))
			i += 2
			continue
		}
		if content[i] == '"' {
			return b.String(), i + 1, deltaLines, newLineStart
		}
		if content[i] == '\n' {
			return b.String(), i, deltaLines, newLineStart
		}
		b.WriteByte(content[i])
		i++
	}
	return b.String(), i, deltaLines, newLineStart
}

func unescape(c byte) byte {
	switch c {
	case 'n':
		return '\n'
	case 't':
		return '\t'
	case 'r':
		return '\r'
	}
	return c
}

func isAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func attachLiteralsToShapes(idx *Index, file string) {
	type shapeRange struct {
		id        string
		startLine int
		endLine   int
	}
	var ranges []shapeRange
	for _, shape := range idx.Shapes {
		if shape.Location.File != file {
			continue
		}
		ranges = append(ranges, shapeRange{id: shape.ID, startLine: shape.Location.StartLine, endLine: shape.Location.EndLine})
	}
	for i, lit := range idx.Literals {
		if lit.Location.File != file || lit.ShapeID != "" {
			continue
		}
		for _, r := range ranges {
			if lit.Location.StartLine >= r.startLine && lit.Location.StartLine <= r.endLine {
				idx.Literals[i].ShapeID = r.id
				if lit.Predicate == "sh:name" {
					sh := idx.Shapes[r.id]
					sh.Names = append(sh.Names, idx.Literals[i])
					idx.Shapes[r.id] = sh
				}
				break
			}
		}
	}
}

var skipPredicate = map[string]bool{
	"sh:path":        true,
	"sh:node":        true,
	"sh:targetClass": true,
	"sh:datatype":    true,
	"sh:nodeKind":    true,
	"sh:class":       true,
	"sh:or":          true,
	"sh:xone":        true,
	"sh:and":         true,
	"sh:not":         true,
	"a":              true,
	"rdf:type":       true,
}

func extractCustomPredicates(idx *Index, file, blockText string, blockStart int) []ShapeLink {
	var out []ShapeLink
	starts := lineStarts(blockText)
	blockLines := strings.SplitAfter(blockText, "\n")
	seen := map[string]bool{}
	for _, m := range predicateObjectRe.FindAllStringSubmatchIndex(blockText, -1) {
		predicate := blockText[m[2]:m[3]]
		object := blockText[m[4]:m[5]]
		if skipPredicate[predicate] {
			continue
		}
		if literalPredicates[predicate] {
			continue
		}
		termText, iri := idx.normalizeTTLTokenInFile(file, object)
		if termText == "" {
			continue
		}
		lineNo, col := offsetToLineCol(starts, m[4])
		key := predicate + "|" + termText + "|" + intKey(lineNo) + "|" + intKey(col)
		if seen[key] {
			continue
		}
		seen[key] = true
		raw := strings.TrimSpace(rawLine(blockLines, lineNo))
		loc := Location{
			File:        file,
			StartLine:   blockStart + lineNo,
			StartColumn: col,
			EndLine:     blockStart + lineNo,
			EndColumn:   col + len(object),
			Kind:        predicate,
			Raw:         raw,
		}
		idx.AddTerm(termText, iri, &loc)
		out = append(out, ShapeLink{Predicate: predicate, Term: termText, IRI: iri, Location: loc})
	}
	return out
}

func extractBranches(idx *Index, file, blockText string, blockStart int, fileLines []string) []Branch {
	var out []Branch
	starts := lineStarts(blockText)
	for _, kind := range []string{"sh:or", "sh:xone"} {
		re := regexp.MustCompile(`\b` + regexp.QuoteMeta(kind) + `\s*\(`)
		for _, m := range re.FindAllStringIndex(blockText, -1) {
			openIdx := m[1] - 1
			closeIdx, ok := findMatchingTTLParen(blockText, openIdx)
			if !ok {
				continue
			}
			list := blockText[openIdx+1 : closeIdx]
			listOffset := openIdx + 1
			for _, br := range splitTTLBranches(list, listOffset) {
				lineNo, col := offsetToLineCol(starts, br.offset)
				absLine := blockStart + lineNo
				branch := Branch{
					Kind: kind,
					Location: Location{
						File:        file,
						StartLine:   absLine,
						StartColumn: col,
						EndLine:     absLine,
						EndColumn:   col + (br.end - br.offset),
						Kind:        kind + ":branch",
						Raw:         strings.TrimSpace(br.text),
					},
				}
				populateBranchFields(idx, file, &branch, br.text)
				out = append(out, branch)
			}
		}
	}
	_ = fileLines
	return out
}

type rawBranch struct {
	text   string
	offset int
	end    int
}

func splitTTLBranches(list string, baseOffset int) []rawBranch {
	var out []rawBranch
	depth := 0
	parenDepth := 0
	start := -1
	i := 0
	for i < len(list) {
		c := list[i]
		switch c {
		case '#':
			for i < len(list) && list[i] != '\n' {
				i++
			}
			continue
		case '"':
			_, end, _, _ := readTTLString(list, i)
			i = end
			continue
		case '(':
			parenDepth++
			i++
			continue
		case ')':
			parenDepth--
			i++
			continue
		case '[':
			if depth == 0 {
				start = i
			}
			depth++
			i++
			continue
		case ']':
			depth--
			if depth == 0 && start >= 0 {
				out = append(out, rawBranch{text: list[start : i+1], offset: baseOffset + start, end: baseOffset + i + 1})
				start = -1
			}
			i++
			continue
		default:
			i++
		}
	}
	return out
}

func findMatchingTTLParen(s string, openIndex int) (int, bool) {
	if openIndex < 0 || openIndex >= len(s) || s[openIndex] != '(' {
		return 0, false
	}
	depth := 0
	i := openIndex
	for i < len(s) {
		c := s[i]
		switch c {
		case '#':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			continue
		case '"':
			_, end, _, _ := readTTLString(s, i)
			i = end
			continue
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i, true
			}
		}
		i++
	}
	return 0, false
}

func populateBranchFields(idx *Index, file string, br *Branch, text string) {
	for _, m := range branchPropRe.FindAllStringSubmatch(text, -1) {
		predicate := m[1]
		raw := strings.TrimSpace(m[2])
		switch predicate {
		case "sh:name":
			if val, _ := firstStringLiteral(raw); val != "" {
				if br.Name == "" {
					br.Name = val
				}
			}
		case "sh:datatype":
			if tok := firstToken(raw); tok != "" {
				term, iri := idx.normalizeTTLTokenInFile(file, tok)
				br.Datatype = term
				br.DatatypeIRI = iri
			}
		case "sh:pattern":
			if val, _ := firstStringLiteral(raw); val != "" {
				br.Pattern = val
			}
		case "sh:path":
			if tok := firstToken(raw); tok != "" {
				term, iri := idx.normalizeTTLTokenInFile(file, tok)
				br.Path = term
				br.PathIRI = iri
			}
		}
	}
}

func firstStringLiteral(s string) (string, int) {
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			value, end, _, _ := readTTLString(s, i)
			return value, end
		}
	}
	return "", 0
}

func firstToken(s string) string {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) && !isSpace(s[end]) && s[end] != ',' && s[end] != ';' && s[end] != ']' {
		end++
	}
	return s[:end]
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func isSHACLishTTL(content string) bool {
	return strings.Contains(content, "sh:") ||
		strings.Contains(content, "http://www.w3.org/ns/shacl#")
}

func (idx *Index) normalizeTTLTokenInFile(file, token string) (string, string) {
	token = strings.TrimSpace(token)
	if strings.HasPrefix(token, "<") && strings.HasSuffix(token, ">") {
		iri := strings.TrimSuffix(strings.TrimPrefix(token, "<"), ">")
		return idx.CompactIRIInFile(file, iri), iri
	}
	iri := idx.ResolveTermInFile(file, token)
	if iri == "" {
		return token, ""
	}
	return token, iri
}

func scanTTLTermUsages(idx *Index, file string, lines []string) {
	for i, line := range lines {
		if prefixRe.MatchString(line) {
			continue
		}
		scannable := stripTTLComment(line)
		for _, match := range ttlTokenRe.FindAllStringIndex(scannable, -1) {
			token := scannable[match[0]:match[1]]
			termText, iri := idx.normalizeTTLTokenInFile(file, token)
			if termText == "" || iri == "" {
				continue
			}
			loc := Location{
				File:        file,
				StartLine:   i + 1,
				StartColumn: match[0] + 1,
				EndLine:     i + 1,
				EndColumn:   match[1] + 1,
				Kind:        "ttl:term",
			}
			idx.AddTerm(termText, iri, &loc)
		}
	}
}

func stripTTLComment(line string) string {
	inSingle := false
	inDouble := false
	escaped := false
	for i, r := range line {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && (inSingle || inDouble) {
			escaped = true
			continue
		}
		switch r {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble {
				return line[:i]
			}
		}
	}
	return line
}

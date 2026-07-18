package mamari

import (
	"regexp"
	"strings"
)

const maxDocstringChars = 320

var tsReturnAnnotationRe = regexp.MustCompile(`\)\s*:\s*([^={;]+)`)

func extractSymbolDocstring(lines []string, startLine int, language string) string {
	if startLine <= 1 {
		return ""
	}
	switch language {
	case "python":
		if doc := extractPythonInlineDocstring(lines, startLine); doc != "" {
			return trimDocstring(doc)
		}
	}
	return trimDocstring(extractLeadingCommentDocstring(lines, startLine))
}

func extractLeadingCommentDocstring(lines []string, startLine int) string {
	i := startLine - 2
	for i >= 0 && strings.TrimSpace(lines[i]) == "" {
		i--
	}
	if i < 0 {
		return ""
	}
	trimmed := strings.TrimSpace(lines[i])
	if strings.HasSuffix(trimmed, "*/") {
		var block []string
		for i >= 0 {
			line := strings.TrimSpace(lines[i])
			block = append([]string{line}, block...)
			if strings.HasPrefix(line, "/*") {
				break
			}
			i--
		}
		if len(block) == 0 || !strings.HasPrefix(strings.TrimSpace(block[0]), "/*") {
			return ""
		}
		var parts []string
		for _, line := range block {
			line = strings.TrimSpace(line)
			line = strings.TrimPrefix(line, "/**")
			line = strings.TrimPrefix(line, "/*")
			line = strings.TrimSuffix(line, "*/")
			line = strings.TrimPrefix(strings.TrimSpace(line), "*")
			line = strings.TrimSpace(line)
			if line != "" {
				parts = append(parts, line)
			}
		}
		return strings.Join(parts, " ")
	}
	var prefix string
	switch {
	case strings.HasPrefix(trimmed, "//"):
		prefix = "//"
	case strings.HasPrefix(trimmed, "#"):
		prefix = "#"
	default:
		return ""
	}
	var block []string
	for i >= 0 {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, prefix) {
			break
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, prefix))
		if line != "" {
			block = append([]string{line}, block...)
		}
		i--
	}
	return strings.Join(block, " ")
}

func extractPythonInlineDocstring(lines []string, startLine int) string {
	for i := startLine; i < len(lines) && i < startLine+8; i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		for _, quote := range []string{`"""`, `'''`, `"`, `'`} {
			if !strings.HasPrefix(line, quote) {
				continue
			}
			rest := strings.TrimPrefix(line, quote)
			if end := strings.Index(rest, quote); end >= 0 {
				return rest[:end]
			}
			var parts []string
			if rest != "" {
				parts = append(parts, rest)
			}
			for j := i + 1; j < len(lines) && j < i+12; j++ {
				next := lines[j]
				if end := strings.Index(next, quote); end >= 0 {
					parts = append(parts, next[:end])
					return strings.Join(parts, " ")
				}
				parts = append(parts, strings.TrimSpace(next))
			}
			return strings.Join(parts, " ")
		}
		break
	}
	return ""
}

func trimDocstring(doc string) string {
	doc = strings.Join(strings.Fields(doc), " ")
	doc = strings.TrimSpace(doc)
	if len(doc) <= maxDocstringChars {
		return doc
	}
	return strings.TrimSpace(doc[:maxDocstringChars-1]) + "..."
}

func inferReturnTypesFromSignature(signature, language string) []string {
	signature = strings.TrimSpace(signature)
	if signature == "" {
		return nil
	}
	switch language {
	case "typescript", "javascript", "vue":
		match := tsReturnAnnotationRe.FindStringSubmatch(signature)
		if len(match) < 2 {
			return nil
		}
		typ := strings.TrimSpace(match[1])
		typ = strings.TrimSuffix(typ, "=>")
		typ = strings.TrimSpace(typ)
		if typ == "" {
			return nil
		}
		return []string{typ}
	default:
		return nil
	}
}

package treesitter

import "bytes"

// csharpBlankableDirectives are C# preprocessor directive lines that carry no
// code-graph meaning and are blanked (to same-length spaces) before parsing.
// #region/#endregion are cosmetic folding markers; #pragma/#nullable/#line/
// #warning/#error are compiler hints; the conditional family (#if/#elif/#else/
// #endif/#define/#undef) is blanked line-only, keeping every branch body so
// symbols in all branches are indexed. tree-sitter-c-sharp mis-recovers when
// these interleave (a #region license header with #pragma directives) or when a
// conditional directive splits an expression (`.Select(x)` `#if` `.ToArray()`
// `#endif`), turning valid files "partial". Blanking is line-level and length-preserving, so downstream
// signature/docstring extraction reads the original, unmodified source.
var csharpBlankableDirectives = [][]byte{
	[]byte("#region"), []byte("#endregion"), []byte("#pragma"),
	[]byte("#if"), []byte("#elif"), []byte("#else"), []byte("#endif"),
	[]byte("#define"), []byte("#undef"), []byte("#nullable"),
	[]byte("#line"), []byte("#warning"), []byte("#error"),
}

// stripCSharpNonSemanticDirectives returns a same-length copy with the
// directive lines in csharpBlankableDirectives blanked to spaces.
func stripCSharpNonSemanticDirectives(content []byte) []byte {
	if bytes.IndexByte(content, '#') < 0 {
		return content
	}
	out := append([]byte(nil), content...)
	for start := 0; start < len(out); {
		end := bytes.IndexByte(out[start:], '\n')
		if end < 0 {
			end = len(out)
		} else {
			end += start
		}
		line := bytes.TrimSpace(out[start:end])
		if csharpNonSemanticDirectiveLine(line) {
			for i := start; i < end; i++ {
				if out[i] != '\r' {
					out[i] = ' '
				}
			}
		}
		if end == len(out) {
			break
		}
		start = end + 1
	}
	return out
}

func csharpNonSemanticDirectiveLine(line []byte) bool {
	for _, directive := range csharpBlankableDirectives {
		if !bytes.HasPrefix(line, directive) {
			continue
		}
		return len(line) == len(directive) || line[len(directive)] == ' ' || line[len(directive)] == '\t'
	}
	return false
}

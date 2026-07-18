package treesitter

import "bytes"

// stripSwiftConditionalDirectives returns a same-length copy with only
// conditional-compilation directive lines blanked. The Swift grammar can
// parse the declarations inside each branch but reports ERROR nodes for the
// surrounding #if/#elseif/#else/#endif tokens. Preserving every byte and
// newline keeps symbol/call offsets aligned with the original source.
func stripSwiftConditionalDirectives(content []byte) []byte {
	out := append([]byte(nil), content...)
	for start := 0; start < len(out); {
		end := bytes.IndexByte(out[start:], '\n')
		if end < 0 {
			end = len(out)
		} else {
			end += start
		}
		line := bytes.TrimSpace(out[start:end])
		if swiftConditionalDirectiveLine(line) {
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

func swiftConditionalDirectiveLine(line []byte) bool {
	for _, directive := range [][]byte{
		[]byte("#if"), []byte("#elseif"), []byte("#else"), []byte("#endif"),
	} {
		if !bytes.HasPrefix(line, directive) {
			continue
		}
		return len(line) == len(directive) || line[len(directive)] == ' ' || line[len(directive)] == '\t'
	}
	return false
}

package mamari

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// coverageData holds line-hit counts per repo-relative file, parsed from an
// lcov report. It makes the review flow's "untested" verdict authoritative:
// static call-graph closure can only say "no test path exists", which
// overstates coverage when jest/vitest mocks or data-dependent branches sever
// a statically-present path. Coverage says what actually executed under the
// test suite.
type coverageData struct {
	byFile map[string]map[int]int // relfile -> line -> hit count
}

// symbolTested reports whether a symbol spanning [start,end] executed under
// the test suite. known is false when coverage cannot speak to it (the file
// was not in the report, or the symbol's range carries no instrumented
// lines), so the caller falls back to static reachability.
func (c *coverageData) symbolTested(file string, start, end int) (tested, known bool) {
	if c == nil {
		return false, false
	}
	lines, ok := c.byFile[file]
	if !ok {
		return false, false
	}
	if end < start {
		end = start
	}
	sawInstrumented := false
	for line, hits := range lines {
		if line < start || line > end {
			continue
		}
		sawInstrumented = true
		if hits > 0 {
			return true, true
		}
	}
	if !sawInstrumented {
		return false, false // no instrumented lines in range → unknown
	}
	return false, true // instrumented but nothing ran → genuinely untested
}

// loadCoverage reads and parses an lcov report at path, normalizing each
// record's source path to a repo-relative file present in indexedFiles.
func loadCoverage(path, repoRoot string, indexedFiles map[string]bool) (*coverageData, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseLCOV(data, repoRoot, indexedFiles), nil
}

// parseLCOV parses lcov `SF:`/`DA:` records. Each `SF:<path>` opens a record;
// `DA:<line>,<hits>` adds a line hit count; `end_of_record` closes it. Source
// paths are normalized to the repo-relative form the index uses via
// normalizeCoverageFile.
func parseLCOV(data []byte, repoRoot string, indexedFiles map[string]bool) *coverageData {
	cov := &coverageData{byFile: map[string]map[int]int{}}
	// Precompute a basename/suffix index of indexed files for fuzzy matching.
	suffixIndex := buildCoverageSuffixIndex(indexedFiles)
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var cur string
	var curLines map[int]int
	flush := func() {
		if cur != "" && len(curLines) > 0 {
			if existing, ok := cov.byFile[cur]; ok {
				for l, h := range curLines {
					existing[l] += h
				}
			} else {
				cov.byFile[cur] = curLines
			}
		}
		cur, curLines = "", nil
	}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "SF:"):
			flush()
			raw := strings.TrimSpace(line[3:])
			cur = normalizeCoverageFile(raw, repoRoot, indexedFiles, suffixIndex)
			curLines = map[int]int{}
		case strings.HasPrefix(line, "DA:"):
			if curLines == nil {
				continue
			}
			rest := line[3:]
			comma := strings.IndexByte(rest, ',')
			if comma < 0 {
				continue
			}
			ln, err1 := strconv.Atoi(strings.TrimSpace(rest[:comma]))
			hits, err2 := strconv.Atoi(strings.TrimSpace(rest[comma+1:]))
			if err1 != nil || err2 != nil {
				continue
			}
			curLines[ln] += hits
		case line == "end_of_record":
			flush()
		}
	}
	flush()
	return cov
}

// normalizeCoverageFile maps an lcov `SF:` path to the repo-relative path the
// index uses. lcov paths are variously absolute, repo-relative, or relative to
// a sub-package (a monorepo runs coverage per app), so it tries, in order:
// exact repo-relative, absolute-under-root, then a unique suffix match against
// indexed files. Returns "" when unresolvable (that record is then ignored).
func normalizeCoverageFile(raw, repoRoot string, indexedFiles map[string]bool, suffixIndex map[string][]string) string {
	slash := filepath.ToSlash(raw)
	// Absolute path under the repo root.
	if repoRoot != "" {
		if rel, err := filepath.Rel(repoRoot, raw); err == nil {
			rel = filepath.ToSlash(rel)
			if !strings.HasPrefix(rel, "../") && indexedFiles[rel] {
				return rel
			}
		}
	}
	// Already repo-relative.
	if indexedFiles[slash] {
		return slash
	}
	clean := filepath.ToSlash(filepath.Clean(strings.TrimPrefix(slash, "./")))
	if indexedFiles[clean] {
		return clean
	}
	// Unique suffix match (handles per-package coverage whose SF paths are
	// relative to the package dir, e.g. `services/x.js` for `backend/services/x.js`).
	base := filepath.Base(clean)
	if cands, ok := suffixIndex[base]; ok {
		var match string
		hits := 0
		for _, cand := range cands {
			if cand == clean || strings.HasSuffix(cand, "/"+clean) {
				match = cand
				hits++
			}
		}
		if hits == 1 {
			return match
		}
		// No sole-basename fallback: attributing an lcov record to the only
		// indexed file sharing its basename — ignoring the directory path —
		// silently maps coverage onto an unrelated file (e.g. one repo's
		// utils/index.js onto another package's index.js), corrupting the
		// untested verdict. The unique-suffix match above already resolves
		// every legitimate absolute/relative/per-package form; anything past
		// it is ambiguous and left unresolved (the record is simply ignored).
	}
	return ""
}

func buildCoverageSuffixIndex(indexedFiles map[string]bool) map[string][]string {
	out := map[string][]string{}
	for f := range indexedFiles {
		base := filepath.Base(f)
		out[base] = append(out[base], f)
	}
	return out
}

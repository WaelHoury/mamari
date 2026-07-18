package mamari

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

const cgpBenchmarkSchemaVersion = "cgp-bench-v1"

// BenchmarkCGP runs the symbol-level recall harness against a gold fixture.
// The fixture is read from goldPath; the index is queried in-memory only —
// no source files are touched. The result is suitable for CI gating: an
// agent or pipeline can fail the build if AppRecall drops below 1.0 on any
// symbol that previously hit it.
func BenchmarkCGP(idx *Index, goldPath string) (CGPBenchmarkReport, error) {
	report := CGPBenchmarkReport{
		SchemaVersion: cgpBenchmarkSchemaVersion,
		GoldPath:      goldPath,
		Results:       map[string]CGPBenchmarkSymbolStat{},
	}
	idx.mu.Lock()
	report.Repo = idx.Repo.Root
	idx.mu.Unlock()

	data, err := os.ReadFile(goldPath)
	if err != nil {
		return report, err
	}
	var gold CGPBenchmarkGold
	if err := json.Unmarshal(data, &gold); err != nil {
		return report, fmt.Errorf("parse %s: %w", goldPath, err)
	}

	snap := idx.snapshot()
	callerIndex := buildReverseCallIndexSnapshot(snap)

	for name, spec := range gold.Symbols {
		stat := scoreSymbol(idx, snap, callerIndex, name, spec)
		report.Results[name] = stat
		report.Summary.Symbols++
		switch stat.Status {
		case "not_found":
			report.Summary.NotFound++
		case "ambiguous":
			report.Summary.Ambiguous++
		}
		report.Summary.AppExpected += stat.AppExpected
		report.Summary.AppFound += stat.AppFound
		report.Summary.TestExpected += stat.TestExpected
		report.Summary.TestFound += stat.TestFound
		report.Summary.Violations += len(stat.Violations)
	}
	report.Summary.AppRecall = ratio(report.Summary.AppFound, report.Summary.AppExpected)
	report.Summary.TestRecall = ratio(report.Summary.TestFound, report.Summary.TestExpected)
	report.Summary.OverallStatus = overallStatus(report.Summary)
	return report, nil
}

func scoreSymbol(idx *Index, snap indexSnapshot, callerIndex map[string][]CGPEdge, name string, spec CGPBenchmarkGoldSpec) CGPBenchmarkSymbolStat {
	stat := CGPBenchmarkSymbolStat{
		Status:       "ok",
		AppExpected:  len(spec.AppCallers),
		TestExpected: len(spec.TestCallers),
	}
	matches := pickByName(snap.Symbols, name, spec.FileHint)
	if len(matches) == 0 {
		stat.Status = "not_found"
		stat.AppRecall = ratio(0, stat.AppExpected)
		stat.TestRecall = ratio(0, stat.TestExpected)
		return stat
	}
	if len(matches) > 1 && spec.FileHint == "" {
		stat.Status = "ambiguous"
	}
	target := matches[0]
	stat.SymbolID = target.ID
	stat.File = target.File
	stat.StartLine = target.StartLine

	observed := observedCallerLines(callerIndex[target.ID])
	stat.ObservedCallers = observed.sortedSlice()

	stat.AppFound, stat.AppMissing = countHits(observed, spec.AppCallers)
	stat.TestFound, stat.TestMissing = countHits(observed, spec.TestCallers)
	for _, line := range spec.MustNotFind {
		if observed.has(line) {
			stat.Violations = append(stat.Violations, line)
		}
	}
	stat.AppRecall = ratio(stat.AppFound, stat.AppExpected)
	stat.TestRecall = ratio(stat.TestFound, stat.TestExpected)
	return stat
}

// pickByName returns symbols whose Name == name, optionally filtered to a
// specific file hint. Function symbols win over file symbols.
func pickByName(symbols map[string]CGPSymbol, name, fileHint string) []CGPSymbol {
	var out []CGPSymbol
	for _, sym := range symbols {
		if sym.Name != name {
			continue
		}
		if sym.Kind == "file" {
			continue
		}
		if fileHint != "" && sym.File != fileHint {
			continue
		}
		out = append(out, sym)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].StartLine < out[j].StartLine
	})
	return out
}

// observedCallerLines collects the unique "file:line" strings of every
// caller edge pointing at the target. We dedup because a single caller
// often invokes the target on multiple lines, and the gold fixture is
// normally per-callsite anyway.
func observedCallerLines(edges []CGPEdge) lineSet {
	out := newLineSet()
	for _, e := range edges {
		out.add(fmt.Sprintf("%s:%d", e.Evidence.File, e.Evidence.StartLine))
	}
	return out
}

func countHits(observed lineSet, expected []string) (int, []string) {
	if len(expected) == 0 {
		return 0, nil
	}
	missing := make([]string, 0)
	hit := 0
	for _, line := range expected {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if observed.has(line) {
			hit++
		} else {
			missing = append(missing, line)
		}
	}
	sort.Strings(missing)
	return hit, missing
}

func ratio(found, expected int) float64 {
	if expected <= 0 {
		return 1.0
	}
	return float64(found) / float64(expected)
}

func overallStatus(s CGPBenchmarkSummary) string {
	switch {
	case s.NotFound > 0 || s.Violations > 0 || s.AppRecall < 1.0:
		return "fail"
	case s.Ambiguous > 0 || s.TestRecall < 1.0:
		return "warn"
	default:
		return "pass"
	}
}

type lineSet struct {
	m map[string]struct{}
}

func newLineSet() lineSet { return lineSet{m: map[string]struct{}{}} }
func (s lineSet) add(v string) {
	if v != "" {
		s.m[v] = struct{}{}
	}
}
func (s lineSet) has(v string) bool {
	_, ok := s.m[v]
	return ok
}
func (s lineSet) sortedSlice() []string {
	out := make([]string, 0, len(s.m))
	for k := range s.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

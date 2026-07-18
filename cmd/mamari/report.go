package main

import (
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/waelhoury/mamari/internal/mamari"
)

// runReport prints the repo "report card": an honest, call-graph-grounded
// quality snapshot (parse health, edge confidence, dead code, test
// reachability, duplication, hotspots). With -fail-on it doubles as a CI
// gate: the command exits non-zero when any threshold is exceeded.
func runReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	topN := fs.Int("top", 10, "entries per hotspot list")
	failOn := fs.String("fail-on", "", `CI thresholds, comma-separated "metric<=value" (metrics: dead, dead-uncertain, untested-pct, unresolved-pct, dup-clusters, parse-failures, hot-paths); exits non-zero when exceeded`)
	args = normalizeFlags(args, map[string]bool{"--index": true, "--top": true, "--fail-on": true}, map[string]bool{"--json": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	resp := mamari.Report(idx, mamari.ReportOptions{TopN: *topN})

	violations, err := mamari.EvaluateReportGates(resp, *failOn)
	if err != nil {
		return err
	}

	if *asJSON {
		if err := printJSON(struct {
			mamari.ReportResponse
			GateViolations []mamari.ReportGateViolation `json:"gateViolations,omitempty"`
		}{resp, violations}); err != nil {
			return err
		}
	} else {
		printReportText(resp)
		for _, v := range violations {
			fmt.Printf("GATE FAILED: %s = %.1f exceeds %.1f\n", v.Metric, v.Actual, v.Threshold)
		}
	}
	if len(violations) > 0 {
		return fmt.Errorf("%d report gate(s) failed", len(violations))
	}
	return nil
}

func printReportText(r mamari.ReportResponse) {
	fmt.Printf("mamari report — %s (status: %s)\n", r.Repo.Root, r.Status)
	fmt.Printf("  files: %d (%s)   symbols: %d   edges: %d\n", r.Files, topLanguages(r.FilesByLang, 4), r.Symbols, r.Edges)
	fmt.Printf("  graph confidence: %s   unresolved: %.1f%%   parse failures: %d\n",
		confidenceSummary(r.EdgesByConfidence), r.UnresolvedPct, r.ParseFailures)
	fmt.Printf("  dead code: %d unreferenced (+%d uncertain, held back honestly)\n", r.DeadCode, r.DeadCodeUncertain)
	fmt.Printf("  test reachability: %d/%d symbols have no static test path (%.1f%%)\n", r.UntestedSymbols, r.TestableSymbols, r.UntestedPct)
	fmt.Printf("  duplication: %d structural clone cluster(s)\n", r.DuplicationClusters)
	fmt.Printf("  hot paths: %d symbol(s) with deep loops or scans-in-loop\n", r.HotPathSymbols)

	printHotspots := func(title string, items []mamari.ReportHotspot) {
		if len(items) == 0 {
			return
		}
		fmt.Printf("\n  %s:\n", title)
		for _, h := range items {
			if h.StartLine > 0 {
				fmt.Printf("    %-6d %s (%s:%d) — %s\n", h.Value, h.Name, h.File, h.StartLine, h.Detail)
			} else {
				fmt.Printf("    %-6d %s — %s\n", h.Value, h.Name, h.Detail)
			}
		}
	}
	printHotspots("complexity hotspots", r.ComplexityHotspots)
	printHotspots("biggest blast radius", r.BlastRadiusTop)
	printHotspots("god files", r.GodFiles)

	if len(r.TopDuplication) > 0 {
		fmt.Printf("\n  top duplication clusters:\n")
		for _, c := range r.TopDuplication {
			fmt.Printf("    [%d copies, ~%d lines] %s\n", c.Count, c.Lines, strings.Join(c.Members, " | "))
		}
	}
	for _, w := range r.Warnings {
		fmt.Printf("  note: %s\n", w)
	}
}

func topLanguages(byLang map[string]int, n int) string {
	type kv struct {
		lang  string
		count int
	}
	var all []kv
	for l, c := range byLang {
		all = append(all, kv{l, c})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].count != all[j].count {
			return all[i].count > all[j].count
		}
		return all[i].lang < all[j].lang
	})
	var parts []string
	for i, e := range all {
		if i >= n {
			parts = append(parts, "…")
			break
		}
		parts = append(parts, fmt.Sprintf("%s %d", e.lang, e.count))
	}
	return strings.Join(parts, ", ")
}

func confidenceSummary(byConf map[string]int) string {
	order := []string{"exact", "scoped", "heuristic", "unresolved"}
	var parts []string
	for _, k := range order {
		if v, ok := byConf[k]; ok && v > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", k, v))
		}
	}
	return strings.Join(parts, " / ")
}

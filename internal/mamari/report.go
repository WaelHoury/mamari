package mamari

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

const (
	reportTopN            = 10
	reportMaxGateExprs    = 20
	reportDupTopClusters  = 5
	reportDupMaxMembers   = 4
	reportHotPathLoopMin  = 3
	reportHighComplexity  = 15
	reportGodFileSymbols  = 40
	reportBlastConfidence = 2 // exact + scoped count as proven inbound
)

// ReportOptions configures Report.
type ReportOptions struct {
	// TopN bounds each hotspot list (complexity, blast radius, god files).
	// 0 = default (10).
	TopN int
}

// ReportHotspot is one entry in a ranked hotspot list.
type ReportHotspot struct {
	Name      string `json:"name"`
	Kind      string `json:"kind,omitempty"`
	File      string `json:"file"`
	StartLine int    `json:"startLine,omitempty"`
	// Value is the metric the list is ranked by (complexity score, proven
	// inbound callers, or symbol count for god files).
	Value int `json:"value"`
	// Detail carries the human-readable qualifier ("complexity 28, hot-path",
	// "31 proven callers", "182 symbols").
	Detail string `json:"detail,omitempty"`
}

// ReportDuplicationCluster is a compact clone-cluster summary.
type ReportDuplicationCluster struct {
	Count   int      `json:"count"`
	Lines   int      `json:"lines"`
	Members []string `json:"members"` // "file:line name", capped
}

// ReportResponse is the full repo report card. Every number is derived from
// the same honest call graph the query tools use: dead code excludes symbols
// an unresolved call might reach, untested is a static-reachability claim
// (marked as such), and the unresolved percentage is reported rather than
// hidden — the report inherits mamari's confidence discipline instead of
// presenting guesses as facts.
type ReportResponse struct {
	Status string   `json:"status"` // ok | warn | error (parse health, mirrors doctor)
	Repo   RepoInfo `json:"repo"`

	// Inventory.
	Files       int            `json:"files"`
	FilesByLang map[string]int `json:"filesByLanguage,omitempty"`
	Symbols     int            `json:"symbols"`
	Edges       int            `json:"edges"`

	// Honesty / graph confidence.
	EdgesByConfidence map[string]int `json:"edgesByConfidence"`
	UnresolvedPct     float64        `json:"unresolvedPct"`
	ParseFailures     int            `json:"parseFailures"`

	// Dead code (unreferenced; uncertain = held back by unresolved calls).
	DeadCode          int `json:"deadCode"`
	DeadCodeUncertain int `json:"deadCodeUncertain"`

	// Test reachability (static call-graph claim, not runtime coverage).
	TestableSymbols int     `json:"testableSymbols"`
	UntestedSymbols int     `json:"untestedSymbols"`
	UntestedPct     float64 `json:"untestedPct"`

	// Duplication.
	DuplicationClusters int                        `json:"duplicationClusters"`
	TopDuplication      []ReportDuplicationCluster `json:"topDuplication,omitempty"`

	// Risk hotspots.
	HotPathSymbols     int             `json:"hotPathSymbols"` // transitive loop depth >= 3 or scan-in-loop
	ComplexityHotspots []ReportHotspot `json:"complexityHotspots,omitempty"`
	BlastRadiusTop     []ReportHotspot `json:"blastRadiusTop,omitempty"`
	GodFiles           []ReportHotspot `json:"godFiles,omitempty"`

	Warnings []string `json:"warnings,omitempty"`
}

// Report aggregates the signals mamari already computes (parse health, edge
// confidence, dead code, test reachability, duplication, complexity/hot-path,
// blast radius) into one repo "report card" — a first-run x-ray and a CI
// gate (see EvaluateReportGates). It is a read-only aggregation: no new
// analysis, no index mutation.
func Report(idx *Index, opts ReportOptions) ReportResponse {
	topN := opts.TopN
	if topN <= 0 {
		topN = reportTopN
	}
	// Bound the response: a huge `top` over MCP would otherwise emit an
	// unbounded hotspot list (any symbol with >=1 proven caller qualifies for
	// blast radius) into the model's context.
	if topN > 200 {
		topN = 200
	}

	snap := idx.snapshot()
	resp := ReportResponse{
		Status:            "ok",
		Repo:              snap.Repo,
		Files:             len(snap.Files),
		FilesByLang:       map[string]int{},
		Symbols:           len(snap.Symbols),
		Edges:             snap.edgeCount(),
		EdgesByConfidence: map[string]int{},
	}

	parsePartial, parseError := 0, 0
	for _, f := range snap.Files {
		if f.Language != "" {
			resp.FilesByLang[f.Language]++
		}
		switch f.ParseStatus {
		case ParseStatusPartial:
			parsePartial++
		case ParseStatusError:
			parseError++
		}
	}
	resp.ParseFailures = parsePartial + parseError
	if parseError > 0 {
		resp.Status = "error"
	} else if parsePartial > 0 {
		resp.Status = "warn"
	}

	snap.forEachEdge(func(_ int, e CGPEdge) bool {
		resp.EdgesByConfidence[e.Confidence]++
		return true
	})
	if resp.Edges > 0 {
		resp.UnresolvedPct = round1(float64(resp.EdgesByConfidence[ConfUnresolved]) / float64(resp.Edges) * 100)
	}

	// Dead code + duplication + untested reuse the shipped flows (identical
	// semantics to the CLI/MCP tools, so the report never disagrees with them).
	dead := DeadCode(idx, DeadCodeOptions{})
	resp.DeadCode = dead.Total
	resp.DeadCodeUncertain = dead.UncertainSkipped

	untested := UntestedSymbols(idx, UntestedSymbolsOptions{})
	resp.UntestedSymbols = untested.Total
	listOpts := ListSymbolsOptions{SourceOnly: true}
	for _, sym := range snap.Symbols {
		if defaultUntestedKinds[sym.Kind] && !shouldExcludeNoisyFile(sym.File, listOpts) {
			resp.TestableSymbols++
		}
	}
	if resp.TestableSymbols > 0 {
		resp.UntestedPct = round1(float64(resp.UntestedSymbols) / float64(resp.TestableSymbols) * 100)
	}

	dup := Duplication(idx, DuplicationOptions{})
	resp.DuplicationClusters = dup.TotalClusters
	for i, cluster := range dup.Clusters {
		if i >= reportDupTopClusters {
			break
		}
		entry := ReportDuplicationCluster{Count: cluster.Count, Lines: cluster.Lines}
		for j, m := range cluster.Members {
			if j >= reportDupMaxMembers {
				entry.Members = append(entry.Members, fmt.Sprintf("… %d more", cluster.Count-reportDupMaxMembers))
				break
			}
			entry.Members = append(entry.Members, fmt.Sprintf("%s:%d %s", m.File, m.StartLine, m.Name))
		}
		resp.TopDuplication = append(resp.TopDuplication, entry)
	}

	// Hotspots from the snapshot itself.
	var complexitySyms []CGPSymbol
	for _, sym := range snap.Symbols {
		if !isComplexityKind(sym.Kind) || shouldExcludeNoisyFile(sym.File, listOpts) {
			continue
		}
		if sym.TransitiveLoopDepth >= reportHotPathLoopMin || sym.LinearScanInLoop > 0 {
			resp.HotPathSymbols++
		}
		if sym.Complexity >= reportHighComplexity {
			complexitySyms = append(complexitySyms, sym)
		}
	}
	sort.Slice(complexitySyms, func(i, j int) bool {
		if complexitySyms[i].Complexity != complexitySyms[j].Complexity {
			return complexitySyms[i].Complexity > complexitySyms[j].Complexity
		}
		if complexitySyms[i].File != complexitySyms[j].File {
			return complexitySyms[i].File < complexitySyms[j].File
		}
		return complexitySyms[i].StartLine < complexitySyms[j].StartLine
	})
	for i, sym := range complexitySyms {
		if i >= topN {
			break
		}
		detail := fmt.Sprintf("complexity %d", sym.Complexity)
		if sym.TransitiveLoopDepth >= reportHotPathLoopMin {
			detail += ", hot-path"
		}
		if sym.LinearScanInLoop > 0 {
			detail += ", scan-in-loop"
		}
		resp.ComplexityHotspots = append(resp.ComplexityHotspots, ReportHotspot{
			Name: sym.Name, Kind: sym.Kind, File: sym.File, StartLine: sym.StartLine,
			Value: sym.Complexity, Detail: detail,
		})
	}

	// Blast radius: DISTINCT proven (exact/scoped) callers per symbol —
	// deduplicated by caller symbol, self-edges and file-kind module-scope
	// callers excluded, matching reviewCallers' semantics so "N proven
	// callers" here never disagrees with the review flow. (Counting raw edge
	// instances overstated callers >3× on symbols called many times from the
	// same function.)
	inboundCallers := map[string]map[string]bool{}
	snap.forEachEdge(func(_ int, e CGPEdge) bool {
		if e.Type != "calls" {
			return true
		}
		if e.Confidence != ConfExact && e.Confidence != ConfScoped {
			return true
		}
		if e.From == e.To {
			return true
		}
		if caller, ok := snap.Symbols[e.From]; !ok || caller.Kind == "file" {
			return true
		}
		if inboundCallers[e.To] == nil {
			inboundCallers[e.To] = map[string]bool{}
		}
		inboundCallers[e.To][e.From] = true
		return true
	})
	type blastEntry struct {
		sym   CGPSymbol
		count int
	}
	var blast []blastEntry
	for id, callers := range inboundCallers {
		sym, ok := snap.Symbols[id]
		if !ok || !isComplexityKind(sym.Kind) || shouldExcludeNoisyFile(sym.File, listOpts) {
			continue
		}
		blast = append(blast, blastEntry{sym: sym, count: len(callers)})
	}
	sort.Slice(blast, func(i, j int) bool {
		if blast[i].count != blast[j].count {
			return blast[i].count > blast[j].count
		}
		if blast[i].sym.File != blast[j].sym.File {
			return blast[i].sym.File < blast[j].sym.File
		}
		return blast[i].sym.StartLine < blast[j].sym.StartLine
	})
	for i, b := range blast {
		if i >= topN {
			break
		}
		resp.BlastRadiusTop = append(resp.BlastRadiusTop, ReportHotspot{
			Name: b.sym.Name, Kind: b.sym.Kind, File: b.sym.File, StartLine: b.sym.StartLine,
			Value: b.count, Detail: fmt.Sprintf("%d proven callers", b.count),
		})
	}

	// God files: most symbols per file.
	perFile := map[string]int{}
	for _, sym := range snap.Symbols {
		if sym.Kind == "file" || shouldExcludeNoisyFile(sym.File, listOpts) {
			continue
		}
		perFile[sym.File]++
	}
	type fileEntry struct {
		file  string
		count int
	}
	var god []fileEntry
	for f, n := range perFile {
		if n >= reportGodFileSymbols {
			god = append(god, fileEntry{file: f, count: n})
		}
	}
	sort.Slice(god, func(i, j int) bool {
		if god[i].count != god[j].count {
			return god[i].count > god[j].count
		}
		return god[i].file < god[j].file
	})
	for i, g := range god {
		if i >= topN {
			break
		}
		resp.GodFiles = append(resp.GodFiles, ReportHotspot{
			Name: g.file, File: g.file, Value: g.count,
			Detail: fmt.Sprintf("%d symbols", g.count),
		})
	}

	resp.Warnings = append(resp.Warnings, dead.Warnings...)
	resp.Warnings = append(resp.Warnings, untested.Warnings...)
	resp.Warnings = append(resp.Warnings,
		"untested = no static test-call path reaches the symbol; dynamic dispatch and black-box tests are not seen (pass a coverage report to `review` for runtime truth)")
	return resp
}

// ReportGateViolation is one failed CI threshold.
type ReportGateViolation struct {
	Metric    string  `json:"metric"`
	Actual    float64 `json:"actual"`
	Threshold float64 `json:"threshold"`
}

// reportGateMetrics maps a gate name to the report value it checks. Names are
// deliberately short/stable — they are CLI surface (`-fail-on "dead<=150"`).
func reportGateMetrics(resp ReportResponse) map[string]float64 {
	return map[string]float64{
		"dead":           float64(resp.DeadCode),
		"dead-uncertain": float64(resp.DeadCodeUncertain),
		"untested-pct":   resp.UntestedPct,
		"unresolved-pct": resp.UnresolvedPct,
		"dup-clusters":   float64(resp.DuplicationClusters),
		"parse-failures": float64(resp.ParseFailures),
		"hot-paths":      float64(resp.HotPathSymbols),
	}
}

// EvaluateReportGates parses a comma-separated gate expression
// ("dead<=150,untested-pct<=80,dup-clusters<=100" — `metric<=value`; a bare
// `metric=value` means the same) against the report and returns the
// violations. An unknown metric or malformed expression is an error, not a
// silent pass — a CI gate that cannot be evaluated must fail loudly.
func EvaluateReportGates(resp ReportResponse, expr string) ([]ReportGateViolation, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, nil
	}
	metrics := reportGateMetrics(resp)
	parts := strings.Split(expr, ",")
	if len(parts) > reportMaxGateExprs {
		return nil, fmt.Errorf("too many gate expressions (%d > %d)", len(parts), reportMaxGateExprs)
	}
	var violations []ReportGateViolation
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var name, valueRaw string
		switch {
		case strings.Contains(part, "<="):
			bits := strings.SplitN(part, "<=", 2)
			name, valueRaw = bits[0], bits[1]
		case strings.Contains(part, "="):
			bits := strings.SplitN(part, "=", 2)
			name, valueRaw = bits[0], bits[1]
		default:
			return nil, fmt.Errorf("gate %q: expected metric<=value", part)
		}
		name = strings.TrimSpace(name)
		actual, ok := metrics[name]
		if !ok {
			known := make([]string, 0, len(metrics))
			for k := range metrics {
				known = append(known, k)
			}
			sort.Strings(known)
			return nil, fmt.Errorf("gate %q: unknown metric %q (known: %s)", part, name, strings.Join(known, ", "))
		}
		threshold, err := strconv.ParseFloat(strings.TrimSpace(valueRaw), 64)
		if err != nil {
			return nil, fmt.Errorf("gate %q: bad threshold: %v", part, err)
		}
		// ParseFloat accepts "NaN"/"Inf"; `actual > NaN` is always false, so a
		// typo'd NaN would silently disable the gate — a CI gate that cannot
		// be evaluated must fail loudly instead.
		if math.IsNaN(threshold) || math.IsInf(threshold, 0) {
			return nil, fmt.Errorf("gate %q: threshold must be a finite number", part)
		}
		if actual > threshold {
			violations = append(violations, ReportGateViolation{Metric: name, Actual: actual, Threshold: threshold})
		}
	}
	return violations, nil
}

func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}

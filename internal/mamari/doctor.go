package mamari

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Doctor produces an index-health report. It never mutates the index. Stale
// detection compares the current git HEAD against the commit recorded at
// build time; if either is empty the check is skipped.
func Doctor(idx *Index) DoctorReport {
	report := DoctorReport{
		Status:        "ok",
		SchemaVersion: SchemaVersion,
	}
	snap := idx.snapshot()
	report.RepoRoot = snap.Repo.Root
	report.IndexedAt = snap.Repo.IndexedAt
	report.IndexedCommit = snap.Repo.GitCommit

	if snap.Repo.Root != "" {
		if current := gitCommit(snap.Repo.Root); current != "" {
			report.CurrentCommit = current
			if snap.Repo.GitCommit != "" && current != snap.Repo.GitCommit {
				report.Stale = true
				report.Warnings = append(report.Warnings,
					"index commit "+snap.Repo.GitCommit+" differs from current HEAD "+current)
			}
		}
	}
	if snap.Repo.IndexedAt != "" {
		if t, err := time.Parse(time.RFC3339, snap.Repo.IndexedAt); err == nil {
			age := time.Since(t)
			report.IndexAgeHours = age.Hours()
			if age > 24*time.Hour {
				report.Warnings = append(report.Warnings,
					"index is older than 24 hours")
			}
		}
	}

	report.Files = summarizeFiles(snap.Files)
	report.Symbols = summarizeSymbolsForDoctor(snap.Symbols)
	report.Edges, report.Unresolved, report.TopUnresolved = summarizeEdgesForDoctorSnapshot(snap)
	report.DynamicIRIs = len(snap.DynamicIRICalls)
	report.ParseFailures = collectParseFailures(snap.Files)
	report.ParseFailureTotal = len(report.ParseFailures)
	report.IgnorePatterns = activeIgnorePatterns(snap.Repo.Root)

	report.FilesChangedSinceIndex, report.FilesDeletedSinceIndex = detectContentStaleness(snap.Repo.Root, snap.Files)
	if len(report.FilesChangedSinceIndex) > 0 || len(report.FilesDeletedSinceIndex) > 0 {
		report.Warnings = append(report.Warnings,
			fmt.Sprintf("%d file(s) changed and %d file(s) deleted on disk since the index was built",
				len(report.FilesChangedSinceIndex), len(report.FilesDeletedSinceIndex)))
	}

	if report.Files.ByStatus[ParseStatusError] > 0 {
		report.Status = "error"
	} else if report.Stale || report.Files.ByStatus[ParseStatusPartial] > 0 || len(report.Warnings) > 0 {
		if report.Status != "error" {
			report.Status = "warn"
		}
	}
	return report
}

// LimitDoctorParseFailures bounds the verbose per-file examples while retaining
// the complete count. A non-positive limit preserves the full report.
func LimitDoctorParseFailures(report DoctorReport, limit int) DoctorReport {
	if report.ParseFailureTotal == 0 {
		report.ParseFailureTotal = len(report.ParseFailures)
	}
	if limit <= 0 || len(report.ParseFailures) <= limit {
		return report
	}
	report.ParseFailures = append([]DoctorParseFailure(nil), report.ParseFailures[:limit]...)
	report.ParseFailuresTruncated = true
	return report
}

// detectContentStaleness compares each indexed file's recorded SHA256
// against its current on-disk content. It catches uncommitted edits and
// deletions that the git-commit-based Stale check misses. Results are
// capped to keep doctor output bounded on large repos with many edits.
func detectContentStaleness(root string, files map[string]File) (changed, deleted []string) {
	if root == "" {
		return nil, nil
	}
	const maxReported = 50
	for path, f := range files {
		if f.SHA256 == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			if os.IsNotExist(err) {
				deleted = append(deleted, path)
			}
			continue
		}
		if hash(data) != f.SHA256 {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	sort.Strings(deleted)
	if len(changed) > maxReported {
		changed = changed[:maxReported]
	}
	if len(deleted) > maxReported {
		deleted = deleted[:maxReported]
	}
	return changed, deleted
}

func summarizeFiles(files map[string]File) DoctorFileSummary {
	out := DoctorFileSummary{
		Total:      len(files),
		ByLanguage: map[string]int{},
		ByStatus:   map[string]int{},
	}
	for _, f := range files {
		if f.Language != "" {
			out.ByLanguage[f.Language]++
		}
		status := f.ParseStatus
		if status == "" {
			status = ParseStatusOK
		}
		out.ByStatus[status]++
	}
	return out
}

func summarizeSymbolsForDoctor(symbols map[string]CGPSymbol) DoctorSymbolSummary {
	out := DoctorSymbolSummary{
		Total:        len(symbols),
		ByKind:       map[string]int{},
		ByConfidence: map[string]int{},
	}
	for _, s := range symbols {
		out.ByKind[s.Kind]++
		out.ByConfidence[s.Confidence]++
	}
	return out
}

func summarizeEdgesForDoctorSnapshot(snap indexSnapshot) (DoctorEdgeSummary, DoctorUnresolved, []DoctorUnresolvedItem) {
	return summarizeEdgesForDoctorEach(snap.edgeCount(), func(visit func(CGPEdge)) {
		snap.forEachEdge(func(_ int, edge CGPEdge) bool {
			visit(edge)
			return true
		})
	})
}

func summarizeEdgesForDoctorEach(total int, each func(func(CGPEdge))) (DoctorEdgeSummary, DoctorUnresolved, []DoctorUnresolvedItem) {
	edgeSummary := DoctorEdgeSummary{
		Total:        total,
		ByType:       map[string]int{},
		ByConfidence: map[string]int{},
	}
	unresolved := DoctorUnresolved{ByReason: map[string]int{}}
	type unkey struct {
		target string
		reason string
	}
	counts := map[unkey]int{}
	each(func(e CGPEdge) {
		edgeSummary.ByType[e.Type]++
		edgeSummary.ByConfidence[e.Confidence]++
		if e.Confidence == ConfUnresolved || strings.HasPrefix(e.To, "unresolved:") {
			unresolved.Total++
			if e.UnresolvedReason != "" {
				unresolved.ByReason[e.UnresolvedReason]++
			} else {
				unresolved.ByReason["unspecified"]++
			}
			counts[unkey{target: e.To, reason: e.UnresolvedReason}]++
		}
	})
	unresolved.UnknownTo = len(counts)
	top := make([]DoctorUnresolvedItem, 0, len(counts))
	for k, c := range counts {
		top = append(top, DoctorUnresolvedItem{Target: k.target, Reason: k.reason, Count: c})
	}
	sort.Slice(top, func(i, j int) bool {
		if top[i].Count != top[j].Count {
			return top[i].Count > top[j].Count
		}
		return top[i].Target < top[j].Target
	})
	if len(top) > 20 {
		top = top[:20]
	}
	return edgeSummary, unresolved, top
}

func collectParseFailures(files map[string]File) []DoctorParseFailure {
	var out []DoctorParseFailure
	for _, f := range files {
		if f.ParseStatus == "" || f.ParseStatus == ParseStatusOK {
			continue
		}
		out = append(out, DoctorParseFailure{
			File:   f.Path,
			Parser: f.Parser,
			Status: f.ParseStatus,
			Error:  f.ParseError,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out
}

// activeIgnorePatterns reproduces the ignore set used by WalkRepo, so doctor
// can show agents which paths the index intentionally skipped. We re-read
// from disk rather than caching to stay honest if the repo's ignore files
// changed since the index was built.
func activeIgnorePatterns(root string) []string {
	if root == "" {
		return nil
	}
	var patterns []string
	patterns = append(patterns, builtInIgnores...)
	for _, pattern := range readIgnoreFile(root + "/.gitignore") {
		patterns = append(patterns, pattern.value)
	}
	for _, pattern := range readIgnoreFile(root + "/.mamariignore") {
		patterns = append(patterns, pattern.value)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

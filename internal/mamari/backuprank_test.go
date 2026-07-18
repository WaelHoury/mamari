package mamari

import (
	"testing"
)

// Backup/dead copies once led ambiguous candidate lists in
// find-references/trace, so the caller's first look landed on a stale
// snapshot. Active source must sort first; the backup stays listed
// (it exists and may be the target of an explicit query), just last.

func buildBackupFixture(t *testing.T) *Index {
	t.Helper()
	root := t.TempDir()
	write(t, root, "backend/date_utils_backup_07-10-2024.js", `function convertDate(d) { return d }
module.exports = { convertDate }
`)
	write(t, root, "backend/utilities/dateUtils.js", `function convertDate(d) { return new Date(d) }
module.exports = { convertDate }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func TestAmbiguousTraceCandidatesRankBackupFilesLast(t *testing.T) {
	idx := buildBackupFixture(t)
	trace := TraceSymbol(idx, "convertDate")
	if trace.Status != "ambiguous" {
		t.Fatalf("expected ambiguous, got %s", trace.Status)
	}
	if len(trace.Candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %#v", trace.Candidates)
	}
	if isBackupOrDeadFile(trace.Candidates[0].File) {
		t.Fatalf("backup file ranked first among candidates: %#v", trace.Candidates)
	}
	if !isBackupOrDeadFile(trace.Candidates[1].File) {
		t.Fatalf("backup candidate should remain listed last, got %#v", trace.Candidates)
	}
}

func TestFindReferencesCandidatesRankBackupFilesLast(t *testing.T) {
	idx := buildBackupFixture(t)
	resp := FindReferences(idx, "convertDate")
	if resp.Status != "ambiguous" {
		t.Fatalf("expected ambiguous, got %s", resp.Status)
	}
	if len(resp.SymbolCandidates) < 2 {
		t.Fatalf("expected candidates, got %#v", resp.SymbolCandidates)
	}
	if isBackupOrDeadFile(resp.SymbolCandidates[0].File) {
		t.Fatalf("backup file ranked first among reference candidates: %#v", resp.SymbolCandidates)
	}
}

func TestBackupFileNameDetection(t *testing.T) {
	cases := map[string]bool{
		"backend/date_utils_backup_07-10-2024.js": true,
		"utils.js.bak":                   true,
		"App.tsx.orig":                   true,
		"config.old.json":                true,
		"notes.txt~":                     true,
		"backend/utilities/dateUtils.js": false,
		"src/backupService.js":           false, // live code ABOUT backups
		"src/oldham.go":                  false, // 'old' inside a word
	}
	for file, want := range cases {
		if got := isBackupOrDeadFile(file); got != want {
			t.Errorf("isBackupOrDeadFile(%q) = %v, want %v", file, got, want)
		}
	}
}

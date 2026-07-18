package mamari

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
)

const (
	// maxCoChangeCommits bounds how much git history is scanned, keeping
	// `mamari index` fast on repos with very long histories.
	maxCoChangeCommits = 1000
	// maxCoChangeFilesPerCommit skips commits that touch an unusually large
	// number of files (mass renames, vendor/lockfile bumps, etc.) — without
	// this, a single huge commit would dominate every file's co-change list
	// with noise.
	maxCoChangeFilesPerCommit = 50
	// maxCoChangeEntriesPerFile bounds how many co-changed files are kept per
	// file, both in the cached index and in API responses.
	maxCoChangeEntriesPerFile = 10
)

// coChangeFileSchema is the on-disk schema of .mamari/cochange.json.
type coChangeFileSchema struct {
	SchemaVersion int                        `json:"schemaVersion"`
	Commit        string                     `json:"commit"`
	Commits       int                        `json:"commits"`
	CoChanges     map[string][]CoChangeEntry `json:"coChanges"`
}

const coChangeSchemaVersion = 1

func coChangePath(root string) string {
	return filepath.Join(root, ".mamari", "cochange.json")
}

// buildCoChangeGraph runs `git log --name-only` over up to
// maxCoChangeCommits commits and counts, for each pair of files touched in
// the same commit, how often that pairing occurred. Commits touching more
// than maxCoChangeFilesPerCommit files are skipped as noise. Returns a nil
// map (not an error) if root is not a git repository or git is unavailable —
// co-change is an optional enrichment, never a hard dependency.
func buildCoChangeGraph(root string) map[string][]CoChangeEntry {
	cmd := exec.Command("git", "log", "--name-only", "--no-renames", "--pretty=format:%x00", "-n", strconv.Itoa(maxCoChangeCommits))
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	counts := map[string]map[string]int{}
	var current []string
	flush := func() {
		if len(current) == 0 || len(current) > maxCoChangeFilesPerCommit {
			current = current[:0]
			return
		}
		for _, a := range current {
			for _, b := range current {
				if a == b {
					continue
				}
				if counts[a] == nil {
					counts[a] = map[string]int{}
				}
				counts[a][b]++
			}
		}
		current = current[:0]
	}

	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "\x00" || line == "" {
			flush()
			continue
		}
		current = append(current, filepath.ToSlash(line))
	}
	flush()

	if len(counts) == 0 {
		return map[string][]CoChangeEntry{}
	}

	result := make(map[string][]CoChangeEntry, len(counts))
	for file, peers := range counts {
		entries := make([]CoChangeEntry, 0, len(peers))
		for peer, n := range peers {
			entries = append(entries, CoChangeEntry{File: peer, Count: n})
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].Count != entries[j].Count {
				return entries[i].Count > entries[j].Count
			}
			return entries[i].File < entries[j].File
		})
		if len(entries) > maxCoChangeEntriesPerFile {
			entries = entries[:maxCoChangeEntriesPerFile]
		}
		result[file] = entries
	}
	return result
}

// loadCoChangeCache reads .mamari/cochange.json if it is fresh (matches the
// current git HEAD). Returns nil, false on any miss (missing file, schema
// mismatch, stale commit, not a git repo).
func loadCoChangeCache(root string) (map[string][]CoChangeEntry, bool) {
	head := gitCommit(root)
	if head == "" {
		return nil, false
	}
	data, err := os.ReadFile(coChangePath(root))
	if err != nil {
		return nil, false
	}
	var cached coChangeFileSchema
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil, false
	}
	if cached.SchemaVersion != coChangeSchemaVersion || cached.Commit != head {
		return nil, false
	}
	return cached.CoChanges, true
}

// saveCoChangeCache writes .mamari/cochange.json atomically (temp file +
// rename), tagged with the git commit it was computed at.
func saveCoChangeCache(root string, coChanges map[string][]CoChangeEntry) error {
	dir := filepath.Join(root, ".mamari")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	payload := coChangeFileSchema{
		SchemaVersion: coChangeSchemaVersion,
		Commit:        gitCommit(root),
		Commits:       maxCoChangeCommits,
		CoChanges:     coChanges,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "cochange-*.json.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, coChangePath(root))
}

// ensureCoChangeGraph lazily loads (or builds and caches) the co-change
// graph for idx.Repo.Root. Safe to call repeatedly and from multiple
// goroutines; the result is cached in-process for the lifetime of idx.
func (idx *Index) ensureCoChangeGraph() map[string][]CoChangeEntry {
	idx.mu.Lock()
	if idx.coChangeLoaded {
		out := idx.coChange
		idx.mu.Unlock()
		return out
	}
	idx.mu.Unlock()

	root := idx.Repo.Root
	var graph map[string][]CoChangeEntry
	if cached, ok := loadCoChangeCache(root); ok {
		graph = cached
	} else {
		graph = buildCoChangeGraph(root)
		if graph != nil {
			_ = saveCoChangeCache(root, graph)
		}
	}

	idx.mu.Lock()
	if !idx.coChangeLoaded {
		idx.coChange = graph
		idx.coChangeLoaded = true
	}
	out := idx.coChange
	idx.mu.Unlock()
	return out
}

// CoChangedFiles returns up to limit files historically changed in the same
// git commit as file, ranked by co-change frequency. Returns an empty slice
// if the repo has no git history or file has no recorded co-changes.
func (idx *Index) CoChangedFiles(file string, limit int) []CoChangeEntry {
	if limit <= 0 {
		limit = maxCoChangeEntriesPerFile
	}
	graph := idx.ensureCoChangeGraph()
	entries := graph[file]
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries
}

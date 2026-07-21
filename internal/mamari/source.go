package mamari

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func FetchSource(idx *Index, file string, startLine, endLine int) (FetchSourceResponse, error) {
	if startLine < 1 || endLine < startLine {
		return FetchSourceResponse{}, fmt.Errorf("invalid line range")
	}
	rel, err := cleanRepoRelativePath(file)
	if err != nil {
		return FetchSourceResponse{}, err
	}
	idx.mu.Lock()
	fileInfo, ok := idx.Files[rel]
	root := idx.Repo.Root
	idx.mu.Unlock()
	if !ok {
		return FetchSourceResponse{}, fmt.Errorf("file is not indexed: %s", rel)
	}
	data, err := readRepoFile(root, rel)
	if err != nil {
		return FetchSourceResponse{}, err
	}
	lines := strings.SplitAfter(string(data), "\n")
	if endLine > fileInfo.LineCount {
		return FetchSourceResponse{}, fmt.Errorf("line range exceeds file length")
	}
	return FetchSourceResponse{
		Status:    "ok",
		File:      rel,
		StartLine: startLine,
		EndLine:   endLine,
		Text:      strings.Join(lines[startLine-1:endLine], ""),
	}, nil
}

func cleanRepoRelativePath(file string) (string, error) {
	rel := filepath.Clean(filepath.FromSlash(file))
	if rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("file must be inside the indexed repo")
	}
	return filepath.ToSlash(rel), nil
}

// resolvedRepoFilePath follows symlinks and verifies that the final target is
// still beneath root. Repositories are often opened before they are trusted;
// a tracked `secret.ts -> ../../outside` symlink must not let an MCP source or
// search request read arbitrary files from the user's machine.
func resolvedRepoFilePath(root, file string) (string, error) {
	rel, err := cleanRepoRelativePath(file)
	if err != nil {
		return "", err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	realPath, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	within, err := filepath.Rel(realRoot, realPath)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) || filepath.IsAbs(within) {
		return "", fmt.Errorf("file must be inside the indexed repo")
	}
	return realPath, nil
}

func readRepoFile(root, file string) ([]byte, error) {
	path, err := resolvedRepoFilePath(root, file)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func lineStarts(content string) []int {
	starts := []int{0}
	for i, r := range content {
		if r == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

func offsetToLineCol(starts []int, offset int) (int, int) {
	line := 0
	for line+1 < len(starts) && starts[line+1] <= offset {
		line++
	}
	return line + 1, offset - starts[line] + 1
}

func rawLine(lines []string, line int) string {
	if line < 1 || line > len(lines) {
		return ""
	}
	return strings.TrimRight(lines[line-1], "\r\n")
}

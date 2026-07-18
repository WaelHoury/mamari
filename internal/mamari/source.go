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
	rel := filepath.ToSlash(filepath.Clean(file))
	if strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return FetchSourceResponse{}, fmt.Errorf("file must be inside the indexed repo")
	}
	idx.mu.Lock()
	fileInfo, ok := idx.Files[rel]
	root := idx.Repo.Root
	idx.mu.Unlock()
	if !ok {
		return FetchSourceResponse{}, fmt.Errorf("file is not indexed: %s", rel)
	}
	data, err := os.ReadFile(filepath.Join(root, rel))
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

package mamari

import (
	"bufio"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const largeGeneratedArtifactBytes int64 = 512 * 1024

var builtInIgnores = []string{
	".git",
	".mamari",
	"node_modules",
	"vendor",
	"third_party",
	"external",
	"dist",
	"build",
	"coverage",
	".next",
	".nuxt",
	".svelte-kit",
	".turbo",
	".vite",
	".gradle",
	".dart_tool",
	".terraform",
	".build",
	"Pods",
	"DerivedData",
	"obj",
	"storybook-static",
	"out",
	"target",
	".venv",
	"venv",
	"__pycache__",
	".env",
	"*.pem",
	"*.key",
}

type ignorePattern struct {
	value   string
	builtIn bool
}

func WalkRepo(root string) ([]string, error) {
	ignores := make([]ignorePattern, 0, len(builtInIgnores))
	for _, pattern := range builtInIgnores {
		ignores = append(ignores, ignorePattern{value: pattern, builtIn: true})
	}
	ignores = append(ignores, readIgnoreFile(filepath.Join(root, ".gitignore"))...)
	ignores = append(ignores, readIgnoreFile(filepath.Join(root, ".mamariignore"))...)
	tracked := trackedGitFiles(root)
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if ignored(rel, d.IsDir(), ignores, tracked) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			if _, err := resolvedRepoFilePath(root, rel); err != nil {
				// Never index a symlink whose final target leaves the repo.
				return nil
			}
		}
		if (isIndexableSourceFile(rel) || isShellShebangFile(path)) && !shouldSkipLargeGeneratedArtifact(rel, d) {
			files = append(files, rel)
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

func isShellShebangFile(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	line, err := bufio.NewReaderSize(file, 512).ReadString('\n')
	if err != nil && len(line) == 0 {
		return false
	}
	return shellLanguageFromShebang([]byte(line)) == "bash"
}

func isIndexableSourceFile(rel string) bool {
	switch languageFor(rel) {
	case "ttl", "typescript", "javascript", "vue", "python", "java", "json", "yaml",
		"go", "rust", "csharp", "ruby", "php", "c", "cpp", "kotlin", "bash", "scala", "lua", "elixir", "dart", "haskell", "clojure", "swift",
		"r", "julia", "zig", "ocaml", "hcl", "dockerfile":
		return true
	default:
		return false
	}
}

func shouldSkipLargeGeneratedArtifact(rel string, d fs.DirEntry) bool {
	if !looksLikeGeneratedArtifactPath(rel) && !isGeneratedTreeSitterParserHeader(rel) {
		return false
	}
	info, err := d.Info()
	if err != nil {
		return false
	}
	return shouldSkipLargeGeneratedArtifactInfo(rel, info)
}

func shouldSkipLargeGeneratedArtifactInfo(rel string, info fs.FileInfo) bool {
	if info == nil || info.IsDir() {
		return false
	}
	if isGeneratedTreeSitterParserHeader(rel) {
		return true
	}
	if !looksLikeGeneratedArtifactPath(rel) {
		return false
	}
	return info.Size() >= largeGeneratedArtifactBytes
}

func isGeneratedTreeSitterParserHeader(rel string) bool {
	rel = filepath.ToSlash(strings.ToLower(rel))
	return strings.HasSuffix(rel, "/tree_sitter/parser.h") &&
		(strings.Contains(rel, "tree-sitter") || strings.Contains(rel, "treesitter") || strings.Contains(rel, "grammar"))
}

func looksLikeGeneratedArtifactPath(rel string) bool {
	rel = filepath.ToSlash(strings.ToLower(rel))
	base := filepath.Base(rel)
	switch {
	case base == "parser.c" && (strings.Contains(rel, "tree-sitter") || strings.Contains(rel, "treesitter") || strings.Contains(rel, "/grammar") || strings.Contains(rel, "grammar/")):
		return true
	case strings.HasSuffix(base, ".min.js") || strings.HasSuffix(base, ".bundle.js"):
		return true
	case base == "package-lock.json" || base == "composer.lock":
		return true
	case strings.Contains(base, ".generated.") || strings.Contains(base, "_generated.") || strings.Contains(base, ".g."):
		return true
	case strings.Contains(rel, "/generated/") || strings.Contains(rel, "/gen/") || strings.Contains(rel, "/target/generated-sources/"):
		return true
	default:
		return false
	}
}

func readIgnoreFile(path string) []ignorePattern {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	var patterns []ignorePattern
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		patterns = append(patterns, ignorePattern{value: line})
	}
	return patterns
}

func ignored(rel string, isDir bool, patterns []ignorePattern, tracked map[string]bool) bool {
	if tracked[rel] {
		return false
	}
	base := filepath.Base(rel)
	for _, entry := range patterns {
		pattern := strings.TrimSpace(filepath.ToSlash(entry.value))
		pattern = strings.TrimPrefix(pattern, "/")
		if pattern == "" {
			continue
		}
		dirOnly := strings.HasSuffix(pattern, "/")
		pattern = strings.TrimSuffix(pattern, "/")
		if dirOnly && !isDir {
			continue
		}
		hasSlash := strings.Contains(pattern, "/")
		hasGlob := strings.ContainsAny(pattern, "*?[")
		if pattern == rel {
			return true
		}
		if pattern == base && (!isDir || entry.builtIn || dirOnly) {
			return true
		}
		if strings.HasPrefix(rel, pattern+"/") && (entry.builtIn || dirOnly || hasSlash) {
			return true
		}
		if ok, _ := filepath.Match(pattern, base); ok {
			if !isDir || entry.builtIn || dirOnly || hasGlob {
				return true
			}
		}
		if hasSlash {
			if ok, _ := filepath.Match(pattern, rel); ok {
				return true
			}
		}
	}
	return false
}

func trackedGitFiles(root string) map[string]bool {
	cmd := exec.Command("git", "-C", root, "ls-files", "-z")
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return nil
	}
	tracked := map[string]bool{}
	for _, item := range strings.Split(string(out), "\x00") {
		if item == "" {
			continue
		}
		tracked[filepath.ToSlash(item)] = true
	}
	return tracked
}

package mamari

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// aliasRule maps an import-path prefix to one or more repo-relative base
// directories, scoped to the project rooted at dir (the directory holding the
// tsconfig/jsconfig/vite config the rule came from). A `@/components/Button`
// import in a file under dir resolves against each base in order.
//
// Aliases are how modern JS/TS projects avoid deep `../../..` relative
// imports; a monorepo can have a different alias target per app, so rules are
// scoped to each config's directory and only apply to importers beneath it.
// Without alias resolution, every
// `@/…` / `~/…` / custom-prefix import silently fails to resolve, losing all
// cross-file edges (and route-component liveness) for those imports.
type aliasRule struct {
	dir    string   // repo-relative directory the rule is scoped to (config location)
	prefix string   // e.g. "@/", "@components/", or "@" for an exact bare-name alias
	bases  []string // repo-relative base directories to substitute for the prefix
}

var (
	tsBaseURLRe = regexp.MustCompile(`"baseUrl"\s*:\s*"([^"]*)"`)
	tsPathsRe   = regexp.MustCompile(`"paths"\s*:\s*\{`)
	tsPathEntry = regexp.MustCompile(`"([^"]+)"\s*:\s*\[\s*("[^"]+"(?:\s*,\s*"[^"]+")*)\s*\]`)
	// vite/webpack alias entries: `'@': <expr with a quoted rel path>`. We
	// take the alias key (quoted or a bareword like @ / ~) and the first
	// quoted relative path found in its value expression.
	viteAliasBlockRe = regexp.MustCompile(`(?s)\balias\s*:\s*\{(.*?)\}`)
	viteAliasEntryRe = regexp.MustCompile(`(?m)['"]?(@[A-Za-z0-9_/-]*|~[A-Za-z0-9_/-]*)['"]?\s*:\s*([^,\n}]+)`)
	viteRelPathRe    = regexp.MustCompile(`['"](\.\.?/[^'"]*|\./[^'"]*|[^'":]*/src)['"]`)
)

// detectAliasRules scans tsconfig.json / jsconfig.json (compilerOptions
// baseUrl + paths) and vite/vue config alias blocks across the repo, building
// the alias-resolution rule set. Called once during BuildIndex, before the
// relations phase that resolves imports. Config files are read from the
// already-loaded contents map, so this adds no extra IO.
func detectAliasRules(idx *Index, contents map[string]string) {
	var rules []aliasRule
	for rel, content := range contents {
		base := filepath.Base(rel)
		dir := filepath.ToSlash(filepath.Dir(rel))
		if dir == "." {
			dir = ""
		}
		switch {
		case base == "tsconfig.json" || base == "jsconfig.json":
			rules = append(rules, tsconfigAliasRules(dir, content)...)
		case strings.HasPrefix(base, "vite.config.") || strings.HasPrefix(base, "vue.config.") || base == "nuxt.config.ts" || base == "nuxt.config.js":
			rules = append(rules, viteAliasRules(dir, content)...)
		}
	}
	// Longest prefix first so `@components/` wins over `@` for the same
	// importer, and deeper config dirs win over shallow ones.
	sort.SliceStable(rules, func(i, j int) bool {
		if len(rules[i].dir) != len(rules[j].dir) {
			return len(rules[i].dir) > len(rules[j].dir)
		}
		return len(rules[i].prefix) > len(rules[j].prefix)
	})
	idx.mu.Lock()
	idx.aliasRules = rules
	idx.mu.Unlock()
}

func tsconfigAliasRules(dir, content string) []aliasRule {
	baseURL := ""
	if m := tsBaseURLRe.FindStringSubmatch(content); m != nil {
		baseURL = m[1]
	}
	loc := tsPathsRe.FindStringIndex(content)
	if loc == nil {
		return nil
	}
	// Scan the paths object body (from the opening brace to its match).
	body := content[loc[1]:]
	if end := matchBraceEnd(body); end >= 0 {
		body = body[:end]
	}
	var rules []aliasRule
	for _, m := range tsPathEntry.FindAllStringSubmatch(body, -1) {
		key := m[1]
		var bases []string
		for _, tgt := range splitQuotedList(m[2]) {
			bases = append(bases, joinAliasBase(dir, baseURL, tgt))
		}
		if len(bases) == 0 {
			continue
		}
		rules = append(rules, aliasRule{dir: dir, prefix: normalizeAliasPrefix(key), bases: bases})
	}
	return rules
}

func viteAliasRules(dir, content string) []aliasRule {
	block := viteAliasBlockRe.FindStringSubmatch(content)
	if block == nil {
		return nil
	}
	var rules []aliasRule
	for _, m := range viteAliasEntryRe.FindAllStringSubmatch(block[1], -1) {
		key := m[1]
		rel := viteRelPathRe.FindStringSubmatch(m[2])
		if rel == nil {
			continue
		}
		// Vite/webpack alias matching is prefix replacement: `@` maps `@/foo`
		// to `<base>/foo` (not just the exact module `@`). Store a
		// slash-terminated prefix so `@/components/X` matches, unless the key
		// already carries an explicit trailing slash.
		prefix := key
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		rules = append(rules, aliasRule{
			dir:    dir,
			prefix: prefix,
			bases:  []string{joinAliasBase(dir, "", rel[1])},
		})
	}
	return rules
}

// normalizeAliasPrefix strips the trailing `/*` (or `*`) from a tsconfig key
// and normalizes to either a slash-terminated prefix (`@/`, `@components/`)
// or a bare exact alias (`@`).
func normalizeAliasPrefix(key string) string {
	key = strings.TrimSuffix(key, "*")
	if key == "" {
		return ""
	}
	return key
}

// joinAliasBase resolves a tsconfig/vite target path against the config's
// directory and baseUrl, and strips a trailing `/*` wildcard, yielding a
// repo-relative base directory.
func joinAliasBase(dir, baseURL, target string) string {
	target = strings.TrimSuffix(strings.TrimSuffix(target, "*"), "/")
	parts := []string{}
	if dir != "" {
		parts = append(parts, dir)
	}
	if baseURL != "" && baseURL != "." {
		parts = append(parts, baseURL)
	}
	parts = append(parts, target)
	return filepath.ToSlash(filepath.Clean(strings.Join(parts, "/")))
}

// resolveAliasBases returns candidate repo-relative bases for an aliased spec
// imported from file, using the detected rules whose config directory is an
// ancestor of file. Longest-prefix and deepest-config rules win (rules are
// pre-sorted). Returns nil when no rule matches, so callers fall back to the
// structural `@`→src heuristic.
func (idx *Index) resolveAliasBases(file, spec string) []string {
	idx.mu.Lock()
	rules := idx.aliasRules
	idx.mu.Unlock()
	if len(rules) == 0 {
		return nil
	}
	file = filepath.ToSlash(file)
	var out []string
	for _, r := range rules {
		if r.dir != "" && !(file == r.dir || strings.HasPrefix(file, r.dir+"/")) {
			continue
		}
		rest, ok := matchAliasPrefix(spec, r.prefix)
		if !ok {
			continue
		}
		for _, b := range r.bases {
			joined := b
			if rest != "" {
				joined = filepath.ToSlash(filepath.Clean(b + "/" + rest))
			}
			out = append(out, joined)
		}
	}
	return out
}

// matchAliasPrefix reports whether spec is covered by prefix. A
// slash-terminated prefix (`@/`, `@components/`) matches by string prefix and
// returns the remainder; a bare prefix (`@`) matches only the exact spec (an
// exact-module alias) and returns "".
func matchAliasPrefix(spec, prefix string) (string, bool) {
	if strings.HasSuffix(prefix, "/") {
		if strings.HasPrefix(spec, prefix) {
			return strings.TrimPrefix(spec, prefix), true
		}
		return "", false
	}
	if spec == prefix {
		return "", true
	}
	return "", false
}

// matchBraceEnd returns the index just past the `}` matching an implied open
// brace at position -1 (i.e. body begins just inside the object). Returns -1
// if unbalanced.
func matchBraceEnd(body string) int {
	depth := 1
	inStr := byte(0)
	for i := 0; i < len(body); i++ {
		c := body[i]
		if inStr != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == inStr {
				inStr = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			inStr = c
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func splitQuotedList(s string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

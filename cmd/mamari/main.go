package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/waelhoury/mamari/internal/mamari"
	"github.com/waelhoury/mamari/internal/mcpserver"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	if err := run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "mamari:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 2 {
		return usage()
	}
	switch args[1] {
	case "init":
		return runInit(args[2:])
	case "index":
		return runIndex(args[2:])
	case "hooks":
		return runHooks(args[2:])
	case "status":
		return runStatus(args[2:])
	case "setup-mcp":
		return runSetupMCP(args[2:])
	case "install-skill":
		return runInstallSkill(args[2:])
	case "version":
		return runVersion(args[2:])
	case "query":
		return runQuery(args[2:])
	case "demo":
		return runDemo(args[2:])
	case "trace-term":
		return runTraceTerm(args[2:])
	case "inspect-term":
		return runInspectTerm(args[2:])
	case "find-references":
		return runFindReferences(args[2:])
	case "list-terms":
		return runListTerms(args[2:])
	case "fetch-source":
		return runFetchSource(args[2:])
	case "fetch-context":
		return runFetchContext(args[2:])
	case "list-dynamic-iris":
		return runListDynamicIRIs(args[2:])
	case "search-literal":
		return runSearchLiteral(args[2:])
	case "search-code":
		return runSearchCode(args[2:])
	case "semantic-query":
		return runSemanticQuery(args[2:])
	case "inspect-exact":
		return runInspectExact(args[2:])
	case "repo-map":
		return runRepoMap(args[2:])
	case "import-scip":
		return runImportSCIP(args[2:])
	case "find-containing-shape":
		return runFindContainingShape(args[2:])
	case "query-graph":
		return runQueryGraph(args[2:])
	case "list-symbols":
		return runListSymbols(args[2:])
	case "find-symbol":
		return runFindSymbol(args[2:])
	case "trace-symbol":
		return runTraceSymbol(args[2:])
	case "inspect-symbol":
		return runInspectSymbol(args[2:])
	case "inspect-flow":
		return runInspectFlow(args[2:])
	case "impact":
		return runImpact(args[2:])
	case "review":
		return runReview(args[2:])
	case "dead-code":
		return runDeadCode(args[2:])
	case "duplicates", "dupes":
		return runDuplicates(args[2:])
	case "report":
		return runReport(args[2:])
	case "doctor":
		return runDoctor(args[2:])
	case "benchmark-cgp":
		return runBenchmarkCGP(args[2:])
	case "serve":
		return runServe(args[2:])
	case "ui":
		return runUI(args[2:])
	case "watch":
		return runWatch(args[2:])
	default:
		return usage()
	}
}

func usage() error {
	return fmt.Errorf("usage: mamari <init|index|hooks|status|setup-mcp|install-skill|version|query|demo|trace-term|inspect-term|find-references|list-terms|list-symbols|find-symbol|trace-symbol|inspect-symbol|inspect-flow|impact|review|dead-code|duplicates|report|doctor|benchmark-cgp|fetch-source|fetch-context|list-dynamic-iris|search-literal|search-code|semantic-query|inspect-exact|repo-map|import-scip|find-containing-shape|query-graph|serve|ui|watch>")
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	repo := fs.String("repo", ".", "repository root to index")
	indexPath := fs.String("index", "", "where to write the index file (defaults to <repo>/.mamari/index.json)")
	mcpClient := fs.String("mcp", "", "index and configure an MCP client in one step: claude|codex|vscode|jetbrains|all")
	mcpCommand := fs.String("mcp-command", "", "mamari executable path in MCP config (defaults to the resolved executable)")
	mcpForce := fs.Bool("mcp-force", false, "replace an existing mamari MCP server entry")
	printMCP := fs.Bool("print-mcp", false, "print MCP configuration snippets after indexing")
	commitIndex := fs.Bool("commit-index", false, "also write a git-trackable copy to .mamari/committed/index.json and install a pre-commit hook that keeps it fresh, so `git pull` alone is enough to use Mamari against this repo (see `mamari hooks install`)")
	args = normalizeFlags(args, map[string]bool{"--repo": true, "--index": true, "--mcp": true, "--mcp-command": true}, map[string]bool{"--mcp-force": true, "--print-mcp": true, "--commit-index": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	var clients []string
	if strings.TrimSpace(*mcpClient) != "" {
		clients = expandMCPClients(*mcpClient)
		if len(clients) == 0 {
			return unknownMCPClientError(*mcpClient)
		}
	}
	idx, err := mamari.BuildIndex(*repo)
	if err != nil {
		return err
	}
	out := *indexPath
	if out == "" {
		out = defaultIndexPath(idx.Repo.Root)
	} else if !filepath.IsAbs(out) {
		out = filepath.Join(idx.Repo.Root, out)
	}
	out = filepath.Clean(out)

	var resolvedCommand string
	if len(clients) > 0 {
		resolvedCommand, err = resolveMCPConfigCommandForRepo(*mcpCommand, true, idx.Repo.Root)
		if err != nil {
			return err
		}
		if err := preflightMCPConfigWrites(idx.Repo.Root, writableMCPClients(clients), "mamari", *mcpForce); err != nil {
			return err
		}
		if err := validateMCPCommand(resolvedCommand); err != nil {
			return err
		}
	}
	if err := mamari.SaveIndex(idx, out); err != nil {
		return err
	}
	fmt.Printf("mamari initialized\n")
	fmt.Printf("repo: %s\n", idx.Repo.Root)
	fmt.Printf("index: %s\n", out)
	fmt.Printf("files: %d\n", len(idx.Files))
	fmt.Printf("symbols: %d\n", len(idx.Symbols))
	fmt.Printf("terms: %d\n", len(idx.Terms))
	if *commitIndex {
		if err := mamari.SaveIndexJSON(idx, mamari.CommittedIndexPath(idx.Repo.Root)); err != nil {
			return err
		}
		hookPath, err := mamari.InstallPreCommitHook(idx.Repo.Root)
		if err != nil {
			return err
		}
		fmt.Println()
		fmt.Printf("committed index: %s\n", mamari.CommittedIndexPath(idx.Repo.Root))
		fmt.Printf("pre-commit hook installed: %s\n", hookPath)
		fmt.Println("commit .mamari/committed/ and the updated .gitignore so teammates get the index on `git pull`")
	}
	if len(clients) > 0 {
		if err := validateMCPRuntime(resolvedCommand, out); err != nil {
			return fmt.Errorf("index saved at %s, but MCP validation failed: %w", out, err)
		}
		written, err := writeMCPConfigs(idx.Repo.Root, writableMCPClients(clients), "mamari", resolvedCommand, out, *mcpForce)
		if err != nil {
			return fmt.Errorf("index saved at %s, but MCP configuration failed: %w", out, err)
		}
		fmt.Println()
		for _, path := range written {
			fmt.Printf("mcp config: %s\n", path)
		}
		if containsMCPClient(clients, "jetbrains") {
			fmt.Println("JetBrains keeps MCP configuration in IDE-managed settings:")
			printMCPConfigSnippetsForClients([]string{"jetbrains"}, out, resolvedCommand, "mamari")
		}
		fmt.Println("mcp validation: ok")
		printMCPNextSteps(clients)
	} else {
		fmt.Printf("next: run `mamari setup-mcp --client codex --write` from %s\n", idx.Repo.Root)
	}
	if *printMCP {
		fmt.Println()
		fmt.Println("MCP snippets:")
		printMCPConfigSnippets(out, "mamari")
	}
	return nil
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	args = normalizeFlags(args, map[string]bool{"--index": true}, map[string]bool{"--json": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	report := mamari.Doctor(idx)
	if *asJSON {
		return printJSON(report)
	}
	fmt.Printf("status: %s\n", report.Status)
	fmt.Printf("schema: %d\n", report.SchemaVersion)
	fmt.Printf("repo: %s\n", report.RepoRoot)
	fmt.Printf("indexedAt: %s (%.1fh ago)\n", report.IndexedAt, report.IndexAgeHours)
	fmt.Printf("files: %d\n", report.Files.Total)
	fmt.Printf("symbols: %d\n", report.Symbols.Total)
	fmt.Printf("edges: %d\n", report.Edges.Total)
	fmt.Printf("unresolved: %d\n", report.Unresolved.Total)
	if report.Stale {
		fmt.Printf("stale: true (indexed=%s current=%s)\n", report.IndexedCommit, report.CurrentCommit)
	}
	for _, w := range report.Warnings {
		fmt.Printf("warning: %s\n", w)
	}
	return nil
}

func runVersion(args []string) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "emit JSON")
	args = normalizeFlags(args, nil, map[string]bool{"--json": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	resolvedVersion, resolvedCommit, resolvedDate := resolveVersionMetadata(version, commit, date, debug.ReadBuildInfo)
	info := map[string]any{
		"version":       resolvedVersion,
		"commit":        resolvedCommit,
		"date":          resolvedDate,
		"schemaVersion": mamari.SchemaVersion,
	}
	if *asJSON {
		return printJSON(info)
	}
	fmt.Printf("mamari %s\n", resolvedVersion)
	fmt.Printf("commit: %s\n", resolvedCommit)
	fmt.Printf("date: %s\n", resolvedDate)
	fmt.Printf("schemaVersion: %d\n", mamari.SchemaVersion)
	return nil
}

// resolveVersionMetadata preserves release linker flags while making binaries
// installed with `go install module/cmd@version` self-identifying. Those
// builds do not receive GoReleaser's -X flags, but Go records the selected
// module version (including pseudo-versions) in the executable's build info.
// Local VCS builds can additionally supply vcs.revision and vcs.time.
func resolveVersionMetadata(linkedVersion, linkedCommit, linkedDate string, readBuildInfo func() (*debug.BuildInfo, bool)) (string, string, string) {
	resolvedVersion, resolvedCommit, resolvedDate := linkedVersion, linkedCommit, linkedDate
	info, ok := readBuildInfo()
	if !ok || info == nil {
		return resolvedVersion, resolvedCommit, resolvedDate
	}
	if (resolvedVersion == "" || resolvedVersion == "dev") && info.Main.Version != "" && info.Main.Version != "(devel)" {
		resolvedVersion = info.Main.Version
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if resolvedCommit == "" || resolvedCommit == "unknown" {
				resolvedCommit = setting.Value
			}
		case "vcs.time":
			if resolvedDate == "" || resolvedDate == "unknown" {
				resolvedDate = setting.Value
			}
		}
	}
	return resolvedVersion, resolvedCommit, resolvedDate
}

func runSetupMCP(args []string) error {
	fs := flag.NewFlagSet("setup-mcp", flag.ContinueOnError)
	client := fs.String("client", "all", "client to configure: claude|codex|vscode|jetbrains|all")
	repo := fs.String("repo", ".", "project root where the MCP configuration is written")
	indexPath := fs.String("index", ".mamari/index.json", "index file path used by the MCP server")
	command := fs.String("command", "", "mamari executable path in MCP config (defaults to the resolved executable when writing)")
	name := fs.String("name", "mamari", "MCP server name")
	writeFiles := fs.Bool("write", false, "write project config files instead of printing snippets")
	force := fs.Bool("force", false, "overwrite an existing mamari server entry when writing")
	args = normalizeFlags(args, map[string]bool{"--client": true, "--repo": true, "--index": true, "--command": true, "--name": true}, map[string]bool{"--write": true, "--force": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	clients := expandMCPClients(*client)
	if len(clients) == 0 {
		return unknownMCPClientError(*client)
	}
	if *writeFiles && containsMCPClient(clients, "jetbrains") {
		return fmt.Errorf("JetBrains stores MCP configuration in IDE-managed settings; omit --write, then paste the printed JSON in Copilot Chat > MCP > Add MCP Tools")
	}
	guidedJetBrains := !*writeFiles && containsMCPClient(clients, "jetbrains")
	repoRoot, err := filepath.Abs(*repo)
	if err != nil {
		return fmt.Errorf("resolve project root: %w", err)
	}
	repoRoot = filepath.Clean(repoRoot)
	resolvedCommand, err := resolveMCPConfigCommandForRepo(*command, *writeFiles || guidedJetBrains, repoRoot)
	if err != nil {
		return err
	}
	resolvedIndex := *indexPath
	if *writeFiles || guidedJetBrains {
		if !filepath.IsAbs(resolvedIndex) {
			resolvedIndex = filepath.Join(repoRoot, resolvedIndex)
		}
		resolvedIndex = filepath.Clean(resolvedIndex)
	}
	if !*writeFiles {
		if guidedJetBrains {
			if err := validateMCPRuntime(resolvedCommand, resolvedIndex); err != nil {
				return err
			}
			fmt.Println("mcp validation: ok")
		}
		printMCPConfigSnippetsForClients(clients, resolvedIndex, resolvedCommand, *name)
		if guidedJetBrains {
			printMCPNextSteps(clients)
		}
		return nil
	}
	if err := validateMCPRuntime(resolvedCommand, resolvedIndex); err != nil {
		return err
	}
	if err := preflightMCPConfigWrites(repoRoot, clients, *name, *force); err != nil {
		return err
	}
	written, err := writeMCPConfigs(repoRoot, clients, *name, resolvedCommand, resolvedIndex, *force)
	if err != nil {
		return err
	}
	for _, path := range written {
		fmt.Printf("mcp config: %s\n", path)
	}
	fmt.Println("mcp validation: ok")
	printMCPNextSteps(clients)
	return nil
}

func runQuery(args []string) error {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	budget := fs.Int("budget", 900, "estimated token budget for fallback search")
	limit := fs.Int("limit", 6, "maximum exact clusters or search hits")
	args = normalizeFlags(args, map[string]bool{"--index": true, "--budget": true, "--limit": true}, map[string]bool{"--json": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("query requires one natural-language query string")
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	query := fs.Arg(0)
	exact := mamari.InspectExact(idx, query, mamari.InspectExactOptions{
		Limit:      *limit,
		SourceOnly: true,
	})
	if exact.Status == "ok" && len(exact.Clusters) > 0 {
		if *asJSON {
			return printJSON(map[string]any{
				"status":   "ok",
				"strategy": "inspect_exact",
				"exact":    exact,
			})
		}
		fmt.Printf("%s: ok strategy=inspect-exact estimated=%d clusters=%d\n", query, exact.EstimatedTokens, len(exact.Clusters))
		for _, cluster := range exact.Clusters {
			header := cluster.File
			if cluster.Symbol != "" {
				header += ":" + cluster.Symbol
			}
			if cluster.Route != "" {
				header += " route=" + cluster.Route
			}
			fmt.Printf("--- %s matched=%s estimated=%d ---\n", header, strings.Join(cluster.Matched, ","), cluster.EstimatedTokens)
			for _, line := range cluster.Lines {
				fmt.Printf("%d: %s\n", line.Line, line.Text)
			}
		}
		return nil
	}
	search := mamari.SearchCode(idx, query, mamari.SearchCodeOptions{
		Limit:        *limit,
		BudgetTokens: *budget,
		SourceOnly:   true,
		ExactFirst:   true,
		Mode:         mamari.ModeEvidence,
	})
	if *asJSON {
		return printJSON(map[string]any{
			"status":   search.Status,
			"strategy": "search_code",
			"search":   search,
		})
	}
	fmt.Printf("%s: %s strategy=search-code estimated=%d/%d hits=%d/%d truncated=%t\n", query, search.Status, search.EstimatedTokens, search.BudgetTokens, len(search.Hits), search.Total, search.Truncated)
	for _, hit := range search.Hits {
		fmt.Printf("--- %s:%d score=%d terms=%s exact=%s estimated=%d ---\n", hit.File, hit.FocusLine, hit.Score, strings.Join(hit.MatchedTerms, ","), strings.Join(hit.MatchedExact, ","), hit.EstimatedTokens)
		if hit.Text != "" {
			fmt.Println(hit.Text)
		}
	}
	return nil
}

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	keep := fs.Bool("keep", false, "keep the generated demo repository")
	args = normalizeFlags(args, nil, map[string]bool{"--keep": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := os.MkdirTemp("", "mamari-demo-*")
	if err != nil {
		return err
	}
	if !*keep {
		defer os.RemoveAll(root)
	}
	srcDir := filepath.Join(root, "src", "views")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		return err
	}
	demoVue := `<template>
  <div class="signature-thumb">
    <div class="signature-image-wrap">
      <img :src="signature.imageUrl" :alt="signature.name" />
    </div>
  </div>
</template>

<script setup>
const signature = { name: 'Signer', imageUrl: 'data:image/gif;base64,AAAA' }
</script>

<style scoped>
.signature-thumb {
  display: grid;
  grid-template-columns: 84px 1fr;
  min-height: 58px;
}

.signature-image-wrap {
  height: 42px;
}

.signature-image-wrap img {
  max-height: 38px;
  object-fit: contain;
}
</style>
`
	if err := os.WriteFile(filepath.Join(srcDir, "TrackingDrawer.vue"), []byte(demoVue), 0o644); err != nil {
		return err
	}
	idx, err := mamari.BuildIndex(root)
	if err != nil {
		return err
	}
	indexPath := defaultIndexPath(idx.Repo.Root)
	if err := mamari.SaveIndex(idx, indexPath); err != nil {
		return err
	}
	resp := mamari.InspectExact(idx, "signature-thumb signature-image-wrap", mamari.InspectExactOptions{
		Limit:      4,
		SourceOnly: true,
	})
	fmt.Printf("demo repo: %s\n", root)
	fmt.Printf("index: %s\n", indexPath)
	fmt.Printf("query: signature-thumb signature-image-wrap\n")
	fmt.Printf("result: %s estimated=%d clusters=%d\n", resp.Status, resp.EstimatedTokens, len(resp.Clusters))
	for _, cluster := range resp.Clusters {
		fmt.Printf("--- %s:%s ---\n", cluster.File, cluster.Symbol)
		for _, line := range cluster.Lines {
			fmt.Printf("%d: %s\n", line.Line, line.Text)
		}
	}
	if !*keep {
		fmt.Println("demo repo removed; rerun with --keep to inspect it")
	}
	return nil
}

func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	repo := fs.String("repo", ".", "repository root to watch")
	indexPath := fs.String("index", "", "where to write the index file (defaults to <repo>/.mamari/index.json)")
	debounceMs := fs.Int("debounce-ms", 200, "debounce window for filesystem events")
	persist := fs.Bool("persist", true, "rewrite the on-disk index after each rebake")
	args = normalizeFlags(args, map[string]bool{"--repo": true, "--index": true, "--debounce-ms": true}, map[string]bool{"--persist": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	idx, err := mamari.BuildIndex(*repo)
	if err != nil {
		return err
	}
	out := *indexPath
	if out == "" {
		out = filepath.Join(idx.Repo.Root, ".mamari", "index.json")
	}
	if *persist {
		if err := mamari.SaveIndex(idx, out); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "mamari watch: indexed %d files; watching %s\n", len(idx.Files), idx.Repo.Root)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	opts := mamari.WatchOptions{
		Debounce: time.Duration(*debounceMs) * time.Millisecond,
		OnRebake: func(updated, removed []string) {
			if len(updated)+len(removed) == 0 {
				return
			}
			fmt.Fprintf(os.Stderr, "mamari watch: rebake updated=%d removed=%d\n", len(updated), len(removed))
			if *persist {
				if err := mamari.SaveIndex(idx, out); err != nil {
					fmt.Fprintln(os.Stderr, "mamari watch: save error:", err)
				}
			}
		},
		OnError: func(err error) {
			fmt.Fprintln(os.Stderr, "mamari watch:", err)
		},
	}
	return mamari.Watch(ctx, idx, opts)
}

func runListSymbols(args []string) error {
	fs := flag.NewFlagSet("list-symbols", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	kind := fs.String("kind", "", "filter by symbol kind")
	lang := fs.String("lang", "", "filter by language")
	limit := fs.Int("limit", 0, "maximum symbols to return (0 means no limit)")
	kinds := fs.String("kinds", "", "comma-separated symbol kinds to include")
	sourceOnly := fs.Bool("source-only", false, "exclude tests and stories unless explicitly included")
	includeTests := fs.Bool("include-tests", false, "include test symbols when --source-only is set")
	includeStories := fs.Bool("include-stories", false, "include story symbols when --source-only is set")
	withScores := fs.Bool("scores", false, "include relevance scores in JSON output")
	args = normalizeFlags(args, map[string]bool{"--index": true, "--kind": true, "--lang": true, "--limit": true, "--kinds": true}, map[string]bool{"--json": true, "--source-only": true, "--include-tests": true, "--include-stories": true, "--scores": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	query := ""
	if fs.NArg() > 0 {
		query = fs.Arg(0)
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	resp := mamari.ListSymbolsWithOptions(idx, query, *kind, *lang, mamari.ListSymbolsOptions{
		Limit:          *limit,
		Kinds:          splitCSV(*kinds),
		SourceOnly:     *sourceOnly,
		IncludeTests:   *includeTests,
		IncludeStories: *includeStories,
		WithScores:     *withScores,
	})
	if *asJSON {
		return printJSON(resp)
	}
	for _, sym := range resp.Symbols {
		fmt.Printf("%s\t%s\t%s\t%s:%d\t%s\n", sym.Kind, sym.Language, sym.Name, sym.File, sym.StartLine, sym.ID)
	}
	return nil
}

func runFindSymbol(args []string) error {
	fs := flag.NewFlagSet("find-symbol", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	kind := fs.String("kind", "", "filter by symbol kind")
	lang := fs.String("lang", "", "filter by language")
	limit := fs.Int("limit", 10, "maximum symbols to return")
	kinds := fs.String("kinds", "", "comma-separated symbol kinds to include")
	sourceOnly := fs.Bool("source-only", false, "exclude tests and stories unless explicitly included")
	includeTests := fs.Bool("include-tests", false, "include test symbols when --source-only is set")
	includeStories := fs.Bool("include-stories", false, "include story symbols when --source-only is set")
	withScores := fs.Bool("scores", false, "include relevance scores in JSON output")
	args = normalizeFlags(args, map[string]bool{"--index": true, "--kind": true, "--lang": true, "--limit": true, "--kinds": true}, map[string]bool{"--json": true, "--source-only": true, "--include-tests": true, "--include-stories": true, "--scores": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("find-symbol requires one query")
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	resp := mamari.ListSymbolsWithOptions(idx, fs.Arg(0), *kind, *lang, mamari.ListSymbolsOptions{
		Limit:          *limit,
		Kinds:          splitCSV(*kinds),
		SourceOnly:     *sourceOnly,
		IncludeTests:   *includeTests,
		IncludeStories: *includeStories,
		WithScores:     *withScores,
	})
	if *asJSON {
		return printJSON(resp)
	}
	for _, sym := range resp.Symbols {
		fmt.Printf("%s\t%s\t%s\t%s:%d\t%s\n", sym.Kind, sym.Language, sym.Name, sym.File, sym.StartLine, sym.ID)
	}
	return nil
}

func runTraceSymbol(args []string) error {
	fs := flag.NewFlagSet("trace-symbol", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	withEdges := fs.Bool("with-edges", false, "include full raw CGP edge objects")
	sites := fs.Bool("sites", true, "include compact caller-site evidence (ignored when -compact is set)")
	includeTestDetails := fs.Bool("include-test-details", false, "return individual test callback callers instead of file groups")
	excludeTests := fs.Bool("exclude-tests", false, "omit test callers and test caller sites")
	compact := fs.Bool("compact", false, "drop signature/docstring/return-types/hot-path/id/language fields and caller-site evidence")
	args = normalizeFlags(args, map[string]bool{"--index": true}, map[string]bool{"--json": true, "--with-edges": true, "--sites": true, "--include-test-details": true, "--exclude-tests": true, "--compact": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("trace-symbol requires one symbol id or name")
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	resp := mamari.TraceSymbolWithOptions(idx, fs.Arg(0), mamari.TraceSymbolOptions{
		WithEdges:          *withEdges,
		Sites:              *sites,
		IncludeTestDetails: *includeTestDetails,
		ExcludeTests:       *excludeTests,
		Compact:            *compact,
	})
	if *asJSON {
		return printJSON(resp)
	}
	fmt.Printf("%s: %s\n", resp.Query, resp.Status)
	if resp.Symbol != nil {
		fmt.Printf("symbol: %s %s %s:%d\n", resp.Symbol.Kind, resp.Symbol.Name, resp.Symbol.File, resp.Symbol.StartLine)
	}
	for _, caller := range resp.Callers {
		if caller.Kind == "test-callback-group" {
			fmt.Printf("- caller test callbacks %s count=%d lines=%v\n", caller.File, caller.Count, caller.Lines)
			continue
		}
		fmt.Printf("- caller %s %s:%d\n", caller.Name, caller.File, caller.StartLine)
	}
	for _, callee := range resp.Callees {
		fmt.Printf("- callee %s %s:%d\n", callee.Name, callee.File, callee.StartLine)
	}
	return nil
}

func runInspectSymbol(args []string) error {
	fs := flag.NewFlagSet("inspect-symbol", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	budget := fs.Int("budget", 1800, "estimated token budget for source context")
	contextLines := fs.Int("context-lines", 3, "line context around caller sites")
	mode := fs.String("mode", "context", "context mode: compact|evidence|context|full")
	format := fs.String("format", "context", "response format: context|node")
	includeTests := fs.Bool("include-tests", false, "include test callers and caller sites")
	includeTestDetails := fs.Bool("include-test-details", false, "return individual test callback callers instead of grouped test files")
	withEdges := fs.Bool("with-edges", false, "include full raw CGP edge objects in the trace")
	args = normalizeFlags(args, map[string]bool{"--index": true, "--budget": true, "--context-lines": true, "--mode": true, "--format": true}, map[string]bool{"--json": true, "--include-tests": true, "--include-test-details": true, "--with-edges": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("inspect-symbol requires one symbol id or name")
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	if *format == "node" {
		resp, err := mamari.InspectSymbolNode(idx, fs.Arg(0), mamari.InspectSymbolNodeOptions{
			BudgetTokens:       *budget,
			IncludeTests:       *includeTests,
			IncludeTestDetails: *includeTestDetails,
		})
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(resp)
		}
		printInspectSymbolNode(resp)
		return nil
	}
	if *format != "context" {
		return fmt.Errorf("inspect-symbol --format must be context or node")
	}
	resp, err := mamari.InspectSymbol(idx, fs.Arg(0), mamari.InspectSymbolOptions{
		BudgetTokens:       *budget,
		ContextLines:       *contextLines,
		Mode:               *mode,
		IncludeTests:       *includeTests,
		IncludeTestDetails: *includeTestDetails,
		WithEdges:          *withEdges,
	})
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(resp)
	}
	fmt.Printf("%s: %s estimated=%d\n", resp.Query, resp.Status, resp.EstimatedTokens)
	if resp.Symbol != nil {
		fmt.Printf("symbol: %s %s %s:%d\n", resp.Symbol.Kind, resp.Symbol.Name, resp.Symbol.File, resp.Symbol.StartLine)
	}
	for _, site := range resp.Trace.CallerSites {
		fmt.Printf("- caller-site %s:%d:%d %s (%s)\n", site.File, site.Line, site.Column, site.Raw, site.Confidence)
	}
	for _, edge := range resp.Frontend {
		fmt.Printf("- frontend %s %s:%d %s\n", edge.Type, edge.Evidence.File, edge.Evidence.StartLine, edge.Evidence.Raw)
	}
	for _, slice := range resp.Context.Slices {
		fmt.Printf("--- %s:%d:%d [%s] %s estimated=%d ---\n", slice.File, slice.StartLine, slice.EndLine, slice.Kind, slice.Reason, slice.EstimatedTokens)
		fmt.Print(slice.Text)
		if !strings.HasSuffix(slice.Text, "\n") {
			fmt.Println()
		}
	}
	return nil
}

func runInspectFlow(args []string) error {
	fs := flag.NewFlagSet("inspect-flow", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	limit := fs.Int("limit", 6, "maximum discovery hits/symbols to inspect")
	budget := fs.Int("budget", 1800, "estimated total token budget")
	searchBudget := fs.Int("search-budget", 700, "estimated token budget for discovery evidence")
	contextLines := fs.Int("context-lines", 8, "line context around merged evidence")
	searchContextLines := fs.Int("search-context-lines", 1, "line context around search hits")
	mode := fs.String("mode", "context", "context mode: compact|evidence|context|full")
	sourceOnly := fs.Bool("source-only", true, "exclude tests and stories unless explicitly included")
	includeTests := fs.Bool("include-tests", false, "include test files")
	includeStories := fs.Bool("include-stories", false, "include story files")
	includeCallers := fs.Bool("callers", false, "include caller source slices for traced symbols")
	includeCallees := fs.Bool("callees", false, "include callee source slices for traced symbols")
	includeTraces := fs.Bool("traces", false, "include full trace_symbol payloads in JSON output")
	includeSearchSymbols := fs.Bool("search-symbols", false, "include symbol summaries on each search hit in JSON output")
	args = normalizeFlags(args, map[string]bool{
		"--index": true, "--limit": true, "--budget": true, "--search-budget": true,
		"--context-lines": true, "--search-context-lines": true, "--mode": true,
	}, map[string]bool{
		"--json": true, "--source-only": true, "--include-tests": true, "--include-stories": true,
		"--callers": true, "--callees": true, "--traces": true, "--search-symbols": true,
	})
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("inspect-flow requires one natural-language query")
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	resp, err := mamari.InspectFlow(idx, fs.Arg(0), mamari.InspectFlowOptions{
		Limit:                *limit,
		BudgetTokens:         *budget,
		SearchBudgetTokens:   *searchBudget,
		ContextLines:         *contextLines,
		SearchContextLines:   *searchContextLines,
		Mode:                 *mode,
		SourceOnly:           *sourceOnly,
		IncludeTests:         *includeTests,
		IncludeStories:       *includeStories,
		IncludeCallers:       *includeCallers,
		IncludeCallees:       *includeCallees,
		IncludeTraces:        *includeTraces,
		IncludeSearchSymbols: *includeSearchSymbols,
	})
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(resp)
	}
	fmt.Printf("%s: %s estimated=%d search_hits=%d context_slices=%d\n", resp.Query, resp.Status, resp.EstimatedTokens, len(resp.Search.Hits), len(resp.Context.Slices))
	for _, sym := range resp.Symbols {
		fmt.Printf("- symbol %s %s %s:%d\n", sym.Kind, sym.Name, sym.File, sym.StartLine)
	}
	for _, slice := range resp.Context.Slices {
		focus := ""
		if len(slice.FocusLines) > 0 {
			var parts []string
			for _, line := range slice.FocusLines {
				parts = append(parts, strconv.Itoa(line))
			}
			focus = " focus=" + strings.Join(parts, ",")
		} else if slice.FocusLine > 0 {
			focus = " focus=" + strconv.Itoa(slice.FocusLine)
		}
		fmt.Printf("--- %s:%d:%d [%s] %s%s estimated=%d ---\n", slice.File, slice.StartLine, slice.EndLine, slice.Kind, slice.Reason, focus, slice.EstimatedTokens)
		fmt.Print(slice.Text)
		if !strings.HasSuffix(slice.Text, "\n") {
			fmt.Println()
		}
	}
	for _, warning := range resp.Warnings {
		fmt.Printf("warning: %s\n", warning)
	}
	return nil
}

func runFetchContext(args []string) error {
	fs := flag.NewFlagSet("fetch-context", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	budget := fs.Int("budget", 1200, "estimated token budget for the complete serialized response")
	contextLines := fs.Int("context-lines", 8, "line context around file:line and term evidence")
	includeCallers := fs.Bool("callers", false, "include caller signature slices")
	includeCallees := fs.Bool("callees", false, "include callee signature slices")
	mode := fs.String("mode", "context", "response mode: compact|evidence|context|full")
	args = normalizeFlags(args, map[string]bool{"--index": true, "--budget": true, "--context-lines": true, "--mode": true}, map[string]bool{"--json": true, "--callers": true, "--callees": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("fetch-context requires one symbol, file:line, or RDF term query")
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	resp, err := mamari.FetchContext(idx, fs.Arg(0), mamari.FetchContextOptions{
		BudgetTokens:   *budget,
		ContextLines:   *contextLines,
		IncludeCallers: *includeCallers,
		IncludeCallees: *includeCallees,
		Mode:           *mode,
	})
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(resp)
	}
	fmt.Printf("%s: %s estimated=%d/%d truncated=%t\n", resp.Query, resp.Status, resp.EstimatedTokens, resp.BudgetTokens, resp.Truncated)
	for _, slice := range resp.Slices {
		fmt.Printf("--- %s:%d:%d [%s] %s estimated=%d ---\n", slice.File, slice.StartLine, slice.EndLine, slice.Kind, slice.Reason, slice.EstimatedTokens)
		fmt.Print(slice.Text)
		if !strings.HasSuffix(slice.Text, "\n") {
			fmt.Println()
		}
	}
	return nil
}

func runSearchLiteral(args []string) error {
	fs := flag.NewFlagSet("search-literal", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	lang := fs.String("lang", "", "filter to a specific language tag (e.g. de, en)")
	args = normalizeFlags(args, map[string]bool{"--index": true, "--lang": true}, map[string]bool{"--json": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("search-literal requires one query string")
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	resp := mamari.SearchLiteral(idx, fs.Arg(0), *lang)
	if *asJSON {
		return printJSON(resp)
	}
	for _, h := range resp.Hits {
		lang := ""
		if h.Lang != "" {
			lang = "@" + h.Lang
		}
		fmt.Printf("- %s:%d:%d %s%s = %q\n", h.Location.File, h.Location.StartLine, h.Location.StartColumn, h.Predicate, lang, h.Value)
	}
	return nil
}

func runSearchCode(args []string) error {
	fs := flag.NewFlagSet("search-code", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	limit := fs.Int("limit", 10, "maximum hits to return")
	budget := fs.Int("budget", 1200, "estimated token budget")
	contextLines := fs.Int("context-lines", 1, "line context around each hit")
	sourceOnly := fs.Bool("source-only", true, "exclude tests and stories unless explicitly included")
	includeTests := fs.Bool("include-tests", false, "include test files")
	includeStories := fs.Bool("include-stories", false, "include story files")
	blastRadius := fs.Bool("blast-radius", false, "include compact caller/callee/test hints for symbols on returned lines")
	exactFirst := fs.Bool("exact-first", false, "when exact literals are detected, return only exact matches if any exist")
	preferDefinitions := fs.Bool("prefer-definitions", false, "rank definition lines above usage sites")
	preferUsages := fs.Bool("prefer-usages", false, "rank usage sites above definition lines")
	mode := fs.String("mode", "context", "response mode: compact|evidence|context")
	symbolDetail := fs.Bool("symbol-detail", false, "include each hit's full containing-symbol signature/docstring/return-types/hot-path fields instead of just name/kind/file/startLine")
	args = normalizeFlags(args, map[string]bool{"--index": true, "--limit": true, "--budget": true, "--context-lines": true, "--mode": true}, map[string]bool{"--json": true, "--source-only": true, "--include-tests": true, "--include-stories": true, "--blast-radius": true, "--exact-first": true, "--prefer-definitions": true, "--prefer-usages": true, "--symbol-detail": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("search-code requires one query string")
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	resp := mamari.SearchCode(idx, fs.Arg(0), mamari.SearchCodeOptions{
		Limit:             *limit,
		BudgetTokens:      *budget,
		ContextLines:      *contextLines,
		SourceOnly:        *sourceOnly,
		IncludeTests:      *includeTests,
		IncludeStories:    *includeStories,
		BlastRadius:       *blastRadius,
		ExactFirst:        *exactFirst,
		PreferDefinitions: *preferDefinitions,
		PreferUsages:      *preferUsages,
		Mode:              *mode,
		SymbolDetail:      *symbolDetail,
		DiversifyFiles:    true,
	})
	mamari.FitSearchCodeResponse(&resp, *budget)
	if *asJSON {
		return printJSON(resp)
	}
	fmt.Printf("%s: %s estimated=%d/%d hits=%d/%d truncated=%t\n", resp.Query, resp.Status, resp.EstimatedTokens, resp.BudgetTokens, len(resp.Hits), resp.Total, resp.Truncated)
	for _, hit := range resp.Hits {
		fmt.Printf("--- %s:%d:%d score=%d terms=%s estimated=%d ---\n", hit.File, hit.StartLine, hit.EndLine, hit.Score, strings.Join(hit.MatchedTerms, ","), hit.EstimatedTokens)
		if len(hit.Symbols) > 0 {
			var names []string
			for _, sym := range hit.Symbols {
				names = append(names, sym.Kind+":"+sym.Name)
			}
			fmt.Printf("symbols: %s\n", strings.Join(names, ", "))
		}
		fmt.Print(hit.Text)
		if !strings.HasSuffix(hit.Text, "\n") {
			fmt.Println()
		}
	}
	return nil
}

func runSemanticQuery(args []string) error {
	fs := flag.NewFlagSet("semantic-query", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	limit := fs.Int("limit", 10, "maximum symbols to return")
	minScore := fs.Float64("min-score", 0.40, "minimum hybrid semantic score")
	sourceOnly := fs.Bool("source-only", true, "exclude tests and stories unless explicitly included")
	includeTests := fs.Bool("include-tests", false, "include test files")
	includeStories := fs.Bool("include-stories", false, "include story files")
	args = normalizeFlags(args, map[string]bool{"--index": true, "--limit": true, "--min-score": true}, map[string]bool{"--json": true, "--source-only": true, "--include-tests": true, "--include-stories": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("semantic-query requires one query string")
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	resp := mamari.SemanticQuery(idx, fs.Arg(0), mamari.SemanticQueryOptions{
		Limit: *limit, MinScore: *minScore, SourceOnly: *sourceOnly,
		IncludeTests: *includeTests, IncludeStories: *includeStories,
	})
	if *asJSON {
		return printJSON(resp)
	}
	fmt.Printf("%s: %s model=%s dimensions=%d hits=%d/%d\n", resp.Query, resp.Status, resp.Model, resp.Dimensions, len(resp.Hits), resp.Total)
	for _, hit := range resp.Hits {
		fmt.Printf("- %.4f %s %s %s:%d concepts=%s\n", hit.Score, hit.Symbol.Kind, hit.Symbol.Name, hit.Symbol.File, hit.Symbol.StartLine, strings.Join(hit.Terms, ","))
	}
	return nil
}

func runInspectExact(args []string) error {
	fs := flag.NewFlagSet("inspect-exact", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	limit := fs.Int("limit", 8, "maximum evidence clusters to return")
	contextLines := fs.Int("context-lines", 0, "optional source context around exact evidence lines")
	withSource := fs.Bool("with-source", false, "include full containing symbol source where available")
	sourceOnly := fs.Bool("source-only", true, "exclude tests and stories unless explicitly included")
	includeTests := fs.Bool("include-tests", false, "include test files")
	args = normalizeFlags(args, map[string]bool{"--index": true, "--limit": true, "--context-lines": true}, map[string]bool{"--json": true, "--with-source": true, "--source-only": true, "--include-tests": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("inspect-exact requires one query string")
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	resp := mamari.InspectExact(idx, fs.Arg(0), mamari.InspectExactOptions{
		ContextLines: *contextLines,
		WithSource:   *withSource,
		Limit:        *limit,
		SourceOnly:   *sourceOnly,
		IncludeTests: *includeTests,
	})
	if *asJSON {
		return printJSON(resp)
	}
	fmt.Printf("%s: %s estimated=%d clusters=%d truncated=%t\n", resp.Query, resp.Status, resp.EstimatedTokens, len(resp.Clusters), resp.Truncated)
	for _, cluster := range resp.Clusters {
		header := cluster.File
		if cluster.Symbol != "" {
			header += ":" + cluster.Symbol
		}
		if cluster.Route != "" {
			header += " route=" + cluster.Route
		}
		fmt.Printf("--- %s score=%d matched=%s estimated=%d ---\n", header, cluster.Score, strings.Join(cluster.Matched, ","), cluster.EstimatedTokens)
		for _, line := range cluster.Lines {
			fmt.Printf("%d: %s\n", line.Line, line.Text)
		}
	}
	return nil
}

func runRepoMap(args []string) error {
	fs := flag.NewFlagSet("repo-map", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	budget := fs.Int("budget", 1200, "estimated token budget")
	limit := fs.Int("limit", 40, "maximum files and symbols to return")
	mentioned := fs.String("mentioned", "", "comma-separated identifiers, paths, or terms to personalize ranking")
	sourceOnly := fs.Bool("source-only", true, "exclude tests and stories unless explicitly included")
	includeTests := fs.Bool("include-tests", false, "include test files")
	architecture := fs.Bool("architecture", true, "include compact architecture summary")
	args = normalizeFlags(args, map[string]bool{"--index": true, "--budget": true, "--limit": true, "--mentioned": true}, map[string]bool{"--json": true, "--source-only": true, "--include-tests": true, "--architecture": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	resp := mamari.RepoMap(idx, mamari.RepoMapOptions{
		BudgetTokens:        *budget,
		Limit:               *limit,
		Mentioned:           splitCSV(*mentioned),
		Query:               strings.Join(fs.Args(), " "),
		SourceOnly:          *sourceOnly,
		IncludeTests:        *includeTests,
		IncludeArchitecture: *architecture,
	})
	if *asJSON {
		return printJSON(resp)
	}
	if resp.Query != "" {
		fmt.Printf("repo-map: query=%q %s estimated=%d/%d files=%d symbols=%d truncated=%t\n", resp.Query, resp.Status, resp.EstimatedTokens, resp.BudgetTokens, len(resp.Files), len(resp.Symbols), resp.Truncated)
	} else {
		fmt.Printf("repo-map: %s estimated=%d/%d files=%d symbols=%d truncated=%t\n", resp.Status, resp.EstimatedTokens, resp.BudgetTokens, len(resp.Files), len(resp.Symbols), resp.Truncated)
	}
	if resp.Architecture != nil {
		fmt.Printf("architecture: languages=%d packages=%d entrypoints=%d routes=%d hotspots=%d communities=%d boundaries=%d estimated=%d\n",
			len(resp.Architecture.Languages), len(resp.Architecture.Packages), len(resp.Architecture.EntryPoints),
			len(resp.Architecture.Routes), len(resp.Architecture.Hotspots), len(resp.Architecture.Communities),
			len(resp.Architecture.Boundaries), resp.Architecture.EstimatedTokens)
	}
	for _, file := range resp.Files {
		fmt.Printf("- file %s rank=%.6f in=%d out=%d mentions=%s\n", file.File, file.Rank, file.Inbound, file.Outbound, strings.Join(file.MatchedMention, ","))
	}
	for _, sym := range resp.Symbols {
		fmt.Printf("- symbol %s %s %s:%d score=%d\n", sym.Kind, sym.Name, sym.File, sym.StartLine, sym.Score)
	}
	return nil
}

func runImportSCIP(args []string) error {
	fs := flag.NewFlagSet("import-scip", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file to update")
	scipPath := fs.String("scip", "", "SCIP index file")
	args = normalizeFlags(args, map[string]bool{"--index": true, "--scip": true}, nil)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *scipPath == "" {
		return fmt.Errorf("import-scip requires --scip <index.scip>")
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	file, err := os.Open(*scipPath)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := mamari.IngestSCIP(idx, file); err != nil {
		return err
	}
	if err := mamari.SaveIndex(idx, *indexPath); err != nil {
		return err
	}
	fmt.Printf("imported SCIP into %s: symbols=%d edges=%d\n", *indexPath, len(idx.Symbols), len(idx.SymbolEdges))
	return nil
}

func runInspectTerm(args []string) error {
	fs := flag.NewFlagSet("inspect-term", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	budget := fs.Int("budget", 1200, "estimated token budget for implementation context")
	contextLines := fs.Int("context-lines", 6, "line context around implementation evidence")
	mode := fs.String("mode", "context", "context mode: compact|evidence|context|full")
	includeWeak := fs.Bool("include-weak", false, "include weak local-name references")
	limit := fs.Int("limit", 8, "maximum implementation evidence candidates")
	args = normalizeFlags(args, map[string]bool{"--index": true, "--budget": true, "--context-lines": true, "--mode": true, "--limit": true}, map[string]bool{"--json": true, "--include-weak": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("inspect-term requires one RDF term, local name, or IRI")
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	resp, err := mamari.InspectTerm(idx, fs.Arg(0), mamari.InspectTermOptions{
		BudgetTokens: *budget,
		ContextLines: *contextLines,
		Mode:         *mode,
		IncludeWeak:  *includeWeak,
		Limit:        *limit,
	})
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(resp)
	}
	fmt.Printf("%s: %s estimated=%d\n", resp.Query, resp.Status, resp.EstimatedTokens)
	if resp.Term != nil {
		fmt.Printf("term: %s %s\n", resp.Term.Term, resp.Term.IRI)
	}
	for _, hit := range resp.Implementation {
		fmt.Printf("- implementation %s:%d score=%d exact=%s\n", hit.File, hit.FocusLine, hit.Score, strings.Join(hit.MatchedExact, ","))
		if hit.Text != "" {
			fmt.Print(hit.Text)
			if !strings.HasSuffix(hit.Text, "\n") {
				fmt.Println()
			}
		}
	}
	for _, slice := range resp.Context.Slices {
		fmt.Printf("--- %s:%d:%d [%s] %s estimated=%d ---\n", slice.File, slice.StartLine, slice.EndLine, slice.Kind, slice.Reason, slice.EstimatedTokens)
		fmt.Print(slice.Text)
		if !strings.HasSuffix(slice.Text, "\n") {
			fmt.Println()
		}
	}
	return nil
}

func runFindContainingShape(args []string) error {
	fs := flag.NewFlagSet("find-containing-shape", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	args = normalizeFlags(args, map[string]bool{"--index": true}, map[string]bool{"--json": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("find-containing-shape requires one term/IRI")
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	resp := mamari.FindContainingShape(idx, fs.Arg(0))
	if *asJSON {
		return printJSON(resp)
	}
	target := fs.Arg(0)
	for _, sh := range resp.Containers {
		fmt.Printf("- %s (%s) %s:%d:%d\n", sh.Term, sh.IRI, sh.Location.File, sh.Location.StartLine, sh.Location.StartColumn)
		matches := matchingPathsAndBranches(sh, target)
		for _, line := range matches {
			fmt.Printf("    %s\n", line)
		}
	}
	return nil
}

func runQueryGraph(args []string) error {
	fs := flag.NewFlagSet("query-graph", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	maxRows := fs.Int("max-rows", 0, "optional row cap below the hard 5000-row ceiling")
	args = normalizeFlags(args, map[string]bool{"--index": true, "--max-rows": true}, map[string]bool{"--json": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("query-graph requires one MATCH/WHERE/RETURN/ORDER BY/LIMIT query string")
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	resp := mamari.QueryGraphLite(idx, fs.Arg(0), mamari.QueryGraphLiteOptions{MaxRows: *maxRows})
	if *asJSON {
		return printJSON(resp)
	}
	fmt.Printf("%s: %d row(s)", resp.Status, len(resp.Rows))
	if resp.Truncated {
		fmt.Printf(" (truncated, total=%d)", resp.Total)
	}
	fmt.Println()
	for _, w := range resp.Warnings {
		fmt.Printf("warning: %s\n", w)
	}
	for _, row := range resp.Rows {
		fmt.Printf("- %v\n", row)
	}
	return nil
}

func runListDynamicIRIs(args []string) error {
	fs := flag.NewFlagSet("list-dynamic-iris", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	file := fs.String("file", "", "filter to a single repo-relative file")
	args = normalizeFlags(args, map[string]bool{"--index": true, "--file": true}, map[string]bool{"--json": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	resp := mamari.ListDynamicIRIs(idx, *file)
	if *asJSON {
		return printJSON(resp)
	}
	for _, c := range resp.Calls {
		fmt.Printf("- %s:%d:%d %s(%s)\n", c.File, c.Line, c.Column, c.Callee, c.Snippet)
	}
	return nil
}

func runIndex(args []string) error {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	repo := fs.String("repo", ".", "repository root to index")
	indexPath := fs.String("index", "", "where to write the index file (defaults to <repo>/.mamari/index.json)")
	commit := fs.Bool("commit", false, "also write a plain-JSON copy to .mamari/committed/index.json for git, and ensure .gitignore tracks it (see `mamari hooks install`)")
	quiet := fs.Bool("quiet", false, "suppress the summary line (used by the pre-commit hook)")
	args = normalizeFlags(args, map[string]bool{"--repo": true, "--index": true}, map[string]bool{"--commit": true, "--quiet": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	idx, err := mamari.BuildIndex(*repo)
	if err != nil {
		return err
	}
	out := *indexPath
	if out == "" {
		out = filepath.Join(idx.Repo.Root, ".mamari", "index.json")
	}
	if err := mamari.SaveIndex(idx, out); err != nil {
		return err
	}
	if *commit {
		if err := mamari.SaveIndexJSON(idx, mamari.CommittedIndexPath(idx.Repo.Root)); err != nil {
			return err
		}
	}
	if !*quiet {
		fmt.Printf("indexed %d files, %d terms, %d references -> %s\n", len(idx.Files), len(idx.Terms), len(idx.References), out)
		if *commit {
			fmt.Printf("committed index -> %s\n", mamari.CommittedIndexPath(idx.Repo.Root))
		}
	}
	return nil
}

func runHooks(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: mamari hooks <install|uninstall> [--repo <path>]")
	}
	sub := args[0]
	fs := flag.NewFlagSet("hooks "+sub, flag.ContinueOnError)
	repo := fs.String("repo", ".", "repository root")
	rest := normalizeFlags(args[1:], map[string]bool{"--repo": true}, nil)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	abs, err := filepath.Abs(*repo)
	if err != nil {
		return err
	}
	switch sub {
	case "install":
		hookPath, err := mamari.InstallPreCommitHook(abs)
		if err != nil {
			return err
		}
		fmt.Printf("installed pre-commit hook: %s\n", hookPath)
		fmt.Printf("committed index will be written to: %s\n", mamari.CommittedIndexPath(abs))
		fmt.Println("run `mamari index --commit` once now to write the first committed index")
		return nil
	case "uninstall":
		if err := mamari.UninstallPreCommitHook(abs); err != nil {
			return err
		}
		fmt.Println("removed mamari's pre-commit hook block")
		return nil
	default:
		return fmt.Errorf("usage: mamari hooks <install|uninstall> [--repo <path>]")
	}
}

func runTraceTerm(args []string) error {
	fs := flag.NewFlagSet("trace-term", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	compact := fs.Bool("compact", false, "emit compact response")
	grouped := fs.Bool("grouped", false, "group compact locations by file")
	includeWeak := fs.Bool("include-weak", false, "include weak local-name references")
	args = normalizeFlags(args, map[string]bool{"--index": true}, map[string]bool{"--json": true, "--compact": true, "--grouped": true, "--include-weak": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("trace-term requires one term")
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	opts := mamari.QueryOptions{IncludeWeak: *includeWeak}
	if *grouped {
		resp := mamari.TraceTermGroupedCompact(idx, fs.Arg(0), opts)
		if *asJSON {
			return printJSON(resp)
		}
		printGroupedCompactTrace(resp)
		return nil
	}
	if *compact {
		resp := mamari.TraceTermCompact(idx, fs.Arg(0), opts)
		if *asJSON {
			return printJSON(resp)
		}
		printCompactTrace(resp)
		return nil
	}
	resp := mamari.TraceTerm(idx, fs.Arg(0), opts)
	if *asJSON {
		return printJSON(resp)
	}
	printTrace(resp)
	return nil
}

func runFindReferences(args []string) error {
	fs := flag.NewFlagSet("find-references", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	includeWeak := fs.Bool("include-weak", false, "include weak local-name references")
	args = normalizeFlags(args, map[string]bool{"--index": true}, map[string]bool{"--json": true, "--include-weak": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("find-references requires one term")
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	resp := mamari.FindReferences(idx, fs.Arg(0), mamari.QueryOptions{IncludeWeak: *includeWeak})
	if *asJSON {
		return printJSON(resp)
	}
	fmt.Printf("%s: %s\n", resp.Query, resp.Status)
	for _, ref := range resp.References {
		fmt.Printf("- %s:%d:%d [%s] %s\n", ref.File, ref.StartLine, ref.StartColumn, ref.Confidence, ref.Context)
	}
	return nil
}

func runListTerms(args []string) error {
	fs := flag.NewFlagSet("list-terms", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	args = normalizeFlags(args, map[string]bool{"--index": true}, map[string]bool{"--json": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	prefix := ""
	if fs.NArg() > 0 {
		prefix = fs.Arg(0)
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	resp := mamari.ListTerms(idx, prefix)
	if *asJSON {
		return printJSON(resp)
	}
	for _, term := range resp.Terms {
		fmt.Printf("%s\t%s\n", term.Term, term.IRI)
	}
	return nil
}

func runFetchSource(args []string) error {
	fs := flag.NewFlagSet("fetch-source", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	args = normalizeFlags(args, map[string]bool{"--index": true}, map[string]bool{"--json": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("fetch-source requires filepath:start_line:end_line")
	}
	file, start, end, err := parseRange(fs.Arg(0))
	if err != nil {
		return err
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	resp, err := mamari.FetchSource(idx, file, start, end)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(resp)
	}
	fmt.Print(resp.Text)
	return nil
}

func runBenchmarkCGP(args []string) error {
	fs := flag.NewFlagSet("benchmark-cgp", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	gold := fs.String("gold", "", "path to gold fixture JSON (required)")
	jsonOut := fs.String("json-out", "", "write JSON report to this path (default stdout)")
	args = normalizeFlags(args, map[string]bool{"--index": true, "--gold": true, "--json-out": true}, nil)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *gold == "" {
		return fmt.Errorf("benchmark-cgp requires --gold <fixture>")
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	report, err := mamari.BenchmarkCGP(idx, *gold)
	if err != nil {
		return err
	}
	out, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if *jsonOut != "" {
		if err := os.WriteFile(*jsonOut, append(out, '\n'), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "benchmark-cgp: status=%s appRecall=%.3f testRecall=%.3f written=%s\n",
			report.Summary.OverallStatus, report.Summary.AppRecall, report.Summary.TestRecall, *jsonOut)
	} else {
		fmt.Println(string(out))
	}
	if report.Summary.OverallStatus == "fail" {
		return fmt.Errorf("benchmark-cgp failed: %d notFound, %d violations, appRecall=%.3f",
			report.Summary.NotFound, report.Summary.Violations, report.Summary.AppRecall)
	}
	return nil
}

func runImpact(args []string) error {
	fs := flag.NewFlagSet("impact", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	depth := fs.Int("depth", 2, "reverse traversal depth (1 = direct callers only)")
	compact := fs.Bool("compact", false, "drop signature/docstring/return-types/hot-path/id/language fields from each layer entry")
	args = normalizeFlags(args, map[string]bool{"--index": true, "--depth": true}, map[string]bool{"--json": true, "--compact": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("impact requires one symbol id or name")
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	resp := mamari.ImpactWithOptions(idx, fs.Arg(0), mamari.ImpactOptions{Depth: *depth, Compact: *compact})
	if *asJSON {
		return printJSON(resp)
	}
	fmt.Printf("%s: %s depth=%d total=%d truncated=%t\n", resp.Query, resp.Status, resp.Depth, resp.Total, resp.Truncated)
	if resp.Symbol != nil {
		fmt.Printf("target: %s %s %s:%d\n", resp.Symbol.Kind, resp.Symbol.Name, resp.Symbol.File, resp.Symbol.StartLine)
	}
	for _, layer := range resp.Layers {
		fmt.Printf("layer %d (%d symbols):\n", layer.Depth, len(layer.Symbols))
		for _, sym := range layer.Symbols {
			extra := ""
			if sym.PathReason != "" {
				extra = " " + sym.PathReason
			}
			fmt.Printf("  - [%s%s] %s %s:%d\n", sym.PathConfidence, extra, sym.Name, sym.File, sym.StartLine)
		}
	}
	return nil
}

func runReview(args []string) error {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	base := fs.String("base", "HEAD", "git ref to diff the working tree against")
	depth := fs.Int("depth", 2, "caller hops to walk for blast radius")
	limit := fs.Int("limit", 0, "max changed symbols to report (0 = default)")
	includeTests := fs.Bool("include-tests", false, "include changed symbols in test files")
	callers := fs.Bool("callers", false, "include per-symbol proven/possible caller lists")
	coverage := fs.String("coverage", "", "lcov report (e.g. coverage/lcov.info); makes 'untested' reflect what actually ran under tests")
	args = normalizeFlags(args, map[string]bool{"--index": true, "--base": true, "--depth": true, "--limit": true, "--coverage": true}, map[string]bool{"--json": true, "--include-tests": true, "--callers": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	resp := mamari.Review(idx, mamari.ReviewOptions{Base: *base, Depth: *depth, Limit: *limit, IncludeTests: *includeTests, Callers: *callers, CoveragePath: *coverage})
	if *asJSON {
		return printJSON(resp)
	}
	if resp.Status != "ok" {
		fmt.Printf("review vs %s: %s — %s\n", resp.Base, resp.Status, resp.Message)
		return nil
	}
	fmt.Printf("review vs %s: %d changed symbol(s) across %d file(s); %d proven-affected, %d possible-affected, %d untested-changed, %d high-risk\n",
		resp.Base, resp.ChangedSymbols, resp.ChangedFiles, resp.ProvenAffected, resp.PossibleAffected, resp.UntestedChanged, resp.HighRisk)
	for _, s := range resp.Symbols {
		flags := ""
		if s.Untested {
			flags += " untested"
		}
		fmt.Printf("  [%s]%s %s %s:%d — %d proven / %d possible callers", s.Risk, flags, s.Name, s.File, s.StartLine, s.ProvenCount, s.PossibleCount)
		if len(s.RiskReasons) > 0 {
			fmt.Printf("  (%s)", strings.Join(s.RiskReasons, "; "))
		}
		fmt.Println()
	}
	if len(resp.AlsoConsider) > 0 {
		names := make([]string, 0, len(resp.AlsoConsider))
		for _, e := range resp.AlsoConsider {
			names = append(names, e.File)
		}
		fmt.Printf("also consider (historically co-changed): %s\n", strings.Join(names, ", "))
	}
	return nil
}

func runDeadCode(args []string) error {
	fs := flag.NewFlagSet("dead-code", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	limit := fs.Int("limit", 0, "max symbols to report (0 = default)")
	kinds := fs.String("kinds", "", "comma-separated symbol kinds (default: function,class,interface,component)")
	includeExported := fs.Bool("include-exported", false, "include exported symbols as candidates")
	includeTests := fs.Bool("include-tests", false, "include symbols in test files")
	includeUncertain := fs.Bool("include-uncertain", true, "list 'possibly dead' symbols held back by unresolved same-name calls")
	args = normalizeFlags(args, map[string]bool{"--index": true, "--limit": true, "--kinds": true}, map[string]bool{"--json": true, "--include-exported": true, "--include-tests": true, "--include-uncertain": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	var kindList []string
	if strings.TrimSpace(*kinds) != "" {
		for _, k := range strings.Split(*kinds, ",") {
			if k = strings.TrimSpace(k); k != "" {
				kindList = append(kindList, k)
			}
		}
	}
	resp := mamari.DeadCode(idx, mamari.DeadCodeOptions{Limit: *limit, Kinds: kindList, IncludeExported: *includeExported, IncludeTests: *includeTests, IncludeUncertain: *includeUncertain})
	if *asJSON {
		return printJSON(resp)
	}
	fmt.Printf("dead-code: %d unreferenced, %d uncertain (possibly reached via unresolved edges), truncated=%t\n", resp.Total, resp.UncertainSkipped, resp.Truncated)
	for _, s := range resp.Symbols {
		fmt.Printf("  [dead] %s %s %s:%d\n", s.Kind, s.Name, s.File, s.StartLine)
	}
	for _, s := range resp.Uncertain {
		fmt.Printf("  [review] %s %s %s:%d — unresolved same-name call may reach it\n", s.Kind, s.Name, s.File, s.StartLine)
	}
	return nil
}

func runDuplicates(args []string) error {
	fs := flag.NewFlagSet("duplicates", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	asJSON := fs.Bool("json", false, "emit JSON")
	limit := fs.Int("limit", 0, "max clone clusters to report (0 = default)")
	includeTests := fs.Bool("include-tests", false, "include clones among test files")
	args = normalizeFlags(args, map[string]bool{"--index": true, "--limit": true}, map[string]bool{"--json": true, "--include-tests": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	resp := mamari.Duplication(idx, mamari.DuplicationOptions{Limit: *limit, IncludeTests: *includeTests})
	if *asJSON {
		return printJSON(resp)
	}
	fmt.Printf("duplicates: %d clone cluster(s), truncated=%t\n", resp.TotalClusters, resp.Truncated)
	for _, c := range resp.Clusters {
		fmt.Printf("  [%d copies, ~%d lines]\n", c.Count, c.Lines)
		for _, m := range c.Members {
			fmt.Printf("      %s %s %s:%d\n", m.Kind, m.Name, m.File, m.StartLine)
		}
	}
	return nil
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	repo := fs.String("repo", "", "repository root (default: index.json's repo)")
	asJSON := fs.Bool("json", false, "emit JSON")
	parseFailureLimit := fs.Int("parse-failure-limit", 0, "maximum parse-failure examples (0 returns all)")
	checkCommitted := fs.Bool("check-committed", false, "check the git-committed index (.mamari/committed/index.json) for staleness instead of --index; exits non-zero if stale (useful as a CI guard when a contributor bypassed the pre-commit hook with --no-verify)")
	args = normalizeFlags(args, map[string]bool{"--index": true, "--repo": true, "--parse-failure-limit": true}, map[string]bool{"--json": true, "--check-committed": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *checkCommitted {
		root := *repo
		if root == "" {
			root = "."
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			return err
		}
		committedPath := mamari.CommittedIndexPath(abs)
		idx, err := mamari.LoadIndex(committedPath)
		if err != nil {
			return fmt.Errorf("load committed index %s: %w (run `mamari hooks install` or `mamari index --commit` first)", committedPath, err)
		}
		idx.Repo.Root = abs
		report := mamari.LimitDoctorParseFailures(mamari.Doctor(idx), *parseFailureLimit)
		if *asJSON {
			if err := printJSON(report); err != nil {
				return err
			}
		} else {
			fmt.Printf("committed index: %s\n", committedPath)
			fmt.Printf("status: %s\n", report.Status)
			if len(report.FilesChangedSinceIndex) > 0 {
				fmt.Printf("changed since committed index: %d (%s)\n", len(report.FilesChangedSinceIndex), strings.Join(report.FilesChangedSinceIndex, ", "))
			}
			if len(report.FilesDeletedSinceIndex) > 0 {
				fmt.Printf("deleted since committed index: %d (%s)\n", len(report.FilesDeletedSinceIndex), strings.Join(report.FilesDeletedSinceIndex, ", "))
			}
		}
		if report.Stale || len(report.FilesChangedSinceIndex) > 0 || len(report.FilesDeletedSinceIndex) > 0 {
			return fmt.Errorf("committed index is stale — run `mamari index --commit` and commit the result")
		}
		return nil
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	if *repo != "" {
		// Override repo root for staleness probe (useful when running from
		// elsewhere or when the index was built in a different working dir).
		abs, err := filepath.Abs(*repo)
		if err != nil {
			return err
		}
		idx.Repo.Root = abs
	}
	report := mamari.LimitDoctorParseFailures(mamari.Doctor(idx), *parseFailureLimit)
	if *asJSON {
		return printJSON(report)
	}
	fmt.Printf("status: %s\n", report.Status)
	fmt.Printf("repo: %s\n", report.RepoRoot)
	fmt.Printf("indexedAt: %s (%.1fh ago)\n", report.IndexedAt, report.IndexAgeHours)
	if report.IndexedCommit != "" || report.CurrentCommit != "" {
		stale := ""
		if report.Stale {
			stale = " (stale)"
		}
		fmt.Printf("commit: indexed=%s current=%s%s\n", report.IndexedCommit, report.CurrentCommit, stale)
	}
	fmt.Printf("files: %d (by status %v)\n", report.Files.Total, report.Files.ByStatus)
	fmt.Printf("symbols: %d (by confidence %v)\n", report.Symbols.Total, report.Symbols.ByConfidence)
	fmt.Printf("edges: %d (by confidence %v)\n", report.Edges.Total, report.Edges.ByConfidence)
	fmt.Printf("unresolved: %d edges across %d distinct targets (by reason %v)\n",
		report.Unresolved.Total, report.Unresolved.UnknownTo, report.Unresolved.ByReason)
	fmt.Printf("dynamicIris: %d\n", report.DynamicIRIs)
	if len(report.FilesChangedSinceIndex) > 0 {
		fmt.Printf("changed since index: %d (%s)\n", len(report.FilesChangedSinceIndex), strings.Join(report.FilesChangedSinceIndex, ", "))
	}
	if len(report.FilesDeletedSinceIndex) > 0 {
		fmt.Printf("deleted since index: %d (%s)\n", len(report.FilesDeletedSinceIndex), strings.Join(report.FilesDeletedSinceIndex, ", "))
	}
	if len(report.ParseFailures) > 0 {
		fmt.Println("parse failures:")
		for _, pf := range report.ParseFailures {
			fmt.Printf("  - %s [%s] %s: %s\n", pf.File, pf.Parser, pf.Status, pf.Error)
		}
		if report.ParseFailuresTruncated {
			fmt.Printf("  ... %d total (raise --parse-failure-limit or use 0 for all)\n", report.ParseFailureTotal)
		}
	}
	if len(report.TopUnresolved) > 0 {
		fmt.Println("top unresolved targets:")
		for _, u := range report.TopUnresolved {
			fmt.Printf("  - %dx %s (%s)\n", u.Count, u.Target, u.Reason)
		}
	}
	for _, w := range report.Warnings {
		fmt.Printf("warning: %s\n", w)
	}
	return nil
}

type serveCommandConfig struct {
	indexPath string
	options   mcpserver.ServeOptions
}

func parseServeCommand(args []string) (serveCommandConfig, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	watch := fs.Bool("watch", true, "watch the indexed repo and update the in-memory MCP index; use --watch=false to disable")
	debounceMs := fs.Int("debounce-ms", 200, "debounce window for --watch filesystem events")
	persist := fs.Bool("persist", false, "with --watch, rewrite the on-disk index after each rebake")
	link := fs.String("link", "", "comma-separated paths to other repos' index.json files, loaded read-only for cross-repo tools like find_route")
	toolset := fs.String("toolset", "slim", "MCP tool surface: slim (one mamari router tool), adaptive (named tools gated by repo/session capabilities), or full (all named tools)")
	fullToolset := fs.Bool("full-toolset", false, "register every MCP tool unconditionally, including TTL/event/cross-repo tools that are normally hidden when the index/session has nothing for them to act on, plus the rarely-needed admin tools (manage_notes, manage_adr, diff_index). Off by default to keep the per-session tools/list token cost down — pass this when you need notes/ADR bookkeeping or PR-diff analysis.")
	memoryLimitMB := fs.Int64("memory-limit-mb", -1, "soft Go heap limit in MiB; -1 selects an index-size-based default, 0 respects the existing runtime/GOMEMLIMIT")
	args = normalizeFlags(args, map[string]bool{"--index": true, "--debounce-ms": true, "--link": true, "--toolset": true, "--memory-limit-mb": true}, map[string]bool{"--watch": true, "--persist": true, "--full-toolset": true})
	if err := fs.Parse(args); err != nil {
		return serveCommandConfig{}, err
	}
	if *memoryLimitMB < -1 {
		return serveCommandConfig{}, fmt.Errorf("--memory-limit-mb must be -1, 0, or a positive MiB value")
	}
	const maxMemoryLimitMiB = int64(^uint64(0)>>1) >> 20
	if *memoryLimitMB > maxMemoryLimitMiB {
		return serveCommandConfig{}, fmt.Errorf("--memory-limit-mb is too large")
	}
	switch strings.ToLower(strings.TrimSpace(*toolset)) {
	case "slim", "adaptive", "full":
	default:
		return serveCommandConfig{}, fmt.Errorf("--toolset must be one of slim|adaptive|full, got %q", *toolset)
	}
	var linked []string
	for _, p := range strings.Split(*link, ",") {
		if p = strings.TrimSpace(p); p != "" {
			linked = append(linked, p)
		}
	}
	memoryLimitBytes := *memoryLimitMB
	if memoryLimitBytes > 0 {
		memoryLimitBytes <<= 20
	}
	resolvedVersion, _, _ := resolveVersionMetadata(version, commit, date, debug.ReadBuildInfo)
	return serveCommandConfig{
		indexPath: *indexPath,
		options: mcpserver.ServeOptions{
			ServerVersion:    resolvedVersion,
			Watch:            *watch,
			Debounce:         time.Duration(*debounceMs) * time.Millisecond,
			Persist:          *persist,
			LinkedIndexes:    linked,
			FullToolset:      *fullToolset,
			Toolset:          *toolset,
			MemoryLimitBytes: memoryLimitBytes,
		},
	}, nil
}

func runServe(args []string) error {
	config, err := parseServeCommand(args)
	if err != nil {
		return err
	}
	if err := mcpserver.ServeWithOptions(config.indexPath, config.options); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf(
				"index not found at %s; run `mamari init --repo .` from the codebase or rebuild it with `mamari index --repo /path/to/repo --index %s`: %w",
				config.indexPath,
				config.indexPath,
				err,
			)
		}
		return err
	}
	return nil
}

func runUI(args []string) error {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	indexPath := fs.String("index", ".mamari/index.json", "index file")
	addr := fs.String("addr", "127.0.0.1:7331", "local HTTP listen address")
	watch := fs.Bool("watch", false, "watch the indexed repo and update the graph live")
	debounceMs := fs.Int("debounce-ms", 200, "debounce window for --watch filesystem events")
	args = normalizeFlags(args, map[string]bool{"--index": true, "--addr": true, "--debounce-ms": true}, map[string]bool{"--watch": true})
	if err := fs.Parse(args); err != nil {
		return err
	}
	idx, err := loadIndexOrHint(*indexPath)
	if err != nil {
		return err
	}
	// mamari ui is the other long-running process besides `mamari serve` —
	// see Index.ReleaseUnusedMemory's doc comment in internal/mamari for
	// the measured rationale (one-shot CLI commands like `mamari index`
	// deliberately don't call this: the OS reclaims everything on exit
	// regardless, so there's no benefit, only wasted GC/scavenge time).
	idx.ReleaseUnusedMemory()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *watch {
		go func() {
			if err := mamari.Watch(ctx, idx, mamari.WatchOptions{
				Debounce: time.Duration(*debounceMs) * time.Millisecond,
				OnError:  func(err error) { fmt.Fprintln(os.Stderr, "mamari ui watch:", err) },
			}); err != nil && !errors.Is(err, context.Canceled) {
				fmt.Fprintln(os.Stderr, "mamari ui watch:", err)
			}
		}()
	}
	server := &http.Server{Addr: *addr, Handler: mamari.NewGraphUIHandler(idx), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	fmt.Fprintf(os.Stderr, "mamari graph explorer: http://%s\n", *addr)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func normalizeFlags(args []string, valueFlags map[string]bool, boolFlags map[string]bool) []string {
	var flags []string
	var rest []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		moved := false
		if boolFlags[arg] {
			flags = append(flags, arg)
			continue
		}
		for key := range boolFlags {
			if strings.HasPrefix(arg, key+"=") {
				flags = append(flags, arg)
				moved = true
				break
			}
		}
		if moved {
			continue
		}
		if valueFlags[arg] && i+1 < len(args) {
			flags = append(flags, arg, args[i+1])
			i++
			continue
		}
		for key := range valueFlags {
			if strings.HasPrefix(arg, key+"=") {
				flags = append(flags, arg)
				moved = true
				break
			}
		}
		if moved {
			continue
		}
		rest = append(rest, arg)
	}
	return append(flags, rest...)
}

func matchingPathsAndBranches(sh mamari.Shape, target string) []string {
	var out []string
	matches := func(term, iri string) bool { return term == target || iri == target }
	for _, p := range sh.Paths {
		if matches(p.Term, p.IRI) {
			out = append(out, fmt.Sprintf("sh:path %s at %s:%d", p.Term, p.Location.File, p.Location.StartLine))
		}
	}
	for _, n := range sh.Nodes {
		if matches(n.Term, n.IRI) {
			out = append(out, fmt.Sprintf("sh:node %s at %s:%d", n.Term, n.Location.File, n.Location.StartLine))
		}
	}
	for _, t := range sh.TargetClasses {
		if matches(t.Term, t.IRI) {
			out = append(out, fmt.Sprintf("sh:targetClass %s at %s:%d", t.Term, t.Location.File, t.Location.StartLine))
		}
	}
	for _, p := range sh.Predicates {
		if p.Predicate == target || matches(p.Term, p.IRI) {
			out = append(out, fmt.Sprintf("%s %s at %s:%d", p.Predicate, p.Term, p.Location.File, p.Location.StartLine))
		}
	}
	for i, b := range sh.Branches {
		if matches(b.Datatype, b.DatatypeIRI) || matches(b.Path, b.PathIRI) {
			out = append(out, fmt.Sprintf("%s[%d] name=%q datatype=%s pattern=%q at %s:%d", b.Kind, i, b.Name, b.Datatype, b.Pattern, b.Location.File, b.Location.StartLine))
		}
	}
	for _, n := range sh.Names {
		if n.Lang != "" {
			out = append(out, fmt.Sprintf("shape name@%s: %q", n.Lang, n.Value))
			break
		}
	}
	return out
}

func parseRange(spec string) (string, int, int, error) {
	parts := strings.Split(spec, ":")
	if len(parts) < 3 {
		return "", 0, 0, fmt.Errorf("range must be filepath:start_line:end_line")
	}
	end, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return "", 0, 0, err
	}
	start, err := strconv.Atoi(parts[len(parts)-2])
	if err != nil {
		return "", 0, 0, err
	}
	return strings.Join(parts[:len(parts)-2], ":"), start, end, nil
}

func splitCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func defaultIndexPath(repoRoot string) string {
	return filepath.Join(repoRoot, ".mamari", "index.json")
}

func loadIndexOrHint(path string) (*mamari.Index, error) {
	idx, err := mamari.LoadIndex(path)
	if err == nil {
		return idx, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("index not found at %s\nrun: mamari init --repo .\nor:  mamari index --repo . --index %s", path, path)
	}
	if strings.Contains(err.Error(), "schemaVersion") {
		return nil, fmt.Errorf("%w\nrun: mamari index --repo . --index %s", err, path)
	}
	return nil, err
}

func expandMCPClients(client string) []string {
	var out []string
	seen := map[string]bool{}
	for _, requested := range strings.Split(client, ",") {
		var expanded []string
		switch strings.ToLower(strings.TrimSpace(requested)) {
		case "all":
			expanded = []string{"claude", "codex", "vscode"}
		case "claude", "claude-code":
			expanded = []string{"claude"}
		case "codex":
			expanded = []string{"codex"}
		case "vscode", "vs-code", "vscode-copilot", "copilot-vscode":
			expanded = []string{"vscode"}
		case "jetbrains", "intellij", "jetbrains-copilot":
			expanded = []string{"jetbrains"}
		default:
			return nil
		}
		for _, canonical := range expanded {
			if !seen[canonical] {
				seen[canonical] = true
				out = append(out, canonical)
			}
		}
	}
	return out
}

func unknownMCPClientError(client string) error {
	return fmt.Errorf("unknown MCP client %q; expected claude, codex, vscode, jetbrains, or all", client)
}

func containsMCPClient(clients []string, target string) bool {
	for _, client := range clients {
		if client == target {
			return true
		}
	}
	return false
}

func writableMCPClients(clients []string) []string {
	out := make([]string, 0, len(clients))
	for _, client := range clients {
		if client != "jetbrains" {
			out = append(out, client)
		}
	}
	return out
}

func resolveMCPConfigCommand(command string, writing bool) (string, error) {
	if command = strings.TrimSpace(command); command != "" {
		return command, nil
	}
	if !writing {
		return "mamari", nil
	}
	if path, err := exec.LookPath("mamari"); err == nil {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("resolve mamari executable: %w", err)
		}
		return absolute, nil
	}
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve mamari executable: %w; pass --command explicitly", err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve mamari executable: %w", err)
	}
	return path, nil
}

func resolveMCPConfigCommandForRepo(command string, writing bool, repoRoot string) (string, error) {
	resolved, err := resolveMCPConfigCommand(command, writing)
	if err != nil {
		return "", err
	}
	if writing && command != "" && !filepath.IsAbs(resolved) && strings.ContainsAny(resolved, `/\`) {
		resolved = filepath.Join(repoRoot, resolved)
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return "", fmt.Errorf("resolve mamari executable: %w", err)
		}
	}
	return filepath.Clean(resolved), nil
}

func validateMCPRuntime(command, indexPath string) error {
	if _, err := mamari.LoadIndex(indexPath); err != nil {
		return fmt.Errorf("validate index %s: %w", indexPath, err)
	}
	return validateMCPCommand(command)
}

func validateMCPCommand(command string) error {
	resolvedCommand := command
	if !filepath.IsAbs(resolvedCommand) && !strings.ContainsAny(resolvedCommand, `/\`) {
		var err error
		resolvedCommand, err = exec.LookPath(resolvedCommand)
		if err != nil {
			return fmt.Errorf("mamari command %q is not available on PATH: %w", command, err)
		}
	}
	info, err := os.Stat(resolvedCommand)
	if err != nil {
		return fmt.Errorf("validate mamari command %s: %w", resolvedCommand, err)
	}
	if info.IsDir() {
		return fmt.Errorf("mamari command %s is a directory", resolvedCommand)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("mamari command %s is not executable", resolvedCommand)
	}
	return nil
}

func mcpServeArgs(indexPath string) []string {
	return []string{"serve", "--index", indexPath}
}

func printMCPConfigSnippets(indexPath, name string) {
	printMCPConfigSnippetsForClients([]string{"claude", "codex", "vscode"}, indexPath, "mamari", name)
}

func printMCPConfigSnippetsForClients(clients []string, indexPath, command, name string) {
	for _, client := range clients {
		switch client {
		case "claude":
			fmt.Println("Claude Code .mcp.json:")
			fmt.Println(claudeMCPConfigSnippet(name, command, indexPath))
		case "codex":
			fmt.Println("Codex .codex/config.toml:")
			fmt.Println(codexMCPConfigSnippet(name, command, indexPath))
		case "vscode":
			fmt.Println("VS Code .vscode/mcp.json:")
			fmt.Println(vscodeMCPConfigSnippet(name, command, indexPath))
		case "jetbrains":
			fmt.Println("JetBrains GitHub Copilot mcp.json (Copilot Chat > MCP > Add MCP Tools):")
			fmt.Println(vscodeMCPConfigSnippet(name, command, indexPath))
			fmt.Println("JetBrains AI Assistant JSON (Settings > Tools > AI Assistant > Model Context Protocol):")
			fmt.Println(claudeMCPConfigSnippet(name, command, indexPath))
		}
	}
}

func claudeMCPConfigSnippet(name, command, indexPath string) string {
	cfg := map[string]any{
		"mcpServers": map[string]any{
			name: map[string]any{
				"command": command,
				"args":    mcpServeArgs(indexPath),
			},
		},
	}
	return mustCompactJSON(cfg)
}

func vscodeMCPConfigSnippet(name, command, indexPath string) string {
	cfg := map[string]any{
		"servers": map[string]any{
			name: map[string]any{
				"type":    "stdio",
				"command": command,
				"args":    mcpServeArgs(indexPath),
			},
		},
	}
	return mustCompactJSON(cfg)
}

func codexMCPConfigSnippet(name, command, indexPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[mcp_servers.%s]\n", tomlBareKey(name))
	fmt.Fprintf(&b, "command = %s\n", strconv.Quote(command))
	fmt.Fprintf(&b, "args = [%s]\n", quotedTOMLArray(mcpServeArgs(indexPath)))
	return b.String()
}

func mustCompactJSON(v any) string {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err)
	}
	return string(data)
}

func quotedTOMLArray(values []string) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.Quote(value))
	}
	return strings.Join(parts, ", ")
}

func tomlBareKey(value string) string {
	if value == "" {
		return strconv.Quote("mamari")
	}
	for _, r := range value {
		if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return strconv.Quote(value)
		}
	}
	return value
}

func writeClaudeMCPConfig(path, name, command, indexPath string, force bool) error {
	return writeJSONMCPConfig(path, "mcpServers", name, map[string]any{
		"command": command,
		"args":    mcpServeArgs(indexPath),
	}, force)
}

func writeVSCodeMCPConfig(path, name, command, indexPath string, force bool) error {
	return writeJSONMCPConfig(path, "servers", name, map[string]any{
		"type":    "stdio",
		"command": command,
		"args":    mcpServeArgs(indexPath),
	}, force)
}

func writeJSONMCPConfig(path, rootKey, name string, server map[string]any, force bool) error {
	cfg := map[string]any{}
	if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	rootValue, rootExists := cfg[rootKey]
	root, rootIsObject := rootValue.(map[string]any)
	if rootExists && !rootIsObject {
		return fmt.Errorf("%s field %q must be a JSON object", path, rootKey)
	}
	if !rootExists {
		root = map[string]any{}
		cfg[rootKey] = root
	}
	if _, exists := root[name]; exists && !force {
		return fmt.Errorf("%s already has %s.%s; rerun with --force to replace it", path, rootKey, name)
	}
	root[name] = server
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(path, data, 0o644)
}

func writeCodexMCPConfig(path, name, command, indexPath string, force bool) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	content := string(data)
	header := "[mcp_servers." + tomlBareKey(name) + "]"
	if hasTOMLTable(content, header) {
		if !force {
			return fmt.Errorf("%s already has %s; rerun with --force to replace it", path, header)
		}
		content = removeTOMLTable(content, header)
	}
	if strings.TrimSpace(content) != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if strings.TrimSpace(content) != "" {
		content += "\n"
	}
	content += codexMCPConfigSnippet(name, command, indexPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(path, []byte(content), 0o644)
}

func preflightMCPConfigWrites(repoRoot string, clients []string, name string, force bool) error {
	for _, client := range clients {
		switch client {
		case "claude":
			if err := preflightJSONMCPConfig(filepath.Join(repoRoot, ".mcp.json"), "mcpServers", name, force); err != nil {
				return err
			}
		case "vscode":
			if err := preflightJSONMCPConfig(filepath.Join(repoRoot, ".vscode", "mcp.json"), "servers", name, force); err != nil {
				return err
			}
		case "codex":
			path := filepath.Join(repoRoot, ".codex", "config.toml")
			data, err := os.ReadFile(path)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			header := "[mcp_servers." + tomlBareKey(name) + "]"
			if hasTOMLTable(string(data), header) && !force {
				return fmt.Errorf("%s already has %s; rerun with --force or --mcp-force to replace it", path, header)
			}
		}
	}
	return nil
}

func preflightJSONMCPConfig(path, rootKey, name string, force bool) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	cfg := map[string]any{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	rootValue, rootExists := cfg[rootKey]
	root, rootIsObject := rootValue.(map[string]any)
	if rootExists && !rootIsObject {
		return fmt.Errorf("%s field %q must be a JSON object", path, rootKey)
	}
	if _, exists := root[name]; exists && !force {
		return fmt.Errorf("%s already has %s.%s; rerun with --force or --mcp-force to replace it", path, rootKey, name)
	}
	return nil
}

func writeMCPConfigs(repoRoot string, clients []string, name, command, indexPath string, force bool) ([]string, error) {
	var written []string
	for _, client := range clients {
		var path string
		var err error
		switch client {
		case "claude":
			path = filepath.Join(repoRoot, ".mcp.json")
			err = writeClaudeMCPConfig(path, name, command, indexPath, force)
		case "vscode":
			path = filepath.Join(repoRoot, ".vscode", "mcp.json")
			err = writeVSCodeMCPConfig(path, name, command, indexPath, force)
		case "codex":
			path = filepath.Join(repoRoot, ".codex", "config.toml")
			err = writeCodexMCPConfig(path, name, command, indexPath, force)
		default:
			continue
		}
		if err != nil {
			return written, err
		}
		written = append(written, path)
	}
	return written, nil
}

func writeFileAtomic(path string, data []byte, defaultMode os.FileMode) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	mode := defaultMode
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if err = tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func printMCPNextSteps(clients []string) {
	fmt.Println("next:")
	for _, client := range clients {
		switch client {
		case "claude":
			fmt.Println("  Claude Code: restart the session, approve the project server, then run /mcp")
		case "codex":
			fmt.Println("  Codex: trust the project, restart Codex, then run /mcp (or `codex mcp list`)")
		case "vscode":
			fmt.Println("  VS Code / Copilot: reload the window, trust the server, then run `MCP: List Servers`")
		case "jetbrains":
			fmt.Println("  JetBrains: paste the matching snippet for Copilot or AI Assistant, approve it, and confirm `mamari` in the tools list")
			fmt.Println("  JetBrains AI Assistant agents: also enable `Pass custom MCP servers` under Settings > Tools > AI Assistant > Agents")
		}
	}
	fmt.Println("  Test prompt: Use Mamari to map this repository and identify its main entry points.")
}

func removeTOMLTable(content, header string) string {
	lines := strings.SplitAfter(content, "\n")
	var out strings.Builder
	skipping := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == header {
			skipping = true
			continue
		}
		if skipping && strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			skipping = false
		}
		if !skipping {
			out.WriteString(line)
		}
	}
	return strings.TrimRight(out.String(), "\n") + "\n"
}

func hasTOMLTable(content, header string) bool {
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == header {
			return true
		}
	}
	return false
}

func printJSON(v any) error {
	data, err := mamari.MarshalWithRealTokenEstimate(v, false, false)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(append(data, '\n'))
	return err
}

func printInspectSymbolNode(resp mamari.InspectSymbolNodeResponse) {
	if resp.Symbol == nil {
		fmt.Printf("%s: %s\n", resp.Query, resp.Status)
		for _, warning := range resp.Warnings {
			fmt.Printf("warning: %s\n", warning)
		}
		return
	}
	sym := resp.Symbol
	fmt.Printf("## %s (%s)\n\n", sym.Name, sym.Kind)
	fmt.Printf("Location: %s:%d\n", sym.File, sym.StartLine)
	if sym.Signature != "" {
		fmt.Printf("Signature: %s\n", sym.Signature)
	}
	if sym.Docstring != "" {
		fmt.Printf("\n%s\n", sym.Docstring)
	}
	if resp.Source != "" {
		fmt.Printf("\n```%s\n%s```\n", languageFence(sym.Language), numberedSource(resp.Source, sym.StartLine))
	}
	if len(resp.Callees) > 0 || len(resp.Callers) > 0 || len(resp.Tests) > 0 {
		fmt.Println("\nTrail:")
		if len(resp.Callees) > 0 {
			fmt.Printf("Calls: %s\n", joinNodeTrail(resp.Callees))
		}
		if len(resp.Callers) > 0 {
			fmt.Printf("Called by: %s\n", joinNodeTrail(resp.Callers))
		}
		if len(resp.Tests) > 0 {
			fmt.Printf("Tests: %s\n", joinNodeTrail(resp.Tests))
		}
	}
}

func languageFence(language string) string {
	switch language {
	case "javascript":
		return "javascript"
	case "typescript", "vue":
		return "typescript"
	case "python":
		return "python"
	case "go":
		return "go"
	case "java":
		return "java"
	case "csharp":
		return "csharp"
	default:
		return ""
	}
}

func numberedSource(source string, startLine int) string {
	lines := strings.SplitAfter(source, "\n")
	var b strings.Builder
	for i, line := range lines {
		if line == "" {
			continue
		}
		fmt.Fprintf(&b, "%d\t%s", startLine+i, line)
		if !strings.HasSuffix(line, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func joinNodeTrail(symbols []mamari.CGPSymbolSummary) string {
	parts := make([]string, 0, len(symbols))
	for _, sym := range symbols {
		if sym.Count > 0 {
			label := fmt.Sprintf("%s (%s:%d, %d sites)", sym.Name, sym.File, sym.StartLine, sym.Count)
			if sym.Truncated {
				label += "+"
			}
			parts = append(parts, label)
			continue
		}
		parts = append(parts, fmt.Sprintf("%s (%s:%d)", sym.Name, sym.File, sym.StartLine))
	}
	return strings.Join(parts, ", ")
}

func printTrace(resp mamari.TraceResponse) {
	fmt.Printf("%s: %s\n", resp.Query, resp.Status)
	if resp.Term != nil {
		fmt.Printf("term: %s <%s>\n", resp.Term.Term, resp.Term.IRI)
	}
	if len(resp.Candidates) > 0 {
		fmt.Println("candidates:")
		for _, candidate := range resp.Candidates {
			fmt.Printf("- %s <%s>\n", candidate.Term, candidate.IRI)
		}
	}
	if len(resp.TTLUsages) > 0 {
		fmt.Println("ttl:")
		for _, loc := range resp.TTLUsages {
			fmt.Printf("- %s:%d:%d %s\n", loc.File, loc.StartLine, loc.StartColumn, loc.Kind)
		}
	}
	if len(resp.CodeReferences) > 0 {
		fmt.Println("code:")
		for _, ref := range resp.CodeReferences {
			fmt.Printf("- %s:%d:%d [%s] %s\n", ref.File, ref.StartLine, ref.StartColumn, ref.Confidence, ref.Context)
		}
	}
}

func printCompactTrace(resp mamari.TraceCompactResponse) {
	fmt.Printf("%s: %s\n", resp.Query, resp.Status)
	if resp.Term != nil {
		fmt.Printf("term: %s <%s>\n", resp.Term.Term, resp.Term.IRI)
	}
	fmt.Printf("ttl=%d refs=%d edges=%d dynamicIris=%d\n", resp.TTLUsageCount, resp.CodeReferenceCount, resp.EdgeCount, resp.DynamicIRICallCount)
	for _, loc := range resp.TTLUsages {
		fmt.Printf("- ttl %s:%d:%d %s\n", loc.File, loc.StartLine, loc.StartColumn, loc.Kind)
	}
	for _, ref := range resp.CodeReferences {
		fmt.Printf("- ref %s:%d:%d [%s] %s\n", ref.File, ref.StartLine, ref.StartColumn, ref.Confidence, ref.Kind)
	}
}

func printGroupedCompactTrace(resp mamari.TraceGroupedCompactResponse) {
	fmt.Printf("%s: %s\n", resp.Query, resp.Status)
	if resp.Term != nil {
		fmt.Printf("term: %s <%s>\n", resp.Term.Term, resp.Term.IRI)
	}
	fmt.Printf("ttl=%d refs=%d edges=%d dynamicIris=%d\n", resp.TTLUsageCount, resp.CodeReferenceCount, resp.EdgeCount, resp.DynamicIRICallCount)
	for file, locations := range resp.TTLUsages {
		for _, loc := range locations {
			fmt.Printf("- ttl %s:%d:%d %s\n", file, loc.Line, loc.Column, loc.Kind)
		}
	}
	for file, refs := range resp.CodeReferences {
		for _, ref := range refs {
			fmt.Printf("- ref %s:%d:%d [%s] %s\n", file, ref.Line, ref.Column, ref.Confidence, ref.Kind)
		}
	}
}

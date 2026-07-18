package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/waelhoury/mamari/internal/mamari"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type ServeOptions struct {
	// ServerVersion is reported in the MCP initialize response. Release
	// binaries pass their linker-injected version; direct package users fall
	// back to "dev".
	ServerVersion string
	Watch         bool
	Debounce      time.Duration
	Persist       bool
	// LinkedIndexes are paths to other repos' index.json files, loaded
	// read-only alongside the primary index so cross-repo tools like
	// find_route can resolve references that cross repo boundaries.
	LinkedIndexes []string
	// FullToolset registers every MCP tool unconditionally, including ones
	// that are normally hidden because the loaded index/session has no
	// content they could ever act on (TTL-specific tools on a non-TTL repo,
	// trace_event/list_events with no recorded event-bus edges,
	// cross-repo tools with no --link) or that are rarely needed admin
	// tools (manage_notes, manage_adr, diff_index). Off by default: the
	// default registers fewer tools, which directly shrinks the fixed
	// tools/list schema cost every session pays regardless of use. Pass true to get the full
	// tool surface back (e.g. for sessions doing notes/ADR bookkeeping or
	// PR-diff analysis).
	FullToolset bool
	// Toolset controls the MCP tools/list surface. "slim" (the default)
	// registers one primary router tool named "mamari" and keeps the full
	// code-intelligence power behind an args_json payload. "adaptive"
	// registers the historical named tools, gated by repo capabilities.
	// "full" registers every named tool.
	Toolset string
	// MemoryLimitBytes configures Go's soft runtime heap limit for the
	// long-running server. A negative value selects Mamari's index-size
	// based default, zero leaves the runtime/GOMEMLIMIT unchanged, and a
	// positive value is used verbatim.
	MemoryLimitBytes int64
}

func Serve(indexPath string) error {
	return ServeWithOptions(indexPath, ServeOptions{
		Watch:            true,
		MemoryLimitBytes: -1,
	})
}

func ServeWithOptions(indexPath string, opts ServeOptions) error {
	restoreMemoryLimit := applyServerMemoryLimit(indexPath, opts.LinkedIndexes, opts.MemoryLimitBytes)
	defer restoreMemoryLimit()

	idx, err := mamari.LoadIndex(indexPath)
	if err != nil {
		return err
	}
	var linked []mamari.LinkedRepo
	for _, p := range opts.LinkedIndexes {
		l, err := mamari.LoadIndex(p)
		if err != nil {
			return fmt.Errorf("load linked index %q: %w", p, err)
		}
		l.CompactReadOnly()
		linked = append(linked, mamari.LinkedRepo{Index: l})
	}
	// Reclaim decode scratch only after every linked index has loaded. Doing
	// this after the primary index but before linked indexes left the latter's
	// gob scratch resident for the lifetime of the server.
	idx.ReleaseUnusedMemory()
	s := newMCPServer(idx, linked, opts)
	if opts.Watch {
		capabilities := currentAdaptiveCapabilities(idx)
		watchReady := make(chan struct{})
		watchStopped := make(chan error, 1)
		go func() {
			err := mamari.Watch(context.Background(), idx, mamari.WatchOptions{
				Debounce: opts.Debounce,
				OnReady: func() {
					close(watchReady)
				},
				OnRebake: func(updated, removed []string) {
					if opts.Persist {
						if err := mamari.SaveIndex(idx, indexPath); err != nil {
							fmt.Fprintln(os.Stderr, "mamari serve watch: save error:", err)
						}
					}
					next := currentAdaptiveCapabilities(idx)
					if next != capabilities {
						refreshAdaptiveTools(s, idx, linked, opts)
						capabilities = next
					}
				},
				OnError: func(err error) {
					fmt.Fprintln(os.Stderr, "mamari serve watch:", err)
				},
			})
			if err != nil {
				fmt.Fprintln(os.Stderr, "mamari serve watch:", err)
			}
			watchStopped <- err
		}()
		// Do not accept MCP requests until fsnotify has registered the repo
		// directories. Without this short handshake, an editor can save during
		// initialize and lose that event before the watcher goroutine starts.
		// A watch setup failure preserves historical behavior: log it and keep
		// serving the immutable loaded index.
		select {
		case <-watchReady:
		case <-watchStopped:
		}
	}
	return server.ServeStdio(s)
}

const (
	defaultServerMemoryFloor = int64(224 << 20)
	v2IndexMemoryMultiplier  = int64(5)
)

func applyServerMemoryLimit(indexPath string, linked []string, requested int64) func() {
	if requested == 0 || (requested < 0 && os.Getenv("GOMEMLIMIT") != "") {
		return func() {}
	}
	limit := requested
	if limit < 0 {
		limit = automaticServerMemoryLimit(append([]string{indexPath}, linked...))
	}
	previous := debug.SetMemoryLimit(limit)
	return func() {
		debug.SetMemoryLimit(previous)
	}
}

func automaticServerMemoryLimit(indexPaths []string) int64 {
	limit := defaultServerMemoryFloor
	for _, path := range indexPaths {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			size := info.Size()
			// v2's global string table and numeric records make the on-disk
			// index roughly 7x smaller than v1, so treating one compressed byte
			// as one heap byte creates an unrealistically low GOMEMLIMIT and
			// makes cold search spend most of its time in emergency GC. The
			// multiplier covers the measured warmed graph/search cache while
			// retaining the 224 MiB floor for small repositories. Legacy gob,
			// JSON, and unknown formats keep the historical 1x rule.
			if isV2IndexFile(path) && size <= (1<<63-1)/v2IndexMemoryMultiplier {
				size *= v2IndexMemoryMultiplier
			}
			limit += size
		}
	}
	return limit
}

func isV2IndexFile(path string) bool {
	const magic = "mamari-index-v2\n"
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	header := make([]byte, len(magic))
	if _, err := io.ReadFull(file, header); err != nil {
		return false
	}
	return string(header) == magic
}

// newMCPServer builds the MCP server and registers tools against idx and
// linked, without starting the stdio transport. Split out from
// ServeWithOptions so tests can drive it with an in-process MCP client.
//
// Which tools get registered depends on opts.FullToolset and on what the
// loaded index/session contains — see ServeOptions.FullToolset's doc
// comment. Adaptive watch sessions refresh this set after a rebake changes
// the index capabilities and emit MCP's tools/list_changed notification.
func newMCPServer(idx *mamari.Index, linked []mamari.LinkedRepo, opts ServeOptions) *server.MCPServer {
	toolset := normalizeToolset(opts)
	serverVersion := strings.TrimSpace(opts.ServerVersion)
	if serverVersion == "" {
		serverVersion = "dev"
	}
	full := toolset == "full"
	hasTTL := full || idx.HasTTLContent()
	hasEvents := full || idx.HasEventEdges()
	hasLinked := full || len(linked) > 0
	hasWatch := full || opts.Watch
	s := server.NewMCPServer(
		"mamari",
		serverVersion,
		server.WithToolCapabilities(toolset == "adaptive" && opts.Watch),
		server.WithRecovery(),
	)
	addPrimaryTool(s, idx)
	if toolset == "slim" {
		return s
	}
	if hasTTL {
		s.AddTool(mcp.NewTool("trace_term",
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithDescription("Return TTL usages, code references, and graph edges for an RDF term. The format parameter controls response size: full (all evidence, the default), compact (locations only — use this to save tokens, then fetch_source for exact slices), grouped (compact, grouped by file — the smallest shape for agent planning)."),
			mcp.WithString("term", mcp.Required(), mcp.Description("Prefixed term, local name, or full IRI.")),
			mcp.WithString("format", mcp.Description("Response shape: full|compact|grouped. Defaults to full."), mcp.Enum("full", "compact", "grouped")),
			mcp.WithBoolean("include_weak", mcp.Description("Include weak local-name references (noisy for short/common locals). Defaults to false.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			term, err := req.RequireString("term")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			opts := mamari.QueryOptions{IncludeWeak: req.GetBool("include_weak", false)}
			switch format := req.GetString("format", "full"); format {
			case "full":
				return jsonResult(mamari.TraceTerm(idx, term, opts)), nil
			case "compact":
				return jsonResult(mamari.TraceTermCompact(idx, term, opts)), nil
			case "grouped":
				return jsonResult(mamari.TraceTermGroupedCompact(idx, term, opts)), nil
			default:
				return mcp.NewToolResultError(fmt.Sprintf("format must be one of full|compact|grouped, got %q", format)), nil
			}
		})
		s.AddTool(mcp.NewTool("inspect_term",
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithDescription("Bridge an RDF term to likely implementation code in one compact workflow: grouped term refs, derived identifier hits, and budgeted source context. Use this when trace_term with format: \"grouped\" finds the term but you still need the code path."),
			mcp.WithString("term", mcp.Required(), mcp.Description("Prefixed term, local name, or full IRI.")),
			mcp.WithNumber("budget", mcp.Description("Estimated source-context token budget. Defaults to 1200.")),
			mcp.WithNumber("context_lines", mcp.Description("Line context around implementation evidence. Defaults to 6.")),
			mcp.WithString("mode", mcp.Description("Context mode: compact|evidence|context|full. Defaults to context.")),
			mcp.WithBoolean("include_weak", mcp.Description("Include weak local-name references. Defaults to false.")),
			mcp.WithNumber("limit", mcp.Description("Maximum implementation evidence candidates. Defaults to 8.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			term, err := req.RequireString("term")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			resp, err := mamari.InspectTerm(idx, term, mamari.InspectTermOptions{
				BudgetTokens: int(req.GetFloat("budget", 1200)),
				ContextLines: int(req.GetFloat("context_lines", 6)),
				Mode:         req.GetString("mode", ""),
				IncludeWeak:  req.GetBool("include_weak", false),
				Limit:        int(req.GetFloat("limit", 8)),
			})
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return jsonResult(resp), nil
		})
	}
	s.AddTool(mcp.NewTool("find_references",
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithDescription("Return code references for an RDF term. If no RDF term matches, falls back to the CGP symbol graph (like trace_symbol) so plain code identifiers (e.g. function/component names) also resolve."),
		mcp.WithString("term", mcp.Required(), mcp.Description("Prefixed term, local name, full IRI, or a code symbol/identifier name.")),
		mcp.WithBoolean("include_weak", mcp.Description("Include weak local-name references. Defaults to false.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		term, err := req.RequireString("term")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		opts := mamari.QueryOptions{IncludeWeak: req.GetBool("include_weak", false)}
		return jsonResult(mamari.FindReferences(idx, term, opts)), nil
	})
	if hasTTL {
		s.AddTool(mcp.NewTool("list_terms",
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithDescription("List indexed RDF terms, optionally filtered by prefix."),
			mcp.WithString("prefix", mcp.Description("Optional RDF prefix.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return jsonResult(mamari.ListTerms(idx, req.GetString("prefix", ""))), nil
		})
	}
	s.AddTool(mcp.NewTool("list_symbols",
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithDescription("List CGP symbols from indexed source languages, including Terraform/OpenTofu HCL, optionally filtered by query, kind, or language."),
		mcp.WithString("query", mcp.Description("Optional symbol name or id substring.")),
		mcp.WithString("kind", mcp.Description("Optional symbol kind, e.g. function, class, component, terraform-resource, ttl-shape.")),
		mcp.WithString("kinds", mcp.Description("Optional comma-separated symbol kinds to include.")),
		mcp.WithString("lang", mcp.Description("Optional language, e.g. typescript, javascript, vue, python, hcl, ttl.")),
		mcp.WithNumber("limit", mcp.Description("Optional maximum number of symbols to return. Use this for token-bounded discovery.")),
		mcp.WithBoolean("source_only", mcp.Description("Exclude tests and stories unless explicitly included.")),
		mcp.WithBoolean("include_tests", mcp.Description("Include test symbols when source_only is true.")),
		mcp.WithBoolean("include_stories", mcp.Description("Include story symbols when source_only is true.")),
		mcp.WithBoolean("scores", mcp.Description("Include relevance scores.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return jsonResult(mamari.ListSymbolsWithOptions(idx, req.GetString("query", ""), req.GetString("kind", ""), req.GetString("lang", ""), mamari.ListSymbolsOptions{
			Limit:          int(req.GetFloat("limit", 0)),
			Kinds:          splitCSV(req.GetString("kinds", "")),
			SourceOnly:     req.GetBool("source_only", false),
			IncludeTests:   req.GetBool("include_tests", false),
			IncludeStories: req.GetBool("include_stories", false),
			WithScores:     req.GetBool("scores", false),
		})), nil
	})
	s.AddTool(mcp.NewTool("trace_symbol",
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithDescription("Trace a CGP symbol by id or name, returning compact callers, callees, imports, caller sites, and confidence counts. Full raw edges are opt-in."),
		mcp.WithString("query", mcp.Required(), mcp.Description("Symbol id, exact name, file:name, or substring.")),
		mcp.WithBoolean("with_edges", mcp.Description("Include full raw CGP edge objects. Defaults to false.")),
		mcp.WithBoolean("sites", mcp.Description("Include compact caller-site evidence. Defaults to true. Always omitted when compact=true, regardless of this flag — see compact's description.")),
		mcp.WithBoolean("include_test_details", mcp.Description("Return individual test callback callers instead of grouped test files. Defaults to false.")),
		mcp.WithBoolean("exclude_tests", mcp.Description("Omit test callers and test caller sites. Defaults to false.")),
		mcp.WithBoolean("compact", mcp.Description("Drop signature/docstring/return-types/hot-path/id/language fields and per-call-site evidence, keeping only name/kind/file/startLine for each caller/callee — substantially smaller responses when a symbol has many callers. Defaults to false.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, err := req.RequireString("query")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(mamari.TraceSymbolWithOptions(idx, query, mamari.TraceSymbolOptions{
			WithEdges:          req.GetBool("with_edges", false),
			Sites:              req.GetBool("sites", true),
			IncludeTestDetails: req.GetBool("include_test_details", false),
			ExcludeTests:       req.GetBool("exclude_tests", false),
			Compact:            req.GetBool("compact", false),
		})), nil
	})
	s.AddTool(mcp.NewTool("inspect_symbol",
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithDescription("Return a deterministic packet for one symbol. format=context returns trace, caller sites, bounded source context, frontend UI edges, and notes. format=node returns a compact source/docstring/signature/caller/callee view for quick symbol reading."),
		mcp.WithString("query", mcp.Required(), mcp.Description("Symbol id, exact name, or file:name.")),
		mcp.WithString("format", mcp.Description("Response shape: context|node. Defaults to context."), mcp.Enum("context", "node")),
		mcp.WithNumber("budget", mcp.Description("Estimated source-context token budget. Defaults to 1800.")),
		mcp.WithNumber("context_lines", mcp.Description("Line context around caller sites. Defaults to 3.")),
		mcp.WithString("mode", mcp.Description("Context mode: compact|evidence|context|full. Defaults to context.")),
		mcp.WithBoolean("include_tests", mcp.Description("Include test callers and test caller sites. Defaults to false.")),
		mcp.WithBoolean("include_test_details", mcp.Description("Return individual test callback callers instead of grouped test files. Defaults to false.")),
		mcp.WithBoolean("with_edges", mcp.Description("Include full raw CGP edge objects in the trace. Defaults to false.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, err := req.RequireString("query")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if req.GetString("format", "context") == "node" {
			resp, err := mamari.InspectSymbolNode(idx, query, mamari.InspectSymbolNodeOptions{
				BudgetTokens:       int(req.GetFloat("budget", 900)),
				IncludeTests:       req.GetBool("include_tests", false),
				IncludeTestDetails: req.GetBool("include_test_details", false),
			})
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return jsonResult(resp), nil
		}
		resp, err := mamari.InspectSymbol(idx, query, mamari.InspectSymbolOptions{
			BudgetTokens:       int(req.GetFloat("budget", 1800)),
			ContextLines:       int(req.GetFloat("context_lines", 3)),
			Mode:               req.GetString("mode", ""),
			IncludeTests:       req.GetBool("include_tests", false),
			IncludeTestDetails: req.GetBool("include_test_details", false),
			WithEdges:          req.GetBool("with_edges", false),
		})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(resp), nil
	})
	s.AddTool(mcp.NewTool("fetch_context",
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithDescription("Return budgeted source context for a symbol id/name, file:line, or RDF term. Use this after compact graph queries to get exact bounded code. The mode parameter controls how much source text travels back: compact (no text, just locations), evidence (single line per slice), context (default, adaptive merged slices), full (context plus auto-included callers and callees)."),
		mcp.WithString("query", mcp.Required(), mcp.Description("Symbol id/name, file:line, file:line:column, prefixed RDF term, or full IRI.")),
		mcp.WithNumber("budget", mcp.Description("Estimated token budget. Defaults to 1200.")),
		mcp.WithNumber("context_lines", mcp.Description("Line context around file:line and term evidence. Defaults to 8.")),
		mcp.WithBoolean("callers", mcp.Description("Include caller signature slices. Defaults to false.")),
		mcp.WithBoolean("callees", mcp.Description("Include callee signature slices. Defaults to false.")),
		mcp.WithString("mode", mcp.Description("Response mode: compact|evidence|context|full. Defaults to context. Prefer compact for graph-only triage.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, err := req.RequireString("query")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		budget := int(req.GetFloat("budget", 1200))
		contextLines := int(req.GetFloat("context_lines", 8))
		resp, err := mamari.FetchContext(idx, query, mamari.FetchContextOptions{
			BudgetTokens:   budget,
			ContextLines:   contextLines,
			IncludeCallers: req.GetBool("callers", false),
			IncludeCallees: req.GetBool("callees", false),
			Mode:           req.GetString("mode", ""),
		})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(resp), nil
	})
	if hasTTL {
		s.AddTool(mcp.NewTool("search_literal",
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithDescription("Search indexed TTL string literals (sh:name, sh:message, sh:description, rdfs:label, etc.) by substring. Use this for German/English labels, validation messages, and other free-text content that is not an RDF term."),
			mcp.WithString("query", mcp.Required(), mcp.Description("Substring to search (case-insensitive).")),
			mcp.WithString("lang", mcp.Description("Optional language tag filter, e.g. 'de' or 'en'.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			query, err := req.RequireString("query")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return jsonResult(mamari.SearchLiteral(idx, query, req.GetString("lang", ""))), nil
		})
	}
	s.AddTool(mcp.NewTool("search_code",
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithDescription("Rank small source evidence snippets for a natural-language code question. Use this as the first cheap discovery step before inspect_symbol or fetch_context; it returns bounded snippets, matched terms, nearby symbols, and estimated token counts instead of whole files."),
		mcp.WithString("query", mcp.Required(), mcp.Description("Natural-language task or keywords.")),
		mcp.WithNumber("limit", mcp.Description("Maximum hits to return. Defaults to 10.")),
		mcp.WithNumber("budget", mcp.Description("Estimated token budget for the complete serialized response. Defaults to 1200.")),
		mcp.WithNumber("context_lines", mcp.Description("Line context around each hit. Defaults to 1.")),
		mcp.WithBoolean("source_only", mcp.Description("Exclude tests and stories unless explicitly included. Defaults to true.")),
		mcp.WithBoolean("include_tests", mcp.Description("Include test files when source_only is true.")),
		mcp.WithBoolean("include_stories", mcp.Description("Include story files when source_only is true.")),
		mcp.WithBoolean("exact_first", mcp.Description("When exact literals are detected, return only exact matches if any exist.")),
		mcp.WithBoolean("prefer_definitions", mcp.Description("Rank definition lines above usage sites.")),
		mcp.WithBoolean("prefer_usages", mcp.Description("Rank usage sites above definition lines.")),
		mcp.WithString("mode", mcp.Description("Response mode: compact|evidence|context. Defaults to context.")),
		mcp.WithBoolean("symbol_detail", mcp.Description("Include each hit's full containing-symbol signature/docstring/return-types/hot-path fields instead of just name/kind/file/startLine. Defaults to false — a hit already carries its own matched source text, so the full symbol detail is rarely needed and was the largest single contributor to response size.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, err := req.RequireString("query")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		budget := int(req.GetFloat("budget", 1200))
		resp := mamari.SearchCode(idx, query, mamari.SearchCodeOptions{
			Limit:             int(req.GetFloat("limit", 10)),
			BudgetTokens:      budget,
			ContextLines:      int(req.GetFloat("context_lines", 1)),
			SourceOnly:        req.GetBool("source_only", true),
			IncludeTests:      req.GetBool("include_tests", false),
			IncludeStories:    req.GetBool("include_stories", false),
			ExactFirst:        req.GetBool("exact_first", false),
			PreferDefinitions: req.GetBool("prefer_definitions", false),
			PreferUsages:      req.GetBool("prefer_usages", false),
			Mode:              req.GetString("mode", ""),
			SymbolDetail:      req.GetBool("symbol_detail", false),
		})
		mamari.FitSearchCodeResponse(&resp, budget)
		return jsonResult(resp), nil
	})
	s.AddTool(mcp.NewTool("semantic_query",
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithDescription("Find conceptually related symbols with Mamari's local vector index. Use when query and code may use different vocabulary (for example send vs publish, persist vs save). Combines corpus co-occurrence, software concepts, source metadata, and call-graph diffusion; no API key or external service."),
		mcp.WithString("query", mcp.Required(), mcp.Description("Natural-language behavior or concepts to find.")),
		mcp.WithNumber("limit", mcp.Description("Maximum symbols to return. Defaults to 10.")),
		mcp.WithNumber("min_score", mcp.Description("Minimum hybrid semantic score from 0 to 1. Defaults to 0.40.")),
		mcp.WithBoolean("source_only", mcp.Description("Exclude tests and stories unless explicitly included. Defaults to true.")),
		mcp.WithBoolean("include_tests", mcp.Description("Include test files when source_only is true.")),
		mcp.WithBoolean("include_stories", mcp.Description("Include story files when source_only is true.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, err := req.RequireString("query")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(mamari.SemanticQuery(idx, query, mamari.SemanticQueryOptions{
			Limit:          int(req.GetFloat("limit", 10)),
			MinScore:       req.GetFloat("min_score", 0.40),
			SourceOnly:     req.GetBool("source_only", true),
			IncludeTests:   req.GetBool("include_tests", false),
			IncludeStories: req.GetBool("include_stories", false),
		})), nil
	})
	s.AddTool(mcp.NewTool("inspect_exact",
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithDescription("Return a compact exact-evidence bundle for queries with rare literals, route paths, MIME types, RDF predicates, or long identifiers. Use this instead of chaining search_code, inspect_symbol, and fetch_context when the user already has exact strings."),
		mcp.WithString("query", mcp.Required(), mcp.Description("Natural-language task containing exact identifiers, routes, MIME types, predicates, or quoted literals.")),
		mcp.WithNumber("limit", mcp.Description("Maximum clusters to return. Defaults to 8.")),
		mcp.WithNumber("context_lines", mcp.Description("Optional source context around exact evidence lines. Defaults to 0.")),
		mcp.WithBoolean("with_source", mcp.Description("Include full containing symbol source where available. Defaults to false.")),
		mcp.WithBoolean("source_only", mcp.Description("Exclude tests and stories unless explicitly included. Defaults to true.")),
		mcp.WithBoolean("include_tests", mcp.Description("Include test files when source_only is true.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, err := req.RequireString("query")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(mamari.InspectExact(idx, query, mamari.InspectExactOptions{
			Limit:        int(req.GetFloat("limit", 8)),
			ContextLines: int(req.GetFloat("context_lines", 0)),
			WithSource:   req.GetBool("with_source", false),
			SourceOnly:   req.GetBool("source_only", true),
			IncludeTests: req.GetBool("include_tests", false),
		})), nil
	})
	s.AddTool(mcp.NewTool("inspect_flow",
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithDescription("Investigate a vague behavior question in one deterministic workflow: natural-language code search, compact symbol traces, and one merged fetch_context packet. Use this before manually chaining search_code, trace_symbol, and fetch_context."),
		mcp.WithString("query", mcp.Required(), mcp.Description("Natural-language behavior or implementation question.")),
		mcp.WithNumber("limit", mcp.Description("Maximum discovery hits/symbols. Defaults to 6.")),
		mcp.WithNumber("budget", mcp.Description("Estimated total token budget. Defaults to 1800.")),
		mcp.WithNumber("search_budget", mcp.Description("Estimated token budget for discovery evidence. Defaults to 700.")),
		mcp.WithNumber("context_lines", mcp.Description("Line context around merged evidence. Defaults to 8.")),
		mcp.WithNumber("search_context_lines", mcp.Description("Line context around search hits. Defaults to 1.")),
		mcp.WithString("mode", mcp.Description("Context mode: compact|evidence|context|full. Defaults to context.")),
		mcp.WithBoolean("source_only", mcp.Description("Exclude tests and stories unless explicitly included. Defaults to true.")),
		mcp.WithBoolean("include_tests", mcp.Description("Include test files when source_only is true.")),
		mcp.WithBoolean("include_stories", mcp.Description("Include story files when source_only is true.")),
		mcp.WithBoolean("callers", mcp.Description("Include caller source slices for traced symbols.")),
		mcp.WithBoolean("callees", mcp.Description("Include callee source slices for traced symbols.")),
		mcp.WithBoolean("traces", mcp.Description("Include full trace_symbol payloads. Defaults to false; traced symbol summaries are still returned.")),
		mcp.WithBoolean("search_symbols", mcp.Description("Include symbol summaries on each search hit. Defaults to false; inspect_flow still uses them internally.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, err := req.RequireString("query")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		resp, err := mamari.InspectFlow(idx, query, mamari.InspectFlowOptions{
			Limit:                int(req.GetFloat("limit", 6)),
			BudgetTokens:         int(req.GetFloat("budget", 1800)),
			SearchBudgetTokens:   int(req.GetFloat("search_budget", 700)),
			ContextLines:         int(req.GetFloat("context_lines", 8)),
			SearchContextLines:   int(req.GetFloat("search_context_lines", 1)),
			Mode:                 req.GetString("mode", ""),
			SourceOnly:           req.GetBool("source_only", true),
			IncludeTests:         req.GetBool("include_tests", false),
			IncludeStories:       req.GetBool("include_stories", false),
			IncludeCallers:       req.GetBool("callers", false),
			IncludeCallees:       req.GetBool("callees", false),
			IncludeTraces:        req.GetBool("traces", false),
			IncludeSearchSymbols: req.GetBool("search_symbols", false),
		})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(resp), nil
	})
	s.AddTool(mcp.NewTool("repo_map",
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithDescription("Return a budgeted architecture overview plus a PageRank-style map of important files and symbols, optionally personalized by a query or mentioned identifiers. The architecture packet includes languages, packages, entry points, routes, hotspots, connectivity-refined weighted graph communities, typed coupling, and cross-community boundaries. Each file also includes historical coChangedFiles."),
		mcp.WithString("query", mcp.Description("Optional natural-language focus, e.g. 'auth login token refresh'. Significant terms are extracted and used to personalize ranking toward matching files/symbols.")),
		mcp.WithString("mentioned", mcp.Description("Comma-separated identifiers, paths, or terms to personalize ranking.")),
		mcp.WithNumber("budget", mcp.Description("Estimated token budget. Defaults to 1200.")),
		mcp.WithNumber("limit", mcp.Description("Maximum files and symbols to return. Defaults to 40.")),
		mcp.WithBoolean("source_only", mcp.Description("Exclude tests and stories unless explicitly included. Defaults to true.")),
		mcp.WithBoolean("include_tests", mcp.Description("Include test files when source_only is true.")),
		mcp.WithBoolean("architecture", mcp.Description("Include the compact architecture packet. Defaults to true.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return jsonResult(mamari.RepoMap(idx, mamari.RepoMapOptions{
			BudgetTokens:        int(req.GetFloat("budget", 1200)),
			Limit:               int(req.GetFloat("limit", 40)),
			Mentioned:           splitCSV(req.GetString("mentioned", "")),
			Query:               req.GetString("query", ""),
			SourceOnly:          req.GetBool("source_only", true),
			IncludeTests:        req.GetBool("include_tests", false),
			IncludeArchitecture: req.GetBool("architecture", true),
		})), nil
	})
	if hasTTL {
		s.AddTool(mcp.NewTool("find_containing_shape",
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithDescription("Find SHACL shapes (sh:NodeShape) that reference the given term/IRI as their subject, sh:path, sh:targetClass, sh:node, or as the sh:datatype/sh:path inside an sh:or / sh:xone branch. Use this to locate the parent shape for a property or to navigate from a child branch back to the wrapping property."),
			mcp.WithString("query", mcp.Required(), mcp.Description("Prefixed term or full IRI.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			query, err := req.RequireString("query")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return jsonResult(mamari.FindContainingShape(idx, query)), nil
		})
		s.AddTool(mcp.NewTool("list_dynamic_iris",
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithDescription("List unresolved namedNode(...) call sites where mamari cannot statically resolve the IRI (variable/computed argument). Use this to know where flow analysis or fetch_source is required."),
			mcp.WithString("file", mcp.Description("Optional repo-relative file path to filter by.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return jsonResult(mamari.ListDynamicIRIs(idx, req.GetString("file", ""))), nil
		})
	}
	if hasWatch {
		s.AddTool(mcp.NewTool("changed_since",
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithDescription("Return files and symbols affected by watch-mode rebakes since a given sequence number. Pass since=0 to learn the current latest sequence as a baseline, then poll with that sequence next. If missedHistory is true, the journal evicted entries you cared about and you should rebaseline."),
			mcp.WithNumber("since", mcp.Description("Last seen journal sequence. Defaults to 0.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			since := uint64(req.GetFloat("since", 0))
			return jsonResult(mamari.ChangedSince(idx, since)), nil
		})
	}
	s.AddTool(mcp.NewTool("impact",
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithDescription("Return the reverse caller closure of a symbol, layer by layer, up to depth N. Use this for change-impact analysis: 'if I edit X, what touches it?'. Each impacted symbol carries a pathConfidence (worst-case along the chain) so you can filter to high-trust impact only. Also includes coChangedFiles: files historically modified in the same git commit as the target's file — a 'you might also need to touch these' signal that the static call graph can't see (e.g. config/migration pairs)."),
		mcp.WithString("query", mcp.Required(), mcp.Description("Symbol id, exact name, file:name, or substring.")),
		mcp.WithNumber("depth", mcp.Description("Reverse traversal depth. Default 2.")),
		mcp.WithBoolean("compact", mcp.Description("Drop signature/docstring/return-types/hot-path/id/language fields from each layer entry, keeping only name/kind/file/startLine plus pathConfidence/pathReason — substantially smaller responses on a wide blast radius. Defaults to false.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, err := req.RequireString("query")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		depth := int(req.GetFloat("depth", 2))
		compact := req.GetBool("compact", false)
		return jsonResult(mamari.ImpactWithOptions(idx, query, mamari.ImpactOptions{Depth: depth, Compact: compact})), nil
	})
	if hasEvents {
		s.AddTool(mcp.NewTool("trace_event",
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithDescription("Trace an event-bus event by name. Returns every emit, on/once, and off site recorded in the index, attributed to its containing symbol. Use this for Node EventEmitter / mitt / Vue $emit-style flows. The query may be the bare event name (\"FOO_BAR\"), the qualified target (\"event:FOO_BAR\"), or the dotted constant path (\"APP_EVENTS.FOO_BAR\")."),
			mcp.WithString("event", mcp.Required(), mcp.Description("Event key — string-literal value, qualified constant path, or full event:* target.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			event, err := req.RequireString("event")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return jsonResult(mamari.TraceEvent(idx, event)), nil
		})
		s.AddTool(mcp.NewTool("list_events",
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithDescription("List every event key seen in the index with emit/listen/remove site counts. Useful for discovery before trace_event."),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return jsonResult(mamari.ListEvents(idx)), nil
		})
	}
	s.AddTool(mcp.NewTool("doctor",
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithDescription("Return an index health report: parse failures, unresolved-edge breakdown, ignored patterns, and staleness against current git HEAD. Use this first when graph results look unexpectedly empty or noisy."),
		mcp.WithNumber("parse_failure_limit", mcp.Description("Maximum per-file parse-failure examples. Defaults to 10; total and truncation metadata are always returned. Use 0 for all.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return jsonResult(mamari.LimitDoctorParseFailures(
			mamari.Doctor(idx),
			int(req.GetFloat("parse_failure_limit", 10)),
		)), nil
	})
	s.AddTool(mcp.NewTool("fetch_source",
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithDescription("Return exact raw source text for an indexed file line range."),
		mcp.WithString("filepath", mcp.Required(), mcp.Description("Repo-relative file path.")),
		mcp.WithNumber("start_line", mcp.Required(), mcp.Description("1-based start line.")),
		mcp.WithNumber("end_line", mcp.Required(), mcp.Description("1-based end line.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		file, err := req.RequireString("filepath")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		start, err := req.RequireFloat("start_line")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		end, err := req.RequireFloat("end_line")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		resp, err := mamari.FetchSource(idx, file, int(start), int(end))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(resp), nil
	})
	if full {
		s.AddTool(mcp.NewTool("diff_index",
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithDescription("Structural diff between a saved baseline index file and the currently loaded index: symbols added/removed/changed and edges added/removed. Use this to summarize the structural impact of a PR — e.g. save a baseline with `mamari index -index /tmp/base.json` at the merge-base, then diff it against the live index."),
			mcp.WithString("base_index", mcp.Required(), mcp.Description("Path to a previously saved index.json file to use as the diff baseline.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			basePath, err := req.RequireString("base_index")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			base, err := mamari.LoadIndex(basePath)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("load base_index: %v", err)), nil
			}
			return jsonResult(mamari.DiffIndex(base, idx)), nil
		})
	}
	s.AddTool(mcp.NewTool("query_graph",
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithDescription("Run a deliberately restricted, Cypher-like query against the symbol graph for ad-hoc multi-hop/property/aggregate questions the fixed-shape tools don't cover (e.g. hot-path hotspots, transitive-reachability questions like \"everything this function can affect within 3 calls\", or counts/sums per file). This is NOT full Cypher — only this exact grammar is supported:\n"+
			"MATCH (a:Label)[-[:EDGE_TYPE[*[minHops][..[maxHops]]]]->(b:Label)]*\n"+
			"[WHERE <var>.<field> <op> <value> [AND <var>.<field> <op> <value>]*]\n"+
			"RETURN <item>[, <item>...]   where <item> is <var>.<field> | COUNT(*) | COUNT/SUM/AVG/MIN/MAX/COLLECT(<var>.<field>)\n"+
			"[ORDER BY <item> [ASC|DESC]]\n"+
			"[LIMIT n]\n"+
			"Chained, multi-hop patterns are supported (any number of `-[:TYPE]->(node)` segments, e.g. (a)-[:calls]->(b)-[:calls]->(c)), as are variable-length hops: `*` (unbounded, internally capped), `*N` (exact N hops), `*N..M` (between N and M hops inclusive), `*..M` (1 to M hops). A variable-length hop matches by shortest-path reachability within range (one row per node reached, not one row per distinct path — true exhaustive path enumeration is full-Cypher territory this grammar deliberately doesn't attempt); hop counts above 10 are silently clamped to 10, the same way max_rows is clamped rather than rejected. A fixed (non-variable-length) hop still returns one row per matching relationship, preserving multiplicity (e.g. two distinct call sites between the same two functions = two rows). Label matches CGPSymbol.kind case-insensitively (e.g. function, method, class). EDGE_TYPE matches a CGP edge type case-insensitively (e.g. calls, imports, extends). <op> is one of = != > >= < <= CONTAINS IN (IN takes a '|'-separated list, e.g. kind IN 'function|method'). Supported fields: id, name, kind, language, file, startLine, endLine, signature, confidence, exported, complexity, loopDepth, transitiveLoopDepth, linearScanInLoop, allocInLoop, recursionInLoop. "+
			"If RETURN includes an aggregate, every other RETURN item becomes an implicit GROUP BY key (standard Cypher/SQL rule) — e.g. RETURN f.file, COUNT(*) groups by file. An aggregate-only RETURN (no plain items) produces one row total even when nothing matched. Supported aggregates: COUNT, SUM, AVG, MIN, MAX, COLLECT. "+
			"Examples: MATCH (f:function) WHERE f.transitiveLoopDepth >= 3 RETURN f.name, f.file, f.transitiveLoopDepth ORDER BY f.transitiveLoopDepth DESC LIMIT 20. MATCH (f:function) RETURN f.file, COUNT(*), SUM(f.complexity) ORDER BY COUNT(*) DESC LIMIT 10. MATCH (a:function)-[:calls*1..3]->(b:function) WHERE a.name = 'handleRequest' RETURN b.name, b.file. "+
			"Hard cap of 5000 rows regardless of max_rows; response includes total (pre-truncation match count) and truncated."),
		mcp.WithString("query", mcp.Required(), mcp.Description("The MATCH/WHERE/RETURN/ORDER BY/LIMIT query — see tool description for the exact supported grammar.")),
		mcp.WithNumber("max_rows", mcp.Description("Optional row cap below the hard 5000-row ceiling.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, err := req.RequireString("query")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(mamari.QueryGraphLite(idx, query, mamari.QueryGraphLiteOptions{
			MaxRows: int(req.GetFloat("max_rows", 0)),
		})), nil
	})
	s.AddTool(mcp.NewTool("dead_code",
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithDescription("Best-effort dead-code report: symbols with no inbound CGP edges marking them as used, restricted by default to function/class/interface/component declarations in non-test/story source files. Heuristic, not proof — dynamic dispatch, reflection, and external callers can reference a symbol without producing a graph edge. Symbols that have unresolved same-name call evidence are conservatively omitted; uncertainSkipped and warnings report how many. Treat returned symbols as candidates for manual review."),
		mcp.WithNumber("limit", mcp.Description("Optional maximum number of symbols to return. Defaults to 500.")),
		mcp.WithString("kinds", mcp.Description("Optional comma-separated symbol kinds to consider. Defaults to function,class,interface,component.")),
		mcp.WithBoolean("include_exported", mcp.Description("Include exported symbols as candidates. Defaults to false (exported symbols may be a public API).")),
		mcp.WithBoolean("include_tests", mcp.Description("Include symbols declared in test files as candidates. Defaults to false.")),
		mcp.WithBoolean("include_stories", mcp.Description("Include symbols declared in story files as candidates. Defaults to false.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return jsonResult(mamari.DeadCode(idx, mamari.DeadCodeOptions{
			Limit:           int(req.GetFloat("limit", 0)),
			Kinds:           splitCSV(req.GetString("kinds", "")),
			IncludeExported: req.GetBool("include_exported", false),
			IncludeTests:    req.GetBool("include_tests", false),
			IncludeStories:  req.GetBool("include_stories", false),
		})), nil
	})
	s.AddTool(mcp.NewTool("tests_for",
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithDescription("Return test files/functions that, transitively via the static call graph (up to 6 hops), exercise the queried symbol. Resolved callers appear in tests; unresolved same-name test callers appear separately in possibleTests with warnings and are never promoted to resolved coverage. Best-effort: cannot see dynamic dispatch, reflection, or black-box/integration tests that never call the symbol directly."),
		mcp.WithString("query", mcp.Required(), mcp.Description("Symbol id, exact name, file:name, or substring.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, err := req.RequireString("query")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(mamari.TestsFor(idx, query, 0)), nil
	})
	s.AddTool(mcp.NewTool("untested_symbols",
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithDescription("Return symbols (default: function/method/class/component in non-test source files) whose static call-graph reverse closure contains no test-file caller within 6 hops. Symbols with unresolved same-name calls from tests are conservatively omitted; uncertainSkipped and warnings report how many. Use this as a prioritization signal for 'add tests for the change you just made'. Best-effort: dynamic dispatch, reflection, and black-box tests are invisible to this check."),
		mcp.WithNumber("limit", mcp.Description("Optional maximum number of symbols to return. Defaults to 500.")),
		mcp.WithString("kinds", mcp.Description("Optional comma-separated symbol kinds to consider. Defaults to function,method,class,component.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return jsonResult(mamari.UntestedSymbols(idx, mamari.UntestedSymbolsOptions{
			Limit: int(req.GetFloat("limit", 0)),
			Kinds: splitCSV(req.GetString("kinds", "")),
		})), nil
	})
	s.AddTool(mcp.NewTool("file_outline",
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithDescription("Return a repo-relative file's symbol tree (nesting via parent/child relationships) with signatures, complexity, and line ranges only — no source text. Use this as a cheap 'what's in this file' step before fetch_context, especially for large files."),
		mcp.WithString("file", mcp.Required(), mcp.Description("Repo-relative file path.")),
		mcp.WithNumber("limit", mcp.Description("Optional maximum total symbols (across the whole tree) to return. Defaults to 1000.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		file, err := req.RequireString("file")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(mamari.FileOutline(idx, file, mamari.FileOutlineOptions{
			Limit: int(req.GetFloat("limit", 0)),
		})), nil
	})
	if full {
		s.AddTool(mcp.NewTool("manage_notes",
			mcp.WithReadOnlyHintAnnotation(false),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithIdempotentHintAnnotation(false),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithDescription("Add, list, or remove freeform notes attached to symbol ids, persisted in .mamari/notes.json alongside the index (separate from the index itself, so `mamari index` reruns never clobber notes). Use add for cross-session continuity, e.g. 'this function has a known race condition, see issue #123'. Notes on a symbol are surfaced automatically by inspect_symbol."),
			mcp.WithString("action", mcp.Required(), mcp.Description("add: attach a note (requires symbol_id and text). list: list notes, most-recently-added first, optionally filtered by symbol_id. remove: delete a note by id (requires id)."), mcp.Enum("add", "list", "remove")),
			mcp.WithString("symbol_id", mcp.Description("Symbol id to attach or filter notes by. Required for action=add (must exist in the current index); optional filter for action=list.")),
			mcp.WithString("text", mcp.Description("Note text (max 4000 characters). Required for action=add.")),
			mcp.WithNumber("id", mcp.Description("Note id, as returned by action=add or action=list. Required for action=remove.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			action, err := req.RequireString("action")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			switch action {
			case "add":
				symbolID := req.GetString("symbol_id", "")
				text := req.GetString("text", "")
				if symbolID == "" || text == "" {
					return mcp.NewToolResultError("action=add requires symbol_id and text"), nil
				}
				resp, err := mamari.AddNote(idx, idx.Repo.Root, symbolID, text)
				if err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				return jsonResult(resp), nil
			case "list":
				resp, err := mamari.ListNotes(idx.Repo.Root, req.GetString("symbol_id", ""))
				if err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				return jsonResult(resp), nil
			case "remove":
				id, err := req.RequireFloat("id")
				if err != nil {
					return mcp.NewToolResultError("action=remove requires id: " + err.Error()), nil
				}
				resp, err := mamari.RemoveNote(idx.Repo.Root, int(id))
				if err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				return jsonResult(resp), nil
			default:
				return mcp.NewToolResultError(fmt.Sprintf("action must be one of add|list|remove, got %q", action)), nil
			}
		})

		s.AddTool(mcp.NewTool("manage_adr",
			mcp.WithReadOnlyHintAnnotation(false),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithIdempotentHintAnnotation(false),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithDescription("Get, list, update, or remove sections of the project's Architecture Decision Record document, persisted in .mamari/adr.json. Unlike manage_notes (per-symbol annotations), this is a project-level knowledge base of named sections (e.g. 'auth-strategy', 'data-storage') describing durable architectural decisions — use it for cross-session continuity on decisions that don't attach to one specific symbol."),
			mcp.WithString("action", mcp.Required(), mcp.Description("get: return one section by title, or the whole document if title is omitted. list: list section titles + timestamps only (no content) — cheap discovery. update: create or overwrite a section by title (requires title and content). remove: delete a section by title (requires title)."), mcp.Enum("get", "list", "update", "remove")),
			mcp.WithString("title", mcp.Description("Section title (case-insensitive, the upsert/lookup key). Required for action=update and action=remove; optional for action=get (omit for the whole document).")),
			mcp.WithString("content", mcp.Description("Section content (max 20000 characters). Required for action=update.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			action, err := req.RequireString("action")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			switch action {
			case "get":
				resp, err := mamari.GetADRSection(idx.Repo.Root, req.GetString("title", ""))
				if err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				return jsonResult(resp), nil
			case "list":
				resp, err := mamari.ListADRSections(idx.Repo.Root)
				if err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				return jsonResult(resp), nil
			case "update":
				title := req.GetString("title", "")
				content := req.GetString("content", "")
				if title == "" || content == "" {
					return mcp.NewToolResultError("action=update requires title and content"), nil
				}
				resp, err := mamari.UpdateADRSection(idx.Repo.Root, title, content)
				if err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				return jsonResult(resp), nil
			case "remove":
				title := req.GetString("title", "")
				if title == "" {
					return mcp.NewToolResultError("action=remove requires title"), nil
				}
				resp, err := mamari.RemoveADRSection(idx.Repo.Root, title)
				if err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
				return jsonResult(resp), nil
			default:
				return mcp.NewToolResultError(fmt.Sprintf("action must be one of get|list|update|remove, got %q", action)), nil
			}
		})
	}

	s.AddTool(mcp.NewTool("edit_symbol",
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithDescription("Produce an edit plan for a symbol: rename it everywhere mamari can see it used, replace its entire source range, or insert new text immediately after it. Mamari never writes files — review the returned `edits` (file, line/column ranges, old and new text) and apply them with your own editing tools. Edits derived from heuristic or unresolved references are flagged via `warnings` for extra scrutiny."),
		mcp.WithString("query", mcp.Required(), mcp.Description("Symbol name, qualified name, or id to operate on (same lookup as inspect_symbol/trace_symbol/impact).")),
		mcp.WithString("operation", mcp.Required(), mcp.Description("rename: rename the symbol's definition and every resolvable reference (requires new_name). replace_body: replace the symbol's entire source range, first line through last inclusive (requires new_body). insert_after: insert new text immediately after the symbol's source range, e.g. a new function right after an existing one (requires text)."), mcp.Enum("rename", "replace_body", "insert_after")),
		mcp.WithString("new_name", mcp.Description("New identifier name. Required when operation=rename. Must be a valid identifier (letters, digits, underscore, optionally leading '$').")),
		mcp.WithString("new_body", mcp.Description("Replacement source text for the symbol's full range. Required when operation=replace_body. A trailing newline is added automatically if missing.")),
		mcp.WithString("text", mcp.Description("Source text to insert after the symbol. Required when operation=insert_after. A trailing newline is added automatically if missing.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, err := req.RequireString("query")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		operation, err := req.RequireString("operation")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		var resp mamari.EditPlanResponse
		switch operation {
		case "rename":
			newName := req.GetString("new_name", "")
			if newName == "" {
				return mcp.NewToolResultError("operation=rename requires new_name"), nil
			}
			resp, err = mamari.RenameSymbol(idx, query, newName)
		case "replace_body":
			newBody := req.GetString("new_body", "")
			if newBody == "" {
				return mcp.NewToolResultError("operation=replace_body requires new_body"), nil
			}
			resp, err = mamari.ReplaceSymbolBody(idx, query, newBody)
		case "insert_after":
			text := req.GetString("text", "")
			if text == "" {
				return mcp.NewToolResultError("operation=insert_after requires text"), nil
			}
			resp, err = mamari.InsertAfterSymbol(idx, query, text)
		default:
			return mcp.NewToolResultError(fmt.Sprintf("operation must be one of rename|replace_body|insert_after, got %q", operation)), nil
		}
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(resp), nil
	})

	s.AddTool(mcp.NewTool("find_route",
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithDescription("Resolve an HTTP route, or an event name, to where it is handled/listened-to and where it is called/emitted, across the primary index and any repos linked with --link. Use this for cross-repo navigation, e.g. a frontend repo's `fetch('/api/users/:id')` resolving to the backend repo's route handler, or one repo's `bus.emit('user.created')` resolving to another repo's `bus.on('user.created')` listener. HTTP route matches are normalized so '/users/:id', '/users/${id}', and '/users/123' all match each other. If query doesn't parse as an HTTP route ('METHOD /path' or '/path'), it is tried as a bare event name instead — handlers are listen sites, callers are emit sites."),
		mcp.WithString("query", mcp.Required(), mcp.Description("A route as 'METHOD /path' (e.g. 'GET /users/:id') or just '/path' (defaults to GET); or, for event matching, a bare event name (e.g. 'user.created').")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, err := req.RequireString("query")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(mamari.FindRoute(idx, linked, query)), nil
	})

	if hasLinked {
		s.AddTool(mcp.NewTool("list_linked_repos",
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithDescription("List the primary repo and every repo linked with --link, with file/symbol counts. Use this to see what cross-repo context find_route and cross_repo_architecture can search."),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return jsonResult(mamari.ListLinkedRepos(idx, linked)), nil
		})

		s.AddTool(mcp.NewTool("cross_repo_architecture",
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(false),
			mcp.WithDescription("Cross-repo intelligence: find every HTTP and event-based coupling edge across the primary index and all repos linked with --link, then run the same community-detection architecture analysis repo_map uses, generalized to span repo boundaries. A returned community whose 'repos' field has more than one entry is a real cross-repo architectural boundary that a single-repo view cannot see — e.g. a frontend and backend repo that are actually one tightly-coupled module despite living in separate repos. Requires at least one repo loaded via --link; with none, returns just the primary repo with no cross-repo edges."),
			mcp.WithBoolean("source_only", mcp.Description("Exclude test/story/noisy files from the architecture analysis. Defaults to false.")),
			mcp.WithBoolean("include_tests", mcp.Description("Include test files even when source_only is set. Defaults to false.")),
		), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return jsonResult(mamari.CrossRepoArchitecture(idx, linked, mamari.RepoMapOptions{
				SourceOnly:   req.GetBool("source_only", false),
				IncludeTests: req.GetBool("include_tests", false),
			})), nil
		})
	}
	return s
}

type adaptiveCapabilities struct {
	ttl    bool
	events bool
}

func currentAdaptiveCapabilities(idx *mamari.Index) adaptiveCapabilities {
	return adaptiveCapabilities{
		ttl:    idx.HasTTLContent(),
		events: idx.HasEventEdges(),
	}
}

// refreshAdaptiveTools atomically replaces the named adaptive tool surface.
// SetTools invalidates the MCP library's compiled-schema caches and emits one
// tools/list_changed notification to every initialized session.
func refreshAdaptiveTools(s *server.MCPServer, idx *mamari.Index, linked []mamari.LinkedRepo, opts ServeOptions) {
	if normalizeToolset(opts) != "adaptive" {
		return
	}
	next := newMCPServer(idx, linked, opts).ListTools()
	names := make([]string, 0, len(next))
	for name := range next {
		names = append(names, name)
	}
	sort.Strings(names)
	tools := make([]server.ServerTool, 0, len(names))
	for _, name := range names {
		tools = append(tools, *next[name])
	}
	s.SetTools(tools...)
}

func normalizeToolset(opts ServeOptions) string {
	if opts.FullToolset {
		return "full"
	}
	toolset := strings.ToLower(strings.TrimSpace(opts.Toolset))
	switch toolset {
	case "", "slim":
		return "slim"
	case "adaptive", "full":
		return toolset
	default:
		return "slim"
	}
}

func addPrimaryTool(s *server.MCPServer, idx *mamari.Index) {
	s.AddTool(mcp.NewTool("mamari",
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithDescription("Local code intelligence. Start with explore for a question, map for architecture, or search for evidence; then use trace, node, context, or source for exact code. Other actions cover impact, graph queries, review, dead code, duplicates, reports, and index health."),
		mcp.WithString("action", mcp.Required(), mcp.Description("explore|map|search|exact|trace|node|context|source|impact|graph|review|dead_code|duplicates|report|doctor"), mcp.Enum("explore", "map", "search", "exact", "trace", "node", "context", "source", "impact", "graph", "review", "dead_code", "duplicates", "report", "doctor")),
		mcp.WithString("query", mcp.Description("Question, symbol, graph query, file:line, or file:start:end.")),
		mcp.WithString("args_json", mcp.Description("Optional JSON object for budget, limit, mode, depth, flags, file/start/end.")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		action, err := req.RequireString("action")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		args, err := primaryArgs(req.GetString("args_json", ""))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		query := req.GetString("query", "")
		switch strings.ToLower(strings.TrimSpace(action)) {
		case "explore":
			resp, err := mamari.InspectFlow(idx, query, mamari.InspectFlowOptions{
				Limit:                argInt(args, "limit", 6),
				BudgetTokens:         argInt(args, "budget", 1800),
				SearchBudgetTokens:   argInt(args, "search_budget", 700),
				ContextLines:         argInt(args, "context_lines", 8),
				SearchContextLines:   argInt(args, "search_context_lines", 1),
				Mode:                 argString(args, "mode", ""),
				SourceOnly:           argBool(args, "source_only", true),
				IncludeTests:         argBool(args, "include_tests", false),
				IncludeStories:       argBool(args, "include_stories", false),
				IncludeCallers:       argBool(args, "callers", false),
				IncludeCallees:       argBool(args, "callees", false),
				IncludeTraces:        argBool(args, "traces", false),
				IncludeSearchSymbols: argBool(args, "search_symbols", false),
			})
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return jsonResult(resp), nil
		case "map":
			return jsonResult(mamari.RepoMap(idx, mamari.RepoMapOptions{
				BudgetTokens:        argInt(args, "budget", 1200),
				Limit:               argInt(args, "limit", 40),
				Mentioned:           splitCSV(argString(args, "mentioned", "")),
				Query:               query,
				SourceOnly:          argBool(args, "source_only", true),
				IncludeTests:        argBool(args, "include_tests", false),
				IncludeArchitecture: argBool(args, "architecture", true),
			})), nil
		case "search":
			budget := argInt(args, "budget", 1200)
			resp := mamari.SearchCode(idx, query, mamari.SearchCodeOptions{
				Limit:             argInt(args, "limit", 10),
				BudgetTokens:      budget,
				ContextLines:      argInt(args, "context_lines", 1),
				SourceOnly:        argBool(args, "source_only", true),
				IncludeTests:      argBool(args, "include_tests", false),
				IncludeStories:    argBool(args, "include_stories", false),
				ExactFirst:        argBool(args, "exact_first", false),
				PreferDefinitions: argBool(args, "prefer_definitions", false),
				PreferUsages:      argBool(args, "prefer_usages", false),
				// A direct pattern-search action is most useful as focused,
				// grep-like evidence. Callers can still request "context"
				// explicitly when they need surrounding lines; changing
				// only the slim router default preserves SearchCode and the
				// named search_code tool's historical behavior.
				Mode:           argString(args, "mode", mamari.ModeEvidence),
				SymbolDetail:   argBool(args, "symbol_detail", false),
				DiversifyFiles: true,
			})
			mamari.FitSearchCodeResponse(&resp, budget)
			return jsonResult(resp), nil
		case "exact":
			return jsonResult(mamari.InspectExact(idx, query, mamari.InspectExactOptions{
				Limit:        argInt(args, "limit", 8),
				ContextLines: argInt(args, "context_lines", 0),
				WithSource:   argBool(args, "with_source", false),
				SourceOnly:   argBool(args, "source_only", true),
				IncludeTests: argBool(args, "include_tests", false),
			})), nil
		case "trace":
			return jsonResult(mamari.TraceSymbolWithOptions(idx, query, mamari.TraceSymbolOptions{
				WithEdges:          argBool(args, "with_edges", false),
				Sites:              argBool(args, "sites", true),
				IncludeTestDetails: argBool(args, "include_test_details", false),
				ExcludeTests:       argBool(args, "exclude_tests", false),
				Compact:            argBool(args, "compact", true),
			})), nil
		case "node":
			resp, err := mamari.InspectSymbolNode(idx, query, mamari.InspectSymbolNodeOptions{
				BudgetTokens:       argInt(args, "budget", 900),
				SourceLines:        argInt(args, "source_lines", 80),
				IncludeTests:       argBool(args, "include_tests", false),
				IncludeTestDetails: argBool(args, "include_test_details", false),
			})
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return jsonResult(resp), nil
		case "context":
			resp, err := mamari.FetchContext(idx, query, mamari.FetchContextOptions{
				BudgetTokens:   argInt(args, "budget", 1200),
				ContextLines:   argInt(args, "context_lines", 8),
				IncludeCallers: argBool(args, "callers", false),
				IncludeCallees: argBool(args, "callees", false),
				Mode:           argString(args, "mode", ""),
			})
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return jsonResult(resp), nil
		case "source":
			file, start, end, err := sourceRangeFromQuery(query, args)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			resp, err := mamari.FetchSource(idx, file, start, end)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return jsonResult(resp), nil
		case "impact":
			return jsonResult(mamari.ImpactWithOptions(idx, query, mamari.ImpactOptions{
				Depth:   argInt(args, "depth", 2),
				Compact: argBool(args, "compact", true),
			})), nil
		case "graph":
			resp := mamari.QueryGraphLite(idx, query, mamari.QueryGraphLiteOptions{
				MaxRows: argInt(args, "max_rows", 0),
			})
			if argBool(args, "compact", true) && resp.Status == "ok" {
				return jsonResult(mamari.CompactQueryGraphLiteResponse(resp)), nil
			}
			return jsonResult(resp), nil
		case "review":
			// The base ref is documented as the `query` param, but accept
			// args_json {"base": ...} too — silently ignoring it reviewed
			// against HEAD, a wrong-scope result that looks like success.
			reviewBase := query
			if strings.TrimSpace(reviewBase) == "" {
				reviewBase = argString(args, "base", "")
			}
			return jsonResult(mamari.Review(idx, mamari.ReviewOptions{
				Base:         reviewBase,
				Depth:        argInt(args, "depth", 2),
				Limit:        argInt(args, "limit", 0),
				IncludeTests: argBool(args, "include_tests", false),
				Callers:      argBool(args, "callers", false),
				CoveragePath: argString(args, "coverage", ""),
			})), nil
		case "dead_code", "deadcode":
			return jsonResult(mamari.DeadCode(idx, mamari.DeadCodeOptions{
				Limit:            argInt(args, "limit", 0),
				Kinds:            splitCSV(argString(args, "kinds", "")),
				IncludeExported:  argBool(args, "include_exported", false),
				IncludeTests:     argBool(args, "include_tests", false),
				IncludeStories:   argBool(args, "include_stories", false),
				IncludeUncertain: argBool(args, "include_uncertain", true),
			})), nil
		case "duplicates", "dupes":
			return jsonResult(mamari.Duplication(idx, mamari.DuplicationOptions{
				Limit:        argInt(args, "limit", 0),
				IncludeTests: argBool(args, "include_tests", false),
			})), nil
		case "report":
			return jsonResult(mamari.Report(idx, mamari.ReportOptions{
				TopN: argInt(args, "top", 10),
			})), nil
		case "doctor":
			return jsonResult(mamari.LimitDoctorParseFailures(
				mamari.Doctor(idx),
				argInt(args, "parse_failure_limit", 10),
			)), nil
		default:
			return mcp.NewToolResultError(fmt.Sprintf("unknown action %q", action)), nil
		}
	})
}

func primaryArgs(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}, nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, fmt.Errorf("args_json must be a JSON object: %w", err)
	}
	if args == nil {
		args = map[string]any{}
	}
	return args, nil
}

func argString(args map[string]any, key, fallback string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return fallback
}

func argBool(args map[string]any, key string, fallback bool) bool {
	if v, ok := args[key]; ok {
		switch x := v.(type) {
		case bool:
			return x
		case string:
			switch strings.ToLower(strings.TrimSpace(x)) {
			case "true", "1", "yes":
				return true
			case "false", "0", "no":
				return false
			}
		}
	}
	return fallback
}

func argInt(args map[string]any, key string, fallback int) int {
	if v, ok := args[key]; ok {
		switch x := v.(type) {
		case float64:
			return int(x)
		case int:
			return x
		case string:
			var out int
			if _, err := fmt.Sscanf(strings.TrimSpace(x), "%d", &out); err == nil {
				return out
			}
		}
	}
	return fallback
}

func sourceRangeFromQuery(query string, args map[string]any) (string, int, int, error) {
	file := argString(args, "file", "")
	start := argInt(args, "start", 0)
	end := argInt(args, "end", 0)
	if file != "" && start > 0 && end > 0 {
		return file, start, end, nil
	}
	parts := strings.Split(query, ":")
	if len(parts) < 3 {
		return "", 0, 0, fmt.Errorf("source action requires query file:start:end or args_json with file/start/end")
	}
	file = strings.Join(parts[:len(parts)-2], ":")
	start = parsePositiveInt(parts[len(parts)-2])
	end = parsePositiveInt(parts[len(parts)-1])
	if file == "" || start <= 0 || end <= 0 {
		return "", 0, 0, fmt.Errorf("source action requires positive file:start:end")
	}
	return file, start, end, nil
}

func parsePositiveInt(s string) int {
	var out int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &out); err != nil {
		return 0
	}
	return out
}

func jsonResult(v any) *mcp.CallToolResult {
	data, err := mamari.MarshalWithRealTokenEstimate(v, false, true)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("json marshal failed: %v", err))
	}
	return mcp.NewToolResultText(string(data))
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

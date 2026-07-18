package mamari

import (
	"sync"
	"sync/atomic"
)

const SchemaVersion = 10

// Confidence levels assigned to CGP edges and symbols.
// The taxonomy is closed: any other value is a bug.
const (
	ConfExact      = "exact"      // Declared/structural evidence with no ambiguity.
	ConfScoped     = "scoped"     // Resolved via imports or local scope to a single target.
	ConfHeuristic  = "heuristic"  // Best-effort guess; multiple plausible candidates.
	ConfUnresolved = "unresolved" // Target is known to exist but cannot be pinned to a symbol.
)

// Reason codes for unresolved edges. Exposed so agents can decide whether
// silence is acceptable (parse_error, runtime_value) or whether they should
// fall back to grep / fetch_source.
const (
	ReasonDynamicReceiver = "dynamic_receiver" // obj.method() where the receiver type is unknown.
	ReasonAmbiguousName   = "ambiguous_name"   // Multiple symbols share the bare name and we have no scope hint.
	ReasonRuntimeValue    = "runtime_value"    // Argument is computed at runtime (e.g. namedNode(varExpr)).
	ReasonMissingImport   = "missing_import"   // Imported binding is not present in the indexed repo.
	ReasonParseError      = "parse_error"      // Region was unparseable; edge is conservative.
)

// Per-file parser status. Lives on File so doctor can summarize coverage.
const (
	ParseStatusOK      = "ok"
	ParseStatusPartial = "partial"
	ParseStatusError   = "error"
)

type Index struct {
	SchemaVersion   int                  `json:"schemaVersion"`
	Repo            RepoInfo             `json:"repo"`
	Files           map[string]File      `json:"files"`
	Prefixes        map[string]Prefix    `json:"prefixes"`
	Terms           map[string]Term      `json:"terms"`
	Shapes          map[string]Shape     `json:"shapes"`
	References      []Reference          `json:"references"`
	Edges           []Edge               `json:"edges"`
	Literals        []Literal            `json:"literals,omitempty"`
	DynamicIRICalls []DynamicIRICall     `json:"dynamicIriCalls,omitempty"`
	Symbols         map[string]CGPSymbol `json:"symbols,omitempty"`
	SymbolEdges     []CGPEdge            `json:"symbolEdges,omitempty"`

	mu                       sync.Mutex                           `json:"-"`
	termLocationSeen         map[string]map[string]bool           `json:"-"`
	referenceSeen            map[string]bool                      `json:"-"`
	edgeSeen                 map[string]bool                      `json:"-"`
	symbolSeen               map[string]bool                      `json:"-"`
	symbolEdgeSeen           map[string]bool                      `json:"-"`
	classBases               map[string][]string                  `json:"-"` // class symbol ID -> base-class names (super()/super./extends resolution)
	classInterfaces          map[string][]string                  `json:"-"` // class symbol ID -> implemented interface simple names (Java `implements`)
	varTypes                 map[string]map[string]string         `json:"-"` // scope symbol ID (class or method) -> var/field name -> declared simple type name (Java)
	javaPackages             map[string]string                    `json:"-"` // file -> Java package name ("" for default package)
	javaImports              map[string][]string                  `json:"-"` // file -> imported type specs (e.g. "com.example.model.Person")
	javaFQN                  map[string]string                    `json:"-"` // "pkg.SimpleName" -> class symbol ID, for Java classes/interfaces
	csharpNamespaces         map[string]string                    `json:"-"` // file -> C# namespace name ("" for global namespace)
	csharpUsings             map[string][]string                  `json:"-"` // file -> `using` namespace specs (e.g. "System.Collections.Generic")
	csharpFQN                map[string]string                    `json:"-"` // "Namespace.SimpleName" -> class symbol ID, for C# classes/interfaces/structs/records
	csharpPartialFragments   map[string][]string                  `json:"-"` // "Namespace.SimpleName" -> every fragment symbol ID of a `partial class Service` declared across multiple files — csharpFQN above only keeps the last-written one (a race under parallel Phase 5 scanning), so field/method lookups that miss on one fragment's own symbol ID can still check its siblings
	goMethodsByReceiverType  map[string]map[string]string         `json:"-"` // Go receiver type simple name -> method name -> method symbol ID (cross-file `recv.Method()` resolution)
	goReceivers              map[string]goReceiver                `json:"-"` // Go method symbol ID -> its receiver variable name + simple type, for resolving same-receiver calls (`func (s *Service) Foo() { s.Bar() }`)
	goReturnTypes            map[string][]string                  `json:"-"` // Go function/method symbol ID -> simple (pointer-stripped) return type names, in order; used to infer `x := NewThing()` -> x's type from NewThing's return type
	goModulePath             string                               `json:"-"` // module directive from go.mod, used to distinguish internal package imports from external package receivers
	goModulePathLoaded       bool                                 `json:"-"`
	luaReceiverTypeBySymbol  map[string]string                    `json:"-"` // Lua method symbol ID -> its colon-call receiver table name (e.g. "Service" for `function Service:load()`), for self-attribute type resolution across methods of the same table — Lua has no real "class" symbol to scope through, unlike every other language with this rule
	luaMethodsByReceiverType map[string]map[string]string         `json:"-"` // Lua receiver table name -> method name -> method symbol ID, the Lua analogue of goMethodsByReceiverType — needed because findClassByName/findMethodInClass require a "class"-kind symbol, which Lua tables never have
	extensionMethods         map[string]map[string][]string       `json:"-"` // language+"\x00"+receiver type -> method name -> extension symbol IDs; [] preserves ambiguity instead of choosing an overload arbitrarily
	unresolvedMethodParents  map[string]string                    `json:"-"` // generic-engine method symbol ID -> its wanted receiver-type class name, recorded when emitGenericSymbolsTS's same-file-only typeIDByName lookup misses (e.g. a C++ out-of-line "ReturnType Class::method() {...}" defined in a .cpp whose class lives in a separate .h, not yet indexed when this file was scanned in parallel) — resolveOutOfLineMethodParents fixes these up globally once every file's symbols exist
	codeNamespaceLocals      map[string]map[string]namespaceEntry `json:"-"`
	codeNamespaceImports     map[string][]importStmt              `json:"-"`
	jsDefaultExports         map[string]string                    `json:"-"` // JS/TS file -> the identifier name it default-exports (`module.exports = X` / `export default X`), so a renamed default import (`const y = require('./x')` where x's fn is named X, y != X) resolves value references to the actual export instead of guessing
	aliasRules               []aliasRule                          `json:"-"` // import-path aliases (`@/`, `~`, custom) read from tsconfig/jsconfig `paths` and vite `resolve.alias`, so `@/components/X` resolves to the app's real source dir instead of failing or guessing
	codeScanTerms            map[string]Term                      `json:"-"`
	codeScanTermsActive      bool                                 `json:"-"`
	infraResources           map[string]infraResource             `json:"-"`
	infraKustomizations      map[string]infraKustomization        `json:"-"`
	infraDockerfiles         map[string]infraDockerfile           `json:"-"`
	symbolsByFile            map[string][]CGPSymbol               `json:"-"`
	symbolsByName            map[string][]CGPSymbol               `json:"-"` // symbol Name -> all symbols with that name, for O(1) call/class resolution
	childrenByParent         map[string][]CGPSymbol               `json:"-"` // ParentID -> direct child symbols (methods/fields of a class, etc.)
	symbolIndexBuilt         bool                                 `json:"-"`
	orderedSymbolIDs         []string                             `json:"-"` // stable graph-query scan order; replaced, never mutated, after publication
	codeSearchFiles          []codeSearchFile                     `json:"-"`
	codeSearchPostings       codeSearchPostingIndex               `json:"-"`
	codeSearchTermDocFreq    map[string]int                       `json:"-"`
	codeSearchBuilt          bool                                 `json:"-"`
	codeSearchBuildMu        sync.Mutex                           `json:"-"`
	codeSearchSidecarPath    string                               `json:"-"`
	semanticIndex            *semanticIndex                       `json:"-"`
	semanticSidecarPath      string                               `json:"-"`
	semanticBuildMu          sync.Mutex                           `json:"-"`
	// semanticPrewarmInFlight guards triggerSemanticPrewarm (watch.go) against
	// piling up redundant background rebuild goroutines when edits arrive
	// faster than a rebuild completes. See triggerSemanticPrewarm's doc
	// comment for why skipping a spawn here never drops an edit's effect.
	semanticPrewarmInFlight atomic.Bool `json:"-"`
	literalSidecarPath      string      `json:"-"`
	literalsLoaded          bool        `json:"-"`

	// published is an atomically-swapped, immutable snapshot of the data
	// SearchCode's hot path needs (tokenized search cache + prefix names).
	// Only ever written by the watcher (see watch.go's
	// publishQuerySnapshot calls), one goroutine at a time, so concurrent
	// Store calls never race each other; queries Load() it with no locking
	// at all. This makes a query's latency independent of how long
	// a concurrent rebake takes: readers do not block on a writer, without requiring
	// a database engine. nil until the first publish (plain one-shot CLI
	// usage with no watcher running never publishes, so it keeps today's
	// lazy on-demand build behavior unchanged).
	published atomic.Pointer[publishedQueryIndex] `json:"-"`

	// publishedSymbolGraph is the current immutable symbol-graph generation.
	// Query paths load it atomically and retain its maps/slices for the duration
	// of the request without copying them. Mutations detach the live graph first
	// (copy-on-write), so a reader that loaded an older generation can continue
	// safely while a watcher rebakes and later publishes its replacement.
	publishedSymbolGraph  atomic.Pointer[symbolGraphSnapshot] `json:"-"`
	symbolGraphMutable    bool                                `json:"-"` // guarded by mu; live Symbols/SymbolEdges no longer alias the published generation
	symbolGraphGeneration uint64                              `json:"-"` // guarded by mu; monotonic even when direct mutations temporarily clear the published pointer
	indexStringsInterned  bool                                `json:"-"` // v2 decoded all persisted strings from one canonical string table
	compactSymbolEdges    *compactSymbolEdgeStore             `json:"-"` // read-only numeric edge storage used after MCP/UI startup

	// searchResults memoizes fully-shaped search responses across watcher
	// generations. The watcher advances or invalidates entries before it
	// publishes a new immutable query snapshot, so repeated agent questions
	// remain cheap across edits that cannot affect their ranking or snippets.
	searchResultsMu sync.Mutex                                  `json:"-"`
	searchResults   map[searchCodeCacheKey]searchCodeCacheEntry `json:"-"`

	// repoMapResults memoizes the expensive PageRank/community response for
	// a fixed option set. repoMapCacheActive lets the first mutation in a
	// rebake invalidate once; the thousands of AddCGPEdge calls that follow
	// then avoid cache-lock traffic until another query enables caching.
	repoMapResultsMu    sync.Mutex                            `json:"-"`
	repoMapResults      map[repoMapCacheKey]repoMapCacheEntry `json:"-"`
	repoMapCacheActive  atomic.Bool                           `json:"-"`
	repoMapCacheVersion atomic.Uint64                         `json:"-"`

	// journal records file-level rebake events for `changed_since` queries.
	// It is process-local and not serialized: a fresh load starts with seq 0
	// and an empty journal, so consumers must treat the first response after
	// a server restart as a baseline.
	journal    []JournalEntry `json:"-"`
	journalSeq uint64         `json:"-"`

	// coChange caches the git co-change graph (lazily built/loaded from
	// .mamari/cochange.json on first use). coChangeLoaded distinguishes "not
	// yet attempted" from "attempted, repo has no git history" (nil map).
	coChange       map[string][]CoChangeEntry `json:"-"`
	coChangeLoaded bool                       `json:"-"`
}

// JournalEntry is one bucketed rebake event. Multiple file changes inside a
// single watch debounce window collapse into one entry so sequence numbers
// match the agent's observable cadence rather than raw fsnotify events.
type JournalEntry struct {
	Seq       uint64   `json:"seq"`
	Timestamp string   `json:"timestamp"`
	Updated   []string `json:"updated,omitempty"`
	Removed   []string `json:"removed,omitempty"`
}

// ChangedSinceResponse aggregates everything that changed in the journal
// after a given sequence. AffectedSymbols are the symbols currently living
// in the changed file set — handy for "what edges might be stale?"
// inspection without rebuilding the whole graph.
type ChangedSinceResponse struct {
	Status          string             `json:"status"`
	SinceSeq        uint64             `json:"sinceSeq"`
	LatestSeq       uint64             `json:"latestSeq"`
	Updated         []string           `json:"updated"`
	Removed         []string           `json:"removed"`
	AffectedSymbols []CGPSymbolSummary `json:"affectedSymbols,omitempty"`
	Entries         []JournalEntry     `json:"entries,omitempty"`
	// MissedHistory means sinceSeq predates the oldest retained entry. The
	// client should treat the response as a partial baseline and re-fetch
	// what it cares about explicitly.
	MissedHistory bool `json:"missedHistory,omitempty"`
}

type Literal struct {
	Predicate string   `json:"predicate"`
	Value     string   `json:"value"`
	Lang      string   `json:"lang,omitempty"`
	Location  Location `json:"location"`
	ShapeID   string   `json:"shapeId,omitempty"`
}

type DynamicIRICall struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Callee  string `json:"callee"`
	Snippet string `json:"snippet"`
}

type SearchLiteralResponse struct {
	Status string    `json:"status"`
	Query  string    `json:"query"`
	Lang   string    `json:"lang,omitempty"`
	Hits   []Literal `json:"hits"`
}

type SearchCodeOptions struct {
	Limit             int
	BudgetTokens      int
	ContextLines      int
	SourceOnly        bool
	IncludeTests      bool
	IncludeStories    bool
	BlastRadius       bool
	ExactFirst        bool
	PreferDefinitions bool
	PreferUsages      bool
	Mode              string
	// SymbolDetail keeps each hit's full containing-symbol summary
	// (signature, docstring, return types, hot-path fields) in the
	// response's Symbols field. Defaults to false: a hit's Symbols entries
	// are trimmed to name/kind/file/startLine, the same compact projection
	// trace_symbol/impact use for caller/callee entries. A search hit
	// already carries its own matched source text — restating the
	// containing symbol's full docstring on every one of (typically 10)
	// hits was the single largest contributor to search_code's response
	// size, disproportionate to the question "where did this match". Internal relevance scoring
	// is unaffected either way: it always uses the full symbol summary,
	// this option only controls what gets serialized into the response.
	SymbolDetail bool
	// DiversifyFiles caps how many top-N hits any single file may occupy
	// (see maxHitsPerFileFirstPass), so a large file cannot crowd equally
	// relevant smaller files out of a discovery result. Over-cap candidates
	// are deferred and still fill remaining slots in score order, so the hit
	// count never drops. Set for the standalone lean `search` action/CLI;
	// left off for inspect_flow's internal call so its context-slice merging
	// stays byte-identical. Scoring/determinism are unaffected.
	DiversifyFiles bool
}

// SearchCodeExactPhrase reports an exact-string signal (route literal, MIME
// type, RDF predicate hint, long identifier, quoted phrase) that the code
// search ranker boosted lines for. Returned in SearchCodeResponse so callers
// can see *why* a hit ranked high and replay the exact query directly.
type SearchCodeExactPhrase struct {
	Literal string `json:"literal"`
	Kind    string `json:"kind"`
}

type SearchCodeResponse struct {
	Status          string                  `json:"status"`
	Query           string                  `json:"query,omitempty"`
	Mode            string                  `json:"mode,omitempty"`
	Limit           int                     `json:"limit,omitempty"`
	BudgetTokens    int                     `json:"-"`
	EstimatedTokens int                     `json:"-"`
	Total           int                     `json:"total"`
	Truncated       bool                    `json:"truncated,omitempty"`
	Hits            []SearchCodeHit         `json:"hits"`
	ExactPhrases    []SearchCodeExactPhrase `json:"exactPhrases,omitempty"`
	// Confidence is "low" when part of the query matched nothing in the
	// corpus and the best hit matched only a minority of the typed terms —
	// hits exist but are anchored on the query's least-distinctive words, so
	// a calling agent should treat them as leads, not answers. Omitted
	// (empty) on normal result sets.
	Confidence string   `json:"confidence,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
}

type SemanticQueryOptions struct {
	Limit          int
	MinScore       float64
	SourceOnly     bool
	IncludeTests   bool
	IncludeStories bool
}

type SemanticQueryHit struct {
	Symbol CGPSymbolSummary `json:"symbol"`
	Score  float64          `json:"score"`
	Terms  []string         `json:"terms,omitempty"`
}

type SemanticQueryResponse struct {
	Status     string             `json:"status"`
	Query      string             `json:"query"`
	Model      string             `json:"model"`
	Dimensions int                `json:"dimensions"`
	Total      int                `json:"total"`
	Truncated  bool               `json:"truncated,omitempty"`
	Hits       []SemanticQueryHit `json:"hits"`
	Warnings   []string           `json:"warnings,omitempty"`
}

type InspectExactOptions struct {
	ContextLines int
	WithSource   bool
	Limit        int
	SourceOnly   bool
	IncludeTests bool
}

type InspectExactResponse struct {
	Status          string                  `json:"status"`
	Query           string                  `json:"query"`
	ExactPhrases    []SearchCodeExactPhrase `json:"exactPhrases"`
	Clusters        []ExactEvidenceCluster  `json:"clusters"`
	EstimatedTokens int                     `json:"-"`
	Truncated       bool                    `json:"truncated,omitempty"`
	Warnings        []string                `json:"warnings,omitempty"`
}

type ExactEvidenceCluster struct {
	File            string              `json:"file"`
	Symbol          string              `json:"symbol,omitempty"`
	SymbolID        string              `json:"symbolId,omitempty"`
	StartLine       int                 `json:"startLine,omitempty"`
	Route           string              `json:"route,omitempty"`
	Handler         string              `json:"handler,omitempty"`
	Line            int                 `json:"line,omitempty"`
	Matched         []string            `json:"matched"`
	Lines           []ExactEvidenceLine `json:"lines,omitempty"`
	Callers         []CGPSymbolSummary  `json:"callers,omitempty"`
	Callees         []CGPSymbolSummary  `json:"callees,omitempty"`
	Source          string              `json:"source,omitempty"`
	Score           int                 `json:"score,omitempty"`
	EstimatedTokens int                 `json:"-"`
}

type ExactEvidenceLine struct {
	Line    int      `json:"line"`
	Text    string   `json:"text"`
	Matched []string `json:"matched,omitempty"`
}

type RepoMapOptions struct {
	BudgetTokens int
	Limit        int
	Mentioned    []string
	// IncludeArchitecture adds a compact repository-level architecture packet
	// (languages, packages, entry points, routes, hotspots, connectivity-refined
	// communities, and typed cross-community boundaries) before the flat map.
	IncludeArchitecture bool
	// Query is an optional natural-language focus string. Significant terms
	// are extracted from it (stopwords/stemming via the same pipeline as
	// search-code) and personalize the PageRank ranking the same way
	// Mentioned does, so "repo-map <query>" biases the map toward files and
	// symbols relevant to the query instead of returning a query-independent
	// generic ranking.
	Query        string
	SourceOnly   bool
	IncludeTests bool
}

type RepoMapResponse struct {
	Status          string            `json:"status"`
	Query           string            `json:"query,omitempty"`
	BudgetTokens    int               `json:"-"`
	EstimatedTokens int               `json:"-"`
	Architecture    *RepoArchitecture `json:"architecture,omitempty"`
	Files           []RepoMapFile     `json:"files"`
	Symbols         []RepoMapSymbol   `json:"symbols"`
	Truncated       bool              `json:"truncated,omitempty"`
	Warnings        []string          `json:"warnings,omitempty"`
}

type RepoArchitecture struct {
	Languages       []RepoLanguageSummary `json:"languages,omitempty"`
	Packages        []RepoPackageSummary  `json:"packages,omitempty"`
	EntryPoints     []RepoEntryPoint      `json:"entryPoints,omitempty"`
	Routes          []CGPSymbolSummary    `json:"routes,omitempty"`
	Hotspots        []RepoHotspot         `json:"hotspots,omitempty"`
	Communities     []RepoCommunity       `json:"communities,omitempty"`
	Boundaries      []RepoBoundary        `json:"boundaries,omitempty"`
	EstimatedTokens int                   `json:"-"`
	Truncated       bool                  `json:"truncated,omitempty"`
}

type RepoLanguageSummary struct {
	Language string `json:"language"`
	Files    int    `json:"files"`
	Symbols  int    `json:"symbols"`
}

type RepoPackageSummary struct {
	Package  string   `json:"package"`
	Files    int      `json:"files"`
	Symbols  int      `json:"symbols"`
	Rank     float64  `json:"rank"`
	TopFiles []string `json:"topFiles,omitempty"`
}

type RepoEntryPoint struct {
	CGPSymbolSummary
	Reason string `json:"reason"`
}

type RepoHotspot struct {
	File       string  `json:"file"`
	Score      int     `json:"score"`
	Rank       float64 `json:"rank"`
	Inbound    int     `json:"inbound"`
	Outbound   int     `json:"outbound"`
	Complexity int     `json:"complexity"`
}

type RepoCommunity struct {
	ID             int                     `json:"id"`
	Name           string                  `json:"name"`
	FileCount      int                     `json:"fileCount"`
	Files          []string                `json:"files"`
	Packages       []string                `json:"packages,omitempty"`
	TopSymbols     []string                `json:"topSymbols,omitempty"`
	EdgeTypes      []RepoCommunityEdgeType `json:"edgeTypes,omitempty"`
	Cohesion       float64                 `json:"cohesion"`
	InternalWeight float64                 `json:"internalWeight"`
	ExternalWeight float64                 `json:"externalWeight"`
	// Repos lists the distinct repos this community's member files belong
	// to, set only by CrossRepoArchitecture (nil/omitted for the ordinary
	// single-repo repo_map architecture path). A community with more than
	// one entry here is a real cross-repo architectural boundary that a
	// single-repo view cannot see.
	Repos []string `json:"repos,omitempty"`
}

type RepoCommunityEdgeType struct {
	Type           string  `json:"type"`
	InternalEdges  int     `json:"internalEdges,omitempty"`
	ExternalEdges  int     `json:"externalEdges,omitempty"`
	InternalWeight float64 `json:"internalWeight,omitempty"`
	ExternalWeight float64 `json:"externalWeight,omitempty"`
}

type RepoBoundary struct {
	FromCommunity int                   `json:"fromCommunity"`
	ToCommunity   int                   `json:"toCommunity"`
	Edges         int                   `json:"edges"`
	Weight        float64               `json:"weight"`
	EdgeTypes     []RepoEdgeTypeSummary `json:"edgeTypes,omitempty"`
}

type RepoEdgeTypeSummary struct {
	Type   string  `json:"type"`
	Edges  int     `json:"edges"`
	Weight float64 `json:"weight"`
}

type RepoMapFile struct {
	File           string   `json:"file"`
	Language       string   `json:"language,omitempty"`
	Rank           float64  `json:"rank"`
	Score          int      `json:"score"`
	Inbound        int      `json:"inbound"`
	Outbound       int      `json:"outbound"`
	MatchedMention []string `json:"matchedMention,omitempty"`
	// CoChangedFiles lists files most often modified in the same git commit
	// as this file (top few, by historical co-change count). Empty when the
	// repo has no git history or co-change data hasn't been built yet.
	CoChangedFiles  []string `json:"coChangedFiles,omitempty"`
	EstimatedTokens int      `json:"-"`
}

type RepoMapSymbol struct {
	CGPSymbolSummary
	FileRank        float64  `json:"fileRank"`
	MatchedMention  []string `json:"matchedMention,omitempty"`
	EstimatedTokens int      `json:"-"`
}

type SearchCodeHit struct {
	File            string             `json:"file"`
	StartLine       int                `json:"startLine"`
	EndLine         int                `json:"endLine"`
	FocusLine       int                `json:"focusLine"`
	Score           int                `json:"score"`
	EstimatedTokens int                `json:"-"`
	MatchedTerms    []string           `json:"matchedTerms,omitempty"`
	MatchedExact    []string           `json:"matchedExact,omitempty"`
	Symbols         []CGPSymbolSummary `json:"symbols,omitempty"`
	BlastRadius     []SearchCodeBlast  `json:"blastRadius,omitempty"`
	Text            string             `json:"text"`
}

type SearchCodeBlast struct {
	Symbol  SearchCodeRelatedSymbol   `json:"symbol"`
	Callers []SearchCodeRelatedSymbol `json:"callers,omitempty"`
	Callees []SearchCodeRelatedSymbol `json:"callees,omitempty"`
	Tests   []SearchCodeRelatedSymbol `json:"tests,omitempty"`
}

type SearchCodeRelatedSymbol struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	File      string `json:"file"`
	StartLine int    `json:"startLine"`
	Count     int    `json:"count,omitempty"`
	More      bool   `json:"more,omitempty"`
}

type InspectTermResponse struct {
	Status          string                      `json:"status"`
	Query           string                      `json:"query"`
	Term            *TermSummary                `json:"term,omitempty"`
	Candidates      []TermSummary               `json:"candidates,omitempty"`
	Trace           TraceGroupedCompactResponse `json:"trace,omitempty"`
	Implementation  []SearchCodeHit             `json:"implementation,omitempty"`
	Context         FetchContextResponse        `json:"context,omitempty"`
	EstimatedTokens int                         `json:"-"`
	Warnings        []string                    `json:"warnings,omitempty"`
}

type InspectTermOptions struct {
	BudgetTokens int
	ContextLines int
	Mode         string
	IncludeWeak  bool
	Limit        int
}

type FindContainingShapeResponse struct {
	Status     string  `json:"status"`
	Query      string  `json:"query"`
	Containers []Shape `json:"containers"`
}

type ListDynamicIRIsResponse struct {
	Status string           `json:"status"`
	File   string           `json:"file,omitempty"`
	Calls  []DynamicIRICall `json:"calls"`
}

type FetchContextOptions struct {
	BudgetTokens          int
	ContextLines          int
	IncludeCallers        bool
	IncludeCallees        bool
	SuppressImports       bool
	MaxSymbolContextLines int
	// Mode is one of ModeCompact, ModeEvidence, ModeContext, ModeFull.
	// Empty string is treated as ModeContext (current default).
	Mode string
}

// Response modes for fetch-context. The progression goes from cheapest
// (compact: no source text) to richest (full: source + callers + callees +
// imports). Use the cheapest mode that answers the question.
const (
	ModeCompact  = "compact"  // Graph metadata only. No source text.
	ModeEvidence = "evidence" // Metadata + single evidence line per slice.
	ModeContext  = "context"  // Adaptive merged source slices (default).
	ModeFull     = "full"     // Context + auto-included callers and callees.
)

type FetchContextResponse struct {
	Status          string             `json:"status"`
	Query           string             `json:"query,omitempty"`
	BudgetTokens    int                `json:"-"`
	EstimatedTokens int                `json:"-"`
	Truncated       bool               `json:"truncated"`
	Target          *CGPSymbolSummary  `json:"target,omitempty"`
	Targets         []CGPSymbolSummary `json:"targets,omitempty"`
	Slices          []ContextSlice     `json:"slices"`
	Warnings        []string           `json:"warnings,omitempty"`
}

type ContextSlice struct {
	File            string `json:"file"`
	StartLine       int    `json:"startLine"`
	EndLine         int    `json:"endLine"`
	FullStartLine   int    `json:"fullStartLine,omitempty"`
	FullEndLine     int    `json:"fullEndLine,omitempty"`
	Truncated       bool   `json:"truncated,omitempty"`
	FocusLine       int    `json:"focusLine,omitempty"`
	FocusLines      []int  `json:"focusLines,omitempty"`
	Kind            string `json:"kind"`
	Reason          string `json:"reason"`
	EstimatedTokens int    `json:"-"`
	Text            string `json:"text"`
}

type InspectFlowOptions struct {
	Limit                int
	BudgetTokens         int
	SearchBudgetTokens   int
	ContextLines         int
	SearchContextLines   int
	Mode                 string
	SourceOnly           bool
	IncludeTests         bool
	IncludeStories       bool
	IncludeCallers       bool
	IncludeCallees       bool
	IncludeTraces        bool
	IncludeSearchSymbols bool
}

type InspectFlowResponse struct {
	Status          string                `json:"status"`
	Query           string                `json:"query,omitempty"`
	Search          SearchCodeResponse    `json:"search"`
	Symbols         []CGPSymbolSummary    `json:"symbols,omitempty"`
	Traces          []TraceSymbolResponse `json:"traces,omitempty"`
	Context         FetchContextResponse  `json:"context"`
	EstimatedTokens int                   `json:"-"`
	Warnings        []string              `json:"warnings,omitempty"`
}

type CGPSymbol struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Language    string   `json:"language"`
	File        string   `json:"file"`
	StartLine   int      `json:"startLine"`
	StartColumn int      `json:"startColumn"`
	EndLine     int      `json:"endLine"`
	EndColumn   int      `json:"endColumn"`
	Signature   string   `json:"signature,omitempty"`
	Docstring   string   `json:"docstring,omitempty"`
	ReturnTypes []string `json:"returnTypes,omitempty"`
	// ReceiverType is persisted for language constructs whose owner is not
	// necessarily a repo class, currently Kotlin extension functions. It
	// lets a loaded/watch-mode index rebuild receiver-scoped resolution.
	ReceiverType string `json:"receiverType,omitempty"`
	Exported     bool   `json:"exported,omitempty"`
	ParentID     string `json:"parentId,omitempty"`
	Confidence   string `json:"confidence"`
	SCIPSymbol   string `json:"scipSymbol,omitempty"`
	// Complexity is an approximate cyclomatic-complexity score for
	// function/method/callback symbols (1 = single straight-line path, plus
	// one per branch/loop/boolean-operator decision point found in the
	// symbol's source range). Zero for symbols where it is not computed
	// (e.g. classes, imports, TTL terms).
	Complexity int `json:"complexity,omitempty"`
	// LoopDepth is the maximum loop-nesting depth found directly inside this
	// symbol's own source range (0 = no loop). See hotpath.go.
	LoopDepth int `json:"loopDepth,omitempty"`
	// TransitiveLoopDepth is the maximum LoopDepth reachable by following
	// "calls" edges outward from this symbol, capped at
	// maxTransitiveLoopDepthHops — a hint that a shallow-looking function may
	// still sit on a hot path because something it calls (possibly several
	// hops away) loops deeply. See propagateTransitiveLoopDepth in hotpath.go.
	TransitiveLoopDepth int `json:"transitiveLoopDepth,omitempty"`
	// LinearScanInLoop counts find/indexOf/includes/contains-style calls
	// found inside a loop body in this symbol — the hidden O(n^2) that
	// LoopDepth alone misses. See hotpath.go.
	LinearScanInLoop int `json:"linearScanInLoop,omitempty"`
	// AllocInLoop counts allocation/append/push-style calls or expressions
	// found inside a loop body in this symbol. See hotpath.go.
	AllocInLoop int `json:"allocInLoop,omitempty"`
	// RecursionInLoop is true when this symbol calls itself from inside a
	// loop body (as opposed to plain non-looping recursion). See hotpath.go.
	RecursionInLoop bool `json:"recursionInLoop,omitempty"`
	// ShapeHash is a language-agnostic structural fingerprint of a
	// function/method body: source with strings/comments masked and
	// identifiers/literals normalized (control keywords preserved), then
	// hashed. Two symbols with the same ShapeHash are structural (Type-2)
	// clones — the same code with names/literals changed. Empty for symbols
	// too small to fingerprint meaningfully. Powers the `duplicates` flow.
	ShapeHash string `json:"shapeHash,omitempty"`
}

type CGPEdge struct {
	ID               string   `json:"id"`
	From             string   `json:"from"`
	To               string   `json:"to"`
	Type             string   `json:"type"`
	Confidence       string   `json:"confidence"`
	UnresolvedReason string   `json:"unresolvedReason,omitempty"`
	Evidence         Location `json:"evidence"`
}

type CGPSymbolSummary struct {
	// ID/Language are omitempty so summarizeSymbolCompact (cgp.go) can
	// deliberately leave them unset for trace_symbol's Compact mode's
	// caller/callee entries — every other caller of this type always
	// populates both, so omitempty changes nothing for them.
	ID          string   `json:"id,omitempty"`
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Language    string   `json:"language,omitempty"`
	File        string   `json:"file"`
	StartLine   int      `json:"startLine"`
	Signature   string   `json:"signature,omitempty"`
	Docstring   string   `json:"docstring,omitempty"`
	ReturnTypes []string `json:"returnTypes,omitempty"`
	Score       int      `json:"score,omitempty"`
	Complexity  int      `json:"complexity,omitempty"`
	// LoopDepth/TransitiveLoopDepth/LinearScanInLoop/AllocInLoop/
	// RecursionInLoop mirror the same-named CGPSymbol fields — see
	// hotpath.go.
	LoopDepth           int      `json:"loopDepth,omitempty"`
	TransitiveLoopDepth int      `json:"transitiveLoopDepth,omitempty"`
	LinearScanInLoop    int      `json:"linearScanInLoop,omitempty"`
	AllocInLoop         int      `json:"allocInLoop,omitempty"`
	RecursionInLoop     bool     `json:"recursionInLoop,omitempty"`
	Count               int      `json:"count,omitempty"`
	Lines               []int    `json:"lines,omitempty"`
	NamesPreview        []string `json:"namesPreview,omitempty"`
	Truncated           bool     `json:"truncated,omitempty"`
	searchNameTokens    compactTokenSet
}

type ListSymbolsResponse struct {
	Status    string             `json:"status"`
	Query     string             `json:"query,omitempty"`
	Kind      string             `json:"kind,omitempty"`
	Lang      string             `json:"lang,omitempty"`
	Limit     int                `json:"limit,omitempty"`
	Total     int                `json:"total,omitempty"`
	Truncated bool               `json:"truncated,omitempty"`
	Symbols   []CGPSymbolSummary `json:"symbols"`
}

type ListSymbolsOptions struct {
	Limit          int
	Kinds          []string
	SourceOnly     bool
	IncludeTests   bool
	IncludeStories bool
	WithScores     bool
}

// DeadCodeOptions configures DeadCode. The defaults are deliberately
// conservative: only top-level declaration kinds that are unlikely to be
// invoked reflectively/dynamically are considered, exported symbols are
// excluded (they may be part of a public API consumed outside this index),
// and test/story/backup files are excluded from both the candidate set and
// the "referenced" evidence.
type DeadCodeOptions struct {
	// Limit caps the number of returned symbols. <= 0 means use the default
	// cap (maxDeadCodeSymbols).
	Limit int
	// Kinds restricts the candidate symbol kinds. Empty means the default
	// set: function, class, interface, component.
	Kinds []string
	// IncludeExported includes exported symbols as dead-code candidates.
	// Off by default since exported symbols may be a public API used by
	// code outside this repo/index.
	IncludeExported bool
	// IncludeTests includes symbols declared in test files as candidates.
	IncludeTests bool
	// IncludeStories includes symbols declared in story files as candidates.
	IncludeStories bool
	// IncludeUncertain returns the "possibly dead" symbols (those with no
	// resolved reference but a same-name unresolved call that might reach them)
	// as a separate Uncertain list instead of only counting them. This makes
	// the honesty explicit: Symbols are unreferenced (safe to remove), Uncertain
	// are held back precisely because Mamari will not claim a symbol is dead
	// when an edge it could not resolve might use it.
	IncludeUncertain bool
}

type DeadCodeResponse struct {
	Status           string             `json:"status"`
	Total            int                `json:"total"`
	Limit            int                `json:"limit,omitempty"`
	Truncated        bool               `json:"truncated"`
	UncertainSkipped int                `json:"uncertainSkipped,omitempty"`
	Symbols          []CGPSymbolSummary `json:"symbols"`
	// Uncertain lists the symbols held back from Symbols because an unresolved
	// same-name call might reference them (populated only when
	// DeadCodeOptions.IncludeUncertain is set). These are "review before
	// removing", never asserted dead.
	Uncertain []CGPSymbolSummary `json:"uncertain,omitempty"`
	Warnings  []string           `json:"warnings,omitempty"`
}

type TestsForResponse struct {
	Status        string             `json:"status"`
	Query         string             `json:"query"`
	Symbol        *CGPSymbolSummary  `json:"symbol,omitempty"`
	Candidates    []CGPSymbolSummary `json:"candidates,omitempty"`
	Tests         []CGPSymbolSummary `json:"tests"`
	PossibleTests []CGPSymbolSummary `json:"possibleTests,omitempty"`
	Total         int                `json:"total"`
	Truncated     bool               `json:"truncated"`
	Warnings      []string           `json:"warnings,omitempty"`
}

// UntestedSymbolsOptions configures UntestedSymbols. Defaults restrict
// candidates to function/method/class/component declarations in non-test
// source files.
type UntestedSymbolsOptions struct {
	// Limit caps the number of returned symbols. <= 0 means use the default
	// cap (maxUntestedSymbols).
	Limit int
	// Kinds restricts the candidate symbol kinds. Empty means the default
	// set: function, method, class, component.
	Kinds []string
}

type UntestedSymbolsResponse struct {
	Status           string             `json:"status"`
	Total            int                `json:"total"`
	Limit            int                `json:"limit,omitempty"`
	Truncated        bool               `json:"truncated"`
	UncertainSkipped int                `json:"uncertainSkipped,omitempty"`
	Symbols          []CGPSymbolSummary `json:"symbols"`
	Warnings         []string           `json:"warnings,omitempty"`
}

// FileOutlineOptions configures FileOutline.
type FileOutlineOptions struct {
	// Limit caps the total number of symbols (across the whole tree) in the
	// response. <= 0 means use the default cap (maxOutlineSymbols).
	Limit int
}

// OutlineSymbol is one node in a file's symbol tree: signatures and metadata
// only, no source text. Children are nested declarations (e.g. methods inside
// a class).
type OutlineSymbol struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Kind       string          `json:"kind"`
	StartLine  int             `json:"startLine"`
	EndLine    int             `json:"endLine"`
	Signature  string          `json:"signature,omitempty"`
	Exported   bool            `json:"exported,omitempty"`
	Complexity int             `json:"complexity,omitempty"`
	Children   []OutlineSymbol `json:"children,omitempty"`
}

type FileOutlineResponse struct {
	Status    string          `json:"status"`
	File      string          `json:"file"`
	Total     int             `json:"total"`
	Limit     int             `json:"limit,omitempty"`
	Truncated bool            `json:"truncated"`
	Symbols   []OutlineSymbol `json:"symbols"`
}

// SymbolNote is a short freeform note an agent attached to a symbol, for
// cross-session continuity. Persisted in .mamari/notes.json, separate from
// the index itself so `mamari index` reruns never clobber notes.
type SymbolNote struct {
	ID        int    `json:"id"`
	SymbolID  string `json:"symbolId"`
	Text      string `json:"text"`
	CreatedAt string `json:"createdAt"`
}

// NotesFile is the on-disk schema of .mamari/notes.json.
type NotesFile struct {
	NextID int          `json:"nextId"`
	Notes  []SymbolNote `json:"notes"`
}

type AddNoteResponse struct {
	Status string     `json:"status"`
	Note   SymbolNote `json:"note,omitempty"`
}

type ListNotesResponse struct {
	Status string       `json:"status"`
	Total  int          `json:"total"`
	Notes  []SymbolNote `json:"notes"`
}

type RemoveNoteResponse struct {
	Status  string `json:"status"`
	Removed bool   `json:"removed"`
}

// ADRSection is one named section of the project's Architecture Decision
// Record document — a project-level (not per-symbol) freeform knowledge
// base, persisted in .mamari/adr.json. Title is the upsert key
// (case-insensitive unique).
type ADRSection struct {
	Title     string `json:"title"`
	Content   string `json:"content,omitempty"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// ADRDocument is the on-disk schema of .mamari/adr.json.
type ADRDocument struct {
	SchemaVersion int          `json:"schemaVersion"`
	Sections      []ADRSection `json:"sections"`
}

type ADRSectionResponse struct {
	Status  string     `json:"status"`
	Section ADRSection `json:"section,omitempty"`
}

// ADRListResponse is action=list's response: titles + timestamps only, no
// content, so discovering what ADR sections exist is cheap.
type ADRListResponse struct {
	Status   string       `json:"status"`
	Total    int          `json:"total"`
	Sections []ADRSection `json:"sections"`
}

// ADRGetResponse is action=get's response. When Title is empty (no specific
// section requested), Sections carries the whole document.
type ADRGetResponse struct {
	Status   string       `json:"status"`
	Sections []ADRSection `json:"sections"`
}

type ADRRemoveResponse struct {
	Status  string `json:"status"`
	Removed bool   `json:"removed"`
}

// SymbolChange describes a symbol present in both index snapshots whose
// observable shape changed between base and head.
type SymbolChange struct {
	ID  string           `json:"id"`
	Old CGPSymbolSummary `json:"old"`
	New CGPSymbolSummary `json:"new"`
	// Fields lists which top-level attributes differ, e.g.
	// "signature", "startLine", "endLine", "complexity", "exported", "kind".
	Fields []string `json:"fields"`
}

// QueryGraphLiteOptions configures QueryGraphLite.
type QueryGraphLiteOptions struct {
	// MaxRows caps returned rows; 0 or >cypherLiteHardRowCeiling uses the
	// hard ceiling.
	MaxRows int
}

// QueryGraphLiteResponse is QueryGraphLite's result: rows as
// var.field -> value maps (matching the RETURN clause), plus the
// pre-truncation total so callers can tell a query was capped.
type QueryGraphLiteResponse struct {
	Status string `json:"status"`
	// Query is only populated when Status != "ok" (an invalid query) — see
	// QueryGraphLite's doc comment. A successful response never echoes it:
	// the caller already has it, and a Cypher-lite statement can be long
	// relative to the typically small, row-shaped result it produces.
	Query     string           `json:"query,omitempty"`
	Rows      []map[string]any `json:"rows"`
	Total     int              `json:"total"`
	Truncated bool             `json:"truncated,omitempty"`
	Warnings  []string         `json:"warnings,omitempty"`
	// Columns preserves RETURN-clause order for compact table rendering. The
	// ordinary response remains the historical map-per-row JSON shape.
	Columns []string `json:"-"`
}

// QueryGraphLiteTableResponse is the token-efficient, lossless table
// projection used by Mamari's slim MCP router. Column names are emitted once
// instead of being repeated in every row; the named query_graph tool and Go
// API retain QueryGraphLiteResponse's map-per-row representation.
type QueryGraphLiteTableResponse struct {
	Status    string   `json:"status,omitempty"`
	Query     string   `json:"query,omitempty"`
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Total     int      `json:"total,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

// DiffIndexResponse is the structural diff between two CGP index snapshots
// (e.g. before/after a PR), used to summarize "what changed" without
// re-running the whole graph by hand.
type DiffIndexResponse struct {
	Status string `json:"status"`

	SymbolsAdded   []CGPSymbolSummary `json:"symbolsAdded"`
	SymbolsRemoved []CGPSymbolSummary `json:"symbolsRemoved"`
	SymbolsChanged []SymbolChange     `json:"symbolsChanged"`

	EdgesAdded   []CGPEdge `json:"edgesAdded"`
	EdgesRemoved []CGPEdge `json:"edgesRemoved"`

	Summary DiffIndexSummary `json:"summary"`
}

type DiffIndexSummary struct {
	SymbolsAdded   int `json:"symbolsAdded"`
	SymbolsRemoved int `json:"symbolsRemoved"`
	SymbolsChanged int `json:"symbolsChanged"`
	EdgesAdded     int `json:"edgesAdded"`
	EdgesRemoved   int `json:"edgesRemoved"`
}

// FileEdit is one textual change to a file, expressed with LSP-style
// half-open ranges: StartLine/StartColumn is inclusive, EndLine/EndColumn is
// exclusive, both 1-based. A zero-width range (Start == End) is a pure
// insertion; OldText is empty in that case.
type FileEdit struct {
	File        string `json:"file"`
	StartLine   int    `json:"startLine"`
	StartColumn int    `json:"startColumn"`
	EndLine     int    `json:"endLine"`
	EndColumn   int    `json:"endColumn"`
	OldText     string `json:"oldText"`
	NewText     string `json:"newText"`
	// Confidence reflects how the edit location was determined (ConfExact,
	// ConfScoped, ConfHeuristic, ConfUnresolved). Only set for edits derived
	// from resolved call/reference edges, e.g. by rename_symbol.
	Confidence string `json:"confidence,omitempty"`
}

// EditPlanResponse is a proposed set of textual edits produced by one of
// mamari's edit-plan tools (rename_symbol, replace_symbol_body,
// insert_after_symbol). Mamari never writes files itself: callers review
// Edits and apply them with their own editing tools.
type EditPlanResponse struct {
	Status        string             `json:"status"`
	Operation     string             `json:"operation"`
	Query         string             `json:"query,omitempty"`
	Symbol        *CGPSymbolSummary  `json:"symbol,omitempty"`
	Candidates    []CGPSymbolSummary `json:"candidates,omitempty"`
	Edits         []FileEdit         `json:"edits"`
	FilesAffected int                `json:"filesAffected"`
	Warnings      []string           `json:"warnings,omitempty"`
}

type TraceSymbolResponse struct {
	Status     string             `json:"status"`
	Query      string             `json:"query"`
	Symbol     *CGPSymbol         `json:"symbol,omitempty"`
	Candidates []CGPSymbolSummary `json:"candidates,omitempty"`
	// CandidateDetails carries compact caller summaries per candidate when
	// status="ambiguous" and there are few enough candidates to expand
	// inline (see maxAmbiguousTraceDetails) — lets a calling agent resolve
	// "who calls this ambiguous name" in one round trip instead of
	// disambiguating and re-querying once per candidate.
	CandidateDetails []TraceSymbolCandidateDetail `json:"candidateDetails,omitempty"`
	Callers          []CGPSymbolSummary           `json:"callers"`
	Callees          []CGPSymbolSummary           `json:"callees"`
	PossibleCallers  []CGPSymbolSummary           `json:"possibleCallers,omitempty"`
	PossibleCount    int                          `json:"possibleCallerCount,omitempty"`
	Imports          []CGPEdge                    `json:"imports,omitempty"`
	CallerSites      []CGPCallSite                `json:"callerSites,omitempty"`
	PossibleSites    []CGPCallSite                `json:"possibleCallerSites,omitempty"`
	CallerConfidence CGPConfidenceSummary         `json:"callerConfidence"`
	Edges            []CGPEdge                    `json:"edges,omitempty"`
	Warnings         []string                     `json:"warnings,omitempty"`
	// compactMainSymbol is unexported (never serialized — Go's
	// encoding/json always skips unexported fields, regardless of tags)
	// and set only by traceSymbolFromSnapshot when Compact is requested.
	// TraceSymbolResponse's own MarshalJSON (cgp.go) checks it to decide
	// whether to render Symbol via the full CGPSymbol shape or the leaner
	// compactMainSymbolJSON projection.
	compactMainSymbol bool
}

// TraceSymbolCandidateDetail is one ambiguous candidate's caller-side trace,
// as included inline in TraceSymbolResponse.CandidateDetails. Callees are
// populated only for call sites that explicitly opt into a fuller expansion;
// the default ambiguous response stays focused on "who calls each candidate?"
// so it can replace several disambiguation round trips without ballooning.
type TraceSymbolCandidateDetail struct {
	Symbol           *CGPSymbolSummary    `json:"symbol,omitempty"`
	Callers          []CGPSymbolSummary   `json:"callers"`
	Callees          []CGPSymbolSummary   `json:"callees,omitempty"`
	PossibleCallers  []CGPSymbolSummary   `json:"possibleCallers,omitempty"`
	PossibleCount    int                  `json:"possibleCallerCount,omitempty"`
	CallerSites      []CGPCallSite        `json:"callerSites,omitempty"`
	PossibleSites    []CGPCallSite        `json:"possibleCallerSites,omitempty"`
	CallerConfidence CGPConfidenceSummary `json:"callerConfidence"`
}

type TraceSymbolOptions struct {
	WithEdges          bool
	Sites              bool
	IncludeTestDetails bool
	ExcludeTests       bool
	// Compact drops Signature/Docstring/ReturnTypes/hot-path fields from
	// Callers/Callees/CandidateDetails entries, keeping only the
	// identifying fields (id/name/kind/language/file/startLine) — for
	// callers that just need "who calls this" without per-caller source
	// detail, where the full per-caller signature+docstring can dominate
	// response size once there are more than a handful of callers. Mirrors
	// the same lean-summary precedent already established for
	// inspect_symbol's format=node response. Defaults to false
	// (unchanged, full-detail behavior) — existing callers/tests are
	// unaffected unless they opt in.
	Compact bool
}

type InspectSymbolResponse struct {
	Status     string             `json:"status"`
	Query      string             `json:"query"`
	Symbol     *CGPSymbol         `json:"symbol,omitempty"`
	Candidates []CGPSymbolSummary `json:"candidates,omitempty"`
	// CandidateDetails carries per-candidate caller summaries when
	// status="ambiguous" and there are few enough candidates to expand
	// inline — see TraceSymbolResponse.CandidateDetails /
	// maxAmbiguousTraceDetails. Avoids a second disambiguate-then-trace
	// round trip for the common "2-4 same-named symbols" case.
	CandidateDetails []TraceSymbolCandidateDetail `json:"candidateDetails,omitempty"`
	Trace            TraceSymbolResponse          `json:"trace,omitempty"`
	Context          FetchContextResponse         `json:"context,omitempty"`
	Frontend         []CGPEdge                    `json:"frontend,omitempty"`
	// Notes are any notes saved against this symbol via add_note, most
	// recently added first. Empty when none exist.
	Notes           []SymbolNote `json:"notes,omitempty"`
	EstimatedTokens int          `json:"-"`
	Warnings        []string     `json:"warnings,omitempty"`
}

type InspectSymbolOptions struct {
	BudgetTokens       int
	ContextLines       int
	Mode               string
	IncludeTests       bool
	IncludeTestDetails bool
	WithEdges          bool
}

type InspectSymbolNodeResponse struct {
	Status     string             `json:"status"`
	Query      string             `json:"query"`
	Symbol     *CGPSymbolSummary  `json:"symbol,omitempty"`
	Candidates []CGPSymbolSummary `json:"candidates,omitempty"`
	// CandidateDetails mirrors TraceSymbolResponse.CandidateDetails — see
	// InspectSymbolResponse.CandidateDetails for why this exists.
	CandidateDetails []TraceSymbolCandidateDetail `json:"candidateDetails,omitempty"`
	Docstring        string                       `json:"docstring,omitempty"`
	ReturnTypes      []string                     `json:"returnTypes,omitempty"`
	Source           string                       `json:"source,omitempty"`
	Callers          []CGPSymbolSummary           `json:"callers,omitempty"`
	Callees          []CGPSymbolSummary           `json:"callees,omitempty"`
	CallerSites      []CGPCallSite                `json:"callerSites,omitempty"`
	Tests            []CGPSymbolSummary           `json:"tests,omitempty"`
	EstimatedTokens  int                          `json:"-"`
	Truncated        bool                         `json:"truncated,omitempty"`
	Warnings         []string                     `json:"warnings,omitempty"`
}

type InspectSymbolNodeOptions struct {
	BudgetTokens       int
	SourceLines        int
	IncludeTests       bool
	IncludeTestDetails bool
}

type CGPCallSite struct {
	File       string `json:"file"`
	Line       int    `json:"line"`
	Column     int    `json:"column"`
	Raw        string `json:"raw"`
	Caller     string `json:"caller"`
	Confidence string `json:"confidence"`
}

type CGPConfidenceSummary struct {
	Exact      int `json:"exact"`
	Scoped     int `json:"scoped"`
	Heuristic  int `json:"heuristic"`
	Unresolved int `json:"unresolved"`
}

type RepoInfo struct {
	Root      string `json:"root"`
	IndexedAt string `json:"indexedAt"`
	GitCommit string `json:"gitCommit,omitempty"`
}

type File struct {
	ID          string            `json:"id"`
	Path        string            `json:"path"`
	Language    string            `json:"language"`
	SHA256      string            `json:"sha256"`
	LineCount   int               `json:"lineCount"`
	Prefixes    map[string]string `json:"prefixes,omitempty"`
	Parser      string            `json:"parser,omitempty"`      // Parser identifier, e.g. "jsparse-token", "ttl-lex", "tree-sitter-python".
	ParseStatus string            `json:"parseStatus,omitempty"` // ParseStatusOK | ParseStatusPartial | ParseStatusError.
	ParseError  string            `json:"parseError,omitempty"`  // Human-readable error or warning when not OK.
}

type Prefix struct {
	Prefix   string   `json:"prefix"`
	IRI      string   `json:"iri"`
	Location Location `json:"location"`
}

type Term struct {
	ID        string     `json:"id"`
	Term      string     `json:"term"`
	IRI       string     `json:"iri"`
	Prefix    string     `json:"prefix,omitempty"`
	LocalName string     `json:"localName"`
	Locations []Location `json:"locations"`
}

type Shape struct {
	ID            string      `json:"id"`
	TermID        string      `json:"termId"`
	Term          string      `json:"term"`
	IRI           string      `json:"iri"`
	Location      Location    `json:"location"`
	TargetClasses []ShapeLink `json:"targetClasses"`
	Paths         []ShapeLink `json:"paths"`
	Nodes         []ShapeLink `json:"nodes"`
	Predicates    []ShapeLink `json:"predicates,omitempty"`
	Branches      []Branch    `json:"branches,omitempty"`
	Names         []Literal   `json:"names,omitempty"`
	Unsupported   []Location  `json:"unsupported,omitempty"`
}

type Branch struct {
	Kind        string   `json:"kind"`
	Name        string   `json:"name,omitempty"`
	Datatype    string   `json:"datatype,omitempty"`
	DatatypeIRI string   `json:"datatypeIri,omitempty"`
	Pattern     string   `json:"pattern,omitempty"`
	Path        string   `json:"path,omitempty"`
	PathIRI     string   `json:"pathIri,omitempty"`
	Location    Location `json:"location"`
}

type ShapeLink struct {
	Predicate string   `json:"predicate,omitempty"`
	Term      string   `json:"term"`
	IRI       string   `json:"iri"`
	Location  Location `json:"location"`
}

type Reference struct {
	ID          string `json:"id"`
	TermID      string `json:"termId"`
	Term        string `json:"term"`
	IRI         string `json:"iri"`
	File        string `json:"file"`
	StartLine   int    `json:"startLine"`
	StartColumn int    `json:"startColumn"`
	EndLine     int    `json:"endLine"`
	EndColumn   int    `json:"endColumn"`
	Confidence  string `json:"confidence"`
	Kind        string `json:"kind"`
	Context     string `json:"context"`
}

type Edge struct {
	ID         string   `json:"id"`
	From       string   `json:"from"`
	To         string   `json:"to"`
	Type       string   `json:"type"`
	Confidence string   `json:"confidence"`
	Evidence   Location `json:"evidence"`
}

type Location struct {
	File        string `json:"file"`
	StartLine   int    `json:"startLine"`
	StartColumn int    `json:"startColumn"`
	EndLine     int    `json:"endLine"`
	EndColumn   int    `json:"endColumn"`
	Kind        string `json:"kind"`
	Raw         string `json:"raw,omitempty"`
}

type TermSummary struct {
	ID        string `json:"id"`
	Term      string `json:"term"`
	IRI       string `json:"iri"`
	Prefix    string `json:"prefix,omitempty"`
	LocalName string `json:"localName"`
}

type TraceResponse struct {
	Status         string        `json:"status"`
	Query          string        `json:"query"`
	Term           *TermSummary  `json:"term,omitempty"`
	Candidates     []TermSummary `json:"candidates"`
	TTLUsages      []Location    `json:"ttlUsages"`
	CodeReferences []Reference   `json:"codeReferences"`
	Edges          []Edge        `json:"edges"`
	Warnings       []string      `json:"warnings,omitempty"`
}

type TraceCompactResponse struct {
	Status              string           `json:"status"`
	Query               string           `json:"query"`
	Term                *TermSummary     `json:"term,omitempty"`
	Candidates          []TermSummary    `json:"candidates,omitempty"`
	TTLUsageCount       int              `json:"ttlUsageCount"`
	CodeReferenceCount  int              `json:"codeReferenceCount"`
	EdgeCount           int              `json:"edgeCount"`
	DynamicIRICallCount int              `json:"dynamicIriCallCount,omitempty"`
	TTLUsages           []LocationBrief  `json:"ttlUsages"`
	CodeReferences      []ReferenceBrief `json:"codeReferences"`
	Warnings            []string         `json:"warnings,omitempty"`
}

type TraceGroupedCompactResponse struct {
	Status              string                        `json:"status"`
	Query               string                        `json:"query"`
	Term                *TermSummary                  `json:"term,omitempty"`
	Candidates          []TermSummary                 `json:"candidates,omitempty"`
	TTLUsageCount       int                           `json:"ttlUsageCount"`
	CodeReferenceCount  int                           `json:"codeReferenceCount"`
	EdgeCount           int                           `json:"edgeCount"`
	DynamicIRICallCount int                           `json:"dynamicIriCallCount,omitempty"`
	TTLUsages           map[string][]GroupedLocation  `json:"ttlUsages"`
	CodeReferences      map[string][]GroupedReference `json:"codeReferences"`
	Warnings            []string                      `json:"warnings,omitempty"`
}

type GroupedLocation struct {
	Line   int    `json:"line"`
	Column int    `json:"column"`
	Kind   string `json:"kind"`
}

type GroupedReference struct {
	Line       int    `json:"line"`
	Column     int    `json:"column"`
	Confidence string `json:"confidence"`
	Kind       string `json:"kind"`
}

type LocationBrief struct {
	File        string `json:"file"`
	StartLine   int    `json:"startLine"`
	StartColumn int    `json:"startColumn"`
	Kind        string `json:"kind"`
}

type ReferenceBrief struct {
	File        string `json:"file"`
	StartLine   int    `json:"startLine"`
	StartColumn int    `json:"startColumn"`
	Confidence  string `json:"confidence"`
	Kind        string `json:"kind"`
}

type FindReferencesResponse struct {
	Status     string        `json:"status"`
	Query      string        `json:"query"`
	Term       *TermSummary  `json:"term,omitempty"`
	Candidates []TermSummary `json:"candidates"`
	References []Reference   `json:"references"`
	Warnings   []string      `json:"warnings,omitempty"`

	// Symbol, SymbolCandidates, Callers, and Callees are populated as a
	// fallback when the term is not found in the RDF term graph but matches
	// a symbol in the CGP symbol graph (e.g. "useAuth"). SymbolCandidates is
	// set instead of Symbol when the query matches more than one symbol.
	Symbol           *CGPSymbolSummary  `json:"symbol,omitempty"`
	SymbolCandidates []CGPSymbolSummary `json:"symbolCandidates,omitempty"`
	// SymbolCandidateDetails mirrors TraceSymbolResponse.CandidateDetails:
	// a full caller/callee trace per symbol candidate, populated when there
	// are few enough candidates to expand inline.
	SymbolCandidateDetails []TraceSymbolCandidateDetail `json:"symbolCandidateDetails,omitempty"`
	Callers                []CGPSymbolSummary           `json:"callers,omitempty"`
	Callees                []CGPSymbolSummary           `json:"callees,omitempty"`
}

type ListTermsResponse struct {
	Status string        `json:"status"`
	Prefix string        `json:"prefix,omitempty"`
	Terms  []TermSummary `json:"terms"`
}

type QueryOptions struct {
	IncludeWeak bool
}

type FetchSourceResponse struct {
	Status    string `json:"status"`
	File      string `json:"file"`
	StartLine int    `json:"startLine"`
	EndLine   int    `json:"endLine"`
	Text      string `json:"text"`
}

// EventSite is one emit / listen / remove call attributed to its containing
// symbol. SymbolID/Name/Kind describe the *caller* — the function that owns
// the call site — while Location pins the call itself for fetch_source.
type EventSite struct {
	SymbolID   string   `json:"symbolId"`
	SymbolName string   `json:"symbolName,omitempty"`
	SymbolKind string   `json:"symbolKind,omitempty"`
	Confidence string   `json:"confidence"`
	Reason     string   `json:"unresolvedReason,omitempty"`
	Location   Location `json:"location"`
	Raw        string   `json:"raw,omitempty"`
}

// EventCandidate aggregates per-event site counts. Used both as the body of
// list-events and as a fallback when trace-event misses on the exact key.
type EventCandidate struct {
	Event       string `json:"event"`
	TotalSites  int    `json:"totalSites"`
	EmitCount   int    `json:"emitCount,omitempty"`
	ListenCount int    `json:"listenCount,omitempty"`
	RemoveCount int    `json:"removeCount,omitempty"`
}

type TraceEventResponse struct {
	Status     string           `json:"status"`
	Query      string           `json:"query"`
	Event      string           `json:"event,omitempty"`
	Emits      []EventSite      `json:"emits"`
	Listens    []EventSite      `json:"listens"`
	Removes    []EventSite      `json:"removes,omitempty"`
	Candidates []EventCandidate `json:"candidates,omitempty"`
	Warnings   []string         `json:"warnings,omitempty"`
}

type ListEventsResponse struct {
	Status string           `json:"status"`
	Events []EventCandidate `json:"events"`
}

// CGPBenchmarkGold is the on-disk shape of a gold fixture. Each entry names
// a symbol whose caller/callee evidence we want to verify, with optional
// app-code / test splits and must-not-find lines for precision.
type CGPBenchmarkGold struct {
	Description string                          `json:"description,omitempty"`
	Symbols     map[string]CGPBenchmarkGoldSpec `json:"symbols"`
}

// CGPBenchmarkGoldSpec lists expected evidence for one symbol.
//
// Lines are written as "<file>:<startLine>". The fixture format separates
// app-code expectations from test-code expectations because most projects
// can produce 100% app recall but lower test recall (anonymous callbacks,
// ambiguous local symbols, etc.). The split lets us assert on the strong
// guarantee without lying about the weak one.
type CGPBenchmarkGoldSpec struct {
	// FileHint optionally pins the gold spec to a specific file when the
	// name matches multiple symbols across the repo.
	FileHint    string   `json:"file,omitempty"`
	AppCallers  []string `json:"appCallers,omitempty"`
	TestCallers []string `json:"testCallers,omitempty"`
	MustNotFind []string `json:"mustNotFind,omitempty"`
}

// CGPBenchmarkReport is the harness output. Per-symbol breakdown plus an
// aggregate summary fit for CI gating.
type CGPBenchmarkReport struct {
	SchemaVersion string                            `json:"schemaVersion"`
	Repo          string                            `json:"repo,omitempty"`
	GoldPath      string                            `json:"goldPath,omitempty"`
	Results       map[string]CGPBenchmarkSymbolStat `json:"results"`
	Summary       CGPBenchmarkSummary               `json:"summary"`
}

type CGPBenchmarkSymbolStat struct {
	Status          string   `json:"status"` // ok | not_found | ambiguous
	SymbolID        string   `json:"symbolId,omitempty"`
	File            string   `json:"file,omitempty"`
	StartLine       int      `json:"startLine,omitempty"`
	AppExpected     int      `json:"appExpected"`
	AppFound        int      `json:"appFound"`
	AppMissing      []string `json:"appMissing,omitempty"`
	TestExpected    int      `json:"testExpected"`
	TestFound       int      `json:"testFound"`
	TestMissing     []string `json:"testMissing,omitempty"`
	Violations      []string `json:"violations,omitempty"`
	ObservedCallers []string `json:"observedCallers,omitempty"`
	AppRecall       float64  `json:"appRecall"`
	TestRecall      float64  `json:"testRecall"`
}

type CGPBenchmarkSummary struct {
	Symbols       int     `json:"symbols"`
	NotFound      int     `json:"notFound"`
	Ambiguous     int     `json:"ambiguous"`
	AppExpected   int     `json:"appExpected"`
	AppFound      int     `json:"appFound"`
	TestExpected  int     `json:"testExpected"`
	TestFound     int     `json:"testFound"`
	Violations    int     `json:"violations"`
	AppRecall     float64 `json:"appRecall"`
	TestRecall    float64 `json:"testRecall"`
	OverallStatus string  `json:"overallStatus"` // pass | warn | fail
}

// ImpactOptions configures ImpactWithOptions.
type ImpactOptions struct {
	Depth int
	// Compact drops Signature/Docstring/ReturnTypes/hot-path/ID/Language
	// from every layer entry, keeping only Name/Kind/File/StartLine plus
	// PathConfidence/PathReason — see ImpactWithOptions' doc comment.
	Compact bool
}

// ImpactResponse describes the reverse caller closure of a symbol up to a
// fixed depth. It is intended for change-impact analysis: "if I edit symbol
// X, what touches it?". The depth-bounded BFS keeps the answer tractable
// even on huge codebases; agents widen depth on demand.
type ImpactResponse struct {
	Status     string             `json:"status"`
	Query      string             `json:"query"`
	Symbol     *CGPSymbolSummary  `json:"symbol,omitempty"`
	Candidates []CGPSymbolSummary `json:"candidates,omitempty"`
	Depth      int                `json:"depth"`
	Total      int                `json:"total"`
	Layers     []ImpactLayer      `json:"layers"`
	Truncated  bool               `json:"truncated,omitempty"`
	// CoChangedFiles lists files historically changed in the same git commit
	// as the target symbol's file — a "you might also need to touch these"
	// signal that static call-graph analysis alone cannot see (e.g.
	// config/migration pairs). Empty when the repo has no git history.
	CoChangedFiles []CoChangeEntry `json:"coChangedFiles,omitempty"`
	// compactMainSymbol mirrors TraceSymbolResponse's own field of the
	// same name — see its doc comment. Set only by ImpactWithOptions when
	// Compact is requested; ImpactResponse's own MarshalJSON (impact.go)
	// checks it.
	compactMainSymbol bool
}

// CoChangeEntry is one file historically co-changed (same git commit) with
// another file, ranked by how often that co-change occurred.
type CoChangeEntry struct {
	File  string `json:"file"`
	Count int    `json:"count"`
}

type ImpactLayer struct {
	Depth   int            `json:"depth"`
	Symbols []ImpactSymbol `json:"symbols"`
}

type ImpactSymbol struct {
	CGPSymbolSummary
	// PathConfidence is the weakest confidence found along the BFS path from
	// the target to this symbol. Use it to filter for high-trust impact.
	PathConfidence string `json:"pathConfidence"`
	// PathReason is the first unresolved reason encountered on the path.
	// Empty unless PathConfidence == ConfUnresolved.
	PathReason string `json:"pathReason,omitempty"`
}

// DoctorReport surfaces index health for agent triage. It is read-only and
// derived from the loaded index plus a fresh probe of the repo root for
// staleness detection.
type DoctorReport struct {
	Status        string  `json:"status"` // ok | warn | error
	SchemaVersion int     `json:"schemaVersion"`
	RepoRoot      string  `json:"repoRoot"`
	IndexedAt     string  `json:"indexedAt"`
	IndexAgeHours float64 `json:"indexAgeHours"`
	IndexedCommit string  `json:"indexedCommit,omitempty"`
	CurrentCommit string  `json:"currentCommit,omitempty"`
	Stale         bool    `json:"stale"`
	// FilesChangedSinceIndex and FilesDeletedSinceIndex are populated by
	// comparing each indexed file's recorded SHA256 against its current
	// on-disk content. Unlike Stale (git-commit comparison), this also
	// catches uncommitted edits made since the index was built.
	FilesChangedSinceIndex []string               `json:"filesChangedSinceIndex,omitempty"`
	FilesDeletedSinceIndex []string               `json:"filesDeletedSinceIndex,omitempty"`
	Files                  DoctorFileSummary      `json:"files"`
	Symbols                DoctorSymbolSummary    `json:"symbols"`
	Edges                  DoctorEdgeSummary      `json:"edges"`
	Unresolved             DoctorUnresolved       `json:"unresolved"`
	DynamicIRIs            int                    `json:"dynamicIris"`
	IgnorePatterns         []string               `json:"ignorePatterns,omitempty"`
	ParseFailures          []DoctorParseFailure   `json:"parseFailures,omitempty"`
	ParseFailureTotal      int                    `json:"parseFailureTotal,omitempty"`
	ParseFailuresTruncated bool                   `json:"parseFailuresTruncated,omitempty"`
	TopUnresolved          []DoctorUnresolvedItem `json:"topUnresolved,omitempty"`
	Warnings               []string               `json:"warnings,omitempty"`
}

type DoctorFileSummary struct {
	Total      int            `json:"total"`
	ByLanguage map[string]int `json:"byLanguage,omitempty"`
	ByStatus   map[string]int `json:"byStatus,omitempty"`
}

type DoctorSymbolSummary struct {
	Total        int            `json:"total"`
	ByKind       map[string]int `json:"byKind,omitempty"`
	ByConfidence map[string]int `json:"byConfidence,omitempty"`
}

type DoctorEdgeSummary struct {
	Total        int            `json:"total"`
	ByType       map[string]int `json:"byType,omitempty"`
	ByConfidence map[string]int `json:"byConfidence,omitempty"`
}

type DoctorUnresolved struct {
	Total     int            `json:"total"`
	ByReason  map[string]int `json:"byReason,omitempty"`
	UnknownTo int            `json:"unknownTargets"` // distinct unresolved:* targets.
}

type DoctorParseFailure struct {
	File   string `json:"file"`
	Parser string `json:"parser,omitempty"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type DoctorUnresolvedItem struct {
	Target string `json:"target"`
	Reason string `json:"reason,omitempty"`
	Count  int    `json:"count"`
}

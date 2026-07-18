package mamari

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/waelhoury/mamari/internal/mamari/treesitter"
)

// expectedSymbol is one assertion in a language's testdata/expected.json:
// a symbol with this name/kind must exist at this confidence. ParentName, if
// set, asserts the symbol's parent is a same-named symbol — this is the
// check that would have caught a real pilot bug where Ruby methods were
// silently parented to their file instead of their enclosing class, because
// the emitter only consulted Def.ReceiverType (Rust's non-lexical shape) and
// never Def.ParentName (the lexical shape Ruby/Python use). Name/kind/
// confidence alone do not catch a wrong-parent bug, since a method can have
// the right name, kind, and confidence while still being attached to the
// wrong owner — only an explicit parent assertion does.
type expectedSymbol struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Confidence string `json:"confidence"`
	ParentName string `json:"parentName"`
}

// expectedCall asserts a "calls" edge exists from a symbol named FromName to
// a symbol named ToName, at any confidence other than unresolved — i.e. the
// call actually resolved to a real symbol, not just got recorded as an
// edge to a missing/unresolved target.
type expectedCall struct {
	FromName string `json:"fromName"`
	ToName   string `json:"toName"`
}

type languageConformanceFixture struct {
	Language      string           `json:"language"`
	Symbols       []expectedSymbol `json:"symbols"`
	ResolvedCalls []expectedCall   `json:"resolvedCalls"`
}

// TestConformance is the generic, registry-driven regression guard described
// in the language-coverage rollout plan: it iterates every tree-sitter
// language with a registered grammar and, for each one's required
// internal/mamari/treesitter/testdata/<lang>/ fixture, builds a real index
// from that fixture and checks both structural invariants (every symbol ID
// well-formed and unique, every edge resolvable or honestly marked
// unresolved, no parent cycles) and the language's own expected.json
// assertions. Adding tree-sitter language #N means adding a testdata
// fixture — this test's code does not change. Missing fixture data is a test
// failure: otherwise a newly registered language could silently bypass the
// only registry-wide end-to-end symbol, call, parent, and isolation checks.
func TestConformance(t *testing.T) {
	for _, lang := range treesitter.RegisteredLanguages() {
		lang := lang
		t.Run(lang, func(t *testing.T) {
			dir := filepath.Join("treesitter", "testdata", lang)
			fixtureBytes, err := os.ReadFile(filepath.Join(dir, "expected.json"))
			if os.IsNotExist(err) {
				t.Fatalf("registered language %q requires testdata/%s/expected.json", lang, lang)
			}
			if err != nil {
				t.Fatal(err)
			}
			var fixture languageConformanceFixture
			if err := json.Unmarshal(fixtureBytes, &fixture); err != nil {
				t.Fatalf("parse expected.json: %v", err)
			}

			idx := buildIndexFromTestdataDir(t, dir)
			checkStructuralInvariants(t, idx)

			snap := idx.snapshot()
			for _, want := range fixture.Symbols {
				sym, ok := findExpectedSymbol(snap, want, lang)
				if !ok {
					t.Fatalf("expected %s symbol %q (kind %q, parent %q), got none", lang, want.Name, want.Kind, want.ParentName)
				}
				if sym.Confidence != want.Confidence {
					t.Fatalf("expected %s symbol %q confidence %q, got %q", lang, want.Name, want.Confidence, sym.Confidence)
				}
				checkExpectedParent(t, snap, sym, want, lang)
			}
			for _, want := range fixture.ResolvedCalls {
				if !hasResolvedCall(snap, want.FromName, want.ToName, lang) {
					t.Fatalf("expected a resolved (non-unresolved) call edge %s -> %s in %s, found none", want.FromName, want.ToName, lang)
				}
			}
		})
	}
}

// TestCrossLanguageIsolation proves — rather than argues by construction —
// that languages registered together cannot perturb each other's output: it
// builds one repo containing every language's testdata fixture side by
// side, then re-asserts each language's own expected.json against that
// combined index. If adding language B ever changed language A's symbols,
// confidences, or call resolution, this test (not memory, not code review)
// catches it, and it does so automatically for every future language with a
// testdata fixture.
func TestCrossLanguageIsolation(t *testing.T) {
	var fixtures []languageConformanceFixture
	root := t.TempDir()
	for _, lang := range treesitter.RegisteredLanguages() {
		dir := filepath.Join("treesitter", "testdata", lang)
		fixtureBytes, err := os.ReadFile(filepath.Join(dir, "expected.json"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var fixture languageConformanceFixture
		if err := json.Unmarshal(fixtureBytes, &fixture); err != nil {
			t.Fatalf("parse expected.json: %v", err)
		}
		fixtures = append(fixtures, fixture)
		copyTestdataFilesInto(t, root, dir, lang)
	}
	if len(fixtures) < 1 {
		t.Skip("no languages with testdata fixtures registered")
	}

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	checkStructuralInvariants(t, idx)
	snap := idx.snapshot()

	for _, fixture := range fixtures {
		for _, want := range fixture.Symbols {
			sym, ok := findExpectedSymbol(snap, want, fixture.Language)
			if !ok {
				t.Fatalf("[isolation] expected %s symbol %q (kind %q, parent %q) unaffected by other languages, got none", fixture.Language, want.Name, want.Kind, want.ParentName)
			}
			if sym.Confidence != want.Confidence {
				t.Fatalf("[isolation] expected %s symbol %q confidence %q unaffected by other languages, got %q", fixture.Language, want.Name, want.Confidence, sym.Confidence)
			}
			checkExpectedParent(t, snap, sym, want, fixture.Language)
		}
		for _, want := range fixture.ResolvedCalls {
			if !hasResolvedCall(snap, want.FromName, want.ToName, fixture.Language) {
				t.Fatalf("[isolation] expected resolved call %s -> %s in %s unaffected by other languages, found none", want.FromName, want.ToName, fixture.Language)
			}
		}
	}
}

// buildIndexFromTestdataDir copies every file in dir into a fresh temp repo
// and builds an index from it.
func buildIndexFromTestdataDir(t *testing.T, dir string) *Index {
	t.Helper()
	root := t.TempDir()
	copyTestdataFilesInto(t, root, dir, "")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

// copyTestdataFilesInto copies every non-expected.json file from dir into
// root, namespaced under a per-language subdirectory (langPrefix) when
// building a combined multi-language repo so same-named files across
// languages (e.g. two "lib.rs"-shaped fixtures) can never collide.
func copyTestdataFilesInto(t *testing.T, root, dir, langPrefix string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	destDir := root
	if langPrefix != "" {
		destDir = filepath.Join(root, langPrefix)
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == "expected.json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(destDir, e.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// checkExpectedParent asserts sym's parent symbol is named want.ParentName,
// when set. No-op for fixture entries that don't assert parentage.
func checkExpectedParent(t *testing.T, snap indexSnapshot, sym CGPSymbol, want expectedSymbol, lang string) {
	t.Helper()
	if want.ParentName == "" {
		return
	}
	if sym.ParentID == "" {
		t.Fatalf("expected %s symbol %q to be parented under %q, got no parent", lang, want.Name, want.ParentName)
	}
	var parent CGPSymbol
	found := false
	for _, s := range snap.Symbols {
		if s.ID == sym.ParentID {
			parent, found = s, true
			break
		}
	}
	if !found || parent.Name != want.ParentName {
		t.Fatalf("expected %s symbol %q to be parented under %q, got %q", lang, want.Name, want.ParentName, parent.Name)
	}
}

// findExpectedSymbol also considers an asserted parent name. Real codebases
// routinely contain same-named methods under different owners, so selecting
// the first name/kind match would make conformance results depend on file
// iteration order and could validate the wrong definition.
func findExpectedSymbol(snap indexSnapshot, want expectedSymbol, language string) (CGPSymbol, bool) {
	byID := make(map[string]CGPSymbol, len(snap.Symbols))
	for _, sym := range snap.Symbols {
		byID[sym.ID] = sym
	}
	for _, sym := range snap.Symbols {
		if sym.Name != want.Name || sym.Kind != want.Kind || sym.Language != language {
			continue
		}
		if want.ParentName == "" {
			return sym, true
		}
		if parent, ok := byID[sym.ParentID]; ok && parent.Name == want.ParentName {
			return sym, true
		}
	}
	return CGPSymbol{}, false
}

// hasResolvedCall requires an exact language match on both ends because the
// isolation test's combined repo can have identically-named call pairs in two
// different languages' fixtures, and matching by name alone would let one
// language's edge silently satisfy another language's assertion even if that
// language's own resolution were broken.
func hasResolvedCall(snap indexSnapshot, fromName, toName, language string) bool {
	idByID := make(map[string]CGPSymbol, len(snap.Symbols))
	for _, sym := range snap.Symbols {
		idByID[sym.ID] = sym
	}
	for _, e := range snap.SymbolEdges {
		if e.Type != "calls" {
			continue
		}
		if e.Confidence == ConfUnresolved {
			continue
		}
		from, ok := idByID[e.From]
		if !ok || from.Name != fromName || from.Language != language {
			continue
		}
		to, ok := idByID[e.To]
		if !ok || to.Name != toName || to.Language != language {
			continue
		}
		return true
	}
	return false
}

// checkStructuralInvariants asserts properties that must hold for any
// language's output regardless of which parser produced it: well-formed,
// unique symbol IDs, edges that either resolve to a real symbol or are
// honestly marked unresolved (never a dangling silent reference), and
// parent chains that terminate without a cycle.
func checkStructuralInvariants(t *testing.T, idx *Index) {
	t.Helper()
	snap := idx.snapshot()

	seen := make(map[string]bool, len(snap.Symbols))
	byID := make(map[string]CGPSymbol, len(snap.Symbols))
	for _, sym := range snap.Symbols {
		if sym.ID == "" {
			t.Fatalf("symbol %q has empty ID", sym.Name)
		}
		if seen[sym.ID] {
			t.Fatalf("duplicate symbol ID %q", sym.ID)
		}
		seen[sym.ID] = true
		byID[sym.ID] = sym
	}

	for _, e := range snap.SymbolEdges {
		if e.Type != "calls" {
			continue
		}
		if _, ok := byID[e.To]; !ok && e.Confidence != ConfUnresolved {
			t.Fatalf("call edge %s -> %s has no resolvable target but confidence %q (want %q)", e.From, e.To, e.Confidence, ConfUnresolved)
		}
	}

	for _, sym := range snap.Symbols {
		visited := map[string]bool{}
		current := sym
		for current.ParentID != "" {
			if visited[current.ParentID] {
				t.Fatalf("parent cycle detected starting at symbol %q", sym.ID)
			}
			visited[current.ParentID] = true
			parent, ok := byID[current.ParentID]
			if !ok {
				break
			}
			current = parent
		}
	}
}

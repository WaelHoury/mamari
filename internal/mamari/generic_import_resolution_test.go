package mamari

import (
	"path/filepath"
	"testing"
)

func TestGenericImportResolutionRejectsSameNameDecoys(t *testing.T) {
	tests := []struct {
		name       string
		language   string
		from       string
		to         string
		wantFile   string
		wantParent string
	}{
		{
			name:       "elixir alias",
			language:   "elixir",
			from:       "load_user_ex",
			to:         "find_user_ex",
			wantFile:   "user_repo.ex",
			wantParent: "ExUserRepo",
		},
		{
			name:       "php use alias",
			language:   "php",
			from:       "load",
			to:         "find_user",
			wantFile:   "UserRepo.php",
			wantParent: "UserRepo",
		},
		{
			name:     "lua require",
			language: "lua",
			from:     "loadLua",
			to:       "findUserLua",
			wantFile: "repo.lua",
		},
		{
			name:     "haskell qualified import",
			language: "haskell",
			from:     "qualifiedUserHs",
			to:       "qualifiedHelperHs",
			wantFile: "Helper.hs",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx := buildIndexFromTestdataDir(t, filepath.Join("treesitter", "testdata", tt.language))
			assertResolvedCallTarget(t, idx.snapshot(), tt)
		})
	}
}

func assertResolvedCallTarget(t *testing.T, snap indexSnapshot, want struct {
	name       string
	language   string
	from       string
	to         string
	wantFile   string
	wantParent string
}) {
	t.Helper()

	byID := make(map[string]CGPSymbol, len(snap.Symbols))
	var fromIDs []string
	for _, sym := range snap.Symbols {
		byID[sym.ID] = sym
		if sym.Language == want.language && sym.Name == want.from {
			fromIDs = append(fromIDs, sym.ID)
		}
	}
	if len(fromIDs) != 1 {
		t.Fatalf("expected one %s source symbol %q, got %d", want.language, want.from, len(fromIDs))
	}

	var targets []CGPSymbol
	for _, edge := range snap.SymbolEdges {
		if edge.Type != "calls" || edge.From != fromIDs[0] || edge.Confidence == ConfUnresolved {
			continue
		}
		target, ok := byID[edge.To]
		if ok && target.Name == want.to && target.Language == want.language {
			targets = append(targets, target)
		}
	}
	if len(targets) != 1 {
		t.Fatalf("expected one resolved %s call %s -> %s, got %+v", want.language, want.from, want.to, targets)
	}
	target := targets[0]
	if filepath.Base(target.File) != want.wantFile {
		t.Fatalf("resolved %s -> %s to %q, want file %q", want.from, want.to, target.File, want.wantFile)
	}
	if want.wantParent != "" {
		parent, ok := byID[target.ParentID]
		if !ok || parent.Name != want.wantParent {
			t.Fatalf("resolved %s -> %s under parent %q, want %q", want.from, want.to, parent.Name, want.wantParent)
		}
	}
}

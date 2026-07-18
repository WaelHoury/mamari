package mamari

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waelhoury/mamari/internal/mamari/treesitter"
)

func TestLoadIndexRejectsOldSchemaWithRebuildInstruction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.json")
	if err := os.WriteFile(path, []byte(`{"schemaVersion":8}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadIndex(path)
	if err == nil {
		t.Fatal("expected old schema to be rejected")
	}
	message := err.Error()
	for _, want := range []string{"schemaVersion 8", "schemaVersion 10", "mamari index"} {
		if !strings.Contains(message, want) {
			t.Fatalf("schema error %q does not contain %q", message, want)
		}
	}
}

func TestParserMetadataCoversEveryStructuralFrontend(t *testing.T) {
	for _, language := range treesitter.RegisteredLanguages() {
		want := "tree-sitter-" + language
		if got := parserFor(language); got != want {
			t.Errorf("parserFor(%q)=%q, want %q", language, got, want)
		}
	}
	for language, want := range map[string]string{
		"typescript": "jsparse-token",
		"javascript": "jsparse-token",
		"vue":        "jsparse-token",
		"ttl":        "ttl-lex",
		"swift":      "tree-sitter-swift",
		"yaml":       "yaml-ast",
		"dockerfile": "dockerfile-structural",
	} {
		if got := parserFor(language); got != want {
			t.Errorf("parserFor(%q)=%q, want %q", language, got, want)
		}
	}
}

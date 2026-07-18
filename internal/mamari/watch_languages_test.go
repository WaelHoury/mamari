package mamari

import "testing"

func TestWatchRebakeSupportsStructuralAndFallbackLanguages(t *testing.T) {
	tests := []struct {
		file, before, after, expected string
	}{
		{"main.go", "package demo\nfunc Before() {}\n", "package demo\nfunc After() {}\n", "After"},
		{"lib.rs", "pub fn before() {}\n", "pub fn after() {}\n", "after"},
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, tc.file, tc.before)
			idx, err := BuildIndex(root)
			if err != nil {
				t.Fatal(err)
			}
			write(t, root, tc.file, tc.after)
			if err := rebakeFile(idx, root, tc.file); err != nil {
				t.Fatal(err)
			}
			if sym := findSymbolByName(idx, tc.expected); sym.ID == "" {
				t.Fatalf("expected updated %s symbol %q after watch rebake", tc.file, tc.expected)
			}
		})
	}
}

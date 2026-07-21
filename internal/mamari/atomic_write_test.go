package mamari

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteStreamAtomicPreservesExistingFileOnFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "index.json")
	if err := os.WriteFile(path, []byte("old-complete-index"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("injected write failure")
	err := writeStreamAtomic(path, 0o644, func(file *os.File) error {
		if _, err := file.Write([]byte("partial")); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("write error=%v, want %v", err, wantErr)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "old-complete-index" {
		t.Fatalf("failed atomic write replaced target with %q", data)
	}
	matches, err := filepath.Glob(filepath.Join(root, ".index.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files were not cleaned up: %v", matches)
	}
}

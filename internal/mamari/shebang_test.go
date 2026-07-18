package mamari

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtensionlessShellShebangIsIndexedAndRebaked(t *testing.T) {
	root := t.TempDir()
	script := "#!/usr/bin/env bash\nrun_task() { echo first; }\nrun_task\n"
	write(t, root, "bin/run-task", script)
	write(t, root, "bin/not-source", "plain extensionless data\n")

	files, err := WalkRepo(root)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(files, "bin/run-task") {
		t.Fatalf("extensionless shell script missing from walk: %v", files)
	}
	if containsString(files, "bin/not-source") {
		t.Fatalf("extensionless non-source file was indexed: %v", files)
	}

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := idx.Files["bin/run-task"].Language; got != "bash" {
		t.Fatalf("script language = %q, want bash", got)
	}
	if findSymbolByName(idx, "run_task").ID == "" {
		t.Fatal("run_task symbol not indexed")
	}

	updated := "#!/bin/sh\nrun_next() { echo next; }\nrun_next\n"
	if err := os.WriteFile(filepath.Join(root, "bin/run-task"), []byte(updated), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rebakeChangedFiles(idx, root, []string{"bin/run-task"}, nil); err != nil {
		t.Fatal(err)
	}
	if findSymbolByName(idx, "run_next").ID == "" {
		t.Fatal("extensionless shell script was not rebaked")
	}
	if findSymbolByName(idx, "run_task").ID != "" {
		t.Fatal("removed shell function survived rebake")
	}
}

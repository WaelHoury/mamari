package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

func TestResolveVersionMetadataUsesGoInstallBuildInfo(t *testing.T) {
	read := func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			Main: debug.Module{Version: "v0.0.0-20260720194500-c5a160e892c5"},
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "c5a160e892c5abcdef"},
				{Key: "vcs.time", Value: "2026-07-20T19:45:00Z"},
			},
		}, true
	}
	gotVersion, gotCommit, gotDate := resolveVersionMetadata("dev", "unknown", "unknown", read)
	if gotVersion != "v0.0.0-20260720194500-c5a160e892c5" || gotCommit != "c5a160e892c5abcdef" || gotDate != "2026-07-20T19:45:00Z" {
		t.Fatalf("resolved metadata = (%q, %q, %q)", gotVersion, gotCommit, gotDate)
	}
}

func TestResolveVersionMetadataPreservesReleaseLinkerFlags(t *testing.T) {
	read := func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-ignored"}}, true
	}
	gotVersion, gotCommit, gotDate := resolveVersionMetadata("v0.2.0", "release-commit", "release-date", read)
	if gotVersion != "v0.2.0" || gotCommit != "release-commit" || gotDate != "release-date" {
		t.Fatalf("release metadata was replaced: (%q, %q, %q)", gotVersion, gotCommit, gotDate)
	}
}

// initGitRepo initializes a git repo with a fixed commit identity, so tests
// don't depend on the host's global git config.
func initGitRepo(t *testing.T, root string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
}

// TestIndexCommitHooksInstallAndDoctorCheckCommitted covers the end-to-end
// git-portable-index CLI flow: `index --commit` writes a committed JSON
// index, `hooks install` wires up the pre-commit hook and .gitignore, and
// `doctor --check-committed` reports freshness against that committed copy
// without needing the regular (gitignored) .mamari/index.json at all.
func TestIndexCommitHooksInstallAndDoctorCheckCommitted(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)
	if err := os.WriteFile(filepath.Join(root, "app.js"), []byte("function add(a,b){return a+b}\nmodule.exports={add}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := run([]string{"mamari", "index", "--repo", root, "--commit", "--quiet"}); err != nil {
		t.Fatalf("index --commit: %v", err)
	}
	committedPath := filepath.Join(root, ".mamari", "committed", "index.json")
	if _, err := os.Stat(committedPath); err != nil {
		t.Fatalf("expected committed index at %s: %v", committedPath, err)
	}

	if err := run([]string{"mamari", "hooks", "install", "--repo", root}); err != nil {
		t.Fatalf("hooks install: %v", err)
	}
	hookPath := filepath.Join(root, ".git", "hooks", "pre-commit")
	data, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "mamari index --commit") {
		t.Fatalf("expected pre-commit hook to call `mamari index --commit`, got:\n%s", data)
	}

	// Freshly committed index, no on-disk drift yet: doctor --check-committed
	// must succeed.
	if err := run([]string{"mamari", "doctor", "--repo", root, "--check-committed"}); err != nil {
		t.Fatalf("expected doctor --check-committed to pass on a fresh index, got: %v", err)
	}

	// Edit a file without re-indexing: doctor --check-committed must now
	// report staleness and exit non-zero.
	if err := os.WriteFile(filepath.Join(root, "app.js"), []byte("function add(a,b){return a+b+1}\nmodule.exports={add}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"mamari", "doctor", "--repo", root, "--check-committed"}); err == nil {
		t.Fatalf("expected doctor --check-committed to fail after an uncommitted/unindexed edit")
	}

	if err := run([]string{"mamari", "hooks", "uninstall", "--repo", root}); err != nil {
		t.Fatalf("hooks uninstall: %v", err)
	}
	after, err := os.ReadFile(hookPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if strings.Contains(string(after), "mamari index --commit") {
		t.Fatalf("expected hooks uninstall to remove the mamari block, got:\n%s", after)
	}
}

func TestMCPConfigSnippetsUseStdioServe(t *testing.T) {
	claude := claudeMCPConfigSnippet("mamari", "mamari", ".mamari/index.json")
	for _, want := range []string{`"mcpServers"`, `"command": "mamari"`, `"serve"`, `"--index"`} {
		if !strings.Contains(claude, want) {
			t.Fatalf("claude snippet missing %q:\n%s", want, claude)
		}
	}
	if strings.Contains(claude, `"--watch"`) {
		t.Fatalf("watching is the serve default; generated config should not carry a redundant flag:\n%s", claude)
	}

	codex := codexMCPConfigSnippet("mamari", "mamari", ".mamari/index.json")
	for _, want := range []string{`[mcp_servers.mamari]`, `command = "mamari"`, `"serve"`, `"--index"`} {
		if !strings.Contains(codex, want) {
			t.Fatalf("codex snippet missing %q:\n%s", want, codex)
		}
	}
	if strings.Contains(codex, `"--watch"`) {
		t.Fatalf("watching is the serve default; generated config should not carry a redundant flag:\n%s", codex)
	}

	vscode := vscodeMCPConfigSnippet("mamari", "mamari", ".mamari/index.json")
	for _, want := range []string{`"servers"`, `"type": "stdio"`, `"command": "mamari"`, `"--index"`} {
		if !strings.Contains(vscode, want) {
			t.Fatalf("vscode snippet missing %q:\n%s", want, vscode)
		}
	}
}

func TestResolveMCPConfigCommandUsesStableAbsolutePathWhenWriting(t *testing.T) {
	root := t.TempDir()
	name := "mamari"
	if os.PathSeparator == '\\' {
		name += ".exe"
	}
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte("test"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root)

	got, err := resolveMCPConfigCommand("", true)
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Fatalf("resolved command = %q, want %q", got, path)
	}

	printed, err := resolveMCPConfigCommand("", false)
	if err != nil {
		t.Fatal(err)
	}
	if printed != "mamari" {
		t.Fatalf("printed command = %q, want portable mamari", printed)
	}

	explicit, err := resolveMCPConfigCommand("/opt/mamari", true)
	if err != nil {
		t.Fatal(err)
	}
	if explicit != "/opt/mamari" {
		t.Fatalf("explicit command changed to %q", explicit)
	}
}

func TestExpandMCPClientsAcceptsFriendlyAliases(t *testing.T) {
	got := expandMCPClients("claude-code,vscode-copilot,intellij,codex,vs-code")
	want := []string{"claude", "vscode", "jetbrains", "codex"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("expandMCPClients() = %v, want %v", got, want)
	}
	if got := expandMCPClients("unknown"); got != nil {
		t.Fatalf("unknown client expansion = %v, want nil", got)
	}
}

func TestInitMCPWritesValidatedProjectConfigFromOutsideRepo(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.js"), []byte("export function main() { return 42 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	command, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	if err := runInit([]string{
		"--repo", root,
		"--index", "state/index.json",
		"--mcp", "codex",
		"--mcp-command", command,
	}); err != nil {
		t.Fatalf("init --mcp codex: %v", err)
	}

	indexPath := filepath.Join(root, "state", "index.json")
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("expected project-relative index at %s: %v", indexPath, err)
	}
	configPath := filepath.Join(root, ".codex", "config.toml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{command, indexPath, `[mcp_servers.mamari]`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("%s missing %q:\n%s", configPath, want, data)
		}
	}
}

func TestInitMCPPreflightsConflictsBeforeSavingIndex(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.js"), []byte("export const answer = 42\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := "model = \"test\"\n\n[mcp_servers.mamari]\ncommand = \"old\"\nargs = []\n"
	if err := os.WriteFile(configPath, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	command, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	err = runInit([]string{"--repo", root, "--mcp", "codex", "--mcp-command", command})
	if err == nil || !strings.Contains(err.Error(), "--mcp-force") {
		t.Fatalf("init conflict error = %v, want --mcp-force guidance", err)
	}
	indexPath := filepath.Join(root, ".mamari", "index.json")
	if _, statErr := os.Stat(indexPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("conflicting setup should not save index; stat error = %v", statErr)
	}
	data, readErr := os.ReadFile(configPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != existing {
		t.Fatalf("conflicting setup changed existing config:\n%s", data)
	}

	if err := runInit([]string{"--repo", root, "--mcp", "codex", "--mcp-command", command, "--mcp-force"}); err != nil {
		t.Fatalf("init --mcp-force: %v", err)
	}
	data, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `model = "test"`) || !strings.Contains(string(data), command) {
		t.Fatalf("forced setup did not preserve unrelated config and replace mamari:\n%s", data)
	}
}

func TestInitMCPRejectsInvalidExecutableBeforeSavingIndex(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.js"), []byte("export const answer = 42\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missingCommand := filepath.Join(root, "missing-mamari")
	err := runInit([]string{"--repo", root, "--mcp", "codex", "--mcp-command", missingCommand})
	if err == nil || !strings.Contains(err.Error(), "validate mamari command") {
		t.Fatalf("init invalid-command error = %v, want command validation failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, ".mamari", "index.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("invalid executable should not save index; stat error = %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, ".codex", "config.toml")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("invalid executable should not write config; stat error = %v", statErr)
	}
}

func TestSetupMCPRequiresAReadableIndexBeforeWriting(t *testing.T) {
	root := t.TempDir()
	command, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	err = runSetupMCP([]string{
		"--repo", root,
		"--client", "vscode",
		"--command", command,
		"--write",
	})
	if err == nil || !strings.Contains(err.Error(), "validate index") {
		t.Fatalf("setup-mcp missing-index error = %v, want validation failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, ".vscode", "mcp.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("setup-mcp should not write a config for a missing index; stat error = %v", statErr)
	}
}

func TestServeRejectsInvalidMemoryLimitsBeforeLoadingIndex(t *testing.T) {
	tests := []struct {
		name string
		arg  string
		want string
	}{
		{name: "below automatic sentinel", arg: "-2", want: "must be -1, 0, or a positive"},
		{name: "MiB conversion overflow", arg: "8796093022208", want: "too large"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := runServe([]string{"--memory-limit-mb", tc.arg})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runServe(%q) error=%v, want substring %q", tc.arg, err, tc.want)
			}
		})
	}
}

func TestServeWatchesByDefaultWithExplicitOptOut(t *testing.T) {
	defaultConfig, err := parseServeCommand(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !defaultConfig.options.Watch {
		t.Fatal("serve should keep the loaded index live by default")
	}
	if defaultConfig.options.ServerVersion != version {
		t.Fatalf("serve server version=%q, want build version %q", defaultConfig.options.ServerVersion, version)
	}
	if defaultConfig.options.MemoryLimitBytes != -1 {
		t.Fatalf("default memory limit mode=%d, want automatic (-1)", defaultConfig.options.MemoryLimitBytes)
	}

	disabled, err := parseServeCommand([]string{"--watch=false"})
	if err != nil {
		t.Fatal(err)
	}
	if disabled.options.Watch {
		t.Fatal("--watch=false should disable filesystem watching")
	}

	enabledAfterPositional, err := parseServeCommand([]string{"workspace", "--watch"})
	if err != nil {
		t.Fatal(err)
	}
	if !enabledAfterPositional.options.Watch {
		t.Fatal("normalizeFlags should preserve an explicit --watch")
	}
}

func TestRunServeMissingIndexExplainsRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "index.json")
	err := runServe([]string{"--index", path})
	if err == nil {
		t.Fatal("runServe unexpectedly succeeded with a missing index")
	}
	for _, want := range []string{"index not found", path, "mamari init --repo .", "mamari index --repo"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("runServe error %q does not contain %q", err, want)
		}
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runServe error should preserve os.ErrNotExist, got %v", err)
	}
}

func TestWriteMCPConfigRefusesExistingWithoutForce(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".mcp.json")
	if err := writeClaudeMCPConfig(path, "mamari", "mamari", ".mamari/index.json", false); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeMCPConfig(path, "mamari", "mamari", ".mamari/index.json", false); err == nil {
		t.Fatal("expected duplicate write to require --force")
	}
	if err := writeClaudeMCPConfig(path, "mamari", "/opt/mamari", ".mamari/index.json", true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "/opt/mamari") {
		t.Fatalf("forced write did not replace command:\n%s", data)
	}
}

package mamari

import (
	"strings"
	"testing"
)

// a bare call on an import-bound local name must resolve inside the file the
// import statement names, even when the callee's bare name is declared in 2+
// files repo-wide. The fixture uses a monorepo with two parallel apps that
// both declare `useAuth`.

func writeTwoAppUseAuth(t *testing.T, root string) {
	t.Helper()
	for _, app := range []string{"app1", "app2"} {
		write(t, root, app+"/src/composables/useAuth.js", `export function useAuth() {
  return { refresh: () => 1 }
}
`)
		write(t, root, app+"/src/main.js", `import { useAuth } from './composables/useAuth'
export function boot() {
  const { refresh } = useAuth()
  return refresh()
}
`)
	}
}

func TestBareCallImportBindingDisambiguatesDuplicateNames(t *testing.T) {
	root := t.TempDir()
	writeTwoAppUseAuth(t, root)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, app := range []string{"app1", "app2"} {
		trace := TraceSymbol(idx, app+"/src/composables/useAuth.js:useAuth")
		if trace.Status != "found" {
			t.Fatalf("%s: expected found, got %s", app, trace.Status)
		}
		if len(trace.Callers) != 1 || trace.Callers[0].Name != "boot" || trace.Callers[0].File != app+"/src/main.js" {
			t.Fatalf("%s: expected sole caller boot in %s/src/main.js, got %#v (possible=%#v)",
				app, app, trace.Callers, trace.PossibleCallers)
		}
		// The other app's caller must not leak in as a possible caller: the
		// import binding makes each side's call site fully attributed.
		for _, p := range trace.PossibleCallers {
			if !strings.HasPrefix(p.File, app+"/") {
				t.Fatalf("%s: cross-app possibleCaller pollution: %#v", app, p)
			}
		}
	}
}

func TestBareCallImportBindingResolvesInVueScriptSetup(t *testing.T) {
	root := t.TempDir()
	writeTwoAppUseAuth(t, root)
	for _, app := range []string{"app1", "app2"} {
		write(t, root, app+"/src/App.vue", `<script setup>
import { useAuth } from './composables/useAuth'
const { refresh } = useAuth()
</script>
<template><div>hi</div></template>
`)
	}
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	trace := TraceSymbol(idx, "app1/src/composables/useAuth.js:useAuth")
	var appVueCaller bool
	for _, c := range trace.Callers {
		if c.File == "app1/src/App.vue" {
			appVueCaller = true
		}
		if !strings.HasPrefix(c.File, "app1/") {
			t.Fatalf("caller from wrong app: %#v", c)
		}
	}
	if !appVueCaller {
		t.Fatalf("expected app1/src/App.vue among callers, got %#v (possible=%#v)",
			trace.Callers, trace.PossibleCallers)
	}
}

func TestBareCallRenamedNamedImportResolvesViaImportedName(t *testing.T) {
	root := t.TempDir()
	write(t, root, "lib/auth.js", `export function useAuth() { return 1 }
`)
	// A second same-named declaration elsewhere keeps the bare name ambiguous
	// repo-wide, so only the rename-aware binding lookup can resolve this.
	write(t, root, "other/auth.js", `export function useAuth() { return 2 }
`)
	write(t, root, "lib/consumer.js", `import { useAuth as useA } from './auth'
export function run() {
  return useA()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	trace := TraceSymbol(idx, "lib/auth.js:useAuth")
	if len(trace.Callers) != 1 || trace.Callers[0].Name != "run" {
		t.Fatalf("expected renamed import call to resolve to lib/auth.js useAuth, got callers=%#v possible=%#v",
			trace.Callers, trace.PossibleCallers)
	}
}

func TestBareCallRenamedDefaultRequireResolvesViaDefaultExport(t *testing.T) {
	root := t.TempDir()
	write(t, root, "jobs/exportJob.js", `async function runExportJob() { return 'done' }
module.exports = runExportJob
`)
	write(t, root, "other/exportJob.js", `async function runExportJob() { return 'other' }
module.exports = runExportJob
`)
	write(t, root, "scheduler.js", `const run = require('./jobs/exportJob')
async function schedule() {
  await run()
}
module.exports = schedule
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	trace := TraceSymbol(idx, "jobs/exportJob.js:runExportJob")
	if len(trace.Callers) != 1 || trace.Callers[0].Name != "schedule" {
		t.Fatalf("expected renamed default require call to resolve, got callers=%#v possible=%#v",
			trace.Callers, trace.PossibleCallers)
	}
}

func TestBareCallImportBindingBarrelFallsThroughToUniqueName(t *testing.T) {
	root := t.TempDir()
	// The barrel re-exports the implementation; the imported name is not
	// declared in the barrel file itself. The binding lookup must not turn
	// this into unresolved — the repo-wide unique-name fallback still applies.
	write(t, root, "lib/impl.js", `export function doWork() { return 1 }
`)
	write(t, root, "lib/index.js", `export { doWork } from './impl'
`)
	write(t, root, "consumer.js", `import { doWork } from './lib/index'
export function run() {
  return doWork()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	trace := TraceSymbol(idx, "lib/impl.js:doWork")
	if len(trace.Callers) != 1 || trace.Callers[0].Name != "run" {
		t.Fatalf("barrel import must keep resolving via unique-name fallback, got callers=%#v possible=%#v",
			trace.Callers, trace.PossibleCallers)
	}
}

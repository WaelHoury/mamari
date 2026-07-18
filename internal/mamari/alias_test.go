package mamari

import (
	"strings"
	"testing"
)

// tsconfig `paths` alias must resolve cross-file even when the alias is not
// the `@`→src convention.
func TestTsconfigPathsAliasResolves(t *testing.T) {
	root := t.TempDir()
	write(t, root, "tsconfig.json", `{
  "compilerOptions": {
    "baseUrl": ".",
    "paths": {
      "@app/*": ["src/*"],
      "~utils/*": ["src/shared/utils/*"]
    }
  }
}`)
	write(t, root, "src/shared/utils/money.ts", `export function formatMoney(n: number) { return '$' + n }`)
	write(t, root, "src/pages/Bill.ts", `import { formatMoney } from '~utils/money'
export function render() { return formatMoney(10) }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	snap := idx.snapshot()
	for _, e := range snap.SymbolEdges {
		if e.Type == "calls" && strings.HasSuffix(e.To, "money.ts:formatMoney") && e.Confidence != ConfUnresolved {
			return
		}
	}
	t.Fatalf("~utils/* tsconfig alias must resolve formatMoney cross-file; edges=%#v", snap.SymbolEdges)
}

// A vite.config alias from `@` to `./src` must resolve.
func TestViteAliasResolves(t *testing.T) {
	root := t.TempDir()
	write(t, root, "frontend/vite.config.js", `import { fileURLToPath, URL } from 'node:url'
export default {
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
}
`)
	write(t, root, "frontend/src/utils/format.js", `export function fmt(n) { return '' + n }`)
	write(t, root, "frontend/src/components/Bill.vue", `<template><div>{{ fmt(1) }}</div></template>
<script setup>
import { fmt } from '@/utils/format'
const t = fmt(2)
</script>
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	snap := idx.snapshot()
	for _, e := range snap.SymbolEdges {
		if e.Type == "calls" && strings.HasSuffix(e.To, "format.js:fmt") && e.Confidence != ConfUnresolved {
			return
		}
	}
	t.Fatalf("vite '@' alias must resolve fmt cross-file; edges=%#v", snap.SymbolEdges)
}

// A monorepo alias must be scoped: `@/x` from app-a resolves to app-a/src,
// not app-b/src, even though both define `@`.
func TestAliasScopedPerApp(t *testing.T) {
	root := t.TempDir()
	for _, app := range []string{"app-a", "app-b"} {
		write(t, root, app+"/vite.config.js", `export default { resolve: { alias: { '@': './src' } } }`)
	}
	write(t, root, "app-a/src/util.js", `export function onlyA() { return 'a' }`)
	write(t, root, "app-b/src/util.js", `export function onlyB() { return 'b' }`)
	write(t, root, "app-a/src/main.js", `import { onlyA } from '@/util'
export function go() { return onlyA() }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	snap := idx.snapshot()
	for _, e := range snap.SymbolEdges {
		if e.Type == "calls" && strings.Contains(e.To, "util.js:onlyA") && e.Confidence != ConfUnresolved {
			if !strings.HasPrefix(e.To, "symbol:javascript:function:app-a/") {
				t.Fatalf("onlyA must resolve within app-a, got %s", e.To)
			}
			return
		}
	}
	t.Fatalf("scoped @ alias must resolve onlyA within app-a; edges=%#v", snap.SymbolEdges)
}

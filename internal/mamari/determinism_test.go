package mamari

import (
	"encoding/json"
	"fmt"
	"sort"
	"testing"
)

func TestParallelVueCSSLinksDeterministically(t *testing.T) {
	root := t.TempDir()
	const fileCount = 32
	for i := 0; i < fileCount; i++ {
		write(t, root, fmt.Sprintf("src/View%02d.vue", i), `<template><div class="shared">view</div></template>
<style scoped>.shared { color: red; }</style>
`)
	}

	for build := 0; build < 5; build++ {
		idx, err := BuildIndex(root)
		if err != nil {
			t.Fatal(err)
		}
		got := 0
		for _, edge := range idx.SymbolEdges {
			if edge.Type == "uses-css-class" {
				got++
			}
		}
		if got != fileCount {
			t.Fatalf("build %d: uses-css-class edges = %d, want %d", build, got, fileCount)
		}
	}
}

func TestParallelCodeScanUsesFrozenVocabulary(t *testing.T) {
	root := t.TempDir()
	write(t, root, "vocab.ttl", `@prefix pv: <http://example.org/pv/> .
@prefix ex: <http://example.org/> .
ex:subject ex:predicate ex:object .
`)
	write(t, root, "src/discover.ts", `export const state = "pv:draft"
`)
	write(t, root, "src/weak.ts", `export const state = "draft"
`)

	var baseline []string
	for build := 0; build < 12; build++ {
		idx, err := BuildIndex(root)
		if err != nil {
			t.Fatal(err)
		}
		refs := make([]string, 0, len(idx.References))
		for _, ref := range idx.References {
			refs = append(refs, ref.ID)
			if ref.File == "src/weak.ts" && ref.Term == "pv:draft" {
				t.Fatalf("build %d: weak.ts was influenced by a term discovered concurrently: %#v", build, ref)
			}
		}
		sort.Strings(refs)
		if build == 0 {
			baseline = refs
			continue
		}
		if fmt.Sprint(refs) != fmt.Sprint(baseline) {
			t.Fatalf("build %d: reference IDs changed\n got: %v\nwant: %v", build, refs, baseline)
		}
	}
}

func TestOverloadedMethodResolutionIsDeterministicAcrossBuilds(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/Service.kt", `class Service {
    fun work(value: Int) = value
    fun work(value: String) = value.length
    fun run() = this.work(1)
}
`)

	var baseline []byte
	for build := 0; build < 30; build++ {
		idx, err := BuildIndex(root)
		if err != nil {
			t.Fatal(err)
		}
		type endpoint struct {
			From string
			To   string
			Raw  string
		}
		var calls []endpoint
		for _, edge := range idx.SymbolEdges {
			if edge.Type == "calls" {
				calls = append(calls, endpoint{From: edge.From, To: edge.To, Raw: edge.Evidence.Raw})
			}
		}
		if len(calls) == 0 {
			t.Fatalf("build %d produced no call edge for this.work", build)
		}
		sort.Slice(calls, func(i, j int) bool {
			if calls[i].From != calls[j].From {
				return calls[i].From < calls[j].From
			}
			if calls[i].To != calls[j].To {
				return calls[i].To < calls[j].To
			}
			return calls[i].Raw < calls[j].Raw
		})
		got, err := json.Marshal(calls)
		if err != nil {
			t.Fatal(err)
		}
		if build == 0 {
			baseline = got
			continue
		}
		if string(got) != string(baseline) {
			t.Fatalf("build %d produced different call targets:\n got: %s\nwant: %s", build, got, baseline)
		}
	}
}

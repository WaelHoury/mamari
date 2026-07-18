package mamari

import (
	"strings"
	"testing"
)

// A Vue bound attribute whose expression is a literal keyword
// (:disabled="false", v-if="true") can match the template bare-handler shape.
// Literal keywords are never call targets in any language.
func TestVueTemplateLiteralKeywordIsNotACallTarget(t *testing.T) {
	root := t.TempDir()
	write(t, root, "src/Widget.vue", `<script setup>
import { ref } from 'vue'
const open = ref(false)
function toggle() { open.value = !open.value }
</script>
<template>
  <el-dialog v-if="true" :closable="false" :disabled="false" @click="toggle">
    <span v-show="undefined">{{ null }}</span>
  </el-dialog>
</template>
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range idx.SymbolEdges {
		if e.Type != "calls" {
			continue
		}
		name := strings.TrimPrefix(e.To, "unresolved:")
		switch name {
		case "true", "false", "null", "undefined", "NaN", "Infinity":
			t.Fatalf("literal keyword emitted as call target: %#v", e)
		}
	}
	// The legitimate bare handler on the same element must survive the filter.
	var toggleCalled bool
	for _, e := range idx.SymbolEdges {
		if e.Type == "calls" && strings.HasSuffix(e.To, ":toggle") {
			toggleCalled = true
		}
	}
	if !toggleCalled {
		t.Fatalf("@click=\"toggle\" should still produce a call edge")
	}
}

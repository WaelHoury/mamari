package mamari

import (
	"reflect"
	"testing"
)

func TestVueComponentNameSkipsNonLiteralNameProperty(t *testing.T) {
	content := `<script setup>
defineOptions({
  metadata: { name: resolveDisplayName() },
  name: "InvoiceCard",
})
</script>`

	if got := vueComponentName(content); got != "InvoiceCard" {
		t.Fatalf("vueComponentName() = %q, want InvoiceCard", got)
	}
}

func TestTemplateExpressionRangesFindsMultipleNestedExpressions(t *testing.T) {
	raw := "`${first({value: '}'})}-${second(\"}\")}`"
	want := [][2]int{{3, 22}, {26, 37}}

	if got := templateExpressionRanges(raw); !reflect.DeepEqual(got, want) {
		t.Fatalf("templateExpressionRanges() = %#v, want %#v", got, want)
	}
}

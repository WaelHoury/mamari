package mamari

import "testing"

// A regex inside a template interpolation (`${x.replace(/"/g, '""')}`) must
// not desync the lexer. Before the fix the `"` inside the regex was treated
// as a string delimiter, the interpolation's `}` was never matched, and the
// template token ran to EOF — silently dropping every token after it. Verified
// against a real 3,200-line Vue component where this swallowed ~170 lines.
func TestTokenizeTemplateInterpolationWithRegex(t *testing.T) {
	src := "const csv = `\"${(tx.description || '').replace(/\"/g, '\"\"')}\"`\n" +
		"function afterTemplate() { return helper() }\n" +
		"function helper() { return 1 }\n"
	res := ParseJS(src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("expected no parse diagnostics, got %+v", res.Diagnostics)
	}
	// The declarations AFTER the template must still be recovered.
	if defByNameOrNil(res.Symbols, "afterTemplate") == nil {
		t.Fatalf("afterTemplate declaration lost after template-with-regex; symbols=%+v", res.Symbols)
	}
	// The call inside afterTemplate must still be recorded.
	found := false
	for _, c := range res.Calls {
		if c.Callee == "helper" {
			found = true
		}
	}
	if !found {
		t.Fatalf("call after template-with-regex was dropped; calls=%+v", res.Calls)
	}
}

// Nested regex forms that previously desynced: char classes with quotes/braces,
// and a division that must NOT be treated as regex.
func TestTokenizeTemplateInterpolationRegexEdgeCases(t *testing.T) {
	cases := []string{
		"const a = `${s.replace(/[{}\"']/g, '')}`\nfunction z(){ return q() }\nfunction q(){return 1}\n",
		"const b = `${ (x / y) + z }`\nfunction z2(){ return q2() }\nfunction q2(){return 1}\n",
		"const c = `${ `inner ${nested(/a/)}` }`\nfunction z3(){ return q3() }\nfunction q3(){return 1}\n",
	}
	for i, src := range cases {
		res := ParseJS(src)
		if len(res.Diagnostics) != 0 {
			t.Fatalf("case %d: unexpected diagnostics %+v", i, res.Diagnostics)
		}
	}
}

func defByNameOrNil(syms []ScannedSymbol, name string) *ScannedSymbol {
	for i := range syms {
		if syms[i].Name == name {
			return &syms[i]
		}
	}
	return nil
}

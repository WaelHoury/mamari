package treesitter

import "testing"

// A #region license header paired with #pragma directives is valid C# that
// tree-sitter-c-sharp mis-recovers on unless the directive lines are blanked
// first. The blanked copy must still parse
// cleanly and yield the class/method with offsets into the ORIGINAL source.
func TestParseCSharpRegionPragmaDirectives(t *testing.T) {
	src := []byte("#region License\n// copyright\n#endregion\n" +
		"#pragma warning disable 618\n" +
		"namespace N\n{\n" +
		"    public class Widget\n    {\n" +
		"        public void DoThing() { }\n" +
		"    }\n}\n" +
		"#pragma warning restore 618\n")
	res, err := Parse("csharp", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !res.ParseOK {
		t.Fatalf("expected ParseOK for #region+#pragma C#, got error: %s", res.ParseError)
	}
	cls := defByNameMatch(t, res.Defs, "Widget", "class")
	// Offsets index the original, unblanked source, not the blanked copy.
	if got := string(src[cls.NameStart:cls.NameEnd]); got != "Widget" {
		t.Fatalf("class name offset misaligned: got %q", got)
	}
	if res.Package != "N" {
		t.Fatalf("expected namespace N, got %q", res.Package)
	}
	defByNameMatch(t, res.Defs, "DoThing", "method")
}

// A conditional-compilation directive splitting an expression between chained
// calls must not turn the file partial: the
// directive lines are blanked, both branch bodies stay, and the enclosing
// method still parses.
func TestParseCSharpConditionalDirectiveInExpression(t *testing.T) {
	src := []byte("namespace N\n{\n" +
		"    public class C\n    {\n" +
		"        public string M(System.Collections.Generic.IEnumerable<string> names)\n        {\n" +
		"            return string.Join(\", \", names\n" +
		"#if !HAVE_STRING_JOIN_WITH_ENUMERABLE\n" +
		"                .ToArray()\n" +
		"#endif\n" +
		"            );\n" +
		"        }\n" +
		"    }\n}\n")
	res, err := Parse("csharp", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !res.ParseOK {
		t.Fatalf("expected ParseOK for #if-in-expression C#, got error: %s", res.ParseError)
	}
	defByNameMatch(t, res.Defs, "M", "method")
}

// defByNameMatch finds a Def by its simple (last-segment) name and kind,
// avoiding assumptions about C# qualified-name construction.
func defByNameMatch(t *testing.T, defs []Def, name, kind string) Def {
	t.Helper()
	for _, d := range defs {
		if (d.Name == name || lastSegment(d.QualifiedName) == name) && d.Kind == kind {
			return d
		}
	}
	t.Fatalf("no %s named %q in defs %+v", kind, name, defs)
	return Def{}
}

func lastSegment(s string) string {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '.' {
			return s[i+1:]
		}
	}
	return s
}

func TestStripCSharpDirectivesPreservesLength(t *testing.T) {
	src := []byte("#region X\nint a = 1;\n#pragma warning disable 1\n#if FOO\nint b = 2;\n#endif\n")
	out := stripCSharpNonSemanticDirectives(src)
	if len(out) != len(src) {
		t.Fatalf("length not preserved: src=%d out=%d", len(src), len(out))
	}
	// Non-directive lines are untouched.
	if string(out) == string(src) {
		t.Fatalf("expected directive lines to be blanked")
	}
	for _, keep := range []string{"int a = 1;", "int b = 2;"} {
		if !contains(out, keep) {
			t.Fatalf("blanking removed non-directive content %q", keep)
		}
	}
}

func contains(b []byte, s string) bool {
	return len(s) == 0 || (len(b) >= len(s) && indexOf(string(b), s) >= 0)
}
func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

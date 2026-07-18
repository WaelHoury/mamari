package treesitter

import "testing"

// callByCallee finds a resolved call to callee (optionally with receiver).
func hasResolvedCall(calls []Call, receiver, callee string) bool {
	for _, c := range calls {
		if c.Callee == callee && c.Receiver == receiver {
			return true
		}
	}
	return false
}

func TestParseRFunctionsAndQualifiedCalls(t *testing.T) {
	src := []byte("load <- function(p) {\n  clean(read.csv(p))\n}\nclean = function(df) df\nrun <- function() {\n  pkg::helper(x)\n  obj$method(y)\n}\n")
	res, err := Parse("r", src)
	if err != nil || !res.ParseOK {
		t.Fatalf("Parse r: %v ok=%v", err, res.ParseOK)
	}
	for _, name := range []string{"load", "clean", "run"} {
		d := defByName(t, res.Defs, name)
		if d.Kind != "function" {
			t.Fatalf("%s: kind %q want function", name, d.Kind)
		}
	}
	if !hasResolvedCall(res.Calls, "", "clean") {
		t.Fatalf("expected bare call clean(); got %+v", res.Calls)
	}
	if !hasResolvedCall(res.Calls, "pkg", "helper") {
		t.Fatalf("expected pkg::helper() -> recv=pkg callee=helper; got %+v", res.Calls)
	}
	if !hasResolvedCall(res.Calls, "obj", "method") {
		t.Fatalf("expected obj$method() -> recv=obj callee=method; got %+v", res.Calls)
	}
}

func TestParseJuliaFunctionsStructsAndCalls(t *testing.T) {
	src := []byte("module M\nstruct Point\n  x::Int\nend\nfunction dist(a)\n  sq(a)\n  Base.sqrt(a)\nend\nsq(x) = x * x\nend\n")
	res, err := Parse("julia", src)
	if err != nil || !res.ParseOK {
		t.Fatalf("Parse julia: %v ok=%v", err, res.ParseOK)
	}
	defByName(t, res.Defs, "M")
	defByName(t, res.Defs, "M.Point")
	if !hasResolvedCall(res.Calls, "", "sq") {
		t.Fatalf("expected bare call sq(); got %+v", res.Calls)
	}
	if !hasResolvedCall(res.Calls, "Base", "sqrt") {
		t.Fatalf("expected Base.sqrt() -> recv=Base callee=sqrt; got %+v", res.Calls)
	}
	// The function signatures dist(a) / sq(x) must NOT self-call.
	if hasResolvedCall(res.Calls, "", "dist") {
		t.Fatalf("function signature dist(a) leaked a self-call; got %+v", res.Calls)
	}
}

func TestParseZigFunctionsStructsAndCalls(t *testing.T) {
	src := []byte("const Vec = struct {\n  fn norm(self: Vec) f32 { return scale(self.x); }\n};\nfn scale(v: f32) f32 { return v; }\npub fn main() void {\n  var v: Vec = undefined;\n  _ = v.norm();\n}\n")
	res, err := Parse("zig", src)
	if err != nil || !res.ParseOK {
		t.Fatalf("Parse zig: %v ok=%v", err, res.ParseOK)
	}
	vec := defByName(t, res.Defs, "Vec")
	if vec.Kind != "class" {
		t.Fatalf("Vec kind %q want class", vec.Kind)
	}
	defByName(t, res.Defs, "main")
	if !hasResolvedCall(res.Calls, "", "scale") {
		t.Fatalf("expected bare call scale(); got %+v", res.Calls)
	}
	if !hasResolvedCall(res.Calls, "v", "norm") {
		t.Fatalf("expected v.norm() -> recv=v callee=norm; got %+v", res.Calls)
	}
}

// Zig receiver-type resolution: typed parameters and typed var/const
// declarations must yield VarDecls (bare, pointer, and qualified `pkg.Type`),
// which is what lets `recv.method()` resolve to recv's declared type.
func TestParseZigVarTypeExtraction(t *testing.T) {
	src := []byte("fn handle(self: *Server, res: httpz.Response, cfg: Config) void {\n" +
		"  var buf: Buffer = undefined;\n" +
		"  const p: *Pool = pool;\n" +
		"  _ = self; _ = res; _ = cfg; _ = buf; _ = p;\n}\n")
	res, err := Parse("zig", src)
	if err != nil || !res.ParseOK {
		t.Fatalf("Parse zig: %v ok=%v", err, res.ParseOK)
	}
	want := map[string]string{
		"self": "Server",   // pointer unwrapped
		"res":  "Response", // qualified httpz.Response -> Response
		"cfg":  "Config",
		"buf":  "Buffer",
		"p":    "Pool", // pointer var
	}
	got := map[string]string{}
	for _, v := range res.Vars {
		got[v.Name] = v.Type
	}
	for name, typ := range want {
		if got[name] != typ {
			t.Fatalf("var %q: got type %q want %q (all vars: %+v)", name, got[name], typ, res.Vars)
		}
	}
}

func TestParseOCamlFunctionsModulesAndCalls(t *testing.T) {
	src := []byte("let square x = x * x\nlet sum a b = square a + square b\nmodule Geo = struct\n  let area r = List.length r\nend\n")
	res, err := Parse("ocaml", src)
	if err != nil || !res.ParseOK {
		t.Fatalf("Parse ocaml: %v ok=%v", err, res.ParseOK)
	}
	sq := defByName(t, res.Defs, "square")
	if sq.Kind != "function" {
		t.Fatalf("square kind %q want function", sq.Kind)
	}
	defByName(t, res.Defs, "Geo")
	if !hasResolvedCall(res.Calls, "", "square") {
		t.Fatalf("expected bare call square; got %+v", res.Calls)
	}
	if !hasResolvedCall(res.Calls, "List", "length") {
		t.Fatalf("expected List.length -> recv=List callee=length; got %+v", res.Calls)
	}
}

func TestParseHCLBlocksBecomeSymbols(t *testing.T) {
	src := []byte("variable \"region\" {\n  type = string\n}\nlocals {\n  prefix = \"${var.region}-app\"\n}\nresource \"aws_instance\" \"web\" {\n  ami = data.aws_ami.ubuntu.id\n  provider = aws.west\n}\nmodule \"vpc\" {\n  source = \"./vpc\"\n}\ndata \"aws_ami\" \"ubuntu\" {\n  most_recent = true\n}\n")
	res, err := Parse("hcl", src)
	if err != nil || !res.ParseOK {
		t.Fatalf("Parse hcl: %v ok=%v", err, res.ParseOK)
	}
	// Single-label blocks are named by their label; two-label blocks (resource,
	// data) are named by the second label (their own name).
	for _, name := range []string{"region", "web", "vpc", "ubuntu"} {
		defByName(t, res.Defs, name)
	}
	if len(res.HCLBlocks) != 5 {
		t.Fatalf("expected five HCL blocks, got %+v", res.HCLBlocks)
	}
	var foundLocal, foundModuleSource bool
	for _, attr := range res.HCLAttrs {
		if attr.Name == "prefix" {
			foundLocal = true
		}
		if attr.Name == "source" && attr.StaticValue == "./vpc" {
			foundModuleSource = true
		}
	}
	if !foundLocal || !foundModuleSource {
		t.Fatalf("expected local attribute and static module source, got %+v", res.HCLAttrs)
	}
	wantRefs := map[string]bool{
		"var.region":          false,
		"data.aws_ami.ubuntu": false,
		"aws.west":            false,
	}
	for _, ref := range res.HCLRefs {
		joined := ""
		switch {
		case len(ref.Parts) >= 3 && ref.Parts[0] == "data":
			joined = ref.Parts[0] + "." + ref.Parts[1] + "." + ref.Parts[2]
		case len(ref.Parts) >= 2:
			joined = ref.Parts[0] + "." + ref.Parts[1]
		}
		if _, ok := wantRefs[joined]; ok {
			wantRefs[joined] = true
		}
	}
	for ref, found := range wantRefs {
		if !found {
			t.Fatalf("expected traversal %s, got %+v", ref, res.HCLRefs)
		}
	}
}

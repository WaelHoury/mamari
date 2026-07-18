package treesitter

import (
	"sync"
	"testing"
)

// hasCall reports whether calls contains an entry with the given
// receiver/callee, the same matching convention TestParsePythonCalls uses.
func hasCall(calls []Call, receiver, callee string) bool {
	for _, c := range calls {
		if c.Receiver == receiver && c.Callee == callee {
			return true
		}
	}
	return false
}

// TestParsePHPNullsafeMemberCall covers PHP 8's "?->" null-safe operator,
// which the grammar represents as a distinct "nullsafe_member_call_expression"
// node kind, not "member_call_expression" — found via a precision audit that
// deliberately exercised null-safe/safe-navigation operators across every
// language with one, since PHP's calls.scm had no pattern for it at all
// (every other language's safe-navigation operator reuses its normal call
// node kind, making PHP's grammar split the exception, not the rule).
func TestParsePHPNullsafeMemberCall(t *testing.T) {
	src := []byte(`<?php
function top() {
    $a?->b();
    $a?->b?->c();
}
`)
	res, err := Parse("php", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !hasCall(res.Calls, "$a", "b") {
		t.Errorf("missing call $a?->b(), got %+v", res.Calls)
	}
	if !hasCall(res.Calls, "$a?->b", "c") {
		t.Errorf("missing call $a?->b?->c(), got %+v", res.Calls)
	}
}

// TestParseCSharpConditionalAccessCall covers C#'s "?." null-conditional
// operator, represented as "conditional_access_expression" with the member
// name nested inside a "member_binding_expression" — distinct from the
// unconditional "member_access_expression" shape already handled, and
// previously fell through to a default case that left both Receiver and
// Callee empty.
func TestParseCSharpConditionalAccessCall(t *testing.T) {
	src := []byte(`class A {
  void M() {
    a?.b();
    a?.b?.c();
  }
}
`)
	res, err := Parse("csharp", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !hasCall(res.Calls, "a", "b") {
		t.Errorf("missing call a?.b(), got %+v", res.Calls)
	}
	if !hasCall(res.Calls, "a?.b", "c") {
		t.Errorf("missing call a?.b?.c(), got %+v", res.Calls)
	}
}

// TestParseCSharpGenericMethodCallNameIsClean covers a generic method call
// like `list.OfType<Foo>()`: the grammar wraps the method name in a
// "generic_name" node (identifier + type_argument_list), and naively taking
// its full text would pollute Callee with "OfType<Foo>", silently breaking
// every bare-name lookup for the real method name.
func TestParseCSharpGenericMethodCallNameIsClean(t *testing.T) {
	src := []byte(`class A {
  void M() {
    list.OfType<Foo>();
    Foo.Bar<int>();
  }
}
`)
	res, err := Parse("csharp", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !hasCall(res.Calls, "list", "OfType") {
		t.Errorf("expected clean callee %q, got %+v", "OfType", res.Calls)
	}
	if !hasCall(res.Calls, "Foo", "Bar") {
		t.Errorf("expected clean callee %q, got %+v", "Bar", res.Calls)
	}
}

// TestParseRustGenericTypedCallsResolve covers Rust's "::<T>" turbofish
// syntax, which wraps the callee in a "generic_function" node — previously
// unrecognized by every case in the dispatch switch, so the call was
// silently dropped (empty Receiver and Callee) rather than resolved.
func TestParseRustGenericTypedCallsResolve(t *testing.T) {
	src := []byte(`fn top() {
    func::<T>();
    obj.method::<i32>();
}
`)
	res, err := Parse("rust", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !hasCall(res.Calls, "", "func") {
		t.Errorf("missing call func::<T>(), got %+v", res.Calls)
	}
	if !hasCall(res.Calls, "obj", "method") {
		t.Errorf("missing call obj.method::<i32>(), got %+v", res.Calls)
	}
}

// TestParseCppGenericTypedCallsResolveCleanly covers C++'s three distinct
// generic-call shapes — a bare call wraps the callee in "template_function"
// (previously dropped entirely, like Rust's "generic_function"); a method
// call's field is a "template_method" node instead of a plain
// field_identifier (previously polluted Callee with "method<int>"); and a
// qualified static call's scope is a "template_type" instead of a plain
// type_identifier (previously polluted Receiver with "Foo<int>").
func TestParseCppGenericTypedCallsResolveCleanly(t *testing.T) {
	src := []byte(`void top() {
    foo<int>();
    obj.method<int>();
    Foo<int>::bar();
}
`)
	res, err := Parse("cpp", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !hasCall(res.Calls, "", "foo") {
		t.Errorf("missing call foo<int>(), got %+v", res.Calls)
	}
	if !hasCall(res.Calls, "obj", "method") {
		t.Errorf("expected clean callee for obj.method<int>(), got %+v", res.Calls)
	}
	if !hasCall(res.Calls, "Foo", "bar") {
		t.Errorf("expected clean receiver for Foo<int>::bar(), got %+v", res.Calls)
	}
}

// TestParseKotlinGenericParameterTypeIsClean covers a generic-typed
// parameter (`items: List<Foo>`): Kotlin's "user_type" node is the type
// node for every declared type, generic or not, and previously had no case
// in simpleTypeName at all, so a generic type's full "List<Foo>" text
// leaked into VarDecl.Type — silently breaking type-based call resolution
// for any variable of a generic type (the single most common parameter/
// property type shape in idiomatic Kotlin, since collections are
// everywhere). Plain, non-generic types must still resolve correctly too.
func TestParseKotlinGenericParameterTypeIsClean(t *testing.T) {
	src := []byte(`fun top(items: List<Foo>) {}
class A(private val repo: UserRepo) {}
`)
	res, err := Parse("kotlin", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var sawGeneric, sawPlain bool
	for _, v := range res.Vars {
		if v.Name == "items" && v.Type == "List" {
			sawGeneric = true
		}
		if v.Name == "repo" && v.Type == "UserRepo" {
			sawPlain = true
		}
	}
	if !sawGeneric {
		t.Errorf("expected clean type %q for generic parameter, got %+v", "List", res.Vars)
	}
	if !sawPlain {
		t.Errorf("expected plain type %q to still resolve, got %+v", "UserRepo", res.Vars)
	}
}

// TestParseCppGenericBaseClassResolves covers `class Foo : public Bar<int>`:
// a generic base class is a "template_type" node, which cppClassBaseNames
// had no case for, so the base was silently dropped from Def.Bases entirely
// (not polluted — just missing) rather than resolving to "Bar".
func TestParseCppGenericBaseClassResolves(t *testing.T) {
	src := []byte(`class Foo : public Bar<int> {};`)
	res, err := Parse("cpp", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	d := defByName(t, res.Defs, "Foo")
	if len(d.Bases) != 1 || d.Bases[0] != "Bar" {
		t.Errorf("expected Bases [Bar], got %v", d.Bases)
	}
}

// TestParseCppValueTypeFieldIsVarDecl covers a value-type (non-pointer)
// class/struct member like `Repo repo;`. Its declarator is a
// "field_identifier" node, a distinct kind from the "identifier" used for
// free variables and parameters in tree-sitter-c/cpp's grammar — found via
// a same-name-collision resolution test where `repo.find(id)` failed to
// resolve to the right `find` despite `Repo repo;` being declared right
// there, because cDeclaratorName only ever accepted "identifier".
func TestParseCppValueTypeFieldIsVarDecl(t *testing.T) {
	src := []byte(`
class Service {
    Repo repo;
public:
    int load(int id) { return repo.find(id); }
};
`)
	res, err := Parse("cpp", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var found bool
	for _, v := range res.Vars {
		if v.Name == "repo" && v.Type == "Repo" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a VarDecl{Name:repo, Type:Repo}, got %+v", res.Vars)
	}
}

// TestParseKotlinBodyPropertyIsVarDecl covers a body-level `var`/`val`
// property (`property_declaration`), distinct from a primary-constructor
// `class_parameter` — found missing via a same-name-collision test where
// `repo.find(id)` failed to type-resolve despite the type annotation being
// right there, because only class_parameter/parameter were captured.
// Covers both the explicit-type and inferred-from-constructor-call shapes.
func TestParseKotlinBodyPropertyIsVarDecl(t *testing.T) {
	src := []byte(`
class Service {
    var repo: Repo
    var other = Repo()
    private val client: Repo = Repo()
}
`)
	res, err := Parse("kotlin", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// "private val client: ..." was found broken on real-world code
	// (OkHttp's CacheResponse.kt): a leading "modifiers" child shifts
	// "variable_declaration" off position 0, the same hazard
	// class_parameter/parameter already guard against by indexing from the
	// end — property_declaration didn't, until this fixture caught it.
	want := map[string]string{"repo": "Repo", "other": "Repo", "client": "Repo"}
	got := map[string]string{}
	for _, v := range res.Vars {
		got[v.Name] = v.Type
	}
	for name, typ := range want {
		if got[name] != typ {
			t.Errorf("expected VarDecl{Name:%s, Type:%s}, got %+v", name, typ, res.Vars)
		}
	}
}

// TestParseScalaBodyPropertyIsVarDecl is the Scala analogue of
// TestParseKotlinBodyPropertyIsVarDecl: a body-level `var`/`val_definition`
// property is a different node kind from class_parameter, with the bound
// name under field "pattern" rather than "name".
func TestParseScalaBodyPropertyIsVarDecl(t *testing.T) {
	src := []byte(`
class Service {
  var repo: Repo = new Repo()
  val other = new Repo()
}
`)
	res, err := Parse("scala", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := map[string]string{"repo": "Repo", "other": "Repo"}
	got := map[string]string{}
	for _, v := range res.Vars {
		got[v.Name] = v.Type
	}
	for name, typ := range want {
		if got[name] != typ {
			t.Errorf("expected VarDecl{Name:%s, Type:%s}, got %+v", name, typ, res.Vars)
		}
	}
}

// TestParseDartClassFieldIsVarDecl covers Dart's two idiomatic
// dependency-typing shapes — Dart had zero var-tracking at all before this:
// an explicitly-typed field (`Repo repo;`, the field half of the
// constructor-shorthand DI pattern `Service(this.repo);`) and an untyped
// `var` field whose type is inferred from a same-statement bare-call
// initializer (`var repo = Repo();`). Both use the same "declaration" node
// kind Dart's body-less-constructor tags.scm pattern also uses — must not
// collide, discriminated by the first child's kind (constructor_signature
// there, never a type node here).
func TestParseDartClassFieldIsVarDecl(t *testing.T) {
	src := []byte(`
class Service {
  Repo repo;
  var other = Repo();
}
`)
	res, err := Parse("dart", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := map[string]string{"repo": "Repo", "other": "Repo"}
	got := map[string]string{}
	for _, v := range res.Vars {
		got[v.Name] = v.Type
	}
	for name, typ := range want {
		if got[name] != typ {
			t.Errorf("expected VarDecl{Name:%s, Type:%s}, got %+v", name, typ, res.Vars)
		}
	}
}

// TestParseLuaSelfAttributeAssignmentIsVarDecl covers Lua's idiomatic
// dependency-construction pattern (`self.repo = Repo.new()`, e.g. inside a
// `Service.new()` factory) — found completely missing during a benchmark
// against competitor tools. ChildByFieldName("name")/("value") on
// "assignment_statement" return the single target/value directly for a
// single-target assignment (confirmed via direct AST inspection) — a
// "variable_list"/"expression_list" wrapper only appears for multi-target
// assignments (`a, b = 1, 2`), not this shape.
func TestParseLuaSelfAttributeAssignmentIsVarDecl(t *testing.T) {
	src := []byte(`
function Service.new()
  local self = setmetatable({}, Service)
  self.repo = Repo.new()
  return self
end
`)
	res, err := Parse("lua", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var found bool
	for _, v := range res.Vars {
		if v.Name == "self.repo" && v.Type == "Repo" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a VarDecl{Name:self.repo, Type:Repo}, got %+v", res.Vars)
	}
}

// TestParsePHPThisAttributeAssignmentIsVarDecl covers PHP's untyped-property
// convention (`private $repo;` with no type hint, still extremely common in
// PHP 5/7-style code) — `property_declaration` only has evidence when an
// explicit type hint is present, so the type can only come from the
// constructor assignment site (`$this->repo = new Repo();`).
func TestParsePHPThisAttributeAssignmentIsVarDecl(t *testing.T) {
	src := []byte(`<?php
class Service {
    private $repo;
    function __construct() {
        $this->repo = new Repo();
    }
}
`)
	res, err := Parse("php", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var found bool
	for _, v := range res.Vars {
		if v.Name == "$this->repo" && v.Type == "Repo" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a VarDecl{Name:$this->repo, Type:Repo}, got %+v", res.Vars)
	}
}

// TestParsePHPConstructorPropertyPromotionIsVarDecl covers PHP 8's
// constructor property promotion (`__construct(private Repo $repo)`) — the
// dominant modern PHP idiom (Laravel and Monolog have both fully moved to
// it; neither had a single explicit `$this->x = new Y();` left anywhere).
// It is both the parameter declaration and an implicit `$this->repo =
// $repo` assignment with no literal "$this->" text anywhere — handled by
// pre-prefixing the returned Name with "$this->" so it flows through the
// existing populateSelfAttributeVarTypes promotion with no new plumbing.
func TestParsePHPConstructorPropertyPromotionIsVarDecl(t *testing.T) {
	src := []byte(`<?php
class Service {
    public function __construct(private Repo $repo) {}
}
`)
	res, err := Parse("php", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var found bool
	for _, v := range res.Vars {
		if v.Name == "$this->repo" && v.Type == "Repo" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a VarDecl{Name:$this->repo, Type:Repo}, got %+v", res.Vars)
	}
}

// TestParsePHPThisAttributeAliasOfParameterMatchesFormat covers a real bug
// found via real-world testing (Laravel's Migrator: `$this->files =
// $files;` where `$files` is typed by a constructor parameter, not
// constructed inline) — simple_parameter's own Name keeps PHP's "$" sigil
// ("$files"), but the assignment case's AliasOf previously stripped it
// ("files"), so the alias lookup could never match and silently produced
// no type at all. Both must agree on the same format.
func TestParsePHPThisAttributeAliasOfParameterMatchesFormat(t *testing.T) {
	src := []byte(`<?php
class Service {
    protected $repo;
    public function __construct(Repo $repo) {
        $this->repo = $repo;
    }
}
`)
	res, err := Parse("php", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var paramName, aliasOf string
	for _, v := range res.Vars {
		if v.Name == "$repo" && v.Type == "Repo" {
			paramName = v.Name
		}
		if v.Name == "$this->repo" && v.AliasOf != "" {
			aliasOf = v.AliasOf
		}
	}
	if paramName == "" || aliasOf == "" || paramName != aliasOf {
		t.Errorf("expected parameter Name and assignment AliasOf to match exactly, got param=%q aliasOf=%q (full vars: %+v)", paramName, aliasOf, res.Vars)
	}
}

// TestParseConcurrentSameLanguageIsRaceFree stress-tests the per-language
// compiledLang cache (lang/tagsQuery/callsQuery shared, read-only, across
// concurrently-running parses) added to fix a real performance bug found
// while indexing a real-world repo: every Parse call previously rebuilt
// the Language and both Queries from scratch (~47% of total parse time on
// a profiled 10,000-line file came from ts_query_new alone). BuildIndex
// parses many files of the same language concurrently via runParallel;
// this runs many goroutines through Parse for the same language at once,
// repeatedly, under -race, to catch any concurrent-misuse bug the shared
// cache could introduce that the existing (mostly sequential/small)
// fixture tests wouldn't exercise.
func TestParseConcurrentSameLanguageIsRaceFree(t *testing.T) {
	sources := []string{
		`package app
type Repo struct{}
func (r *Repo) Find(id int) int { return id }
`,
		`package app
type Service struct{}
func (s *Service) Run() error { return nil }
`,
		`package app
func Helper(x int) int { return x * 2 }
`,
	}
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for round := 0; round < 20; round++ {
		for _, src := range sources {
			wg.Add(1)
			go func(src string) {
				defer wg.Done()
				if _, err := Parse("go", []byte(src)); err != nil {
					errs <- err
				}
			}(src)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Parse error: %v", err)
	}
}

// TestParseCppThreadSafetyAnnotationFieldParsesCleanly covers a real
// correctness gap found via real-world verification: indexing
// `google/leveldb` flagged 33 of 134 files (24.6%) as parse failures,
// almost entirely from Clang thread-safety annotations
// (https://clang.llvm.org/docs/ThreadSafetyAnalysis.html) — real, valid,
// extremely common modern C++ (also used by Abseil, Chromium, gRPC) that
// tree-sitter-cpp can't parse without the macro expansion a real compiler
// performs. `GUARDED_BY(mu)` in a field declarator position previously
// broke the whole surrounding struct.
func TestParseCppThreadSafetyAnnotationFieldParsesCleanly(t *testing.T) {
	src := `struct IterState {
  port::Mutex* const mu;
  Version* const version GUARDED_BY(mu);
  MemTable* const mem GUARDED_BY(mu);
};
`
	res, err := Parse("cpp", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if !res.ParseOK {
		t.Fatalf("expected struct with GUARDED_BY field annotations to parse cleanly, got ParseError=%q", res.ParseError)
	}
}

// TestParseCppThreadSafetyAnnotationMethodParsesCleanly covers the
// method-level shape of the same gap (`LOCKS_EXCLUDED(mu_)` between a
// method's parameter list and its body).
func TestParseCppThreadSafetyAnnotationMethodParsesCleanly(t *testing.T) {
	src := `class AtomicCounter {
 public:
  int Read() LOCKS_EXCLUDED(mu_) {
    return count_;
  }
};
`
	res, err := Parse("cpp", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if !res.ParseOK {
		t.Fatalf("expected method with LOCKS_EXCLUDED annotation to parse cleanly, got ParseError=%q", res.ParseError)
	}
}

// TestParseCppExportMacroBeforeClassNameParsesCleanly covers the other
// dominant real-world shape found in the same leveldb verification pass:
// a library export/visibility macro (`LEVELDB_EXPORT`) between `class`
// and the class name — present in nearly every public header of a
// library with a shared/DLL build mode.
func TestParseCppExportMacroBeforeClassNameParsesCleanly(t *testing.T) {
	src := `class LEVELDB_EXPORT Status {
 public:
  Status() = default;
};

LEVELDB_EXPORT Status DestroyDB(const std::string& name);
`
	res, err := Parse("cpp", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if !res.ParseOK {
		t.Fatalf("expected LEVELDB_EXPORT-annotated class/function to parse cleanly, got ParseError=%q", res.ParseError)
	}
}

// TestParseCppAnnotationMacroDefinitionItselfNotCorrupted is a regression
// guard for a real bug found while building the two fixes above: the
// stripper must not blank a macro name out of its *own*
// `#define`/`#ifndef` declaration line (`#define GUARDED_BY(x) ...`,
// `#ifndef GUARDED_BY`) — doing so corrupts the preprocessor directive
// itself (shifting the macro body into the name position), which broke
// `leveldb/port/thread_annotations.h` and `leveldb/include/leveldb/
// export.h` — both of which previously parsed fine — the moment the
// stripping fix was first added, before this guard existed.
func TestParseCppAnnotationMacroDefinitionItselfNotCorrupted(t *testing.T) {
	src := `#ifndef GUARDED_BY
#define GUARDED_BY(x) __attribute__((guarded_by(x)))
#endif

#if !defined(LEVELDB_EXPORT)
#define LEVELDB_EXPORT __attribute__((visibility("default")))
#endif

class LEVELDB_EXPORT Foo {
 public:
  int* const x GUARDED_BY(mu);
  port::Mutex* const mu;
};
`
	res, err := Parse("cpp", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if !res.ParseOK {
		t.Fatalf("expected file defining its own annotation macros to still parse cleanly, got ParseError=%q", res.ParseError)
	}
	foo := defByName(t, res.Defs, "Foo")
	if foo.Kind != "class" {
		t.Fatalf("expected Foo to still be recognized as a class def, got %#v", foo)
	}
}

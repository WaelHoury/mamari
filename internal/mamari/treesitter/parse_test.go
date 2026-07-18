package treesitter

import (
	"strings"
	"testing"
)

func defByName(t *testing.T, defs []Def, qualifiedName string) Def {
	t.Helper()
	for _, d := range defs {
		if d.QualifiedName == qualifiedName {
			return d
		}
	}
	t.Fatalf("no def with QualifiedName %q in %+v", qualifiedName, defs)
	return Def{}
}

func TestParsePythonNestedClassAndMethods(t *testing.T) {
	src := []byte(`class Outer:
    class Inner:
        def method(self):
            pass

    def top(self):
        pass

def standalone():
    pass
`)
	res, err := Parse("python", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !res.ParseOK {
		t.Fatalf("expected ParseOK, got error: %s", res.ParseError)
	}

	outer := defByName(t, res.Defs, "Outer")
	if outer.Kind != "class" || outer.ParentName != "" {
		t.Fatalf("Outer: got %+v", outer)
	}

	inner := defByName(t, res.Defs, "Outer.Inner")
	if inner.Kind != "class" || inner.ParentName != "Outer" || inner.ParentKind != "class" {
		t.Fatalf("Inner: got %+v", inner)
	}

	method := defByName(t, res.Defs, "Outer.Inner.method")
	if method.Kind != "method" || method.ParentName != "Inner" || method.ParentKind != "class" {
		t.Fatalf("method: got %+v", method)
	}

	top := defByName(t, res.Defs, "Outer.top")
	if top.Kind != "method" || top.ParentName != "Outer" {
		t.Fatalf("top: got %+v", top)
	}

	standalone := defByName(t, res.Defs, "standalone")
	if standalone.Kind != "function" || standalone.ParentName != "" {
		t.Fatalf("standalone: got %+v", standalone)
	}
}

func TestParsePythonDecoratorsIncludedInRange(t *testing.T) {
	src := []byte(`@staticmethod
@app.route("/x")
def handler():
    pass
`)
	res, err := Parse("python", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	handler := defByName(t, res.Defs, "handler")
	if handler.Start != 0 {
		t.Fatalf("expected handler.Start == 0 (decorators included), got %d", handler.Start)
	}
	if src[handler.Start] != '@' {
		t.Fatalf("expected handler range to start at '@', got %q", src[handler.Start])
	}
}

func TestParsePythonAsyncDef(t *testing.T) {
	src := []byte(`async def fetch():
    pass
`)
	res, err := Parse("python", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	fetch := defByName(t, res.Defs, "fetch")
	if fetch.Kind != "function" {
		t.Fatalf("fetch: got %+v", fetch)
	}
	if string(src[fetch.Start:fetch.Start+5]) != "async" {
		t.Fatalf("expected fetch range to start at 'async', got %q", src[fetch.Start:fetch.Start+5])
	}
}

func TestParsePythonMultiLineSignature(t *testing.T) {
	src := []byte(`def configure(
    name: str,
    value: int = 0,
) -> bool:
    return True
`)
	res, err := Parse("python", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	configure := defByName(t, res.Defs, "configure")
	if configure.Kind != "function" {
		t.Fatalf("configure: got %+v", configure)
	}
	// The def's range must span the whole multi-line signature plus body.
	if configure.End <= configure.Start+len("def configure(") {
		t.Fatalf("expected configure range to span multi-line signature, got %+v", configure)
	}
}

func TestParsePythonSameNamedMethodsInDifferentClasses(t *testing.T) {
	src := []byte(`class A:
    def process(self):
        pass

class B:
    def process(self):
        pass
`)
	res, err := Parse("python", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	a := defByName(t, res.Defs, "A.process")
	b := defByName(t, res.Defs, "B.process")
	if a.QualifiedName == b.QualifiedName {
		t.Fatalf("expected distinct qualified names, got %q and %q", a.QualifiedName, b.QualifiedName)
	}
	if a.ParentStart == b.ParentStart {
		t.Fatalf("expected distinct ParentStart for A.process and B.process")
	}
}

func TestParsePythonLambdaCallbackSymbol(t *testing.T) {
	src := []byte(`def outer():
    handler = lambda x: x + 1
    return handler(5)

callback = lambda: None
`)
	res, err := Parse("python", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	handler := defByName(t, res.Defs, "outer.handler")
	if handler.Kind != "callback" || handler.ParentName != "outer" || handler.ParentKind != "function" {
		t.Fatalf("handler: got %+v", handler)
	}

	top := defByName(t, res.Defs, "callback")
	if top.Kind != "callback" || top.ParentName != "" {
		t.Fatalf("callback: got %+v", top)
	}
}

// TestParsePythonComprehensionCallsAttributeToEnclosingFunction guards a call
// inside a comprehension: such calls are not captured as their own
// definitions (comprehensions are deferred per the plan), so they must still
// fall within the enclosing function's Start/End range for line-based
// attribution (containingSymbolFast) to work.
func TestParsePythonComprehensionCallsAttributeToEnclosingFunction(t *testing.T) {
	src := []byte(`def run(items):
    return [transform(i) for i in items]
`)
	res, err := Parse("python", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	run := defByName(t, res.Defs, "run")

	var call Call
	found := false
	for _, c := range res.Calls {
		if c.Callee == "transform" {
			call = c
			found = true
		}
	}
	if !found {
		t.Fatalf("missing call transform(i) in %+v", res.Calls)
	}
	if call.Start < run.Start || call.End > run.End {
		t.Fatalf("expected transform() call within run()'s range, call=%+v run=%+v", call, run)
	}
}

func TestParsePythonCalls(t *testing.T) {
	src := []byte(`def run():
    helper()
    self.process()
    cls.build()
    obj.method()
`)
	res, err := Parse("python", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var sawHelper, sawSelfProcess, sawClsBuild, sawObjMethod bool
	for _, c := range res.Calls {
		switch {
		case c.Callee == "helper" && c.Receiver == "":
			sawHelper = true
		case c.Callee == "process" && c.Receiver == "self":
			sawSelfProcess = true
		case c.Callee == "build" && c.Receiver == "cls":
			sawClsBuild = true
		case c.Callee == "method" && c.Receiver == "obj":
			sawObjMethod = true
		}
	}
	if !sawHelper {
		t.Errorf("missing call helper()")
	}
	if !sawSelfProcess {
		t.Errorf("missing call self.process()")
	}
	if !sawClsBuild {
		t.Errorf("missing call cls.build()")
	}
	if !sawObjMethod {
		t.Errorf("missing call obj.method()")
	}
}

func TestParseGoQualifiedParameterType(t *testing.T) {
	res, err := Parse("go", []byte(`package sample
import "testing"
func verify(t *testing.T) { t.Fatal("failed") }
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, variable := range res.Vars {
		if variable.Name == "t" {
			if variable.Type != "testing.T" {
				t.Fatalf("unexpected qualified Go parameter type %q in %+v", variable.Type, res.Vars)
			}
			return
		}
	}
	t.Fatalf("qualified Go parameter was not extracted: %+v", res.Vars)
}

func TestParsePythonImports(t *testing.T) {
	src := []byte(`import os
import os.path as osp
from collections import OrderedDict
from .models import User
from . import db
from ..pkg.sub import Thing as T
`)
	res, err := Parse("python", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	type want struct {
		spec, alias string
	}
	wants := []want{
		{"os", ""},
		{"os.path", "osp"},
		{"collections", ""},
		{".models", ""},
		{".", ""},
		{"..pkg.sub", "T"},
	}

	for _, w := range wants {
		found := false
		for _, imp := range res.Imports {
			if imp.Spec == w.spec && imp.Alias == w.alias {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing import {Spec: %q, Alias: %q} in %+v", w.spec, w.alias, res.Imports)
		}
	}
}

func TestParseGenericAliasedImportsAndQualifiedCalls(t *testing.T) {
	tests := []struct {
		name         string
		language     string
		source       string
		wantImport   Import
		wantReceiver string
		wantCallee   string
	}{
		{
			name:         "elixir alias",
			language:     "elixir",
			source:       "defmodule Service do\n  alias UserRepo, as: Repo\n  def load(id), do: Repo.find(id)\nend\n",
			wantImport:   Import{Spec: "UserRepo", Alias: "Repo"},
			wantReceiver: "Repo",
			wantCallee:   "find",
		},
		{
			name:         "haskell qualified import",
			language:     "haskell",
			source:       "module Service where\nimport qualified UserRepo as Repo\nload id = Repo.find id\n",
			wantImport:   Import{Spec: "UserRepo", Alias: "Repo"},
			wantReceiver: "Repo",
			wantCallee:   "find",
		},
		{
			name:         "lua require",
			language:     "lua",
			source:       "local Repo = require(\"user_repo\")\nlocal function load(id)\n  return Repo.find(id)\nend\n",
			wantImport:   Import{Spec: "user_repo", Alias: "Repo"},
			wantReceiver: "Repo",
			wantCallee:   "find",
		},
		{
			name:         "php use alias",
			language:     "php",
			source:       "<?php\nuse App\\Repos\\UserRepo as Repo;\nfunction load($id) { return Repo::find($id); }\n",
			wantImport:   Import{Spec: "App\\Repos\\UserRepo", Alias: "Repo"},
			wantReceiver: "Repo",
			wantCallee:   "find",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := Parse(tt.language, []byte(tt.source))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			foundImport := false
			for _, imp := range res.Imports {
				if imp.Spec == tt.wantImport.Spec && imp.Alias == tt.wantImport.Alias {
					foundImport = true
					break
				}
			}
			if !foundImport {
				t.Fatalf("missing import %+v in %+v", tt.wantImport, res.Imports)
			}
			foundCall := false
			for _, call := range res.Calls {
				if call.Receiver == tt.wantReceiver && call.Callee == tt.wantCallee {
					foundCall = true
					break
				}
			}
			if !foundCall {
				t.Fatalf("missing call %s.%s in %+v", tt.wantReceiver, tt.wantCallee, res.Calls)
			}
		})
	}
}

func TestParseCppLocalConstruction(t *testing.T) {
	src := []byte(`class Repo {
public:
    Repo(int);
    void find();
};
void use() {
    Repo a(1);
    Repo b{2};
    Repo c = Repo(3);
    int x(5);
    a.find();
}
`)
	res, err := Parse("cpp", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	vars := map[string]string{}
	for _, decl := range res.Vars {
		vars[decl.Name] = decl.Type
	}
	for _, name := range []string{"a", "b", "c"} {
		if vars[name] != "Repo" {
			t.Fatalf("local %s type = %q, want Repo; vars=%+v", name, vars[name], vars)
		}
	}
	if vars["x"] != "int" {
		t.Fatalf("primitive local type = %q, want int; vars=%+v", vars["x"], vars)
	}

	constructors := map[string]int{}
	for _, call := range res.Calls {
		if call.Constructor {
			constructors[call.Callee]++
		}
	}
	if constructors["Repo"] != 2 {
		t.Fatalf("Repo direct constructors = %d, want 2; calls=%+v", constructors["Repo"], res.Calls)
	}
	if constructors["int"] != 1 {
		t.Fatalf("int direct initializers = %d, want 1 for class-only resolver filtering; calls=%+v", constructors["int"], res.Calls)
	}
}

func TestParseKotlinVisibility(t *testing.T) {
	src := []byte(`class Service {
    private fun hidden() {}
    protected fun inherited() {}
    internal fun moduleOnly() {}
    public fun explicitPublic() {}
    fun defaultPublic() {}
}
`)
	res, err := Parse("kotlin", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	wants := map[string]bool{
		"hidden": false, "inherited": false, "moduleOnly": false,
		"explicitPublic": true, "defaultPublic": true,
	}
	for name, exported := range wants {
		def := defByName(t, res.Defs, "Service."+name)
		if def.Exported != exported {
			t.Fatalf("%s Exported = %v, want %v", name, def.Exported, exported)
		}
	}
}

func TestParseSwiftConditionalCompilationAndImports(t *testing.T) {
	src := []byte(`import Foundation
import struct Networking.Request

#if canImport(UIKit)
func platformHelper() {}
#else
func platformHelperFallback() {}
#endif

func run() {
    platformHelper()
}
`)
	res, err := Parse("swift", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !res.ParseOK {
		t.Fatalf("conditional directives should not poison the Swift parse: %s", res.ParseError)
	}
	defByName(t, res.Defs, "platformHelper")
	defByName(t, res.Defs, "platformHelperFallback")
	found := map[string]bool{}
	for _, imp := range res.Imports {
		found[imp.Spec] = true
	}
	for _, spec := range []string{"Foundation", "Networking.Request"} {
		if !found[spec] {
			t.Fatalf("missing Swift import %q in %+v", spec, res.Imports)
		}
	}
}

func TestParsePythonSyntaxErrorReported(t *testing.T) {
	src := []byte("def foo(:\n    pass\n")
	res, err := Parse("python", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if res.ParseOK {
		t.Fatalf("expected ParseOK=false for invalid syntax")
	}
	if res.ParseError == "" {
		t.Fatalf("expected non-empty ParseError")
	}
	if !strings.Contains(res.ParseError, "invalid or unsupported syntax") {
		t.Fatalf("ParseError should distinguish source errors from grammar gaps, got %q", res.ParseError)
	}
}

func TestParseUnsupportedLanguage(t *testing.T) {
	if _, err := Parse("cobol", []byte("")); err == nil {
		t.Fatalf("expected error for unsupported language")
	}
}

func TestSupported(t *testing.T) {
	if !Supported("python") {
		t.Fatalf("expected python to be supported")
	}
	if !Supported("csharp") {
		t.Fatalf("expected csharp to be supported")
	}
	if Supported("cobol") {
		t.Fatalf("expected cobol to be unsupported")
	}
}

const csharpControllerSrc = `using System;
using System.Collections.Generic;
using Models = MyApp.Domain.Models;

namespace MyApp.Controllers
{
    [ApiController]
    [Route("api/[controller]")]
    public class OwnersController : ControllerBase, IDisposable
    {
        private readonly IOwnerRepository _owners;

        public OwnersController(IOwnerRepository owners)
        {
            _owners = owners;
        }

        [HttpGet("{id}")]
        public Owner GetOwner(int id)
        {
            var x = _owners.FindById(id);
            return x;
        }
    }

    public interface IOwnerRepository
    {
        Owner FindById(int id);
    }
}
`

func TestParseCSharpNamespaceAndImports(t *testing.T) {
	res, err := Parse("csharp", []byte(csharpControllerSrc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !res.ParseOK {
		t.Fatalf("expected ParseOK, got error: %s", res.ParseError)
	}
	if res.Package != "MyApp.Controllers" {
		t.Fatalf("Package: got %q, want %q", res.Package, "MyApp.Controllers")
	}

	wantSpecs := map[string]string{
		"System":                     "",
		"System.Collections.Generic": "",
		"MyApp.Domain.Models":        "Models",
	}
	gotSpecs := map[string]string{}
	for _, imp := range res.Imports {
		gotSpecs[imp.Spec] = imp.Alias
	}
	for spec, alias := range wantSpecs {
		got, ok := gotSpecs[spec]
		if !ok {
			t.Fatalf("missing import %q in %+v", spec, res.Imports)
		}
		if got != alias {
			t.Fatalf("import %q alias: got %q, want %q", spec, got, alias)
		}
	}
}

func TestParseCSharpClassNestingAndAttributes(t *testing.T) {
	res, err := Parse("csharp", []byte(csharpControllerSrc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	controller := defByName(t, res.Defs, "OwnersController")
	if controller.Kind != "class" {
		t.Fatalf("OwnersController: got Kind %q", controller.Kind)
	}
	if len(controller.Bases) != 1 || controller.Bases[0] != "ControllerBase" {
		t.Fatalf("OwnersController.Bases: got %v", controller.Bases)
	}
	if len(controller.Interfaces) != 1 || controller.Interfaces[0] != "IDisposable" {
		t.Fatalf("OwnersController.Interfaces: got %v", controller.Interfaces)
	}

	var hasAPIController, hasRoute bool
	for _, ann := range controller.Annotations {
		switch ann.Name {
		case "ApiController":
			hasAPIController = true
		case "Route":
			hasRoute = true
			if ann.Value != "api/[controller]" {
				t.Fatalf("Route annotation value: got %q", ann.Value)
			}
		}
	}
	if !hasAPIController || !hasRoute {
		t.Fatalf("OwnersController.Annotations: got %+v", controller.Annotations)
	}

	ctor := defByName(t, res.Defs, "OwnersController.OwnersController")
	if ctor.Kind != "method" || ctor.ParentName != "OwnersController" || ctor.ParentKind != "class" {
		t.Fatalf("constructor: got %+v", ctor)
	}

	getOwner := defByName(t, res.Defs, "OwnersController.GetOwner")
	if getOwner.Kind != "method" || getOwner.ParentName != "OwnersController" {
		t.Fatalf("GetOwner: got %+v", getOwner)
	}
	var hasGetMapping bool
	for _, ann := range getOwner.Annotations {
		if ann.Name == "HttpGet" {
			hasGetMapping = true
			if ann.Value != "{id}" {
				t.Fatalf("HttpGet annotation value: got %q", ann.Value)
			}
		}
	}
	if !hasGetMapping {
		t.Fatalf("GetOwner.Annotations: got %+v", getOwner.Annotations)
	}

	iface := defByName(t, res.Defs, "IOwnerRepository")
	if iface.Kind != "class" {
		t.Fatalf("IOwnerRepository: got Kind %q", iface.Kind)
	}
	findByID := defByName(t, res.Defs, "IOwnerRepository.FindById")
	if findByID.Kind != "method" || findByID.ParentName != "IOwnerRepository" {
		t.Fatalf("FindById: got %+v", findByID)
	}
}

func TestParseCSharpCallsAndVars(t *testing.T) {
	res, err := Parse("csharp", []byte(csharpControllerSrc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var found bool
	for _, call := range res.Calls {
		if call.Receiver == "_owners" && call.Callee == "FindById" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected _owners.FindById call, got %+v", res.Calls)
	}

	wantTypes := map[string]string{
		"_owners": "IOwnerRepository",
		"owners":  "IOwnerRepository",
		"id":      "int",
	}
	gotTypes := map[string]string{}
	for _, v := range res.Vars {
		gotTypes[v.Name] = v.Type
	}
	for name, typ := range wantTypes {
		got, ok := gotTypes[name]
		if !ok {
			t.Fatalf("missing var %q in %+v", name, res.Vars)
		}
		if got != typ {
			t.Fatalf("var %q type: got %q, want %q", name, got, typ)
		}
	}
}

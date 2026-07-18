package mamari

import "testing"

func TestPythonTypedAndConstructedReceiversResolveImportedClass(t *testing.T) {
	root := t.TempDir()
	write(t, root, "models/a.py", `class Repo:
    def find(self):
        return "a"
`)
	write(t, root, "models/b.py", `class Repo:
    def find(self):
        return "b"
`)
	write(t, root, "service.py", `from models.a import Repo as PrimaryRepo
import models.a as model_a
from typing import Optional

def by_parameter(repo: PrimaryRepo):
    return repo.find()

def by_optional(repo: Optional[PrimaryRepo]):
    return repo.find()

def by_local():
    repo: PrimaryRepo = PrimaryRepo()
    return repo.find()

def by_constructor():
    repo = PrimaryRepo()
    return repo.find()

def by_module_type(repo: model_a.Repo):
    return repo.find()

def by_alias(repo: PrimaryRepo):
    alias = repo
    return alias.find()

class Service:

    def __init__(self, repo: PrimaryRepo):
        self.repo = repo

    def run(self):
        return self.repo.find()

class FieldService:
    repo: PrimaryRepo

    def run(self):
        return self.repo.find()

module_repo: PrimaryRepo = PrimaryRepo()
module_repo.find()
`)

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	var target string
	for _, sym := range idx.Symbols {
		if sym.File == "models/a.py" && sym.Name == "find" {
			target = sym.ID
		}
	}
	if target == "" {
		t.Fatal("models/a.py find method was not indexed")
	}
	got := 0
	for _, edge := range idx.SymbolEdges {
		if edge.Type != "calls" || edge.Evidence.File != "service.py" || edge.Evidence.Raw != "repo.find" && edge.Evidence.Raw != "alias.find" && edge.Evidence.Raw != "self.repo.find" && edge.Evidence.Raw != "module_repo.find" {
			continue
		}
		got++
		if edge.To != target || edge.Confidence != ConfScoped {
			t.Fatalf("receiver call resolved incorrectly: %#v", edge)
		}
	}
	if got != 9 {
		t.Fatalf("resolved receiver calls = %d, want 9", got)
	}
}

func TestPythonUnknownPolymorphicReceiverRemainsAmbiguous(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.py", "class A:\n    def execute(self):\n        pass\n")
	write(t, root, "b.py", "class B:\n    def execute(self):\n        pass\n")
	write(t, root, "run.py", "def run(strategy):\n    strategy.execute()\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range idx.SymbolEdges {
		if edge.Type == "calls" && edge.Evidence.File == "run.py" && edge.Evidence.Raw == "strategy.execute" {
			if edge.Confidence != ConfUnresolved || edge.UnresolvedReason != ReasonAmbiguousName {
				t.Fatalf("dynamic receiver should remain ambiguous: %#v", edge)
			}
			return
		}
	}
	t.Fatal("strategy.execute call edge not found")
}

func TestPythonUnknownReceiverDoesNotResolveToSameFileFunction(t *testing.T) {
	root := t.TempDir()
	write(t, root, "app.py", `class App:
    def make_response(self, value):
        return value
`)
	write(t, root, "helpers.py", `def make_response(value):
    return current_app.make_response(value)
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	helperID := ""
	for _, sym := range idx.Symbols {
		if sym.File == "helpers.py" && sym.Name == "make_response" {
			helperID = sym.ID
		}
	}
	if helperID == "" {
		t.Fatal("helper make_response was not indexed")
	}
	for _, edge := range idx.SymbolEdges {
		if edge.From != helperID || edge.Type != "calls" || edge.Evidence.Raw != "current_app.make_response" {
			continue
		}
		if edge.To == helperID {
			t.Fatalf("unknown receiver created false self-call: %#v", edge)
		}
		if edge.To != "unresolved:current_app.make_response" || edge.Confidence != ConfUnresolved || edge.UnresolvedReason != ReasonDynamicReceiver {
			t.Fatalf("unknown receiver should remain explicitly unresolved: %#v", edge)
		}
		return
	}
	t.Fatal("current_app.make_response edge not found")
}

func TestPythonClassFactoryAttributeTypesConstructedInstance(t *testing.T) {
	root := t.TempDir()
	write(t, root, "maps/primary.py", `class URLMap:
    def bind(self):
        return "primary"
`)
	write(t, root, "maps/decoy.py", `class URLMap:
    def bind(self):
        return "decoy"
`)
	write(t, root, "app.py", `from maps.primary import URLMap as PrimaryMap

class App:
    url_map_class = PrimaryMap

    def __init__(self):
        self.url_map = self.url_map_class()

    def bind_route(self):
        return self.url_map.bind()
`)

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	var target string
	for _, sym := range idx.Symbols {
		if sym.File == "maps/primary.py" && sym.Name == "bind" {
			target = sym.ID
		}
	}
	if target == "" {
		t.Fatal("primary URLMap.bind was not indexed")
	}
	for _, edge := range idx.SymbolEdges {
		if edge.Type != "calls" || edge.Evidence.File != "app.py" || edge.Evidence.Raw != "self.url_map.bind" {
			continue
		}
		if edge.To != target || edge.Confidence != ConfScoped {
			t.Fatalf("class-factory instance call resolved incorrectly: %#v", edge)
		}
		return
	}
	t.Fatal("self.url_map.bind call edge not found")
}

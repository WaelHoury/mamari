package mamari

import "testing"

func TestGoExternalPackageCallsStayImportEdgesNotInternalCalls(t *testing.T) {
	root := t.TempDir()
	write(t, root, "go.mod", "module example.com/app\n\ngo 1.24\n")
	write(t, root, "internal/work/work.go", `package work

func Run() {}
`)
	write(t, root, "main.go", `package main

import (
	"flag"
	"fmt"
	"strings"
	"sync"
	"testing"

	"example.com/app/internal/work"
)

type First struct{ mu sync.Mutex }
type Second struct{}

func (First) Maybe() {}
func (Second) Maybe() {}
func (First) Println() {}
func (first *First) lock() { first.mu.Lock() }
func fail(t *testing.T) { t.Fatal("failed") }
func flags() {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	_ = fs.Bool("enabled", false, "")
}

func run(client any) {
	fmt.Println("start")
	_ = strings.Contains("value", "v")
	work.Run()
	client.Maybe()
}
`)

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	var imports, externalCalls, internalCalls, uncertainInternal int
	for _, edge := range idx.SymbolEdges {
		switch {
		case edge.Type == "imports":
			imports++
		case edge.Type == "calls" && (edge.Evidence.Raw == "fmt.Println" ||
			edge.Evidence.Raw == "strings.Contains" ||
			edge.Evidence.Raw == "first.mu.Lock" ||
			edge.Evidence.Raw == "t.Fatal" ||
			edge.Evidence.Raw == "fs.Bool"):
			externalCalls++
		case edge.Type == "calls" && edge.Evidence.Raw == "work.Run":
			internalCalls++
			if edge.Confidence == ConfUnresolved {
				t.Fatalf("internal package call stayed unresolved: %#v", edge)
			}
		case edge.Type == "calls" && edge.Evidence.Raw == "client.Maybe":
			uncertainInternal++
			if edge.Confidence != ConfUnresolved || edge.UnresolvedReason != ReasonAmbiguousName {
				t.Fatalf("ambiguous object receiver lost uncertainty: %#v", edge)
			}
		}
	}
	if imports != 6 {
		t.Fatalf("expected all six dependencies as import edges, got %d", imports)
	}
	if externalCalls != 0 {
		t.Fatalf("external package calls should not masquerade as internal call edges, got %d", externalCalls)
	}
	if internalCalls != 1 {
		t.Fatalf("expected internal package call to remain in graph, got %d", internalCalls)
	}
	if uncertainInternal != 1 {
		t.Fatalf("expected ambiguous object call to remain conservative, got %d", uncertainInternal)
	}
}

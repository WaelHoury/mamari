package mamari

import "testing"

func TestJSTypedReceiverBindingsResolveToImportedClass(t *testing.T) {
	root := t.TempDir()
	write(t, root, "UserRepo.ts", `export class UserRepo {
  find(id: number) { return id }
}
export function createRepo() { return new UserRepo() }
`)
	write(t, root, "OtherRepo.ts", `export class OtherRepo {
  find(id: number) { return id * 2 }
}
`)
	write(t, root, "service.ts", `import { UserRepo } from './UserRepo'
import * as repoNS from './UserRepo'

export function loadParam(repo: UserRepo) {
  return repo.find(1)
}

export function loadLocal(input: unknown) {
  const repo: UserRepo = input as UserRepo
  const alias = repo
  return alias.find(2)
}

export const loadArrow = (repo: UserRepo) => repo.find(3)

export function loadNamespace() {
  return repoNS.createRepo()
}

export class Service {
  private repo: UserRepo

  constructor(repo: UserRepo) {
    this.repo = repo
  }

  loadField() {
    return this.repo.find(4)
  }
}

export class ParameterPropertyService {
  constructor(private repo: UserRepo) {}
  load() { return this.repo.find(5) }
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	for _, caller := range []string{"loadParam", "loadLocal", "loadArrow", "loadField", "load"} {
		assertJSTypedCallTarget(t, idx, caller, "UserRepo.ts", "find")
	}
	assertJSTypedCallTarget(t, idx, "loadNamespace", "UserRepo.ts", "createRepo")
}

func TestJSTypedInterfaceReceiverStaysAmbiguous(t *testing.T) {
	root := t.TempDir()
	write(t, root, "types.ts", `export interface Strategy { execute(): number }
`)
	write(t, root, "a.ts", `export class A { execute() { return 1 } }
`)
	write(t, root, "b.ts", `export class B { execute() { return 2 } }
`)
	write(t, root, "runner.ts", `import type { Strategy } from './types'
export function run(strategy: Strategy) {
  return strategy.execute()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	run := findSymbolByName(idx, "run")
	if run.ID == "" {
		t.Fatal("expected run symbol")
	}
	edges := outgoingCallEdges(t, idx, run)
	if len(edges) != 1 {
		t.Fatalf("expected one strategy.execute edge, got %#v", edges)
	}
	if edges[0].Confidence != ConfUnresolved || edges[0].UnresolvedReason != ReasonAmbiguousName {
		t.Fatalf("typed interface with two implementations must remain unresolved/ambiguous, got %#v", edges[0])
	}
}

func assertJSTypedCallTarget(t *testing.T, idx *Index, callerName, targetFile, targetName string) {
	t.Helper()
	idx.mu.Lock()
	var callers []CGPSymbol
	for _, sym := range idx.Symbols {
		if sym.Name == callerName && sym.File == "service.ts" {
			callers = append(callers, sym)
		}
	}
	var matching []CGPEdge
	for _, caller := range callers {
		for _, edge := range idx.SymbolEdges {
			if edge.From != caller.ID || edge.Type != "calls" {
				continue
			}
			target := idx.Symbols[edge.To]
			if target.File == targetFile && target.Name == targetName && (edge.Confidence == ConfScoped || edge.Confidence == ConfExact) {
				matching = append(matching, edge)
			}
		}
	}
	idx.mu.Unlock()
	if len(matching) == 0 {
		t.Fatalf("expected %s to resolve to %s#%s with exact/scoped confidence; callers=%#v edges=%#v", callerName, targetFile, targetName, callers, outgoingEdgesForSymbols(idx, callers))
	}
}

func outgoingEdgesForSymbols(idx *Index, symbols []CGPSymbol) []CGPEdge {
	wanted := map[string]bool{}
	for _, sym := range symbols {
		wanted[sym.ID] = true
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	var out []CGPEdge
	for _, edge := range idx.SymbolEdges {
		if wanted[edge.From] && edge.Type == "calls" {
			out = append(out, edge)
		}
	}
	return out
}

package mamari

import (
	"strings"
	"testing"
)

// A value assigned from a return-type-annotated factory resolves its method
// calls to the annotated type's file.
func TestReturnTypeAnnotationResolvesMethodCall(t *testing.T) {
	root := t.TempDir()
	write(t, root, "userRepo.ts", `export class UserRepo {
  findById(id: string) { return id }
}
`)
	write(t, root, "factory.ts", `import { UserRepo } from './userRepo'
export function getRepo(): UserRepo {
  return new UserRepo()
}
`)
	write(t, root, "svc.ts", `import { getRepo } from './factory'
export function work() {
  const r = getRepo()
  return r.findById('x')
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	snap := idx.snapshot()
	for _, e := range snap.SymbolEdges {
		if e.Type == "calls" && strings.HasSuffix(e.To, "userRepo.ts:UserRepo.findById") && e.Confidence != ConfUnresolved {
			return
		}
	}
	t.Fatalf("r.findById() must resolve via getRepo(): UserRepo return type; edges=%#v", snap.SymbolEdges)
}

// Promise<T> return type is unwrapped so an awaited result resolves.
func TestReturnTypePromiseUnwrapResolves(t *testing.T) {
	root := t.TempDir()
	write(t, root, "svc.ts", `export class Service {
  run() { return 1 }
}
`)
	write(t, root, "factory.ts", `import { Service } from './svc'
export async function makeService(): Promise<Service> {
  return new Service()
}
`)
	write(t, root, "main.ts", `import { makeService } from './factory'
export async function go() {
  const s = await makeService()
  return s.run()
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	snap := idx.snapshot()
	for _, e := range snap.SymbolEdges {
		if e.Type == "calls" && strings.HasSuffix(e.To, "svc.ts:Service.run") && e.Confidence != ConfUnresolved {
			return
		}
	}
	t.Fatalf("s.run() must resolve via Promise<Service> return type; edges=%#v", snap.SymbolEdges)
}

// A union return type must NOT be guessed (stays unresolved, not a false edge).
func TestReturnTypeUnionNotGuessed(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.ts", `export class Cat { meow() { return 1 } }`)
	write(t, root, "b.ts", `export class Dog { meow() { return 2 } }`)
	// Body returns a parameter (no concrete type evidence), so only the union
	// annotation is available — which must not be guessed to either class.
	write(t, root, "f.ts", `import { Cat } from './a'
import { Dog } from './b'
export function pick(input): Cat | Dog { return input }
export function use(input) { const x = pick(input); return x.meow() }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	snap := idx.snapshot()
	for _, e := range snap.SymbolEdges {
		if e.Type == "calls" && e.Evidence.Raw == "x.meow" && e.Confidence != ConfUnresolved {
			t.Fatalf("union-typed x.meow() must not resolve to a guessed class: %#v", e)
		}
	}
}

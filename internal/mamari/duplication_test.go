package mamari

import "testing"

// Two functions with identical structure but renamed identifiers/literals are
// a Type-2 clone and must cluster together.
func TestDuplicationDetectsRenamedClone(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.js", `function computeInvoiceTotal(items) {
  let total = 0
  for (const item of items) {
    if (item.active) {
      total = total + item.price * item.quantity
    }
  }
  return total
}
module.exports = { computeInvoiceTotal }
`)
	write(t, root, "b.js", `function sumOrderLines(lines) {
  let acc = 0
  for (const line of lines) {
    if (line.enabled) {
      acc = acc + line.cost * line.count
    }
  }
  return acc
}
module.exports = { sumOrderLines }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := Duplication(idx, DuplicationOptions{})
	if resp.TotalClusters == 0 {
		t.Fatalf("expected a clone cluster for the two structurally-identical functions")
	}
	found := false
	for _, c := range resp.Clusters {
		names := map[string]bool{}
		for _, m := range c.Members {
			names[m.Name] = true
		}
		if names["computeInvoiceTotal"] && names["sumOrderLines"] {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected computeInvoiceTotal and sumOrderLines to cluster; got %#v", resp.Clusters)
	}
}

// Structurally different functions must NOT cluster.
func TestDuplicationIgnoresDifferentStructure(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.js", `function alpha(items) {
  let total = 0
  for (const item of items) {
    total += item.price
  }
  return total
}
module.exports = { alpha }
`)
	write(t, root, "b.js", `function beta(config) {
  if (!config) throw new Error('no config')
  const client = config.client
  return client.connect().then(() => client.ready)
}
module.exports = { beta }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := Duplication(idx, DuplicationOptions{})
	for _, c := range resp.Clusters {
		names := map[string]bool{}
		for _, m := range c.Members {
			names[m.Name] = true
		}
		if names["alpha"] && names["beta"] {
			t.Fatalf("structurally different alpha/beta must not cluster: %#v", c)
		}
	}
}

// Trivial one-liners must not register as clones (min-size gate).
func TestDuplicationIgnoresTrivialBodies(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.js", `function getA() { return 1 }
function getB() { return 2 }
module.exports = { getA, getB }
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := Duplication(idx, DuplicationOptions{})
	if resp.TotalClusters != 0 {
		t.Fatalf("trivial getters must not cluster; got %#v", resp.Clusters)
	}
}

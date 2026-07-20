package mamari

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSemanticQueryBridgesVocabularyMismatch(t *testing.T) {
	root := t.TempDir()
	write(t, root, "events.js", `function publishNotification(payload) {
  eventBus.emit('account.changed', payload)
}

function calculateTax(invoice) {
  return invoice.total * 0.2
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := SemanticQuery(idx, "send an event message", SemanticQueryOptions{Limit: 5, SourceOnly: true})
	if resp.Status != "ok" || len(resp.Hits) == 0 {
		t.Fatalf("expected semantic results, got %#v", resp)
	}
	if resp.Hits[0].Symbol.Name != "publishNotification" {
		t.Fatalf("expected vocabulary bridge send/event/message -> publishNotification first, got %#v", resp.Hits)
	}
	if len(resp.Hits[0].Terms) == 0 {
		t.Fatalf("expected matched semantic concepts to explain the result, got %#v", resp.Hits[0])
	}
}

func TestSemanticQueryUsesGraphDiffusion(t *testing.T) {
	root := t.TempDir()
	write(t, root, "checkout.js", `function authorizePayment(user) {
  return permissionPolicy.check(user)
}

function processCheckout(user) {
  return authorizePayment(user)
}

function renderReceipt() {
  return '<p>done</p>'
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	resp := SemanticQuery(idx, "verify access permission", SemanticQueryOptions{Limit: 5, SourceOnly: true})
	if resp.Status != "ok" {
		t.Fatalf("expected semantic results, got %#v", resp)
	}
	positions := map[string]int{}
	for i, hit := range resp.Hits {
		positions[hit.Symbol.Name] = i + 1
	}
	if positions["authorizePayment"] == 0 {
		t.Fatalf("expected authorizePayment from concept similarity, got %#v", resp.Hits)
	}
	if positions["processCheckout"] == 0 {
		t.Fatalf("expected processCheckout to inherit semantic context through the call graph, got %#v", resp.Hits)
	}
	if positions["renderReceipt"] != 0 && positions["renderReceipt"] < positions["processCheckout"] {
		t.Fatalf("expected graph-related processCheckout above unrelated renderReceipt, got %#v", resp.Hits)
	}
}

func TestSemanticIndexPersistsAndReloads(t *testing.T) {
	root := t.TempDir()
	write(t, root, "service.js", `function storeDocument(doc) {
  return repository.save(doc)
}
`)
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(root, ".mamari", "index.json")
	if err := SaveIndex(idx, indexPath); err != nil {
		t.Fatal(err)
	}
	first := SemanticQuery(idx, "persist file", SemanticQueryOptions{Limit: 3})
	if first.Status != "ok" || len(first.Hits) == 0 {
		t.Fatalf("expected initial semantic result, got %#v", first)
	}
	sidecar := filepath.Join(root, ".mamari", "semantic.gob")
	if info, err := os.Stat(sidecar); err != nil || info.Size() == 0 {
		t.Fatalf("expected non-empty semantic sidecar, info=%#v err=%v", info, err)
	}

	loaded, err := LoadIndex(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	second := SemanticQuery(loaded, "persist file", SemanticQueryOptions{Limit: 3})
	if second.Status != "ok" || len(second.Hits) == 0 {
		t.Fatalf("expected semantic result after reload, got %#v", second)
	}
	if first.Hits[0].Symbol.ID != second.Hits[0].Symbol.ID || first.Hits[0].Score != second.Hits[0].Score {
		t.Fatalf("expected persisted semantic ranking to be stable, first=%#v second=%#v", first.Hits, second.Hits)
	}
}

func TestSemanticIndexInvalidatesAfterWatchRebake(t *testing.T) {
	root := t.TempDir()
	write(t, root, "service.js", "function publishAlert() { return true }\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if resp := SemanticQuery(idx, "send warning", SemanticQueryOptions{}); resp.Status != "ok" {
		t.Fatalf("expected initial semantic index, got %#v", resp)
	}
	idx.mu.Lock()
	built := idx.semanticIndex != nil
	idx.mu.Unlock()
	if !built {
		t.Fatal("expected semantic index to be built")
	}
	write(t, root, "service.js", "function storeDocument() { return true }\n")
	if err := rebakeFile(idx, root, "service.js"); err != nil {
		t.Fatal(err)
	}
	idx.mu.Lock()
	invalidated := idx.semanticIndex == nil
	idx.mu.Unlock()
	if !invalidated {
		t.Fatal("expected watch rebake to invalidate semantic vectors")
	}
	resp := SemanticQuery(idx, "persist file", SemanticQueryOptions{Limit: 3})
	if resp.Status != "ok" || len(resp.Hits) == 0 || resp.Hits[0].Symbol.Name != "storeDocument" {
		t.Fatalf("expected rebuilt vectors to reflect edited symbol, got %#v", resp)
	}
}

// TestWatchKeepsSemanticIndexLazyWithoutAQuery guards the idle-resource
// contract: editor and generator writes are allowed to refresh the live graph,
// but must not trigger a corpus-wide semantic rebuild until a request needs it.
func TestWatchKeepsSemanticIndexLazyWithoutAQuery(t *testing.T) {
	root := t.TempDir()
	write(t, root, "service.js", "function publishAlert() { return true }\n")
	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	ready := make(chan struct{})
	rebakes := make(chan struct{}, 4)
	go func() {
		defer wg.Done()
		_ = Watch(ctx, idx, WatchOptions{
			Debounce: 50 * time.Millisecond,
			OnReady:  func() { close(ready) },
			OnRebake: func(updated, removed []string) {
				select {
				case rebakes <- struct{}{}:
				default:
				}
			},
		})
	}()
	defer wg.Wait()
	defer cancel()

	<-ready
	write(t, root, "service.js", "function storeDocument() { return true }\n")
	select {
	case <-rebakes:
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not rebake within 3s")
	}

	// OnRebake runs just before flush finishes. Give any accidentally spawned
	// background work time to publish so this catches a prewarm regression.
	time.Sleep(100 * time.Millisecond)
	idx.mu.Lock()
	sem := idx.semanticIndex
	idx.mu.Unlock()
	if sem != nil {
		t.Fatal("watch rebake built the semantic index without a query")
	}

	resp := SemanticQuery(idx, "store document", SemanticQueryOptions{Limit: 3})
	if resp.Status != "ok" || len(resp.Hits) == 0 || resp.Hits[0].Symbol.Name != "storeDocument" {
		t.Fatalf("lazy semantic build did not reflect the edited source: %#v", resp)
	}
}

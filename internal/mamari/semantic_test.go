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

// TestWatchPrewarmsSemanticIndexWithoutAQuery is the regression guard for the
// fix in watch.go's triggerSemanticPrewarm: before that fix, every edit during
// a `--watch` session left the *next* semantic_query/inspect_flow call to pay
// the full corpus rebuild synchronously. This test starts the
// real fsnotify-backed Watch() (not rebakeFile, which bypasses the watcher's
// own flush() entirely), edits a file, waits for the rebake to fire, and
// asserts idx.semanticIndex becomes populated WITHOUT ever calling
// SemanticQuery/InspectFlow — proving the rebuild happens proactively on the
// watcher's own goroutine, not lazily on the next query's critical path.
func TestWatchPrewarmsSemanticIndexWithoutAQuery(t *testing.T) {
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
	rebakes := make(chan struct{}, 4)
	go func() {
		defer wg.Done()
		_ = Watch(ctx, idx, WatchOptions{
			Debounce: 50 * time.Millisecond,
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

	time.Sleep(150 * time.Millisecond) // let the watcher register dirs
	write(t, root, "service.js", "function storeDocument() { return true }\n")
	select {
	case <-rebakes:
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not rebake within 3s")
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		idx.mu.Lock()
		sem := idx.semanticIndex
		idx.mu.Unlock()
		if sem != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expected the watcher to prewarm the semantic index without a query, but it stayed nil")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The prewarmed index must actually reflect the edit (not a stale build
	// of the pre-edit content the watcher raced past).
	idx.mu.Lock()
	sem := idx.semanticIndex
	idx.mu.Unlock()
	hashesOK := semanticHashesMatch(idx, sem.FileHashes)
	if !hashesOK {
		t.Fatal("prewarmed semantic index does not match the post-edit file hashes")
	}
}

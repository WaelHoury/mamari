package mamari

import (
	"sort"
	"time"
)

const journalCapacity = 200

// recordRebake appends a journal entry for a watch-mode rebake. Empty
// rebakes (no updates, no removes) are dropped so seq numbers align with
// observable changes.
func (idx *Index) recordRebake(updated, removed []string) {
	if len(updated) == 0 && len(removed) == 0 {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.journalSeq++
	entry := JournalEntry{
		Seq:       idx.journalSeq,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Updated:   append([]string(nil), updated...),
		Removed:   append([]string(nil), removed...),
	}
	idx.journal = append(idx.journal, entry)
	if len(idx.journal) > journalCapacity {
		// Drop the oldest. We keep the trim cheap by reslicing rather than
		// reallocating; the underlying array grows over time but never past
		// the cap because Go's slice growth + this trim balance out.
		over := len(idx.journal) - journalCapacity
		idx.journal = append([]JournalEntry(nil), idx.journal[over:]...)
	}
}

// ChangedSince returns everything in the journal after sinceSeq. If
// sinceSeq predates the oldest retained entry, MissedHistory is set so the
// caller knows the answer is incomplete.
func ChangedSince(idx *Index, sinceSeq uint64) ChangedSinceResponse {
	idx.mu.Lock()
	latest := idx.journalSeq
	journal := append([]JournalEntry(nil), idx.journal...)
	idx.mu.Unlock()

	resp := ChangedSinceResponse{
		Status:    "ok",
		SinceSeq:  sinceSeq,
		LatestSeq: latest,
		Updated:   []string{},
		Removed:   []string{},
		Entries:   []JournalEntry{},
	}
	if latest == 0 {
		// Either no rebakes have occurred or the index was just loaded. The
		// status is still "ok"; the caller can poll later.
		return resp
	}

	earliest := uint64(0)
	if len(journal) > 0 {
		earliest = journal[0].Seq
	}
	if sinceSeq < earliest && sinceSeq != 0 {
		// Caller asked about a window we no longer remember. Mark it so
		// they can rebaseline rather than silently undercounting.
		resp.MissedHistory = true
	}

	updatedSet := map[string]bool{}
	removedSet := map[string]bool{}
	for _, e := range journal {
		if e.Seq <= sinceSeq {
			continue
		}
		resp.Entries = append(resp.Entries, e)
		for _, u := range e.Updated {
			updatedSet[u] = true
			delete(removedSet, u)
		}
		for _, r := range e.Removed {
			removedSet[r] = true
			delete(updatedSet, r)
		}
	}
	resp.Updated = sortedKeys(updatedSet)
	resp.Removed = sortedKeys(removedSet)
	resp.AffectedSymbols = symbolsInFiles(idx, resp.Updated)
	return resp
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// symbolsInFiles returns the current symbols whose File matches any of the
// given relative paths. Used by ChangedSince to surface what an agent might
// want to re-examine without re-querying every changed file individually.
func symbolsInFiles(idx *Index, files []string) []CGPSymbolSummary {
	if len(files) == 0 {
		return nil
	}
	want := make(map[string]bool, len(files))
	for _, f := range files {
		want[f] = true
	}
	idx.mu.Lock()
	syms := make([]CGPSymbol, 0, len(idx.Symbols))
	for _, s := range idx.Symbols {
		if want[s.File] && s.Kind != "file" {
			syms = append(syms, s)
		}
	}
	idx.mu.Unlock()
	return summarizeSymbols(syms)
}

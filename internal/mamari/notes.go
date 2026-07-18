package mamari

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// maxNoteTextLength bounds the size of a single note so the notes file
// cannot grow unbounded from a single bad write.
const maxNoteTextLength = 4000

// maxNotesPerFile bounds the total number of notes kept per repo, evicting
// the oldest notes (by ID) once exceeded so .mamari/notes.json stays bounded
// even across a very long-running session.
const maxNotesPerFile = 5000

// notesMu serializes read-modify-write access to .mamari/notes.json across
// goroutines within this process. The file itself is written atomically
// (temp file + rename) for durability against crashes mid-write.
var notesMu sync.Mutex

func notesPath(root string) string {
	return filepath.Join(root, ".mamari", "notes.json")
}

// LoadNotes reads .mamari/notes.json under root. A missing file is not an
// error: it returns an empty NotesFile ready for use.
func LoadNotes(root string) (*NotesFile, error) {
	data, err := os.ReadFile(notesPath(root))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &NotesFile{NextID: 1}, nil
		}
		return nil, err
	}
	var nf NotesFile
	if err := json.Unmarshal(data, &nf); err != nil {
		return nil, fmt.Errorf("parse notes file: %w", err)
	}
	if nf.NextID <= 0 {
		nf.NextID = 1
	}
	return &nf, nil
}

// SaveNotes writes nf to .mamari/notes.json under root atomically (write to a
// temp file in the same directory, then rename), so a crash mid-write cannot
// leave a truncated/corrupt notes file.
func SaveNotes(root string, nf *NotesFile) error {
	dir := filepath.Join(root, ".mamari")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(nf, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "notes-*.json.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, notesPath(root))
}

// AddNote attaches a freeform note to a symbol, persisted in
// .mamari/notes.json. symbolID must refer to a symbol currently present in
// idx; this catches typos/stale IDs early rather than silently accumulating
// notes for symbols that no longer exist.
func AddNote(idx *Index, root, symbolID, text string) (AddNoteResponse, error) {
	symbolID = strings.TrimSpace(symbolID)
	text = strings.TrimSpace(text)
	if symbolID == "" {
		return AddNoteResponse{Status: "error"}, errors.New("symbolId is required")
	}
	if text == "" {
		return AddNoteResponse{Status: "error"}, errors.New("text is required")
	}
	if len(text) > maxNoteTextLength {
		return AddNoteResponse{Status: "error"}, fmt.Errorf("text exceeds max length of %d characters", maxNoteTextLength)
	}

	idx.mu.Lock()
	_, ok := idx.Symbols[symbolID]
	idx.mu.Unlock()
	if !ok {
		return AddNoteResponse{Status: "not_found"}, fmt.Errorf("symbol %q not found in index", symbolID)
	}

	notesMu.Lock()
	defer notesMu.Unlock()

	nf, err := LoadNotes(root)
	if err != nil {
		return AddNoteResponse{Status: "error"}, err
	}
	note := SymbolNote{
		ID:        nf.NextID,
		SymbolID:  symbolID,
		Text:      text,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	nf.NextID++
	nf.Notes = append(nf.Notes, note)
	if len(nf.Notes) > maxNotesPerFile {
		nf.Notes = nf.Notes[len(nf.Notes)-maxNotesPerFile:]
	}
	if err := SaveNotes(root, nf); err != nil {
		return AddNoteResponse{Status: "error"}, err
	}
	return AddNoteResponse{Status: "ok", Note: note}, nil
}

// ListNotes returns notes, most-recently-added first, optionally filtered to
// a single symbol ID.
func ListNotes(root, symbolID string) (ListNotesResponse, error) {
	notesMu.Lock()
	nf, err := LoadNotes(root)
	notesMu.Unlock()
	if err != nil {
		return ListNotesResponse{Status: "error"}, err
	}

	symbolID = strings.TrimSpace(symbolID)
	out := make([]SymbolNote, 0, len(nf.Notes))
	for i := len(nf.Notes) - 1; i >= 0; i-- {
		n := nf.Notes[i]
		if symbolID != "" && n.SymbolID != symbolID {
			continue
		}
		out = append(out, n)
	}
	return ListNotesResponse{Status: "ok", Total: len(out), Notes: out}, nil
}

// RemoveNote deletes the note with the given ID, if present.
func RemoveNote(root string, id int) (RemoveNoteResponse, error) {
	notesMu.Lock()
	defer notesMu.Unlock()

	nf, err := LoadNotes(root)
	if err != nil {
		return RemoveNoteResponse{Status: "error"}, err
	}
	idx := -1
	for i, n := range nf.Notes {
		if n.ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		return RemoveNoteResponse{Status: "ok", Removed: false}, nil
	}
	nf.Notes = append(nf.Notes[:idx], nf.Notes[idx+1:]...)
	if err := SaveNotes(root, nf); err != nil {
		return RemoveNoteResponse{Status: "error"}, err
	}
	return RemoveNoteResponse{Status: "ok", Removed: true}, nil
}

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

// maxADRSectionContentLength bounds a single ADR section's content. Larger
// than maxNoteTextLength (notes.go) because ADR content is prose-document
// text, not a short annotation.
const maxADRSectionContentLength = 20000

// maxADRSections bounds the total number of sections kept in one project's
// ADR document, so .mamari/adr.json stays bounded.
const maxADRSections = 200

// adrMu serializes read-modify-write access to .mamari/adr.json across
// goroutines within this process, mirroring notesMu in notes.go. The file
// itself is written atomically (temp file + rename).
var adrMu sync.Mutex

func adrPath(root string) string {
	return filepath.Join(root, ".mamari", "adr.json")
}

// LoadADR reads .mamari/adr.json under root. A missing file is not an
// error: it returns an empty ADRDocument ready for use.
func LoadADR(root string) (*ADRDocument, error) {
	data, err := os.ReadFile(adrPath(root))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &ADRDocument{SchemaVersion: 1}, nil
		}
		return nil, err
	}
	var doc ADRDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse ADR file: %w", err)
	}
	if doc.SchemaVersion <= 0 {
		doc.SchemaVersion = 1
	}
	return &doc, nil
}

// SaveADR writes doc to .mamari/adr.json under root atomically (write to a
// temp file in the same directory, then rename), mirroring SaveNotes.
func SaveADR(root string, doc *ADRDocument) error {
	dir := filepath.Join(root, ".mamari")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(adrPath(root), data, 0o644)
}

func adrSectionIndex(doc *ADRDocument, title string) int {
	for i, s := range doc.Sections {
		if strings.EqualFold(s.Title, title) {
			return i
		}
	}
	return -1
}

// UpdateADRSection creates or overwrites (upsert by case-insensitive title)
// a section of the project's ADR document.
func UpdateADRSection(root, title, content string) (ADRSectionResponse, error) {
	title = strings.TrimSpace(title)
	content = strings.TrimSpace(content)
	if title == "" {
		return ADRSectionResponse{Status: "error"}, errors.New("title is required")
	}
	if content == "" {
		return ADRSectionResponse{Status: "error"}, errors.New("content is required")
	}
	if len(content) > maxADRSectionContentLength {
		return ADRSectionResponse{Status: "error"}, fmt.Errorf("content exceeds max length of %d characters", maxADRSectionContentLength)
	}

	adrMu.Lock()
	defer adrMu.Unlock()

	doc, err := LoadADR(root)
	if err != nil {
		return ADRSectionResponse{Status: "error"}, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if i := adrSectionIndex(doc, title); i >= 0 {
		doc.Sections[i].Content = content
		doc.Sections[i].UpdatedAt = now
	} else {
		if len(doc.Sections) >= maxADRSections {
			return ADRSectionResponse{Status: "error"}, fmt.Errorf("ADR document already has the maximum of %d sections", maxADRSections)
		}
		doc.Sections = append(doc.Sections, ADRSection{
			Title:     title,
			Content:   content,
			CreatedAt: now,
			UpdatedAt: now,
		})
	}
	if err := SaveADR(root, doc); err != nil {
		return ADRSectionResponse{Status: "error"}, err
	}
	section := doc.Sections[adrSectionIndex(doc, title)]
	return ADRSectionResponse{Status: "ok", Section: section}, nil
}

// ListADRSections returns every section's title and timestamps, with content
// omitted, so discovering what ADR sections a project has is cheap. Sections
// are returned in document order (insertion order; updates do not reorder).
func ListADRSections(root string) (ADRListResponse, error) {
	adrMu.Lock()
	doc, err := LoadADR(root)
	adrMu.Unlock()
	if err != nil {
		return ADRListResponse{Status: "error"}, err
	}
	out := make([]ADRSection, 0, len(doc.Sections))
	for _, s := range doc.Sections {
		out = append(out, ADRSection{Title: s.Title, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt})
	}
	return ADRListResponse{Status: "ok", Total: len(out), Sections: out}, nil
}

// GetADRSection returns one section (with content) by case-insensitive
// title, or — when title is empty — the whole document.
func GetADRSection(root, title string) (ADRGetResponse, error) {
	adrMu.Lock()
	doc, err := LoadADR(root)
	adrMu.Unlock()
	if err != nil {
		return ADRGetResponse{Status: "error"}, err
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return ADRGetResponse{Status: "ok", Sections: doc.Sections}, nil
	}
	if i := adrSectionIndex(doc, title); i >= 0 {
		return ADRGetResponse{Status: "ok", Sections: []ADRSection{doc.Sections[i]}}, nil
	}
	return ADRGetResponse{Status: "not_found"}, nil
}

// RemoveADRSection deletes the section with the given case-insensitive
// title, if present.
func RemoveADRSection(root, title string) (ADRRemoveResponse, error) {
	adrMu.Lock()
	defer adrMu.Unlock()

	doc, err := LoadADR(root)
	if err != nil {
		return ADRRemoveResponse{Status: "error"}, err
	}
	i := adrSectionIndex(doc, title)
	if i == -1 {
		return ADRRemoveResponse{Status: "ok", Removed: false}, nil
	}
	doc.Sections = append(doc.Sections[:i], doc.Sections[i+1:]...)
	if err := SaveADR(root, doc); err != nil {
		return ADRRemoveResponse{Status: "error"}, err
	}
	return ADRRemoveResponse{Status: "ok", Removed: true}, nil
}

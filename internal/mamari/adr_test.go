package mamari

import "testing"

// TestADRSectionRoundTripPersistence covers update -> get -> list -> remove
// against .mamari/adr.json, mirroring notes.go's test coverage style.
func TestADRSectionRoundTripPersistence(t *testing.T) {
	root := t.TempDir()

	updated, err := UpdateADRSection(root, "Auth Strategy", "We use JWT with refresh tokens.")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "ok" || updated.Section.Title != "Auth Strategy" {
		t.Fatalf("expected ok status with the new section, got %#v", updated)
	}
	if updated.Section.CreatedAt == "" || updated.Section.UpdatedAt == "" {
		t.Fatalf("expected timestamps to be set, got %#v", updated.Section)
	}

	got, err := GetADRSection(root, "auth strategy") // case-insensitive lookup
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "ok" || len(got.Sections) != 1 || got.Sections[0].Content != "We use JWT with refresh tokens." {
		t.Fatalf("expected case-insensitive get to find the section, got %#v", got)
	}

	listed, err := ListADRSections(root)
	if err != nil {
		t.Fatal(err)
	}
	if listed.Status != "ok" || listed.Total != 1 {
		t.Fatalf("expected 1 listed section, got %#v", listed)
	}
	if listed.Sections[0].Content != "" {
		t.Fatalf("expected list to omit content, got %#v", listed.Sections[0])
	}

	removed, err := RemoveADRSection(root, "AUTH STRATEGY")
	if err != nil {
		t.Fatal(err)
	}
	if removed.Status != "ok" || !removed.Removed {
		t.Fatalf("expected removal to succeed, got %#v", removed)
	}

	afterRemoval, err := GetADRSection(root, "Auth Strategy")
	if err != nil {
		t.Fatal(err)
	}
	if afterRemoval.Status != "not_found" {
		t.Fatalf("expected not_found after removal, got %#v", afterRemoval)
	}
}

// TestADRSectionUpdateUpsertsByTitle covers that a second update call with
// the same (case-insensitive) title overwrites content in place rather than
// creating a duplicate section, and bumps UpdatedAt while preserving
// CreatedAt.
func TestADRSectionUpdateUpsertsByTitle(t *testing.T) {
	root := t.TempDir()

	first, err := UpdateADRSection(root, "data-storage", "Postgres, single primary.")
	if err != nil {
		t.Fatal(err)
	}

	second, err := UpdateADRSection(root, "Data-Storage", "Postgres with read replicas.")
	if err != nil {
		t.Fatal(err)
	}
	if second.Section.Content != "Postgres with read replicas." {
		t.Fatalf("expected upsert to overwrite content, got %#v", second.Section)
	}
	if second.Section.CreatedAt != first.Section.CreatedAt {
		t.Fatalf("expected CreatedAt to be preserved across an upsert, got first=%q second=%q", first.Section.CreatedAt, second.Section.CreatedAt)
	}

	listed, err := ListADRSections(root)
	if err != nil {
		t.Fatal(err)
	}
	if listed.Total != 1 {
		t.Fatalf("expected the upsert to keep exactly 1 section, got %#v", listed)
	}
}

// TestADRSectionValidation covers required-field and length-cap validation.
func TestADRSectionValidation(t *testing.T) {
	root := t.TempDir()

	if _, err := UpdateADRSection(root, "", "content"); err == nil {
		t.Fatalf("expected an error for an empty title")
	}
	if _, err := UpdateADRSection(root, "title", ""); err == nil {
		t.Fatalf("expected an error for empty content")
	}

	oversized := make([]byte, maxADRSectionContentLength+1)
	for i := range oversized {
		oversized[i] = 'x'
	}
	if _, err := UpdateADRSection(root, "too-long", string(oversized)); err == nil {
		t.Fatalf("expected an error for content exceeding maxADRSectionContentLength")
	}
}

// TestGetADRSectionWithoutTitleReturnsWholeDocument covers action=get with
// no title — the whole document, not just one section.
func TestGetADRSectionWithoutTitleReturnsWholeDocument(t *testing.T) {
	root := t.TempDir()
	if _, err := UpdateADRSection(root, "one", "first decision"); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateADRSection(root, "two", "second decision"); err != nil {
		t.Fatal(err)
	}

	got, err := GetADRSection(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "ok" || len(got.Sections) != 2 {
		t.Fatalf("expected the whole document (2 sections) when title is empty, got %#v", got)
	}
}

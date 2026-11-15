package aiassist

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johandry/vueltario/internal/config"
	"github.com/johandry/vueltario/internal/db"
)

func TestBuildPromptAndParseAIExtractionMapsAliasesAndCreatesNewMoves(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cfg := config.Load()
	cfg.DBDriver = "sqlite"
	cfg.DatabaseURL = filepath.Join(tmp, "ai_test.db")

	store, err := db.Open(cfg.DBDriver, cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()
	if err := store.MigrateUp(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	admin, err := store.EnsureBootstrap(ctx, "admin@vueltario.com", "admin-pass-123")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	saved, err := store.SaveLink(ctx, db.SaveLinkRequest{
		UserID: admin.ID,
		RawURL: "https://www.facebook.com/reel/1507199801474207",
	})
	if err != nil {
		t.Fatalf("save link: %v", err)
	}

	tax, err := LoadTaxonomyFromStore(ctx, store)
	if err != nil {
		t.Fatalf("load tax: %v", err)
	}

	prompt := BuildExtractionPrompt(PromptSourceInfo{
		EntryID:      saved.Entry.ID,
		OriginalURL:  saved.Source.OriginalURL,
		CanonicalURL: saved.Source.CanonicalURL,
		Platform:     string(saved.Source.Platform),
		FramePaths:   []string{"/tmp/frame_01.jpg"},
	}, tax)

	if !strings.Contains(prompt, "https://www.facebook.com/reel/1507199801474207") ||
		!strings.Contains(prompt, "Setenta (aliases: 70)") ||
		!strings.Contains(prompt, "/tmp/frame_01.jpg") {
		t.Fatalf("unexpected prompt output:\n%s", prompt)
	}

	// Simulate fenced ChatGPT / Claude response for the Casinoconelpelon Facebook Reel screenshot
	fencedAIResponse := "```json\n" + `{
  "title": "Montaña, Evelyn, 70, Aventura & Consorte Combo",
  "creator_name": "Casinoconelpelon",
  "creator_handle": "@casinoconelpelon",
  "creator_profile_url": "https://www.facebook.com/casinoconelpelon",
  "creator_role": "original",
  "duration": "01:36",
  "public_comment": "Social combination breakdown",
  "private_notes": "Extracted from on-screen Reel overlay",
  "occurrences": [
    { "move_name": "Montaña", "is_new_move": false, "start": "00:05", "end": "00:18", "notes": "Opening figure" },
    { "move_name": "70", "is_new_move": false, "start": null, "end": null, "notes": "Mapped via numeric alias" },
    { "move_name": "Aventura", "is_new_move": true, "start": null, "end": null, "notes": "New move from overlay" },
    { "move_name": "Consorte", "is_new_move": true, "start": null, "end": null, "notes": "" }
  ]
}` + "\n```"

	parsed, err := ParseAndValidateExtraction(fencedAIResponse, tax)
	if err != nil {
		t.Fatalf("parse AI JSON: %v", err)
	}
	if len(parsed.Occurrences) != 4 {
		t.Fatalf("expected 4 occurrences, got %d", len(parsed.Occurrences))
	}
	// Verify "70" alias resolved automatically to canonical "Setenta" (IsNewMove=false)
	if parsed.Occurrences[1].MoveName != "Setenta" || parsed.Occurrences[1].IsNewMove {
		t.Fatalf("expected alias '70' mapped to canonical 'Setenta', got %+v", parsed.Occurrences[1])
	}
	// Verify "Aventura" flagged as new move (IsNewMove=true)
	if !parsed.Occurrences[2].IsNewMove || parsed.Occurrences[2].MoveName != "Aventura" {
		t.Fatalf("expected 'Aventura' flagged as new move, got %+v", parsed.Occurrences[2])
	}

	updated, err := ApplyToStore(ctx, store, *admin, saved.Entry.ID, parsed, true)
	if err != nil {
		t.Fatalf("apply to store: %v", err)
	}
	if updated.PersonalTitleOverride == nil || *updated.PersonalTitleOverride != "Montaña, Evelyn, 70, Aventura & Consorte Combo" {
		t.Fatalf("title not updated: %+v", updated.PersonalTitleOverride)
	}
	if len(updated.Source.Creators) == 0 || updated.Source.Creators[0].Creator.DisplayName != "Casinoconelpelon" {
		t.Fatalf("creator not attributed: %+v", updated.Source.Creators)
	}
	if len(updated.Occurrences) != 4 {
		t.Fatalf("expected 4 saved occurrences, got %d", len(updated.Occurrences))
	}
}

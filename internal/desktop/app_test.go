package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/johandry/vueltario/internal/cli"
	"github.com/johandry/vueltario/internal/config"
	"github.com/johandry/vueltario/internal/domain"
)

// TestWailsDesktopCLIDraftEquivalenceCredentialPersistenceAndBatchIsolation verifies Phase 5 acceptance:
// 1. DESK-01: CLI and Wails GUI produce identical drafts, captions, tags, and SHA-256 hashes for the same inputs.
// 2. Updates/restarts preserve credentials securely via SecretStore (Keychain abstraction), never plaintext files.
// 3. Batch ingest and batch draft workflows isolate per-item failures without aborting valid items.
func TestWailsDesktopCLIDraftEquivalenceCredentialPersistenceAndBatchIsolation(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	cfg := config.Load()
	cfg.DBDriver = "sqlite"
	cfg.DatabaseURL = filepath.Join(tmp, "phase5.db")
	cfg.StorageRoot = filepath.Join(tmp, "storage")
	cfg.FFprobeBinary = ""
	cfg.AdminEmail = "admin@vueltario.com"
	cfg.AdminPassword = "desktop-admin-secret"

	secrets := NewMemorySecretStore()
	deskApp := NewApp(cfg, secrets, nil)
	if err := deskApp.Startup(ctx); err != nil {
		t.Fatalf("desktop startup: %v", err)
	}

	// Authenticate via PKCE and verify session persists across app restart ("update") via SecretStore
	authState, err := deskApp.LoginPKCE(cfg.AdminEmail, cfg.AdminPassword)
	if err != nil || !authState.Authenticated {
		t.Fatalf("desktop PKCE login failed: %v", err)
	}
	deskApp.Shutdown(ctx)

	// Simulate app update / relaunch reusing the same SecretStore
	deskAppAfterUpdate := NewApp(cfg, secrets, nil)
	if err := deskAppAfterUpdate.Startup(ctx); err != nil {
		t.Fatalf("desktop restart after update: %v", err)
	}
	defer deskAppAfterUpdate.Shutdown(ctx)

	if !deskAppAfterUpdate.GetAuthState().Authenticated {
		t.Fatalf("expected desktop session restored from keychain SecretStore after update")
	}

	// Batch ingest with 2 valid + 1 invalid host (isolated failure)
	batchRes, err := deskAppAfterUpdate.BatchIngestURLs([]string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"https://unsupported.example.com/video/123",
		"https://www.instagram.com/reel/C8abcXYZ123/",
	}, "")
	if err != nil {
		t.Fatalf("batch ingest: %v", err)
	}
	if batchRes.Created != 2 || batchRes.Failed != 1 {
		t.Fatalf("expected 2 created and 1 isolated failure, got %+v", batchRes)
	}
	ytEntryID := batchRes.Items[0].EntryID
	ytSourceID := batchRes.Items[0].SourceID
	igEntryID := batchRes.Items[2].EntryID

	if _, err := deskAppAfterUpdate.AddOccurrence(ytEntryID, "Setenta complicado", "00:14", "00:29", ""); err != nil {
		t.Fatalf("add occurrence: %v", err)
	}

	mp4Path := filepath.Join(tmp, "clip.mp4")
	_ = os.WriteFile(mp4Path, []byte{
		0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm',
		0x00, 0x00, 0x02, 0x00, 'i', 's', 'o', 'm', 'm', 'p', '4', '2',
	}, 0o600)
	asset, err := deskAppAfterUpdate.ImportAuthorizedMediaFile(ytSourceID, mp4Path)
	if err != nil {
		t.Fatalf("import media: %v", err)
	}
	if _, err := deskAppAfterUpdate.RecordRights(ytSourceID, asset.ID, "Creator", "Permission #9", true, true); err != nil {
		t.Fatalf("record rights: %v", err)
	}

	// Create draft via Wails Desktop App
	guiDraft, err := deskAppAfterUpdate.CreatePublicationDraft(DesktopDraftInput{
		PrivateEntryID:       ytEntryID,
		AssetID:              asset.ID,
		DestinationAccount:   "@cubansocial.sd",
		BrandPrefix:          "CubanSocial",
		IncludeCompanionTags: true,
	})
	if err != nil {
		t.Fatalf("desktop create draft: %v", err)
	}

	// Create draft for the EXACT same inputs via CLI and assert byte-for-byte caption & SHA-256 DraftHash equivalence (DESK-01)!
	var cliOut, cliErr bytes.Buffer
	cliApp := &cli.App{Cfg: cfg, Stdout: &cliOut, Stderr: &cliErr}
	exitCode := cliApp.Run(ctx, []string{
		"publish", "draft",
		"--entry", ytEntryID,
		"--asset", asset.ID,
		"--account", "@cubansocial.sd",
		"--brand", "CubanSocial",
		"--companion-tags",
		"--json",
	})
	if exitCode != cli.ExitOK {
		t.Fatalf("cli draft failed (%d): %s", exitCode, cliErr.String())
	}
	var cliDraft domain.PublicationJob
	if err := json.Unmarshal(cliOut.Bytes(), &cliDraft); err != nil {
		t.Fatalf("unmarshal cli draft: %v", err)
	}

	if guiDraft.Caption != cliDraft.Caption {
		t.Fatalf("DESK-01 violation: GUI caption != CLI caption\nGUI: %q\nCLI: %q", guiDraft.Caption, cliDraft.Caption)
	}
	if guiDraft.DraftHash != cliDraft.DraftHash {
		t.Fatalf("DESK-01 violation: GUI DraftHash (%s) != CLI DraftHash (%s)", guiDraft.DraftHash, cliDraft.DraftHash)
	}
	if guiDraft.Status != cliDraft.Status {
		t.Fatalf("DESK-01 violation: GUI Status (%s) != CLI Status (%s)", guiDraft.Status, cliDraft.Status)
	}

	// Batch draft workflow: 1 valid Instagram entry + 1 nonexistent entry -> isolated failure without aborting valid draft
	batchDrafts, err := deskAppAfterUpdate.BatchCreateDrafts([]DesktopDraftInput{
		{PrivateEntryID: "non-existent-entry-id"},
		{PrivateEntryID: igEntryID, DestinationAccount: "@cubansocial.sd", BrandPrefix: "CubanSocial"},
	})
	if err != nil {
		t.Fatalf("batch drafts: %v", err)
	}
	if batchDrafts.Created != 1 || batchDrafts.Failed != 1 {
		t.Fatalf("expected 1 created and 1 failed in batch drafts, got %+v", batchDrafts)
	}
}

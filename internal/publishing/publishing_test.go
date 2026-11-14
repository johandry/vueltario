package publishing

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johandry/vueltario/internal/config"
	"github.com/johandry/vueltario/internal/db"
	"github.com/johandry/vueltario/internal/domain"
	"github.com/johandry/vueltario/internal/media"
)

type fakeIGTransport struct {
	createCalls    int
	publishCalls   int
	commentCalls   int
	failPublishAmb bool
	failComment    bool
	remoteStatus   ContainerStatus
}

func (f *fakeIGTransport) CreateReelContainer(ctx context.Context, videoStagingURL, caption string) (string, error) {
	f.createCalls++
	return "cont-1001", nil
}

func (f *fakeIGTransport) GetContainerStatus(ctx context.Context, containerID string) (ContainerStatus, error) {
	if f.remoteStatus.ContainerID != "" {
		return f.remoteStatus, nil
	}
	return ContainerStatus{ContainerID: containerID, StatusCode: "FINISHED"}, nil
}

func (f *fakeIGTransport) PublishContainer(ctx context.Context, containerID string) (string, string, error) {
	f.publishCalls++
	if f.failPublishAmb {
		return "", "", ErrAmbiguousOutcome
	}
	return "ig-media-777", "https://www.instagram.com/reel/Published777/", nil
}

func (f *fakeIGTransport) PostFirstComment(ctx context.Context, mediaID, commentText string) (string, error) {
	f.commentCalls++
	if f.failComment {
		return "", errors.New("comment rate limited")
	}
	return "ig-comment-888", nil
}

func makeMinimalMP4() []byte {
	// Valid ISO BMFF ftyp box header (>= 16 bytes)
	return []byte{
		0x00, 0x00, 0x00, 0x18,
		'f', 't', 'y', 'p',
		'i', 's', 'o', 'm',
		0x00, 0x00, 0x02, 0x00,
		'i', 's', 'o', 'm', 'm', 'p', '4', '2',
	}
}

func setupPublishingEnv(t *testing.T, tr *fakeIGTransport) (context.Context, config.Config, *db.Store, *media.Service, *Service, domain.User) {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	cfg := config.Load()
	cfg.DBDriver = "sqlite"
	cfg.DatabaseURL = filepath.Join(tmp, "test.db")
	cfg.StorageRoot = filepath.Join(tmp, "storage")
	cfg.FFprobeBinary = "" // Deterministic unit test without external probe binary
	cfg.IGDestination = "@cubansocial.sd"
	cfg.IGBrandPrefix = "CubanSocial"
	cfg.IGAllowCompanionTags = true

	store, err := db.Open(cfg.DBDriver, cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.MigrateUp(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	admin, err := store.EnsureBootstrap(ctx, "admin@vueltario.com", "strong-admin-pass")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	mediaSvc, err := media.NewService(cfg, store)
	if err != nil {
		t.Fatalf("media svc: %v", err)
	}
	pubSvc := NewService(cfg, store, tr)
	return ctx, cfg, store, mediaSvc, pubSvc, *admin
}

func TestExternalPublishApprovalInvalidationOutcomeUnknownAndCommentIsolation(t *testing.T) {
	tr := &fakeIGTransport{failPublishAmb: true, failComment: true}
	ctx, _, store, mediaSvc, pubSvc, admin := setupPublishingEnv(t, tr)

	saved, err := store.SaveLink(ctx, db.SaveLinkRequest{
		UserID: admin.ID,
		RawURL: "https://www.tiktok.com/@maestrocubano/video/7345678901234567890",
	})
	if err != nil {
		t.Fatalf("save link: %v", err)
	}
	// Attribute verified creator + two moves (Section 12 illustrative example)
	_, err = store.UpdateEntry(ctx, db.UpdateEntryInput{
		UserID:           admin.ID,
		EntryID:          saved.Entry.ID,
		ExpectedRevision: saved.Entry.Revision,
		CreatorAttribution: &db.CreatorAttributionInput{
			DisplayName: "Maestro Cubano",
			Role:        "original",
			Handle:      "maestro_cubano",
			ProfileURL:  "https://www.tiktok.com/@maestrocubano",
			Verified:    true,
		},
	})
	if err != nil {
		t.Fatalf("update creator: %v", err)
	}
	m1 := "Enchufla doble"
	m2 := "Dile que no"
	_, _ = store.AddOccurrence(ctx, db.OccurrenceInput{UserID: admin.ID, EntryID: saved.Entry.ID, ProposedName: &m1})
	_, _ = store.AddOccurrence(ctx, db.OccurrenceInput{UserID: admin.ID, EntryID: saved.Entry.ID, ProposedName: &m2})

	// 1. Draft without asset -> needs_asset
	draftNoAsset, err := pubSvc.CreateDraft(ctx, DraftRequest{
		AdminUser:      admin,
		PrivateEntryID: &saved.Entry.ID,
	})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if draftNoAsset.Status != domain.PubNeedsAsset {
		t.Fatalf("expected needs_asset, got %s", draftNoAsset.Status)
	}

	// 2. Import authorized MP4 -> without rights -> needs_rights
	asset, err := mediaSvc.ImportAuthorizedFile(ctx, media.ImportRequest{
		User:     admin,
		SourceID: saved.Source.ID,
		Filename: "lesson.mp4",
		Reader:   bytes.NewReader(makeMinimalMP4()),
	})
	if err != nil {
		t.Fatalf("import asset: %v", err)
	}
	draftNoRights, err := pubSvc.CreateDraft(ctx, DraftRequest{
		AdminUser:      admin,
		PrivateEntryID: &saved.Entry.ID,
		AssetID:        &asset.ID,
	})
	if err != nil || draftNoRights.Status != domain.PubNeedsRights {
		t.Fatalf("expected needs_rights, got %v / %v", draftNoRights, err)
	}
	if _, err := pubSvc.Approve(ctx, admin, draftNoRights.ID, draftNoRights.DraftHash, ""); err == nil {
		t.Fatalf("approval must fail when rights are missing")
	}

	// 3. Record BOTH video and music clearance -> ready_for_review
	_, err = store.CreateRightsRecord(ctx, domain.RightsRecord{
		SourceID:            saved.Source.ID,
		AssetID:             &asset.ID,
		Grantor:             "Maestro Cubano",
		Basis:               "Signed permission #42",
		VideoCleared:        true,
		MusicCleared:        true,
		AuthorizedUses:      []string{"republish"},
		AuthorizedPlatforms: []string{"instagram"},
		ReviewerID:          admin.ID,
	})
	if err != nil {
		t.Fatalf("create rights: %v", err)
	}

	firstComment := "Practice slow before Timba tempo!"
	draft, err := pubSvc.CreateDraft(ctx, DraftRequest{
		AdminUser:            admin,
		PrivateEntryID:       &saved.Entry.ID,
		AssetID:              &asset.ID,
		IncludeCompanionTags: false,
		CustomFirstComment:   &firstComment,
	})
	if err != nil {
		t.Fatalf("create ready draft: %v", err)
	}
	if draft.Status != domain.PubReadyForReview {
		t.Fatalf("expected ready_for_review, got %s", draft.Status)
	}
	// Verify caption formatting & strict dash-tag behavior (no companion tag unless requested!)
	if !strings.Contains(draft.Caption, "Enchufla doble → Dile que no") ||
		!strings.Contains(draft.Caption, "Follow @maestro_cubano") ||
		!strings.Contains(draft.Caption, "#CubanSocial-EnchuflaDoble #CubanSocial-DileQueNo") ||
		strings.Contains(draft.Caption, "#CubanSocial_EnchuflaDoble") {
		t.Fatalf("unexpected caption:\n%s", draft.Caption)
	}

	// 4. Approve exact hash, then edit -> approval MUST be invalidated (PUB-03)
	approved, err := pubSvc.Approve(ctx, admin, draft.ID, draft.DraftHash, "@cubansocial.sd")
	if err != nil || approved.Status != domain.PubApproved {
		t.Fatalf("approve failed: %v", err)
	}
	enableComp := true
	edited, err := pubSvc.UpdateDraft(ctx, UpdateDraftInput{
		AdminUser:            admin,
		PublicationID:        approved.ID,
		IncludeCompanionTags: &enableComp,
	})
	if err != nil {
		t.Fatalf("update draft: %v", err)
	}
	if edited.ApprovedDraftHash != "" || edited.Status != domain.PubReadyForReview {
		t.Fatalf("editing draft must invalidate approval, got %+v", edited)
	}
	if _, err := pubSvc.Execute(ctx, ExecuteOptions{AdminUser: admin, PublicationID: edited.ID}); !errors.Is(err, ErrApprovalInvalid) {
		t.Fatalf("executing unapproved edited draft must fail with ErrApprovalInvalid, got %v", err)
	}

	// 5. Re-approve new hash & test Dry-Run (zero remote calls!)
	reapproved, err := pubSvc.Approve(ctx, admin, edited.ID, edited.DraftHash, "@cubansocial.sd")
	if err != nil {
		t.Fatalf("re-approve: %v", err)
	}
	if _, err := pubSvc.Execute(ctx, ExecuteOptions{AdminUser: admin, PublicationID: reapproved.ID, DryRun: true}); err != nil {
		t.Fatalf("dry-run failed: %v", err)
	}
	if tr.createCalls != 0 || tr.publishCalls != 0 {
		t.Fatalf("dry-run must perform zero remote transport calls")
	}

	// 6. Execute with simulated timeout during PublishContainer -> outcome_unknown (Section 17 concrete example!)
	unkJob, err := pubSvc.Execute(ctx, ExecuteOptions{AdminUser: admin, PublicationID: reapproved.ID})
	if !errors.Is(err, ErrAmbiguousOutcome) {
		t.Fatalf("expected ErrAmbiguousOutcome, got %v", err)
	}
	if unkJob.Status != domain.PubOutcomeUnknown || unkJob.RemoteContainerID != "cont-1001" {
		t.Fatalf("expected outcome_unknown with persisted container ID, got %+v", unkJob)
	}
	// Retry without reconcile MUST be blocked so we never create a duplicate Reel!
	if _, err := pubSvc.Execute(ctx, ExecuteOptions{AdminUser: admin, PublicationID: reapproved.ID}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected conflict blocking retry while outcome_unknown, got %v", err)
	}
	if tr.publishCalls != 1 {
		t.Fatalf("expected publishCalls to remain 1, got %d", tr.publishCalls)
	}

	// 7. Reconcile discovers remote container actually published -> recovers without second upload!
	tr.remoteStatus = ContainerStatus{
		ContainerID:  "cont-1001",
		StatusCode:   "PUBLISHED",
		PublishedID:  "ig-media-777",
		PublishedURL: "https://www.instagram.com/reel/Published777/",
	}
	// First reconcile with failComment=true -> video is published, comment status is 'failed' without unpublishing!
	reconciled, err := pubSvc.Reconcile(ctx, admin, reapproved.ID, "", true)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if reconciled.Status != domain.PubPublished || reconciled.FirstCommentStatus != "failed" {
		t.Fatalf("expected published video with isolated failed first comment, got status=%s comment=%s", reconciled.Status, reconciled.FirstCommentStatus)
	}
	if tr.publishCalls != 1 {
		t.Fatalf("reconcile must not re-invoke PublishContainer, got %d calls", tr.publishCalls)
	}

	// 8. Retry first comment independently -> succeeds without republishing video (PUB-05)!
	tr.failComment = false
	reconciled2, err := pubSvc.Reconcile(ctx, admin, reapproved.ID, "", true)
	if err != nil || reconciled2.FirstCommentStatus != "posted" {
		t.Fatalf("expected first comment posted on retry, got %+v / %v", reconciled2, err)
	}
	if tr.publishCalls != 1 {
		t.Fatalf("comment retry must never republish video")
	}
}

func TestInstagramOriginUsesManualNativeRepostHandoff(t *testing.T) {
	tr := &fakeIGTransport{}
	ctx, _, store, _, pubSvc, admin := setupPublishingEnv(t, tr)

	saved, err := store.SaveLink(ctx, db.SaveLinkRequest{
		UserID: admin.ID,
		RawURL: "https://www.instagram.com/reel/C8abcXYZ123/",
	})
	if err != nil {
		t.Fatalf("save ig link: %v", err)
	}
	mv := "Sombrero"
	_, _ = store.AddOccurrence(ctx, db.OccurrenceInput{UserID: admin.ID, EntryID: saved.Entry.ID, ProposedName: &mv})

	draft, err := pubSvc.CreateDraft(ctx, DraftRequest{
		AdminUser:      admin,
		PrivateEntryID: &saved.Entry.ID,
	})
	if err != nil {
		t.Fatalf("create ig draft: %v", err)
	}
	if draft.PublicationMode != "native_repost_handoff" {
		t.Fatalf("expected native_repost_handoff, got %s", draft.PublicationMode)
	}
	if strings.Contains(draft.Caption, "Follow @") {
		t.Fatalf("Instagram-origin draft must omit extra follow promotion (PUB-02), got:\n%s", draft.Caption)
	}

	approved, err := pubSvc.Approve(ctx, admin, draft.ID, draft.DraftHash, "@cubansocial.sd")
	if err != nil {
		t.Fatalf("approve ig handoff: %v", err)
	}
	handedOff, err := pubSvc.Execute(ctx, ExecuteOptions{AdminUser: admin, PublicationID: approved.ID})
	if err != nil {
		t.Fatalf("execute ig handoff: %v", err)
	}
	// Phase 3 acceptance: native repost is NEVER reported published until manually confirmed or verified!
	if handedOff.Status != domain.PubManualHandoff {
		t.Fatalf("expected manual_handoff, got %s", handedOff.Status)
	}
	if tr.createCalls != 0 || tr.publishCalls != 0 {
		t.Fatalf("native repost handoff must not upload a copy as a new Reel")
	}

	confirmed, err := pubSvc.Reconcile(ctx, admin, handedOff.ID, "https://www.instagram.com/cubansocial.sd/", false)
	if err != nil || confirmed.Status != domain.PubPublished {
		t.Fatalf("expected manual confirmation to transition to published, got %+v / %v", confirmed, err)
	}
}

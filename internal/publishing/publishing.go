package publishing

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/johandry/vueltario/internal/config"
	"github.com/johandry/vueltario/internal/db"
	"github.com/johandry/vueltario/internal/domain"
	"github.com/johandry/vueltario/internal/storage"
)

var (
	ErrAmbiguousOutcome = errors.New("ambiguous remote outcome during publish; reconciliation required")
	ErrTokenExpired     = errors.New("instagram access token missing, expired, or lacks publish scope")
	ErrApprovalInvalid  = errors.New("publication approval missing or invalidated by draft/asset changes")
)

type ContainerStatus struct {
	ContainerID   string
	StatusCode    string // "IN_PROGRESS", "FINISHED", "PUBLISHED", "ERROR", "EXPIRED"
	PublishedID   string
	PublishedURL  string
	StatusMessage string
}

// Transport abstracts the official Instagram Content Publishing API so both live Graph calls and deterministic tests use identical rules.
type Transport interface {
	CreateReelContainer(ctx context.Context, videoStagingURL, caption string) (containerID string, err error)
	GetContainerStatus(ctx context.Context, containerID string) (ContainerStatus, error)
	PublishContainer(ctx context.Context, containerID string) (mediaID string, permalink string, err error)
	PostFirstComment(ctx context.Context, mediaID, commentText string) (commentID string, err error)
}

type Service struct {
	cfg        config.Config
	store      *db.Store
	transport  Transport
	cloudStore storage.ObjectStore
}

func NewService(cfg config.Config, store *db.Store, transport Transport) *Service {
	if transport == nil {
		transport = NewGraphTransport(cfg)
	}
	var cloud storage.ObjectStore
	switch cfg.StorageProvider {
	case "r2":
		if r2, err := storage.NewR2Store(storage.R2Config{
			AccountID:       cfg.R2AccountID,
			Endpoint:        cfg.R2Endpoint,
			Bucket:          cfg.R2Bucket,
			AccessKeyID:     cfg.R2AccessKeyID,
			SecretAccessKey: cfg.R2SecretAccessKey,
			PublicBaseURL:   cfg.R2PublicBaseURL,
		}); err == nil {
			cloud = r2
		}
	case "supabase":
		if sb, err := storage.NewSupabaseStorage(cfg.SupabaseURL, cfg.SupabaseServiceKey, cfg.SupabaseServiceKey, cfg.SupabaseBucket); err == nil {
			cloud = sb
		}
	}
	return &Service{
		cfg:        cfg,
		store:      store,
		transport:  transport,
		cloudStore: cloud,
	}
}

func (s *Service) SetCloudStore(cs storage.ObjectStore) {
	s.cloudStore = cs
}

type DraftRequest struct {
	AdminUser            domain.User
	PublicRevisionID     *string
	PrivateEntryID       *string
	AssetID              *string
	DestinationAccount   string
	BrandPrefix          string
	SelectedMoveNames    []string
	SelectedSegments     []domain.SegmentRange
	IncludeCompanionTags bool
	TagsInFirstComment   bool
	CustomCaption        *string
	CustomFirstComment   *string
}

// CreateDraft implements PUB-01 & PUB-02, assembling the publication draft, computing hashes, and checking rights/asset gates.
func (s *Service) CreateDraft(ctx context.Context, req DraftRequest) (*domain.PublicationJob, error) {
	if req.AdminUser.Role != domain.RoleAdmin {
		return nil, domain.ErrForbidden
	}
	if (req.PublicRevisionID == nil || *req.PublicRevisionID == "") && (req.PrivateEntryID == nil || *req.PrivateEntryID == "") {
		return nil, fmt.Errorf("%w: either public_revision_id or private_entry_id is required", domain.ErrValidation)
	}

	var (
		src          *domain.Source
		payload      domain.PublicPayload
		privRev      *int
	)
	if req.PublicRevisionID != nil && *req.PublicRevisionID != "" {
		pr, err := s.store.GetPublicRevision(ctx, *req.PublicRevisionID)
		if err != nil {
			return nil, err
		}
		if pr.Status != "active" {
			return nil, fmt.Errorf("%w: public revision %s is %s", domain.ErrValidation, pr.ID, pr.Status)
		}
		payload = pr.Payload
		src, err = s.store.GetSource(ctx, pr.SourceID)
		if err != nil {
			return nil, err
		}
	} else {
		ent, err := s.store.GetEntry(ctx, req.AdminUser.ID, *req.PrivateEntryID)
		if err != nil {
			return nil, err
		}
		rv := ent.Revision
		privRev = &rv
		payload = s.store.BuildPublicSnapshotFromEntry(*ent)
		src = &ent.Source
	}

	dest := strings.TrimSpace(req.DestinationAccount)
	if dest == "" {
		dest = s.cfg.IGDestination
	}
	brand := domain.SanitizeBrandPrefix(req.BrandPrefix)
	if req.BrandPrefix == "" {
		brand = domain.SanitizeBrandPrefix(s.cfg.IGBrandPrefix)
	}
	if req.IncludeCompanionTags && !s.cfg.IGAllowCompanionTags {
		return nil, fmt.Errorf("%w: underscore companion hashtags are disabled by configuration", domain.ErrValidation)
	}

	movesCatalog, _ := s.store.ListMoves(ctx, "", true)
	disambByKey := make(map[string]string)
	for _, m := range movesCatalog {
		disambByKey[m.NormalizedSearchKey] = m.Disambiguator
	}

	moveNames := collectOrderedMoveNames(payload.Occurrences, req.SelectedMoveNames)
	var moveTags []domain.MoveTag
	for _, mn := range moveNames {
		dis := disambByKey[domain.NormalizeSearchKey(mn)]
		moveTags = append(moveTags, domain.BuildMoveTag(brand, mn, dis, req.IncludeCompanionTags))
	}

	mode := "external_upload"
	if src.Platform == domain.PlatformInstagram {
		mode = "native_repost_handoff"
	}

	caption, firstComment, repostGuide := s.composeTexts(*src, payload, moveNames, moveTags, req.TagsInFirstComment)
	if req.CustomCaption != nil {
		caption = strings.TrimSpace(*req.CustomCaption)
	}
	if req.CustomFirstComment != nil {
		firstComment = strings.TrimSpace(*req.CustomFirstComment)
	}
	if err := s.validateCaptionLimits(caption, firstComment); err != nil {
		return nil, err
	}

	var (
		assetHash      string
		rightsSnapshot *domain.RightsRecord
		status         domain.PublicationStatus
		statusDetail   string
	)

	if mode == "native_repost_handoff" {
		status = domain.PubReadyForReview
		statusDetail = "Instagram-origin source uses manual native-repost handoff; review suggested labels and approve handoff."
	} else {
		// External source upload requires both a ready authorized MediaAsset AND cleared video+music rights!
		if req.AssetID == nil || *req.AssetID == "" {
			status = domain.PubNeedsAsset
			statusDetail = "External-platform publication requires an imported authorized video asset."
		} else {
			asset, err := s.store.GetMediaAsset(ctx, *req.AssetID)
			if err != nil || asset.Status != "ready" || asset.DeletedAt != nil || asset.SourceID != src.ID {
				status = domain.PubNeedsAsset
				statusDetail = "Selected media asset is missing, not ready, or belongs to a different source."
			} else {
				assetHash = asset.SHA256Hex
				rr, rErr := s.store.FindEligibleInstagramRights(ctx, src.ID, &asset.ID)
				if rErr != nil {
					status = domain.PubNeedsRights
					statusDetail = "Missing active rights record clearing BOTH video and music for Instagram redistribution."
				} else {
					rightsSnapshot = rr
					status = domain.PubReadyForReview
					statusDetail = "Asset and rights verified; awaiting administrator approval of exact draft hash."
				}
			}
		}
	}

	firstCommentStatus := "none"
	if firstComment != "" {
		firstCommentStatus = "pending"
	}

	pub := domain.PublicationJob{
		DestinationAccount:   dest,
		BrandPrefix:          brand,
		OriginPlatform:       src.Platform,
		PublicationMode:      mode,
		SourceID:             src.ID,
		PublicRevisionID:     req.PublicRevisionID,
		PrivateEntryID:       req.PrivateEntryID,
		PrivateEntryRevision: privRev,
		AssetID:              req.AssetID,
		SelectedSegments:     req.SelectedSegments,
		MoveTags:             moveTags,
		IncludeCompanionTags: req.IncludeCompanionTags,
		Caption:              caption,
		FirstComment:         firstComment,
		FirstCommentStatus:   firstCommentStatus,
		AssetHash:            assetHash,
		RightsSnapshot:       rightsSnapshot,
		Status:               status,
		StatusDetail:         statusDetail,
		CreatedBy:            req.AdminUser.ID,
	}
	pub.DraftHash = ComputeDraftHash(pub)

	saved, err := s.store.SavePublicationJob(ctx, pub)
	if err != nil {
		return nil, err
	}
	saved.NativeRepostGuide = repostGuide
	_ = s.store.RecordAudit(ctx, &req.AdminUser.ID, string(domain.RoleAdmin), "publication.draft_created", "publication_job", saved.ID, 1, saved.DraftHash, nil, "")
	return saved, nil
}

type UpdateDraftInput struct {
	AdminUser            domain.User
	PublicationID        string
	Caption              *string
	FirstComment         *string
	IncludeCompanionTags *bool
	AssetID              *string
}

// UpdateDraft edits a draft and unconditionally invalidates any prior approval (PUB-03).
func (s *Service) UpdateDraft(ctx context.Context, in UpdateDraftInput) (*domain.PublicationJob, error) {
	if in.AdminUser.Role != domain.RoleAdmin {
		return nil, domain.ErrForbidden
	}
	pub, err := s.store.GetPublicationJob(ctx, in.PublicationID)
	if err != nil {
		return nil, err
	}
	if pub.Status == domain.PubPublished || pub.Status == domain.PubPublishing || pub.Status == domain.PubOutcomeUnknown {
		return nil, fmt.Errorf("%w: cannot edit publication in state %s", domain.ErrConflict, pub.Status)
	}

	if in.Caption != nil {
		pub.Caption = strings.TrimSpace(*in.Caption)
	}
	if in.FirstComment != nil {
		pub.FirstComment = strings.TrimSpace(*in.FirstComment)
		if pub.FirstComment == "" {
			pub.FirstCommentStatus = "none"
		} else {
			pub.FirstCommentStatus = "pending"
		}
	}
	if in.IncludeCompanionTags != nil && *in.IncludeCompanionTags != pub.IncludeCompanionTags {
		if *in.IncludeCompanionTags && !s.cfg.IGAllowCompanionTags {
			return nil, fmt.Errorf("%w: companion tags are disabled", domain.ErrValidation)
		}
		pub.IncludeCompanionTags = *in.IncludeCompanionTags
		var rebuilt []domain.MoveTag
		for _, mt := range pub.MoveTags {
			rebuilt = append(rebuilt, domain.BuildMoveTag(pub.BrandPrefix, mt.CanonicalName, "", pub.IncludeCompanionTags))
		}
		pub.MoveTags = rebuilt
	}
	if in.AssetID != nil {
		pub.AssetID = in.AssetID
	}
	if err := s.validateCaptionLimits(pub.Caption, pub.FirstComment); err != nil {
		return nil, err
	}

	// Re-evaluate asset/rights eligibility and INVALIDATE approval!
	pub.ApprovedBy = nil
	pub.ApprovedAt = nil
	pub.ApprovedDraftHash = ""
	pub.ApprovedAssetHash = ""

	if pub.PublicationMode == "native_repost_handoff" {
		pub.Status = domain.PubReadyForReview
		pub.StatusDetail = "Draft edited; re-approval required."
	} else {
		if pub.AssetID == nil || *pub.AssetID == "" {
			pub.AssetHash = ""
			pub.Status = domain.PubNeedsAsset
			pub.StatusDetail = "Missing authorized video asset."
		} else {
			asset, aErr := s.store.GetMediaAsset(ctx, *pub.AssetID)
			if aErr != nil || asset.Status != "ready" || asset.DeletedAt != nil {
				pub.AssetHash = ""
				pub.Status = domain.PubNeedsAsset
				pub.StatusDetail = "Selected asset is not ready."
			} else {
				pub.AssetHash = asset.SHA256Hex
				rr, rErr := s.store.FindEligibleInstagramRights(ctx, pub.SourceID, &asset.ID)
				if rErr != nil {
					pub.RightsSnapshot = nil
					pub.Status = domain.PubNeedsRights
					pub.StatusDetail = "Missing active video+music rights for Instagram."
				} else {
					pub.RightsSnapshot = rr
					pub.Status = domain.PubReadyForReview
					pub.StatusDetail = "Draft updated; prior approval invalidated—please review and approve the new hash."
				}
			}
		}
	}

	pub.DraftHash = ComputeDraftHash(*pub)
	saved, err := s.store.SavePublicationJob(ctx, *pub)
	if err != nil {
		return nil, err
	}
	s.populateHandoffGuide(saved)
	return saved, nil
}

// Approve binds administrator approval to the exact draft_hash, asset_hash, destination, and rights (PUB-03).
func (s *Service) Approve(ctx context.Context, admin domain.User, pubID, expectedDraftHash, expectedDestination string) (*domain.PublicationJob, error) {
	if admin.Role != domain.RoleAdmin {
		return nil, domain.ErrForbidden
	}
	pub, err := s.store.GetPublicationJob(ctx, pubID)
	if err != nil {
		return nil, err
	}
	currentHash := ComputeDraftHash(*pub)
	if expectedDraftHash == "" || currentHash != expectedDraftHash || pub.DraftHash != expectedDraftHash {
		return nil, fmt.Errorf("%w: draft hash mismatch (current=%s, supplied=%s)", domain.ErrConflict, currentHash, expectedDraftHash)
	}
	if expectedDestination != "" && !strings.EqualFold(strings.TrimSpace(expectedDestination), pub.DestinationAccount) {
		return nil, fmt.Errorf("%w: destination account mismatch (draft=%s, supplied=%s)", domain.ErrValidation, pub.DestinationAccount, expectedDestination)
	}

	if pub.PublicationMode == "external_upload" {
		if pub.AssetID == nil || *pub.AssetID == "" {
			return nil, fmt.Errorf("%w: cannot approve external upload without an authorized media asset", domain.ErrValidation)
		}
		asset, err := s.store.GetMediaAsset(ctx, *pub.AssetID)
		if err != nil || asset.Status != "ready" || asset.DeletedAt != nil {
			return nil, fmt.Errorf("%w: media asset is unavailable", domain.ErrValidation)
		}
		if asset.SHA256Hex != pub.AssetHash {
			return nil, fmt.Errorf("%w: media asset checksum changed since draft creation", domain.ErrConflict)
		}
		rr, err := s.store.FindEligibleInstagramRights(ctx, pub.SourceID, &asset.ID)
		if err != nil {
			return nil, fmt.Errorf("%w: active video and music rights record is required before approval", domain.ErrValidation)
		}
		pub.RightsSnapshot = rr
	}

	now := time.Now().UTC()
	pub.ApprovedBy = &admin.ID
	pub.ApprovedAt = &now
	pub.ApprovedDraftHash = currentHash
	pub.ApprovedAssetHash = pub.AssetHash
	pub.Status = domain.PubApproved
	pub.StatusDetail = fmt.Sprintf("Approved by %s for %s (hash %s)", admin.Email, pub.DestinationAccount, currentHash[:12])

	saved, err := s.store.SavePublicationJob(ctx, *pub)
	if err != nil {
		return nil, err
	}
	s.populateHandoffGuide(saved)
	_ = s.store.RecordAudit(ctx, &admin.ID, string(domain.RoleAdmin), "publication.approved", "publication_job", saved.ID, 1, currentHash, nil, "")
	return saved, nil
}

type ExecuteOptions struct {
	AdminUser      domain.User
	PublicationID  string
	IdempotencyKey string
	DryRun         bool
}

// Execute runs an approved publication job (or dry-run validation without remote calls).
func (s *Service) Execute(ctx context.Context, opts ExecuteOptions) (*domain.PublicationJob, error) {
	if opts.AdminUser.Role != domain.RoleAdmin {
		return nil, domain.ErrForbidden
	}
	pub, err := s.store.GetPublicationJob(ctx, opts.PublicationID)
	if err != nil {
		return nil, err
	}

	// PUB-04: Never duplicate a published item or retry an outcome_unknown job without reconciliation!
	if pub.Status == domain.PubPublished {
		s.populateHandoffGuide(pub)
		return pub, nil
	}
	if pub.Status == domain.PubOutcomeUnknown {
		return pub, fmt.Errorf("%w: run reconcile before retrying publication %s", domain.ErrConflict, pub.ID)
	}
	if opts.IdempotencyKey != "" && pub.ExecuteIdempotencyKey == opts.IdempotencyKey && pub.Status == domain.PubManualHandoff {
		s.populateHandoffGuide(pub)
		return pub, nil
	}

	currentHash := ComputeDraftHash(*pub)
	if pub.ApprovedBy == nil || pub.ApprovedDraftHash == "" || pub.ApprovedDraftHash != currentHash {
		return nil, ErrApprovalInvalid
	}

	if pub.PublicationMode == "native_repost_handoff" {
		if opts.DryRun {
			clone := *pub
			clone.StatusDetail = "[DRY-RUN] Verified Instagram native-repost handoff package; no external mutation performed."
			s.populateHandoffGuide(&clone)
			return &clone, nil
		}
		pub.Status = domain.PubManualHandoff
		if opts.IdempotencyKey != "" {
			pub.ExecuteIdempotencyKey = opts.IdempotencyKey
		}
		pub.StatusDetail = "Manual native-repost handoff ready. Repost natively in the Instagram app; confirm result URL once completed."
		saved, err := s.store.SavePublicationJob(ctx, *pub)
		if err != nil {
			return nil, err
		}
		s.populateHandoffGuide(saved)
		_ = s.store.RecordAudit(ctx, &opts.AdminUser.ID, string(domain.RoleAdmin), "publication.manual_handoff", "publication_job", saved.ID, 1, "", nil, "")
		return saved, nil
	}

	// External-upload preflight re-check (rights + asset hash)
	if pub.AssetID == nil {
		return nil, fmt.Errorf("%w: missing asset_id", domain.ErrValidation)
	}
	asset, err := s.store.GetMediaAsset(ctx, *pub.AssetID)
	if err != nil || asset.Status != "ready" || asset.DeletedAt != nil {
		return nil, fmt.Errorf("%w: asset unavailable", domain.ErrValidation)
	}
	if asset.SHA256Hex != pub.ApprovedAssetHash {
		return nil, fmt.Errorf("%w: asset hash changed after approval", ErrApprovalInvalid)
	}
	if _, err := s.store.FindEligibleInstagramRights(ctx, pub.SourceID, &asset.ID); err != nil {
		pub.Status = domain.PubNeedsRights
		pub.StatusDetail = "Rights record expired or was revoked prior to execution."
		_, _ = s.store.SavePublicationJob(ctx, *pub)
		return nil, fmt.Errorf("%w: rights no longer valid", domain.ErrForbidden)
	}

	if opts.DryRun {
		clone := *pub
		clone.StatusDetail = "[DRY-RUN] Verified approval hash, video+music rights, asset SHA-256, and caption limits. Zero remote calls executed."
		return &clone, nil
	}

	// Stage short-lived external URL for Meta's fetch workflow:
	// Prefer Cloudflare R2 ($0 egress SigV4 presigned URL) or Supabase Storage signed URL when serverless,
	// falling back to BaseURL staging token in local/test mode.
	var stagingURL string
	if s.cloudStore != nil && (asset.StorageBackend == "r2" || asset.StorageBackend == "supabase") {
		signedURL, signErr := s.cloudStore.PresignGetURL(ctx, asset.StorageKey, 2*time.Hour)
		if signErr != nil {
			return nil, fmt.Errorf("failed generating %s presigned staging URL: %w", s.cloudStore.ProviderName(), signErr)
		}
		stagingURL = signedURL
	} else {
		tok, tErr := generateStagingToken()
		if tErr != nil {
			return nil, tErr
		}
		pub.StagingToken = tok
		stagingURL = fmt.Sprintf("%s/api/v1/staging/%s/media.mp4", s.cfg.BaseURL, tok)
	}
	exp := time.Now().UTC().Add(s.cfg.StagingTTL)
	pub.StagingExpiresAt = &exp
	if opts.IdempotencyKey != "" {
		pub.ExecuteIdempotencyKey = opts.IdempotencyKey
	}
	pub.Status = domain.PubStaging
	pub.StatusDetail = "Staging short-lived media URL for Instagram container creation."
	pub, err = s.store.SavePublicationJob(ctx, *pub)
	if err != nil {
		return nil, err
	}

	// Step 1: Create container (unless already persisted from an earlier retryable container step)
	if pub.RemoteContainerID == "" {
		containerID, cErr := s.transport.CreateReelContainer(ctx, stagingURL, pub.Caption)
		if cErr != nil {
			if errors.Is(cErr, ErrTokenExpired) {
				pub.Status = domain.PubBlockedAuth
				pub.StagingToken = ""
				pub.StatusDetail = "Instagram credentials missing, expired, or lacking instagram_business_content_publish permission."
				_, _ = s.store.SavePublicationJob(ctx, *pub)
				return pub, cErr
			}
			pub.Status = domain.PubFailed
			pub.StagingToken = ""
			pub.StatusDetail = "Failed creating Instagram media container: " + cErr.Error()
			_, _ = s.store.SavePublicationJob(ctx, *pub)
			return pub, cErr
		}
		// Persist remote container ID IMMEDIATELY before subsequent operations (Section 10 & PUB-04)!
		pub.RemoteContainerID = containerID
		pub.Status = domain.PubProcessing
		pub.StatusDetail = "Instagram container created; waiting for media processing."
		pub, err = s.store.SavePublicationJob(ctx, *pub)
		if err != nil {
			return nil, err
		}
	}

	// Step 2: Check container readiness
	st, stErr := s.transport.GetContainerStatus(ctx, pub.RemoteContainerID)
	if stErr != nil {
		if errors.Is(stErr, ErrTokenExpired) {
			pub.Status = domain.PubBlockedAuth
			pub.StatusDetail = stErr.Error()
			_, _ = s.store.SavePublicationJob(ctx, *pub)
			return pub, stErr
		}
		pub.Status = domain.PubRetryWait
		pub.StatusDetail = "Transient error polling container status: " + stErr.Error()
		_, _ = s.store.SavePublicationJob(ctx, *pub)
		return pub, stErr
	}
	if st.StatusCode == "ERROR" || st.StatusCode == "EXPIRED" {
		pub.Status = domain.PubFailed
		pub.StagingToken = ""
		pub.StatusDetail = fmt.Sprintf("Instagram container %s (%s)", st.StatusCode, st.StatusMessage)
		_, _ = s.store.SavePublicationJob(ctx, *pub)
		return pub, fmt.Errorf("container failed: %s", st.StatusCode)
	}
	if st.StatusCode == "IN_PROGRESS" {
		pub.Status = domain.PubProcessing
		pub.StatusDetail = "Instagram media container is still processing; run status/reconcile or re-run once FINISHED."
		return s.store.SavePublicationJob(ctx, *pub)
	}

	// Step 3: Publish container (non-idempotent remote step -> ambiguous errors become outcome_unknown!)
	pub.Status = domain.PubPublishing
	pub.StatusDetail = "Publishing ready container to " + pub.DestinationAccount
	pub, _ = s.store.SavePublicationJob(ctx, *pub)

	mediaID, permalink, pubErr := s.transport.PublishContainer(ctx, pub.RemoteContainerID)
	if pubErr != nil {
		if errors.Is(pubErr, ErrTokenExpired) {
			pub.Status = domain.PubBlockedAuth
			pub.StatusDetail = pubErr.Error()
		} else {
			pub.Status = domain.PubOutcomeUnknown
			pub.StatusDetail = "Ambiguous outcome during media_publish; reconcile remote container before any retry: " + pubErr.Error()
		}
		saved, _ := s.store.SavePublicationJob(ctx, *pub)
		return saved, pubErr
	}

	// Success! Revoke staging token immediately (Section 6) and persist remote media ID + permalink.
	pub.RemoteMediaID = mediaID
	pub.ResultURL = permalink
	pub.StagingToken = ""
	pub.StagingExpiresAt = nil
	pub.Status = domain.PubPublished
	pub.StatusDetail = "Published to " + pub.DestinationAccount

	// Step 4: Optional first comment (PUB-05: separate status & retry; failure NEVER unpublishes or duplicates video!)
	if strings.TrimSpace(pub.FirstComment) != "" {
		cID, cErr := s.transport.PostFirstComment(ctx, mediaID, pub.FirstComment)
		if cErr != nil {
			pub.FirstCommentStatus = "failed"
			pub.FirstCommentError = cErr.Error()
			pub.StatusDetail = "Video published; optional first comment failed and can be retried independently."
		} else {
			pub.RemoteCommentID = cID
			pub.FirstCommentStatus = "posted"
			pub.FirstCommentError = ""
		}
	}

	saved, err := s.store.SavePublicationJob(ctx, *pub)
	if err != nil {
		return nil, err
	}
	_ = s.store.RecordAudit(ctx, &opts.AdminUser.ID, string(domain.RoleAdmin), "publication.published", "publication_job", saved.ID, 1, saved.ResultURL, nil, "")
	return saved, nil
}

// Reconcile resolves outcome_unknown jobs by inspecting remote container status, confirms manual native reposts when resultURL is supplied,
// or retries a failed optional first comment without republishing the video (PUB-04, PUB-05).
func (s *Service) Reconcile(ctx context.Context, admin domain.User, pubID, manualResultURL string, retryFirstComment bool) (*domain.PublicationJob, error) {
	if admin.Role != domain.RoleAdmin {
		return nil, domain.ErrForbidden
	}
	pub, err := s.store.GetPublicationJob(ctx, pubID)
	if err != nil {
		return nil, err
	}

	// Manual confirmation of Instagram native repost handoff
	if pub.PublicationMode == "native_repost_handoff" && strings.TrimSpace(manualResultURL) != "" {
		pub.ResultURL = strings.TrimSpace(manualResultURL)
		pub.Status = domain.PubPublished
		pub.StatusDetail = "Native repost manually confirmed by administrator."
		saved, err := s.store.SavePublicationJob(ctx, *pub)
		if err == nil {
			s.populateHandoffGuide(saved)
			_ = s.store.RecordAudit(ctx, &admin.ID, string(domain.RoleAdmin), "publication.repost_confirmed", "publication_job", saved.ID, 1, pub.ResultURL, nil, "")
		}
		return saved, err
	}

	if pub.Status == domain.PubOutcomeUnknown && pub.RemoteContainerID != "" {
		st, err := s.transport.GetContainerStatus(ctx, pub.RemoteContainerID)
		if err != nil {
			return nil, fmt.Errorf("reconcile query failed: %w", err)
		}
		switch st.StatusCode {
		case "PUBLISHED":
			pub.RemoteMediaID = st.PublishedID
			if st.PublishedURL != "" {
				pub.ResultURL = st.PublishedURL
			}
			pub.StagingToken = ""
			pub.StagingExpiresAt = nil
			pub.Status = domain.PubPublished
			pub.StatusDetail = "Reconciled remote container as PUBLISHED without duplicate upload."
		case "FINISHED":
			// Remote container finished processing and was NOT published yet; safe to return to approved so Execute can publish it.
			pub.Status = domain.PubApproved
			pub.StatusDetail = "Reconciled: remote container is FINISHED and unpublished; safe to execute."
		case "ERROR", "EXPIRED":
			pub.RemoteContainerID = ""
			pub.StagingToken = ""
			pub.Status = domain.PubApproved
			pub.StatusDetail = "Reconciled: prior remote container expired/errored without publishing; safe to execute fresh container."
		default:
			pub.StatusDetail = "Reconcile checked container status: " + st.StatusCode
		}
	}

	// Independent retry of failed first comment (PUB-05)
	if retryFirstComment && pub.Status == domain.PubPublished && pub.RemoteMediaID != "" &&
		pub.FirstComment != "" && pub.FirstCommentStatus != "posted" {
		cID, cErr := s.transport.PostFirstComment(ctx, pub.RemoteMediaID, pub.FirstComment)
		if cErr != nil {
			pub.FirstCommentStatus = "failed"
			pub.FirstCommentError = cErr.Error()
		} else {
			pub.RemoteCommentID = cID
			pub.FirstCommentStatus = "posted"
			pub.FirstCommentError = ""
			pub.StatusDetail = "Published to " + pub.DestinationAccount + " (first comment posted on retry)."
		}
	}

	saved, err := s.store.SavePublicationJob(ctx, *pub)
	if err == nil {
		_ = s.store.RecordAudit(ctx, &admin.ID, string(domain.RoleAdmin), "publication.reconciled", "publication_job", saved.ID, 1, string(saved.Status), nil, "")
	}
	return saved, err
}

// --- Caption & Tag Generation (Section 6 & Section 12) ---

func (s *Service) composeTexts(src domain.Source, payload domain.PublicPayload, moveNames []string, tags []domain.MoveTag, tagsInComment bool) (caption string, firstComment string, repostGuide string) {
	moveLine := "Vuelta de casino"
	if len(moveNames) > 0 {
		moveLine = strings.Join(moveNames, " → ")
	}

	var tagTokens []string
	for _, t := range tags {
		if t.DisplayLabel != "" {
			tagTokens = append(tagTokens, t.DisplayLabel)
		}
		if t.PlatformHashtag != "" {
			tagTokens = append(tagTokens, t.PlatformHashtag)
		}
	}
	tagBlock := strings.Join(tagTokens, " ")

	if src.Platform == domain.PlatformInstagram {
		// PUB-02 & Section 12: Instagram-origin omits extra follow promotion and explains native repost handoff honestly!
		var lines []string
		lines = append(lines, moveLine)
		if payload.CreatorName != "" && payload.CreatorName != "Unknown creator" {
			if payload.CreatorVerified && payload.CreatorHandle != "" {
				lines = append(lines, fmt.Sprintf("Original creator: @%s", strings.TrimPrefix(payload.CreatorHandle, "@")))
			} else {
				lines = append(lines, fmt.Sprintf("Original creator: %s", payload.CreatorName))
			}
		}
		if tagBlock != "" && !tagsInComment {
			lines = append(lines, tagBlock)
		}
		caption = strings.Join(lines, "\n")
		if tagsInComment && tagBlock != "" {
			firstComment = tagBlock
		}
		repostGuide = fmt.Sprintf(
			"Instagram Native Repost Handoff (manual):\n"+
				"1. Open the original post on Instagram: %s\n"+
				"2. Use Instagram's native Repost action from %s (preserves original creator publication).\n"+
				"3. Note: Official Instagram APIs do not verify automated native reposts or replacement captions. If Instagram's native repost note allows short text, copy the move sequence/labels above; otherwise keep them recorded in Vueltario.",
			src.CanonicalURL, s.cfg.IGDestination,
		)
		return caption, firstComment, repostGuide
	}

	// PUB-01 & Section 12: External-origin caption
	platLabel := platformDisplayName(src.Platform)
	creatorAttr := payload.CreatorName
	if creatorAttr == "" {
		creatorAttr = "Unknown creator"
	}

	attributionLines := []string{
		fmt.Sprintf("Original video: %s — %s", creatorAttr, platLabel),
	}
	// Only insert @mention when CreatorIdentity is explicitly verified!
	if payload.CreatorVerified && payload.CreatorHandle != "" {
		cleanHandle := strings.TrimPrefix(payload.CreatorHandle, "@")
		attributionLines = append(attributionLines, fmt.Sprintf("Follow @%s for more Cuban salsa tutorials.", cleanHandle))
	} else if payload.CreatorProfileURL != "" {
		attributionLines = append(attributionLines, fmt.Sprintf("Creator profile: %s", payload.CreatorProfileURL))
	}
	attributionLines = append(attributionLines, fmt.Sprintf("Source: %s", src.CanonicalURL))
	attrText := strings.Join(attributionLines, "\n")

	if tagsInComment {
		firstComment = tagBlock
		caption = assembleBoundedCaption(moveLine, attrText, "", s.cfg.IGCaptionMaxChars)
	} else {
		caption = assembleBoundedCaption(moveLine, attrText, tagBlock, s.cfg.IGCaptionMaxChars)
	}
	return caption, firstComment, ""
}

// assembleBoundedCaption ensures creator attribution is NEVER silently truncated away (Section 12).
func assembleBoundedCaption(moveLine, requiredAttribution, tagBlock string, maxChars int) string {
	if maxChars <= 0 {
		maxChars = 2200
	}
	joinParts := func(m, a, t string) string {
		var parts []string
		if m != "" {
			parts = append(parts, m)
		}
		if a != "" {
			parts = append(parts, a)
		}
		if t != "" {
			parts = append(parts, t)
		}
		return strings.Join(parts, "\n\n")
	}

	full := joinParts(moveLine, requiredAttribution, tagBlock)
	if len([]rune(full)) <= maxChars {
		return full
	}

	// Trim optional tag tokens from the tail first while keeping requiredAttribution intact.
	tagWords := strings.Fields(tagBlock)
	for len(tagWords) > 0 {
		tagWords = tagWords[:len(tagWords)-1]
		candidate := joinParts(moveLine, requiredAttribution, strings.Join(tagWords, " "))
		if len([]rune(candidate)) <= maxChars {
			return candidate
		}
	}

	// Trim moveLine if needed, still preserving full requiredAttribution.
	availForMoves := maxChars - len([]rune(requiredAttribution)) - 4
	if availForMoves > 10 {
		r := []rune(moveLine)
		if len(r) > availForMoves {
			moveLine = string(r[:availForMoves-1]) + "…"
		}
		return joinParts(moveLine, requiredAttribution, "")
	}
	return requiredAttribution
}

var (
	hashtagCountRe = regexp.MustCompile(`#[A-Za-z0-9_]+`)
	mentionCountRe = regexp.MustCompile(`@[A-Za-z0-9_.]+`)
)

func (s *Service) validateCaptionLimits(caption, firstComment string) error {
	if s.cfg.IGCaptionMaxChars > 0 && len([]rune(caption)) > s.cfg.IGCaptionMaxChars {
		return fmt.Errorf("%w: caption length (%d) exceeds configured limit (%d)", domain.ErrValidation, len([]rune(caption)), s.cfg.IGCaptionMaxChars)
	}
	if s.cfg.IGCaptionMaxChars > 0 && len([]rune(firstComment)) > s.cfg.IGCaptionMaxChars {
		return fmt.Errorf("%w: first_comment length (%d) exceeds configured limit (%d)", domain.ErrValidation, len([]rune(firstComment)), s.cfg.IGCaptionMaxChars)
	}
	combined := caption + " " + firstComment
	if s.cfg.IGMaxHashtags > 0 && len(hashtagCountRe.FindAllString(combined, -1)) > s.cfg.IGMaxHashtags {
		return fmt.Errorf("%w: hashtag count exceeds configured limit (%d)", domain.ErrValidation, s.cfg.IGMaxHashtags)
	}
	if s.cfg.IGMaxMentions > 0 && len(mentionCountRe.FindAllString(combined, -1)) > s.cfg.IGMaxMentions {
		return fmt.Errorf("%w: mention count exceeds configured limit (%d)", domain.ErrValidation, s.cfg.IGMaxMentions)
	}
	return nil
}

func (s *Service) populateHandoffGuide(pub *domain.PublicationJob) {
	if pub == nil || pub.PublicationMode != "native_repost_handoff" {
		return
	}
	src, err := s.store.GetSource(context.Background(), pub.SourceID)
	srcURL := ""
	if err == nil && src != nil {
		srcURL = src.CanonicalURL
	}
	pub.NativeRepostGuide = fmt.Sprintf(
		"Instagram Native Repost Handoff (manual): Open %s on Instagram while signed into %s and select Repost. "+
			"Suggested text is preserved above; Instagram native reposts do not expose a verified automation or caption-replacement endpoint.",
		srcURL, pub.DestinationAccount,
	)
}

func ComputeDraftHash(pub domain.PublicationJob) string {
	segs := pub.SelectedSegments
	if segs == nil {
		segs = []domain.SegmentRange{}
	}
	canonical := struct {
		Dest      string                `json:"dest"`
		Mode      string                `json:"mode"`
		SourceID  string                `json:"source_id"`
		AssetHash string                `json:"asset_hash"`
		Companion bool                  `json:"companion"`
		Caption   string                `json:"caption"`
		Comment   string                `json:"comment"`
		Segments  []domain.SegmentRange `json:"segments"`
	}{
		Dest:      strings.ToLower(strings.TrimSpace(pub.DestinationAccount)),
		Mode:      pub.PublicationMode,
		SourceID:  pub.SourceID,
		AssetHash: pub.AssetHash,
		Companion: pub.IncludeCompanionTags,
		Caption:   pub.Caption,
		Comment:   pub.FirstComment,
		Segments:  segs,
	}
	b, _ := json.Marshal(canonical)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func collectOrderedMoveNames(occs []domain.PublicOccurrenceSnapshot, explicit []string) []string {
	if len(explicit) > 0 {
		var out []string
		seen := make(map[string]bool)
		for _, m := range explicit {
			trimmed := strings.TrimSpace(m)
			nk := domain.NormalizeSearchKey(trimmed)
			if nk != "" && !seen[nk] {
				seen[nk] = true
				out = append(out, trimmed)
			}
		}
		return out
	}
	var out []string
	seen := make(map[string]bool)
	for _, o := range occs {
		nk := domain.NormalizeSearchKey(o.MoveName)
		if nk != "" && !seen[nk] {
			seen[nk] = true
			out = append(out, o.MoveName)
		}
	}
	return out
}

func platformDisplayName(p domain.Platform) string {
	switch p {
	case domain.PlatformYouTube:
		return "YouTube"
	case domain.PlatformTikTok:
		return "TikTok"
	case domain.PlatformFacebook:
		return "Facebook"
	case domain.PlatformInstagram:
		return "Instagram"
	default:
		return string(p)
	}
}

func generateStagingToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// --- Official Instagram Content Publishing Graph Transport [S11] ---

type GraphTransport struct {
	baseURL     string
	igUserID    string
	accessToken string
	httpClient  *http.Client
}

func NewGraphTransport(cfg config.Config) *GraphTransport {
	return &GraphTransport{
		baseURL:     cfg.IGAPIBaseURL,
		igUserID:    cfg.IGUserID,
		accessToken: cfg.IGAccessToken,
		httpClient:  &http.Client{Timeout: 25 * time.Second},
	}
}

func (g *GraphTransport) CreateReelContainer(ctx context.Context, videoStagingURL, caption string) (string, error) {
	if g.accessToken == "" || g.igUserID == "" {
		return "", ErrTokenExpired
	}
	vals := url.Values{}
	vals.Set("media_type", "REELS")
	vals.Set("video_url", videoStagingURL)
	vals.Set("caption", caption)

	endpoint := fmt.Sprintf("%s/%s/media", g.baseURL, url.PathEscape(g.igUserID))
	body, status, err := g.doFormPost(ctx, endpoint, vals)
	if err != nil {
		return "", err
	}
	if status == 401 || status == 403 || isOAuthTokenError(body) {
		return "", ErrTokenExpired
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("instagram media create HTTP %d", status)
	}
	var parsed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.ID == "" {
		return "", errors.New("instagram media create returned empty container id")
	}
	return parsed.ID, nil
}

func (g *GraphTransport) GetContainerStatus(ctx context.Context, containerID string) (ContainerStatus, error) {
	if g.accessToken == "" {
		return ContainerStatus{}, ErrTokenExpired
	}
	q := url.Values{}
	q.Set("fields", "id,status_code,status")
	endpoint := fmt.Sprintf("%s/%s?%s", g.baseURL, url.PathEscape(containerID), q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ContainerStatus{}, err
	}
	req.Header.Set("Authorization", "Bearer "+g.accessToken)
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return ContainerStatus{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode == 401 || resp.StatusCode == 403 || isOAuthTokenError(body) {
		return ContainerStatus{}, ErrTokenExpired
	}
	if resp.StatusCode != 200 {
		return ContainerStatus{}, fmt.Errorf("instagram container status HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		ID         string `json:"id"`
		StatusCode string `json:"status_code"`
		Status     string `json:"status"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ContainerStatus{}, err
	}
	return ContainerStatus{
		ContainerID:   parsed.ID,
		StatusCode:    parsed.StatusCode,
		StatusMessage: parsed.Status,
	}, nil
}

func (g *GraphTransport) PublishContainer(ctx context.Context, containerID string) (string, string, error) {
	if g.accessToken == "" || g.igUserID == "" {
		return "", "", ErrTokenExpired
	}
	vals := url.Values{}
	vals.Set("creation_id", containerID)
	endpoint := fmt.Sprintf("%s/%s/media_publish", g.baseURL, url.PathEscape(g.igUserID))
	body, status, err := g.doFormPost(ctx, endpoint, vals)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrAmbiguousOutcome, err)
	}
	if status == 401 || status == 403 || isOAuthTokenError(body) {
		return "", "", ErrTokenExpired
	}
	if status >= 500 || status == 408 || status == 504 {
		return "", "", fmt.Errorf("%w: upstream HTTP %d", ErrAmbiguousOutcome, status)
	}
	if status < 200 || status >= 300 {
		return "", "", fmt.Errorf("instagram media_publish HTTP %d", status)
	}
	var parsed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.ID == "" {
		return "", "", ErrAmbiguousOutcome
	}

	// Best-effort fetch permalink
	permalink := g.fetchPermalink(ctx, parsed.ID)
	return parsed.ID, permalink, nil
}

func (g *GraphTransport) PostFirstComment(ctx context.Context, mediaID, commentText string) (string, error) {
	if g.accessToken == "" {
		return "", ErrTokenExpired
	}
	vals := url.Values{}
	vals.Set("message", commentText)
	endpoint := fmt.Sprintf("%s/%s/comments", g.baseURL, url.PathEscape(mediaID))
	body, status, err := g.doFormPost(ctx, endpoint, vals)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("instagram first comment HTTP %d", status)
	}
	var parsed struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &parsed)
	return parsed.ID, nil
}

func (g *GraphTransport) fetchPermalink(ctx context.Context, mediaID string) string {
	endpoint := fmt.Sprintf("%s/%s?fields=permalink", g.baseURL, url.PathEscape(mediaID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+g.accessToken)
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	var parsed struct {
		Permalink string `json:"permalink"`
	}
	_ = json.Unmarshal(body, &parsed)
	return parsed.Permalink
}

func (g *GraphTransport) doFormPost(ctx context.Context, endpoint string, form url.Values) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+g.accessToken)
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, rErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	return body, resp.StatusCode, rErr
}

func isOAuthTokenError(body []byte) bool {
	s := strings.ToLower(string(body))
	return strings.Contains(s, "oauthexception") || strings.Contains(s, "invalid oauth access token") || strings.Contains(s, "\"code\":190")
}

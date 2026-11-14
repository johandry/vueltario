package domain

import (
	"errors"
	"time"
)

var (
	ErrNotFound        = errors.New("not found")
	ErrUnauthorized    = errors.New("unauthorized")
	ErrForbidden       = errors.New("forbidden")
	ErrConflict        = errors.New("conflict")
	ErrValidation      = errors.New("validation failed")
	ErrUnsupportedHost = errors.New("unsupported source host")
	ErrRateLimited     = errors.New("rate limit exceeded")
	ErrQuotaExceeded   = errors.New("quota or budget exceeded")
)

type Platform string

const (
	PlatformYouTube   Platform = "youtube"
	PlatformTikTok    Platform = "tiktok"
	PlatformFacebook  Platform = "facebook"
	PlatformInstagram Platform = "instagram"
)

type Role string

const (
	RoleAdmin              Role = "admin"
	RoleTrustedContributor Role = "trusted_contributor"
	RoleMember             Role = "member"
	RoleVisitor            Role = "visitor"
)

type VisibilityState string

const (
	VisibilityPrivate   VisibilityState = "private"
	VisibilitySubmitted VisibilityState = "submitted"
	VisibilityPublished VisibilityState = "published"
	VisibilityRejected  VisibilityState = "rejected"
	VisibilityWithdrawn VisibilityState = "withdrawn"
	VisibilityRemoved   VisibilityState = "removed"
)

type AvailabilityState string

const (
	AvailabilityUnknown       AvailabilityState = "unknown"
	AvailabilityAvailable     AvailabilityState = "available"
	AvailabilityRestricted    AvailabilityState = "restricted"
	AvailabilityPrivate       AvailabilityState = "private"
	AvailabilityDeleted       AvailabilityState = "deleted"
	AvailabilityLoginRequired AvailabilityState = "login_required"
)

type OccurrenceState string

const (
	OccurrenceConfirmed OccurrenceState = "confirmed"
	OccurrenceSuggested OccurrenceState = "suggested"
	OccurrenceRejected  OccurrenceState = "rejected"
	OccurrenceCurated   OccurrenceState = "curated"
)

type TimingMethod string

const (
	TimingPlayerMark TimingMethod = "player_mark"
	TimingManual     TimingMethod = "manual"
	TimingSuggested  TimingMethod = "suggested"
	TimingUnknown    TimingMethod = "unknown"
)

type JobState string

const (
	JobQueued      JobState = "queued"
	JobRunning     JobState = "running"
	JobSucceeded   JobState = "succeeded"
	JobRetryWait   JobState = "retry_wait"
	JobBlockedAuth JobState = "blocked_auth"
	JobNeedsInput  JobState = "needs_input"
	JobUnsupported JobState = "unsupported"
	JobFailed      JobState = "failed"
	JobCancelled   JobState = "cancelled"
)

type PublicationStatus string

const (
	PubDraft          PublicationStatus = "draft"
	PubNeedsRights    PublicationStatus = "needs_rights"
	PubNeedsAsset     PublicationStatus = "needs_asset"
	PubReadyForReview PublicationStatus = "ready_for_review"
	PubApproved       PublicationStatus = "approved"
	PubManualHandoff  PublicationStatus = "manual_handoff"
	PubStaging        PublicationStatus = "staging"
	PubUploading      PublicationStatus = "uploading"
	PubProcessing     PublicationStatus = "processing"
	PubPublishing     PublicationStatus = "publishing"
	PubPublished      PublicationStatus = "published"
	PubRetryWait      PublicationStatus = "retry_wait"
	PubBlockedAuth    PublicationStatus = "blocked_auth"
	PubFailed         PublicationStatus = "failed"
	PubCancelled      PublicationStatus = "cancelled"
	PubOutcomeUnknown PublicationStatus = "outcome_unknown"
)

type User struct {
	ID              string     `json:"id"`
	Email           string     `json:"email"`
	ExternalSubject *string    `json:"external_subject,omitempty"`
	PasswordHash    string     `json:"-"`
	DisplayName     string     `json:"display_name"`
	Role            Role       `json:"role"`
	Locale          string     `json:"locale"`
	AnalysisOptOut  bool       `json:"analysis_opt_out"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	DeletedAt       *time.Time `json:"deleted_at,omitempty"`
}

type Invite struct {
	ID         string     `json:"id"`
	Email      string     `json:"email"`
	Role       Role       `json:"role"`
	InvitedBy  string     `json:"invited_by"`
	AcceptedBy *string    `json:"accepted_by,omitempty"`
	ExpiresAt  time.Time  `json:"expires_at"`
	AcceptedAt *time.Time `json:"accepted_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	PlainCode  string     `json:"code,omitempty"`
}

type CreatorIdentity struct {
	ID                 string     `json:"id"`
	CreatorID          string     `json:"creator_id"`
	Platform           Platform   `json:"platform"`
	PlatformUserID     *string    `json:"platform_user_id,omitempty"`
	Handle             *string    `json:"handle,omitempty"`
	ProfileURL         string     `json:"profile_url"`
	Verified           bool       `json:"verified"`
	VerificationSource string     `json:"verification_source,omitempty"`
	VerifiedAt         *time.Time `json:"verified_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
}

type Creator struct {
	ID             string            `json:"id"`
	DisplayName    string            `json:"display_name"`
	NormalizedName string            `json:"-"`
	Biography      string            `json:"biography,omitempty"`
	Notes          string            `json:"notes,omitempty"`
	Identities     []CreatorIdentity `json:"identities,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

type SourceCreator struct {
	ID         string  `json:"id"`
	SourceID   string  `json:"source_id"`
	CreatorID  string  `json:"creator_id"`
	Role       string  `json:"role"` // original, uploader, collaborator
	Provenance string  `json:"provenance"`
	Creator    Creator `json:"creator"`
}

type Source struct {
	ID                string            `json:"id"`
	Platform          Platform          `json:"platform"`
	PlatformContentID *string           `json:"platform_content_id"`
	CanonicalURL      string            `json:"canonical_url"`
	OriginalURL       string            `json:"original_url"`
	Title             *string           `json:"title"`
	Description       *string           `json:"description"`
	DurationMs        *int64            `json:"duration_ms"`
	AvailabilityState AvailabilityState `json:"availability_state"`
	EmbedEligible     bool              `json:"embed_eligible"`
	Provenance        string            `json:"provenance"`
	MetadataVersion   int               `json:"metadata_version"`
	FetchedAt         *time.Time        `json:"fetched_at"`
	Creators          []SourceCreator   `json:"creators,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

type MoveAlias struct {
	ID               string    `json:"id"`
	MoveID           string    `json:"move_id"`
	Alias            string    `json:"alias"`
	NormalizedAlias  string    `json:"normalized_alias"`
	Language         string    `json:"language"`
	Qualifier        string    `json:"qualifier,omitempty"`
	Provenance       string    `json:"provenance"`
	CollisionFlagged bool      `json:"collision_flagged"`
	CreatedAt        time.Time `json:"created_at"`
}

type CombinationStep struct {
	ID                string `json:"id"`
	CombinationMoveID string `json:"combination_move_id"`
	Ordinal           int    `json:"ordinal"`
	ChildMoveID       string `json:"child_move_id"`
	ChildMoveName     string `json:"child_move_name,omitempty"`
	Notes             string `json:"notes,omitempty"`
}

type Move struct {
	ID                  string            `json:"id"`
	CanonicalName       string            `json:"canonical_name"`
	NormalizedSearchKey string            `json:"normalized_search_key"`
	PortableToken       string            `json:"portable_token"`
	Disambiguator       string            `json:"disambiguator,omitempty"`
	StyleTags           []string          `json:"style_tags"`
	Kind                string            `json:"kind"`           // move, combination
	CurationState       string            `json:"curation_state"` // proposed, curated, rejected
	Description         string            `json:"description,omitempty"`
	Aliases             []MoveAlias       `json:"aliases,omitempty"`
	Steps               []CombinationStep `json:"steps,omitempty"`
	CreatedBy           *string           `json:"created_by,omitempty"`
	CreatedAt           time.Time         `json:"created_at"`
	UpdatedAt           time.Time         `json:"updated_at"`
}

type Occurrence struct {
	ID            string          `json:"id"`
	EntryID       string          `json:"entry_id"`
	MoveID        *string         `json:"move_id"`
	MoveName      string          `json:"move_name,omitempty"`
	ProposedName  *string         `json:"proposed_name"`
	StartMs       *int64          `json:"start_ms"`
	EndMs         *int64          `json:"end_ms"`
	Ordinal       int             `json:"ordinal"`
	Notes         string          `json:"notes,omitempty"`
	State         OccurrenceState `json:"state"`
	TimingMethod  TimingMethod    `json:"timing_method"`
	EvidenceIDs   []string        `json:"evidence_ids"`
	Revision      int             `json:"revision"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

type Evidence struct {
	ID                string     `json:"id"`
	SourceID          string     `json:"source_id"`
	OwnerUserID       *string    `json:"owner_user_id,omitempty"`
	Kind              string     `json:"kind"` // title, description, transcript, ocr, comment, manual
	OriginalRef       string     `json:"original_ref,omitempty"`
	Excerpt           string     `json:"excerpt"`
	ObservedStartMs   *int64     `json:"observed_start_ms"`
	ObservedEndMs     *int64     `json:"observed_end_ms"`
	IsAnnouncementTS  bool       `json:"is_announcement_ts"`
	AuthorRole        string     `json:"author_role"`
	AccessScope       string     `json:"access_scope"` // public_factual, private_user, admin
	RetrievedAt       time.Time  `json:"retrieved_at"`
	RetentionDeadline *time.Time `json:"retention_deadline,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
}

type Suggestion struct {
	ID                 string    `json:"id"`
	EntryID            string    `json:"entry_id"`
	SourceRevision     int       `json:"source_revision"`
	CandidateMoveID    *string   `json:"candidate_move_id"`
	CandidateName      string    `json:"candidate_name"`
	StartMs            *int64    `json:"start_ms"`
	EndMs              *int64    `json:"end_ms"`
	IsAnnouncementOnly bool      `json:"is_announcement_only"`
	EvidenceIDs        []string  `json:"evidence_ids"`
	ProcessorVersion   string    `json:"processor_version"`
	ConfidenceBand     string    `json:"confidence_band"` // high, medium, low, unverified
	State              string    `json:"state"`           // pending, accepted, rejected
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type CatalogEntry struct {
	ID                      string          `json:"id"`
	UserID                  string          `json:"user_id"`
	SourceID                string          `json:"source_id"`
	VisibilityState         VisibilityState `json:"visibility_state"`
	PrivateNotes            string          `json:"private_notes"`
	PersonalTitleOverride   *string         `json:"personal_title_override"`
	PersonalCreatorOverride *string         `json:"personal_creator_override"`
	ObservedOffsetMs        *int64          `json:"observed_offset_ms"`
	ClientRequestID         *string         `json:"client_request_id,omitempty"`
	Revision                int             `json:"revision"`
	Source                  Source          `json:"source"`
	Occurrences             []Occurrence    `json:"occurrences"`
	Suggestions             []Suggestion    `json:"suggestions,omitempty"`
	Evidence                []Evidence      `json:"evidence,omitempty"`
	Assets                  []MediaAsset    `json:"assets,omitempty"`
	LatestJob               *ProcessingJob  `json:"latest_job,omitempty"`
	CreatedAt               time.Time       `json:"created_at"`
	UpdatedAt               time.Time       `json:"updated_at"`
	DeletedAt               *time.Time      `json:"deleted_at,omitempty"`
}

type SourceTimeMap struct {
	SourceStartMs int64 `json:"source_start_ms"`
	SourceEndMs   int64 `json:"source_end_ms"`
	ClipOffsetMs  int64 `json:"clip_offset_ms"`
}

type MediaAsset struct {
	ID                 string         `json:"id"`
	SourceID           string         `json:"source_id"`
	OwnerUserID        string         `json:"owner_user_id"`
	AccessScope        string         `json:"access_scope"` // private, public_authorized, staging
	AssetKind          string         `json:"asset_kind"`   // original, clip_derivative, staging
	StorageBackend     string         `json:"storage_backend"`
	StorageKey         string         `json:"-"` // Never expose raw storage key publicly
	DeviceID           *string        `json:"device_id,omitempty"`
	MIMEType           string         `json:"mime_type"`
	ByteSize           int64          `json:"byte_size"`
	SHA256Hex          string         `json:"sha256_hex"`
	DurationMs         *int64         `json:"duration_ms"`
	Width              *int           `json:"width"`
	Height             *int           `json:"height"`
	Status             string         `json:"status"` // quarantine, ready, quarantined_orphan, revoked, deleted
	DerivationParentID *string        `json:"derivation_parent_id,omitempty"`
	SourceTimeMap      *SourceTimeMap `json:"source_time_map,omitempty"`
	StreamURL          string         `json:"stream_url,omitempty"`
	ExpiresAt          *time.Time     `json:"expires_at,omitempty"`
	DeletedAt          *time.Time     `json:"deleted_at,omitempty"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
}

type RightsRecord struct {
	ID                    string     `json:"id"`
	SourceID              string     `json:"source_id"`
	AssetID               *string    `json:"asset_id,omitempty"`
	Grantor               string     `json:"grantor"`
	Basis                 string     `json:"basis"`
	EvidenceRef           string     `json:"evidence_ref,omitempty"`
	VideoCleared          bool       `json:"video_cleared"`
	MusicCleared          bool       `json:"music_cleared"`
	AuthorizedUses        []string   `json:"authorized_uses"`
	AuthorizedPlatforms   []string   `json:"authorized_platforms"`
	AttributionConditions string     `json:"attribution_conditions,omitempty"`
	ExpiresAt             *time.Time `json:"expires_at,omitempty"`
	RevokedAt             *time.Time `json:"revoked_at,omitempty"`
	RevocationReason      string     `json:"revocation_reason,omitempty"`
	ReviewerID            string     `json:"reviewer_id"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
}

// PublicOccurrenceSnapshot is an immutable occurrence inside a public submission/revision.
type PublicOccurrenceSnapshot struct {
	MoveID       *string `json:"move_id,omitempty"`
	MoveName     string  `json:"move_name"`
	StartMs      *int64  `json:"start_ms"`
	EndMs        *int64  `json:"end_ms"`
	Ordinal      int     `json:"ordinal"`
	PublicNotes  string  `json:"public_notes,omitempty"`
	TimingMethod string  `json:"timing_method"`
}

// PublicPayload is the strictly sanitized public projection (never includes private notes, user IDs, or private assets).
type PublicPayload struct {
	SourceID          string                     `json:"source_id"`
	Platform          Platform                   `json:"platform"`
	CanonicalURL      string                     `json:"canonical_url"`
	Title             string                     `json:"title"`
	CreatorName       string                     `json:"creator_name"`
	CreatorProfileURL string                     `json:"creator_profile_url,omitempty"`
	CreatorRole       string                     `json:"creator_role,omitempty"`
	CreatorVerified   bool                       `json:"creator_verified"`
	CreatorHandle     string                     `json:"creator_handle,omitempty"`
	DurationMs        *int64                     `json:"duration_ms"`
	Occurrences       []PublicOccurrenceSnapshot `json:"occurrences"`
}

type PublicSubmission struct {
	ID             string        `json:"id"`
	EntryID        string        `json:"entry_id"`
	SourceID       string        `json:"source_id"`
	SubmitterID    string        `json:"submitter_id"`
	Snapshot       PublicPayload `json:"snapshot"`
	Status         string        `json:"status"` // pending, approved, changes_requested, rejected, withdrawn
	ReviewerID     *string       `json:"reviewer_id,omitempty"`
	DecisionReason string        `json:"decision_reason,omitempty"`
	Version        int           `json:"version"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
}

type PublicRevision struct {
	ID             string        `json:"id"`
	SourceID       string        `json:"source_id"`
	SubmissionID   *string       `json:"submission_id,omitempty"`
	RevisionNumber int           `json:"revision_number"`
	Payload        PublicPayload `json:"payload"`
	Status         string        `json:"status"` // active, superseded, hidden, removed
	HiddenReason   string        `json:"hidden_reason,omitempty"`
	ApprovedBy     string        `json:"approved_by,omitempty"`
	ApprovedAt     time.Time     `json:"approved_at"`
	CreatedAt      time.Time     `json:"created_at"`
}

type PublicCorrection struct {
	ID               string        `json:"id"`
	PublicRevisionID string        `json:"public_revision_id"`
	SourceID         string        `json:"source_id"`
	ProposerID       string        `json:"proposer_id"`
	ProposedChanges  PublicPayload `json:"proposed_changes"`
	Reason           string        `json:"reason"`
	Status           string        `json:"status"` // pending, approved, rejected
	ReviewerID       *string       `json:"reviewer_id,omitempty"`
	ReviewNote       string        `json:"review_note,omitempty"`
	CreatedAt        time.Time     `json:"created_at"`
	UpdatedAt        time.Time     `json:"updated_at"`
}

type Report struct {
	ID               string    `json:"id"`
	PublicRevisionID *string   `json:"public_revision_id,omitempty"`
	SourceID         string    `json:"source_id"`
	ReporterID       *string   `json:"reporter_id,omitempty"`
	ReporterContact  string    `json:"reporter_contact,omitempty"`
	Category         string    `json:"category"` // incorrect_naming, misattribution, unavailable_source, rights_concern
	Details          string    `json:"details"`
	Status           string    `json:"status"` // open, resolved, dismissed
	ResolvedBy       *string   `json:"resolved_by,omitempty"`
	ResolutionNote   string    `json:"resolution_note,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type ProcessingJob struct {
	ID             string     `json:"id"`
	OwnerUserID    *string    `json:"owner_user_id,omitempty"`
	JobType        string     `json:"job_type"`
	TargetType     string     `json:"target_type"`
	TargetID       string     `json:"target_id"`
	TargetRevision int        `json:"target_revision"`
	PayloadJSON    string     `json:"-"`
	State          JobState   `json:"state"`
	Attempts       int        `json:"attempts"`
	MaxAttempts    int        `json:"max_attempts"`
	LeaseOwner     *string    `json:"lease_owner,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
	NextAttemptAt  time.Time  `json:"next_attempt_at"`
	SanitizedError string     `json:"error,omitempty"`
	IdempotencyKey string     `json:"idempotency_key"`
	CorrelationID  string     `json:"correlation_id,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type MoveTag struct {
	CanonicalName   string `json:"canonical_name"`
	PortableToken   string `json:"portable_token"`
	DisplayLabel    string `json:"display_label"`              // e.g. #CubanSocial-DileQueNo
	PlatformHashtag string `json:"platform_hashtag,omitempty"` // e.g. #CubanSocial_DileQueNo (explicit opt-in only)
}

type SegmentRange struct {
	StartMs *int64 `json:"start_ms"`
	EndMs   *int64 `json:"end_ms"`
	Label   string `json:"label,omitempty"`
}

type PublicationJob struct {
	ID                    string            `json:"id"`
	DestinationAccount    string            `json:"destination_account"`
	BrandPrefix           string            `json:"brand_prefix"`
	OriginPlatform        Platform          `json:"origin_platform"`
	PublicationMode       string            `json:"publication_mode"` // external_upload, native_repost_handoff
	SourceID              string            `json:"source_id"`
	PublicRevisionID      *string           `json:"public_revision_id,omitempty"`
	PrivateEntryID        *string           `json:"private_entry_id,omitempty"`
	PrivateEntryRevision  *int              `json:"private_entry_revision,omitempty"`
	AssetID               *string           `json:"asset_id,omitempty"`
	SelectedSegments      []SegmentRange    `json:"selected_segments"`
	MoveTags              []MoveTag         `json:"move_tags"`
	IncludeCompanionTags  bool              `json:"include_companion_tags"`
	Caption               string            `json:"caption"`
	FirstComment          string            `json:"first_comment,omitempty"`
	FirstCommentStatus    string            `json:"first_comment_status"`
	FirstCommentError     string            `json:"first_comment_error,omitempty"`
	DraftHash             string            `json:"draft_hash"`
	AssetHash             string            `json:"asset_hash"`
	ApprovedBy            *string           `json:"approved_by,omitempty"`
	ApprovedAt            *time.Time        `json:"approved_at,omitempty"`
	ApprovedDraftHash     string            `json:"approved_draft_hash,omitempty"`
	ApprovedAssetHash     string            `json:"approved_asset_hash,omitempty"`
	RightsSnapshot        *RightsRecord     `json:"rights_snapshot,omitempty"`
	StagingToken          string            `json:"-"`
	StagingExpiresAt      *time.Time        `json:"staging_expires_at,omitempty"`
	RemoteContainerID     string            `json:"remote_container_id,omitempty"`
	RemoteMediaID         string            `json:"remote_media_id,omitempty"`
	RemoteCommentID       string            `json:"remote_comment_id,omitempty"`
	ResultURL             string            `json:"result_url,omitempty"`
	ExecuteIdempotencyKey string            `json:"execute_idempotency_key,omitempty"`
	Status                PublicationStatus `json:"status"`
	StatusDetail          string            `json:"status_detail,omitempty"`
	NativeRepostGuide     string            `json:"native_repost_guide,omitempty"`
	CreatedBy             string            `json:"created_by"`
	CreatedAt             time.Time         `json:"created_at"`
	UpdatedAt             time.Time         `json:"updated_at"`
}

type AuditEvent struct {
	ID             string    `json:"id"`
	ActorID        *string   `json:"actor_id,omitempty"`
	ActorRole      string    `json:"actor_role"`
	Action         string    `json:"action"`
	TargetType     string    `json:"target_type"`
	TargetID       string    `json:"target_id"`
	TargetRevision int       `json:"target_revision"`
	Reason         string    `json:"reason,omitempty"`
	MetadataJSON   string    `json:"metadata_json,omitempty"`
	CorrelationID  string    `json:"correlation_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type SyncEvent struct {
	Seq         int64     `json:"seq"`
	EntityType  string    `json:"entity_type"`
	EntityID    string    `json:"entity_id"`
	Operation   string    `json:"operation"`
	Revision    int       `json:"revision"`
	PayloadJSON string    `json:"payload_json"`
	CreatedAt   time.Time `json:"created_at"`
}

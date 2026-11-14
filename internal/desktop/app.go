package desktop

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/johandry/vueltario/internal/analysis"
	"github.com/johandry/vueltario/internal/config"
	"github.com/johandry/vueltario/internal/db"
	"github.com/johandry/vueltario/internal/domain"
	"github.com/johandry/vueltario/internal/media"
	"github.com/johandry/vueltario/internal/netguard"
	"github.com/johandry/vueltario/internal/providers"
	"github.com/johandry/vueltario/internal/publishing"
)

// App is the Wails-bound Go desktop service (DESK-01, Phase 5).
// It invokes the exact same Go domain, store, media, and publishing services as the CLI—zero duplicated business or publishing rules.
type App struct {
	ctx       context.Context
	cfg       config.Config
	store     *db.Store
	providers *providers.Registry
	mediaSvc  *media.Service
	analyzer  *analysis.Service
	pubSvc    *publishing.Service
	secrets   SecretStore
	transport publishing.Transport

	currentUser *domain.User
}

func NewApp(cfg config.Config, secrets SecretStore, transport publishing.Transport) *App {
	if secrets == nil {
		secrets = NewOSKeyringStore()
	}
	return &App{
		cfg:       cfg,
		secrets:   secrets,
		transport: transport,
	}
}

// Startup is called by the Wails runtime (or directly in tests) to open the shared services and restore OS Keychain credentials.
func (a *App) Startup(ctx context.Context) error {
	a.ctx = ctx
	store, err := db.Open(a.cfg.DBDriver, a.cfg.DatabaseURL)
	if err != nil {
		return err
	}
	if err := store.MigrateUp(ctx); err != nil {
		_ = store.Close()
		return err
	}
	if _, err := store.EnsureBootstrap(ctx, a.cfg.AdminEmail, a.cfg.AdminPassword); err != nil {
		_ = store.Close()
		return err
	}

	safeHTTP := netguard.NewSafeClient(netguard.Options{})
	reg := providers.NewDefaultRegistry(safeHTTP, a.cfg.YouTubeAPIKey)
	mediaSvc, err := media.NewService(a.cfg, store)
	if err != nil {
		_ = store.Close()
		return err
	}
	a.store = store
	a.providers = reg
	a.mediaSvc = mediaSvc
	a.analyzer = analysis.NewService(a.cfg, store, reg, nil, nil)
	a.pubSvc = publishing.NewService(a.cfg, store, a.transport)

	// Restore active session from OS Keychain if present (Phase 5 acceptance: updates preserve credentials securely).
	if saved, err := a.secrets.LoadSession("active"); err == nil && saved != nil && saved.AccessToken != "" {
		if sess, sErr := a.store.GetSessionByToken(ctx, saved.AccessToken); sErr == nil {
			a.currentUser = &sess.User
		}
	}
	return nil
}

func (a *App) Shutdown(ctx context.Context) {
	if a.store != nil {
		_ = a.store.Close()
	}
}

func (a *App) opCtx() context.Context {
	if a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}

// --- Desktop Authentication (PKCE S256 + OS Keychain Storage) ---

type AuthState struct {
	Authenticated  bool         `json:"authenticated"`
	User           *domain.User `json:"user,omitempty"`
	IGDestination  string       `json:"ig_destination"`
	IGBrandPrefix  string       `json:"ig_brand_prefix"`
	AllowCompanion bool         `json:"allow_companion_tags"`
}

func (a *App) GetAuthState() AuthState {
	return AuthState{
		Authenticated:  a.currentUser != nil,
		User:           a.currentUser,
		IGDestination:  a.cfg.IGDestination,
		IGBrandPrefix:  a.cfg.IGBrandPrefix,
		AllowCompanion: a.cfg.IGAllowCompanionTags,
	}
}

// LoginPKCE executes the RFC 7636 S256 PKCE flow against the Go store and persists the resulting token in the OS Keychain.
func (a *App) LoginPKCE(email, password string) (AuthState, error) {
	ctx := a.opCtx()
	verifier, challenge, err := generatePKCEPair()
	if err != nil {
		return AuthState{}, err
	}
	code, err := a.store.CreatePKCEAuthorizationCode(ctx, db.PKCEAuthorizeInput{
		Email:               email,
		Password:            password,
		ClientID:            "vueltario-wails-desktop",
		RedirectURI:         "vueltario-desktop://auth/callback",
		CodeChallenge:       challenge,
		CodeChallengeMethod: "S256",
	})
	if err != nil {
		return AuthState{}, err
	}
	sess, err := a.store.ExchangePKCECode(ctx, db.PKCETokenExchangeInput{
		Code:         code,
		ClientID:     "vueltario-wails-desktop",
		RedirectURI:  "vueltario-desktop://auth/callback",
		CodeVerifier: verifier,
	}, 30*24*time.Hour)
	if err != nil {
		return AuthState{}, err
	}
	a.currentUser = &sess.User
	_ = a.secrets.SaveSession(StoredSession{
		ServerURL:   a.cfg.BaseURL,
		AccountID:   sess.User.ID,
		Email:       "active",
		AccessToken: sess.PlainToken,
	})
	return a.GetAuthState(), nil
}

func (a *App) Logout() error {
	if saved, _ := a.secrets.LoadSession("active"); saved != nil && saved.AccessToken != "" {
		_ = a.store.DeleteSessionByToken(a.opCtx(), saved.AccessToken)
	}
	_ = a.secrets.DeleteSession("active")
	a.currentUser = nil
	return nil
}

func (a *App) requireAdmin() (domain.User, error) {
	if a.currentUser == nil {
		return domain.User{}, domain.ErrUnauthorized
	}
	if a.currentUser.Role != domain.RoleAdmin {
		return domain.User{}, domain.ErrForbidden
	}
	return *a.currentUser, nil
}

// --- Catalog, Batch Ingest & Media/Rights Workflows ---

type BatchIngestResult struct {
	Total      int               `json:"total"`
	Created    int               `json:"created"`
	Duplicates int               `json:"duplicates"`
	Failed     int               `json:"failed"`
	Items      []BatchIngestItem `json:"items"`
}

type BatchIngestItem struct {
	URL      string `json:"url"`
	Status   string `json:"status"`
	EntryID  string `json:"entry_id,omitempty"`
	SourceID string `json:"source_id,omitempty"`
	Platform string `json:"platform,omitempty"`
	Error    string `json:"error,omitempty"`
}

// BatchIngestURLs saves multiple links while isolating per-item failures (ING-05 & Phase 5 acceptance).
func (a *App) BatchIngestURLs(urls []string, notes string) (*BatchIngestResult, error) {
	admin, err := a.requireAdmin()
	if err != nil {
		return nil, err
	}
	out := &BatchIngestResult{}
	for _, raw := range urls {
		u := strings.TrimSpace(raw)
		if u == "" {
			continue
		}
		out.Total++
		res, sErr := a.store.SaveLink(a.opCtx(), db.SaveLinkRequest{
			UserID:       admin.ID,
			RawURL:       u,
			PrivateNotes: notes,
		})
		if sErr != nil {
			out.Failed++
			out.Items = append(out.Items, BatchIngestItem{URL: u, Status: "failed", Error: sErr.Error()})
			continue
		}
		st := "created"
		if res.Deduplicated {
			st = "duplicate"
			out.Duplicates++
		} else {
			out.Created++
		}
		out.Items = append(out.Items, BatchIngestItem{
			URL:      u,
			Status:   st,
			EntryID:  res.Entry.ID,
			SourceID: res.Source.ID,
			Platform: string(res.Source.Platform),
		})
	}
	return out, nil
}

func (a *App) ListCatalogEntries(query, platform string) ([]domain.CatalogEntry, error) {
	admin, err := a.requireAdmin()
	if err != nil {
		return nil, err
	}
	return a.store.ListEntries(a.opCtx(), db.EntryFilter{
		UserID:   admin.ID,
		Query:    query,
		Platform: platform,
		Limit:    100,
	})
}

func (a *App) AddOccurrence(entryID, moveName, startText, endText, notes string) (*domain.Occurrence, error) {
	admin, err := a.requireAdmin()
	if err != nil {
		return nil, err
	}
	sMs, err := domain.ParseTimestampMs(startText)
	if err != nil {
		return nil, err
	}
	eMs, err := domain.ParseTimestampMs(endText)
	if err != nil {
		return nil, err
	}
	return a.store.AddOccurrence(a.opCtx(), db.OccurrenceInput{
		UserID:       admin.ID,
		EntryID:      entryID,
		ProposedName: &moveName,
		StartMs:      sMs,
		EndMs:        eMs,
		Notes:        notes,
	})
}

func (a *App) ImportAuthorizedMediaFile(sourceID, filePath string) (*domain.MediaAsset, error) {
	admin, err := a.requireAdmin()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return a.mediaSvc.ImportAuthorizedFile(a.opCtx(), media.ImportRequest{
		User:     admin,
		SourceID: sourceID,
		Filename: filePath,
		Reader:   f,
	})
}

func (a *App) RecordRights(sourceID, assetID, grantor, basis string, videoCleared, musicCleared bool) (*domain.RightsRecord, error) {
	admin, err := a.requireAdmin()
	if err != nil {
		return nil, err
	}
	var aPtr *string
	if strings.TrimSpace(assetID) != "" {
		trimmed := strings.TrimSpace(assetID)
		aPtr = &trimmed
	}
	return a.store.CreateRightsRecord(a.opCtx(), domain.RightsRecord{
		SourceID:            sourceID,
		AssetID:             aPtr,
		Grantor:             grantor,
		Basis:               basis,
		VideoCleared:        videoCleared,
		MusicCleared:        musicCleared,
		AuthorizedUses:      []string{"republish"},
		AuthorizedPlatforms: []string{"instagram"},
		ReviewerID:          admin.ID,
	})
}

func (a *App) ListRights(sourceID string) ([]domain.RightsRecord, error) {
	if _, err := a.requireAdmin(); err != nil {
		return nil, err
	}
	return a.store.ListRightsForSource(a.opCtx(), sourceID)
}

// --- Shared Publishing Core (DESK-01: Identical publishing.Service calls as CLI!) ---

type DesktopDraftInput struct {
	PrivateEntryID       string `json:"private_entry_id"`
	PublicRevisionID     string `json:"public_revision_id"`
	AssetID              string `json:"asset_id"`
	DestinationAccount   string `json:"destination_account"`
	BrandPrefix          string `json:"brand_prefix"`
	IncludeCompanionTags bool   `json:"include_companion_tags"`
	TagsInFirstComment   bool   `json:"tags_in_first_comment"`
	FirstComment         string `json:"first_comment"`
}

func (a *App) CreatePublicationDraft(in DesktopDraftInput) (*domain.PublicationJob, error) {
	admin, err := a.requireAdmin()
	if err != nil {
		return nil, err
	}
	var ePtr, pPtr, aPtr, cPtr *string
	if strings.TrimSpace(in.PrivateEntryID) != "" {
		v := strings.TrimSpace(in.PrivateEntryID)
		ePtr = &v
	}
	if strings.TrimSpace(in.PublicRevisionID) != "" {
		v := strings.TrimSpace(in.PublicRevisionID)
		pPtr = &v
	}
	if strings.TrimSpace(in.AssetID) != "" {
		v := strings.TrimSpace(in.AssetID)
		aPtr = &v
	}
	if strings.TrimSpace(in.FirstComment) != "" {
		v := strings.TrimSpace(in.FirstComment)
		cPtr = &v
	}
	return a.pubSvc.CreateDraft(a.opCtx(), publishing.DraftRequest{
		AdminUser:            admin,
		PublicRevisionID:     pPtr,
		PrivateEntryID:       ePtr,
		AssetID:              aPtr,
		DestinationAccount:   in.DestinationAccount,
		BrandPrefix:          in.BrandPrefix,
		IncludeCompanionTags: in.IncludeCompanionTags,
		TagsInFirstComment:   in.TagsInFirstComment,
		CustomFirstComment:   cPtr,
	})
}

type BatchDraftOutcome struct {
	Total   int                      `json:"total"`
	Created int                      `json:"created"`
	Failed  int                      `json:"failed"`
	Items   []BatchDraftOutcomeEntry `json:"items"`
}

type BatchDraftOutcomeEntry struct {
	InputEntryID string                 `json:"input_entry_id"`
	Status       string                 `json:"status"`
	Publication  *domain.PublicationJob `json:"publication,omitempty"`
	Error        string                 `json:"error,omitempty"`
}

// BatchCreateDrafts generates Instagram publication drafts across multiple entries, isolating per-item errors (Phase 5 acceptance).
func (a *App) BatchCreateDrafts(inputs []DesktopDraftInput) (*BatchDraftOutcome, error) {
	if _, err := a.requireAdmin(); err != nil {
		return nil, err
	}
	res := &BatchDraftOutcome{Total: len(inputs)}
	for _, in := range inputs {
		pub, err := a.CreatePublicationDraft(in)
		label := in.PrivateEntryID
		if label == "" {
			label = in.PublicRevisionID
		}
		if err != nil {
			res.Failed++
			res.Items = append(res.Items, BatchDraftOutcomeEntry{
				InputEntryID: label,
				Status:       "failed",
				Error:        err.Error(),
			})
			continue
		}
		res.Created++
		res.Items = append(res.Items, BatchDraftOutcomeEntry{
			InputEntryID: label,
			Status:       "created",
			Publication:  pub,
		})
	}
	return res, nil
}

func (a *App) ListPublications() ([]domain.PublicationJob, error) {
	if _, err := a.requireAdmin(); err != nil {
		return nil, err
	}
	return a.store.ListPublicationJobs(a.opCtx(), 100)
}

func (a *App) GetPublication(pubID string) (*domain.PublicationJob, error) {
	if _, err := a.requireAdmin(); err != nil {
		return nil, err
	}
	return a.store.GetPublicationJob(a.opCtx(), pubID)
}

func (a *App) UpdatePublicationDraft(pubID, caption, firstComment string, includeCompanion bool, assetID string) (*domain.PublicationJob, error) {
	admin, err := a.requireAdmin()
	if err != nil {
		return nil, err
	}
	var aPtr *string
	if strings.TrimSpace(assetID) != "" {
		v := strings.TrimSpace(assetID)
		aPtr = &v
	}
	return a.pubSvc.UpdateDraft(a.opCtx(), publishing.UpdateDraftInput{
		AdminUser:            admin,
		PublicationID:        pubID,
		Caption:              &caption,
		FirstComment:         &firstComment,
		IncludeCompanionTags: &includeCompanion,
		AssetID:              aPtr,
	})
}

func (a *App) ApprovePublication(pubID, exactDraftHash, destinationAccount string) (*domain.PublicationJob, error) {
	admin, err := a.requireAdmin()
	if err != nil {
		return nil, err
	}
	return a.pubSvc.Approve(a.opCtx(), admin, pubID, exactDraftHash, destinationAccount)
}

func (a *App) ExecutePublication(pubID, idempotencyKey string, dryRun bool) (*domain.PublicationJob, error) {
	admin, err := a.requireAdmin()
	if err != nil {
		return nil, err
	}
	return a.pubSvc.Execute(a.opCtx(), publishing.ExecuteOptions{
		AdminUser:      admin,
		PublicationID:  pubID,
		IdempotencyKey: idempotencyKey,
		DryRun:         dryRun,
	})
}

func (a *App) ReconcilePublication(pubID, manualResultURL string, retryFirstComment bool) (*domain.PublicationJob, error) {
	admin, err := a.requireAdmin()
	if err != nil {
		return nil, err
	}
	return a.pubSvc.Reconcile(a.opCtx(), admin, pubID, manualResultURL, retryFirstComment)
}

func generatePKCEPair() (verifier string, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("pkce rand: %w", err)
	}
	verifier = hex.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

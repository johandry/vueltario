package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johandry/vueltario/internal/analysis"
	"github.com/johandry/vueltario/internal/config"
	"github.com/johandry/vueltario/internal/db"
	"github.com/johandry/vueltario/internal/domain"
	"github.com/johandry/vueltario/internal/media"
	"github.com/johandry/vueltario/internal/providers"
	"github.com/johandry/vueltario/internal/publishing"
	"github.com/johandry/vueltario/internal/worker"
)

type testEnv struct {
	srv      *httptest.Server
	store    *db.Store
	mediaSvc *media.Service
	analyzer *analysis.Service
	cfg      config.Config
	admin    domain.User
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	cfg := config.Load()
	cfg.DBDriver = "sqlite"
	cfg.DatabaseURL = filepath.Join(tmp, "api_test.db")
	cfg.StorageRoot = filepath.Join(tmp, "storage")
	cfg.MaxUploadBytes = 1024 * 1024 // 1 MiB ceiling for oversize test
	cfg.FFprobeBinary = ""
	cfg.AdminEmail = "admin@vueltario.com"
	cfg.AdminPassword = "admin-secret-password"
	cfg.SignupMode = "invited_moderated"

	store, err := db.Open(cfg.DBDriver, cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := store.MigrateUp(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	admin, err := store.EnsureBootstrap(ctx, cfg.AdminEmail, cfg.AdminPassword)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	reg := providers.NewDefaultRegistry(nil, "")
	mediaSvc, err := media.NewService(cfg, store)
	if err != nil {
		t.Fatalf("media: %v", err)
	}
	analyzer := analysis.NewService(cfg, store, reg, nil, nil)
	pubSvc := publishing.NewService(cfg, store, nil)
	hub := worker.NewEventHub()

	apiServer := NewServer(cfg, store, reg, mediaSvc, analyzer, pubSvc, hub)
	ts := httptest.NewServer(apiServer.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = store.Close()
	})

	return &testEnv{
		srv:      ts,
		store:    store,
		mediaSvc: mediaSvc,
		analyzer: analyzer,
		cfg:      cfg,
		admin:    *admin,
	}
}

func (e *testEnv) loginCLI(t *testing.T, email, password string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"email":    email,
		"password": password,
		"client":   "cli",
	})
	resp, err := http.Post(e.srv.URL+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 login, got %d", resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Token == "" {
		t.Fatalf("expected CLI bearer token")
	}
	return out.Token
}

func doBearerJSON(t *testing.T, method, url, token string, reqBody any, extraHeaders map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	var rdr *bytes.Reader
	if reqBody != nil {
		b, _ := json.Marshal(reqBody)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	var parsed map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&parsed)
	return resp, parsed
}

// Phase 1 Acceptance: ingest 20 representative links across all 4 platforms, verify deduplication,
// multi-move occurrences, alias search, optimistic concurrency, and backup/restore.
func TestPhase1TwentyRepresentativeLinksMultiMoveAndBackupRestore(t *testing.T) {
	env := newTestEnv(t)
	adminTok := env.loginCLI(t, env.cfg.AdminEmail, env.cfg.AdminPassword)

	twentyLinks := []string{
		"https://www.youtube.com/watch?v=aaaaaaaaaa1",
		"https://www.youtube.com/watch?v=aaaaaaaaaa2&t=12s",
		"https://youtu.be/aaaaaaaaaa3",
		"https://www.youtube.com/shorts/aaaaaaaaaa4",
		"https://www.youtube.com/watch?v=aaaaaaaaaa5&utm_source=ig",
		"https://www.tiktok.com/@creator1/video/7000000000000000001",
		"https://www.tiktok.com/@creator2/video/7000000000000000002?_t=123",
		"https://www.tiktok.com/@creator3/video/7000000000000000003",
		"https://vm.tiktok.com/ZMMtest004/",
		"https://www.tiktok.com/@creator5/video/7000000000000000005",
		"https://www.facebook.com/watch/?v=1000000000001",
		"https://www.facebook.com/watch/?v=1000000000002&fbclid=xyz",
		"https://www.facebook.com/reel/1000000000003",
		"https://fb.watch/abcdEFGH04/",
		"https://www.facebook.com/watch/?v=1000000000005",
		"https://www.instagram.com/reel/IGReelCode01/",
		"https://www.instagram.com/reel/IGReelCode02/?igsh=123",
		"https://www.instagram.com/p/IGPostCode03/",
		"https://www.instagram.com/tv/IGTVCode0004/",
		"https://www.instagram.com/reels/IGReelCode05/",
	}

	resp, batchOut := doBearerJSON(t, http.MethodPost, env.srv.URL+"/api/v1/entries/batch", adminTok, map[string]any{
		"urls": twentyLinks,
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batch ingest failed: %d %v", resp.StatusCode, batchOut)
	}
	summary := batchOut["summary"].(map[string]any)
	if int(summary["created"].(float64)) != 20 {
		t.Fatalf("expected 20 created entries, got %v", summary)
	}

	// Saving duplicate link returns 200 deduplicated=true
	dupResp, dupBody := doBearerJSON(t, http.MethodPost, env.srv.URL+"/api/v1/entries", adminTok, map[string]any{
		"url": "https://youtu.be/aaaaaaaaaa1?si=tracking",
	}, nil)
	if dupResp.StatusCode != http.StatusOK || dupBody["deduplicated"] != true {
		t.Fatalf("expected deduplicated 200, got %d %v", dupResp.StatusCode, dupBody)
	}
	entryID := dupBody["entry"].(map[string]any)["id"].(string)

	// Section 17 Multi-move video example: Enchufla 00:12-00:22, Dile que no 00:23-00:30, Enchufla 00:48-00:58
	for _, occ := range []map[string]any{
		{"proposed_name": "Enchufla", "start_text": "00:12", "end_text": "00:22"},
		{"proposed_name": "Dile que no", "start_text": "00:23", "end_text": "00:30"},
		{"proposed_name": "Enchufla", "start_text": "00:48", "end_text": "00:58"},
	} {
		oResp, _ := doBearerJSON(t, http.MethodPost, fmt.Sprintf("%s/api/v1/entries/%s/occurrences", env.srv.URL, entryID), adminTok, occ, nil)
		if oResp.StatusCode != http.StatusCreated {
			t.Fatalf("add occurrence failed: %d", oResp.StatusCode)
		}
	}

	// Search by alias ("Adiós" is seeded alias for "Prima"; let's also search "Enchufla")
	sResp, sBody := doBearerJSON(t, http.MethodGet, env.srv.URL+"/api/v1/entries?q=Enchufla", adminTok, nil, nil)
	if sResp.StatusCode != http.StatusOK || len(sBody["entries"].([]any)) != 1 {
		t.Fatalf("expected search for Enchufla to return 1 entry, got %v", sBody)
	}

	// Optimistic concurrency check: stale If-Match v1 must return 409 Conflict!
	conflictResp, _ := doBearerJSON(t, http.MethodPatch, fmt.Sprintf("%s/api/v1/entries/%s", env.srv.URL, entryID), adminTok, map[string]any{
		"private_notes": "Should conflict",
	}, map[string]string{"If-Match": `"v1"`})
	if conflictResp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for stale If-Match, got %d", conflictResp.StatusCode)
	}

	// Backup and restore into a fresh user account
	ctx := context.Background()
	bundle, err := env.store.CreateBackupBundle(ctx, env.admin.ID)
	if err != nil || len(bundle.Entries) != 20 {
		t.Fatalf("backup expected 20 entries, got %d / %v", len(bundle.Entries), err)
	}
	inv, err := env.store.CreateInvite(ctx, env.admin.ID, "restore@example.com", domain.RoleMember, time.Hour)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	member, err := env.store.RegisterMember(ctx, "invited_moderated", inv.PlainCode, "restore@example.com", "member-pass-123", "Restorer")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	restoredCount, err := env.store.RestoreBackupBundle(ctx, member.ID, *bundle)
	if err != nil || restoredCount != 20 {
		t.Fatalf("restore expected 20 entries, got %d / %v", restoredCount, err)
	}
}

// Section 16 Authorization, CSRF, Private Isolation, Media Lifecycle, Suggestion Non-Overwrite & Worker Lease Recovery
func TestCrossUserIsolationCSRFMediaAndSuggestionProtection(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	adminTok := env.loginCLI(t, env.cfg.AdminEmail, env.cfg.AdminPassword)

	// Verify CSP & CSRF enforcement on web cookie sessions
	loginPayload, _ := json.Marshal(map[string]string{
		"email": env.cfg.AdminEmail, "password": env.cfg.AdminPassword, "client": "web",
	})
	webLoginResp, err := http.Post(env.srv.URL+"/api/v1/auth/login", "application/json", bytes.NewReader(loginPayload))
	if err != nil {
		t.Fatalf("web login: %v", err)
	}
	defer webLoginResp.Body.Close()
	if !strings.Contains(webLoginResp.Header.Get("Content-Security-Policy"), "frame-src") {
		t.Fatalf("missing CSP header")
	}
	var webSess struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.NewDecoder(webLoginResp.Body).Decode(&webSess)
	cookies := webLoginResp.Cookies()

	// Cookie mutation WITHOUT X-CSRF-Token must be rejected with 403!
	csrfReq, _ := http.NewRequest(http.MethodPost, env.srv.URL+"/api/v1/entries", strings.NewReader(`{"url":"https://youtu.be/dQw4w9WgXcQ"}`))
	csrfReq.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		csrfReq.AddCookie(c)
	}
	csrfResp, err := http.DefaultClient.Do(csrfReq)
	if err != nil {
		t.Fatalf("csrf req: %v", err)
	}
	csrfResp.Body.Close()
	if csrfResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 without CSRF token, got %d", csrfResp.StatusCode)
	}

	// Create two invited members (Alice and Bob)
	invA, _ := env.store.CreateInvite(ctx, env.admin.ID, "alice@example.com", domain.RoleMember, time.Hour)
	invB, _ := env.store.CreateInvite(ctx, env.admin.ID, "bob@example.com", domain.RoleMember, time.Hour)
	alice, err := env.store.RegisterMember(ctx, "invited_moderated", invA.PlainCode, "alice@example.com", "alice-password-1", "Alice")
	if err != nil {
		t.Fatalf("alice register: %v", err)
	}
	_, err = env.store.RegisterMember(ctx, "invited_moderated", invB.PlainCode, "bob@example.com", "bob-password-1", "Bob")
	if err != nil {
		t.Fatalf("bob register: %v", err)
	}
	aliceTok := env.loginCLI(t, "alice@example.com", "alice-password-1")
	bobTok := env.loginCLI(t, "bob@example.com", "bob-password-1")

	// Alice saves a link with secret private notes and imports a private video file
	_, aliceSave := doBearerJSON(t, http.MethodPost, env.srv.URL+"/api/v1/entries", aliceTok, map[string]any{
		"url":           "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"private_notes": "ALICE_TOP_SECRET_PRACTICE_CUE",
	}, nil)
	aliceEntry := aliceSave["entry"].(map[string]any)
	aliceEntryID := aliceEntry["id"].(string)
	sourceID := aliceEntry["source_id"].(string)

	// Bob saves the EXACT same source URL independently; Bob must NOT see Alice's notes or entry!
	_, bobSave := doBearerJSON(t, http.MethodPost, env.srv.URL+"/api/v1/entries", bobTok, map[string]any{
		"url":           "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"private_notes": "Bob personal note",
	}, nil)
	bobEntryID := bobSave["entry"].(map[string]any)["id"].(string)
	if bobEntryID == aliceEntryID {
		t.Fatalf("Alice and Bob must have distinct personal entry IDs")
	}

	// Bob tries to read Alice's entry -> 403 Forbidden!
	bobReadAlice, _ := doBearerJSON(t, http.MethodGet, env.srv.URL+"/api/v1/entries/"+aliceEntryID, bobTok, nil, nil)
	if bobReadAlice.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 when Bob reads Alice's entry, got %d", bobReadAlice.StatusCode)
	}

	// Media import tests: reject non-video garbage; accept valid MP4; block Bob from streaming Alice's private asset; revoke immediately on delete!
	badAssetResp := uploadMultipart(t, env.srv.URL+"/api/v1/assets/imports", aliceTok, sourceID, "evil.exe", []byte("MZ-not-a-video-file-at-all-1234567890"))
	if badAssetResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed container, got %d", badAssetResp.StatusCode)
	}
	mp4Bytes := []byte{
		0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm',
		0x00, 0x00, 0x02, 0x00, 'i', 's', 'o', 'm', 'm', 'p', '4', '2',
	}
	goodResp := uploadMultipart(t, env.srv.URL+"/api/v1/assets/imports", aliceTok, sourceID, "authorized.mp4", mp4Bytes)
	if goodResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 for valid MP4, got %d", goodResp.StatusCode)
	}
	var importedAsset domain.MediaAsset
	_ = json.NewDecoder(goodResp.Body).Decode(&importedAsset)
	goodResp.Body.Close()

	// Bob tries to stream Alice's private asset -> 403!
	bobStream, _ := doBearerJSON(t, http.MethodGet, env.srv.URL+importedAsset.StreamURL, bobTok, nil, nil)
	if bobStream.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 when Bob streams Alice's private asset, got %d", bobStream.StatusCode)
	}

	// Alice submits to community -> public snapshot MUST exclude ALICE_TOP_SECRET_PRACTICE_CUE!
	_, subBody := doBearerJSON(t, http.MethodPost, fmt.Sprintf("%s/api/v1/entries/%s/submissions", env.srv.URL, aliceEntryID), aliceTok, map[string]any{}, nil)
	subBytes, _ := json.Marshal(subBody)
	if strings.Contains(string(subBytes), "ALICE_TOP_SECRET_PRACTICE_CUE") {
		t.Fatalf("PRIVACY REGRESSION: private notes leaked into public submission!")
	}
	subID := subBody["id"].(string)

	// Admin approves submission -> visible publicly without private notes, and Alice's private asset remains inaccessible to visitors!
	decResp, _ := doBearerJSON(t, http.MethodPost, fmt.Sprintf("%s/api/v1/admin/submissions/%s/decision", env.srv.URL, subID), adminTok, map[string]any{
		"decision": "approved",
		"reason":   "Great vuelta reference",
	}, nil)
	if decResp.StatusCode != http.StatusOK {
		t.Fatalf("approve submission failed: %d", decResp.StatusCode)
	}
	pubResp, pubCatalog := doBearerJSON(t, http.MethodGet, env.srv.URL+"/api/v1/public/entries", "", nil, nil)
	if pubResp.StatusCode != http.StatusOK || len(pubCatalog["entries"].([]any)) != 1 {
		t.Fatalf("expected 1 public entry for visitor, got %v", pubCatalog)
	}

	// Delete Alice's asset -> stream immediately returns 404!
	delAssetResp, _ := doBearerJSON(t, http.MethodDelete, env.srv.URL+"/api/v1/assets/"+importedAsset.ID, aliceTok, nil, nil)
	if delAssetResp.StatusCode != http.StatusOK {
		t.Fatalf("delete asset failed: %d", delAssetResp.StatusCode)
	}
	afterDelStream, _ := doBearerJSON(t, http.MethodGet, env.srv.URL+importedAsset.StreamURL, aliceTok, nil, nil)
	if afterDelStream.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted asset must immediately return 404, got %d", afterDelStream.StatusCode)
	}

	// Section 17 Correction example: description suggests "Enchufla doble", Alice confirms "Enchufla" and rejects "Enchufla doble"; reanalysis MUST NOT revert it!
	descText := "Lesson: 00:10 - 00:25 Enchufla doble tutorial"
	_, _ = env.store.ApplyFetchedSourceMetadata(ctx, sourceID, providers.FetchedMetadata{
		Title:             &descText,
		Description:       &descText,
		AvailabilityState: domain.AvailabilityAvailable,
		EmbedEligible:     true,
		Provenance:        "test",
	})
	out1, err := env.analyzer.AnalyzeEntry(ctx, *alice, aliceEntryID, true)
	if err != nil || out1.SuggestionCount == 0 {
		t.Fatalf("expected suggestion for Enchufla doble, got %+v / %v", out1, err)
	}
	suggID := out1.Suggestions[0].ID
	if _, err := env.store.DecideSuggestion(ctx, alice.ID, aliceEntryID, suggID, "reject"); err != nil {
		t.Fatalf("reject suggestion: %v", err)
	}
	confirmedMove := "Enchufla"
	_, _ = env.store.AddOccurrence(ctx, db.OccurrenceInput{UserID: alice.ID, EntryID: aliceEntryID, ProposedName: &confirmedMove})

	// Force reanalysis -> rejected "Enchufla doble" must NOT reappear as pending and confirmed "Enchufla" must remain intact!
	_, err = env.analyzer.AnalyzeEntry(ctx, *alice, aliceEntryID, true)
	if err != nil {
		t.Fatalf("reanalysis: %v", err)
	}
	reloadedAlice, _ := env.store.GetEntry(ctx, alice.ID, aliceEntryID)
	for _, sg := range reloadedAlice.Suggestions {
		if sg.State == "pending" && sg.CandidateName == "Enchufla doble" {
			t.Fatalf("reanalysis recreated a user-rejected suggestion!")
		}
	}
	if len(reloadedAlice.Occurrences) != 1 || reloadedAlice.Occurrences[0].MoveName != "Enchufla" {
		t.Fatalf("confirmed user occurrence altered by reanalysis: %+v", reloadedAlice.Occurrences)
	}

	// Worker expired lease recovery check (Section 16 Operational tests)
	uid := env.admin.ID
	leaseJob, err := env.store.EnqueueJob(ctx, &uid, "orphan_scan", "system", "storage", 1, nil, "lease-test-1", "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claimed1, err := env.store.ClaimNextJob(ctx, "worker-crashed", -2*time.Second) // Already expired lease
	if err != nil || claimed1 == nil {
		t.Fatalf("claim 1 failed: %v", err)
	}
	// Drain any earlier queued enrich jobs until worker-2 recovers leaseJob
	recovered := false
	for i := 0; i < 10; i++ {
		c, err := env.store.ClaimNextJob(ctx, "worker-healthy", 30*time.Second)
		if err != nil || c == nil {
			break
		}
		if c.ID == leaseJob.ID {
			recovered = true
			_ = env.store.CompleteJob(ctx, c.ID)
			break
		}
		_ = env.store.CompleteJob(ctx, c.ID)
	}
	if !recovered {
		t.Fatalf("healthy worker did not recover crashed worker's expired lease")
	}
}

func uploadMultipart(t *testing.T, url, bearerToken, sourceID, filename string, content []byte) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("source_id", sourceID)
	fw, _ := mw.CreateFormFile("file", filename)
	_, _ = fw.Write(content)
	_ = mw.Close()

	req, _ := http.NewRequest(http.MethodPost, url, &buf)
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload request: %v", err)
	}
	return resp
}

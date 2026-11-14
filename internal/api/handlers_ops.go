package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/johandry/vueltario/internal/db"
	"github.com/johandry/vueltario/internal/domain"
	"github.com/johandry/vueltario/internal/media"
	"github.com/johandry/vueltario/internal/publishing"
)

// --- Playback, Media, Staging & Rights ---

func (s *Server) handleSourcePlayback(w http.ResponseWriter, r *http.Request) {
	sourceID := r.PathValue("id")
	src, err := s.store.GetSource(r.Context(), sourceID)
	if err != nil {
		writeDomainError(w, err)
		return
	}

	sess := currentSession(r)
	// Visitors may only request playback descriptors for sources with an active approved public revision!
	if sess == nil {
		pubItems, _, _ := s.store.ListPublicCatalog(r.Context(), "", string(src.Platform), "", 100)
		allowed := false
		for _, pr := range pubItems {
			if pr.SourceID == src.ID {
				allowed = true
				break
			}
		}
		if !allowed {
			writeError(w, http.StatusForbidden, "forbidden", "Source is not in the public catalog")
			return
		}
	} else if sess.User.Role != domain.RoleAdmin {
		// Verify member either owns an entry for this source OR source is publicly published
		entries, _ := s.store.ListEntries(r.Context(), db.EntryFilter{UserID: sess.User.ID, Limit: 200})
		owns := false
		for _, e := range entries {
			if e.SourceID == src.ID {
				owns = true
				break
			}
		}
		if !owns {
			pubItems, _, _ := s.store.ListPublicCatalog(r.Context(), "", string(src.Platform), "", 100)
			for _, pr := range pubItems {
				if pr.SourceID == src.ID {
					owns = true
					break
				}
			}
		}
		if !owns {
			writeError(w, http.StatusForbidden, "forbidden", "Access denied for this source")
			return
		}
	}

	var permittedAsset *domain.MediaAsset
	if sess != nil {
		entries, _ := s.store.ListEntries(r.Context(), db.EntryFilter{UserID: sess.User.ID, Limit: 200})
		for _, e := range entries {
			if e.SourceID == src.ID && len(e.Assets) > 0 {
				a := e.Assets[0]
				permittedAsset = &a
				break
			}
		}
	}
	desc := s.providers.DescribePlayback(*src, permittedAsset)
	writeJSON(w, http.StatusOK, desc)
}

func (s *Server) handleImportAsset(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes+1024*1024)

	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_multipart", "Expected multipart/form-data with source_id and file")
		return
	}

	var (
		sourceID    string
		accessScope string
		asset       *domain.MediaAsset
	)
	for {
		part, pErr := mr.NextPart()
		if pErr == io.EOF {
			break
		}
		if pErr != nil {
			writeError(w, http.StatusBadRequest, "multipart_error", pErr.Error())
			return
		}
		switch part.FormName() {
		case "source_id":
			b, _ := io.ReadAll(io.LimitReader(part, 128))
			sourceID = strings.TrimSpace(string(b))
		case "access_scope":
			b, _ := io.ReadAll(io.LimitReader(part, 64))
			accessScope = strings.TrimSpace(string(b))
		case "file":
			if sourceID == "" {
				sourceID = strings.TrimSpace(r.URL.Query().Get("source_id"))
			}
			if sourceID == "" {
				writeError(w, http.StatusBadRequest, "validation_error", "source_id field must precede file part or be passed as query param")
				return
			}
			asset, err = s.mediaSvc.ImportAuthorizedFile(r.Context(), media.ImportRequest{
				User:        sess.User,
				SourceID:    sourceID,
				Filename:    part.FileName(),
				AccessScope: accessScope,
				Reader:      part,
			})
			_ = part.Close()
			if err != nil {
				writeDomainError(w, err)
				return
			}
		}
		_ = part.Close()
	}
	if asset == nil {
		writeError(w, http.StatusBadRequest, "missing_file", "No file part received")
		return
	}
	writeJSON(w, http.StatusCreated, asset)
}

func (s *Server) handleDeriveClip(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	var req struct {
		StartMs int64 `json:"start_ms"`
		EndMs   int64 `json:"end_ms"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Invalid clip range")
		return
	}
	clip, err := s.mediaSvc.DeriveClip(r.Context(), sess.User, r.PathValue("id"), req.StartMs, req.EndMs)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, clip)
}

func (s *Server) handleStreamAsset(w http.ResponseWriter, r *http.Request) {
	var caller *domain.User
	if sess := currentSession(r); sess != nil {
		caller = &sess.User
	}
	f, asset, err := s.mediaSvc.OpenPermittedStream(r.Context(), caller, r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "stat_error", "Unable to stat media file")
		return
	}
	w.Header().Set("Content-Type", asset.MIMEType)
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, asset.ID, info.ModTime(), f)
}

func (s *Server) handleDeleteAsset(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	if err := s.mediaSvc.DeleteAsset(r.Context(), sess.User, r.PathValue("id")); err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleStreamStagedMedia serves short-lived externally reachable staging URLs for Meta's URL-fetch workflow (Section 6).
func (s *Server) handleStreamStagedMedia(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	pub, err := s.store.GetPublicationByStagingToken(r.Context(), token)
	if err != nil || pub.AssetID == nil {
		writeError(w, http.StatusNotFound, "not_found", "Staging URL invalid or revoked")
		return
	}
	if pub.StagingExpiresAt == nil || time.Now().UTC().After(*pub.StagingExpiresAt) {
		writeError(w, http.StatusGone, "expired", "Staging URL expired")
		return
	}
	asset, err := s.store.GetMediaAsset(r.Context(), *pub.AssetID)
	if err != nil || asset.Status != "ready" || asset.DeletedAt != nil {
		writeError(w, http.StatusNotFound, "not_found", "Asset unavailable")
		return
	}
	fullPath, err := s.mediaSvc.ResolvePath(asset.StorageKey)
	if err != nil {
		writeError(w, http.StatusForbidden, "forbidden", "Invalid asset path")
		return
	}
	http.ServeFile(w, r, fullPath)
}

func (s *Server) handleListRights(w http.ResponseWriter, r *http.Request) {
	records, err := s.store.ListRightsForSource(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rights": records})
}

func (s *Server) handleCreateRights(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	var req struct {
		SourceID              string     `json:"source_id"`
		AssetID               *string    `json:"asset_id"`
		Grantor               string     `json:"grantor"`
		Basis                 string     `json:"basis"`
		EvidenceRef           string     `json:"evidence_ref"`
		VideoCleared          bool       `json:"video_cleared"`
		MusicCleared          bool       `json:"music_cleared"`
		AuthorizedUses        []string   `json:"authorized_uses"`
		AuthorizedPlatforms   []string   `json:"authorized_platforms"`
		AttributionConditions string     `json:"attribution_conditions"`
		ExpiresAt             *time.Time `json:"expires_at"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Invalid rights payload")
		return
	}
	rec, err := s.store.CreateRightsRecord(r.Context(), domain.RightsRecord{
		SourceID:              req.SourceID,
		AssetID:               req.AssetID,
		Grantor:               req.Grantor,
		Basis:                 req.Basis,
		EvidenceRef:           req.EvidenceRef,
		VideoCleared:          req.VideoCleared,
		MusicCleared:          req.MusicCleared,
		AuthorizedUses:        req.AuthorizedUses,
		AuthorizedPlatforms:   req.AuthorizedPlatforms,
		AttributionConditions: req.AttributionConditions,
		ExpiresAt:             req.ExpiresAt,
		ReviewerID:            sess.User.ID,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

func (s *Server) handleRevokeRights(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Invalid revocation payload")
		return
	}
	if err := s.store.RevokeRightsRecord(r.Context(), sess.User.ID, r.PathValue("id"), req.Reason); err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// --- Enrichment, Suggestions, Jobs & SSE ---

func (s *Server) handleTriggerAnalysis(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	entryID := r.PathValue("id")
	ent, err := s.store.GetEntry(r.Context(), sess.User.ID, entryID)
	if err != nil {
		writeDomainError(w, err)
		return
	}

	var req struct {
		Force bool `json:"force"`
		Sync  bool `json:"sync"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	if req.Sync {
		outcome, err := s.analyzer.AnalyzeEntry(r.Context(), sess.User, ent.ID, req.Force)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, outcome)
		return
	}

	idem := fmt.Sprintf("analyze:%s:v%d:%d", ent.ID, ent.Source.MetadataVersion, time.Now().Unix()/10)
	job, err := s.store.EnqueueJob(
		r.Context(),
		&sess.User.ID,
		"analyze_entry",
		"entry",
		ent.ID,
		ent.Revision,
		map[string]any{"entry_id": ent.ID, "force": req.Force},
		idem,
		correlationID(r),
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

func (s *Server) handleDecideSuggestion(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	var req struct {
		Decision string `json:"decision"` // "accept" or "reject"
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Invalid suggestion decision")
		return
	}
	ent, err := s.store.DecideSuggestion(r.Context(), sess.User.ID, r.PathValue("id"), r.PathValue("suggId"), req.Decision)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	w.Header().Set("ETag", formatETag(ent.Revision))
	writeJSON(w, http.StatusOK, ent)
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	job, err := s.store.GetJobScoped(r.Context(), sess.User, r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok || s.hub == nil {
		writeError(w, http.StatusNotImplemented, "sse_unavailable", "Streaming unsupported")
		return
	}
	sess := currentSession(r)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, unsub := s.hub.Subscribe(sess.User.ID)
	defer unsub()

	_, _ = fmt.Fprintf(w, "event: connected\ndata: {\"status\":\"ready\"}\n\n")
	flusher.Flush()

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			_, _ = fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		case ev := <-ch:
			b, _ := json.Marshal(ev)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, string(b))
			flusher.Flush()
		}
	}
}

// --- Community & Moderation ---

func (s *Server) handleSubmitToCommunity(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	sub, err := s.store.CreateSubmission(r.Context(), sess.User.ID, r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sub)
}

func (s *Server) handleListPublicEntries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	items, nextCursor, err := s.store.ListPublicCatalog(r.Context(), q.Get("q"), q.Get("platform"), q.Get("cursor"), limit)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries":     items,
		"next_cursor": nextCursor,
	})
}

func (s *Server) handleGetPublicEntry(w http.ResponseWriter, r *http.Request) {
	pr, err := s.store.GetPublicRevision(r.Context(), r.PathValue("id"))
	if err != nil || pr.Status != "active" {
		writeError(w, http.StatusNotFound, "not_found", "Public entry not found")
		return
	}
	writeJSON(w, http.StatusOK, pr)
}

func (s *Server) handleCreatePublicCorrection(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	var req struct {
		Reason          string               `json:"reason"`
		ProposedChanges domain.PublicPayload `json:"proposed_changes"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Invalid correction proposal")
		return
	}
	corr, err := s.store.CreatePublicCorrection(r.Context(), sess.User.ID, r.PathValue("id"), req.Reason, req.ProposedChanges)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, corr)
}

func (s *Server) handleCreateReport(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.Allow("report:" + clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "Too many reports")
		return
	}
	var req struct {
		Category string `json:"category"`
		Details  string `json:"details"`
		Contact  string `json:"contact"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Invalid report payload")
		return
	}
	var reporterID *string
	if sess := currentSession(r); sess != nil {
		reporterID = &sess.User.ID
	}
	rep, err := s.store.CreateReport(r.Context(), reporterID, r.PathValue("id"), req.Category, req.Details, req.Contact)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rep)
}

func (s *Server) handleAdminListSubmissions(w http.ResponseWriter, r *http.Request) {
	subs, err := s.store.ListSubmissions(r.Context(), r.URL.Query().Get("status"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"submissions": subs})
}

func (s *Server) handleAdminDecideSubmission(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	var req struct {
		Decision string `json:"decision"` // approved, rejected, changes_requested
		Reason   string `json:"reason"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Invalid moderation decision")
		return
	}
	pubRev, err := s.store.DecideSubmission(r.Context(), sess.User.ID, r.PathValue("id"), req.Decision, req.Reason)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"decision":        req.Decision,
		"public_revision": pubRev,
	})
}

func (s *Server) handleAdminHidePublicRevision(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Invalid hide payload")
		return
	}
	if err := s.store.HidePublicRevision(r.Context(), sess.User.ID, r.PathValue("id"), req.Reason); err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "hidden"})
}

// --- Instagram Publishing Endpoints ---

func (s *Server) handleListPublications(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.store.ListPublicationJobs(r.Context(), 50)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"publications": jobs})
}

func (s *Server) handleGetPublication(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.GetPublicationJob(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleCreatePublication(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	var req struct {
		PublicRevisionID     *string               `json:"public_revision_id"`
		PrivateEntryID       *string               `json:"private_entry_id"`
		AssetID              *string               `json:"asset_id"`
		DestinationAccount   string                `json:"destination_account"`
		BrandPrefix          string                `json:"brand_prefix"`
		SelectedMoveNames    []string              `json:"selected_move_names"`
		SelectedSegments     []domain.SegmentRange `json:"selected_segments"`
		IncludeCompanionTags bool                  `json:"include_companion_tags"`
		TagsInFirstComment   bool                  `json:"tags_in_first_comment"`
		CustomCaption        *string               `json:"custom_caption"`
		CustomFirstComment   *string               `json:"custom_first_comment"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Invalid publication draft payload")
		return
	}
	job, err := s.pubSvc.CreateDraft(r.Context(), publishing.DraftRequest{
		AdminUser:            sess.User,
		PublicRevisionID:     req.PublicRevisionID,
		PrivateEntryID:       req.PrivateEntryID,
		AssetID:              req.AssetID,
		DestinationAccount:   req.DestinationAccount,
		BrandPrefix:          req.BrandPrefix,
		SelectedMoveNames:    req.SelectedMoveNames,
		SelectedSegments:     req.SelectedSegments,
		IncludeCompanionTags: req.IncludeCompanionTags,
		TagsInFirstComment:   req.TagsInFirstComment,
		CustomCaption:        req.CustomCaption,
		CustomFirstComment:   req.CustomFirstComment,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, job)
}

func (s *Server) handleUpdatePublication(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	var req struct {
		Caption              *string `json:"caption"`
		FirstComment         *string `json:"first_comment"`
		IncludeCompanionTags *bool   `json:"include_companion_tags"`
		AssetID              *string `json:"asset_id"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Invalid publication update")
		return
	}
	job, err := s.pubSvc.UpdateDraft(r.Context(), publishing.UpdateDraftInput{
		AdminUser:            sess.User,
		PublicationID:        r.PathValue("id"),
		Caption:              req.Caption,
		FirstComment:         req.FirstComment,
		IncludeCompanionTags: req.IncludeCompanionTags,
		AssetID:              req.AssetID,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleApprovePublication(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	var req struct {
		DraftHash          string `json:"draft_hash"`
		DestinationAccount string `json:"destination_account"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Invalid approval payload")
		return
	}
	job, err := s.pubSvc.Approve(r.Context(), sess.User, r.PathValue("id"), req.DraftHash, req.DestinationAccount)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleExecutePublication(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	var req struct {
		IdempotencyKey string `json:"idempotency_key"`
		DryRun         bool   `json:"dry_run"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	job, err := s.pubSvc.Execute(r.Context(), publishing.ExecuteOptions{
		AdminUser:      sess.User,
		PublicationID:  r.PathValue("id"),
		IdempotencyKey: req.IdempotencyKey,
		DryRun:         req.DryRun,
	})
	if err != nil {
		if job != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error":       map[string]string{"code": "publish_error", "message": err.Error()},
				"publication": job,
			})
			return
		}
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleReconcilePublication(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	var req struct {
		ManualResultURL   string `json:"manual_result_url"`
		RetryFirstComment bool   `json:"retry_first_comment"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	job, err := s.pubSvc.Reconcile(r.Context(), sess.User, r.PathValue("id"), req.ManualResultURL, req.RetryFirstComment)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// --- Sync, Portability & Backup ---

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	cursor, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, nextCursor, err := s.store.GetPersonalSyncDeltas(r.Context(), sess.User.ID, cursor, limit)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"events":      events,
		"next_cursor": nextCursor,
	})
}

func (s *Server) handleExportPersonal(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	bundle, err := s.store.ExportPersonalCatalog(r.Context(), sess.User.ID)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="vueltario-export.json"`)
	writeJSON(w, http.StatusOK, bundle)
}

func (s *Server) handleAdminBackup(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	bundle, err := s.store.CreateBackupBundle(r.Context(), sess.User.ID)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="vueltario-backup.json"`)
	writeJSON(w, http.StatusOK, bundle)
}

func (s *Server) handleAdminRestore(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	var bundle db.BackupBundle
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 25*1024*1024)).Decode(&bundle); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Invalid backup bundle")
		return
	}
	restored, err := s.store.RestoreBackupBundle(r.Context(), sess.User.ID, bundle)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"restored_entries": restored})
}

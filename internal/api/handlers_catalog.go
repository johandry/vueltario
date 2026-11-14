package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/johandry/vueltario/internal/db"
	"github.com/johandry/vueltario/internal/domain"
	"github.com/johandry/vueltario/internal/providers"
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"product":        "Vueltario",
		"db_driver":      s.store.Dialect(),
		"signup_mode":    s.cfg.SignupMode,
		"ig_destination": s.cfg.IGDestination,
		"ig_brand":       s.cfg.IGBrandPrefix,
	})
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"providers":               s.providers.AllCapabilities(),
		"representative_fixtures": providers.RepresentativeFixtures(),
		"instagram_publishing": map[string]any{
			"destination":            s.cfg.IGDestination,
			"default_brand_prefix":   s.cfg.IGBrandPrefix,
			"allow_companion_tags":   s.cfg.IGAllowCompanionTags,
			"native_repost_mode":     "manual_handoff",
			"dash_label_example":     "#" + s.cfg.IGBrandPrefix + "-Enchufla",
			"companion_tag_example":  "#" + s.cfg.IGBrandPrefix + "_Enchufla",
			"caption_max_chars":      s.cfg.IGCaptionMaxChars,
			"max_hashtags":           s.cfg.IGMaxHashtags,
			"credentials_configured": s.cfg.IGAccessToken != "" && s.cfg.IGUserID != "",
		},
	})
}

// --- Auth & Invites ---

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.Allow("login:" + clientIP(r)) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "rate_limited", "Too many login attempts")
		return
	}
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Client   string `json:"client"` // "web" or "cli"
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Invalid login payload")
		return
	}
	kind := "web"
	if req.Client == "cli" {
		kind = "cli"
	}
	sess, err := s.store.AuthenticatePassword(r.Context(), req.Email, req.Password, kind, 14*24*time.Hour)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	if kind == "web" {
		http.SetCookie(w, &http.Cookie{
			Name:     SessionCookieName,
			Value:    sess.PlainToken,
			Path:     "/",
			Expires:  sess.ExpiresAt,
			HttpOnly: true,
			Secure:   s.cfg.CookieSecure,
			SameSite: http.SameSiteLaxMode,
		})
		// Do not expose raw session token in web JSON body; expose CSRF token & user profile.
		sess.PlainToken = ""
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		_ = s.store.DeleteSessionByToken(r.Context(), c.Value)
	} else if authHdr := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(strings.ToLower(authHdr), "bearer ") {
		_ = s.store.DeleteSessionByToken(r.Context(), strings.TrimSpace(authHdr[7:]))
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged_out"})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.Allow("register:" + clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "Too many registration attempts")
		return
	}
	var req struct {
		InviteCode  string `json:"invite_code"`
		Email       string `json:"email"`
		Password    string `json:"password"`
		DisplayName string `json:"display_name"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Invalid registration payload")
		return
	}
	user, err := s.store.RegisterMember(r.Context(), s.cfg.SignupMode, req.InviteCode, req.Email, req.Password, req.DisplayName)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	sess, err := s.store.CreateSession(r.Context(), *user, "web", 14*24*time.Hour)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    sess.PlainToken,
		Path:     "/",
		Expires:  sess.ExpiresAt,
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	sess.PlainToken = ""
	writeJSON(w, http.StatusCreated, sess)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	if sess == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": false,
			"role":          domain.RoleVisitor,
			"signup_mode":   s.cfg.SignupMode,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"user":          sess.User,
		"csrf_token":    sess.CSRFToken,
		"signup_mode":   s.cfg.SignupMode,
	})
}

func (s *Server) handleUpdateMe(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	var req struct {
		Locale         string `json:"locale"`
		AnalysisOptOut bool   `json:"analysis_opt_out"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Invalid preferences payload")
		return
	}
	u, err := s.store.UpdateUserPreferences(r.Context(), sess.User.ID, req.Locale, req.AnalysisOptOut)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) handleDeleteMe(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	if err := s.store.DeleteAccount(r.Context(), sess.User.ID); err != nil {
		writeDomainError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "account_deleted",
		"notice": "Private notes, entries, private assets, and credentials have been removed. Independently curated public revisions and required audit logs are retained per community policy.",
	})
}

func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Invalid invite payload")
		return
	}
	inv, err := s.store.CreateInvite(r.Context(), sess.User.ID, req.Email, domain.Role(req.Role), 7*24*time.Hour)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, inv)
}

// --- Personal Catalog & Occurrences ---

func (s *Server) handleSaveEntry(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	var req struct {
		URL             string `json:"url"`
		ClientRequestID string `json:"client_request_id"`
		PrivateNotes    string `json:"private_notes"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Invalid link payload")
		return
	}
	res, err := s.store.SaveLink(r.Context(), db.SaveLinkRequest{
		UserID:          sess.User.ID,
		RawURL:          req.URL,
		ClientRequestID: req.ClientRequestID,
		PrivateNotes:    req.PrivateNotes,
		CorrelationID:   correlationID(r),
	})
	if err != nil {
		if errors.Is(err, domain.ErrUnsupportedHost) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error": map[string]any{
					"code":         "unsupported_host",
					"message":      err.Error(),
					"original_url": req.URL,
				},
			})
			return
		}
		writeDomainError(w, err)
		return
	}
	w.Header().Set("ETag", formatETag(res.Entry.Revision))
	status := http.StatusCreated
	if res.Deduplicated {
		status = http.StatusOK
	}
	writeJSON(w, status, res)
}

// handleBatchSaveEntries implements ING-05: reports per-item success, duplicates, and failures without aborting the batch.
func (s *Server) handleBatchSaveEntries(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	var req struct {
		URLs []string `json:"urls"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Invalid batch payload")
		return
	}
	if len(req.URLs) == 0 || len(req.URLs) > 100 {
		writeError(w, http.StatusBadRequest, "validation_error", "Batch must contain between 1 and 100 URLs")
		return
	}
	type itemOutcome struct {
		InputURL     string `json:"input_url"`
		Status       string `json:"status"` // "created", "duplicate", "failed"
		EntryID      string `json:"entry_id,omitempty"`
		SourceID     string `json:"source_id,omitempty"`
		Platform     string `json:"platform,omitempty"`
		ErrorMessage string `json:"error,omitempty"`
	}
	results := make([]itemOutcome, 0, len(req.URLs))
	var createdCount, dupCount, failCount int
	for _, rawURL := range req.URLs {
		res, err := s.store.SaveLink(r.Context(), db.SaveLinkRequest{
			UserID:        sess.User.ID,
			RawURL:        rawURL,
			CorrelationID: correlationID(r),
		})
		if err != nil {
			failCount++
			results = append(results, itemOutcome{
				InputURL:     rawURL,
				Status:       "failed",
				ErrorMessage: err.Error(),
			})
			continue
		}
		st := "created"
		if res.Deduplicated {
			st = "duplicate"
			dupCount++
		} else {
			createdCount++
		}
		results = append(results, itemOutcome{
			InputURL: rawURL,
			Status:   st,
			EntryID:  res.Entry.ID,
			SourceID: res.Source.ID,
			Platform: string(res.Source.Platform),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"summary": map[string]int{
			"total":      len(req.URLs),
			"created":    createdCount,
			"duplicates": dupCount,
			"failed":     failCount,
		},
		"items": results,
	})
}

func (s *Server) handleListEntries(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	q := r.URL.Query()
	var hasAsset *bool
	if ha := q.Get("has_asset"); ha == "true" || ha == "1" {
		t := true
		hasAsset = &t
	} else if ha == "false" || ha == "0" {
		f := false
		hasAsset = &f
	}
	entries, err := s.store.ListEntries(r.Context(), db.EntryFilter{
		UserID:     sess.User.ID,
		Query:      q.Get("q"),
		Platform:   q.Get("platform"),
		MoveID:     q.Get("move_id"),
		Visibility: q.Get("visibility"),
		HasAsset:   hasAsset,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (s *Server) handleGetEntry(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	entry, err := s.store.GetEntry(r.Context(), sess.User.ID, r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	w.Header().Set("ETag", formatETag(entry.Revision))
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) handleUpdateEntry(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	var req struct {
		Revision                *int    `json:"revision"`
		PrivateNotes            *string `json:"private_notes"`
		PersonalTitleOverride   *string `json:"personal_title_override"`
		PersonalCreatorOverride *string `json:"personal_creator_override"`
		ClearTitleOverride      bool    `json:"clear_title_override"`
		ClearCreatorOverride    bool    `json:"clear_creator_override"`
		SourceDurationMs        *int64  `json:"source_duration_ms"`
		Creator                 *struct {
			DisplayName string `json:"display_name"`
			Role        string `json:"role"`
			Handle      string `json:"handle"`
			ProfileURL  string `json:"profile_url"`
			Verified    bool   `json:"verified"`
		} `json:"creator"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Invalid entry update payload")
		return
	}
	expectedRev := parseIfMatchRevision(r, req.Revision)
	if expectedRev <= 0 {
		writeError(w, http.StatusPreconditionRequired, "if_match_required", "Provide If-Match header or revision field for optimistic concurrency")
		return
	}

	var creatorIn *db.CreatorAttributionInput
	if req.Creator != nil {
		verified := req.Creator.Verified && sess.User.Role == domain.RoleAdmin
		creatorIn = &db.CreatorAttributionInput{
			DisplayName: req.Creator.DisplayName,
			Role:        req.Creator.Role,
			Handle:      req.Creator.Handle,
			ProfileURL:  req.Creator.ProfileURL,
			Verified:    verified,
		}
	}

	updated, err := s.store.UpdateEntry(r.Context(), db.UpdateEntryInput{
		UserID:                  sess.User.ID,
		EntryID:                 r.PathValue("id"),
		ExpectedRevision:        expectedRev,
		PrivateNotes:            req.PrivateNotes,
		PersonalTitleOverride:   req.PersonalTitleOverride,
		PersonalCreatorOverride: req.PersonalCreatorOverride,
		ClearTitleOverride:      req.ClearTitleOverride,
		ClearCreatorOverride:    req.ClearCreatorOverride,
		SourceDurationMs:        req.SourceDurationMs,
		CreatorAttribution:      creatorIn,
	})
	if err != nil {
		if errors.Is(err, domain.ErrConflict) {
			current, _ := s.store.GetEntry(r.Context(), sess.User.ID, r.PathValue("id"))
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": map[string]string{
					"code":    "revision_conflict",
					"message": err.Error(),
				},
				"current_entry": current,
			})
			return
		}
		writeDomainError(w, err)
		return
	}
	w.Header().Set("ETag", formatETag(updated.Revision))
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteEntry(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	if err := s.store.DeleteEntry(r.Context(), sess.User.ID, r.PathValue("id")); err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleAddOccurrence(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	var req struct {
		MoveID       *string `json:"move_id"`
		ProposedName *string `json:"proposed_name"`
		StartMs      *int64  `json:"start_ms"`
		EndMs        *int64  `json:"end_ms"`
		StartText    *string `json:"start_text"`
		EndText      *string `json:"end_text"`
		Ordinal      int     `json:"ordinal"`
		Notes        string  `json:"notes"`
		TimingMethod string  `json:"timing_method"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Invalid occurrence payload")
		return
	}
	startMs := req.StartMs
	endMs := req.EndMs
	if startMs == nil && req.StartText != nil {
		p, err := domain.ParseTimestampMs(*req.StartText)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		startMs = p
	}
	if endMs == nil && req.EndText != nil {
		p, err := domain.ParseTimestampMs(*req.EndText)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		endMs = p
	}

	occ, err := s.store.AddOccurrence(r.Context(), db.OccurrenceInput{
		UserID:       sess.User.ID,
		EntryID:      r.PathValue("id"),
		MoveID:       req.MoveID,
		ProposedName: req.ProposedName,
		StartMs:      startMs,
		EndMs:        endMs,
		Ordinal:      req.Ordinal,
		Notes:        req.Notes,
		State:        domain.OccurrenceConfirmed,
		TimingMethod: domain.TimingMethod(req.TimingMethod),
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, occ)
}

func (s *Server) handleUpdateOccurrence(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	var req struct {
		MoveID       *string `json:"move_id"`
		ProposedName *string `json:"proposed_name"`
		StartMs      *int64  `json:"start_ms"`
		EndMs        *int64  `json:"end_ms"`
		StartText    *string `json:"start_text"`
		EndText      *string `json:"end_text"`
		Ordinal      int     `json:"ordinal"`
		Notes        string  `json:"notes"`
		State        string  `json:"state"`
		TimingMethod string  `json:"timing_method"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Invalid occurrence update payload")
		return
	}
	startMs := req.StartMs
	endMs := req.EndMs
	if startMs == nil && req.StartText != nil {
		p, err := domain.ParseTimestampMs(*req.StartText)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		startMs = p
	}
	if endMs == nil && req.EndText != nil {
		p, err := domain.ParseTimestampMs(*req.EndText)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		endMs = p
	}
	occ, err := s.store.UpdateOccurrence(r.Context(), db.OccurrenceInput{
		UserID:       sess.User.ID,
		EntryID:      r.PathValue("id"),
		OccurrenceID: r.PathValue("occId"),
		MoveID:       req.MoveID,
		ProposedName: req.ProposedName,
		StartMs:      startMs,
		EndMs:        endMs,
		Ordinal:      req.Ordinal,
		Notes:        req.Notes,
		State:        domain.OccurrenceState(req.State),
		TimingMethod: domain.TimingMethod(req.TimingMethod),
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, occ)
}

func (s *Server) handleDeleteOccurrence(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	if err := s.store.DeleteOccurrence(r.Context(), sess.User.ID, r.PathValue("id"), r.PathValue("occId")); err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- Taxonomy ---

func (s *Server) handleListMoves(w http.ResponseWriter, r *http.Request) {
	includeProposed := currentSession(r) != nil
	moves, err := s.store.ListMoves(r.Context(), r.URL.Query().Get("q"), includeProposed)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"moves": moves})
}

func (s *Server) handleCreateMove(w http.ResponseWriter, r *http.Request) {
	sess := currentSession(r)
	var req struct {
		CanonicalName string                    `json:"canonical_name"`
		Disambiguator string                    `json:"disambiguator"`
		StyleTags     []string                  `json:"style_tags"`
		Kind          string                    `json:"kind"`
		Description   string                    `json:"description"`
		Steps         []db.CombinationStepInput `json:"steps"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Invalid move payload")
		return
	}
	curation := "proposed"
	if sess.User.Role == domain.RoleAdmin {
		curation = "curated"
	}
	mv, err := s.store.CreateMove(r.Context(), db.CreateMoveInput{
		CanonicalName: req.CanonicalName,
		Disambiguator: req.Disambiguator,
		StyleTags:     req.StyleTags,
		Kind:          req.Kind,
		CurationState: curation,
		Description:   req.Description,
		CreatedBy:     &sess.User.ID,
		Steps:         req.Steps,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, mv)
}

func (s *Server) handleAddMoveAlias(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Alias     string `json:"alias"`
		Language  string `json:"language"`
		Qualifier string `json:"qualifier"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Invalid alias payload")
		return
	}
	al, err := s.store.AddMoveAlias(r.Context(), r.PathValue("id"), req.Alias, req.Language, req.Qualifier, "user")
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, al)
}

func (s *Server) handleSetCombinationSteps(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Steps []db.CombinationStepInput `json:"steps"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "Invalid combination steps payload")
		return
	}
	mv, err := s.store.SetCombinationSteps(r.Context(), r.PathValue("id"), req.Steps)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mv)
}

package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/johandry/vueltario/internal/analysis"
	"github.com/johandry/vueltario/internal/config"
	"github.com/johandry/vueltario/internal/db"
	"github.com/johandry/vueltario/internal/domain"
	"github.com/johandry/vueltario/internal/media"
	"github.com/johandry/vueltario/internal/providers"
	"github.com/johandry/vueltario/internal/publishing"
	"github.com/johandry/vueltario/internal/worker"
	"github.com/johandry/vueltario/web"
)

const SessionCookieName = "vueltario_session"

type ctxKey int

const (
	ctxKeySession ctxKey = iota
	ctxKeyViaCookie
	ctxKeyCorrelationID
)

type Server struct {
	cfg        config.Config
	store      *db.Store
	providers  *providers.Registry
	mediaSvc   *media.Service
	analyzer   *analysis.Service
	pubSvc     *publishing.Service
	hub        *worker.EventHub
	limiter    *rateLimiter
	mux        *http.ServeMux
}

func NewServer(
	cfg config.Config,
	store *db.Store,
	reg *providers.Registry,
	mediaSvc *media.Service,
	analyzer *analysis.Service,
	pubSvc *publishing.Service,
	hub *worker.EventHub,
) *Server {
	s := &Server{
		cfg:       cfg,
		store:     store,
		providers: reg,
		mediaSvc:  mediaSvc,
		analyzer:  analyzer,
		pubSvc:    pubSvc,
		hub:       hub,
		limiter:   newRateLimiter(180, time.Minute),
		mux:       http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler {
	return s.withSecurityHeaders(s.withCorrelationID(s.withAuth(s.mux)))
}

func (s *Server) routes() {
	// Health & Phase 0 Capability Contracts
	s.mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/v1/capabilities", s.handleCapabilities)

	// Authentication & Account Management
	s.mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/v1/auth/logout", s.requireMutatingAuth(s.handleLogout))
	s.mux.HandleFunc("POST /api/v1/auth/register", s.handleRegister)
	s.mux.HandleFunc("GET /api/v1/auth/me", s.handleMe)
	s.mux.HandleFunc("PATCH /api/v1/auth/me", s.requireMutatingAuth(s.handleUpdateMe))
	s.mux.HandleFunc("DELETE /api/v1/auth/me", s.requireMutatingAuth(s.handleDeleteMe))
	s.mux.HandleFunc("POST /api/v1/admin/invites", s.requireAdminMutating(s.handleCreateInvite))

	// Personal Catalog & Occurrences
	s.mux.HandleFunc("POST /api/v1/entries", s.requireMutatingAuth(s.handleSaveEntry))
	s.mux.HandleFunc("POST /api/v1/entries/batch", s.requireMutatingAuth(s.handleBatchSaveEntries))
	s.mux.HandleFunc("GET /api/v1/entries", s.requireAuth(s.handleListEntries))
	s.mux.HandleFunc("GET /api/v1/entries/{id}", s.requireAuth(s.handleGetEntry))
	s.mux.HandleFunc("PATCH /api/v1/entries/{id}", s.requireMutatingAuth(s.handleUpdateEntry))
	s.mux.HandleFunc("DELETE /api/v1/entries/{id}", s.requireMutatingAuth(s.handleDeleteEntry))

	s.mux.HandleFunc("POST /api/v1/entries/{id}/occurrences", s.requireMutatingAuth(s.handleAddOccurrence))
	s.mux.HandleFunc("PATCH /api/v1/entries/{id}/occurrences/{occId}", s.requireMutatingAuth(s.handleUpdateOccurrence))
	s.mux.HandleFunc("DELETE /api/v1/entries/{id}/occurrences/{occId}", s.requireMutatingAuth(s.handleDeleteOccurrence))

	// Enrichment, Evidence & Suggestions
	s.mux.HandleFunc("POST /api/v1/entries/{id}/analysis", s.requireMutatingAuth(s.handleTriggerAnalysis))
	s.mux.HandleFunc("POST /api/v1/entries/{id}/suggestions/{suggId}/decision", s.requireMutatingAuth(s.handleDecideSuggestion))
	s.mux.HandleFunc("GET /api/v1/jobs/{id}", s.requireAuth(s.handleGetJob))
	s.mux.HandleFunc("GET /api/v1/events", s.requireAuth(s.handleSSE))

	// Taxonomy (Moves, Aliases, Combinations)
	s.mux.HandleFunc("GET /api/v1/moves", s.handleListMoves)
	s.mux.HandleFunc("POST /api/v1/moves", s.requireMutatingAuth(s.handleCreateMove))
	s.mux.HandleFunc("POST /api/v1/moves/{id}/aliases", s.requireMutatingAuth(s.handleAddMoveAlias))
	s.mux.HandleFunc("PUT /api/v1/moves/{id}/steps", s.requireMutatingAuth(s.handleSetCombinationSteps))

	// Scoped Playback Descriptor & Authorized Media / Rights
	s.mux.HandleFunc("GET /api/v1/sources/{id}/playback", s.handleSourcePlayback)
	s.mux.HandleFunc("POST /api/v1/assets/imports", s.requireMutatingAuth(s.handleImportAsset))
	s.mux.HandleFunc("POST /api/v1/assets/{id}/clips", s.requireMutatingAuth(s.handleDeriveClip))
	s.mux.HandleFunc("GET /api/v1/assets/{id}/stream", s.handleStreamAsset)
	s.mux.HandleFunc("DELETE /api/v1/assets/{id}", s.requireMutatingAuth(s.handleDeleteAsset))
	s.mux.HandleFunc("GET /api/v1/staging/{token}/media.mp4", s.handleStreamStagedMedia)

	s.mux.HandleFunc("GET /api/v1/sources/{id}/rights", s.requireAdmin(s.handleListRights))
	s.mux.HandleFunc("POST /api/v1/admin/rights", s.requireAdminMutating(s.handleCreateRights))
	s.mux.HandleFunc("POST /api/v1/admin/rights/{id}/revoke", s.requireAdminMutating(s.handleRevokeRights))

	// Community Catalog, Submissions, Corrections & Reports
	s.mux.HandleFunc("POST /api/v1/entries/{id}/submissions", s.requireMutatingAuth(s.handleSubmitToCommunity))
	s.mux.HandleFunc("GET /api/v1/public/entries", s.handleListPublicEntries)
	s.mux.HandleFunc("GET /api/v1/public/entries/{id}", s.handleGetPublicEntry)
	s.mux.HandleFunc("POST /api/v1/public/entries/{id}/corrections", s.requireMutatingAuth(s.handleCreatePublicCorrection))
	s.mux.HandleFunc("POST /api/v1/public/entries/{id}/reports", s.handleCreateReport)

	s.mux.HandleFunc("GET /api/v1/admin/submissions", s.requireAdmin(s.handleAdminListSubmissions))
	s.mux.HandleFunc("POST /api/v1/admin/submissions/{id}/decision", s.requireAdminMutating(s.handleAdminDecideSubmission))
	s.mux.HandleFunc("POST /api/v1/admin/public/{id}/hide", s.requireAdminMutating(s.handleAdminHidePublicRevision))

	// Instagram Publishing Workflow (@cubansocial.sd)
	s.mux.HandleFunc("GET /api/v1/admin/publications", s.requireAdmin(s.handleListPublications))
	s.mux.HandleFunc("GET /api/v1/admin/publications/{id}", s.requireAdmin(s.handleGetPublication))
	s.mux.HandleFunc("POST /api/v1/admin/publications", s.requireAdminMutating(s.handleCreatePublication))
	s.mux.HandleFunc("PATCH /api/v1/admin/publications/{id}", s.requireAdminMutating(s.handleUpdatePublication))
	s.mux.HandleFunc("POST /api/v1/admin/publications/{id}/approve", s.requireAdminMutating(s.handleApprovePublication))
	s.mux.HandleFunc("POST /api/v1/admin/publications/{id}/execute", s.requireAdminMutating(s.handleExecutePublication))
	s.mux.HandleFunc("POST /api/v1/admin/publications/{id}/reconcile", s.requireAdminMutating(s.handleReconcilePublication))

	// Sync, Portability Export & Backup/Restore
	s.mux.HandleFunc("GET /api/v1/sync", s.requireAuth(s.handleSync))
	s.mux.HandleFunc("GET /api/v1/export", s.requireAuth(s.handleExportPersonal))
	s.mux.HandleFunc("GET /api/v1/admin/backup", s.requireAdmin(s.handleAdminBackup))
	s.mux.HandleFunc("POST /api/v1/admin/restore", s.requireAdminMutating(s.handleAdminRestore))

	// Embedded responsive Vanilla JS SPA & static assets
	staticHandler := http.FileServer(http.FS(web.Assets))
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusNotFound, "not_found", "API route not found")
			return
		}
		if r.URL.Path != "/" {
			cleanPath := strings.TrimPrefix(r.URL.Path, "/")
			if _, err := fs.Stat(web.Assets, cleanPath); err == nil {
				staticHandler.ServeHTTP(w, r)
				return
			}
		}
		indexBytes, err := fs.ReadFile(web.Assets, "index.html")
		if err != nil {
			http.Error(w, "frontend unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexBytes)
	})
}

// --- Middleware ---

func (s *Server) withCorrelationID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cid := strings.TrimSpace(r.Header.Get("X-Correlation-ID"))
		if cid == "" || len(cid) > 64 {
			cid = uuid.NewString()
		}
		w.Header().Set("X-Correlation-ID", cid)
		ctx := context.WithValue(r.Context(), ctxKeyCorrelationID, cid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	csp := strings.Join([]string{
		"default-src 'self'",
		"script-src 'self'",
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data: https:",
		"media-src 'self' blob:",
		"connect-src 'self'",
		"frame-src https://www.youtube.com https://www.youtube-nocookie.com https://www.tiktok.com https://www.facebook.com https://www.instagram.com",
		"object-src 'none'",
		"base-uri 'self'",
		"frame-ancestors 'none'",
	}, "; ")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		// 1. Check Authorization: Bearer <token> (CLI / API clients)
		if authHdr := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(strings.ToLower(authHdr), "bearer ") {
			tok := strings.TrimSpace(authHdr[7:])
			if sess, err := s.store.GetSessionByToken(ctx, tok); err == nil {
				ctx = context.WithValue(ctx, ctxKeySession, sess)
				ctx = context.WithValue(ctx, ctxKeyViaCookie, false)
			}
		} else if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
			// 2. Check HttpOnly session cookie
			if sess, err := s.store.GetSessionByToken(ctx, c.Value); err == nil {
				ctx = context.WithValue(ctx, ctxKeySession, sess)
				ctx = context.WithValue(ctx, ctxKeyViaCookie, true)
			}
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if currentSession(r) == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
			return
		}
		next(w, r)
	}
}

func (s *Server) requireMutatingAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := currentSession(r)
		if sess == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
			return
		}
		if !s.limiter.Allow("user:" + sess.User.ID) {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "rate_limited", "Too many requests; please slow down")
			return
		}
		if isViaCookie(r) {
			supplied := strings.TrimSpace(r.Header.Get("X-CSRF-Token"))
			if supplied == "" || subtle.ConstantTimeCompare([]byte(supplied), []byte(sess.CSRFToken)) != 1 {
				writeError(w, http.StatusForbidden, "csrf_invalid", "Missing or invalid X-CSRF-Token header")
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		sess := currentSession(r)
		if sess.User.Role != domain.RoleAdmin {
			writeError(w, http.StatusForbidden, "forbidden", "Administrator role required")
			return
		}
		next(w, r)
	})
}

func (s *Server) requireAdminMutating(next http.HandlerFunc) http.HandlerFunc {
	return s.requireMutatingAuth(func(w http.ResponseWriter, r *http.Request) {
		sess := currentSession(r)
		if sess.User.Role != domain.RoleAdmin {
			writeError(w, http.StatusForbidden, "forbidden", "Administrator role required")
			return
		}
		next(w, r)
	})
}

func currentSession(r *http.Request) *db.AuthSession {
	if v, ok := r.Context().Value(ctxKeySession).(*db.AuthSession); ok {
		return v
	}
	return nil
}

func isViaCookie(r *http.Request) bool {
	v, _ := r.Context().Value(ctxKeyViaCookie).(bool)
	return v
}

func correlationID(r *http.Request) string {
	v, _ := r.Context().Value(ctxKeyCorrelationID).(string)
	return v
}

// --- JSON & Error Helpers ---

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 2*1024*1024))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}

func writeDomainError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, domain.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
	case errors.Is(err, domain.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, domain.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, domain.ErrUnsupportedHost):
		writeError(w, http.StatusUnprocessableEntity, "unsupported_host", err.Error())
	case errors.Is(err, domain.ErrValidation):
		writeError(w, http.StatusBadRequest, "validation_error", err.Error())
	case errors.Is(err, domain.ErrQuotaExceeded), errors.Is(err, domain.ErrRateLimited):
		writeError(w, http.StatusTooManyRequests, "quota_exceeded", err.Error())
	case errors.Is(err, publishing.ErrApprovalInvalid):
		writeError(w, http.StatusConflict, "approval_invalid", err.Error())
	case errors.Is(err, publishing.ErrTokenExpired):
		writeError(w, http.StatusBadGateway, "instagram_auth_required", err.Error())
	case errors.Is(err, publishing.ErrAmbiguousOutcome):
		writeError(w, http.StatusBadGateway, "outcome_unknown", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", "Internal server error")
	}
}

func parseIfMatchRevision(r *http.Request, bodyRevision *int) int {
	if hdr := strings.TrimSpace(r.Header.Get("If-Match")); hdr != "" {
		cleaned := strings.Trim(hdr, `"`)
		cleaned = strings.TrimPrefix(cleaned, "v")
		if n, err := strconv.Atoi(cleaned); err == nil && n > 0 {
			return n
		}
	}
	if bodyRevision != nil && *bodyRevision > 0 {
		return *bodyRevision
	}
	return 0
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- Simple In-Memory Window Rate Limiter ---

type rateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	buckets map[string]*bucket
}

type bucket struct {
	count   int
	resetAt time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		limit:   limit,
		window:  window,
		buckets: make(map[string]*bucket),
	}
}

func (rl *rateLimiter) Allow(key string) bool {
	now := time.Now()
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.buckets[key]
	if !ok || now.After(b.resetAt) {
		rl.buckets[key] = &bucket{count: 1, resetAt: now.Add(rl.window)}
		return true
	}
	if b.count >= rl.limit {
		return false
	}
	b.count++
	return true
}

func formatETag(rev int) string {
	return fmt.Sprintf(`"v%d"`, rev)
}

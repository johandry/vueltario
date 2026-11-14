package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/johandry/vueltario/internal/analysis"
	"github.com/johandry/vueltario/internal/config"
	"github.com/johandry/vueltario/internal/db"
	"github.com/johandry/vueltario/internal/domain"
	"github.com/johandry/vueltario/internal/media"
	"github.com/johandry/vueltario/internal/providers"
)

// Event represents an SSE progress notification scoped to a user (or admin).
type Event struct {
	UserID     string `json:"user_id,omitempty"`
	Type       string `json:"type"`
	JobID      string `json:"job_id,omitempty"`
	TargetType string `json:"target_type,omitempty"`
	TargetID   string `json:"target_id,omitempty"`
	State      string `json:"state,omitempty"`
	Message    string `json:"message,omitempty"`
}

type EventHub struct {
	mu          sync.RWMutex
	subscribers map[string]map[chan Event]struct{}
}

func NewEventHub() *EventHub {
	return &EventHub{
		subscribers: make(map[string]map[chan Event]struct{}),
	}
}

func (h *EventHub) Subscribe(userID string) (chan Event, func()) {
	ch := make(chan Event, 16)
	h.mu.Lock()
	if h.subscribers[userID] == nil {
		h.subscribers[userID] = make(map[chan Event]struct{})
	}
	h.subscribers[userID][ch] = struct{}{}
	h.mu.Unlock()

	unsubscribe := func() {
		h.mu.Lock()
		if subs := h.subscribers[userID]; subs != nil {
			delete(subs, ch)
			if len(subs) == 0 {
				delete(h.subscribers, userID)
			}
		}
		h.mu.Unlock()
	}
	return ch, unsubscribe
}

func (h *EventHub) Broadcast(ev Event) {
	if h == nil || ev.UserID == "" {
		return
	}
	h.mu.RLock()
	subs := h.subscribers[ev.UserID]
	for ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
	h.mu.RUnlock()
}

type Runner struct {
	ID            string
	cfg           config.Config
	store         *db.Store
	providers     *providers.Registry
	analyzer      *analysis.Service
	mediaSvc      *media.Service
	hub           *EventHub
	LeaseDuration time.Duration
	PollInterval  time.Duration
}

func NewRunner(cfg config.Config, store *db.Store, reg *providers.Registry, analyzer *analysis.Service, mediaSvc *media.Service, hub *EventHub) *Runner {
	return &Runner{
		ID:            "worker-" + uuid.NewString()[:8],
		cfg:           cfg,
		store:         store,
		providers:     reg,
		analyzer:      analyzer,
		mediaSvc:      mediaSvc,
		hub:           hub,
		LeaseDuration: 45 * time.Second,
		PollInterval:  1500 * time.Millisecond,
	}
}

func (r *Runner) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.PollInterval)
	defer ticker.Stop()

	for {
		processed, err := r.ProcessOne(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("[worker %s] error claiming/processing job: %v", r.ID, err)
		}
		if processed {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// ProcessOne claims and executes a single runnable job, returning true if a job was processed.
func (r *Runner) ProcessOne(ctx context.Context) (bool, error) {
	job, err := r.store.ClaimNextJob(ctx, r.ID, r.LeaseDuration)
	if err != nil {
		return false, err
	}
	if job == nil {
		return false, nil
	}

	ownerID := ""
	if job.OwnerUserID != nil {
		ownerID = *job.OwnerUserID
	}
	r.hub.Broadcast(Event{
		UserID:     ownerID,
		Type:       "job.running",
		JobID:      job.ID,
		TargetType: job.TargetType,
		TargetID:   job.TargetID,
		State:      string(domain.JobRunning),
	})

	jobCtx, cancel := context.WithTimeout(ctx, r.LeaseDuration-5*time.Second)
	defer cancel()

	var retryAfter time.Duration
	execErr := r.executeJob(jobCtx, *job, &retryAfter)
	if execErr == nil {
		if err := r.store.CompleteJob(ctx, job.ID); err != nil {
			return true, err
		}
		r.hub.Broadcast(Event{
			UserID:     ownerID,
			Type:       "job.succeeded",
			JobID:      job.ID,
			TargetType: job.TargetType,
			TargetID:   job.TargetID,
			State:      string(domain.JobSucceeded),
		})
		return true, nil
	}

	nextState, sanitizedMsg, backoff := classifyJobFailure(*job, execErr, retryAfter)
	finalState, fErr := r.store.FailOrRetryJob(ctx, *job, nextState, sanitizedMsg, backoff)
	if fErr != nil {
		return true, fErr
	}
	r.hub.Broadcast(Event{
		UserID:     ownerID,
		Type:       "job.updated",
		JobID:      job.ID,
		TargetType: job.TargetType,
		TargetID:   job.TargetID,
		State:      string(finalState),
		Message:    sanitizedMsg,
	})
	return true, nil
}

func (r *Runner) executeJob(ctx context.Context, job domain.ProcessingJob, retryAfter *time.Duration) error {
	switch job.JobType {
	case "enrich_source":
		var payload struct {
			EntryID  string `json:"entry_id"`
			SourceID string `json:"source_id"`
		}
		_ = json.Unmarshal([]byte(job.PayloadJSON), &payload)
		sourceID := payload.SourceID
		if sourceID == "" {
			sourceID = job.TargetID
		}
		src, err := r.store.GetSource(ctx, sourceID)
		if err != nil {
			return err
		}
		adapter, ok := r.providers.Get(src.Platform)
		if !ok {
			return providers.ErrCapabilityUnsupported
		}
		meta, err := adapter.FetchMetadata(ctx, *src)
		if err != nil {
			if meta.RetryAfter > 0 {
				*retryAfter = meta.RetryAfter
			}
			return err
		}
		if _, err := r.store.ApplyFetchedSourceMetadata(ctx, src.ID, meta); err != nil {
			return err
		}
		// Opportunistically generate assisted suggestions if enabled and owner hasn't opted out
		if r.cfg.AnalysisEnabled && payload.EntryID != "" && job.OwnerUserID != nil {
			if user, uErr := r.store.GetUserByID(ctx, *job.OwnerUserID); uErr == nil && !user.AnalysisOptOut {
				_, _ = r.analyzer.AnalyzeEntry(ctx, *user, payload.EntryID, false)
			}
		}
		return nil

	case "analyze_entry":
		if job.OwnerUserID == nil {
			return domain.ErrUnauthorized
		}
		user, err := r.store.GetUserByID(ctx, *job.OwnerUserID)
		if err != nil {
			return err
		}
		var payload struct {
			EntryID string `json:"entry_id"`
			Force   bool   `json:"force"`
		}
		_ = json.Unmarshal([]byte(job.PayloadJSON), &payload)
		entryID := payload.EntryID
		if entryID == "" {
			entryID = job.TargetID
		}
		_, err = r.analyzer.AnalyzeEntry(ctx, *user, entryID, payload.Force)
		return err

	case "orphan_scan":
		if r.mediaSvc != nil {
			_, _, err := r.mediaSvc.ScanOrphans(ctx)
			return err
		}
		return nil

	default:
		return fmt.Errorf("%w: unknown job type %s", providers.ErrCapabilityUnsupported, job.JobType)
	}
}

func classifyJobFailure(job domain.ProcessingJob, err error, retryAfter time.Duration) (domain.JobState, string, time.Duration) {
	switch {
	case errors.Is(err, providers.ErrCapabilityUnsupported):
		return domain.JobUnsupported, "Automated metadata/analysis is not supported for this source without authorized import or manual annotation.", 0
	case errors.Is(err, providers.ErrProviderAuthRequired), errors.Is(err, domain.ErrUnauthorized), errors.Is(err, domain.ErrForbidden):
		return domain.JobBlockedAuth, "Provider credentials or user permissions required; manual cataloging remains available.", 0
	case errors.Is(err, domain.ErrValidation):
		return domain.JobNeedsInput, err.Error(), 0
	case errors.Is(err, domain.ErrQuotaExceeded):
		return domain.JobFailed, "Daily analysis budget or quota ceiling reached; manual cataloging unaffected.", 0
	default:
		// Transient failure -> bounded exponential backoff with jitter + Retry-After support
		base := time.Duration(1<<minInt(job.Attempts, 5)) * time.Second
		jitter := time.Duration(rand.IntN(1000)) * time.Millisecond
		backoff := base + jitter
		if retryAfter > backoff {
			backoff = retryAfter
		}
		if backoff > 5*time.Minute {
			backoff = 5 * time.Minute
		}
		return domain.JobRetryWait, sanitizeJobError(err), backoff
	}
}

func sanitizeJobError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > 220 {
		msg = msg[:220] + "..."
	}
	return msg
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

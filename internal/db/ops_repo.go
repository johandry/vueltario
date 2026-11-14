package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/johandry/vueltario/internal/domain"
	"github.com/johandry/vueltario/internal/providers"
)

// --- Background Processing Jobs ---

type enqueueJobParams struct {
	OwnerUserID    *string
	JobType        string
	TargetType     string
	TargetID       string
	TargetRevision int
	Payload        any
	IdempotencyKey string
	CorrelationID  string
}

func (s *Store) EnqueueJob(ctx context.Context, ownerUserID *string, jobType, targetType, targetID string, targetRev int, payload any, idempotencyKey, correlationID string) (*domain.ProcessingJob, error) {
	var job *domain.ProcessingJob
	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		job, err = s.enqueueJobTx(ctx, tx, enqueueJobParams{
			OwnerUserID:    ownerUserID,
			JobType:        jobType,
			TargetType:     targetType,
			TargetID:       targetID,
			TargetRevision: targetRev,
			Payload:        payload,
			IdempotencyKey: idempotencyKey,
			CorrelationID:  correlationID,
		})
		return err
	})
	return job, err
}

func (s *Store) enqueueJobTx(ctx context.Context, tx *sql.Tx, p enqueueJobParams) (*domain.ProcessingJob, error) {
	if p.IdempotencyKey == "" {
		p.IdempotencyKey = uuid.NewString()
	}
	// Check existing by idempotency key
	existing, err := s.getJobByIdempotencyTx(ctx, tx, p.IdempotencyKey)
	if err == nil && existing != nil {
		return existing, nil
	}

	now := time.Now().UTC()
	job := &domain.ProcessingJob{
		ID:             uuid.NewString(),
		OwnerUserID:    p.OwnerUserID,
		JobType:        p.JobType,
		TargetType:     p.TargetType,
		TargetID:       p.TargetID,
		TargetRevision: p.TargetRevision,
		PayloadJSON:    marshalJSONString(p.Payload, "{}"),
		State:          domain.JobQueued,
		Attempts:       0,
		MaxAttempts:    5,
		NextAttemptAt:  now,
		IdempotencyKey: p.IdempotencyKey,
		CorrelationID:  p.CorrelationID,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	q := s.rebind(`INSERT INTO processing_jobs (
		id, owner_user_id, job_type, target_type, target_id, target_revision, payload_json,
		state, attempts, max_attempts, next_attempt_at, sanitized_error, idempotency_key, correlation_id, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, 'queued', 0, 5, ?, '', ?, ?, ?, ?)`)
	if _, err := tx.ExecContext(ctx, q,
		job.ID, job.OwnerUserID, job.JobType, job.TargetType, job.TargetID, job.TargetRevision, job.PayloadJSON,
		s.timeArg(now), job.IdempotencyKey, job.CorrelationID, s.timeArg(now), s.timeArg(now),
	); err != nil {
		return nil, err
	}
	return job, nil
}

func (s *Store) getJobByIdempotencyTx(ctx context.Context, qx queryExecer, key string) (*domain.ProcessingJob, error) {
	q := s.rebind(`SELECT id, owner_user_id, job_type, target_type, target_id, target_revision, payload_json,
		state, attempts, max_attempts, lease_owner, lease_expires_at, next_attempt_at, sanitized_error,
		idempotency_key, correlation_id, created_at, updated_at
		FROM processing_jobs WHERE idempotency_key = ?`)
	return s.scanJob(qx.QueryRowContext(ctx, q, key))
}

func (s *Store) getLatestJobForTargetTx(ctx context.Context, qx queryExecer, targetType, targetID string) (*domain.ProcessingJob, error) {
	q := s.rebind(`SELECT id, owner_user_id, job_type, target_type, target_id, target_revision, payload_json,
		state, attempts, max_attempts, lease_owner, lease_expires_at, next_attempt_at, sanitized_error,
		idempotency_key, correlation_id, created_at, updated_at
		FROM processing_jobs WHERE target_type = ? AND target_id = ? ORDER BY created_at DESC LIMIT 1`)
	return s.scanJob(qx.QueryRowContext(ctx, q, targetType, targetID))
}

func (s *Store) GetJobScoped(ctx context.Context, user domain.User, jobID string) (*domain.ProcessingJob, error) {
	q := s.rebind(`SELECT id, owner_user_id, job_type, target_type, target_id, target_revision, payload_json,
		state, attempts, max_attempts, lease_owner, lease_expires_at, next_attempt_at, sanitized_error,
		idempotency_key, correlation_id, created_at, updated_at
		FROM processing_jobs WHERE id = ?`)
	job, err := s.scanJob(s.db.QueryRowContext(ctx, q, jobID))
	if err != nil {
		return nil, err
	}
	if user.Role != domain.RoleAdmin {
		if job.OwnerUserID == nil || *job.OwnerUserID != user.ID {
			return nil, domain.ErrForbidden
		}
	}
	return job, nil
}

func (s *Store) scanJob(row *sql.Row) (*domain.ProcessingJob, error) {
	var (
		j                                       domain.ProcessingJob
		ownerID, leaseOwner                     sql.NullString
		stateStr                                string
		leaseExpRaw, nextRaw, crRaw, updatedRaw any
	)
	if err := row.Scan(
		&j.ID, &ownerID, &j.JobType, &j.TargetType, &j.TargetID, &j.TargetRevision, &j.PayloadJSON,
		&stateStr, &j.Attempts, &j.MaxAttempts, &leaseOwner, &leaseExpRaw, &nextRaw, &j.SanitizedError,
		&j.IdempotencyKey, &j.CorrelationID, &crRaw, &updatedRaw,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	if ownerID.Valid {
		j.OwnerUserID = &ownerID.String
	}
	if leaseOwner.Valid {
		j.LeaseOwner = &leaseOwner.String
	}
	j.State = domain.JobState(stateStr)
	j.LeaseExpiresAt, _ = parseNullDBTime(leaseExpRaw)
	j.NextAttemptAt, _ = parseDBTime(nextRaw)
	j.CreatedAt, _ = parseDBTime(crRaw)
	j.UpdatedAt, _ = parseDBTime(updatedRaw)
	return &j, nil
}

// ClaimNextJob atomically claims a runnable job OR recovers a crashed worker's expired lease.
func (s *Store) ClaimNextJob(ctx context.Context, workerID string, leaseDuration time.Duration) (*domain.ProcessingJob, error) {
	now := time.Now().UTC()
	leaseExp := now.Add(leaseDuration)
	var claimed *domain.ProcessingJob

	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		var selectQuery string
		if s.dialect == "postgres" {
			selectQuery = `SELECT id FROM processing_jobs
				WHERE (state IN ('queued', 'retry_wait') AND next_attempt_at <= $1)
				   OR (state = 'running' AND lease_expires_at IS NOT NULL AND lease_expires_at <= $1)
				ORDER BY next_attempt_at ASC
				FOR UPDATE SKIP LOCKED
				LIMIT 1`
		} else {
			selectQuery = `SELECT id FROM processing_jobs
				WHERE (state IN ('queued', 'retry_wait') AND next_attempt_at <= ?)
				   OR (state = 'running' AND lease_expires_at IS NOT NULL AND lease_expires_at <= ?)
				ORDER BY next_attempt_at ASC
				LIMIT 1`
		}

		var jobID string
		var scanErr error
		if s.dialect == "postgres" {
			scanErr = tx.QueryRowContext(ctx, selectQuery, s.timeArg(now)).Scan(&jobID)
		} else {
			scanErr = tx.QueryRowContext(ctx, selectQuery, s.timeArg(now), s.timeArg(now)).Scan(&jobID)
		}
		if errors.Is(scanErr, sql.ErrNoRows) {
			return nil
		}
		if scanErr != nil {
			return scanErr
		}

		qUp := s.rebind(`UPDATE processing_jobs
			SET state = 'running', attempts = attempts + 1, lease_owner = ?, lease_expires_at = ?, updated_at = ?
			WHERE id = ?`)
		if _, err := tx.ExecContext(ctx, qUp, workerID, s.timeArg(leaseExp), s.timeArg(now), jobID); err != nil {
			return err
		}

		qGet := s.rebind(`SELECT id, owner_user_id, job_type, target_type, target_id, target_revision, payload_json,
			state, attempts, max_attempts, lease_owner, lease_expires_at, next_attempt_at, sanitized_error,
			idempotency_key, correlation_id, created_at, updated_at
			FROM processing_jobs WHERE id = ?`)
		j, err := s.scanJob(tx.QueryRowContext(ctx, qGet, jobID))
		if err != nil {
			return err
		}
		claimed = j
		return nil
	})
	return claimed, err
}

func (s *Store) CompleteJob(ctx context.Context, jobID string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, s.rebind(`UPDATE processing_jobs
		SET state = 'succeeded', lease_owner = NULL, lease_expires_at = NULL, sanitized_error = '', updated_at = ?
		WHERE id = ?`), s.timeArg(now), jobID)
	return err
}

func (s *Store) FailOrRetryJob(ctx context.Context, job domain.ProcessingJob, terminalState domain.JobState, sanitizedErr string, backoff time.Duration) (domain.JobState, error) {
	now := time.Now().UTC()
	nextState := terminalState
	nextAttempt := now
	if terminalState == domain.JobRetryWait {
		if job.Attempts >= job.MaxAttempts {
			nextState = domain.JobFailed
		} else {
			nextAttempt = now.Add(backoff)
		}
	}
	_, err := s.db.ExecContext(ctx, s.rebind(`UPDATE processing_jobs
		SET state = ?, lease_owner = NULL, lease_expires_at = NULL, next_attempt_at = ?, sanitized_error = ?, updated_at = ?
		WHERE id = ?`), string(nextState), s.timeArg(nextAttempt), sanitizedErr, s.timeArg(now), job.ID)
	return nextState, err
}

// ApplyFetchedSourceMetadata updates factual source metadata without overwriting user personal overrides.
func (s *Store) ApplyFetchedSourceMetadata(ctx context.Context, sourceID string, meta providers.FetchedMetadata) (*domain.Source, error) {
	now := time.Now().UTC()
	var updated *domain.Source
	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		cur, err := s.getSourceTx(ctx, tx, sourceID)
		if err != nil {
			return err
		}
		title := cur.Title
		if meta.Title != nil && *meta.Title != "" {
			title = meta.Title
		}
		desc := cur.Description
		if meta.Description != nil && *meta.Description != "" {
			desc = meta.Description
		}
		dur := cur.DurationMs
		if meta.DurationMs != nil && *meta.DurationMs > 0 {
			dur = meta.DurationMs
		}
		avail := cur.AvailabilityState
		if meta.AvailabilityState != "" {
			avail = meta.AvailabilityState
		}
		prov := cur.Provenance
		if meta.Provenance != "" {
			prov = meta.Provenance
		}

		if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE sources SET
			title = ?, description = ?, duration_ms = ?, availability_state = ?, embed_eligible = ?,
			provenance = ?, metadata_version = metadata_version + 1, fetched_at = ?, updated_at = ?
			WHERE id = ?`),
			title, desc, dur, string(avail), s.boolArg(meta.EmbedEligible),
			prov, s.timeArg(now), s.timeArg(now), sourceID,
		); err != nil {
			return err
		}

		if meta.CreatorName != nil && *meta.CreatorName != "" {
			// Platform metadata identifies the uploader channel/account; never automatically label original choreographer (Section 8).
			profileURL := ""
			if meta.CreatorProfileURL != nil {
				profileURL = *meta.CreatorProfileURL
			}
			handle := ""
			if meta.CreatorHandle != nil {
				handle = *meta.CreatorHandle
			}
			if err := s.upsertSourceCreatorTx(ctx, tx, *cur, CreatorAttributionInput{
				DisplayName: *meta.CreatorName,
				Role:        "uploader",
				Handle:      handle,
				ProfileURL:  profileURL,
				Verified:    false,
			}, prov); err != nil {
				return err
			}
		}

		updated, err = s.getSourceTx(ctx, tx, sourceID)
		return err
	})
	return updated, err
}

// --- Evidence, Suggestions, and Budget Ceiling (ANA-01..03, Section 14) ---

func (s *Store) TryConsumeAnalysisBudget(ctx context.Context, dailyCeiling int) error {
	if dailyCeiling <= 0 {
		return domain.ErrQuotaExceeded
	}
	today := time.Now().UTC().Format("2006-01-02")
	now := time.Now().UTC()
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		var count int
		err := tx.QueryRowContext(ctx, s.rebind(`SELECT runs_count FROM analysis_usage_daily WHERE usage_date = ?`), today).Scan(&count)
		if errors.Is(err, sql.ErrNoRows) {
			_, err = tx.ExecContext(ctx, s.rebind(`INSERT INTO analysis_usage_daily (usage_date, runs_count, updated_at) VALUES (?, 1, ?)`), today, s.timeArg(now))
			return err
		}
		if err != nil {
			return err
		}
		if count >= dailyCeiling {
			return fmt.Errorf("%w: daily analysis ceiling (%d) reached", domain.ErrQuotaExceeded, dailyCeiling)
		}
		_, err = tx.ExecContext(ctx, s.rebind(`UPDATE analysis_usage_daily SET runs_count = runs_count + 1, updated_at = ? WHERE usage_date = ?`), s.timeArg(now), today)
		return err
	})
}

func (s *Store) listEvidenceScopedTx(ctx context.Context, qx queryExecer, sourceID, userID string) ([]domain.Evidence, error) {
	q := s.rebind(`SELECT id, source_id, owner_user_id, kind, original_ref, excerpt, observed_start_ms, observed_end_ms,
		is_announcement_ts, author_role, access_scope, retrieved_at, retention_deadline, created_at
		FROM evidence
		WHERE source_id = ? AND (access_scope = 'public_factual' OR (access_scope = 'private_user' AND owner_user_id = ?))
		ORDER BY created_at ASC`)
	rows, err := qx.QueryContext(ctx, q, sourceID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Evidence{}
	for rows.Next() {
		var ev domain.Evidence
		var owner sql.NullString
		var sMs, eMs sql.NullInt64
		var annRaw, retRaw, dlRaw, crRaw any
		if err := rows.Scan(&ev.ID, &ev.SourceID, &owner, &ev.Kind, &ev.OriginalRef, &ev.Excerpt, &sMs, &eMs,
			&annRaw, &ev.AuthorRole, &ev.AccessScope, &retRaw, &dlRaw, &crRaw); err != nil {
			return nil, err
		}
		if owner.Valid {
			ev.OwnerUserID = &owner.String
		}
		if sMs.Valid {
			ev.ObservedStartMs = &sMs.Int64
		}
		if eMs.Valid {
			ev.ObservedEndMs = &eMs.Int64
		}
		ev.IsAnnouncementTS = parseDBBool(annRaw)
		ev.RetrievedAt, _ = parseDBTime(retRaw)
		ev.RetentionDeadline, _ = parseNullDBTime(dlRaw)
		ev.CreatedAt, _ = parseDBTime(crRaw)
		out = append(out, ev)
	}
	return out, nil
}

func (s *Store) listSuggestionsTx(ctx context.Context, qx queryExecer, entryID string) ([]domain.Suggestion, error) {
	q := s.rebind(`SELECT id, entry_id, source_revision, candidate_move_id, candidate_name, start_ms, end_ms,
		is_announcement_only, evidence_ids_json, processor_version, confidence_band, state, created_at, updated_at
		FROM suggestions WHERE entry_id = ? ORDER BY created_at ASC`)
	rows, err := qx.QueryContext(ctx, q, entryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Suggestion{}
	for rows.Next() {
		var sg domain.Suggestion
		var cmID sql.NullString
		var sMs, eMs sql.NullInt64
		var annRaw, crRaw, upRaw any
		var evJSON string
		if err := rows.Scan(&sg.ID, &sg.EntryID, &sg.SourceRevision, &cmID, &sg.CandidateName, &sMs, &eMs,
			&annRaw, &evJSON, &sg.ProcessorVersion, &sg.ConfidenceBand, &sg.State, &crRaw, &upRaw); err != nil {
			return nil, err
		}
		if cmID.Valid {
			sg.CandidateMoveID = &cmID.String
		}
		if sMs.Valid {
			sg.StartMs = &sMs.Int64
		}
		if eMs.Valid {
			sg.EndMs = &eMs.Int64
		}
		sg.IsAnnouncementOnly = parseDBBool(annRaw)
		_ = json.Unmarshal([]byte(evJSON), &sg.EvidenceIDs)
		if sg.EvidenceIDs == nil {
			sg.EvidenceIDs = []string{}
		}
		sg.CreatedAt, _ = parseDBTime(crRaw)
		sg.UpdatedAt, _ = parseDBTime(upRaw)
		out = append(out, sg)
	}
	return out, nil
}

// SaveAnalysisResults persists new evidence and suggestions while strictly preserving confirmed/rejected user decisions (ANA-02).
func (s *Store) SaveAnalysisResults(ctx context.Context, userID, entryID string, sourceRevision int, evidences []domain.Evidence, suggestions []domain.Suggestion) error {
	now := time.Now().UTC()
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		ent, err := s.getEntryTx(ctx, tx, userID, entryID)
		if err != nil {
			return err
		}

		for _, ev := range evidences {
			if ev.ID == "" {
				ev.ID = uuid.NewString()
			}
			if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO evidence (
				id, source_id, owner_user_id, kind, original_ref, excerpt, observed_start_ms, observed_end_ms,
				is_announcement_ts, author_role, access_scope, retrieved_at, retention_deadline, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
				ev.ID, ent.SourceID, ev.OwnerUserID, ev.Kind, ev.OriginalRef, ev.Excerpt, ev.ObservedStartMs, ev.ObservedEndMs,
				s.boolArg(ev.IsAnnouncementTS), ev.AuthorRole, ev.AccessScope, s.timeArg(ev.RetrievedAt),
				s.nullTimeArg(ev.RetentionDeadline), s.timeArg(now),
			); err != nil {
				return err
			}
		}

		// Build sets of moves already confirmed or rejected by the user so reanalysis NEVER reverts or duplicates them!
		decidedNames := make(map[string]bool)
		for _, occ := range ent.Occurrences {
			if occ.State == domain.OccurrenceConfirmed || occ.State == domain.OccurrenceRejected || occ.State == domain.OccurrenceCurated {
				if occ.MoveName != "" {
					decidedNames[domain.NormalizeSearchKey(occ.MoveName)] = true
				}
				if occ.ProposedName != nil {
					decidedNames[domain.NormalizeSearchKey(*occ.ProposedName)] = true
				}
			}
		}
		for _, prev := range ent.Suggestions {
			if prev.State == "accepted" || prev.State == "rejected" {
				decidedNames[domain.NormalizeSearchKey(prev.CandidateName)] = true
			}
		}

		// Clear only stale 'pending' suggestions on this entry before inserting fresh pending ones
		if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM suggestions WHERE entry_id = ? AND state = 'pending'`), ent.ID); err != nil {
			return err
		}

		for _, sg := range suggestions {
			normCand := domain.NormalizeSearchKey(sg.CandidateName)
			if decidedNames[normCand] {
				continue
			}
			if sg.ID == "" {
				sg.ID = uuid.NewString()
			}
			if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO suggestions (
				id, entry_id, source_revision, candidate_move_id, candidate_name, start_ms, end_ms,
				is_announcement_only, evidence_ids_json, processor_version, confidence_band, state, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`),
				sg.ID, ent.ID, sourceRevision, sg.CandidateMoveID, sg.CandidateName, sg.StartMs, sg.EndMs,
				s.boolArg(sg.IsAnnouncementOnly), marshalJSONString(sg.EvidenceIDs, "[]"),
				sg.ProcessorVersion, sg.ConfidenceBand, s.timeArg(now), s.timeArg(now),
			); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) DecideSuggestion(ctx context.Context, userID, entryID, suggestionID, decision string) (*domain.CatalogEntry, error) {
	if decision != "accept" && decision != "reject" {
		return nil, fmt.Errorf("%w: decision must be accept or reject", domain.ErrValidation)
	}
	now := time.Now().UTC()
	var out *domain.CatalogEntry

	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		ent, err := s.getEntryTx(ctx, tx, userID, entryID)
		if err != nil {
			return err
		}
		var target *domain.Suggestion
		for i := range ent.Suggestions {
			if ent.Suggestions[i].ID == suggestionID {
				target = &ent.Suggestions[i]
				break
			}
		}
		if target == nil {
			return domain.ErrNotFound
		}

		newState := "rejected"
		if decision == "accept" {
			newState = "accepted"
			moveID := target.CandidateMoveID
			if moveID == nil {
				mv, err := s.findOrProposeMoveTx(ctx, tx, target.CandidateName, &userID)
				if err != nil {
					return err
				}
				moveID = &mv.ID
			}
			// If start_ms is only an announcement timestamp without end_ms, leave movement boundary unknown unless valid
			var sMs, eMs *int64
			if !target.IsAnnouncementOnly && domain.ValidateOccurrenceBounds(target.StartMs, target.EndMs, ent.Source.DurationMs) == nil {
				sMs = target.StartMs
				eMs = target.EndMs
			}
			method := domain.TimingUnknown
			if sMs != nil {
				method = domain.TimingSuggested
			}
			if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO occurrences (
				id, entry_id, move_id, proposed_name, start_ms, end_ms, ordinal, notes,
				state, timing_method, evidence_ids_json, revision, created_at, updated_at
			) VALUES (?, ?, ?, NULL, ?, ?, ?, '', 'confirmed', ?, ?, 1, ?, ?)`),
				uuid.NewString(), ent.ID, moveID, sMs, eMs, len(ent.Occurrences)+1,
				string(method), marshalJSONString(target.EvidenceIDs, "[]"), s.timeArg(now), s.timeArg(now),
			); err != nil {
				return err
			}
		}

		if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE suggestions SET state = ?, updated_at = ? WHERE id = ?`), newState, s.timeArg(now), target.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE catalog_entries SET revision = revision + 1, updated_at = ? WHERE id = ?`), s.timeArg(now), ent.ID); err != nil {
			return err
		}
		out, err = s.getEntryTx(ctx, tx, userID, entryID)
		return err
	})
	return out, err
}

// --- Media Assets & Rights Records (MED-01, MED-02, Section 9, Section 13) ---

func (s *Store) CreateMediaAsset(ctx context.Context, asset domain.MediaAsset) (*domain.MediaAsset, error) {
	now := time.Now().UTC()
	if asset.ID == "" {
		asset.ID = uuid.NewString()
	}
	if asset.AccessScope == "" {
		asset.AccessScope = "private"
	}
	if asset.StorageBackend == "" {
		asset.StorageBackend = "filesystem"
	}
	asset.CreatedAt = now
	asset.UpdatedAt = now
	q := s.rebind(`INSERT INTO media_assets (
		id, source_id, owner_user_id, access_scope, asset_kind, storage_backend, storage_key, device_id,
		mime_type, byte_size, sha256_hex, duration_ms, width, height, status, derivation_parent_id,
		source_time_map_json, expires_at, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if _, err := s.db.ExecContext(ctx, q,
		asset.ID, asset.SourceID, asset.OwnerUserID, asset.AccessScope, asset.AssetKind, asset.StorageBackend,
		asset.StorageKey, asset.DeviceID, asset.MIMEType, asset.ByteSize, asset.SHA256Hex,
		asset.DurationMs, asset.Width, asset.Height, asset.Status, asset.DerivationParentID,
		marshalJSONString(asset.SourceTimeMap, "{}"), s.nullTimeArg(asset.ExpiresAt), s.timeArg(now), s.timeArg(now),
	); err != nil {
		return nil, err
	}
	asset.StreamURL = "/api/v1/assets/" + asset.ID + "/stream"
	return &asset, nil
}

func (s *Store) GetUserStorageUsageBytes(ctx context.Context, userID string) (int64, error) {
	var total sql.NullInt64
	if err := s.db.QueryRowContext(ctx, s.rebind(`SELECT COALESCE(SUM(byte_size), 0) FROM media_assets WHERE owner_user_id = ? AND deleted_at IS NULL AND status <> 'deleted'`), userID).Scan(&total); err != nil {
		return 0, err
	}
	return total.Int64, nil
}

func (s *Store) GetMediaAsset(ctx context.Context, assetID string) (*domain.MediaAsset, error) {
	q := s.rebind(`SELECT id, source_id, owner_user_id, access_scope, asset_kind, storage_backend, storage_key, device_id,
		mime_type, byte_size, sha256_hex, duration_ms, width, height, status, derivation_parent_id,
		source_time_map_json, expires_at, deleted_at, created_at, updated_at
		FROM media_assets WHERE id = ?`)
	return s.scanAsset(s.db.QueryRowContext(ctx, q, assetID))
}

func (s *Store) listAssetsScopedTx(ctx context.Context, qx queryExecer, sourceID, userID string) ([]domain.MediaAsset, error) {
	q := s.rebind(`SELECT id, source_id, owner_user_id, access_scope, asset_kind, storage_backend, storage_key, device_id,
		mime_type, byte_size, sha256_hex, duration_ms, width, height, status, derivation_parent_id,
		source_time_map_json, expires_at, deleted_at, created_at, updated_at
		FROM media_assets
		WHERE source_id = ? AND deleted_at IS NULL AND status = 'ready'
		  AND (owner_user_id = ? OR access_scope = 'public_authorized')
		ORDER BY created_at DESC`)
	rows, err := qx.QueryContext(ctx, q, sourceID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.MediaAsset{}
	for rows.Next() {
		var (
			a                             domain.MediaAsset
			devID, derivID                sql.NullString
			durMs                         sql.NullInt64
			w, h                          sql.NullInt64
			tmJSON                        string
			expRaw, delRaw, crRaw, upRaw  any
		)
		if err := rows.Scan(&a.ID, &a.SourceID, &a.OwnerUserID, &a.AccessScope, &a.AssetKind, &a.StorageBackend,
			&a.StorageKey, &devID, &a.MIMEType, &a.ByteSize, &a.SHA256Hex, &durMs, &w, &h, &a.Status,
			&derivID, &tmJSON, &expRaw, &delRaw, &crRaw, &upRaw); err != nil {
			return nil, err
		}
		if devID.Valid {
			a.DeviceID = &devID.String
		}
		if derivID.Valid {
			a.DerivationParentID = &derivID.String
		}
		if durMs.Valid {
			a.DurationMs = &durMs.Int64
		}
		if w.Valid {
			wi := int(w.Int64)
			a.Width = &wi
		}
		if h.Valid {
			hi := int(h.Int64)
			a.Height = &hi
		}
		var tm domain.SourceTimeMap
		if json.Unmarshal([]byte(tmJSON), &tm) == nil && (tm.SourceEndMs > 0 || tm.SourceStartMs > 0) {
			a.SourceTimeMap = &tm
		}
		a.ExpiresAt, _ = parseNullDBTime(expRaw)
		a.DeletedAt, _ = parseNullDBTime(delRaw)
		a.CreatedAt, _ = parseDBTime(crRaw)
		a.UpdatedAt, _ = parseDBTime(upRaw)
		a.StreamURL = "/api/v1/assets/" + a.ID + "/stream"
		out = append(out, a)
	}
	return out, nil
}

func (s *Store) scanAsset(row *sql.Row) (*domain.MediaAsset, error) {
	var (
		a                            domain.MediaAsset
		devID, derivID               sql.NullString
		durMs, w, h                  sql.NullInt64
		tmJSON                       string
		expRaw, delRaw, crRaw, upRaw any
	)
	if err := row.Scan(&a.ID, &a.SourceID, &a.OwnerUserID, &a.AccessScope, &a.AssetKind, &a.StorageBackend,
		&a.StorageKey, &devID, &a.MIMEType, &a.ByteSize, &a.SHA256Hex, &durMs, &w, &h, &a.Status,
		&derivID, &tmJSON, &expRaw, &delRaw, &crRaw, &upRaw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	if devID.Valid {
		a.DeviceID = &devID.String
	}
	if derivID.Valid {
		a.DerivationParentID = &derivID.String
	}
	if durMs.Valid {
		a.DurationMs = &durMs.Int64
	}
	if w.Valid {
		wi := int(w.Int64)
		a.Width = &wi
	}
	if h.Valid {
		hi := int(h.Int64)
		a.Height = &hi
	}
	var tm domain.SourceTimeMap
	if json.Unmarshal([]byte(tmJSON), &tm) == nil && (tm.SourceEndMs > 0 || tm.SourceStartMs > 0) {
		a.SourceTimeMap = &tm
	}
	a.ExpiresAt, _ = parseNullDBTime(expRaw)
	a.DeletedAt, _ = parseNullDBTime(delRaw)
	a.CreatedAt, _ = parseDBTime(crRaw)
	a.UpdatedAt, _ = parseDBTime(upRaw)
	if a.Status == "ready" && a.DeletedAt == nil {
		a.StreamURL = "/api/v1/assets/" + a.ID + "/stream"
	}
	return &a, nil
}

// RevokeMediaAsset immediately revokes access; returns whether other active assets still reference the same storage_key.
func (s *Store) RevokeMediaAsset(ctx context.Context, user domain.User, assetID string) (string, bool, error) {
	asset, err := s.GetMediaAsset(ctx, assetID)
	if err != nil {
		return "", false, err
	}
	if user.Role != domain.RoleAdmin && asset.OwnerUserID != user.ID {
		return "", false, domain.ErrForbidden
	}
	now := time.Now().UTC()
	var otherRefs int
	err = s.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE media_assets SET status = 'deleted', deleted_at = ?, updated_at = ? WHERE id = ?`),
			s.timeArg(now), s.timeArg(now), assetID); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM media_assets WHERE storage_key = ? AND id <> ? AND deleted_at IS NULL AND status <> 'deleted'`),
			asset.StorageKey, assetID).Scan(&otherRefs)
	})
	return asset.StorageKey, otherRefs == 0, err
}

func (s *Store) AllActiveStorageKeys(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT storage_key FROM media_assets WHERE deleted_at IS NULL AND status <> 'deleted'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := make(map[string]bool)
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err == nil {
			keys[k] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return keys, nil
}

func (s *Store) CreateRightsRecord(ctx context.Context, r domain.RightsRecord) (*domain.RightsRecord, error) {
	if strings.TrimSpace(r.Grantor) == "" || strings.TrimSpace(r.Basis) == "" {
		return nil, fmt.Errorf("%w: grantor and basis are required for a rights record", domain.ErrValidation)
	}
	now := time.Now().UTC()
	r.ID = uuid.NewString()
	r.CreatedAt = now
	r.UpdatedAt = now
	q := s.rebind(`INSERT INTO rights_records (
		id, source_id, asset_id, grantor, basis, evidence_ref, video_cleared, music_cleared,
		authorized_uses_json, authorized_platforms_json, attribution_conditions, expires_at, reviewer_id, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if _, err := s.db.ExecContext(ctx, q,
		r.ID, r.SourceID, r.AssetID, strings.TrimSpace(r.Grantor), strings.TrimSpace(r.Basis), strings.TrimSpace(r.EvidenceRef),
		s.boolArg(r.VideoCleared), s.boolArg(r.MusicCleared),
		marshalJSONString(r.AuthorizedUses, "[]"), marshalJSONString(r.AuthorizedPlatforms, "[]"),
		strings.TrimSpace(r.AttributionConditions), s.nullTimeArg(r.ExpiresAt), r.ReviewerID, s.timeArg(now), s.timeArg(now),
	); err != nil {
		return nil, err
	}
	_ = s.RecordAudit(ctx, &r.ReviewerID, string(domain.RoleAdmin), "rights.recorded", "source", r.SourceID, 1, r.Basis, nil, "")
	return &r, nil
}

func (s *Store) ListRightsForSource(ctx context.Context, sourceID string) ([]domain.RightsRecord, error) {
	q := s.rebind(`SELECT id, source_id, asset_id, grantor, basis, evidence_ref, video_cleared, music_cleared,
		authorized_uses_json, authorized_platforms_json, attribution_conditions, expires_at, revoked_at, revocation_reason,
		reviewer_id, created_at, updated_at
		FROM rights_records WHERE source_id = ? ORDER BY created_at DESC`)
	rows, err := s.db.QueryContext(ctx, q, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.RightsRecord{}
	for rows.Next() {
		var r domain.RightsRecord
		var assetID sql.NullString
		var vidRaw, musRaw, expRaw, revRaw, crRaw, upRaw any
		var usesJSON, platsJSON string
		if err := rows.Scan(&r.ID, &r.SourceID, &assetID, &r.Grantor, &r.Basis, &r.EvidenceRef, &vidRaw, &musRaw,
			&usesJSON, &platsJSON, &r.AttributionConditions, &expRaw, &revRaw, &r.RevocationReason,
			&r.ReviewerID, &crRaw, &upRaw); err != nil {
			return nil, err
		}
		if assetID.Valid {
			r.AssetID = &assetID.String
		}
		r.VideoCleared = parseDBBool(vidRaw)
		r.MusicCleared = parseDBBool(musRaw)
		_ = json.Unmarshal([]byte(usesJSON), &r.AuthorizedUses)
		_ = json.Unmarshal([]byte(platsJSON), &r.AuthorizedPlatforms)
		r.ExpiresAt, _ = parseNullDBTime(expRaw)
		r.RevokedAt, _ = parseNullDBTime(revRaw)
		r.CreatedAt, _ = parseDBTime(crRaw)
		r.UpdatedAt, _ = parseDBTime(upRaw)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// FindEligibleInstagramRights verifies an active, non-expired, non-revoked rights record clearing BOTH video and music for Instagram redistribution.
func (s *Store) FindEligibleInstagramRights(ctx context.Context, sourceID string, assetID *string) (*domain.RightsRecord, error) {
	records, err := s.ListRightsForSource(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	for _, r := range records {
		if r.RevokedAt != nil {
			continue
		}
		if r.ExpiresAt != nil && now.After(*r.ExpiresAt) {
			continue
		}
		if assetID != nil && r.AssetID != nil && *r.AssetID != *assetID {
			continue
		}
		if !r.VideoCleared || !r.MusicCleared {
			continue
		}
		platOK := false
		for _, p := range r.AuthorizedPlatforms {
			lp := strings.ToLower(p)
			if lp == "instagram" || lp == "all" || lp == "*" {
				platOK = true
				break
			}
		}
		useOK := false
		for _, u := range r.AuthorizedUses {
			lu := strings.ToLower(u)
			if lu == "republish" || lu == "redistribution" || lu == "publish" || lu == "all" {
				useOK = true
				break
			}
		}
		if platOK && useOK {
			rec := r
			return &rec, nil
		}
	}
	return nil, domain.ErrNotFound
}

// RevokeRightsRecord implements Section 13: blocks pending publications and revokes affected hosted public/staging assets.
func (s *Store) RevokeRightsRecord(ctx context.Context, adminID, rightsID, reason string) error {
	now := time.Now().UTC()
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		var sourceID string
		var assetID sql.NullString
		if err := tx.QueryRowContext(ctx, s.rebind(`SELECT source_id, asset_id FROM rights_records WHERE id = ?`), rightsID).Scan(&sourceID, &assetID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return domain.ErrNotFound
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE rights_records SET revoked_at = ?, revocation_reason = ?, updated_at = ? WHERE id = ?`),
			s.timeArg(now), strings.TrimSpace(reason), s.timeArg(now), rightsID); err != nil {
			return err
		}
		// Block pending publications for this source
		if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE publication_jobs
			SET status = 'needs_rights', approved_by = NULL, approved_at = NULL, approved_draft_hash = '',
			    staging_token = '', status_detail = ?, updated_at = ?
			WHERE source_id = ? AND status IN ('draft', 'ready_for_review', 'approved', 'staging', 'retry_wait')`),
			"Rights revoked: "+reason, s.timeArg(now), sourceID); err != nil {
			return err
		}
		// Revoke hosted public/staging assets for this source/asset
		if assetID.Valid {
			_, _ = tx.ExecContext(ctx, s.rebind(`UPDATE media_assets SET status = 'revoked', updated_at = ? WHERE id = ?`), s.timeArg(now), assetID.String)
		} else {
			_, _ = tx.ExecContext(ctx, s.rebind(`UPDATE media_assets SET status = 'revoked', updated_at = ? WHERE source_id = ? AND access_scope IN ('public_authorized', 'staging')`), s.timeArg(now), sourceID)
		}
		return nil
	})
}

// --- Community Submissions, Public Catalog, Corrections & Reports (COM-01, COM-02) ---

func (s *Store) BuildPublicSnapshotFromEntry(ent domain.CatalogEntry) domain.PublicPayload {
	title := ""
	if ent.PersonalTitleOverride != nil && *ent.PersonalTitleOverride != "" {
		title = *ent.PersonalTitleOverride
	} else if ent.Source.Title != nil {
		title = *ent.Source.Title
	}
	creatorName := "Unknown creator"
	creatorURL := ""
	creatorRole := ""
	creatorVerified := false
	creatorHandle := ""
	if ent.PersonalCreatorOverride != nil && *ent.PersonalCreatorOverride != "" {
		creatorName = *ent.PersonalCreatorOverride
	} else if len(ent.Source.Creators) > 0 {
		sc := ent.Source.Creators[0]
		creatorName = sc.Creator.DisplayName
		creatorRole = sc.Role
		for _, id := range sc.Creator.Identities {
			if id.Platform == ent.Source.Platform || creatorURL == "" {
				creatorURL = id.ProfileURL
				creatorVerified = id.Verified
				if id.Handle != nil {
					creatorHandle = *id.Handle
				}
			}
		}
	}
	var pubOccs []domain.PublicOccurrenceSnapshot
	for _, o := range ent.Occurrences {
		if o.State == domain.OccurrenceRejected || o.State == domain.OccurrenceSuggested {
			continue
		}
		name := o.MoveName
		if name == "" && o.ProposedName != nil {
			name = *o.ProposedName
		}
		if name == "" {
			continue
		}
		// Note: Private notes are NEVER included in PublicPayload (Section 4.3)!
		pubOccs = append(pubOccs, domain.PublicOccurrenceSnapshot{
			MoveID:       o.MoveID,
			MoveName:     name,
			StartMs:      o.StartMs,
			EndMs:        o.EndMs,
			Ordinal:      o.Ordinal,
			TimingMethod: string(o.TimingMethod),
		})
	}
	if pubOccs == nil {
		pubOccs = []domain.PublicOccurrenceSnapshot{}
	}
	return domain.PublicPayload{
		SourceID:          ent.SourceID,
		Platform:          ent.Source.Platform,
		CanonicalURL:      ent.Source.CanonicalURL,
		Title:             title,
		CreatorName:       creatorName,
		CreatorProfileURL: creatorURL,
		CreatorRole:       creatorRole,
		CreatorVerified:   creatorVerified,
		CreatorHandle:     creatorHandle,
		DurationMs:        ent.Source.DurationMs,
		Occurrences:       pubOccs,
	}
}

func (s *Store) CreateSubmission(ctx context.Context, userID, entryID string) (*domain.PublicSubmission, error) {
	now := time.Now().UTC()
	var sub *domain.PublicSubmission

	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		ent, err := s.getEntryTx(ctx, tx, userID, entryID)
		if err != nil {
			return err
		}
		snapshot := s.BuildPublicSnapshotFromEntry(*ent)
		var prevCount int
		_ = tx.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM public_submissions WHERE entry_id = ?`), ent.ID).Scan(&prevCount)

		sub = &domain.PublicSubmission{
			ID:          uuid.NewString(),
			EntryID:     ent.ID,
			SourceID:    ent.SourceID,
			SubmitterID: userID,
			Snapshot:    snapshot,
			Status:      "pending",
			Version:     prevCount + 1,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO public_submissions (
			id, entry_id, source_id, submitter_id, snapshot_json, status, decision_reason, version, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, 'pending', '', ?, ?, ?)`),
			sub.ID, sub.EntryID, sub.SourceID, sub.SubmitterID, marshalJSONString(snapshot, "{}"), sub.Version, s.timeArg(now), s.timeArg(now),
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE catalog_entries SET visibility_state = 'submitted', revision = revision + 1, updated_at = ? WHERE id = ?`),
			s.timeArg(now), ent.ID); err != nil {
			return err
		}
		return nil
	})
	if err == nil && sub != nil {
		_ = s.RecordAudit(ctx, &userID, string(domain.RoleMember), "submission.created", "public_submission", sub.ID, sub.Version, "", nil, "")
	}
	return sub, err
}

func (s *Store) ListSubmissions(ctx context.Context, status string) ([]domain.PublicSubmission, error) {
	q := `SELECT id, entry_id, source_id, submitter_id, snapshot_json, status, reviewer_id, decision_reason, version, created_at, updated_at FROM public_submissions`
	var args []any
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC LIMIT 100`
	rows, err := s.db.QueryContext(ctx, s.rebind(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.PublicSubmission{}
	for rows.Next() {
		var sub domain.PublicSubmission
		var snapJSON string
		var revID sql.NullString
		var crRaw, upRaw any
		if err := rows.Scan(&sub.ID, &sub.EntryID, &sub.SourceID, &sub.SubmitterID, &snapJSON, &sub.Status, &revID, &sub.DecisionReason, &sub.Version, &crRaw, &upRaw); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(snapJSON), &sub.Snapshot)
		if revID.Valid {
			sub.ReviewerID = &revID.String
		}
		sub.CreatedAt, _ = parseDBTime(crRaw)
		sub.UpdatedAt, _ = parseDBTime(upRaw)
		out = append(out, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// DecideSubmission implements POST /admin/submissions/{id}/decision (approve, reject, changes_requested).
// Public approval NEVER triggers Instagram publishing automatically (COM-02).
func (s *Store) DecideSubmission(ctx context.Context, adminID, submissionID, decision, reason string) (*domain.PublicRevision, error) {
	switch decision {
	case "approved", "rejected", "changes_requested":
	default:
		return nil, fmt.Errorf("%w: invalid submission decision %q", domain.ErrValidation, decision)
	}
	now := time.Now().UTC()
	var pubRev *domain.PublicRevision

	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		var entryID, sourceID, snapJSON, curStatus string
		var version int
		if err := tx.QueryRowContext(ctx, s.rebind(`SELECT entry_id, source_id, snapshot_json, status, version FROM public_submissions WHERE id = ?`), submissionID).
			Scan(&entryID, &sourceID, &snapJSON, &curStatus, &version); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return domain.ErrNotFound
			}
			return err
		}
		if curStatus != "pending" {
			return fmt.Errorf("%w: submission is already %s", domain.ErrConflict, curStatus)
		}

		if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE public_submissions SET status = ?, reviewer_id = ?, decision_reason = ?, updated_at = ? WHERE id = ?`),
			decision, adminID, strings.TrimSpace(reason), s.timeArg(now), submissionID); err != nil {
			return err
		}

		visState := domain.VisibilityRejected
		if decision == "changes_requested" {
			visState = domain.VisibilityPrivate
		} else if decision == "approved" {
			visState = domain.VisibilityPublished
			// Supersede prior active public revision for this source
			if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE public_revisions SET status = 'superseded' WHERE source_id = ? AND status = 'active'`), sourceID); err != nil {
				return err
			}
			var maxRev sql.NullInt64
			_ = tx.QueryRowContext(ctx, s.rebind(`SELECT MAX(revision_number) FROM public_revisions WHERE source_id = ?`), sourceID).Scan(&maxRev)
			revNum := int(maxRev.Int64) + 1
			var payload domain.PublicPayload
			_ = json.Unmarshal([]byte(snapJSON), &payload)
			subIDCopy := submissionID
			pubRev = &domain.PublicRevision{
				ID:             uuid.NewString(),
				SourceID:       sourceID,
				SubmissionID:   &subIDCopy,
				RevisionNumber: revNum,
				Payload:        payload,
				Status:         "active",
				ApprovedBy:     adminID,
				ApprovedAt:     now,
				CreatedAt:      now,
			}
			if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO public_revisions (
				id, source_id, submission_id, revision_number, curated_payload_json, status, hidden_reason, approved_by, approved_at, created_at
			) VALUES (?, ?, ?, ?, ?, 'active', '', ?, ?, ?)`),
				pubRev.ID, sourceID, submissionID, revNum, snapJSON, adminID, s.timeArg(now), s.timeArg(now),
			); err != nil {
				return err
			}
		}

		_, err := tx.ExecContext(ctx, s.rebind(`UPDATE catalog_entries SET visibility_state = ?, revision = revision + 1, updated_at = ? WHERE id = ?`),
			string(visState), s.timeArg(now), entryID)
		return err
	})
	if err == nil {
		_ = s.RecordAudit(ctx, &adminID, string(domain.RoleAdmin), "submission."+decision, "public_submission", submissionID, 1, reason, nil, "")
	}
	return pubRev, err
}

func (s *Store) ListPublicCatalog(ctx context.Context, query, platform, cursor string, limit int) ([]domain.PublicRevision, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	var conds []string
	var args []any
	conds = append(conds, "pr.status = 'active'")

	if platform != "" {
		conds = append(conds, "src.platform = ?")
		args = append(args, strings.ToLower(strings.TrimSpace(platform)))
	}
	if query != "" {
		qLike := "%" + strings.ToLower(strings.TrimSpace(query)) + "%"
		conds = append(conds, "LOWER(pr.curated_payload_json) LIKE ?")
		args = append(args, qLike)
	}
	if cursor != "" {
		conds = append(conds, "pr.id < ?")
		args = append(args, cursor)
	}
	args = append(args, limit+1)

	sqlText := fmt.Sprintf(`SELECT pr.id, pr.source_id, pr.revision_number, pr.curated_payload_json, pr.status, pr.approved_at, pr.created_at
		FROM public_revisions pr
		JOIN sources src ON src.id = pr.source_id
		WHERE %s
		ORDER BY pr.id DESC
		LIMIT ?`, strings.Join(conds, " AND "))

	rows, err := s.db.QueryContext(ctx, s.rebind(sqlText), args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []domain.PublicRevision{}
	for rows.Next() {
		var pr domain.PublicRevision
		var payloadJSON string
		var appRaw, crRaw any
		if err := rows.Scan(&pr.ID, &pr.SourceID, &pr.RevisionNumber, &payloadJSON, &pr.Status, &appRaw, &crRaw); err != nil {
			return nil, "", err
		}
		_ = json.Unmarshal([]byte(payloadJSON), &pr.Payload)
		pr.ApprovedAt, _ = parseDBTime(appRaw)
		pr.CreatedAt, _ = parseDBTime(crRaw)
		out = append(out, pr)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	nextCursor := ""
	if len(out) > limit {
		nextCursor = out[limit-1].ID
		out = out[:limit]
	}
	return out, nextCursor, nil
}

func (s *Store) GetPublicRevision(ctx context.Context, revID string) (*domain.PublicRevision, error) {
	q := s.rebind(`SELECT id, source_id, submission_id, revision_number, curated_payload_json, status, hidden_reason, approved_by, approved_at, created_at
		FROM public_revisions WHERE id = ?`)
	var pr domain.PublicRevision
	var subID sql.NullString
	var payloadJSON string
	var appRaw, crRaw any
	if err := s.db.QueryRowContext(ctx, q, revID).Scan(&pr.ID, &pr.SourceID, &subID, &pr.RevisionNumber, &payloadJSON, &pr.Status, &pr.HiddenReason, &pr.ApprovedBy, &appRaw, &crRaw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	if subID.Valid {
		pr.SubmissionID = &subID.String
	}
	_ = json.Unmarshal([]byte(payloadJSON), &pr.Payload)
	pr.ApprovedAt, _ = parseDBTime(appRaw)
	pr.CreatedAt, _ = parseDBTime(crRaw)
	return &pr, nil
}

// CreatePublicCorrection records a proposed correction without immediately overwriting curated public content (Section 3 & 10).
func (s *Store) CreatePublicCorrection(ctx context.Context, userID, publicRevID, reason string, proposed domain.PublicPayload) (*domain.PublicCorrection, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("%w: reason is required for correction proposal", domain.ErrValidation)
	}
	pr, err := s.GetPublicRevision(ctx, publicRevID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	corr := &domain.PublicCorrection{
		ID:               uuid.NewString(),
		PublicRevisionID: pr.ID,
		SourceID:         pr.SourceID,
		ProposerID:       userID,
		ProposedChanges:  proposed,
		Reason:           strings.TrimSpace(reason),
		Status:           "pending",
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if _, err := s.db.ExecContext(ctx, s.rebind(`INSERT INTO public_corrections (
		id, public_revision_id, source_id, proposer_id, proposed_changes_json, reason, status, review_note, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, 'pending', '', ?, ?)`),
		corr.ID, corr.PublicRevisionID, corr.SourceID, corr.ProposerID, marshalJSONString(proposed, "{}"), corr.Reason, s.timeArg(now), s.timeArg(now),
	); err != nil {
		return nil, err
	}
	return corr, nil
}

func (s *Store) CreateReport(ctx context.Context, reporterID *string, publicRevID, category, details, contact string) (*domain.Report, error) {
	switch category {
	case "incorrect_naming", "misattribution", "unavailable_source", "rights_concern":
	default:
		return nil, fmt.Errorf("%w: invalid report category %q", domain.ErrValidation, category)
	}
	pr, err := s.GetPublicRevision(ctx, publicRevID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	rep := &domain.Report{
		ID:               uuid.NewString(),
		PublicRevisionID: &pr.ID,
		SourceID:         pr.SourceID,
		ReporterID:       reporterID,
		ReporterContact:  strings.TrimSpace(contact),
		Category:         category,
		Details:          strings.TrimSpace(details),
		Status:           "open",
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if _, err := s.db.ExecContext(ctx, s.rebind(`INSERT INTO reports (
		id, public_revision_id, source_id, reporter_id, reporter_contact, category, details, status, resolution_note, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, 'open', '', ?, ?)`),
		rep.ID, rep.PublicRevisionID, rep.SourceID, rep.ReporterID, rep.ReporterContact, rep.Category, rep.Details, s.timeArg(now), s.timeArg(now),
	); err != nil {
		return nil, err
	}
	return rep, nil
}

func (s *Store) HidePublicRevision(ctx context.Context, adminID, publicRevID, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: moderation hide reason is required", domain.ErrValidation)
	}
	res, err := s.db.ExecContext(ctx, s.rebind(`UPDATE public_revisions SET status = 'hidden', hidden_reason = ? WHERE id = ?`), strings.TrimSpace(reason), publicRevID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.ErrNotFound
	}
	return s.RecordAudit(ctx, &adminID, string(domain.RoleAdmin), "public_revision.hidden", "public_revision", publicRevID, 1, reason, nil, "")
}

// --- Instagram Publication Jobs (PUB-01..PUB-05) ---

func (s *Store) SavePublicationJob(ctx context.Context, pub domain.PublicationJob) (*domain.PublicationJob, error) {
	now := time.Now().UTC()
	if pub.ID == "" {
		pub.ID = uuid.NewString()
		pub.CreatedAt = now
	}
	pub.UpdatedAt = now

	var existing int
	_ = s.db.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM publication_jobs WHERE id = ?`), pub.ID).Scan(&existing)
	if existing == 0 {
		q := s.rebind(`INSERT INTO publication_jobs (
			id, destination_account, brand_prefix, origin_platform, publication_mode,
			source_id, public_revision_id, private_entry_id, private_entry_revision, asset_id,
			selected_segments_json, move_tags_json, include_companion_tags, caption, first_comment,
			first_comment_status, first_comment_error, draft_hash, asset_hash, approved_by, approved_at,
			approved_draft_hash, approved_asset_hash, rights_snapshot_json, staging_token, staging_expires_at,
			remote_container_id, remote_media_id, remote_comment_id, result_url, execute_idempotency_key,
			status, status_detail, created_by, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if _, err := s.db.ExecContext(ctx, q,
			pub.ID, pub.DestinationAccount, pub.BrandPrefix, string(pub.OriginPlatform), pub.PublicationMode,
			pub.SourceID, pub.PublicRevisionID, pub.PrivateEntryID, pub.PrivateEntryRevision, pub.AssetID,
			marshalJSONString(pub.SelectedSegments, "[]"), marshalJSONString(pub.MoveTags, "[]"),
			s.boolArg(pub.IncludeCompanionTags), pub.Caption, pub.FirstComment, pub.FirstCommentStatus, pub.FirstCommentError,
			pub.DraftHash, pub.AssetHash, pub.ApprovedBy, s.nullTimeArg(pub.ApprovedAt),
			pub.ApprovedDraftHash, pub.ApprovedAssetHash, marshalJSONString(pub.RightsSnapshot, "{}"),
			pub.StagingToken, s.nullTimeArg(pub.StagingExpiresAt),
			pub.RemoteContainerID, pub.RemoteMediaID, pub.RemoteCommentID, pub.ResultURL, pub.ExecuteIdempotencyKey,
			string(pub.Status), pub.StatusDetail, pub.CreatedBy, s.timeArg(pub.CreatedAt), s.timeArg(pub.UpdatedAt),
		); err != nil {
			return nil, err
		}
	} else {
		q := s.rebind(`UPDATE publication_jobs SET
			destination_account = ?, brand_prefix = ?, asset_id = ?, selected_segments_json = ?,
			move_tags_json = ?, include_companion_tags = ?, caption = ?, first_comment = ?,
			first_comment_status = ?, first_comment_error = ?, draft_hash = ?, asset_hash = ?,
			approved_by = ?, approved_at = ?, approved_draft_hash = ?, approved_asset_hash = ?,
			rights_snapshot_json = ?, staging_token = ?, staging_expires_at = ?,
			remote_container_id = ?, remote_media_id = ?, remote_comment_id = ?, result_url = ?,
			execute_idempotency_key = ?, status = ?, status_detail = ?, updated_at = ?
			WHERE id = ?`)
		if _, err := s.db.ExecContext(ctx, q,
			pub.DestinationAccount, pub.BrandPrefix, pub.AssetID, marshalJSONString(pub.SelectedSegments, "[]"),
			marshalJSONString(pub.MoveTags, "[]"), s.boolArg(pub.IncludeCompanionTags), pub.Caption, pub.FirstComment,
			pub.FirstCommentStatus, pub.FirstCommentError, pub.DraftHash, pub.AssetHash,
			pub.ApprovedBy, s.nullTimeArg(pub.ApprovedAt), pub.ApprovedDraftHash, pub.ApprovedAssetHash,
			marshalJSONString(pub.RightsSnapshot, "{}"), pub.StagingToken, s.nullTimeArg(pub.StagingExpiresAt),
			pub.RemoteContainerID, pub.RemoteMediaID, pub.RemoteCommentID, pub.ResultURL,
			pub.ExecuteIdempotencyKey, string(pub.Status), pub.StatusDetail, s.timeArg(pub.UpdatedAt),
			pub.ID,
		); err != nil {
			return nil, err
		}
	}
	return s.GetPublicationJob(ctx, pub.ID)
}

func (s *Store) GetPublicationJob(ctx context.Context, id string) (*domain.PublicationJob, error) {
	q := s.rebind(`SELECT id, destination_account, brand_prefix, origin_platform, publication_mode,
		source_id, public_revision_id, private_entry_id, private_entry_revision, asset_id,
		selected_segments_json, move_tags_json, include_companion_tags, caption, first_comment,
		first_comment_status, first_comment_error, draft_hash, asset_hash, approved_by, approved_at,
		approved_draft_hash, approved_asset_hash, rights_snapshot_json, staging_token, staging_expires_at,
		remote_container_id, remote_media_id, remote_comment_id, result_url, execute_idempotency_key,
		status, status_detail, created_by, created_at, updated_at
		FROM publication_jobs WHERE id = ?`)
	return s.scanPublicationJob(s.db.QueryRowContext(ctx, q, id))
}

func (s *Store) GetPublicationByStagingToken(ctx context.Context, token string) (*domain.PublicationJob, error) {
	if strings.TrimSpace(token) == "" {
		return nil, domain.ErrNotFound
	}
	q := s.rebind(`SELECT id, destination_account, brand_prefix, origin_platform, publication_mode,
		source_id, public_revision_id, private_entry_id, private_entry_revision, asset_id,
		selected_segments_json, move_tags_json, include_companion_tags, caption, first_comment,
		first_comment_status, first_comment_error, draft_hash, asset_hash, approved_by, approved_at,
		approved_draft_hash, approved_asset_hash, rights_snapshot_json, staging_token, staging_expires_at,
		remote_container_id, remote_media_id, remote_comment_id, result_url, execute_idempotency_key,
		status, status_detail, created_by, created_at, updated_at
		FROM publication_jobs WHERE staging_token = ?`)
	return s.scanPublicationJob(s.db.QueryRowContext(ctx, q, token))
}

func (s *Store) ListPublicationJobs(ctx context.Context, limit int) ([]domain.PublicationJob, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q := s.rebind(`SELECT id FROM publication_jobs ORDER BY updated_at DESC LIMIT ?`)
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	out := make([]domain.PublicationJob, 0, len(ids))
	for _, id := range ids {
		p, err := s.GetPublicationJob(ctx, id)
		if err == nil {
			out = append(out, *p)
		}
	}
	return out, nil
}

func (s *Store) scanPublicationJob(row *sql.Row) (*domain.PublicationJob, error) {
	var (
		p                                      domain.PublicationJob
		origPlat, statusStr                    string
		pubRevID, privEntID, assetID, appBy    sql.NullString
		privRev                                sql.NullInt64
		segJSON, tagsJSON, rightsJSON          string
		compRaw, appAtRaw, stgExpRaw, crR, upR any
	)
	if err := row.Scan(
		&p.ID, &p.DestinationAccount, &p.BrandPrefix, &origPlat, &p.PublicationMode,
		&p.SourceID, &pubRevID, &privEntID, &privRev, &assetID,
		&segJSON, &tagsJSON, &compRaw, &p.Caption, &p.FirstComment,
		&p.FirstCommentStatus, &p.FirstCommentError, &p.DraftHash, &p.AssetHash, &appBy, &appAtRaw,
		&p.ApprovedDraftHash, &p.ApprovedAssetHash, &rightsJSON, &p.StagingToken, &stgExpRaw,
		&p.RemoteContainerID, &p.RemoteMediaID, &p.RemoteCommentID, &p.ResultURL, &p.ExecuteIdempotencyKey,
		&statusStr, &p.StatusDetail, &p.CreatedBy, &crR, &upR,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	p.OriginPlatform = domain.Platform(origPlat)
	p.Status = domain.PublicationStatus(statusStr)
	if pubRevID.Valid {
		p.PublicRevisionID = &pubRevID.String
	}
	if privEntID.Valid {
		p.PrivateEntryID = &privEntID.String
	}
	if privRev.Valid {
		rv := int(privRev.Int64)
		p.PrivateEntryRevision = &rv
	}
	if assetID.Valid {
		p.AssetID = &assetID.String
	}
	if appBy.Valid {
		p.ApprovedBy = &appBy.String
	}
	p.IncludeCompanionTags = parseDBBool(compRaw)
	p.ApprovedAt, _ = parseNullDBTime(appAtRaw)
	p.StagingExpiresAt, _ = parseNullDBTime(stgExpRaw)
	p.CreatedAt, _ = parseDBTime(crR)
	p.UpdatedAt, _ = parseDBTime(upR)
	_ = json.Unmarshal([]byte(segJSON), &p.SelectedSegments)
	_ = json.Unmarshal([]byte(tagsJSON), &p.MoveTags)
	var rr domain.RightsRecord
	if json.Unmarshal([]byte(rightsJSON), &rr) == nil && rr.ID != "" {
		p.RightsSnapshot = &rr
	}
	return &p, nil
}

// --- Audit Logging ---

func (s *Store) RecordAudit(ctx context.Context, actorID *string, actorRole, action, targetType, targetID string, rev int, reason string, meta map[string]any, correlationID string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, s.rebind(`INSERT INTO audit_events (
		id, actor_id, actor_role, action, target_type, target_id, target_revision, reason, metadata_json, correlation_id, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		uuid.NewString(), actorID, actorRole, action, targetType, targetID, rev, strings.TrimSpace(reason),
		marshalJSONString(meta, "{}"), correlationID, s.timeArg(now),
	)
	return err
}

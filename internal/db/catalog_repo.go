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
	"github.com/johandry/vueltario/internal/urlnorm"
)

type SaveLinkRequest struct {
	UserID          string
	RawURL          string
	ClientRequestID string
	PrivateNotes    string
	CorrelationID   string
}

type SaveLinkResult struct {
	Entry         domain.CatalogEntry  `json:"entry"`
	Source        domain.Source        `json:"source"`
	Job           *domain.ProcessingJob `json:"job,omitempty"`
	Deduplicated  bool                 `json:"deduplicated"`
	CreatedSource bool                 `json:"created_source"`
}

// SaveLink implements ING-01..ING-04: normalizes link, deduplicates source & personal entry,
// saves immediately before enrichment, and enqueues asynchronous enrichment in the same transaction.
func (s *Store) SaveLink(ctx context.Context, req SaveLinkRequest) (*SaveLinkResult, error) {
	norm, err := urlnorm.Normalize(req.RawURL)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	var result SaveLinkResult

	err = s.withWriteTx(ctx, func(tx *sql.Tx) error {
		// 1. Check client_request_id idempotency if provided
		clientReqID := strings.TrimSpace(req.ClientRequestID)
		if clientReqID != "" {
			var existingEntryID string
			qReq := s.rebind(`SELECT id FROM catalog_entries WHERE user_id = ? AND client_request_id = ? AND deleted_at IS NULL`)
			if err := tx.QueryRowContext(ctx, qReq, req.UserID, clientReqID).Scan(&existingEntryID); err == nil {
				result.Deduplicated = true
				entry, err := s.getEntryTx(ctx, tx, req.UserID, existingEntryID)
				if err != nil {
					return err
				}
				result.Entry = *entry
				result.Source = entry.Source
				return nil
			}
		}

		// 2. Find or create Source (dedup by platform + platform_content_id first, then canonical_url)
		var srcID string
		if norm.PlatformContentID != nil && *norm.PlatformContentID != "" {
			qSrc := s.rebind(`SELECT id FROM sources WHERE platform = ? AND platform_content_id = ?`)
			_ = tx.QueryRowContext(ctx, qSrc, string(norm.Platform), *norm.PlatformContentID).Scan(&srcID)
		}
		if srcID == "" {
			qSrc := s.rebind(`SELECT id FROM sources WHERE canonical_url = ?`)
			_ = tx.QueryRowContext(ctx, qSrc, norm.CanonicalURL).Scan(&srcID)
		}

		if srcID == "" {
			srcID = uuid.NewString()
			qInsSrc := s.rebind(`INSERT INTO sources (
				id, platform, platform_content_id, canonical_url, original_url,
				availability_state, embed_eligible, provenance, metadata_version, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, 'unknown', ?, 'user_link', 1, ?, ?)`)
			if _, err := tx.ExecContext(ctx, qInsSrc,
				srcID, string(norm.Platform), norm.PlatformContentID, norm.CanonicalURL, norm.OriginalURL,
				s.boolArg(true), s.timeArg(now), s.timeArg(now),
			); err != nil {
				return err
			}
			result.CreatedSource = true
		}

		// 3. Find or create user's CatalogEntry
		var entryID string
		var deletedRaw any
		qEnt := s.rebind(`SELECT id, deleted_at FROM catalog_entries WHERE user_id = ? AND source_id = ?`)
		err := tx.QueryRowContext(ctx, qEnt, req.UserID, srcID).Scan(&entryID, &deletedRaw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		var clientReqPtr *string
		if clientReqID != "" {
			clientReqPtr = &clientReqID
		}

		if entryID != "" {
			delAt, _ := parseNullDBTime(deletedRaw)
			if delAt != nil {
				// Restore previously deleted entry
				qRes := s.rebind(`UPDATE catalog_entries SET deleted_at = NULL, visibility_state = 'private',
					private_notes = CASE WHEN ? <> '' THEN ? ELSE private_notes END,
					observed_offset_ms = COALESCE(?, observed_offset_ms),
					client_request_id = COALESCE(?, client_request_id),
					revision = revision + 1, updated_at = ?
					WHERE id = ?`)
				if _, err := tx.ExecContext(ctx, qRes, req.PrivateNotes, req.PrivateNotes, norm.ObservedOffsetMs, clientReqPtr, s.timeArg(now), entryID); err != nil {
					return err
				}
			} else {
				result.Deduplicated = true
				if norm.ObservedOffsetMs != nil {
					_, _ = tx.ExecContext(ctx, s.rebind(`UPDATE catalog_entries SET observed_offset_ms = COALESCE(observed_offset_ms, ?), updated_at = ? WHERE id = ?`),
						norm.ObservedOffsetMs, s.timeArg(now), entryID)
				}
			}
		} else {
			entryID = uuid.NewString()
			qInsEnt := s.rebind(`INSERT INTO catalog_entries (
				id, user_id, source_id, visibility_state, private_notes,
				observed_offset_ms, client_request_id, revision, created_at, updated_at
			) VALUES (?, ?, ?, 'private', ?, ?, ?, 1, ?, ?)`)
			if _, err := tx.ExecContext(ctx, qInsEnt,
				entryID, req.UserID, srcID, strings.TrimSpace(req.PrivateNotes),
				norm.ObservedOffsetMs, clientReqPtr, s.timeArg(now), s.timeArg(now),
			); err != nil {
				return err
			}
		}

		// 4. Enqueue async enrichment job in the same transaction if source hasn't been fetched yet
		var fetchedRaw any
		_ = tx.QueryRowContext(ctx, s.rebind(`SELECT fetched_at FROM sources WHERE id = ?`), srcID).Scan(&fetchedRaw)
		fetchedAt, _ := parseNullDBTime(fetchedRaw)
		if fetchedAt == nil {
			job, err := s.enqueueJobTx(ctx, tx, enqueueJobParams{
				OwnerUserID:    &req.UserID,
				JobType:        "enrich_source",
				TargetType:     "source",
				TargetID:       srcID,
				TargetRevision: 1,
				Payload:        map[string]string{"entry_id": entryID, "source_id": srcID},
				IdempotencyKey: fmt.Sprintf("enrich_source:%s:v1", srcID),
				CorrelationID:  req.CorrelationID,
			})
			if err != nil {
				return err
			}
			result.Job = job
		}

		entry, err := s.getEntryTx(ctx, tx, req.UserID, entryID)
		if err != nil {
			return err
		}
		result.Entry = *entry
		result.Source = entry.Source
		return s.appendSyncEventTx(ctx, tx, req.UserID, "catalog_entry", entry.ID, "upsert", entry.Revision, entry)
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

type EntryFilter struct {
	UserID     string
	Query      string
	Platform   string
	MoveID     string
	Visibility string
	HasAsset   *bool
	Limit      int
}

func (s *Store) ListEntries(ctx context.Context, f EntryFilter) ([]domain.CatalogEntry, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var conds []string
	var args []any

	conds = append(conds, "e.user_id = ?", "e.deleted_at IS NULL")
	args = append(args, f.UserID)

	if f.Platform != "" {
		conds = append(conds, "src.platform = ?")
		args = append(args, strings.ToLower(strings.TrimSpace(f.Platform)))
	}
	if f.Visibility != "" {
		conds = append(conds, "e.visibility_state = ?")
		args = append(args, strings.ToLower(strings.TrimSpace(f.Visibility)))
	}
	if f.MoveID != "" {
		conds = append(conds, `EXISTS (SELECT 1 FROM occurrences o WHERE o.entry_id = e.id AND o.move_id = ? AND o.state <> 'rejected')`)
		args = append(args, f.MoveID)
	}
	if f.HasAsset != nil {
		if *f.HasAsset {
			conds = append(conds, `EXISTS (SELECT 1 FROM media_assets ma WHERE ma.source_id = e.source_id AND ma.owner_user_id = e.user_id AND ma.status = 'ready' AND ma.deleted_at IS NULL)`)
		} else {
			conds = append(conds, `NOT EXISTS (SELECT 1 FROM media_assets ma WHERE ma.source_id = e.source_id AND ma.owner_user_id = e.user_id AND ma.status = 'ready' AND ma.deleted_at IS NULL)`)
		}
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		normQ := "%" + domain.NormalizeSearchKey(q) + "%"
		rawQ := "%" + strings.ToLower(q) + "%"
		conds = append(conds, `(
			LOWER(COALESCE(e.personal_title_override, src.title, '')) LIKE ?
			OR LOWER(COALESCE(e.personal_creator_override, '')) LIKE ?
			OR LOWER(src.canonical_url) LIKE ?
			OR EXISTS (
				SELECT 1 FROM source_creators sc
				JOIN creators cr ON cr.id = sc.creator_id
				WHERE sc.source_id = src.id AND (cr.normalized_name LIKE ? OR LOWER(cr.display_name) LIKE ?)
			)
			OR EXISTS (
				SELECT 1 FROM occurrences o
				LEFT JOIN moves m ON m.id = o.move_id
				LEFT JOIN move_aliases ma ON ma.move_id = m.id
				WHERE o.entry_id = e.id AND o.state <> 'rejected' AND (
					LOWER(COALESCE(o.proposed_name, '')) LIKE ?
					OR m.normalized_search_key LIKE ?
					OR ma.normalized_alias LIKE ?
				)
			)
		)`)
		args = append(args, rawQ, rawQ, rawQ, normQ, rawQ, rawQ, normQ, normQ)
	}

	args = append(args, limit)
	sqlText := fmt.Sprintf(`SELECT e.id FROM catalog_entries e
		JOIN sources src ON src.id = e.source_id
		WHERE %s
		ORDER BY e.updated_at DESC
		LIMIT ?`, strings.Join(conds, " AND "))

	rows, err := s.db.QueryContext(ctx, s.rebind(sqlText), args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	entries := make([]domain.CatalogEntry, 0, len(ids))
	for _, id := range ids {
		ent, err := s.getEntryTx(ctx, s.db, f.UserID, id)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				continue
			}
			return nil, err
		}
		entries = append(entries, *ent)
	}
	return entries, nil
}

func (s *Store) GetEntry(ctx context.Context, userID, entryID string) (*domain.CatalogEntry, error) {
	return s.getEntryTx(ctx, s.db, userID, entryID)
}

func (s *Store) getEntryTx(ctx context.Context, qx queryExecer, userID, entryID string) (*domain.CatalogEntry, error) {
	q := s.rebind(`SELECT
		e.id, e.user_id, e.source_id, e.visibility_state, e.private_notes,
		e.personal_title_override, e.personal_creator_override, e.observed_offset_ms,
		e.client_request_id, e.revision, e.created_at, e.updated_at, e.deleted_at
		FROM catalog_entries e
		WHERE e.id = ? AND e.deleted_at IS NULL`)
	var (
		ent                           domain.CatalogEntry
		visStr                        string
		titleOv, creatorOv, clientReq sql.NullString
		offsetMs                      sql.NullInt64
		createdRaw, updatedRaw        any
		delRaw                        any
	)
	err := qx.QueryRowContext(ctx, q, entryID).Scan(
		&ent.ID, &ent.UserID, &ent.SourceID, &visStr, &ent.PrivateNotes,
		&titleOv, &creatorOv, &offsetMs, &clientReq, &ent.Revision,
		&createdRaw, &updatedRaw, &delRaw,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	// Object-level authorization: private entry belongs strictly to its owner!
	if ent.UserID != userID {
		return nil, domain.ErrForbidden
	}
	ent.VisibilityState = domain.VisibilityState(visStr)
	if titleOv.Valid {
		ent.PersonalTitleOverride = &titleOv.String
	}
	if creatorOv.Valid {
		ent.PersonalCreatorOverride = &creatorOv.String
	}
	if offsetMs.Valid {
		ent.ObservedOffsetMs = &offsetMs.Int64
	}
	if clientReq.Valid {
		ent.ClientRequestID = &clientReq.String
	}
	ent.CreatedAt, _ = parseDBTime(createdRaw)
	ent.UpdatedAt, _ = parseDBTime(updatedRaw)
	ent.DeletedAt, _ = parseNullDBTime(delRaw)

	src, err := s.getSourceTx(ctx, qx, ent.SourceID)
	if err != nil {
		return nil, err
	}
	ent.Source = *src

	occs, err := s.listOccurrencesTx(ctx, qx, ent.ID)
	if err != nil {
		return nil, err
	}
	ent.Occurrences = occs

	suggs, err := s.listSuggestionsTx(ctx, qx, ent.ID)
	if err != nil {
		return nil, err
	}
	ent.Suggestions = suggs

	evid, err := s.listEvidenceScopedTx(ctx, qx, ent.SourceID, userID)
	if err != nil {
		return nil, err
	}
	ent.Evidence = evid

	assets, err := s.listAssetsScopedTx(ctx, qx, ent.SourceID, userID)
	if err != nil {
		return nil, err
	}
	ent.Assets = assets

	job, _ := s.getLatestJobForTargetTx(ctx, qx, "source", ent.SourceID)
	if job != nil {
		ent.LatestJob = job
	}
	return &ent, nil
}

func (s *Store) GetSource(ctx context.Context, sourceID string) (*domain.Source, error) {
	return s.getSourceTx(ctx, s.db, sourceID)
}

func (s *Store) getSourceTx(ctx context.Context, qx queryExecer, sourceID string) (*domain.Source, error) {
	q := s.rebind(`SELECT id, platform, platform_content_id, canonical_url, original_url,
		title, description, duration_ms, availability_state, embed_eligible, provenance,
		metadata_version, fetched_at, created_at, updated_at
		FROM sources WHERE id = ?`)
	var (
		src                            domain.Source
		platStr, availStr              string
		contentID, title, desc         sql.NullString
		durMs                          sql.NullInt64
		embedRaw, fetchRaw, crRaw, upR any
	)
	err := qx.QueryRowContext(ctx, q, sourceID).Scan(
		&src.ID, &platStr, &contentID, &src.CanonicalURL, &src.OriginalURL,
		&title, &desc, &durMs, &availStr, &embedRaw, &src.Provenance,
		&src.MetadataVersion, &fetchRaw, &crRaw, &upR,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	src.Platform = domain.Platform(platStr)
	src.AvailabilityState = domain.AvailabilityState(availStr)
	if contentID.Valid {
		src.PlatformContentID = &contentID.String
	}
	if title.Valid {
		src.Title = &title.String
	}
	if desc.Valid {
		src.Description = &desc.String
	}
	if durMs.Valid {
		src.DurationMs = &durMs.Int64
	}
	src.EmbedEligible = parseDBBool(embedRaw)
	src.FetchedAt, _ = parseNullDBTime(fetchRaw)
	src.CreatedAt, _ = parseDBTime(crRaw)
	src.UpdatedAt, _ = parseDBTime(upR)

	creators, err := s.listSourceCreatorsTx(ctx, qx, src.ID)
	if err != nil {
		return nil, err
	}
	src.Creators = creators
	return &src, nil
}

func (s *Store) listSourceCreatorsTx(ctx context.Context, qx queryExecer, sourceID string) ([]domain.SourceCreator, error) {
	q := s.rebind(`SELECT sc.id, sc.source_id, sc.creator_id, sc.role, sc.provenance,
		c.display_name, c.biography, c.notes, c.created_at, c.updated_at
		FROM source_creators sc
		JOIN creators c ON c.id = sc.creator_id
		WHERE sc.source_id = ?
		ORDER BY CASE sc.role WHEN 'original' THEN 0 WHEN 'uploader' THEN 1 ELSE 2 END`)
	rows, err := qx.QueryContext(ctx, q, sourceID)
	if err != nil {
		return nil, err
	}
	var out []domain.SourceCreator
	for rows.Next() {
		var sc domain.SourceCreator
		var crRaw, upRaw any
		if err := rows.Scan(
			&sc.ID, &sc.SourceID, &sc.CreatorID, &sc.Role, &sc.Provenance,
			&sc.Creator.DisplayName, &sc.Creator.Biography, &sc.Creator.Notes, &crRaw, &upRaw,
		); err != nil {
			_ = rows.Close()
			return nil, err
		}
		sc.Creator.ID = sc.CreatorID
		sc.Creator.CreatedAt, _ = parseDBTime(crRaw)
		sc.Creator.UpdatedAt, _ = parseDBTime(upRaw)
		out = append(out, sc)
	}
	_ = rows.Close()

	for i := range out {
		idents, err := s.listCreatorIdentitiesTx(ctx, qx, out[i].CreatorID)
		if err != nil {
			return nil, err
		}
		out[i].Creator.Identities = idents
	}
	return out, nil
}

func (s *Store) listCreatorIdentitiesTx(ctx context.Context, qx queryExecer, creatorID string) ([]domain.CreatorIdentity, error) {
	q := s.rebind(`SELECT id, creator_id, platform, platform_user_id, handle, profile_url, verified, verification_source, verified_at, created_at
		FROM creator_identities WHERE creator_id = ?`)
	rows, err := qx.QueryContext(ctx, q, creatorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.CreatorIdentity
	for rows.Next() {
		var ci domain.CreatorIdentity
		var platStr string
		var puid, handle sql.NullString
		var verRaw, verAtRaw, crRaw any
		if err := rows.Scan(&ci.ID, &ci.CreatorID, &platStr, &puid, &handle, &ci.ProfileURL, &verRaw, &ci.VerificationSource, &verAtRaw, &crRaw); err != nil {
			return nil, err
		}
		ci.Platform = domain.Platform(platStr)
		if puid.Valid {
			ci.PlatformUserID = &puid.String
		}
		if handle.Valid {
			ci.Handle = &handle.String
		}
		ci.Verified = parseDBBool(verRaw)
		ci.VerifiedAt, _ = parseNullDBTime(verAtRaw)
		ci.CreatedAt, _ = parseDBTime(crRaw)
		out = append(out, ci)
	}
	return out, nil
}

type UpdateEntryInput struct {
	UserID                  string
	EntryID                 string
	ExpectedRevision        int
	PrivateNotes            *string
	PersonalTitleOverride   *string
	PersonalCreatorOverride *string
	ClearTitleOverride      bool
	ClearCreatorOverride    bool
	SourceDurationMs        *int64
	CreatorAttribution      *CreatorAttributionInput
}

type CreatorAttributionInput struct {
	DisplayName string
	Role        string // original, uploader, collaborator (defaults to uploader unless explicitly specified)
	Handle      string
	ProfileURL  string
	Verified    bool
}

// UpdateEntry applies personal overlays with optimistic concurrency (If-Match / ExpectedRevision).
func (s *Store) UpdateEntry(ctx context.Context, in UpdateEntryInput) (*domain.CatalogEntry, error) {
	if in.ExpectedRevision <= 0 {
		return nil, fmt.Errorf("%w: If-Match / expected revision is required", domain.ErrValidation)
	}
	now := time.Now().UTC()
	var updated *domain.CatalogEntry

	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		cur, err := s.getEntryTx(ctx, tx, in.UserID, in.EntryID)
		if err != nil {
			return err
		}
		if cur.Revision != in.ExpectedRevision {
			return fmt.Errorf("%w: entry revision mismatch (current=%d, expected=%d)", domain.ErrConflict, cur.Revision, in.ExpectedRevision)
		}

		notes := cur.PrivateNotes
		if in.PrivateNotes != nil {
			notes = *in.PrivateNotes
		}
		titleOv := cur.PersonalTitleOverride
		if in.ClearTitleOverride {
			titleOv = nil
		} else if in.PersonalTitleOverride != nil {
			t := strings.TrimSpace(*in.PersonalTitleOverride)
			if t == "" {
				titleOv = nil
			} else {
				titleOv = &t
			}
		}
		creatorOv := cur.PersonalCreatorOverride
		if in.ClearCreatorOverride {
			creatorOv = nil
		} else if in.PersonalCreatorOverride != nil {
			c := strings.TrimSpace(*in.PersonalCreatorOverride)
			if c == "" {
				creatorOv = nil
			} else {
				creatorOv = &c
			}
		}

		newRev := cur.Revision + 1
		res, err := tx.ExecContext(ctx, s.rebind(`UPDATE catalog_entries
			SET private_notes = ?, personal_title_override = ?, personal_creator_override = ?, revision = ?, updated_at = ?
			WHERE id = ? AND user_id = ? AND revision = ?`),
			notes, titleOv, creatorOv, newRev, s.timeArg(now), cur.ID, in.UserID, in.ExpectedRevision,
		)
		if err != nil {
			return err
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			return domain.ErrConflict
		}

		if in.SourceDurationMs != nil && *in.SourceDurationMs >= 0 {
			if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE sources SET duration_ms = ?, updated_at = ? WHERE id = ?`),
				*in.SourceDurationMs, s.timeArg(now), cur.SourceID); err != nil {
				return err
			}
		}

		if in.CreatorAttribution != nil && strings.TrimSpace(in.CreatorAttribution.DisplayName) != "" {
			if err := s.upsertSourceCreatorTx(ctx, tx, cur.Source, *in.CreatorAttribution, "manual"); err != nil {
				return err
			}
		}

		updated, err = s.getEntryTx(ctx, tx, in.UserID, cur.ID)
		if err != nil {
			return err
		}
		return s.appendSyncEventTx(ctx, tx, in.UserID, "catalog_entry", updated.ID, "upsert", updated.Revision, updated)
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *Store) DeleteEntry(ctx context.Context, userID, entryID string) error {
	now := time.Now().UTC()
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		cur, err := s.getEntryTx(ctx, tx, userID, entryID)
		if err != nil {
			return err
		}
		newRev := cur.Revision + 1
		if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE catalog_entries SET deleted_at = ?, visibility_state = 'withdrawn', revision = ?, updated_at = ? WHERE id = ? AND user_id = ?`),
			s.timeArg(now), newRev, s.timeArg(now), entryID, userID); err != nil {
			return err
		}
		return s.appendSyncEventTx(ctx, tx, userID, "catalog_entry", entryID, "delete", newRev, map[string]any{"id": entryID, "deleted_at": now})
	})
}

func (s *Store) upsertSourceCreatorTx(ctx context.Context, tx *sql.Tx, src domain.Source, attr CreatorAttributionInput, provenance string) error {
	now := time.Now().UTC()
	name := strings.TrimSpace(attr.DisplayName)
	normName := domain.NormalizeSearchKey(name)
	role := strings.ToLower(strings.TrimSpace(attr.Role))
	if role != "original" && role != "uploader" && role != "collaborator" {
		// PRD Section 8: uploader must not automatically be labeled original choreographer!
		role = "uploader"
	}

	var creatorID string
	_ = tx.QueryRowContext(ctx, s.rebind(`SELECT id FROM creators WHERE normalized_name = ? LIMIT 1`), normName).Scan(&creatorID)
	if creatorID == "" {
		creatorID = uuid.NewString()
		if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO creators (id, display_name, normalized_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`),
			creatorID, name, normName, s.timeArg(now), s.timeArg(now)); err != nil {
			return err
		}
	}

	handle := strings.TrimPrefix(strings.TrimSpace(attr.Handle), "@")
	var handlePtr *string
	if handle != "" {
		handlePtr = &handle
	}
	if handlePtr != nil || strings.TrimSpace(attr.ProfileURL) != "" {
		var existingIdent string
		_ = tx.QueryRowContext(ctx, s.rebind(`SELECT id FROM creator_identities WHERE creator_id = ? AND platform = ? LIMIT 1`),
			creatorID, string(src.Platform)).Scan(&existingIdent)
		var verAt *time.Time
		verSrc := ""
		if attr.Verified {
			verAt = &now
			verSrc = provenance
		}
		if existingIdent == "" {
			if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO creator_identities (id, creator_id, platform, handle, profile_url, verified, verification_source, verified_at, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
				uuid.NewString(), creatorID, string(src.Platform), handlePtr, strings.TrimSpace(attr.ProfileURL),
				s.boolArg(attr.Verified), verSrc, s.nullTimeArg(verAt), s.timeArg(now)); err != nil {
				return err
			}
		} else if attr.Verified {
			_, _ = tx.ExecContext(ctx, s.rebind(`UPDATE creator_identities SET handle = COALESCE(?, handle), profile_url = CASE WHEN ? <> '' THEN ? ELSE profile_url END, verified = ?, verification_source = ?, verified_at = ? WHERE id = ?`),
				handlePtr, strings.TrimSpace(attr.ProfileURL), strings.TrimSpace(attr.ProfileURL), s.boolArg(true), verSrc, s.timeArg(now), existingIdent)
		}
	}

	var scID string
	_ = tx.QueryRowContext(ctx, s.rebind(`SELECT id FROM source_creators WHERE source_id = ? AND creator_id = ? AND role = ?`),
		src.ID, creatorID, role).Scan(&scID)
	if scID == "" {
		if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO source_creators (id, source_id, creator_id, role, provenance, created_at) VALUES (?, ?, ?, ?, ?, ?)`),
			uuid.NewString(), src.ID, creatorID, role, provenance, s.timeArg(now)); err != nil {
			return err
		}
	}
	return nil
}

// --- Occurrences ---

type OccurrenceInput struct {
	UserID       string
	EntryID      string
	OccurrenceID string
	MoveID       *string
	ProposedName *string
	StartMs      *int64
	EndMs        *int64
	Ordinal      int
	Notes        string
	State        domain.OccurrenceState
	TimingMethod domain.TimingMethod
	EvidenceIDs  []string
}

func (s *Store) listOccurrencesTx(ctx context.Context, qx queryExecer, entryID string) ([]domain.Occurrence, error) {
	q := s.rebind(`SELECT o.id, o.entry_id, o.move_id, COALESCE(m.canonical_name, ''), o.proposed_name,
		o.start_ms, o.end_ms, o.ordinal, o.notes, o.state, o.timing_method, o.evidence_ids_json,
		o.revision, o.created_at, o.updated_at
		FROM occurrences o
		LEFT JOIN moves m ON m.id = o.move_id
		WHERE o.entry_id = ?
		ORDER BY o.ordinal ASC, COALESCE(o.start_ms, 999999999) ASC, o.created_at ASC`)
	rows, err := qx.QueryContext(ctx, q, entryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Occurrence{}
	for rows.Next() {
		var (
			o                     domain.Occurrence
			moveID, propName      sql.NullString
			startMs, endMs        sql.NullInt64
			stateStr, methodStr   string
			evJSON                string
			createdRaw, updateRaw any
		)
		if err := rows.Scan(
			&o.ID, &o.EntryID, &moveID, &o.MoveName, &propName,
			&startMs, &endMs, &o.Ordinal, &o.Notes, &stateStr, &methodStr, &evJSON,
			&o.Revision, &createdRaw, &updateRaw,
		); err != nil {
			return nil, err
		}
		if moveID.Valid {
			o.MoveID = &moveID.String
		}
		if propName.Valid {
			o.ProposedName = &propName.String
		}
		if startMs.Valid {
			o.StartMs = &startMs.Int64
		}
		if endMs.Valid {
			o.EndMs = &endMs.Int64
		}
		o.State = domain.OccurrenceState(stateStr)
		o.TimingMethod = domain.TimingMethod(methodStr)
		_ = json.Unmarshal([]byte(evJSON), &o.EvidenceIDs)
		if o.EvidenceIDs == nil {
			o.EvidenceIDs = []string{}
		}
		o.CreatedAt, _ = parseDBTime(createdRaw)
		o.UpdatedAt, _ = parseDBTime(updateRaw)
		out = append(out, o)
	}
	return out, nil
}

func (s *Store) AddOccurrence(ctx context.Context, in OccurrenceInput) (*domain.Occurrence, error) {
	now := time.Now().UTC()
	var created *domain.Occurrence

	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		ent, err := s.getEntryTx(ctx, tx, in.UserID, in.EntryID)
		if err != nil {
			return err
		}
		if err := domain.ValidateOccurrenceBounds(in.StartMs, in.EndMs, ent.Source.DurationMs); err != nil {
			return err
		}

		moveID := cleanNullStr(in.MoveID)
		propName := cleanNullStr(in.ProposedName)
		if moveID == nil && propName == nil {
			return fmt.Errorf("%w: either move_id or proposed_name is required", domain.ErrValidation)
		}
		// If proposed_name provided without move_id, either link to matching canonical move or create a proposed term (4.2).
		if moveID == nil && propName != nil {
			mv, err := s.findOrProposeMoveTx(ctx, tx, *propName, &in.UserID)
			if err != nil {
				return err
			}
			moveID = &mv.ID
		}

		state := in.State
		if state == "" {
			state = domain.OccurrenceConfirmed
		}
		method := in.TimingMethod
		if method == "" {
			if in.StartMs == nil && in.EndMs == nil {
				method = domain.TimingUnknown
			} else {
				method = domain.TimingManual
			}
		}
		ord := in.Ordinal
		if ord <= 0 {
			ord = len(ent.Occurrences) + 1
		}

		occID := uuid.NewString()
		evJSON := marshalJSONString(in.EvidenceIDs, "[]")
		q := s.rebind(`INSERT INTO occurrences (
			id, entry_id, move_id, proposed_name, start_ms, end_ms, ordinal, notes,
			state, timing_method, evidence_ids_json, revision, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`)
		if _, err := tx.ExecContext(ctx, q,
			occID, ent.ID, moveID, propName, in.StartMs, in.EndMs, ord, strings.TrimSpace(in.Notes),
			string(state), string(method), evJSON, s.timeArg(now), s.timeArg(now),
		); err != nil {
			return err
		}

		newEntryRev := ent.Revision + 1
		if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE catalog_entries SET revision = ?, updated_at = ? WHERE id = ?`),
			newEntryRev, s.timeArg(now), ent.ID); err != nil {
			return err
		}

		occs, err := s.listOccurrencesTx(ctx, tx, ent.ID)
		if err != nil {
			return err
		}
		for i := range occs {
			if occs[i].ID == occID {
				created = &occs[i]
				break
			}
		}
		return s.appendSyncEventTx(ctx, tx, in.UserID, "occurrence", occID, "upsert", 1, created)
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

func (s *Store) UpdateOccurrence(ctx context.Context, in OccurrenceInput) (*domain.Occurrence, error) {
	now := time.Now().UTC()
	var updated *domain.Occurrence

	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		ent, err := s.getEntryTx(ctx, tx, in.UserID, in.EntryID)
		if err != nil {
			return err
		}
		if err := domain.ValidateOccurrenceBounds(in.StartMs, in.EndMs, ent.Source.DurationMs); err != nil {
			return err
		}
		var existing *domain.Occurrence
		for i := range ent.Occurrences {
			if ent.Occurrences[i].ID == in.OccurrenceID {
				existing = &ent.Occurrences[i]
				break
			}
		}
		if existing == nil {
			return domain.ErrNotFound
		}

		moveID := cleanNullStr(in.MoveID)
		propName := cleanNullStr(in.ProposedName)
		if moveID == nil && propName != nil {
			mv, err := s.findOrProposeMoveTx(ctx, tx, *propName, &in.UserID)
			if err != nil {
				return err
			}
			moveID = &mv.ID
		}
		if moveID == nil && propName == nil {
			moveID = existing.MoveID
			propName = existing.ProposedName
		}

		state := in.State
		if state == "" {
			state = domain.OccurrenceConfirmed
		}
		method := in.TimingMethod
		if method == "" {
			method = existing.TimingMethod
		}
		ord := in.Ordinal
		if ord <= 0 {
			ord = existing.Ordinal
		}
		evIDs := in.EvidenceIDs
		if evIDs == nil {
			evIDs = existing.EvidenceIDs
		}

		newOccRev := existing.Revision + 1
		if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE occurrences SET
			move_id = ?, proposed_name = ?, start_ms = ?, end_ms = ?, ordinal = ?, notes = ?,
			state = ?, timing_method = ?, evidence_ids_json = ?, revision = ?, updated_at = ?
			WHERE id = ? AND entry_id = ?`),
			moveID, propName, in.StartMs, in.EndMs, ord, strings.TrimSpace(in.Notes),
			string(state), string(method), marshalJSONString(evIDs, "[]"), newOccRev, s.timeArg(now),
			existing.ID, ent.ID,
		); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE catalog_entries SET revision = revision + 1, updated_at = ? WHERE id = ?`),
			s.timeArg(now), ent.ID); err != nil {
			return err
		}

		occs, err := s.listOccurrencesTx(ctx, tx, ent.ID)
		if err != nil {
			return err
		}
		for i := range occs {
			if occs[i].ID == existing.ID {
				updated = &occs[i]
				break
			}
		}
		return s.appendSyncEventTx(ctx, tx, in.UserID, "occurrence", existing.ID, "upsert", newOccRev, updated)
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *Store) DeleteOccurrence(ctx context.Context, userID, entryID, occID string) error {
	now := time.Now().UTC()
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		ent, err := s.getEntryTx(ctx, tx, userID, entryID)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM occurrences WHERE id = ? AND entry_id = ?`), occID, ent.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return domain.ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE catalog_entries SET revision = revision + 1, updated_at = ? WHERE id = ?`), s.timeArg(now), ent.ID); err != nil {
			return err
		}
		return s.appendSyncEventTx(ctx, tx, userID, "occurrence", occID, "delete", ent.Revision+1, map[string]string{"id": occID})
	})
}

// --- Moves, Aliases, Combinations (TAX-01) ---

type CreateMoveInput struct {
	CanonicalName string
	Disambiguator string
	StyleTags     []string
	Kind          string // "move" or "combination"
	CurationState string // "proposed" or "curated"
	Description   string
	CreatedBy     *string
	Steps         []CombinationStepInput
}

type CombinationStepInput struct {
	ChildMoveID string `json:"child_move_id"`
	Notes       string `json:"notes"`
}

func (s *Store) CreateMove(ctx context.Context, in CreateMoveInput) (*domain.Move, error) {
	name := strings.TrimSpace(in.CanonicalName)
	if name == "" {
		return nil, fmt.Errorf("%w: canonical_name is required", domain.ErrValidation)
	}
	kind := in.Kind
	if kind == "" {
		kind = "move"
	}
	if kind != "move" && kind != "combination" {
		return nil, fmt.Errorf("%w: invalid move kind %q", domain.ErrValidation, kind)
	}
	curation := in.CurationState
	if curation == "" {
		curation = "proposed"
	}

	now := time.Now().UTC()
	var created *domain.Move
	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		searchKey := domain.NormalizeSearchKey(name)
		disamb := strings.TrimSpace(in.Disambiguator)
		baseToken := domain.PortableMoveToken(name, "")

		// Check portable token collision across existing distinct moves (Section 12: stable reviewed disambiguators)
		if disamb == "" {
			var existingCount int
			if err := tx.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM moves WHERE portable_token = ?`), baseToken).Scan(&existingCount); err != nil {
				return err
			}
			if existingCount > 0 {
				disamb = fmt.Sprintf("V%d", existingCount+1)
			}
		}
		token := domain.PortableMoveToken(name, disamb)
		moveID := uuid.NewString()

		if kind == "combination" && len(in.Steps) > 0 {
			adj, err := s.loadCombinationGraphTx(ctx, tx)
			if err != nil {
				return err
			}
			childIDs := make([]string, len(in.Steps))
			for i, st := range in.Steps {
				childIDs[i] = st.ChildMoveID
			}
			if err := domain.DetectCombinationCycle(moveID, childIDs, adj); err != nil {
				return err
			}
		}

		q := s.rebind(`INSERT INTO moves (
			id, canonical_name, normalized_search_key, portable_token, disambiguator,
			style_tags_json, kind, curation_state, description, created_by, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if _, err := tx.ExecContext(ctx, q,
			moveID, name, searchKey, token, disamb,
			marshalJSONString(in.StyleTags, "[]"), kind, curation, strings.TrimSpace(in.Description),
			in.CreatedBy, s.timeArg(now), s.timeArg(now),
		); err != nil {
			return err
		}

		for idx, st := range in.Steps {
			if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO combination_steps (id, combination_move_id, ordinal, child_move_id, notes)
				VALUES (?, ?, ?, ?, ?)`), uuid.NewString(), moveID, idx+1, st.ChildMoveID, strings.TrimSpace(st.Notes)); err != nil {
				return err
			}
		}
		mv, err := s.getMoveTx(ctx, tx, moveID)
		if err != nil {
			return err
		}
		created = mv
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

func (s *Store) findOrProposeMoveTx(ctx context.Context, tx *sql.Tx, rawName string, userID *string) (*domain.Move, error) {
	normKey := domain.NormalizeSearchKey(rawName)
	if normKey == "" {
		return nil, fmt.Errorf("%w: move name cannot be empty", domain.ErrValidation)
	}
	var moveID string
	_ = tx.QueryRowContext(ctx, s.rebind(`SELECT id FROM moves WHERE normalized_search_key = ? AND curation_state <> 'rejected' ORDER BY CASE curation_state WHEN 'curated' THEN 0 ELSE 1 END LIMIT 1`), normKey).Scan(&moveID)
	if moveID != "" {
		return s.getMoveTx(ctx, tx, moveID)
	}
	// Check non-colliding alias match; if an alias collides across multiple moves, do NOT auto-merge (TAX-01)!
	rows, err := tx.QueryContext(ctx, s.rebind(`SELECT DISTINCT move_id FROM move_aliases WHERE normalized_alias = ?`), normKey)
	if err == nil {
		var matched []string
		for rows.Next() {
			var mid string
			if err := rows.Scan(&mid); err == nil {
				matched = append(matched, mid)
			}
		}
		rowErr := rows.Err()
		_ = rows.Close()
		if rowErr == nil && len(matched) == 1 {
			return s.getMoveTx(ctx, tx, matched[0])
		}
	}

	now := time.Now().UTC()
	name := strings.TrimSpace(rawName)
	baseToken := domain.PortableMoveToken(name, "")
	var existingCount int
	_ = tx.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM moves WHERE portable_token = ?`), baseToken).Scan(&existingCount)
	disamb := ""
	if existingCount > 0 {
		disamb = fmt.Sprintf("V%d", existingCount+1)
	}
	token := domain.PortableMoveToken(name, disamb)
	newID := uuid.NewString()
	if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO moves (
		id, canonical_name, normalized_search_key, portable_token, disambiguator,
		style_tags_json, kind, curation_state, description, created_by, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, '[]', 'move', 'proposed', '', ?, ?, ?)`),
		newID, name, normKey, token, disamb, userID, s.timeArg(now), s.timeArg(now),
	); err != nil {
		return nil, err
	}
	return s.getMoveTx(ctx, tx, newID)
}

// AddMoveAlias adds an alias to a move and surfaces collisions across distinct moves without silent merging (TAX-01).
func (s *Store) AddMoveAlias(ctx context.Context, moveID, alias, language, qualifier, provenance string) (*domain.MoveAlias, error) {
	alias = strings.TrimSpace(alias)
	normAlias := domain.NormalizeSearchKey(alias)
	if normAlias == "" {
		return nil, fmt.Errorf("%w: alias cannot be empty", domain.ErrValidation)
	}
	if language == "" {
		language = "es"
	}
	if provenance == "" {
		provenance = "manual"
	}
	now := time.Now().UTC()
	var out *domain.MoveAlias

	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.getMoveTx(ctx, tx, moveID); err != nil {
			return err
		}
		// Check if another move has the same canonical search key or normalized alias
		var otherMatches int
		if err := tx.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM (
			SELECT id AS mid FROM moves WHERE normalized_search_key = ? AND id <> ?
			UNION
			SELECT move_id AS mid FROM move_aliases WHERE normalized_alias = ? AND move_id <> ?
		) sub`), normAlias, moveID, normAlias, moveID).Scan(&otherMatches); err != nil {
			return err
		}
		collision := otherMatches > 0
		if collision {
			if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE move_aliases SET collision_flagged = ? WHERE normalized_alias = ?`),
				s.boolArg(true), normAlias); err != nil {
				return err
			}
		}

		al := &domain.MoveAlias{
			ID:               uuid.NewString(),
			MoveID:           moveID,
			Alias:            alias,
			NormalizedAlias:  normAlias,
			Language:         language,
			Qualifier:        strings.TrimSpace(qualifier),
			Provenance:       provenance,
			CollisionFlagged: collision,
			CreatedAt:        now,
		}
		if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO move_aliases (
			id, move_id, alias, normalized_alias, language, qualifier, provenance, collision_flagged, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			al.ID, al.MoveID, al.Alias, al.NormalizedAlias, al.Language, al.Qualifier,
			al.Provenance, s.boolArg(al.CollisionFlagged), s.timeArg(now),
		); err != nil {
			return err
		}
		out = al
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) SetCombinationSteps(ctx context.Context, combinationMoveID string, steps []CombinationStepInput) (*domain.Move, error) {
	var updated *domain.Move
	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		mv, err := s.getMoveTx(ctx, tx, combinationMoveID)
		if err != nil {
			return err
		}
		if mv.Kind != "combination" {
			return fmt.Errorf("%w: move %s is not a combination", domain.ErrValidation, combinationMoveID)
		}
		adj, err := s.loadCombinationGraphTx(ctx, tx)
		if err != nil {
			return err
		}
		children := make([]string, len(steps))
		for i, st := range steps {
			children[i] = st.ChildMoveID
			if _, err := s.getMoveTx(ctx, tx, st.ChildMoveID); err != nil {
				return fmt.Errorf("%w: child move %s not found", domain.ErrValidation, st.ChildMoveID)
			}
		}
		if err := domain.DetectCombinationCycle(combinationMoveID, children, adj); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM combination_steps WHERE combination_move_id = ?`), combinationMoveID); err != nil {
			return err
		}
		for idx, st := range steps {
			if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO combination_steps (id, combination_move_id, ordinal, child_move_id, notes) VALUES (?, ?, ?, ?, ?)`),
				uuid.NewString(), combinationMoveID, idx+1, st.ChildMoveID, strings.TrimSpace(st.Notes)); err != nil {
				return err
			}
		}
		updated, err = s.getMoveTx(ctx, tx, combinationMoveID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *Store) loadCombinationGraphTx(ctx context.Context, qx queryExecer) (map[string][]string, error) {
	rows, err := qx.QueryContext(ctx, `SELECT combination_move_id, child_move_id FROM combination_steps ORDER BY ordinal ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	adj := make(map[string][]string)
	for rows.Next() {
		var parent, child string
		if err := rows.Scan(&parent, &child); err != nil {
			return nil, err
		}
		adj[parent] = append(adj[parent], child)
	}
	return adj, nil
}

func (s *Store) ListMoves(ctx context.Context, query string, includeProposed bool) ([]domain.Move, error) {
	var conds []string
	var args []any
	if !includeProposed {
		conds = append(conds, "m.curation_state = 'curated'")
	} else {
		conds = append(conds, "m.curation_state <> 'rejected'")
	}
	if q := strings.TrimSpace(query); q != "" {
		nk := "%" + domain.NormalizeSearchKey(q) + "%"
		conds = append(conds, `(m.normalized_search_key LIKE ? OR EXISTS (SELECT 1 FROM move_aliases ma WHERE ma.move_id = m.id AND ma.normalized_alias LIKE ?))`)
		args = append(args, nk, nk)
	}
	sqlText := fmt.Sprintf(`SELECT m.id FROM moves m WHERE %s ORDER BY m.canonical_name ASC LIMIT 200`, strings.Join(conds, " AND "))
	rows, err := s.db.QueryContext(ctx, s.rebind(sqlText), args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	out := make([]domain.Move, 0, len(ids))
	for _, id := range ids {
		mv, err := s.getMoveTx(ctx, s.db, id)
		if err == nil {
			out = append(out, *mv)
		}
	}
	return out, nil
}

func (s *Store) GetMove(ctx context.Context, moveID string) (*domain.Move, error) {
	return s.getMoveTx(ctx, s.db, moveID)
}

func (s *Store) getMoveTx(ctx context.Context, qx queryExecer, moveID string) (*domain.Move, error) {
	q := s.rebind(`SELECT id, canonical_name, normalized_search_key, portable_token, disambiguator,
		style_tags_json, kind, curation_state, description, created_by, created_at, updated_at
		FROM moves WHERE id = ?`)
	var (
		m                     domain.Move
		tagsJSON              string
		createdBy             sql.NullString
		createdRaw, updateRaw any
	)
	if err := qx.QueryRowContext(ctx, q, moveID).Scan(
		&m.ID, &m.CanonicalName, &m.NormalizedSearchKey, &m.PortableToken, &m.Disambiguator,
		&tagsJSON, &m.Kind, &m.CurationState, &m.Description, &createdBy, &createdRaw, &updateRaw,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal([]byte(tagsJSON), &m.StyleTags)
	if m.StyleTags == nil {
		m.StyleTags = []string{}
	}
	if createdBy.Valid {
		m.CreatedBy = &createdBy.String
	}
	m.CreatedAt, _ = parseDBTime(createdRaw)
	m.UpdatedAt, _ = parseDBTime(updateRaw)

	alRows, err := qx.QueryContext(ctx, s.rebind(`SELECT id, move_id, alias, normalized_alias, language, qualifier, provenance, collision_flagged, created_at
		FROM move_aliases WHERE move_id = ? ORDER BY alias ASC`), m.ID)
	if err != nil {
		return nil, err
	}
	for alRows.Next() {
		var al domain.MoveAlias
		var colRaw, crRaw any
		if err := alRows.Scan(&al.ID, &al.MoveID, &al.Alias, &al.NormalizedAlias, &al.Language, &al.Qualifier, &al.Provenance, &colRaw, &crRaw); err != nil {
			_ = alRows.Close()
			return nil, err
		}
		al.CollisionFlagged = parseDBBool(colRaw)
		al.CreatedAt, _ = parseDBTime(crRaw)
		m.Aliases = append(m.Aliases, al)
	}
	_ = alRows.Close()

	if m.Kind == "combination" {
		stRows, err := qx.QueryContext(ctx, s.rebind(`SELECT cs.id, cs.combination_move_id, cs.ordinal, cs.child_move_id, mv.canonical_name, cs.notes
			FROM combination_steps cs
			JOIN moves mv ON mv.id = cs.child_move_id
			WHERE cs.combination_move_id = ?
			ORDER BY cs.ordinal ASC`), m.ID)
		if err != nil {
			return nil, err
		}
		for stRows.Next() {
			var st domain.CombinationStep
			if err := stRows.Scan(&st.ID, &st.CombinationMoveID, &st.Ordinal, &st.ChildMoveID, &st.ChildMoveName, &st.Notes); err != nil {
				_ = stRows.Close()
				return nil, err
			}
			m.Steps = append(m.Steps, st)
		}
		_ = stRows.Close()
	}
	return &m, nil
}

// --- Personal Sync & Portability / Backup ---

func (s *Store) appendSyncEventTx(ctx context.Context, tx *sql.Tx, userID, entityType, entityID, op string, rev int, payload any) error {
	q := s.rebind(`INSERT INTO sync_events (user_id, entity_type, entity_id, operation, revision, payload_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	_, err := tx.ExecContext(ctx, q, userID, entityType, entityID, op, rev, marshalJSONString(payload, "{}"), s.timeArg(time.Now().UTC()))
	return err
}

func (s *Store) GetPersonalSyncDeltas(ctx context.Context, userID string, cursor int64, limit int) ([]domain.SyncEvent, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := s.rebind(`SELECT seq, entity_type, entity_id, operation, revision, payload_json, created_at
		FROM sync_events WHERE user_id = ? AND seq > ? ORDER BY seq ASC LIMIT ?`)
	rows, err := s.db.QueryContext(ctx, q, userID, cursor, limit)
	if err != nil {
		return nil, cursor, err
	}
	defer rows.Close()
	var events []domain.SyncEvent
	nextCursor := cursor
	for rows.Next() {
		var ev domain.SyncEvent
		var crRaw any
		if err := rows.Scan(&ev.Seq, &ev.EntityType, &ev.EntityID, &ev.Operation, &ev.Revision, &ev.PayloadJSON, &crRaw); err != nil {
			return nil, cursor, err
		}
		ev.CreatedAt, _ = parseDBTime(crRaw)
		events = append(events, ev)
		if ev.Seq > nextCursor {
			nextCursor = ev.Seq
		}
	}
	if err := rows.Err(); err != nil {
		return nil, cursor, err
	}
	return events, nextCursor, nil
}

type PersonalExportBundle struct {
	SchemaVersion string                `json:"schema_version"`
	ExportedAt    time.Time             `json:"exported_at"`
	UserID        string                `json:"user_id"`
	Entries       []domain.CatalogEntry `json:"entries"`
	Moves         []domain.Move         `json:"moves"`
}

func (s *Store) ExportPersonalCatalog(ctx context.Context, userID string) (*PersonalExportBundle, error) {
	entries, err := s.ListEntries(ctx, EntryFilter{UserID: userID, Limit: 200})
	if err != nil {
		return nil, err
	}
	moves, err := s.ListMoves(ctx, "", true)
	if err != nil {
		return nil, err
	}
	return &PersonalExportBundle{
		SchemaVersion: "vueltario.personal.v1",
		ExportedAt:    time.Now().UTC(),
		UserID:        userID,
		Entries:       entries,
		Moves:         moves,
	}, nil
}

type BackupBundle struct {
	SchemaVersion string                `json:"schema_version"`
	CreatedAt     time.Time             `json:"created_at"`
	Moves         []domain.Move         `json:"moves"`
	Entries       []domain.CatalogEntry `json:"entries"`
}

// CreateBackupBundle exports administrator catalog state for portable restore drills (Phase 1 acceptance).
func (s *Store) CreateBackupBundle(ctx context.Context, userID string) (*BackupBundle, error) {
	entries, err := s.ListEntries(ctx, EntryFilter{UserID: userID, Limit: 200})
	if err != nil {
		return nil, err
	}
	moves, err := s.ListMoves(ctx, "", true)
	if err != nil {
		return nil, err
	}
	return &BackupBundle{
		SchemaVersion: "vueltario.backup.v1",
		CreatedAt:     time.Now().UTC(),
		Moves:         moves,
		Entries:       entries,
	}, nil
}

// RestoreBackupBundle idempotently restores moves and personal catalog entries into targetUserID.
func (s *Store) RestoreBackupBundle(ctx context.Context, targetUserID string, bundle BackupBundle) (int, error) {
	restored := 0
	moveNameMap := make(map[string]string)
	existingMoves, err := s.ListMoves(ctx, "", true)
	if err != nil {
		return 0, err
	}
	for _, m := range existingMoves {
		moveNameMap[m.NormalizedSearchKey] = m.ID
	}

	for _, m := range bundle.Moves {
		if _, ok := moveNameMap[m.NormalizedSearchKey]; ok {
			continue
		}
		created, err := s.CreateMove(ctx, CreateMoveInput{
			CanonicalName: m.CanonicalName,
			Disambiguator: m.Disambiguator,
			StyleTags:     m.StyleTags,
			Kind:          "move",
			CurationState: m.CurationState,
			Description:   m.Description,
			CreatedBy:     &targetUserID,
		})
		if err == nil {
			moveNameMap[created.NormalizedSearchKey] = created.ID
		}
	}

	for _, ent := range bundle.Entries {
		res, err := s.SaveLink(ctx, SaveLinkRequest{
			UserID:       targetUserID,
			RawURL:       ent.Source.OriginalURL,
			PrivateNotes: ent.PrivateNotes,
		})
		if err != nil {
			continue
		}
		restored++
		if ent.PersonalTitleOverride != nil || ent.PersonalCreatorOverride != nil {
			_, _ = s.UpdateEntry(ctx, UpdateEntryInput{
				UserID:                  targetUserID,
				EntryID:                 res.Entry.ID,
				ExpectedRevision:        res.Entry.Revision,
				PersonalTitleOverride:   ent.PersonalTitleOverride,
				PersonalCreatorOverride: ent.PersonalCreatorOverride,
			})
		}
		if len(res.Entry.Occurrences) == 0 {
			for _, occ := range ent.Occurrences {
				var targetMoveID *string
				if occ.MoveName != "" {
					if mid, ok := moveNameMap[domain.NormalizeSearchKey(occ.MoveName)]; ok {
						targetMoveID = &mid
					}
				}
				_, _ = s.AddOccurrence(ctx, OccurrenceInput{
					UserID:       targetUserID,
					EntryID:      res.Entry.ID,
					MoveID:       targetMoveID,
					ProposedName: occ.ProposedName,
					StartMs:      occ.StartMs,
					EndMs:        occ.EndMs,
					Ordinal:      occ.Ordinal,
					Notes:        occ.Notes,
					State:        occ.State,
					TimingMethod: occ.TimingMethod,
				})
			}
		}
	}
	return restored, nil
}

func cleanNullStr(p *string) *string {
	if p == nil {
		return nil
	}
	s := strings.TrimSpace(*p)
	if s == "" {
		return nil
	}
	return &s
}

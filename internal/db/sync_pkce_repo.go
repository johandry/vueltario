package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/johandry/vueltario/internal/domain"
)

// --- Native OIDC-Style Authorization Code with PKCE (S256) for iOS & Wails Desktop (Section 7 & 11) ---

type PKCEAuthorizeInput struct {
	Email               string
	Password            string
	ClientID            string
	RedirectURI         string
	CodeChallenge       string
	CodeChallengeMethod string
}

// CreatePKCEAuthorizationCode authenticates the user and issues a short-lived single-use authorization code
// bound to client_id, redirect_uri, and RFC 7636 S256 code_challenge.
func (s *Store) CreatePKCEAuthorizationCode(ctx context.Context, in PKCEAuthorizeInput) (string, error) {
	if strings.ToUpper(strings.TrimSpace(in.CodeChallengeMethod)) != "S256" {
		return "", fmt.Errorf("%w: code_challenge_method must be S256", domain.ErrValidation)
	}
	if len(strings.TrimSpace(in.CodeChallenge)) < 32 {
		return "", fmt.Errorf("%w: valid S256 code_challenge is required", domain.ErrValidation)
	}
	if strings.TrimSpace(in.ClientID) == "" || strings.TrimSpace(in.RedirectURI) == "" {
		return "", fmt.Errorf("%w: client_id and redirect_uri are required", domain.ErrValidation)
	}

	sess, err := s.AuthenticatePassword(ctx, in.Email, in.Password, "cli", time.Minute)
	if err != nil {
		return "", err
	}
	// Immediately discard temporary auth probe session; the token will be minted only on valid PKCE code_verifier exchange.
	_ = s.DeleteSessionByToken(ctx, sess.PlainToken)

	rawCode, err := randomTokenHex(32)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	exp := now.Add(5 * time.Minute)

	q := s.rebind(`INSERT INTO auth_pkce_codes (
		code_hash, user_id, client_id, redirect_uri, code_challenge, code_challenge_method, expires_at, created_at
	) VALUES (?, ?, ?, ?, ?, 'S256', ?, ?)`)
	if _, err := s.db.ExecContext(ctx, q,
		HashToken(rawCode), sess.User.ID, strings.TrimSpace(in.ClientID), strings.TrimSpace(in.RedirectURI),
		strings.TrimSpace(in.CodeChallenge), s.timeArg(exp), s.timeArg(now),
	); err != nil {
		return "", err
	}
	return rawCode, nil
}

type PKCETokenExchangeInput struct {
	Code         string
	ClientID     string
	RedirectURI  string
	CodeVerifier string
}

// ExchangePKCECode verifies the S256 code_verifier against the stored code_challenge, marks the code consumed
// atomically so replay fails, and issues a native bearer session token.
func (s *Store) ExchangePKCECode(ctx context.Context, in PKCETokenExchangeInput, ttl time.Duration) (*AuthSession, error) {
	verifier := strings.TrimSpace(in.CodeVerifier)
	if len(verifier) < 43 || len(verifier) > 128 {
		return nil, fmt.Errorf("%w: code_verifier must be 43..128 chars per RFC 7636", domain.ErrValidation)
	}
	sum := sha256.Sum256([]byte(verifier))
	computedChallenge := base64.RawURLEncoding.EncodeToString(sum[:])

	now := time.Now().UTC()
	var userID string

	err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		var clientID, redirectURI, challenge string
		var expRaw, consumedRaw any
		q := s.rebind(`SELECT user_id, client_id, redirect_uri, code_challenge, expires_at, consumed_at
			FROM auth_pkce_codes WHERE code_hash = ?`)
		if err := tx.QueryRowContext(ctx, q, HashToken(strings.TrimSpace(in.Code))).
			Scan(&userID, &clientID, &redirectURI, &challenge, &expRaw, &consumedRaw); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return domain.ErrUnauthorized
			}
			return err
		}
		if consumedAt, _ := parseNullDBTime(consumedRaw); consumedAt != nil {
			return domain.ErrUnauthorized
		}
		expAt, _ := parseDBTime(expRaw)
		if now.After(expAt) {
			return domain.ErrUnauthorized
		}
		if clientID != strings.TrimSpace(in.ClientID) || redirectURI != strings.TrimSpace(in.RedirectURI) {
			return domain.ErrUnauthorized
		}
		if challenge != computedChallenge {
			return domain.ErrUnauthorized
		}
		res, err := tx.ExecContext(ctx, s.rebind(`UPDATE auth_pkce_codes SET consumed_at = ? WHERE code_hash = ? AND consumed_at IS NULL`),
			s.timeArg(now), HashToken(strings.TrimSpace(in.Code)))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return domain.ErrUnauthorized
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	u, err := s.GetUserByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.CreateSession(ctx, *u, "cli", ttl)
}

// --- Offline-First Sync Mutations with Client IDs, Base Revisions & Tombstones (IOS-03, Section 11) ---

type SyncMutation struct {
	ClientMutationID string          `json:"client_mutation_id"`
	Operation        string          `json:"operation"` // "save_link", "update_entry", "delete_entry", "upsert_occurrence", "delete_occurrence"
	EntryID          string          `json:"entry_id,omitempty"`
	OccurrenceID     string          `json:"occurrence_id,omitempty"`
	BaseRevision     *int            `json:"base_revision,omitempty"`
	URL              string          `json:"url,omitempty"`
	PrivateNotes     *string         `json:"private_notes,omitempty"`
	TitleOverride    *string         `json:"title_override,omitempty"`
	CreatorOverride  *string         `json:"creator_override,omitempty"`
	Occurrence       *SyncOccPayload `json:"occurrence,omitempty"`
}

type SyncOccPayload struct {
	MoveID       *string `json:"move_id,omitempty"`
	ProposedName *string `json:"proposed_name,omitempty"`
	StartMs      *int64  `json:"start_ms,omitempty"`
	EndMs        *int64  `json:"end_ms,omitempty"`
	Notes        string  `json:"notes,omitempty"`
	TimingMethod string  `json:"timing_method,omitempty"`
}

type SyncMutationOutcome struct {
	ClientMutationID string               `json:"client_mutation_id"`
	Status           string               `json:"status"` // "applied", "duplicate_noop", "conflict", "error"
	Entry            *domain.CatalogEntry `json:"entry,omitempty"`
	Occurrence       *domain.Occurrence   `json:"occurrence,omitempty"`
	ServerCurrent    *domain.CatalogEntry `json:"server_current,omitempty"`
	ErrorMessage     string               `json:"error,omitempty"`
}

// ApplySyncMutations processes a batch of offline client mutations idempotently (by client_mutation_id),
// honoring base_revision optimistic concurrency so edits are never silently dropped on reconnect (IOS-03).
func (s *Store) ApplySyncMutations(ctx context.Context, userID string, mutations []SyncMutation) ([]SyncMutationOutcome, error) {
	outcomes := make([]SyncMutationOutcome, 0, len(mutations))

	for _, mut := range mutations {
		cid := strings.TrimSpace(mut.ClientMutationID)
		if cid == "" {
			outcomes = append(outcomes, SyncMutationOutcome{
				Status:       "error",
				ErrorMessage: "client_mutation_id is required",
			})
			continue
		}

		// Check idempotency ledger first so reconnect retries never duplicate mutations!
		if prev, ok := s.lookupSyncMutationLog(ctx, userID, cid); ok {
			prev.Status = "duplicate_noop"
			outcomes = append(outcomes, prev)
			continue
		}

		outcome := s.applySingleSyncMutation(ctx, userID, mut)
		if outcome.Status == "applied" {
			_ = s.recordSyncMutationLog(ctx, userID, cid, mut.Operation, outcome)
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}

func (s *Store) lookupSyncMutationLog(ctx context.Context, userID, clientMutationID string) (SyncMutationOutcome, bool) {
	var raw string
	err := s.db.QueryRowContext(ctx, s.rebind(`SELECT result_json FROM sync_mutation_log WHERE user_id = ? AND client_mutation_id = ?`),
		userID, clientMutationID).Scan(&raw)
	if err != nil {
		return SyncMutationOutcome{}, false
	}
	var out SyncMutationOutcome
	if json.Unmarshal([]byte(raw), &out) != nil {
		return SyncMutationOutcome{}, false
	}
	return out, true
}

func (s *Store) recordSyncMutationLog(ctx context.Context, userID, clientMutationID, op string, out SyncMutationOutcome) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, s.rebind(`INSERT INTO sync_mutation_log (
		client_mutation_id, user_id, operation, result_status, result_json, created_at
	) VALUES (?, ?, ?, ?, ?, ?)`),
		clientMutationID, userID, op, out.Status, marshalJSONString(out, "{}"), s.timeArg(now),
	)
	return err
}

func (s *Store) applySingleSyncMutation(ctx context.Context, userID string, mut SyncMutation) SyncMutationOutcome {
	out := SyncMutationOutcome{ClientMutationID: mut.ClientMutationID}

	switch mut.Operation {
	case "save_link":
		notes := ""
		if mut.PrivateNotes != nil {
			notes = *mut.PrivateNotes
		}
		res, err := s.SaveLink(ctx, SaveLinkRequest{
			UserID:          userID,
			RawURL:          mut.URL,
			ClientRequestID: mut.ClientMutationID,
			PrivateNotes:    notes,
		})
		if err != nil {
			out.Status = "error"
			out.ErrorMessage = err.Error()
			return out
		}
		out.Status = "applied"
		out.Entry = &res.Entry
		return out

	case "update_entry":
		if mut.BaseRevision == nil || *mut.BaseRevision <= 0 {
			out.Status = "error"
			out.ErrorMessage = "base_revision is required for update_entry"
			return out
		}
		updated, err := s.UpdateEntry(ctx, UpdateEntryInput{
			UserID:                  userID,
			EntryID:                 mut.EntryID,
			ExpectedRevision:        *mut.BaseRevision,
			PrivateNotes:            mut.PrivateNotes,
			PersonalTitleOverride:   mut.TitleOverride,
			PersonalCreatorOverride: mut.CreatorOverride,
		})
		if err != nil {
			if errors.Is(err, domain.ErrConflict) {
				cur, _ := s.GetEntry(ctx, userID, mut.EntryID)
				out.Status = "conflict"
				out.ServerCurrent = cur
				out.ErrorMessage = err.Error()
				return out
			}
			out.Status = "error"
			out.ErrorMessage = err.Error()
			return out
		}
		out.Status = "applied"
		out.Entry = updated
		return out

	case "delete_entry":
		// Tombstone deletion; verify base_revision if supplied so offline deletes don't clobber newer edits silently.
		if mut.BaseRevision != nil && *mut.BaseRevision > 0 {
			cur, err := s.GetEntry(ctx, userID, mut.EntryID)
			if errors.Is(err, domain.ErrNotFound) {
				out.Status = "applied"
				return out
			}
			if err != nil {
				out.Status = "error"
				out.ErrorMessage = err.Error()
				return out
			}
			if cur.Revision != *mut.BaseRevision {
				out.Status = "conflict"
				out.ServerCurrent = cur
				out.ErrorMessage = fmt.Sprintf("delete conflict: server revision %d != base_revision %d", cur.Revision, *mut.BaseRevision)
				return out
			}
		}
		if err := s.DeleteEntry(ctx, userID, mut.EntryID); err != nil && !errors.Is(err, domain.ErrNotFound) {
			out.Status = "error"
			out.ErrorMessage = err.Error()
			return out
		}
		out.Status = "applied"
		return out

	case "upsert_occurrence":
		if mut.Occurrence == nil {
			out.Status = "error"
			out.ErrorMessage = "occurrence payload is required"
			return out
		}
		in := OccurrenceInput{
			UserID:       userID,
			EntryID:      mut.EntryID,
			OccurrenceID: mut.OccurrenceID,
			MoveID:       mut.Occurrence.MoveID,
			ProposedName: mut.Occurrence.ProposedName,
			StartMs:      mut.Occurrence.StartMs,
			EndMs:        mut.Occurrence.EndMs,
			Notes:        mut.Occurrence.Notes,
			TimingMethod: domain.TimingMethod(mut.Occurrence.TimingMethod),
		}
		var (
			occ *domain.Occurrence
			err error
		)
		if mut.OccurrenceID != "" {
			occ, err = s.UpdateOccurrence(ctx, in)
		} else {
			occ, err = s.AddOccurrence(ctx, in)
		}
		if err != nil {
			out.Status = "error"
			out.ErrorMessage = err.Error()
			return out
		}
		ent, _ := s.GetEntry(ctx, userID, mut.EntryID)
		out.Status = "applied"
		out.Occurrence = occ
		out.Entry = ent
		return out

	case "delete_occurrence":
		if err := s.DeleteOccurrence(ctx, userID, mut.EntryID, mut.OccurrenceID); err != nil && !errors.Is(err, domain.ErrNotFound) {
			out.Status = "error"
			out.ErrorMessage = err.Error()
			return out
		}
		ent, _ := s.GetEntry(ctx, userID, mut.EntryID)
		out.Status = "applied"
		out.Entry = ent
		return out

	default:
		out.Status = "error"
		out.ErrorMessage = "unsupported sync operation: " + mut.Operation
		return out
	}
}

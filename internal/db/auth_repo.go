package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/johandry/vueltario/internal/domain"
)

type AuthSession struct {
	ID        string      `json:"id"`
	PlainToken string     `json:"token,omitempty"`
	CSRFToken string      `json:"csrf_token"`
	Kind      string      `json:"kind"`
	ExpiresAt time.Time   `json:"expires_at"`
	User      domain.User `json:"user"`
}

func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func randomTokenHex(numBytes int) (string, error) {
	buf := make([]byte, numBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func (s *Store) GetUserByEmail(ctx context.Context, email string) (*domain.User, error) {
	q := s.rebind(`SELECT id, email, external_subject, password_hash, display_name, role, locale, analysis_opt_out, created_at, updated_at, deleted_at
		FROM users WHERE email = ? AND deleted_at IS NULL`)
	return s.scanUser(s.db.QueryRowContext(ctx, q, strings.ToLower(strings.TrimSpace(email))))
}

func (s *Store) GetUserByID(ctx context.Context, id string) (*domain.User, error) {
	q := s.rebind(`SELECT id, email, external_subject, password_hash, display_name, role, locale, analysis_opt_out, created_at, updated_at, deleted_at
		FROM users WHERE id = ? AND deleted_at IS NULL`)
	return s.scanUser(s.db.QueryRowContext(ctx, q, id))
}

func (s *Store) scanUser(row *sql.Row) (*domain.User, error) {
	var (
		u                            domain.User
		extSub                       sql.NullString
		roleStr                      string
		optOutRaw, createdRaw, upRaw any
		delRaw                       any
	)
	if err := row.Scan(&u.ID, &u.Email, &extSub, &u.PasswordHash, &u.DisplayName, &roleStr, &u.Locale, &optOutRaw, &createdRaw, &upRaw, &delRaw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	if extSub.Valid {
		u.ExternalSubject = &extSub.String
	}
	u.Role = domain.Role(roleStr)
	u.AnalysisOptOut = parseDBBool(optOutRaw)
	u.CreatedAt, _ = parseDBTime(createdRaw)
	u.UpdatedAt, _ = parseDBTime(upRaw)
	u.DeletedAt, _ = parseNullDBTime(delRaw)
	return &u, nil
}

func (s *Store) AuthenticatePassword(ctx context.Context, email, password, kind string, ttl time.Duration) (*AuthSession, error) {
	u, err := s.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, domain.ErrUnauthorized
		}
		return nil, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return nil, domain.ErrUnauthorized
	}
	return s.CreateSession(ctx, *u, kind, ttl)
}

func (s *Store) CreateSession(ctx context.Context, user domain.User, kind string, ttl time.Duration) (*AuthSession, error) {
	if kind != "cli" {
		kind = "web"
	}
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	rawTok, err := randomTokenHex(32)
	if err != nil {
		return nil, err
	}
	csrfTok, err := randomTokenHex(24)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	exp := now.Add(ttl)
	sessID := uuid.NewString()

	q := s.rebind(`INSERT INTO sessions (id, user_id, token_hash, csrf_token, kind, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if _, err := s.db.ExecContext(ctx, q, sessID, user.ID, HashToken(rawTok), csrfTok, kind, s.timeArg(exp), s.timeArg(now)); err != nil {
		return nil, err
	}
	return &AuthSession{
		ID:         sessID,
		PlainToken: rawTok,
		CSRFToken:  csrfTok,
		Kind:       kind,
		ExpiresAt:  exp,
		User:       user,
	}, nil
}

func (s *Store) GetSessionByToken(ctx context.Context, plainToken string) (*AuthSession, error) {
	if strings.TrimSpace(plainToken) == "" {
		return nil, domain.ErrUnauthorized
	}
	q := s.rebind(`SELECT s.id, s.csrf_token, s.kind, s.expires_at,
		u.id, u.email, u.external_subject, u.password_hash, u.display_name, u.role, u.locale, u.analysis_opt_out, u.created_at, u.updated_at
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND u.deleted_at IS NULL`)
	var (
		sess                         AuthSession
		expRaw                       any
		extSub                       sql.NullString
		roleStr                      string
		optOutRaw, createdRaw, upRaw any
	)
	err := s.db.QueryRowContext(ctx, q, HashToken(plainToken)).Scan(
		&sess.ID, &sess.CSRFToken, &sess.Kind, &expRaw,
		&sess.User.ID, &sess.User.Email, &extSub, &sess.User.PasswordHash, &sess.User.DisplayName,
		&roleStr, &sess.User.Locale, &optOutRaw, &createdRaw, &upRaw,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrUnauthorized
		}
		return nil, err
	}
	sess.ExpiresAt, _ = parseDBTime(expRaw)
	if time.Now().UTC().After(sess.ExpiresAt) {
		_ = s.DeleteSessionByToken(ctx, plainToken)
		return nil, domain.ErrUnauthorized
	}
	if extSub.Valid {
		sess.User.ExternalSubject = &extSub.String
	}
	sess.User.Role = domain.Role(roleStr)
	sess.User.AnalysisOptOut = parseDBBool(optOutRaw)
	sess.User.CreatedAt, _ = parseDBTime(createdRaw)
	sess.User.UpdatedAt, _ = parseDBTime(upRaw)
	return &sess, nil
}

func (s *Store) DeleteSessionByToken(ctx context.Context, plainToken string) error {
	q := s.rebind(`DELETE FROM sessions WHERE token_hash = ?`)
	_, err := s.db.ExecContext(ctx, q, HashToken(plainToken))
	return err
}

func (s *Store) UpdateUserPreferences(ctx context.Context, userID, locale string, analysisOptOut bool) (*domain.User, error) {
	if locale == "" {
		locale = "en"
	}
	now := time.Now().UTC()
	q := s.rebind(`UPDATE users SET locale = ?, analysis_opt_out = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`)
	if _, err := s.db.ExecContext(ctx, q, locale, s.boolArg(analysisOptOut), s.timeArg(now), userID); err != nil {
		return nil, err
	}
	return s.GetUserByID(ctx, userID)
}

func (s *Store) CreateInvite(ctx context.Context, adminID, email string, role domain.Role, ttl time.Duration) (*domain.Invite, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || !strings.Contains(email, "@") {
		return nil, fmt.Errorf("%w: valid email is required", domain.ErrValidation)
	}
	if role != domain.RoleMember && role != domain.RoleTrustedContributor {
		role = domain.RoleMember
	}
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	rawCode, err := randomTokenHex(16)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	inv := &domain.Invite{
		ID:        uuid.NewString(),
		Email:     email,
		Role:      role,
		InvitedBy: adminID,
		ExpiresAt: now.Add(ttl),
		CreatedAt: now,
		PlainCode: rawCode,
	}
	q := s.rebind(`INSERT INTO invites (id, code_hash, email, role, invited_by, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if _, err := s.db.ExecContext(ctx, q, inv.ID, HashToken(rawCode), inv.Email, string(inv.Role), inv.InvitedBy, s.timeArg(inv.ExpiresAt), s.timeArg(now)); err != nil {
		return nil, err
	}
	_ = s.RecordAudit(ctx, &adminID, string(domain.RoleAdmin), "invite.created", "invite", inv.ID, 1, email, nil, "")
	return inv, nil
}

func (s *Store) RegisterMember(ctx context.Context, signupMode, inviteCode, email, password, displayName string) (*domain.User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	displayName = strings.TrimSpace(displayName)
	if email == "" || !strings.Contains(email, "@") {
		return nil, fmt.Errorf("%w: valid email is required", domain.ErrValidation)
	}
	if len(password) < 8 {
		return nil, fmt.Errorf("%w: password must be at least 8 characters", domain.ErrValidation)
	}
	if displayName == "" {
		displayName = strings.Split(email, "@")[0]
	}
	if signupMode == "admin_only" {
		return nil, fmt.Errorf("%w: community registration is disabled during admin-only pilot", domain.ErrForbidden)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	var createdUser *domain.User

	err = s.withWriteTx(ctx, func(tx *sql.Tx) error {
		role := domain.RoleMember
		var inviteID string
		if signupMode == "invited_moderated" {
			if strings.TrimSpace(inviteCode) == "" {
				return fmt.Errorf("%w: an invitation code is required to join", domain.ErrForbidden)
			}
			var invEmail, invRole string
			var expRaw, accRaw any
			qInv := s.rebind(`SELECT id, email, role, expires_at, accepted_at FROM invites WHERE code_hash = ?`)
			if err := tx.QueryRowContext(ctx, qInv, HashToken(strings.TrimSpace(inviteCode))).Scan(&inviteID, &invEmail, &invRole, &expRaw, &accRaw); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("%w: invalid invitation code", domain.ErrForbidden)
				}
				return err
			}
			if accAt, _ := parseNullDBTime(accRaw); accAt != nil {
				return fmt.Errorf("%w: invitation code already used", domain.ErrConflict)
			}
			expAt, _ := parseDBTime(expRaw)
			if now.After(expAt) {
				return fmt.Errorf("%w: invitation code has expired", domain.ErrForbidden)
			}
			if !strings.EqualFold(invEmail, email) {
				return fmt.Errorf("%w: invitation code was issued for a different email", domain.ErrForbidden)
			}
			role = domain.Role(invRole)
		}

		var existing int
		qCheck := s.rebind(`SELECT COUNT(*) FROM users WHERE email = ?`)
		if err := tx.QueryRowContext(ctx, qCheck, email).Scan(&existing); err != nil {
			return err
		}
		if existing > 0 {
			return fmt.Errorf("%w: email is already registered", domain.ErrConflict)
		}

		u := &domain.User{
			ID:           uuid.NewString(),
			Email:        email,
			PasswordHash: string(hash),
			DisplayName:  displayName,
			Role:         role,
			Locale:       "en",
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		qIns := s.rebind(`INSERT INTO users (id, email, password_hash, display_name, role, locale, analysis_opt_out, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if _, err := tx.ExecContext(ctx, qIns, u.ID, u.Email, u.PasswordHash, u.DisplayName, string(u.Role), u.Locale, s.boolArg(false), s.timeArg(now), s.timeArg(now)); err != nil {
			return err
		}
		if inviteID != "" {
			qAcc := s.rebind(`UPDATE invites SET accepted_by = ?, accepted_at = ? WHERE id = ?`)
			if _, err := tx.ExecContext(ctx, qAcc, u.ID, s.timeArg(now), inviteID); err != nil {
				return err
			}
		}
		createdUser = u
		return nil
	})
	if err != nil {
		return nil, err
	}
	return createdUser, nil
}

// DeleteAccount implements Section 13 account deletion:
// revokes sessions, scrubs private notes/overrides/private assets/credentials, while retaining independently curated public revisions and audit history.
func (s *Store) DeleteAccount(ctx context.Context, userID string) error {
	now := time.Now().UTC()
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM sessions WHERE user_id = ?`), userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM evidence WHERE owner_user_id = ? AND access_scope = 'private_user'`), userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE media_assets SET status = 'revoked', deleted_at = ?, updated_at = ? WHERE owner_user_id = ? AND access_scope = 'private'`), s.timeArg(now), s.timeArg(now), userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM catalog_entries WHERE user_id = ?`), userID); err != nil {
			return err
		}
		scrubEmail := fmt.Sprintf("deleted-%s@redacted.invalid", userID)
		if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE users SET email = ?, external_subject = NULL, password_hash = 'REDACTED', display_name = 'Deleted User', deleted_at = ?, updated_at = ? WHERE id = ?`),
			scrubEmail, s.timeArg(now), s.timeArg(now), userID); err != nil {
			return err
		}
		return nil
	})
}

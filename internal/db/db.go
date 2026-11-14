package db

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"

	"github.com/johandry/vueltario/internal/domain"
)

//go:embed migrations/postgres/*.sql migrations/sqlite/*.sql
var migrationsFS embed.FS

type Store struct {
	db      *sql.DB
	dialect string // "postgres" or "sqlite"
	writeMu sync.Mutex
}

func Open(driver, dsn string) (*Store, error) {
	driver = strings.ToLower(strings.TrimSpace(driver))
	var sqlDriver string
	switch driver {
	case "postgres":
		sqlDriver = "pgx"
	case "sqlite":
		sqlDriver = "sqlite"
		if dsn != ":memory:" && !strings.HasPrefix(dsn, "file:") {
			if dir := filepath.Dir(dsn); dir != "" && dir != "." {
				if err := os.MkdirAll(dir, 0o750); err != nil {
					return nil, fmt.Errorf("create sqlite directory: %w", err)
				}
			}
		}
	default:
		return nil, fmt.Errorf("unsupported db driver: %s", driver)
	}

	conn, err := sql.Open(sqlDriver, dsn)
	if err != nil {
		return nil, err
	}

	if driver == "sqlite" {
		// Serialize writer connections cleanly in SQLite and enable FK constraints + WAL.
		if strings.Contains(dsn, ":memory:") {
			conn.SetMaxOpenConns(1)
		} else {
			conn.SetMaxOpenConns(4)
		}
		for _, pragma := range []string{
			"PRAGMA foreign_keys = ON;",
			"PRAGMA busy_timeout = 5000;",
			"PRAGMA journal_mode = WAL;",
		} {
			if _, err := conn.Exec(pragma); err != nil {
				_ = conn.Close()
				return nil, fmt.Errorf("sqlite pragma %q: %w", pragma, err)
			}
		}
	} else {
		conn.SetMaxOpenConns(20)
		conn.SetMaxIdleConns(5)
		conn.SetConnMaxLifetime(30 * time.Minute)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.PingContext(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ping %s: %w", driver, err)
	}

	return &Store{db: conn, dialect: driver}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Dialect() string {
	return s.dialect
}

func (s *Store) RawDB() *sql.DB {
	return s.db
}

func (s *Store) MigrateUp(ctx context.Context) error {
	path := fmt.Sprintf("migrations/%s/0001_initial.up.sql", s.dialect)
	raw, err := migrationsFS.ReadFile(path)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, string(raw)); err != nil {
		return fmt.Errorf("migrate up (%s): %w", s.dialect, err)
	}
	return nil
}

func (s *Store) MigrateDown(ctx context.Context) error {
	path := fmt.Sprintf("migrations/%s/0001_initial.down.sql", s.dialect)
	raw, err := migrationsFS.ReadFile(path)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, string(raw)); err != nil {
		return fmt.Errorf("migrate down (%s): %w", s.dialect, err)
	}
	return nil
}

// EnsureBootstrap initializes default curated Cuban dance moves and optional bootstrap admin account.
func (s *Store) EnsureBootstrap(ctx context.Context, adminEmail, adminPassword string) (*domain.User, error) {
	var admin *domain.User
	if strings.TrimSpace(adminEmail) != "" && strings.TrimSpace(adminPassword) != "" {
		u, err := s.EnsureAdminUser(ctx, adminEmail, adminPassword)
		if err != nil {
			return nil, err
		}
		admin = u
	}
	if err := s.SeedCuratedMoves(ctx); err != nil {
		return nil, err
	}
	return admin, nil
}

func (s *Store) EnsureAdminUser(ctx context.Context, email, password string) (*domain.User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	u, err := s.GetUserByEmail(ctx, email)
	if err == nil {
		return u, nil
	}
	if err != domain.ErrNotFound {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	user := &domain.User{
		ID:           uuid.NewString(),
		Email:        email,
		PasswordHash: string(hash),
		DisplayName:  "Administrator",
		Role:         domain.RoleAdmin,
		Locale:       "en",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	q := s.rebind(`INSERT INTO users (id, email, password_hash, display_name, role, locale, analysis_opt_out, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if _, err := s.db.ExecContext(ctx, q,
		user.ID, user.Email, user.PasswordHash, user.DisplayName, string(user.Role),
		user.Locale, s.boolArg(false), s.timeArg(now), s.timeArg(now),
	); err != nil {
		return nil, err
	}
	return user, nil
}

type seedMoveDef struct {
	Name    string
	Styles  []string
	Aliases []struct {
		Alias     string
		Qualifier string
	}
}

var defaultCubanMoves = []seedMoveDef{
	{Name: "Dile que no", Styles: []string{"casino", "rueda"}},
	{Name: "Guapea", Styles: []string{"casino", "rueda"}},
	{Name: "Enchufla", Styles: []string{"casino", "rueda"}, Aliases: []struct{ Alias, Qualifier string }{{"Enchufla simple", "general"}, {"Enchufe", "regional"}}},
	{Name: "Enchufla doble", Styles: []string{"casino", "rueda"}},
	{Name: "Vacilala", Styles: []string{"casino", "rueda"}, Aliases: []struct{ Alias, Qualifier string }{{"Vacílala", "orthographic"}}},
	{Name: "Vacilala con la mano", Styles: []string{"casino", "rueda"}},
	{Name: "Exhibela", Styles: []string{"casino", "rueda"}, Aliases: []struct{ Alias, Qualifier string }{{"Exhíbela", "orthographic"}, {"Sácala", "Havana/classic"}}},
	{Name: "Setenta", Styles: []string{"casino", "rueda"}, Aliases: []struct{ Alias, Qualifier string }{{"70", "numeric"}}},
	{Name: "Setenta complicado", Styles: []string{"casino", "rueda"}},
	{Name: "Sombrero", Styles: []string{"casino", "rueda"}},
	{Name: "Kentucky", Styles: []string{"casino", "rueda"}},
	{Name: "Prima", Styles: []string{"casino", "rueda"}, Aliases: []struct{ Alias, Qualifier string }{{"Adiós", "Miami/rueda"}}},
	{Name: "Prima con la hermana", Styles: []string{"casino", "rueda"}, Aliases: []struct{ Alias, Qualifier string }{{"Adiós con la hermana", "Miami/rueda"}}},
	{Name: "Dame", Styles: []string{"rueda"}},
	{Name: "Dame dos", Styles: []string{"rueda"}},
	{Name: "Paseala", Styles: []string{"casino", "rueda"}},
	{Name: "Montaña", Styles: []string{"casino", "rueda"}},
	{Name: "Evelyn", Styles: []string{"casino"}},
	{Name: "Dedos", Styles: []string{"casino"}},
	{Name: "Tornillo", Styles: []string{"timba", "casino"}},
	{Name: "Tembleque", Styles: []string{"timba"}},
	{Name: "Rumba guaguancó paso básico", Styles: []string{"timba", "afro-cuban"}},
}

func (s *Store) SeedCuratedMoves(ctx context.Context) error {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM moves`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	for _, m := range defaultCubanMoves {
		mv, err := s.CreateMove(ctx, CreateMoveInput{
			CanonicalName: m.Name,
			StyleTags:     m.Styles,
			Kind:          "move",
			CurationState: "curated",
		})
		if err != nil {
			return err
		}
		for _, al := range m.Aliases {
			if _, err := s.AddMoveAlias(ctx, mv.ID, al.Alias, "es", al.Qualifier, "seed"); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- SQL Dialect Helpers ---

func (s *Store) rebind(query string) string {
	if s.dialect != "postgres" {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 16)
	idx := 1
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(idx))
			idx++
		} else {
			b.WriteByte(query[i])
		}
	}
	return b.String()
}

func (s *Store) timeArg(t time.Time) any {
	u := t.UTC()
	if s.dialect == "sqlite" {
		return u.Format(time.RFC3339Nano)
	}
	return u
}

func (s *Store) nullTimeArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return s.timeArg(*t)
}

func (s *Store) boolArg(v bool) any {
	if s.dialect == "sqlite" {
		if v {
			return 1
		}
		return 0
	}
	return v
}

func parseDBTime(raw any) (time.Time, error) {
	switch v := raw.(type) {
	case time.Time:
		return v.UTC(), nil
	case string:
		if v == "" {
			return time.Time{}, nil
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05"} {
			if t, err := time.Parse(layout, v); err == nil {
				return t.UTC(), nil
			}
		}
		return time.Time{}, fmt.Errorf("cannot parse time %q", v)
	case []byte:
		return parseDBTime(string(v))
	default:
		return time.Time{}, fmt.Errorf("unexpected time type %T", raw)
	}
}

func parseNullDBTime(raw any) (*time.Time, error) {
	if raw == nil {
		return nil, nil
	}
	t, err := parseDBTime(raw)
	if err != nil || t.IsZero() {
		return nil, err
	}
	return &t, nil
}

func parseDBBool(raw any) bool {
	switch v := raw.(type) {
	case bool:
		return v
	case int64:
		return v != 0
	case int:
		return v != 0
	case string:
		return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "t")
	case []byte:
		return parseDBBool(string(v))
	default:
		return false
	}
}

func marshalJSONString(v any, emptyFallback string) string {
	if v == nil {
		return emptyFallback
	}
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return emptyFallback
	}
	return string(b)
}

func (s *Store) withWriteTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if s.dialect == "sqlite" {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

type queryExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

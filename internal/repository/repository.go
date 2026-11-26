package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"sql-manager/internal/entity"
	appsql "sql-manager/sql"

	_ "modernc.org/sqlite"
)

// LocalDB wraps the local SQLite app.db metadata database.
// Strictly manages local application data; never connects to user target databases.
type LocalDB struct {
	db *sql.DB
}

// OpenLocalDB opens (or creates) app.db with WAL mode, foreign keys, and embedded schema.
func OpenLocalDB(dbPath string) (*LocalDB, error) {
	dsn := buildLocalDBDSN(dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open local app.db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(appsql.InitSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate local app.db: %w", err)
	}
	return &LocalDB{db: db}, nil
}

func buildLocalDBDSN(dbPath string) string {
	trimmed := strings.TrimSpace(dbPath)
	if trimmed == ":memory:" || strings.HasPrefix(trimmed, "file::memory:") {
		return "file::memory:?_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	}
	absPath, err := filepath.Abs(trimmed)
	if err != nil {
		absPath = trimmed
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "journal_mode(WAL)")
	return buildLocalFileURI(filepath.ToSlash(absPath), q)
}

func buildLocalFileURI(slashPath string, q url.Values) string {
	if strings.HasPrefix(slashPath, "//?/") && len(slashPath) >= 6 && slashPath[5] == ':' {
		c := slashPath[4]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
			slashPath = "/" + slashPath[4:]
		}
	} else if !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}
	u := url.URL{
		Scheme:   "file",
		Path:     slashPath,
		RawQuery: q.Encode(),
	}
	return u.String()
}

func (l *LocalDB) Close() error {
	if l == nil || l.db == nil {
		return nil
	}
	return l.db.Close()
}

// EmbeddedSQLiteVersion returns the exact SQLite version compiled into modernc.org/sqlite.
func (l *LocalDB) EmbeddedSQLiteVersion(ctx context.Context) (string, error) {
	var ver string
	err := l.db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&ver)
	return ver, err
}

// --- Connection Repository ---

func (l *LocalDB) ListConnections(ctx context.Context) ([]entity.ConnectionEntity, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT id, group_id, name, driver, env_color, read_only, config_json, COALESCE(secret_ref, ''), sort_order, created_at, updated_at
		FROM connections
		ORDER BY sort_order ASC, updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []entity.ConnectionEntity
	for rows.Next() {
		var e entity.ConnectionEntity
		var groupID sql.NullString
		var ro int
		if err := rows.Scan(&e.ID, &groupID, &e.Name, &e.Driver, &e.EnvColor, &ro, &e.ConfigJSON, &e.SecretRef, &e.SortOrder, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		if groupID.Valid {
			v := groupID.String
			e.GroupID = &v
		}
		e.ReadOnly = ro != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

func (l *LocalDB) GetConnection(ctx context.Context, id string) (*entity.ConnectionEntity, error) {
	var e entity.ConnectionEntity
	var groupID sql.NullString
	var ro int
	err := l.db.QueryRowContext(ctx, `
		SELECT id, group_id, name, driver, env_color, read_only, config_json, COALESCE(secret_ref, ''), sort_order, created_at, updated_at
		FROM connections WHERE id = ?`, id).
		Scan(&e.ID, &groupID, &e.Name, &e.Driver, &e.EnvColor, &ro, &e.ConfigJSON, &e.SecretRef, &e.SortOrder, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if groupID.Valid {
		v := groupID.String
		e.GroupID = &v
	}
	e.ReadOnly = ro != 0
	return &e, nil
}

func (l *LocalDB) UpsertConnection(ctx context.Context, e entity.ConnectionEntity) error {
	ro := 0
	if e.ReadOnly {
		ro = 1
	}
	var groupID any
	if e.GroupID != nil && *e.GroupID != "" {
		groupID = *e.GroupID
	}
	_, err := l.db.ExecContext(ctx, `
		INSERT INTO connections (id, group_id, name, driver, env_color, read_only, config_json, secret_ref, sort_order, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			group_id = excluded.group_id,
			name = excluded.name,
			driver = excluded.driver,
			env_color = excluded.env_color,
			read_only = excluded.read_only,
			config_json = excluded.config_json,
			secret_ref = excluded.secret_ref,
			sort_order = excluded.sort_order,
			updated_at = excluded.updated_at`,
		e.ID, groupID, e.Name, e.Driver, e.EnvColor, ro, e.ConfigJSON, e.SecretRef, e.SortOrder, e.CreatedAt, e.UpdatedAt,
	)
	return err
}

func (l *LocalDB) DeleteConnection(ctx context.Context, id string) error {
	_, err := l.db.ExecContext(ctx, `DELETE FROM connections WHERE id = ?`, id)
	return err
}

// --- Query History Repository ---

func (l *LocalDB) AppendHistory(ctx context.Context, h entity.QueryHistoryEntity) error {
	_, err := l.db.ExecContext(ctx, `
		INSERT INTO query_history (id, connection_id, database_name, sql_text, status, duration_ms, affected_rows, error_code, executed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		h.ID, h.ConnectionID, h.DatabaseName, h.SQLText, h.Status, h.DurationMs, h.AffectedRows, h.ErrorCode, h.ExecutedAt,
	)
	return err
}

func (l *LocalDB) ListHistory(ctx context.Context, connID string, limit int) ([]entity.QueryHistoryEntity, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var rows *sql.Rows
	var err error
	if connID != "" {
		rows, err = l.db.QueryContext(ctx, `
			SELECT id, connection_id, database_name, sql_text, status, duration_ms, affected_rows, error_code, executed_at
			FROM query_history WHERE connection_id = ?
			ORDER BY executed_at DESC LIMIT ?`, connID, limit)
	} else {
		rows, err = l.db.QueryContext(ctx, `
			SELECT id, connection_id, database_name, sql_text, status, duration_ms, affected_rows, error_code, executed_at
			FROM query_history
			ORDER BY executed_at DESC LIMIT ?`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []entity.QueryHistoryEntity
	for rows.Next() {
		var h entity.QueryHistoryEntity
		if err := rows.Scan(&h.ID, &h.ConnectionID, &h.DatabaseName, &h.SQLText, &h.Status, &h.DurationMs, &h.AffectedRows, &h.ErrorCode, &h.ExecutedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// --- App KV Repository ---

func (l *LocalDB) GetKV(ctx context.Context, key string) (string, bool, error) {
	var val string
	err := l.db.QueryRowContext(ctx, `SELECT value_json FROM app_kv WHERE key = ?`, key).Scan(&val)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return val, true, nil
}

func (l *LocalDB) SetKV(ctx context.Context, key, valueJSON string) error {
	_, err := l.db.ExecContext(ctx, `
		INSERT INTO app_kv (key, value_json, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at`,
		key, valueJSON, time.Now().UnixMilli(),
	)
	return err
}

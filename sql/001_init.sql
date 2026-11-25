-- Local SQLite metadata database schema for sql-manager (app.db)
-- Stores connection profiles (without plaintext secrets), groups, query history, saved queries, and UI state.

CREATE TABLE IF NOT EXISTS connection_groups (
    id          TEXT PRIMARY KEY,
    parent_id   TEXT REFERENCES connection_groups(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    sort_order  INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS connections (
    id             TEXT PRIMARY KEY,
    group_id       TEXT REFERENCES connection_groups(id) ON DELETE SET NULL,
    name           TEXT NOT NULL,
    driver         TEXT NOT NULL,
    env_color      TEXT NOT NULL DEFAULT 'default',
    read_only      INTEGER NOT NULL DEFAULT 0,
    config_json    TEXT NOT NULL,
    secret_ref     TEXT,
    sort_order     INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS query_history (
    id             TEXT PRIMARY KEY,
    connection_id  TEXT NOT NULL,
    database_name  TEXT NOT NULL DEFAULT '',
    sql_text       TEXT NOT NULL,
    status         TEXT NOT NULL,
    duration_ms    INTEGER NOT NULL,
    affected_rows  INTEGER NOT NULL DEFAULT 0,
    error_code     TEXT NOT NULL DEFAULT '',
    executed_at    INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_query_history_conn_time
    ON query_history(connection_id, executed_at DESC);

CREATE TABLE IF NOT EXISTS saved_queries (
    id             TEXT PRIMARY KEY,
    connection_id  TEXT NOT NULL DEFAULT '',
    title          TEXT NOT NULL,
    folder         TEXT NOT NULL DEFAULT '',
    sql_text       TEXT NOT NULL,
    updated_at     INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS app_kv (
    key            TEXT PRIMARY KEY,
    value_json     TEXT NOT NULL,
    updated_at     INTEGER NOT NULL
);

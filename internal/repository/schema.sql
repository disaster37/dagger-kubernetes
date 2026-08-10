-- v1
CREATE TABLE IF NOT EXISTS users (
    id             TEXT PRIMARY KEY,
    username       TEXT NOT NULL UNIQUE COLLATE NOCASE,
    role           TEXT NOT NULL CHECK (role IN ('admin', 'user')),
    password_hash  TEXT NOT NULL DEFAULT '',
    oauth_provider TEXT NOT NULL DEFAULT '',
    oauth_id       TEXT NOT NULL DEFAULT '',
    created_at     DATETIME NOT NULL,
    updated_at     DATETIME NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_oauth
    ON users(oauth_provider, oauth_id) WHERE oauth_provider != '';

CREATE TABLE IF NOT EXISTS groups (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL UNIQUE COLLATE NOCASE,
    description         TEXT NOT NULL DEFAULT '',
    max_runner_sessions INTEGER NOT NULL DEFAULT 0,   -- 0 = unlimited
    agent_available     INTEGER NOT NULL DEFAULT 1,
    auto_assign_pattern TEXT NOT NULL DEFAULT '',
    created_at          DATETIME NOT NULL,
    updated_at          DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS user_groups (
    user_id    TEXT NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
    group_id   TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    created_at DATETIME NOT NULL,
    PRIMARY KEY (user_id, group_id)
);
CREATE INDEX IF NOT EXISTS idx_user_groups_group ON user_groups(group_id);

CREATE TABLE IF NOT EXISTS api_tokens (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
    token_hash   TEXT NOT NULL UNIQUE,
    prefix       TEXT NOT NULL DEFAULT '',
    created_at   DATETIME NOT NULL,
    last_used_at DATETIME
);

CREATE TABLE IF NOT EXISTS projects (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE COLLATE NOCASE,
    group_id   TEXT REFERENCES groups(id) ON DELETE SET NULL, -- NULL = unassigned
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_projects_group ON projects(group_id);

CREATE TABLE IF NOT EXISTS trace_meta (
    trace_id     TEXT PRIMARY KEY,
    user_id      TEXT REFERENCES users(id)  ON DELETE SET NULL,
    group_id     TEXT REFERENCES groups(id) ON DELETE SET NULL, -- NULL = unassigned
    project_name TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT '',
    version      TEXT NOT NULL DEFAULT '',
    ci_provider  TEXT NOT NULL DEFAULT '',
    ci_repo      TEXT NOT NULL DEFAULT '',
    duration_ms  INTEGER NOT NULL DEFAULT 0,
    started_at   DATETIME,
    updated_at   DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_trace_meta_group ON trace_meta(group_id);
CREATE INDEX IF NOT EXISTS idx_trace_meta_user  ON trace_meta(user_id);

CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    applied_at DATETIME NOT NULL
);

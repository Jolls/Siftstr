-- Full multi-user schema (ARCHITECTURE.md section 10). Every user-owned table
-- has user_id from the start. Timestamps are RFC 3339 UTC text.

CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('admin', 'user')),
    timezone      TEXT NOT NULL DEFAULT 'UTC',
    disabled      INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL
);

CREATE TABLE sessions (
    id           TEXT PRIMARY KEY, -- hash of the session token
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   TEXT NOT NULL,
    expires_at   TEXT NOT NULL,
    last_seen_at TEXT NOT NULL
);
CREATE INDEX sessions_user ON sessions(user_id);

CREATE TABLE api_keys (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key_hash     TEXT NOT NULL UNIQUE,
    label        TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    last_used_at TEXT,
    revoked_at   TEXT
);
CREATE INDEX api_keys_user ON api_keys(user_id);

CREATE TABLE connections (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('miniflux', 'karakeep', 'metube', 'youtube', 'nextcloud', 'nostr')),
    base_url    TEXT NOT NULL DEFAULT '',
    secret_enc  BLOB, -- encrypted at rest; never returned once saved
    config_json TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX connections_user ON connections(user_id);

CREATE TABLE sources (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind            TEXT NOT NULL CHECK (kind IN ('miniflux_feed', 'nostr')),
    external_id     TEXT NOT NULL,
    name            TEXT NOT NULL,
    category        TEXT NOT NULL DEFAULT '',
    granularity     TEXT NOT NULL DEFAULT 'individual' CHECK (granularity IN ('individual', 'digest')),
    max_depth       TEXT NOT NULL DEFAULT 'deep' CHECK (max_depth IN ('light', 'deep')),
    carryover       TEXT NOT NULL DEFAULT 'carry' CHECK (carryover IN ('carry', 'drop')),
    prompt_override TEXT,
    enabled         INTEGER NOT NULL DEFAULT 1,
    UNIQUE (user_id, kind, external_id),
    UNIQUE (user_id, id)
);

CREATE TABLE digests (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    source_id  TEXT NOT NULL,
    batch_date TEXT NOT NULL,
    summary    TEXT,
    state      TEXT NOT NULL,
    UNIQUE (user_id, id),
    FOREIGN KEY (user_id, source_id) REFERENCES sources(user_id, id) ON DELETE CASCADE
);

CREATE TABLE items (
    id            TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    source_id     TEXT NOT NULL,
    external_id   TEXT NOT NULL,
    media_type    TEXT NOT NULL CHECK (media_type IN ('article', 'video', 'podcast', 'post')),
    url           TEXT NOT NULL,
    title         TEXT NOT NULL,
    excerpt       TEXT NOT NULL DEFAULT '',
    content       TEXT,
    state         TEXT NOT NULL CHECK (state IN ('pending_light', 'light', 'pending_deep', 'deep', 'archived', 'kept', 'expired')),
    light_summary TEXT,
    deep_summary  TEXT,
    promoted_at   TEXT,
    batch_date    TEXT,
    digest_id     TEXT,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    UNIQUE (user_id, source_id, external_id),
    UNIQUE (user_id, id),
    FOREIGN KEY (user_id, source_id) REFERENCES sources(user_id, id) ON DELETE CASCADE,
    FOREIGN KEY (user_id, digest_id) REFERENCES digests(user_id, id)
);
CREATE INDEX items_user_state ON items(user_id, state);
CREATE INDEX items_created ON items(created_at);

CREATE TABLE actions (
    action_id        TEXT PRIMARY KEY, -- client-generated UUID; idempotency key
    user_id          TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    subject_type     TEXT NOT NULL CHECK (subject_type IN ('item', 'digest')),
    subject_id       TEXT NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('archive', 'promote', 'keep', 'undo')),
    target_action_id TEXT,
    client_at        TEXT NOT NULL,
    received_at      TEXT NOT NULL,
    release_at       TEXT NOT NULL,
    status           TEXT NOT NULL CHECK (status IN ('held', 'released', 'cancelled', 'rejected'))
);
CREATE INDEX actions_user_subject ON actions(user_id, subject_type, subject_id);
CREATE INDEX actions_release ON actions(status, release_at);

CREATE TABLE outbox (
    id            TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    action_id     TEXT REFERENCES actions(action_id) ON DELETE CASCADE,
    connection_id TEXT REFERENCES connections(id) ON DELETE SET NULL,
    op            TEXT NOT NULL,
    payload       TEXT NOT NULL DEFAULT '{}',
    status        TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'done', 'failed')),
    attempts      INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT,
    created_at    TEXT NOT NULL,
    done_at       TEXT
);
CREATE INDEX outbox_pending ON outbox(status, created_at);

CREATE TABLE runs (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    started_at   TEXT NOT NULL,
    summaries_at TEXT,
    finished_at  TEXT,
    stats        TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX runs_user ON runs(user_id, started_at);

CREATE TABLE user_settings (
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key     TEXT NOT NULL,
    value   TEXT NOT NULL,
    PRIMARY KEY (user_id, key)
);

CREATE TABLE instance_settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

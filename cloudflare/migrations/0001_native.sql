PRAGMA foreign_keys = ON;
CREATE TABLE users (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 name TEXT NOT NULL COLLATE NOCASE UNIQUE,
 password_hash TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','active','rejected','deleted')),
 approved_at TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, deleted_at TEXT
);
CREATE TABLE tokens (
 id TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id), hash TEXT NOT NULL UNIQUE,
 expires_at TEXT NOT NULL, revoked_at TEXT, created_at TEXT NOT NULL
);
CREATE TABLE login_sessions (
 hash TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id), expires_at TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE TABLE games (
 id TEXT PRIMARY KEY, owner_id INTEGER NOT NULL REFERENCES users(id),
 visibility TEXT NOT NULL DEFAULT 'private' CHECK(visibility IN ('public','private')),
 metadata TEXT NOT NULL, package_key TEXT NOT NULL, size INTEGER NOT NULL, sha256 TEXT NOT NULL,
 format TEXT NOT NULL DEFAULT 'pck', assets TEXT NOT NULL DEFAULT '{}', source TEXT NOT NULL DEFAULT '{}',
 deleted INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
 received_size INTEGER NOT NULL DEFAULT 0, received_sha256 TEXT NOT NULL DEFAULT ''
);
CREATE INDEX games_owner ON games(owner_id);
CREATE TABLE play_sessions (
 hash TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id), game_id TEXT NOT NULL REFERENCES games(id),
 source_token_id TEXT NOT NULL REFERENCES tokens(id), expires_at TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE TABLE player_scores (
 game_id TEXT NOT NULL REFERENCES games(id), user_id INTEGER NOT NULL REFERENCES users(id),
 board TEXT NOT NULL, mode TEXT NOT NULL DEFAULT 'default', value INTEGER NOT NULL, achieved_at TEXT NOT NULL,
 PRIMARY KEY(game_id,user_id,board,mode)
);
CREATE TABLE score_submissions (
 game_id TEXT NOT NULL, user_id INTEGER NOT NULL, submission_id TEXT NOT NULL,
 board TEXT NOT NULL, mode TEXT NOT NULL, value INTEGER NOT NULL,
 best_value INTEGER NOT NULL, achieved_at TEXT NOT NULL, created_at TEXT NOT NULL,
 PRIMARY KEY(game_id,user_id,submission_id)
);
CREATE TABLE device_sessions (
 hash TEXT PRIMARY KEY, code TEXT NOT NULL UNIQUE, game_id TEXT NOT NULL REFERENCES games(id),
 expires_at TEXT NOT NULL, user_id INTEGER, source_token_id TEXT,
 authorized_at TEXT, consumed_at TEXT, created_at TEXT NOT NULL
);
CREATE TABLE rate_limits (
 key TEXT PRIMARY KEY, count INTEGER NOT NULL, expires_at INTEGER NOT NULL
);
CREATE TABLE upload_sessions (
 id TEXT PRIMARY KEY, game_id TEXT NOT NULL, owner_id INTEGER NOT NULL REFERENCES users(id),
 token_id TEXT NOT NULL, expected_size INTEGER NOT NULL, metadata TEXT NOT NULL,
 object_key TEXT NOT NULL, multipart_id TEXT NOT NULL, parts TEXT NOT NULL DEFAULT '[]',
 status TEXT NOT NULL DEFAULT 'uploading', created_at TEXT NOT NULL, expires_at TEXT NOT NULL
);
CREATE UNIQUE INDEX active_upload_game ON upload_sessions(game_id) WHERE status IN ('uploading','validating');

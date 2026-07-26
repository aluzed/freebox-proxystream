-- Credentials live here rather than in .env so the repo and the deployment
-- config stay free of secrets. Single user in practice, but a table costs
-- nothing and makes a password change an ordinary UPDATE.
CREATE TABLE IF NOT EXISTS users (
  id            INTEGER PRIMARY KEY,
  username      TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  created_at    DATETIME NOT NULL,
  updated_at    DATETIME NOT NULL
);

-- Only the SHA-256 of the cookie value is stored, so a database leak does not
-- hand over live sessions.
CREATE TABLE IF NOT EXISTS sessions (
  token_hash TEXT PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at DATETIME NOT NULL,
  expires_at DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_expiry ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS libraries (
  id          INTEGER PRIMARY KEY,
  ftp_path    TEXT NOT NULL UNIQUE,
  name        TEXT NOT NULL,
  kind        TEXT NOT NULL,
  created_at  DATETIME NOT NULL,
  last_scan   DATETIME
);

CREATE TABLE IF NOT EXISTS media (
  id            INTEGER PRIMARY KEY,
  library_id    INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
  ftp_path      TEXT NOT NULL UNIQUE,
  size_bytes    INTEGER NOT NULL,
  ftp_mtime     DATETIME,

  probed_at     DATETIME,
  duration_sec  REAL,
  video_codec   TEXT,
  width         INTEGER,
  height        INTEGER,
  fps           REAL,
  audio_streams TEXT,
  sub_streams   TEXT,

  title         TEXT,
  year          INTEGER,
  season        INTEGER,
  episode       INTEGER,
  overview      TEXT,

  status        TEXT NOT NULL,
  error_msg     TEXT,
  created_at    DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_media_library ON media(library_id);
CREATE INDEX IF NOT EXISTS idx_media_status ON media(status);

CREATE TABLE IF NOT EXISTS playback_progress (
  media_id      INTEGER PRIMARY KEY REFERENCES media(id) ON DELETE CASCADE,
  position_sec  REAL NOT NULL,
  updated_at    DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS segment_cache (
  media_id      INTEGER NOT NULL REFERENCES media(id) ON DELETE CASCADE,
  variant_key   TEXT NOT NULL,
  seq           INTEGER NOT NULL,
  size_bytes    INTEGER NOT NULL,
  created_at    DATETIME NOT NULL,
  last_used_at  DATETIME NOT NULL,
  PRIMARY KEY (media_id, variant_key, seq)
);

CREATE INDEX IF NOT EXISTS idx_segment_lru ON segment_cache(last_used_at);


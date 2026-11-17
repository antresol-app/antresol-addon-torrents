-- Torrent sources: a tracker definition (Cardigann id) plus the user's settings.
CREATE TABLE sources (
  id            INTEGER PRIMARY KEY,
  definition_id TEXT NOT NULL,
  name          TEXT NOT NULL,
  enabled       INTEGER NOT NULL DEFAULT 1,
  settings_json TEXT NOT NULL DEFAULT '{}',   -- includes secrets (password, cookie)
  status        TEXT NOT NULL DEFAULT 'unknown', -- ok | error | unknown
  last_error    TEXT NOT NULL DEFAULT '',
  last_checked  TEXT NULL,
  priority      INTEGER NOT NULL DEFAULT 25,     -- lower = preferred (as in Prowlarr)
  created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

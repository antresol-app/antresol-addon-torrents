-- What a torrent added through «Найти и добавить» is (for naming it in the
-- library) and whether it has been put into the library yet.
CREATE TABLE acquisitions (
  infohash          TEXT PRIMARY KEY,
  kind              TEXT NOT NULL,              -- movie | series
  tmdb_id           INTEGER NOT NULL,
  title             TEXT NOT NULL,
  year              INTEGER NOT NULL DEFAULT 0,
  season            INTEGER NOT NULL DEFAULT 0,
  episode           INTEGER NOT NULL DEFAULT 0,
  added_by          INTEGER NULL,
  created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
  organized_at      TEXT NULL,
  organize_error    TEXT NOT NULL DEFAULT '',
  organize_attempts INTEGER NOT NULL DEFAULT 0,
  paths_json        TEXT NOT NULL DEFAULT '[]'  -- library files created from it
);

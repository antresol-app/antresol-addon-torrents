CREATE TABLE torrents (
  infohash     TEXT PRIMARY KEY,
  name         TEXT NOT NULL DEFAULT '',
  mode         TEXT NOT NULL,                 -- stream | download | seed
  save_dir     TEXT NOT NULL,                 -- where the data lives now
  target_dir   TEXT NULL,                     -- pending move destination (stream -> download)
  item_id      INTEGER NULL,
  added_by     INTEGER NULL,
  added_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
  completed_at TEXT NULL,
  uploaded     INTEGER NOT NULL DEFAULT 0,
  downloaded   INTEGER NOT NULL DEFAULT 0,
  status       TEXT NOT NULL DEFAULT 'queued', -- queued | downloading | seeding | paused | waiting_space | error
  error        TEXT NULL,
  source_url   TEXT NULL,
  magnet       TEXT NULL,                     -- magnet URI until the metafile is known
  release_json TEXT NULL
);

-- Piece completion for anacrolix file storage (pure-Go replacement for its cgo sqlite store).
CREATE TABLE torrent_pieces (
  infohash TEXT NOT NULL,
  piece    INTEGER NOT NULL,
  complete INTEGER NOT NULL,
  PRIMARY KEY (infohash, piece)
) WITHOUT ROWID;

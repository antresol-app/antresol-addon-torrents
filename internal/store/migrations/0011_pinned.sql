-- pinned_releases: the release a household follows for a tracked title (one
-- per title, whoever pinned it last). source_id is the source (sources.id as
-- text) whose link is refreshed; '' for a magnet or an own .torrent file,
-- which are never fetched again. files_json is the release's file list
-- (torrent.FileEntry), suggestion_json a newer release found elsewhere and
-- offered, not applied ('' = none). last_error: why the last refresh kept
-- the pin as it was ('' = it went fine).
CREATE TABLE pinned_releases (
  id              INTEGER PRIMARY KEY,
  title_id        INTEGER NOT NULL UNIQUE,
  source_id       TEXT NOT NULL DEFAULT '',
  link            TEXT NOT NULL DEFAULT '',
  details_url     TEXT NOT NULL DEFAULT '',
  release_title   TEXT NOT NULL DEFAULT '',
  infohash        TEXT NOT NULL,
  quality         TEXT NOT NULL DEFAULT '',
  files_json      TEXT NOT NULL DEFAULT '[]',
  pinned_by       INTEGER,
  pinned_at       TEXT NOT NULL,
  last_checked    TEXT,
  last_error      TEXT NOT NULL DEFAULT '',
  suggestion_json TEXT NOT NULL DEFAULT ''
);

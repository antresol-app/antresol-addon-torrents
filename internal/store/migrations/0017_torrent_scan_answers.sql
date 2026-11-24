-- torrent_scan_answers: when a torrent source last answered the tracking
-- scan of a title (answered_at, RFC 3339 UTC; source_id is the source the
-- user set up, also when its backup way in answered). The core replaces a
-- title's torrent rows with whatever a scan returns, so a source that
-- failed in a scan has its earlier rows handed back by the addon (they
-- carry no time of their own: the core stamps every row it is given); this
-- is what says how long that goes on.
CREATE TABLE torrent_scan_answers (
  title_id    INTEGER NOT NULL,
  source_id   INTEGER NOT NULL,
  answered_at TEXT NOT NULL,
  PRIMARY KEY (title_id, source_id)
);

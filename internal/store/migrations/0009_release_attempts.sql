-- release_attempts: the releases (infohashes) tried for a title (a movie,
-- a season, an album, a book), so the automatic switch away from a release
-- nobody seeds never takes one of them again, across restarts. title_key is
-- acquire.attemptKey; auto: started by the automatic switch (at most 3 per
-- title).
CREATE TABLE release_attempts (
  title_key TEXT NOT NULL,
  infohash  TEXT NOT NULL,
  auto      INTEGER NOT NULL DEFAULT 0,
  at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
  PRIMARY KEY (title_key, infohash)
);

-- acquisitions.had_peers: a peer was connected at some point (such a
-- download is never switched away from). auto_note: 'switched' — this
-- download was started by the automatic switch (shown until it completes
-- or is dismissed); 'none_alive' — it got no peer and no live alternative
-- was found; 'quiet' — that note was dismissed, or the download got a
-- peer while it was being switched (the switch does not try it again);
-- 'replaced' — a switch started another release in its place and is
-- removing it; '' — nothing.
ALTER TABLE acquisitions ADD COLUMN had_peers INTEGER NOT NULL DEFAULT 0;
ALTER TABLE acquisitions ADD COLUMN auto_note TEXT NOT NULL DEFAULT '';

-- release_switch_failures: switches of a title that failed (search error,
-- the start refused, out of time): each puts the next try off longer, and
-- they count toward the 3 switches of a title (no endless retries).
CREATE TABLE release_switch_failures (
  title_key TEXT PRIMARY KEY,
  failures  INTEGER NOT NULL DEFAULT 0,
  next_at   TEXT NOT NULL DEFAULT ''
);

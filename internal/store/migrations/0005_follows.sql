-- «Следить» (series: new episodes are downloaded as they air) and «Хочу
-- посмотреть» (movies: downloaded once released). One follow per title;
-- source tells follows imported from Sonarr/Radarr (plan 3, inactive until
-- «Перейти на Vault»). state_json: {"missing": n, "tried": {"S01E08":
-- "<RFC3339>"}} — tried = no release found then (retried after 24 h).
CREATE TABLE follows (
  id           INTEGER PRIMARY KEY,
  kind         TEXT NOT NULL,                 -- series | movie
  tmdb_id      INTEGER NOT NULL,
  title        TEXT NOT NULL,
  poster       TEXT NOT NULL DEFAULT '',
  user_id      INTEGER,
  active       INTEGER NOT NULL DEFAULT 1,
  source       TEXT NOT NULL DEFAULT 'vault',  -- vault | sonarr | radarr
  created_at   TEXT NOT NULL,
  last_checked TEXT,
  last_error   TEXT NOT NULL DEFAULT '',
  state_json   TEXT NOT NULL DEFAULT '{}',
  UNIQUE(kind, tmdb_id)
);

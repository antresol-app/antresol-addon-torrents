-- schema of origin/main ea78c5bdaad27148b4134923f04e0f5f94830ea7
CREATE TABLE account_links (
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider     TEXT NOT NULL,
  external_id  INTEGER NOT NULL DEFAULT 0,
  nickname     TEXT NOT NULL DEFAULT '',
  access_enc   BLOB,
  refresh_enc  BLOB,
  expires_at   INTEGER NOT NULL DEFAULT 0,
  scope        TEXT NOT NULL DEFAULT '',
  write_state  TEXT NOT NULL DEFAULT 'unasked' CHECK (write_state IN ('unasked','asked','enabled','declined')),
  status       TEXT NOT NULL DEFAULT 'ok',
  last_sync_at INTEGER NOT NULL DEFAULT 0,
  last_error   TEXT NOT NULL DEFAULT '',
  linked_at    INTEGER NOT NULL DEFAULT 0,
  -- 1 once a pull has imported the remote list (the first-link rule ran)
  first_pull_done INTEGER NOT NULL DEFAULT 0,
  -- remote entries the last pull could not map to a TMDB title
  unmapped     INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, provider)
);
CREATE TABLE account_outbox (
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider   TEXT NOT NULL,
  title_id   INTEGER NOT NULL REFERENCES tracked_titles(id) ON DELETE CASCADE,
  op         TEXT NOT NULL CHECK (op IN ('upsert','delete')),
  created_at INTEGER NOT NULL,
  attempts   INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  -- after a failed attempt: not retried before this (unix seconds)
  next_at    INTEGER NOT NULL DEFAULT 0,
  -- bumped by every enqueue: a push removes the row only if no newer
  -- intent arrived while it ran
  seq        INTEGER NOT NULL DEFAULT 0,
  UNIQUE (user_id, provider, title_id)
);
CREATE TABLE account_rates (
  user_id           INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider          TEXT NOT NULL,
  target_id         INTEGER NOT NULL,
  title_id          INTEGER NOT NULL REFERENCES tracked_titles(id) ON DELETE CASCADE,
  tmdb_season       INTEGER NOT NULL DEFAULT 0,
  tmdb_offset       INTEGER NOT NULL DEFAULT 0,
  rate_id           INTEGER NOT NULL DEFAULT 0,
  -- the remote title's episode count (0 = unknown, e.g. still airing)
  anime_episodes    INTEGER NOT NULL DEFAULT 0,
  -- the remote title's airing status (anons | ongoing | released; '' = unknown)
  anime_status      TEXT NOT NULL DEFAULT '',
  remote_status     TEXT NOT NULL DEFAULT '',
  -- the entry's episode count as of the last pull or our last write of it
  remote_episodes   INTEGER NOT NULL DEFAULT 0,
  -- Antresol's watched count in the cour's window when we last wrote an
  -- episode count to it (a lowered count goes out only while the remote
  -- still holds ours)
  local_episodes    INTEGER NOT NULL DEFAULT 0,
  remote_updated_at INTEGER NOT NULL DEFAULT 0,
  pushed_at         INTEGER NOT NULL DEFAULT 0,
  -- the title's mark time (our clock, or the remote change it was pulled
  -- from) as of its last pull win or push: set on every cour of the title
  -- alike; a mark whose time differs from it changed here since
  mark_at           INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, provider, target_id)
);
CREATE TABLE account_settings (
  provider   TEXT PRIMARY KEY,
  client_id  TEXT NOT NULL DEFAULT '',
  secret_enc BLOB,
  updated_at INTEGER NOT NULL DEFAULT 0
);
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
, ext_id TEXT NOT NULL DEFAULT '', meta_json TEXT NOT NULL DEFAULT '{}', poster TEXT NOT NULL DEFAULT '', had_peers INTEGER NOT NULL DEFAULT 0, auto_note TEXT NOT NULL DEFAULT '');
CREATE TABLE addon_file_refs (
  file_id  INTEGER PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
  addon_id TEXT NOT NULL,
  ref      TEXT NOT NULL
);
CREATE TABLE addon_holds (addon_id TEXT PRIMARY KEY, holds_json TEXT NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE addons (
  id           TEXT PRIMARY KEY,
  source       TEXT NOT NULL,
  version      TEXT NOT NULL,
  prev_version TEXT,
  manifest     TEXT NOT NULL,
  enabled      INTEGER NOT NULL DEFAULT 1,
  token_hash   TEXT NOT NULL,
  installed_at INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);
CREATE TABLE anime_map (
  mal_id      INTEGER PRIMARY KEY,
  kind        TEXT NOT NULL DEFAULT '',
  tmdb_id     INTEGER NOT NULL DEFAULT 0,
  tmdb_season INTEGER NOT NULL DEFAULT 0,
  tmdb_offset INTEGER NOT NULL DEFAULT 0,
  anidb_id    INTEGER NOT NULL DEFAULT 0,
  -- 'dataset' rows come from the mapping list; 'search' rows are cached TMDB
  -- search hits (cached_at, unix seconds, lets them expire).
  source      TEXT NOT NULL DEFAULT 'dataset',
  cached_at   INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE annotations (
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  item_id    INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  id         TEXT NOT NULL,
  kind       TEXT NOT NULL,
  locator    TEXT NOT NULL,
  fraction   REAL NOT NULL DEFAULT 0,
  label      TEXT NOT NULL DEFAULT '',
  excerpt    TEXT NOT NULL DEFAULT '',
  note       TEXT NOT NULL DEFAULT '',
  color      TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  client_at  TEXT NOT NULL DEFAULT '',
  rev        INTEGER NOT NULL,
  deleted    INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, item_id, id)
);
CREATE TABLE app_tokens(token TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, proto TEXT NOT NULL, device_id TEXT NOT NULL DEFAULT '', device TEXT NOT NULL DEFAULT '', client TEXT NOT NULL DEFAULT '', version TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, last_seen TEXT NOT NULL, set_id INTEGER NULL REFERENCES iptv_sets(id) ON DELETE SET NULL);
CREATE TABLE channel_cursor (
  channel_id INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  unit_id    INTEGER NOT NULL,
  item_id    INTEGER NOT NULL,
  PRIMARY KEY (channel_id, unit_id)
);
CREATE TABLE channel_extras (
  id              INTEGER PRIMARY KEY,
  channel_id      INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  key             TEXT NOT NULL,
  kind            TEXT NOT NULL DEFAULT 'movie',
  tmdb_id         INTEGER NOT NULL DEFAULT 0,
  title           TEXT NOT NULL,
  year            INTEGER NOT NULL DEFAULT 0,
  overview        TEXT NOT NULL DEFAULT '',
  rating          TEXT NOT NULL DEFAULT '',
  genres_json     TEXT NOT NULL DEFAULT '[]',
  poster          TEXT NOT NULL DEFAULT '',
  backdrop        TEXT NOT NULL DEFAULT '',
  runtime_ms      INTEGER NOT NULL,
  season          INTEGER NOT NULL DEFAULT 0,
  episode         INTEGER NOT NULL DEFAULT 0,
  current         INTEGER NOT NULL DEFAULT 1,
  failed_until_ms INTEGER NOT NULL DEFAULT 0,
  fetched_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
  UNIQUE (channel_id, key)
);
CREATE TABLE channel_favorites (
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  channel_id INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  added_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
  PRIMARY KEY (user_id, channel_id)
);
CREATE TABLE channel_last (
  user_id    INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  channel_id INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE TABLE channel_playout (
  channel_id INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  seq        INTEGER NOT NULL,
  start_ms   INTEGER NOT NULL,
  end_ms     INTEGER NOT NULL,
  item_id    INTEGER NOT NULL,
  file_id    INTEGER NOT NULL,
  unit_id    INTEGER NOT NULL, extra_id INTEGER NULL,
  PRIMARY KEY (channel_id, seq)
);
CREATE TABLE channels (
  id         INTEGER PRIMARY KEY,
  number     INTEGER NOT NULL UNIQUE,
  name       TEXT NOT NULL,
  category   TEXT NOT NULL DEFAULT '',
  user_id    INTEGER NULL REFERENCES users(id) ON DELETE CASCADE,
  rules_json TEXT NOT NULL DEFAULT '{}',
  auto_key   TEXT NULL UNIQUE,
  enabled    INTEGER NOT NULL DEFAULT 1,
  edited     INTEGER NOT NULL DEFAULT 0,
  seed       INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
  updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
, extras_at_ms INTEGER NOT NULL DEFAULT 0, logo_version INTEGER NOT NULL DEFAULT 0, kind TEXT NOT NULL DEFAULT 'antresol', number_manual INTEGER NOT NULL DEFAULT 0, why TEXT NOT NULL DEFAULT '');
CREATE TABLE chapters (
  item_id  INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  idx      INTEGER NOT NULL,
  title    TEXT NOT NULL,
  file_id  INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  start_ms INTEGER NOT NULL DEFAULT 0, -- within the file
  end_ms   INTEGER, -- NULL = the end of the file
  PRIMARY KEY (item_id, idx)
);
CREATE TABLE cleaned_audio (
  file_id      INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  vo_index     INTEGER NOT NULL,
  orig_index   INTEGER NOT NULL,
  studio       TEXT NOT NULL DEFAULT '',
  lang         TEXT NOT NULL DEFAULT '',
  title        TEXT NOT NULL DEFAULT '',
  state        TEXT NOT NULL DEFAULT 'queued',
  method       TEXT NOT NULL DEFAULT 'subtraction',
  path         TEXT NOT NULL DEFAULT '',
  measure_json TEXT NOT NULL DEFAULT '',
  file_size    INTEGER NOT NULL DEFAULT 0,
  file_mtime   INTEGER NOT NULL DEFAULT 0,
  start_ms     REAL NOT NULL DEFAULT 0,
  fails        INTEGER NOT NULL DEFAULT 0,
  retry_at     TEXT NULL,
  updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')), vo_path TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (file_id, vo_index)
);
CREATE TABLE device_pairings (
  id           INTEGER PRIMARY KEY,
  code_hash    TEXT NOT NULL UNIQUE,
  secret_hash  TEXT NOT NULL UNIQUE,
  device_id    TEXT NOT NULL,
  device_name  TEXT NOT NULL DEFAULT '',
  device_model TEXT NOT NULL DEFAULT '',
  app_version  TEXT NOT NULL DEFAULT '',
  client       TEXT NOT NULL DEFAULT '',
  remote       INTEGER NOT NULL DEFAULT 0,
  status       TEXT NOT NULL DEFAULT 'pending',
  user_id      INTEGER REFERENCES users(id) ON DELETE CASCADE,
  approved_by  INTEGER REFERENCES users(id) ON DELETE SET NULL,
  created_at   TEXT NOT NULL,
  expires_at   TEXT NOT NULL,
  decided_at   TEXT
);
CREATE TABLE episode_sources (
  title_id    INTEGER NOT NULL REFERENCES tracked_titles(id) ON DELETE CASCADE,
  season      INTEGER NOT NULL,
  episode     INTEGER NOT NULL,
  kind        TEXT NOT NULL,
  ref         TEXT NOT NULL,
  provider    TEXT NOT NULL DEFAULT '',
  quality     TEXT NOT NULL DEFAULT '',
  labels_json TEXT NOT NULL DEFAULT '[]',
  first_seen  TEXT NOT NULL,
  last_seen   TEXT NOT NULL, meta_json TEXT NOT NULL DEFAULT '{}',
  PRIMARY KEY (title_id, season, episode, kind, ref)
);
CREATE TABLE external_audio_fit (
  path         TEXT PRIMARY KEY,
  file_id      INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  state        TEXT NOT NULL DEFAULT 'pending',
  orig_lang    TEXT NOT NULL DEFAULT '',
  offset_ms    REAL NOT NULL DEFAULT 0,
  tempo        REAL NOT NULL DEFAULT 1,
  measure_json TEXT NOT NULL DEFAULT '',
  track_size   INTEGER NOT NULL DEFAULT 0,
  track_mtime  INTEGER NOT NULL DEFAULT 0,
  file_size    INTEGER NOT NULL DEFAULT 0,
  file_mtime   INTEGER NOT NULL DEFAULT 0,
  fails        INTEGER NOT NULL DEFAULT 0,
  retry_at     TEXT NULL,
  updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE TABLE file_segments (
  file_id  INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  kind     TEXT NOT NULL,    -- recap | intro | credits
  source   TEXT NOT NULL,    -- chapters | theintrodb | introdb | analysis
  start_ms INTEGER NOT NULL,
  end_ms   INTEGER NOT NULL,
  PRIMARY KEY (file_id, kind, source)
);
CREATE TABLE files (
  id               INTEGER PRIMARY KEY,
  item_id          INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  path             TEXT NOT NULL UNIQUE,
  size             INTEGER NOT NULL DEFAULT 0,
  mtime            INTEGER NOT NULL DEFAULT 0,
  duration_ms      INTEGER NULL,
  probe_json       TEXT NULL,
  torrent_infohash TEXT NULL
, missing_since TEXT NULL, probe_rev INTEGER NOT NULL DEFAULT 0);
CREATE TABLE follows (
  id           INTEGER PRIMARY KEY,
  kind         TEXT NOT NULL,                 -- series | movie
  tmdb_id      INTEGER NOT NULL,
  title        TEXT NOT NULL,
  poster       TEXT NOT NULL DEFAULT '',
  user_id      INTEGER REFERENCES users(id) ON DELETE SET NULL,
  active       INTEGER NOT NULL DEFAULT 1,
  source       TEXT NOT NULL DEFAULT 'vault',  -- vault | sonarr | radarr
  created_at   TEXT NOT NULL,
  last_checked TEXT,
  last_error   TEXT NOT NULL DEFAULT '',
  state_json   TEXT NOT NULL DEFAULT '{}', resolution TEXT NOT NULL DEFAULT '',
  UNIQUE(kind, tmdb_id)
);
CREATE TABLE import_albums (
  job_id   INTEGER NOT NULL REFERENCES import_jobs(id) ON DELETE CASCADE,
  key      TEXT NOT NULL,
  artist   TEXT NOT NULL DEFAULT '',
  title    TEXT NOT NULL DEFAULT '',
  year     INTEGER NOT NULL DEFAULT 0,
  added_at TEXT NULL,
  item_id  INTEGER NULL REFERENCES items(id) ON DELETE SET NULL, starred INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(job_id, key)
);
CREATE TABLE import_groups (
  id            INTEGER PRIMARY KEY,
  job_id        INTEGER NOT NULL REFERENCES import_jobs(id) ON DELETE CASCADE,
  release_group TEXT NOT NULL,
  artist        TEXT NOT NULL,
  album         TEXT NOT NULL,
  year          INTEGER NOT NULL DEFAULT 0,
  aliases_json  TEXT NOT NULL DEFAULT '[]',
  album_item_id INTEGER NULL REFERENCES items(id) ON DELETE SET NULL,
  state         TEXT NOT NULL DEFAULT 'waiting',
  infohash      TEXT NOT NULL DEFAULT '',
  bytes         INTEGER NOT NULL DEFAULT 0,
  attempts      INTEGER NOT NULL DEFAULT 0,
  error         TEXT NOT NULL DEFAULT '',
  updated_at    TEXT NOT NULL, wait_title TEXT NOT NULL DEFAULT '', wait_done INTEGER NOT NULL DEFAULT 0, wait_at TEXT NULL, skip_infohash TEXT NOT NULL DEFAULT '', any_format INTEGER NOT NULL DEFAULT 0, dead_json TEXT NOT NULL DEFAULT '[]', slow_json TEXT NOT NULL DEFAULT '[]', run_s INTEGER NOT NULL DEFAULT 0, quiet_s INTEGER NOT NULL DEFAULT 0, run_base INTEGER NULL,
  UNIQUE(job_id, release_group)
);
CREATE TABLE import_jobs (
  id          INTEGER PRIMARY KEY,
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  service     TEXT NOT NULL,
  account     TEXT NOT NULL DEFAULT '',
  status      TEXT NOT NULL,
  favorites   INTEGER NOT NULL DEFAULT 1, -- liked songs become «Избранное»
  error       TEXT NOT NULL DEFAULT '',
  report_json TEXT NOT NULL DEFAULT '{}',
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL,
  started_at  TEXT NULL,
  finished_at TEXT NULL
);
CREATE TABLE import_playlist_entries (
  import_playlist_id INTEGER NOT NULL REFERENCES import_playlists(id) ON DELETE CASCADE,
  position           INTEGER NOT NULL,
  track_id           INTEGER NOT NULL REFERENCES import_tracks(id) ON DELETE CASCADE,
  PRIMARY KEY(import_playlist_id, position)
);
CREATE TABLE import_playlists (
  id          INTEGER PRIMARY KEY,
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  service     TEXT NOT NULL,
  external_id TEXT NOT NULL,
  name        TEXT NOT NULL,
  readable    INTEGER NOT NULL DEFAULT 1,
  playlist_id INTEGER NULL REFERENCES playlists(id) ON DELETE SET NULL,
  job_id      INTEGER NULL REFERENCES import_jobs(id) ON DELETE SET NULL,
  selected    INTEGER NOT NULL DEFAULT 1, built_job INTEGER NULL, built_hash TEXT NOT NULL DEFAULT '',
  UNIQUE(user_id, service, external_id)
);
CREATE TABLE import_tracks (
  id               INTEGER PRIMARY KEY,
  job_id           INTEGER NOT NULL REFERENCES import_jobs(id) ON DELETE CASCADE,
  key              TEXT NOT NULL,
  artist           TEXT NOT NULL DEFAULT '',
  title            TEXT NOT NULL DEFAULT '',
  album            TEXT NOT NULL DEFAULT '',
  album_artist     TEXT NOT NULL DEFAULT '',
  isrc             TEXT NOT NULL DEFAULT '',
  duration_ms      INTEGER NOT NULL DEFAULT 0,
  added_at         TEXT NULL,
  liked            INTEGER NOT NULL DEFAULT 0,
  state            TEXT NOT NULL DEFAULT 'pending',
  reason           TEXT NOT NULL DEFAULT '',
  mb_recording     TEXT NOT NULL DEFAULT '',
  mb_release_group TEXT NOT NULL DEFAULT '',
  mb_title         TEXT NOT NULL DEFAULT '',
  mb_artist        TEXT NOT NULL DEFAULT '',
  mb_album         TEXT NOT NULL DEFAULT '',
  mb_year          INTEGER NOT NULL DEFAULT 0,
  group_id         INTEGER NULL REFERENCES import_groups(id) ON DELETE SET NULL,
  item_id          INTEGER NULL REFERENCES items(id) ON DELETE SET NULL, starred INTEGER NOT NULL DEFAULT 0, match_ver INTEGER NOT NULL DEFAULT 0,
  UNIQUE(job_id, key)
);
CREATE TABLE iptv_epg_urls (
  id              INTEGER PRIMARY KEY,
  source_id       INTEGER NOT NULL REFERENCES iptv_sources(id) ON DELETE CASCADE,
  url_enc         BLOB NOT NULL,
  position        INTEGER NOT NULL DEFAULT 0,
  refresh_hours   INTEGER NOT NULL DEFAULT 24,
  refresh_on_open INTEGER NOT NULL DEFAULT 0,
  auto            INTEGER NOT NULL DEFAULT 0,
  last_ok_ms      INTEGER NOT NULL DEFAULT 0,
  last_try_ms     INTEGER NOT NULL DEFAULT 0,
  last_err        TEXT NOT NULL DEFAULT ''
);
CREATE TABLE iptv_members (
  id            INTEGER PRIMARY KEY,
  source_id     INTEGER NOT NULL REFERENCES iptv_sources(id) ON DELETE CASCADE,
  channel_id    INTEGER NULL REFERENCES channels(id) ON DELETE SET NULL,
  ident         TEXT NOT NULL,
  tvg_id        TEXT NOT NULL DEFAULT '',
  tvg_name      TEXT NOT NULL DEFAULT '',
  name          TEXT NOT NULL DEFAULT '',
  grp           TEXT NOT NULL DEFAULT '',
  logo          TEXT NOT NULL DEFAULT '',
  url_enc       BLOB NULL,
  user_agent    TEXT NOT NULL DEFAULT '',
  referrer      TEXT NOT NULL DEFAULT '',
  epg_id        TEXT NOT NULL DEFAULT '',
  epg_url_id    INTEGER NOT NULL DEFAULT 0,
  epg_how       TEXT NOT NULL DEFAULT '',
  epg_manual    INTEGER NOT NULL DEFAULT 0,
  manual_json   TEXT NOT NULL DEFAULT '',
  shown         INTEGER NOT NULL DEFAULT 0,
  why           TEXT NOT NULL DEFAULT '',
  present       INTEGER NOT NULL DEFAULT 1,
  position      INTEGER NOT NULL DEFAULT 0,
  first_seen_ms INTEGER NOT NULL DEFAULT 0,
  UNIQUE(source_id, ident)
);
CREATE TABLE iptv_programmes (
  epg_url_id  INTEGER NOT NULL REFERENCES iptv_epg_urls(id) ON DELETE CASCADE,
  xml_channel TEXT NOT NULL,
  start_ms    INTEGER NOT NULL,
  end_ms      INTEGER NOT NULL,
  title       TEXT NOT NULL DEFAULT '',
  sub_title   TEXT NOT NULL DEFAULT '',
  descr       TEXT NOT NULL DEFAULT '',
  category    TEXT NOT NULL DEFAULT '',
  icon        TEXT NOT NULL DEFAULT ''
);
CREATE TABLE iptv_set_assign (
  user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  device_id TEXT NOT NULL DEFAULT '',
  set_id    INTEGER NOT NULL REFERENCES iptv_sets(id) ON DELETE CASCADE,
  PRIMARY KEY (user_id, device_id)
);
CREATE TABLE iptv_sets (
  id                   INTEGER PRIMARY KEY,
  name                 TEXT NOT NULL,
  sources_json         TEXT NOT NULL DEFAULT '[]',
  hidden_groups_json   TEXT NOT NULL DEFAULT '[]',
  hidden_channels_json TEXT NOT NULL DEFAULT '[]'
);
CREATE TABLE iptv_sources (
  id               INTEGER PRIMARY KEY,
  name             TEXT NOT NULL,
  kind             TEXT NOT NULL DEFAULT 'm3u',
  url_enc          BLOB NULL,
  xtream_server    TEXT NOT NULL DEFAULT '',
  xtream_user      TEXT NOT NULL DEFAULT '',
  xtream_pass_enc  BLOB NULL,
  default_shown    INTEGER NULL,
  hide_urls        INTEGER NOT NULL DEFAULT 0,
  preset_timeshift INTEGER NOT NULL DEFAULT 1,
  preset_adult     INTEGER NOT NULL DEFAULT 1,
  refresh_hours    INTEGER NOT NULL DEFAULT 24,
  refresh_on_open  INTEGER NOT NULL DEFAULT 0,
  position         INTEGER NOT NULL DEFAULT 0,
  profiles_json    TEXT NOT NULL DEFAULT '[]',
  last_ok_ms       INTEGER NOT NULL DEFAULT 0,
  last_try_ms      INTEGER NOT NULL DEFAULT 0,
  last_err         TEXT NOT NULL DEFAULT '',
  pending_drop     INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE iptv_xml_channels (
  epg_url_id INTEGER NOT NULL REFERENCES iptv_epg_urls(id) ON DELETE CASCADE,
  xml_id     TEXT NOT NULL,
  names_json TEXT NOT NULL DEFAULT '[]',
  icon       TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (epg_url_id, xml_id)
);
CREATE TABLE item_owners (
  item_id INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  PRIMARY KEY (item_id, user_id)
);
CREATE TABLE item_titles (
  item_id INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  title   TEXT NOT NULL,
  norm    TEXT NOT NULL,
  PRIMARY KEY (item_id, norm)
);
CREATE TABLE items (
  id             INTEGER PRIMARY KEY,
  kind           TEXT NOT NULL,
  parent_id      INTEGER NULL REFERENCES items(id) ON DELETE CASCADE,
  title          TEXT NOT NULL,
  original_title TEXT NOT NULL DEFAULT '',
  sort_title     TEXT NOT NULL DEFAULT '',
  year           INTEGER NOT NULL DEFAULT 0,
  overview       TEXT NOT NULL DEFAULT '',
  rating         TEXT NOT NULL DEFAULT '',
  tmdb_id        INTEGER NULL,
  season_no      INTEGER NULL,
  episode_no     INTEGER NULL,
  poster         TEXT NULL,
  backdrop       TEXT NULL,
  added_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
  updated_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
  meta_json      TEXT NOT NULL DEFAULT '{}'
, missing_since TEXT NULL, match_key TEXT);
CREATE VIRTUAL TABLE items_fts USING fts5(title, original_title, overview, tokenize='unicode61 remove_diacritics 2');
CREATE TABLE kid_requests (
  id           INTEGER PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  kind         TEXT NOT NULL,              -- movie | series | music | book | audiobook | comic
  title_key    TEXT NOT NULL,
  name_key     TEXT NOT NULL DEFAULT '',  -- "name:<artist> — <title>" of an album or book (an admin's download without the catalog id matches it)
  tmdb_id      INTEGER NOT NULL DEFAULT 0,
  ext_id       TEXT NOT NULL DEFAULT '',
  title        TEXT NOT NULL,
  subtitle     TEXT NOT NULL DEFAULT '',   -- the artist or the author
  year         INTEGER NOT NULL DEFAULT 0,
  poster       TEXT NOT NULL DEFAULT '',
  aliases_json TEXT NOT NULL DEFAULT '[]',
  rating       TEXT NOT NULL DEFAULT '',   -- the age rating when it was asked for (video)
  status       TEXT NOT NULL DEFAULT 'pending',
  reason       TEXT NOT NULL DEFAULT '',   -- why it was declined (optional)
  created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
  decided_at   TEXT NULL,
  decided_by   INTEGER NULL REFERENCES users(id) ON DELETE SET NULL,
  UNIQUE (user_id, kind, title_key)
);
CREATE TABLE kindle_sends (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  item_id INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  address TEXT NOT NULL,
  sent_at INTEGER NOT NULL,
  PRIMARY KEY (user_id, item_id, address)
);
CREATE TABLE myshows_entries (
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  show_id    INTEGER NOT NULL,
  status     TEXT NOT NULL DEFAULT '',
  score      INTEGER NOT NULL DEFAULT 0,
  first_seen INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, show_id)
);
CREATE TABLE myshows_map (
  show_id   INTEGER PRIMARY KEY,
  imdb_id   TEXT NOT NULL DEFAULT '',
  tmdb_id   INTEGER NOT NULL DEFAULT 0,
  kind      TEXT NOT NULL DEFAULT '',
  cached_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE notify_events (
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  topic      TEXT NOT NULL,
  key        TEXT NOT NULL,
  item_id    INTEGER NOT NULL DEFAULT 0,
  group_id   INTEGER NOT NULL DEFAULT 0,
  data_json  TEXT NOT NULL DEFAULT '{}',
  created_at INTEGER NOT NULL,
  sending_at INTEGER NULL,
  sent_at    INTEGER NULL,
  UNIQUE (user_id, key)
);
CREATE TABLE notify_prefs (
  user_id        INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  email          TEXT NOT NULL DEFAULT '',
  verified       TEXT NOT NULL DEFAULT '',
  episodes       INTEGER NOT NULL DEFAULT 0,
  available      INTEGER NOT NULL DEFAULT 0,
  requests       INTEGER NOT NULL DEFAULT 0,
  downloads      INTEGER NOT NULL DEFAULT 0,
  episodes_at    INTEGER NOT NULL DEFAULT 0,
  available_at   INTEGER NOT NULL DEFAULT 0,
  requests_at    INTEGER NOT NULL DEFAULT 0,
  downloads_at   INTEGER NOT NULL DEFAULT 0,
  quiet_hours    INTEGER NOT NULL DEFAULT 1,
  lang           TEXT NOT NULL DEFAULT '',
  last_sent_at   INTEGER NOT NULL DEFAULT 0,
  verify_sent_at INTEGER NOT NULL DEFAULT 0,
  verify_code    TEXT NOT NULL DEFAULT '',
  verify_tries   INTEGER NOT NULL DEFAULT 0,
  bounced        TEXT NOT NULL DEFAULT '',
  fail_count     INTEGER NOT NULL DEFAULT 0,
  next_try_at    INTEGER NOT NULL DEFAULT 0,
  updated_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE TABLE notify_seen (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  key     TEXT NOT NULL,
  at      INTEGER NOT NULL,
  PRIMARY KEY (user_id, key)
);
CREATE TABLE notify_state (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE notify_verify_log (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  addr    TEXT NOT NULL,
  at      INTEGER NOT NULL
);
CREATE TABLE online_progress (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,
  tmdb_id INTEGER NOT NULL,
  season INTEGER NOT NULL DEFAULT 0,
  episode INTEGER NOT NULL DEFAULT 0,
  position_ms INTEGER NOT NULL DEFAULT 0,
  duration_ms INTEGER NOT NULL DEFAULT 0,
  finished INTEGER NOT NULL DEFAULT 0,
  source TEXT NOT NULL DEFAULT '',
  voice TEXT NOT NULL DEFAULT '',
  quality TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL, title TEXT NOT NULL DEFAULT '', rating TEXT NOT NULL DEFAULT '', year INTEGER NOT NULL DEFAULT 0, poster TEXT NOT NULL DEFAULT '', next_season INTEGER NOT NULL DEFAULT 0, next_episode INTEGER NOT NULL DEFAULT 0, last_listed INTEGER NOT NULL DEFAULT 0, in_library INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(user_id, kind, tmdb_id, season, episode)
);
CREATE TABLE pinned_releases (
  id              INTEGER PRIMARY KEY,
  title_id        INTEGER NOT NULL UNIQUE REFERENCES tracked_titles(id) ON DELETE CASCADE,
  source_id       TEXT NOT NULL DEFAULT '',
  link            TEXT NOT NULL DEFAULT '',
  details_url     TEXT NOT NULL DEFAULT '',
  release_title   TEXT NOT NULL DEFAULT '',
  infohash        TEXT NOT NULL,
  quality         TEXT NOT NULL DEFAULT '',
  files_json      TEXT NOT NULL DEFAULT '[]',
  pinned_by       INTEGER REFERENCES users(id) ON DELETE SET NULL,
  pinned_at       TEXT NOT NULL,
  last_checked    TEXT,
  last_error      TEXT NOT NULL DEFAULT '',
  suggestion_json TEXT NOT NULL DEFAULT ''
, stale_json TEXT NOT NULL DEFAULT '');
CREATE TABLE playback_flags (
  user_id        INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  prefer_cleaned INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE playback_prefs (
  user_id        INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  audio_langs    TEXT NOT NULL DEFAULT '',
  sub_mode       TEXT NOT NULL DEFAULT 'off',
  sub_langs      TEXT NOT NULL DEFAULT '',
  sub_size       TEXT NOT NULL DEFAULT 'm',
  sub_background TEXT NOT NULL DEFAULT 'translucent',
  updated_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE TABLE playlist_items (
  playlist_id INTEGER NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
  idx         INTEGER NOT NULL,
  item_id     INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  PRIMARY KEY (playlist_id, idx)
);
CREATE TABLE playlists (
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE plays (
  user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  item_id   INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  played_at TEXT NOT NULL
, client_at TEXT);
CREATE TABLE progress (
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  item_id     INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  position_ms INTEGER NOT NULL DEFAULT 0,
  duration_ms INTEGER NOT NULL DEFAULT 0,
  finished    INTEGER NOT NULL DEFAULT 0,
  updated_at  TEXT NOT NULL, locator TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (user_id, item_id)
);
CREATE TABLE progress_cleared (
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  item_id    INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  cleared_at TEXT NOT NULL,
  PRIMARY KEY (user_id, item_id)
) WITHOUT ROWID;
CREATE TABLE release_attempts (
  title_key TEXT NOT NULL,
  infohash  TEXT NOT NULL,
  auto      INTEGER NOT NULL DEFAULT 0,
  at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
  PRIMARY KEY (title_key, infohash)
);
CREATE TABLE release_switch_failures (
  title_key TEXT PRIMARY KEY,
  failures  INTEGER NOT NULL DEFAULT 0,
  next_at   TEXT NOT NULL DEFAULT ''
);
CREATE TABLE rt_scores (
  imdb_id     TEXT PRIMARY KEY, -- tt0111161
  tomatometer INTEGER NOT NULL, -- percent
  audience    INTEGER NOT NULL, -- percent (the Popcornmeter)
  fetched_at  INTEGER NOT NULL  -- unix seconds
) WITHOUT ROWID;
CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY);
CREATE TABLE screen_time_bonus (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  day     TEXT NOT NULL, -- YYYY-MM-DD of the screen day (it starts at day_start)
  minutes INTEGER NOT NULL,
  PRIMARY KEY (user_id, day)
) WITHOUT ROWID;
CREATE TABLE screen_time_limits (
  user_id      INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  video_min    INTEGER NULL,
  audio_min    INTEGER NULL,
  weekday_from INTEGER NULL,
  weekday_to   INTEGER NULL,
  weekend_from INTEGER NULL,
  weekend_to   INTEGER NULL,
  day_start    INTEGER NOT NULL DEFAULT 240,
  updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE TABLE screen_time_usage (
  user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  day      TEXT NOT NULL,
  category TEXT NOT NULL,
  item_id  INTEGER NOT NULL DEFAULT 0,
  seconds  REAL NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, day, category, item_id)
) WITHOUT ROWID;
CREATE TABLE segment_lookups (
  service    TEXT NOT NULL,  -- theintrodb | introdb
  lookup     TEXT NOT NULL,  -- tmdb:1418/1/2, imdb:tt0898266/1/2, tmdb:603
  fetched_at INTEGER NOT NULL,
  segments   TEXT NOT NULL DEFAULT '[]',
  group_ms   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (service, lookup)
);
CREATE TABLE segment_status (
  file_id    INTEGER PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
  size       INTEGER NOT NULL,
  mtime      INTEGER NOT NULL,
  chapters_v INTEGER NOT NULL DEFAULT 0,
  online_key TEXT NOT NULL DEFAULT '',
  online_at  INTEGER NOT NULL DEFAULT 0,
  analysis_v INTEGER NOT NULL DEFAULT 0
, chapters_fails INTEGER NOT NULL DEFAULT 0, chapters_retry INTEGER NOT NULL DEFAULT 0, online_fails INTEGER NOT NULL DEFAULT 0, online_retry INTEGER NOT NULL DEFAULT 0, analysis_fails INTEGER NOT NULL DEFAULT 0, analysis_retry INTEGER NOT NULL DEFAULT 0);
CREATE TABLE series_tracks (
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  series_id   INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  has_audio   INTEGER NOT NULL DEFAULT 0,
  audio_lang  TEXT NOT NULL DEFAULT '',
  audio_title TEXT NOT NULL DEFAULT '',
  sub_state   TEXT NOT NULL DEFAULT '',
  sub_lang    TEXT NOT NULL DEFAULT '',
  sub_title   TEXT NOT NULL DEFAULT '',
  updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')), audio_kind TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (user_id, series_id)
);
CREATE TABLE sessions (
  token      TEXT PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
  last_seen  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
, kind TEXT NOT NULL DEFAULT 'web', device_id TEXT NOT NULL DEFAULT '', device_name TEXT NOT NULL DEFAULT '', device_model TEXT NOT NULL DEFAULT '', app_version TEXT NOT NULL DEFAULT '');
CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
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
, fallback_for INTEGER NULL);
CREATE TABLE stars (
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  item_id    INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  starred_at TEXT NOT NULL,
  PRIMARY KEY (user_id, item_id)
);
CREATE TABLE subtitle_attempts (
  file_id  INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  lang     TEXT NOT NULL,
  tried_at TEXT NOT NULL,
  result   TEXT NOT NULL,
  PRIMARY KEY (file_id, lang)
);
CREATE TABLE subtitle_offsets (
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  file_id    INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  offset_ms  INTEGER NOT NULL,
  updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
  PRIMARY KEY (user_id, file_id)
);
CREATE TABLE title_choices (
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  kind        TEXT NOT NULL,
  tmdb_id     INTEGER NOT NULL,
  choice_json TEXT NOT NULL,
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY (user_id, kind, tmdb_id)
);
CREATE TABLE title_episodes (
  title_id INTEGER NOT NULL REFERENCES tracked_titles(id) ON DELETE CASCADE,
  season   INTEGER NOT NULL,
  number   INTEGER NOT NULL,
  name     TEXT NOT NULL DEFAULT '',
  air_date TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (title_id, season, number)
);
CREATE TABLE title_marks (
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  title_id   INTEGER NOT NULL REFERENCES tracked_titles(id) ON DELETE CASCADE,
  state      TEXT NOT NULL CHECK (state IN ('planned','watching','completed','on_hold','dropped')),
  score      INTEGER CHECK (score IS NULL OR (score BETWEEN 1 AND 10)),
  origin     TEXT NOT NULL DEFAULT 'antresol',
  updated_at TEXT NOT NULL,
  PRIMARY KEY (user_id, title_id)
);
CREATE TABLE tmdb_related (
  scope      TEXT NOT NULL,    -- movie | tv | collection
  tmdb_id    INTEGER NOT NULL,
  lang       TEXT NOT NULL,    -- TMDB tag: en-US, ru-RU
  data       TEXT NOT NULL,
  fetched_at INTEGER NOT NULL, -- unix seconds
  PRIMARY KEY (scope, tmdb_id, lang)
) WITHOUT ROWID;
CREATE TABLE tmdb_translations (
  kind       TEXT NOT NULL,    -- movie | tv
  tmdb_id    INTEGER NOT NULL,
  lang       TEXT NOT NULL,    -- TMDB tag: en-US, ru-RU
  data       TEXT NOT NULL,
  fetched_at INTEGER NOT NULL, -- unix seconds
  PRIMARY KEY (kind, tmdb_id, lang)
) WITHOUT ROWID;
CREATE TABLE torrent_pieces (
  infohash TEXT NOT NULL,
  piece    INTEGER NOT NULL,
  complete INTEGER NOT NULL,
  PRIMARY KEY (infohash, piece)
) WITHOUT ROWID;
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
, files_json TEXT NULL);
CREATE TABLE track_lyrics (
  item_id     INTEGER PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
  status      TEXT NOT NULL,
  synced      TEXT NOT NULL DEFAULT '',
  plain       TEXT NOT NULL DEFAULT '',
  source      TEXT NOT NULL DEFAULT 'lrclib',
  fetched_at  TEXT NOT NULL,    -- RFC 3339, the last attempt
  next_try_at TEXT NOT NULL,    -- RFC 3339
  attempts    INTEGER NOT NULL DEFAULT 0
, offset_ms INTEGER NOT NULL DEFAULT 0);
CREATE TABLE tracked_scans (
  title_id INTEGER NOT NULL REFERENCES tracked_titles(id) ON DELETE CASCADE,
  kind     TEXT NOT NULL,
  PRIMARY KEY (title_id, kind)
);
CREATE TABLE tracked_titles (
  id           INTEGER PRIMARY KEY,
  kind         TEXT NOT NULL,                 -- series | movie
  tmdb_id      INTEGER NOT NULL,
  shikimori_id INTEGER,
  myshows_id   INTEGER,
  anidb_id     INTEGER,
  imdb_id      TEXT NOT NULL DEFAULT '',
  name         TEXT NOT NULL,
  poster       TEXT NOT NULL DEFAULT '',
  status       TEXT NOT NULL DEFAULT 'unknown',
  cert         TEXT NOT NULL DEFAULT '',
  next_check   TEXT,
  refreshed_at TEXT,
  UNIQUE(kind, tmdb_id)
);
CREATE TABLE tracks (
  item_id  INTEGER PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
  file_id  INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  artist   TEXT NOT NULL DEFAULT '',
  disc_no  INTEGER NOT NULL DEFAULT 1,
  track_no INTEGER NOT NULL DEFAULT 0,
  start_ms INTEGER NOT NULL DEFAULT 0,
  end_ms   INTEGER, -- NULL = to the end of the file
  codec    TEXT NOT NULL DEFAULT '',
  lossless INTEGER NOT NULL DEFAULT 0,
  mbid     TEXT NOT NULL DEFAULT '',
  lyrics   TEXT NOT NULL DEFAULT ''
, rg_track_gain REAL, rg_track_peak REAL, rg_album_gain REAL, rg_album_peak REAL, rg_scanned INTEGER NOT NULL DEFAULT 0, loud_state INTEGER NOT NULL DEFAULT 0, loud_lufs REAL, loud_peak REAL, loud_fails INTEGER NOT NULL DEFAULT 0, loud_retry TEXT, loud_album_gain REAL, loud_album_peak REAL);
CREATE TABLE "trash" (
  id         INTEGER PRIMARY KEY,
  root       TEXT NOT NULL,
  orig_path  TEXT NOT NULL,
  trash_path TEXT NOT NULL,
  trash_dir  TEXT NOT NULL DEFAULT '',
  name       TEXT NOT NULL,
  is_dir     INTEGER NOT NULL DEFAULT 0,
  size       INTEGER NOT NULL DEFAULT 0,
  deleted_by INTEGER NULL,
  deleted_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
  UNIQUE (trash_dir, trash_path)
);
CREATE TABLE ui_prefs (
  user_id    INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  lang       TEXT NOT NULL,
  updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE TABLE users (
  id            INTEGER PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,
  avatar        TEXT NOT NULL,
  password_hash TEXT NULL,
  is_admin      INTEGER NOT NULL DEFAULT 0,
  is_kid        INTEGER NOT NULL DEFAULT 0,
  max_rating    TEXT NULL,
  created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
, app_password TEXT, app_password_created_at TEXT, hide_progress INTEGER NOT NULL DEFAULT 0, library_scope TEXT NOT NULL DEFAULT 'all', kindle_email TEXT NOT NULL DEFAULT '');
CREATE TABLE watch_time (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  item_id INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  day     TEXT NOT NULL,
  ms      INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, item_id, day)
);
CREATE TABLE watchlist (
  user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  item_id  INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  added_at TEXT NOT NULL,
  PRIMARY KEY (user_id, item_id)
);
CREATE INDEX account_outbox_title ON account_outbox(title_id);
CREATE INDEX account_rates_title ON account_rates(title_id);
CREATE INDEX addon_file_refs_ref ON addon_file_refs(addon_id, ref);
CREATE INDEX addons_token_hash ON addons(token_hash);
CREATE INDEX anime_map_tmdb ON anime_map(tmdb_id);
CREATE INDEX annotations_item ON annotations(item_id);
CREATE INDEX annotations_rev ON annotations(user_id, rev);
CREATE INDEX app_tokens_user ON app_tokens(user_id);
CREATE INDEX channel_favorites_channel ON channel_favorites(channel_id);
CREATE INDEX channel_playout_time ON channel_playout(channel_id, end_ms);
CREATE INDEX channels_user ON channels(user_id);
CREATE INDEX device_pairings_client ON device_pairings(client, status);
CREATE INDEX device_pairings_expires ON device_pairings(expires_at);
CREATE INDEX episode_sources_first_seen ON episode_sources(first_seen);
CREATE INDEX external_audio_fit_file ON external_audio_fit(file_id);
CREATE INDEX files_item ON files(item_id);
CREATE INDEX files_item_probe_rev ON files(item_id, missing_since, probe_rev);
CREATE INDEX files_missing ON files(missing_since) WHERE missing_since IS NOT NULL;
CREATE INDEX import_jobs_status ON import_jobs(status);
CREATE INDEX import_jobs_user ON import_jobs(user_id, id);
CREATE INDEX import_playlist_entries_track ON import_playlist_entries(track_id);
CREATE INDEX import_playlists_job ON import_playlists(job_id);
CREATE INDEX import_tracks_group ON import_tracks(group_id);
CREATE INDEX import_tracks_state ON import_tracks(job_id, state);
CREATE INDEX iptv_epg_urls_source ON iptv_epg_urls(source_id);
CREATE INDEX iptv_members_channel ON iptv_members(channel_id);
CREATE INDEX iptv_programmes_channel ON iptv_programmes(xml_channel, end_ms);
CREATE INDEX iptv_programmes_url ON iptv_programmes(epg_url_id);
CREATE INDEX item_owners_user ON item_owners(user_id, item_id);
CREATE INDEX item_titles_norm ON item_titles(norm);
CREATE INDEX items_kind_added ON items(kind, added_at);
CREATE INDEX items_kind_sort ON items(kind, sort_title);
CREATE INDEX items_match ON items(kind, match_key);
CREATE INDEX items_missing ON items(missing_since) WHERE missing_since IS NOT NULL;
CREATE INDEX items_parent ON items(parent_id) WHERE parent_id IS NOT NULL;
CREATE INDEX items_tmdb ON items(kind, tmdb_id);
CREATE INDEX kid_requests_name ON kid_requests(kind, name_key);
CREATE INDEX kid_requests_status ON kid_requests(user_id, status);
CREATE INDEX kid_requests_title ON kid_requests(kind, title_key);
CREATE INDEX notify_events_pending ON notify_events(user_id) WHERE sent_at IS NULL;
CREATE INDEX notify_verify_log_at ON notify_verify_log(at);
CREATE INDEX online_progress_user_updated ON online_progress(user_id, updated_at);
CREATE INDEX playlist_items_item ON playlist_items(item_id);
CREATE INDEX plays_client ON plays(user_id, item_id, client_at);
CREATE INDEX plays_item ON plays(item_id);
CREATE INDEX plays_user_time ON plays(user_id, played_at);
CREATE INDEX progress_cleared_item ON progress_cleared(item_id);
CREATE INDEX progress_item ON progress(item_id);
CREATE INDEX progress_user_updated ON progress(user_id, updated_at);
CREATE INDEX series_tracks_series ON series_tracks(series_id);
CREATE INDEX sessions_device ON sessions(user_id, kind, device_id);
CREATE INDEX sessions_user ON sessions(user_id);
CREATE INDEX stars_item ON stars(item_id);
CREATE INDEX subtitle_offsets_file ON subtitle_offsets(file_id);
CREATE INDEX title_marks_title ON title_marks(title_id);
CREATE INDEX track_lyrics_next ON track_lyrics(next_try_at);
CREATE INDEX tracked_titles_next_check ON tracked_titles(next_check);
CREATE INDEX tracked_titles_shikimori ON tracked_titles(shikimori_id) WHERE shikimori_id IS NOT NULL;
CREATE INDEX tracks_artist_nocase ON tracks(artist COLLATE NOCASE);
CREATE UNIQUE INDEX tracks_file_start ON tracks(file_id, start_ms);
CREATE INDEX tracks_loud_pending ON tracks(loud_state) WHERE loud_state = 0;
CREATE INDEX trash_deleted_at ON trash(deleted_at);
CREATE INDEX watch_time_day ON watch_time(day);
CREATE TRIGGER files_loudness_stale AFTER UPDATE OF size, mtime ON files
  WHEN old.size IS NOT new.size OR old.mtime IS NOT new.mtime
BEGIN
  UPDATE tracks SET loud_state = 0, loud_lufs = NULL, loud_peak = NULL, loud_fails = 0, loud_retry = NULL
    WHERE file_id = new.id;
END;
CREATE TRIGGER files_probe_rev AFTER UPDATE OF probe_json ON files
  WHEN old.probe_json IS NOT new.probe_json
BEGIN
  UPDATE files SET probe_rev = probe_rev + 1 WHERE id = new.id;
END;
CREATE TRIGGER items_fts_ad AFTER DELETE ON items BEGIN
  DELETE FROM items_fts WHERE rowid = old.id;
END;
CREATE TRIGGER items_fts_ai AFTER INSERT ON items BEGIN
  INSERT INTO items_fts(rowid, title, original_title, overview) VALUES (new.id, new.title, new.original_title, new.overview);
END;
CREATE TRIGGER items_fts_au AFTER UPDATE OF title, original_title, overview ON items BEGIN
  DELETE FROM items_fts WHERE rowid = old.id;
  INSERT INTO items_fts(rowid, title, original_title, overview) VALUES (new.id, new.title, new.original_title, new.overview);
END;
-- data
INSERT INTO schema_migrations(version) VALUES (1),(2),(3),(4),(5),(6),(7),(8),(9),(10),(11),(12),(13),(14),(15),(16),(17),(18),(19),(20),(21),(22),(23),(24),(25),(26),(27),(28),(29),(30),(31),(32),(33),(34),(35),(36),(37),(38),(39),(40),(41),(42),(43),(44),(45),(46),(47),(48),(50),(52),(54),(56),(60),(62),(64),(66),(67),(68),(69),(70),(71),(72),(73),(74),(75),(76),(77),(78),(79),(80),(81),(83),(84),(85),(86),(87),(88),(89),(90),(10001),(10002);
INSERT INTO users(id,name,avatar,is_admin) VALUES (1,'alice','a1',1),(2,'bob','a2',0);
INSERT INTO items(id,kind,title,year,tmdb_id) VALUES (1,'movie','Sample Movie',2001,101),(2,'series','Sample Series',2005,202),(3,'track','Sample Track',2010,NULL);
INSERT INTO files(id,item_id,path,size,mtime,torrent_infohash) VALUES
 (1,1,'/media/movies/Sample Movie (2001)/sample.mkv',1000,1700000000,'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'),
 (2,2,'/media/series/Sample Series/S01E01.mkv',2000,1700000001,'BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB'),
 (3,2,'/media/series/Sample Series/S01E02.mkv',2000,1700000002,'BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB'),
 (4,3,'/media/music/Sample Artist/Sample Track.flac',3000,1700000003,'cccccccccccccccccccccccccccccccccccccccc'),
 (5,1,'/media/movies/Sample Movie (2001)/extra.mkv',500,1700000004,NULL);
INSERT INTO item_owners(item_id,user_id) VALUES (1,1),(2,2);
INSERT INTO torrents(infohash,name,mode,save_dir,item_id,added_by,added_at,completed_at,uploaded,downloaded,status,magnet,files_json) VALUES
 ('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','Sample Movie 2001','download','/media/.torrents/aaaa',1,1,'2026-01-01T00:00:00Z','2026-01-01T01:00:00Z',10,1000,'paused',NULL,'[]'),
 ('bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb','Sample Series S01','download','/media/.torrents/bbbb',2,2,'2026-01-02T00:00:00Z',NULL,0,2000,'downloading','magnet:?xt=urn:btih:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb','[]'),
 ('cccccccccccccccccccccccccccccccccccccccc','Sample Stream','stream','/media/.torrents/cccc',NULL,1,'2026-01-03T00:00:00Z',NULL,0,0,'downloading',NULL,NULL);
INSERT INTO torrent_pieces(infohash,piece,complete) VALUES ('bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',0,1),('bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',1,0),('bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',2,1);
INSERT INTO sources(id,definition_id,name,enabled,settings_json,status,priority,created_at) VALUES
 (1,'example-def','Example source',1,'{"url":"https://example.invalid"}','ok',25,'2026-01-01T00:00:00Z'),
 (2,'example-def2','Disabled source',0,'{}','unknown',30,'2026-01-01T00:00:00Z');
INSERT INTO acquisitions(infohash,kind,tmdb_id,title,year,season,episode,added_by,created_at,paths_json,meta_json) VALUES
 ('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','movie',101,'Sample Movie',2001,0,0,1,'2026-01-01T00:00:00Z','["/media/movies/Sample Movie (2001)/sample.mkv"]','{}'),
 ('bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb','series',202,'Sample Series',2005,1,0,2,'2026-01-02T00:00:00Z','[]','{"release":"Sample Series S01 1080p","seasons":[1],"organized":[1]}'),
 ('cccccccccccccccccccccccccccccccccccccccc','music',0,'Sample Album',2010,0,0,1,'2026-01-03T00:00:00Z','[]','{"artist":"Sample Artist","year":2010,"release":"Sample Artist - Sample Album (2010) FLAC","handPicked":true}'),
 ('dddddddddddddddddddddddddddddddddddddddd','series',202,'Sample Series',2005,0,0,1,'2026-01-04T00:00:00Z','[]','{"release":"Sample Series S01 audio","voiceOver":{"itemId":2,"studio":"Studio A","origLang":"en","picks":{"0":{"fileId":2,"path":"/media/series/Sample Series/S01E01.mkv","season":1,"episode":1,"durationMs":2400000},"1":{"fileId":3,"path":"/media/series/Sample Series/S01E02.mkv","season":1,"episode":2,"durationMs":2400000}}}}');
INSERT INTO follows(id,kind,tmdb_id,title,user_id,active,source,created_at,resolution) VALUES (1,'series',202,'Sample Series',2,1,'vault','2026-01-02T00:00:00Z','1080'),(2,'movie',101,'Sample Movie',1,0,'vault','2026-01-01T00:00:00Z','');
INSERT INTO release_attempts(title_key,infohash,auto,at) VALUES ('series:202','bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',1,'2026-01-02T00:00:00Z');
INSERT INTO release_switch_failures(title_key,failures,next_at) VALUES ('series:202',2,'2026-01-05T00:00:00Z');
INSERT INTO tracked_titles(id,kind,tmdb_id,name) VALUES (1,'series',202,'Sample Series');
INSERT INTO pinned_releases(id,title_id,source_id,link,release_title,infohash,quality,files_json,pinned_by,pinned_at) VALUES (1,1,'1','https://example.invalid/r/1','Sample Series S01 1080p','bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb','1080p','[]',2,'2026-01-02T00:00:00Z');
INSERT INTO settings(key,value) VALUES
 ('app_settings','{"uploadLimited":true,"uploadLimitKBs":512,"streamCacheGB":30,"noSeeding":true,"onlineEnabled":true}'),
 ('release_prefs','{"maxHeight":1080,"preferDub":true,"preferOriginal":false,"maxSizeGB":0,"minSeeders":1,"studios":["Studio A"],"voices":["Voice A","Voice B"]}'),
 ('migration','{"state":"done"}'),
 ('import_token_key','AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=');
INSERT INTO online_progress(user_id,kind,tmdb_id,season,episode,position_ms,duration_ms,source,updated_at) VALUES (2,'series',202,1,1,60000,120000,'example','2026-01-06T00:00:00Z');

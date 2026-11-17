-- The addon's own state (not a replay of a core migration):
--   settings      key/value rows, the core's shape (releaseprefs.Store and
--                 the import token key work on it unchanged): upload_limited,
--                 upload_limit_kbs, stream_cache_gb, no_seeding,
--                 release_prefs, migration, the import token key;
--   import_state  the one-time import from vault.db (one row at most;
--                 fingerprint: the source tables it was taken from);
--   import_refs   library refs the import still has to bind (phase 2).
CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE import_state (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    source      TEXT NOT NULL,
    stamp       TEXT NOT NULL,
    counts_json TEXT NOT NULL,
    phase       INTEGER NOT NULL,
    fingerprint TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL
);

CREATE TABLE import_refs (
    path  TEXT PRIMARY KEY,
    ref   TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'bound', 'missing', 'taken'))
);

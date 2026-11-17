-- «На сервер» for music, books, audiobooks and comics: those have no TMDB
-- id (tmdb_id = 0); ext_id is the catalog id (MusicBrainz release group,
-- Open Library work key, "" for comics) and meta_json what organize needs
-- besides the title: {"artist": "…", "author": "…", "year": 1988}.
ALTER TABLE acquisitions ADD COLUMN ext_id TEXT NOT NULL DEFAULT '';
ALTER TABLE acquisitions ADD COLUMN meta_json TEXT NOT NULL DEFAULT '{}';

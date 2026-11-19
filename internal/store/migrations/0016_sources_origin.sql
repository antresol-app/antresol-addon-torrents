-- sources.origin: where a source came from — 'prowlarr' for one Prowlarr's
-- indexer list brought in («Переезд», «Импорт из Prowlarr»), NULL for one
-- added by hand. sources.synced_at (RFC 3339): the last import that listed
-- it. Searches never go through Prowlarr: each source is asked directly.
ALTER TABLE sources ADD COLUMN origin TEXT NULL;
ALTER TABLE sources ADD COLUMN synced_at TEXT NULL;

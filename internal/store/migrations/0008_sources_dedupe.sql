-- Two Prowlarr imports running at once could add the same tracker twice
-- (fixed by serialising imports). Remove exact copies only: the same
-- definition, name and settings (logins, cookies) byte for byte; the
-- oldest source stays. Nothing else refers to a source id.
DELETE FROM sources
WHERE EXISTS (
  SELECT 1 FROM sources AS older
  WHERE older.definition_id = sources.definition_id
    AND older.name = sources.name
    AND older.settings_json = sources.settings_json
    AND older.id < sources.id
);

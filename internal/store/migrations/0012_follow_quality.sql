-- «Докачивать новые»: a follow's resolution ('' = the household's release
-- preferences; else 720p | 1080p | 2160p, preferred over a better scored
-- release of another resolution).
ALTER TABLE follows ADD COLUMN resolution TEXT NOT NULL DEFAULT '';

-- A pinned release replaced in the engine by a re-upload that no longer
-- lists some of the download's files (renamed): those files, deleted once
-- the new download is complete and imported (internal/pinned stale.go;
-- JSON list of {root, paths, since}, '' = none).
ALTER TABLE pinned_releases ADD COLUMN stale_json TEXT NOT NULL DEFAULT '';

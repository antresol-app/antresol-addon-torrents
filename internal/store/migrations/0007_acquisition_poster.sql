-- acquisitions.poster: the cover of what is downloading, a file of the image
-- cache (DataDir/images) captured when the download was started, so
-- «Загрузки» can show it before the library has the item. '' = none yet.
ALTER TABLE acquisitions ADD COLUMN poster TEXT NOT NULL DEFAULT '';

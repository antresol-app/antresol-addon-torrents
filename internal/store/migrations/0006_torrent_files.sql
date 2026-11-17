-- files_json: the files chosen for download, as a JSON array of indexes in
-- the info's file order (e.g. [0,2]). NULL = the whole torrent. Written
-- once when the torrent is added; never changed afterwards.
ALTER TABLE torrents ADD COLUMN files_json TEXT NULL;

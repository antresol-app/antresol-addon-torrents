-- item_owners (0037) of the titles already in the library: the profile
-- whose «На сервер» / «Найти и добавить» / «Импорт медиатеки» download
-- placed a file of the title (files.torrent_infohash → acquisitions
-- .added_by), and the albums of an import's songs, those it found in the
-- library already too (import_groups.album_item_id of the job). A torrent removed before this
-- migration took its acquisition along: its titles stay ownerless.
INSERT OR IGNORE INTO item_owners (item_id, user_id)
SELECT unit, user_id FROM (
  SELECT CASE i.kind
      WHEN 'episode' THEN p.parent_id
      WHEN 'season' THEN i.parent_id
      WHEN 'track' THEN i.parent_id
      WHEN 'comic_volume' THEN i.parent_id
      ELSE i.id END AS unit,
    a.added_by AS user_id
  FROM files f
  JOIN acquisitions a ON a.infohash = f.torrent_infohash
  JOIN items i ON i.id = f.item_id
  LEFT JOIN items p ON p.id = i.parent_id
  WHERE a.added_by IS NOT NULL AND EXISTS (SELECT 1 FROM users u WHERE u.id = a.added_by)
)
WHERE unit IS NOT NULL;

INSERT OR IGNORE INTO item_owners (item_id, user_id)
SELECT g.album_item_id, j.user_id FROM import_groups g JOIN import_jobs j ON j.id = g.job_id
WHERE g.album_item_id IS NOT NULL AND EXISTS (SELECT 1 FROM items i WHERE i.id = g.album_item_id);

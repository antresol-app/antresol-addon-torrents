-- sources.fallback_for: a source that is a second way into another one (the
-- primary): searched only when the primary fails, shown as part of it.
-- RuTracker by cookie (rutracker-org-cookie) backs RuTracker by login, which
-- the site's captcha or blocks used to break; both searching every time
-- asked the tracker twice and listed every topic twice.
ALTER TABLE sources ADD COLUMN fallback_for INTEGER NULL;
UPDATE sources SET fallback_for = (SELECT p.id FROM sources p WHERE p.definition_id = 'rutracker' ORDER BY p.id LIMIT 1)
 WHERE definition_id = 'rutracker-org-cookie'
   AND id = (SELECT MIN(id) FROM sources WHERE definition_id = 'rutracker-org-cookie')
   AND EXISTS (SELECT 1 FROM sources p WHERE p.definition_id = 'rutracker');

-- The addon's own (not a core replay): the profile a row belongs to, by
-- its stable key (the core's users.uid, X-Antresol-User uid). The core
-- may give a deleted profile's id to a new one; a uid is never reused.
-- user_id / pinned_by / added_by stay the id the core was told at the
-- time; every per-profile read goes by the uid (its id now taken from
-- the core's profile list). Rows from before (and from the import) get
-- their uid from the one-time mapping at start (follow.MapUserUIDs).
ALTER TABLE follows ADD COLUMN user_uid TEXT;
ALTER TABLE pinned_releases ADD COLUMN pinned_by_uid TEXT;
ALTER TABLE torrents ADD COLUMN added_by_uid TEXT;
ALTER TABLE acquisitions ADD COLUMN added_by_uid TEXT;

-- The addon's own (not a core replay): a follow whose profile was found
-- deleted (a profile list read without it) keeps that mark. The core
-- reuses a deleted profile's id, so user_id is nulled at the same time:
-- a new profile with the same id inherits nothing.
ALTER TABLE follows ADD COLUMN user_gone INTEGER NOT NULL DEFAULT 0;

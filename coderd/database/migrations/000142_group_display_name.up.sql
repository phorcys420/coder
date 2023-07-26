BEGIN;

ALTER TABLE groups
	ADD COLUMN display_name TEXT NOT NULL DEFAULT '';

COMMENT ON COLUMN groups.display_name IS 'Display name is a custom, human-friendly group name that user can set. This is not required to be unique.';

-- Use the name as the display name for all existing groups
UPDATE groups SET display_name = name;

COMMIT;
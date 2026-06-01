-- Revert: drop the files.eas extended-attributes column.

ALTER TABLE files
    DROP COLUMN IF EXISTS eas;

-- Rollback: Re-add the unique constraint on child_id
-- Note: This will fail if any hard links exist in the database

ALTER TABLE parent_child_map ADD CONSTRAINT unique_child UNIQUE (child_id);

-- Migration to enable hard link support
-- Remove the unique constraint on child_id in parent_child_map
-- This allows the same file to appear in multiple directory entries (hard links)

ALTER TABLE parent_child_map DROP CONSTRAINT unique_child;

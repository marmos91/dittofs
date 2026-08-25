-- A manifest row claims [rowOffset, rowOffset+data_size) of the file, served by
-- the chunk's bytes starting at start_offset. Zero means the claim begins at the
-- chunk's first byte, which is what every existing row means and what every row
-- a carve writes still means, so the default backfills the whole table
-- correctly and no rewrite is needed.
--
-- A non-zero value comes from narrowing a row off its head, which a carve span
-- ending inside a row that also starts inside it needs: the row keeps only what
-- lies past the span, and its id moves to the first byte it still claims.
ALTER TABLE file_blocks ADD COLUMN start_offset INTEGER NOT NULL DEFAULT 0;

-- Same offset on the FileAttr.Blocks projection: a clone and a manifest repair
-- rebuild rows from it, so a ref that dropped the offset would rebuild a row
-- serving the chunk's head at the ref's offset.
ALTER TABLE file_block_refs ADD COLUMN start_offset INTEGER NOT NULL DEFAULT 0;

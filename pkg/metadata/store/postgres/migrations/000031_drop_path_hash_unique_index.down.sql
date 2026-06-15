-- Revert: restore the active-path uniqueness index on files (#1160).
--
-- NOTE: this recreate fails if the table now holds two active rows
-- (nlink > 0) sharing a (share_name, path_hash) — exactly the hard-link
-- state the up migration permits. Roll back only against data that predates
-- such a state.

CREATE UNIQUE INDEX IF NOT EXISTS unique_share_path_hash_active
    ON files(share_name, path_hash) WHERE nlink > 0;

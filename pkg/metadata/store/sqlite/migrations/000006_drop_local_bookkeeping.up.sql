-- Post-journal cleanup: the journal's .idx sidecars are the authoritative local
-- index and rollup positions are journal-managed. local_chunk_index and
-- rollup_offsets are dead (BlockChunkCommit.Local is always zero; the local
-- store is journal-backed). Drop both.
DROP TABLE IF EXISTS local_chunk_index;
DROP TABLE IF EXISTS rollup_offsets;

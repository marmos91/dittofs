-- The create verifier an exclusive CREATE/OPEN carries, so a retransmitted
-- request can be recognised as a replay of the one that already committed
-- rather than answered EEXIST. The column existed in neither SQL dialect, so
-- the value round-tripped on the KV backends and read back as zero here, and
-- every retry compared the client's verifier against 0 and mismatched.
--
-- BIGINT holds the signed bit pattern of the uint64: the token is opaque, only
-- ever compared for equality, and values above 2^63 are ordinary and must
-- survive the trip rather than saturate.
ALTER TABLE inodes ADD COLUMN IF NOT EXISTS idempotency_token BIGINT NOT NULL DEFAULT 0;

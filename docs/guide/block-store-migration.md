# Block Store Migration Guide

DittoFS has changed its on-disk/remote block layout twice:

| Layout | Servers | Local | Remote |
|--------|---------|-------|--------|
| Path-indexed (`.blk`) | ≤ v0.15 | `{payloadID}/block-{idx}.blk` | per-block objects |
| Standalone CAS | v0.16 – v0.21 | per-chunk files `blocks/{hh}/{hh}/{hex}` | per-chunk objects `cas/{hh}/{hh}/{hex}` |
| Packed blocks | current | append-only log blobs (`blobs/`) | packed containers `blocks/<id>` |

Current servers store file content as FastCDC chunks (BLAKE3-hashed,
dedup-safe) packed into ~16 MiB block containers. What you need to do
depends on the layout your data is on.

## Standalone CAS (v0.16 – v0.21) → packed blocks: automatic

Nothing to run. On startup, each share's block store detects leftover
standalone-CAS state — pre-flip per-chunk local files, remote `cas/`
objects, or chunk locators that still point at standalone objects — and
converts it **before the share starts serving**:

1. Local per-chunk files are imported into the append-only log-blob tier
   (BLAKE3-verified, deduplicated) and deleted.
2. Standalone remote chunks are re-packed into `blocks/<id>` containers.
   Each container's chunk locators and block record commit in a single
   metadata transaction, so a crash can never leave a half-pointed block.
3. The now-unreferenced `cas/` namespace is purged.

The migration is **idempotent and resumable**: if the process is killed
mid-way, the next start picks up where it left off and converges. There is
no flag, sentinel, or journal to manage. Expect the first start after an
upgrade to take roughly (total standalone bytes ÷ remote throughput);
chunks that are still cached locally are re-packed without downloading.

If the share's remote is unreachable at startup and standalone chunks
remain, the server refuses to start that share (the data would be
unreadable anyway — the read path no longer understands standalone
objects). Restore connectivity and start again.

## Path-indexed `.blk` (≤ v0.15): migrate with an older release first

The offline `dfs migrate-to-cas` tool shipped through **v0.21** and has
been removed. A current server still refuses to start against a `.blk`
layout (exit code 78) — but the directive now is:

1. Install dittofs v0.21 (or any v0.16–v0.21 release).
2. Stop the server and run `dfs migrate-to-cas` per that release's
   documentation (idempotent, resumable, per-share `.cas-migrated-v1`
   sentinel on success).
3. Upgrade to the current release. The automatic cas→blocks conversion
   above finishes the job on first start.

## Verifying

After the first post-upgrade start:

- The log line `cas→blocks migration complete` reports repacked chunk and
  purged object counts (only printed when there was something to do).
- The remote bucket/prefix should contain no keys under `cas/`.
- Reads verify BLAKE3 end-to-end; any corruption introduced in transit
  fails closed rather than returning wrong bytes.

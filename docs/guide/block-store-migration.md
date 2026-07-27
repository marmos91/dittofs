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

1. Local per-chunk files are imported into the local journal (the append-only
   write-back cache; BLAKE3-verified, deduplicated) and deleted.
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

The offline `migrate-to-cas` command shipped through **v0.21** and has
been removed. A current server still refuses to start against a `.blk`
layout (exit code 78) — but the directive now is:

1. Install dittofs v0.21 (or any v0.16–v0.21 release).
2. Stop the server and run that release's `migrate-to-cas` per its
   documentation (idempotent, resumable, per-share `.cas-migrated-v1`
   sentinel on success).
3. Upgrade to the current release. The automatic cas→blocks conversion
   above finishes the job on first start.

## Upgrading and rolling back

**Every migration above is one-way.** Once a share has been converted, the
release you upgraded from can no longer read it. There is no downgrade
command, and there is no partial-downgrade state to repair.

So: **take a snapshot before you upgrade** — the share's local store
directory and, for remote-backed shares, the bucket/prefix. That snapshot is
the only way back.

The migrations announce themselves. Each one logs a `WARN` naming what it is
about to convert before it touches anything, and long-running ones log
progress every few seconds (payload/chunk counts) so a large store is
visibly working rather than apparently hung. Nothing blocks on an
acknowledgement — an unattended upgrade-and-restart completes on its own.

From this release on, starting an **older** binary against state a newer one
wrote refuses to boot: the server exits **78** (`EX_CONFIG`) and prints the
share path along with both the on-disk format version and the highest this
build reads. Earlier releases had no such check and would open the share and
serve stored files as zeros — right length, no content — which is why the
guard fails closed instead.

If you hit it, no data has been modified. Either:

- reinstall the newer release and start again (the usual case: an accidental
  downgrade or a rollback of the wrong component), or
- restore the pre-upgrade snapshot and start the older release against that.

## Verifying

After the first post-upgrade start:

- The log line `cas→blocks migration complete` reports repacked chunk and
  purged object counts (only printed when there was something to do).
- The remote bucket/prefix should contain no keys under `cas/`.
- Reads verify BLAKE3 end-to-end; any corruption introduced in transit
  fails closed rather than returning wrong bytes.

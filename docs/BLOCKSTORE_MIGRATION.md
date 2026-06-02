# Block Store Migration Guide

Older DittoFS servers stored block content under a path-indexed layout
(`{payloadID}/block-{idx}`). Current servers (v0.16+) use a
content-addressable (CAS) layout: file content is chunked with FastCDC,
each chunk is hashed with BLAKE3, and chunks are stored under
`blocks/{hh}/{hh}/{hex}` locally and `cas/{hh}/{hh}/{hex}` remotely.

A v0.16+ server **refuses to start** against a store still on the older
layout — its boot guard exits with a clear directive to run
`dfs migrate-to-cas` first. This guide is the operator runbook for that
offline migration.

For the underlying design (BlockRef, ObjectID dedup, the CAS-only gate),
see [ARCHITECTURE.md](ARCHITECTURE.md#migration--block-layout-routing).

## Why migrate

- **CAS layout:** immutable, hash-keyed, dedup-safe across files and across
  VMs that share a remote — typically 40–80% cross-VM dedup.
- **Simpler GC:** a single mark-sweep over one hash-keyed namespace.
- **Atomic per-share snapshots** (reference holds; see [SNAPSHOTS.md](SNAPSHOTS.md)).
- **Cost:** one offline maintenance window. Each share carries a
  `block_layout` flag (`legacy` or `cas-only`); the migration flips it to
  `cas-only` in the same step that deletes the last legacy keys, so the
  engine never sees a half-migrated share.

## The tool

`dfs migrate-to-cas` runs against the **stopped** server's on-disk storage
root. It is part of the `dfs` server binary (not `dfsctl`).

```
dfs migrate-to-cas [flags]
```

| Flag | Required | Meaning |
|------|----------|---------|
| `--storage-dir <root>` | yes | Storage root; expects a `shares/<name>/blocks/` subtree per share. |
| `--metadata-dir <path>` | yes | Path to the Badger metadata database directory (the `path` from the metadata store config). |
| `--share <name>` | no | Scope the run to one share. Default: every share discovered under `<storage-dir>/shares/`. |
| `--dry-run` | no | Walk + sample only; report file count, bytes, estimated dedup ratio, and ETA. Writes nothing. |
| `--json` | no | Emit one JSON progress object per second to stdout (machine-parseable). |
| `--max-disk <bytes>` | no | Per-share max-disk budget for the destination store (0 = unlimited). |
| `--max-memory <bytes>` | no | Per-share max-memory budget for the destination store (0 = 256 MiB default). |

The tool is **idempotent and resumable**: a per-share journal at
`<storage-dir>/shares/<name>/.dittofs-migrate-to-cas.state` lets a run
resume after a crash or Ctrl-C without re-uploading already-migrated
chunks. On success it writes `<storage-dir>/shares/<name>/.cas-migrated-v1`
via atomic rename — the boot guard refuses to start the server until that
sentinel exists (it exits with code 78 otherwise).

## Procedure

1. **Stop the server.** The migration rewrites the on-disk layout in place;
   a concurrent server would race the rename and corrupt the store.

   ```bash
   sudo systemctl stop dfs    # or: pkill -INT dfs
   ```

2. **Estimate the work** with a dry run:

   ```bash
   dfs migrate-to-cas \
     --storage-dir /var/lib/dfs/storage \
     --metadata-dir /var/lib/dfs/metadata \
     --share myshare \
     --dry-run
   ```

   Multiply the reported upload bytes by your remote-store throughput to
   size the maintenance window (add ~50% headroom).

3. **Migrate.** Drop `--dry-run`; optionally scope to one share. Omit
   `--share` to migrate every share under the storage root:

   ```bash
   dfs migrate-to-cas \
     --storage-dir /var/lib/dfs/storage \
     --metadata-dir /var/lib/dfs/metadata \
     --share myshare
   ```

   For unattended runs, add `--json` and capture stdout for progress
   monitoring.

4. **Restart the server.** With the `.cas-migrated-v1` sentinel in place,
   the boot guard passes:

   ```bash
   sudo systemctl start dfs
   ```

## Recovery

- **Crash or Ctrl-C mid-migration.** Re-run the exact same command. The
  journal makes the tool skip already-migrated chunks; it does not
  re-upload them.
- **Integrity-check failure.** The tool HEADs each uploaded chunk against
  its expected BLAKE3 hash before cutover. A mismatch aborts the run before
  any legacy key is deleted and before the `block_layout` flip — the share
  stays on the old layout and is safe to retry. Inspect the failure log,
  verify the remote object, then re-run.
- **The server won't start (exit code 78).** The store still lacks the
  `.cas-migrated-v1` sentinel for some share. Run `dfs migrate-to-cas`
  (without `--share`) to migrate any remaining shares, then start the
  server.

## Pre-flight checklist

- [ ] Confirm the server binary: `dfs --version`.
- [ ] Stop the server.
- [ ] Confirm remote credentials are valid (e.g. `aws s3 head-bucket --bucket NAME`).
- [ ] Run `--dry-run` and size the maintenance window.
- [ ] Confirm free space in each share's data dir for the journal.
- [ ] Run the migration; restart the server.

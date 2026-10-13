# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

> **This file does not cover every release.** It lapsed after 0.13.0 and was
> picked up again at 0.34.0, so the versions in between are documented only by
> their GitHub release pages, which are generated from commit history. The
> entries that were written during the gap are filed below under the version
> that actually shipped them. Nothing here has been reconstructed after the
> fact.

## [0.35.0] — 2026-10-12

The main change in 0.35.0 is that **BadgerDB is now the only metadata
store**: the PostgreSQL, SQLite and legacy `memory` metadata store backends have
been removed. Read the migration guide below before upgrading a server that uses
any of them.

### Removed

- **Breaking: the PostgreSQL, SQLite and legacy `memory` metadata stores are
  gone; BadgerDB is the only metadata store.** The `postgres`, `sqlite` and
  `memory` metadata store types no longer exist. A server whose configuration
  still names one refuses to start, with `unsupported metadata store type`, and
  changes nothing. There is no conversion tool: a share on a SQL store moves by
  copying its files to a share on a new instance. A `memory` store is removed
  before the upgrade and recreated as a Badger store kept in RAM, with
  `dfsctl store metadata add --name <store> --in-memory`. See the
  [metadata store migration guide](docs/guide/metadata-store-migration.md) before
  upgrading. The control-plane database (`database:`, SQLite or PostgreSQL) is not
  affected.

### Fixed

- **A write refused because the local store is full now reaches the client as
  "no space", not as an I/O error.** SMB returns `STATUS_DISK_FULL`, NFSv3
  `NFS3ERR_NOSPC` and NFSv4 `NFS4ERR_NOSPC`, where all three used to return
  their generic I/O error.

- **A request that arrives with no deadline gets 30 seconds at the block
  store.** A cold read from a remote that stopped answering, or a write held
  while the local store was at capacity, used to wait out their own 60-second
  budgets, and the backpressure budget restarted whenever the remote drained a
  little.

- **Deleting a file no longer waits for a flush pass to finish uploading.** The
  delete itself was already durable; only the best-effort reclaim at its end
  waited, and with the remote down it waited for upload slots that never freed.
  An SMB delete-on-close was measured held for 68 seconds, past the point where
  clients give up on the connection.

- **A write that lands while a CLONE replaces its destination is no longer read
  back as zeros.** A write outside the cloned range could commit between the
  clone's metadata transaction and its journal discard and be lost, and a
  background carve or warm could republish the old content over the clone.
  CLONE now holds the source and destination exclusively for the whole
  replacement, and ordinary writes, carves and warms take shared ownership.

- **That ownership is no longer held across a clone's upload or a whole-share
  warm.** A CLONE drains its source before taking the exclusive hold, and a warm
  holds one payload at a time, so reads and writes on other files no longer
  queue behind either.

- **Directory removal and rename are ordered against creates inside the
  directory.** A directory could be removed after another operation had
  created or moved a child into it, a create could commit after its parent was
  gone, and a delayed removal could delete a name that a rename had already
  replaced. Creates of distinct names in one directory still run concurrently.

- **New files created over SMB get the user's own primary GID, as they do over
  NFS.** A user with a GID and no group memberships used to write files owned by
  group 1000, and a user with groups got their first group's GID instead of
  their own.

- **`dfsctl store block add --config` no longer drops the flags given beside
  it.** `--compression`, every `--encryption-*` flag, `--parallel-uploads` and
  the S3 field flags were silently discarded, so a store asked for compression
  or encryption was created without it. Flags are now merged into the JSON, and
  a flag that contradicts a key the JSON already sets is refused. `--type
  memory` now sends the compression, encryption and upload settings as well.

- **An S3 endpoint refused by the private-address guard fails at once.** The SDK
  retried the refused dial up to ten times with backoff, turning a definite
  refusal into a 30-second timeout.

- **The startup warning for a missing config file names `dfs init`**, not
  `dittofs init`.

### Performance

- **Sequential reads of a file that is already local no longer re-download it.**
  Prefetch ignored what the journal already held, so each warm 32 MiB pass
  fetched 24 MiB from the remote again; it now fetches only the ranges that are
  not resident. Demand fetches and integrity checks are unchanged.

## [0.34.0] — 2026-09-24

### Changed

- **The metadata store's on-disk format is now version 2, and the move is
  one-way.** Once a 0.34.0 server opens a Badger metadata store, 0.33.0 and
  earlier refuse to open it and report a future-format error. Nothing is lost
  and nothing is corrupted, but the only way back is a backup taken before the
  upgrade. Existing stores migrate themselves with no migration step to run: a
  file's chunk list is now held in segments rather than one value, reads accept
  both shapes, and the old whole-list key is retired the first time the file is
  written. The reason for the change is that a chunk list past a few thousand
  refs crossed Badger's large-value threshold, after which every commit
  appended a whole fresh copy to a log whose reclamation pass the workload
  never triggered; one field store reached 245 GiB of log against 0.15 GiB of
  live data.

### Fixed

- **Local-tier eviction and backpressure counters report real values.**
  `evictions_total`, `evicted_bytes_total`, `backpressure_total` and
  `backpressure_wait_seconds` had read zero for the life of every process, which
  is worse than their being absent: an operator reads `evictions_total = 0` and
  concludes the local tier never evicts while it evicts constantly. The recorder
  was declared in a package the only production local tier could not name
  without an import cycle, so the capability probe that installed it could never
  match, and the seam's own documentation named a type that does not exist. The
  recorder now lives beside the tier that emits to it.

- **A file's extended attributes are now bounded in total, at 256 KiB.** Each
  value was already capped at 64 KiB but nothing capped their sum, so enough
  legal attributes pushed the file's whole attribute record past the metadata
  backend's large-value threshold. Past that every attribute-only write —
  `chmod`, `utimes`, `rename`, `close` — rewrote the record in full into a log
  the workload never triggered reclamation of: measured at 592 MiB of growth
  over 300 `chmod`s. A write whose result would exceed the total is refused
  with `NFS4ERR_XATTR2BIG`, or `STATUS_EA_TOO_LARGE` over SMB, and stores
  nothing at all rather than part of the set. The bound counts the encoded
  size, so attribute names and framing count toward it, and a replace is
  charged only for its net change. Records already over the threshold are not
  repaired by this; only deleting attributes brings one back.

- **A read over a sparse region no longer rescans the chunk index once per
  preceding chunk.** Finding the chunk that covers a byte offset restarted the
  whole keys-only index scan for every candidate whose row turned out not to
  cover it, so a hole behind *n* chunks cost *n* scans of *n* keys. On a file
  with 5,000 chunks a single such lookup allocated 25 million times and took
  seconds of wall clock. The scan now runs once and a second pass collects the
  remaining candidates, which is 140,000 allocations for the same lookup.
  Covered reads are unchanged, and no key format or schema changed with it.

- **A serialization conflict no longer reaches the caller as an I/O error.**
  Concurrent writers to one file could exhaust a fixed twenty-attempt retry
  budget, after which the conflict surfaced as a failed operation even though
  nothing was wrong with the write, the store or the data. The backoff made it
  worse: it was computed from the attempt number alone, so every loser of one
  conflict waited the same interval and collided again on the next attempt.
  Retries now use randomised exponential backoff bounded by the caller's
  deadline rather than by a constant, which is what the SQL backends already
  did. Measured before the change at one error per two hundred commits with
  eight writers appending to a single file.

- **Renaming a directory into its own subtree is refused with `EINVAL`.** It
  used to be accepted, and it built a cycle of directories that no path
  reached, that the link count reported as alive, and that nothing ever
  collected, so the rows and the content they referenced leaked permanently.
  Any client with write permission on two directories could create one. The
  check runs inside the rename transaction and reads each parent edge through
  it, so a concurrent rename that re-parents anything on the walked chain
  aborts this one. Cycles created by an earlier build are still present and
  still uncollected.

## [0.30.0] — 2026-08-22

### Fixed

- **Journal record CRC now covers the framed FileID.** A record's trailing CRC
  summed only the payload, leaving the FileID bytes outside every checksum: bit
  rot or a torn write there verified clean and recovery replayed the payload
  under whatever file the damaged bytes spelled. The trailing CRC now covers
  `FileID || payload`, and the record magic byte carries the framing version so
  readers pick the matching CRC domain per record. Journals written by earlier
  builds are read unchanged; GC repack re-frames surviving records as it copies
  them forward. Downgrading after this change is one-way — an older build reads
  a new record as a torn tail and truncates the segment there.

## [0.15.0] — 2026-05-27

### Added

- **Block-level compression on remote stores (opt-in).** A new
  `pkg/block/compression` decorator transparently compresses block
  payloads before upload and decompresses on download. Configured per
  remote via the `compression` key in the block-store config JSON
  (`{ "compression": { "algo": "zstd" } }` or `{ "compression": { "algo": "lz4" } }`).
  The plaintext BLAKE3 hash stays the CAS key — dedup, GC, and
  `ReadBlockVerified` semantics are unchanged. Per-block adaptive:
  incompressible bodies are stored raw with no header. See
  `docs/CONFIGURATION.md` for the operator guide.

## [0.13.0] — 2026-04-18

### Breaking Changes

- **Backup/restore/repo CLI surface.** New verbs under
  `dfsctl store metadata <store> backup` — `run`, `list`, `show`, `pin`,
  `unpin`, `restore`, `repo` (add/list/show/edit/remove), and
  `job` (list/show/cancel). Restore and repo management live under
  `backup` so every backup-related operation is in one subtree.

- **New share verbs `disable` / `enable`.** Drain clients + refuse new
  connections. `disable` is synchronous — the command returns only after
  connected clients have been disconnected (or the server's lifecycle
  shutdown timeout fires). `disable` on every share backing a metadata
  store is the required precondition for `backup restore`.

### Added

- **CLI: first-class metadata-store backup/restore.** `dfsctl store metadata <store> backup`
  exposes `run`, `list`, `show`, `pin`, `unpin`, `restore`, `repo`
  (add/edit/list/show/remove), and `job` (list/show/cancel). Repos can be
  local filesystem or S3 with optional AES-256-GCM encryption and cron
  schedules.
- **CLI: `share list` and `share show` surface an `ENABLED` field / column.**
  `share list` adds an `ENABLED` column rendering `yes`/`-`. `share show`
  adds an `Enabled: yes/no` row. Both are surfaced in `-o json` / `-o yaml`
  output via the `enabled` field on the Share record.
- **REST: `POST /api/v1/shares/{name}/disable` + `POST /api/v1/shares/{name}/enable`.**
  Admin-only. Return the updated Share record on success. The disable route
  blocks until the drain completes.
- **REST: backup/restore/job/repo endpoints under
  `/api/v1/store/metadata/{name}/`.** Admin-only. Backup triggers return
  202 Accepted with `{Record, Job}`; restore returns 202 with `BackupJob`.
  Restore refuses with 409 + RFC7807 `RestorePreconditionError` if any
  share backing the store is still enabled.

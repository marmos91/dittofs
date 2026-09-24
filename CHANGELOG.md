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

## Unreleased

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

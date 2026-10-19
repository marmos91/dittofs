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

## [0.36.0] — 2026-10-19

0.36.0 is a fix release, mostly for SMB. Before upgrading, check any SMB
adapter config you have set by hand: **the SMB adapter now refuses an
`encryption` object it cannot apply**, both when the config is stored and at
boot. A stored row that 0.35.0 accepted can keep the SMB adapter down after the
upgrade. See Changed below. No on-disk format changes in this release.

### Changed

- **The SMB adapter refuses an `encryption` setting that it would otherwise
  ignore.** `encryption` must be an object holding only `encryption_mode` and
  `allowed_ciphers`, and `encryption_mode` must be a string. Before, a
  non-object `encryption`, a non-string mode or a misspelt key such as `mode`
  was read as no mode at all. The adapter stayed on `preferred` and `dfsctl`
  reported success, so an operator who asked for `required` still served
  unencrypted SMB 2.x.

- **`allowed_ciphers` can no longer be set; the SMB cipher set is fixed.** The
  key was accepted with any value and never applied: NEGOTIATE kept offering
  every cipher while `dfsctl adapter edit smb --config` reported success. A
  non-empty value is now refused with `allowed_ciphers cannot be configured: the
  SMB cipher set is fixed`. An absent key, `null` and `[]` are still accepted.
  Negotiation itself is unchanged.

  Both checks run when the config is stored and when the server boots. A stored
  row that fails them leaves the SMB adapter down with `Adapter failed to start;
  continuing without it`, while the server, the API and NFS keep running. To
  recover, store the config again without the offending key, for example
  `dfsctl adapter edit smb --config '{"encryption":{"encryption_mode":"required"}}'`.
  The adapter then starts without a server restart. Adapters seeded by default
  carry no config and are not affected.

### Security

- **`encryption.encryption_mode` in the SMB adapter config now takes effect.**
  `dfs start` read `bind_address` and `signing` out of the adapter config but
  skipped `encryption_mode`, so the process stayed on the `preferred` default and
  `required` could not be selected from the adapter config at all.

- **SMB refuses a security descriptor holding an ACE type it cannot store,
  instead of dropping that ACE.** Object ACEs, callback ACEs, mandatory labels and
  other types the server does not model were skipped, the rest of the DACL was
  stored, and the request succeeded. A skipped deny granted what the client had
  denied; a skipped allow silently withdrew a grant. SET_INFO now answers
  `STATUS_INVALID_PARAMETER` and leaves the stored ACL as it was. A CREATE whose
  security descriptor fails to parse or validate is answered the same way and
  creates nothing, where it used to create the file without the descriptor. A
  SACL is parsed whenever one is present, so a SACL carrying anything other than
  audit or alarm ACEs, a mandatory label for instance, now fails the request too,
  even when the client did not ask for the SACL to be set.

- **SMB TREE_CONNECT is refused when the user's share permission cannot be
  resolved.** A store error used to fall back to the share's default permission,
  which can be above a user's explicit grant, or above an explicit `none`. Since
  a tree keeps the permission it was connected with until a control-plane change
  re-checks it, the fallback outlived the failure. TREE_CONNECT now answers
  `STATUS_ACCESS_DENIED`, as NFS already refuses, and the client can retry once
  the store answers. A failed lookup of a principal's SID grants refuses as well.

### Fixed

- **Truncating two files down to the same content no longer fails forever on
  the second one.** The second truncate was refused on every retry, as
  `NFS4ERR_DELAY` over NFS and `STATUS_INSUFFICIENT_RESOURCES` over SMB. Truncate
  gave each file an identifier derived from its remaining content, which the
  Badger store kept unique, although nothing read it any more. That identifier
  and its index are now removed. The on-disk format stays compatible: stores and
  backups written by older versions still open, a file record drops the old field
  the next time it is written, and the leftover index keys are deleted when a
  store is first opened or right after a restore, with a log line
  (`badger: dropped the retired ObjectID index`). On a store with a million such
  keys this takes about 12 seconds. Rolling back to an older version can hit the
  bug again.

- **Opening an existing file for append over NFSv3 no longer empties it.** A
  CREATE in UNCHECKED mode that named an existing file truncated it to zero and
  applied the request's mode and ownership to it. Linux clients send that CREATE
  for `open(O_CREAT)`, so on some kernels `echo x >> file` lost the file's
  contents and wrote at offset 0. Now, as on the Linux kernel server, only a size
  the request carries is applied; without one the file is left alone. On a
  read-only share such a CREATE fails with `NFS3ERR_ROFS`, so a client opening the
  file for writing learns at open rather than at its first write. An existing
  directory or symlink named without a size is answered with its own handle and
  attributes. NFSv4 was not affected.

- **A Windows client can open an encrypted share when the session does not
  require encryption.** In `preferred` mode the server encrypted the
  TREE_CONNECT response for a share that requires encryption, but the client
  learns that from the flag inside that response and was not decrypting yet.
  Windows retried and dropped the connection. TREE_CONNECT is now sent in the
  clear in that case; later commands on the tree are still encrypted, and a
  session that already requires encryption still encrypts TREE_CONNECT.

- **SMB Kerberos accepts a ticket whose addresses are NetBIOS names only.**
  Windows often lists the workstation's NetBIOS name and no IP in a ticket's
  addresses, and comparing those to the connection's IP failed session setup with
  `KRB_AP_ERR_BADADDR`. Non-IP addresses no longer constrain the peer. A ticket
  that lists any IP still has to match the connection's IP. Verify failures now
  log `client_address` and `ticket_addresses`. NFS is unchanged.

- **A whole-share warm no longer holds memory in proportion to the share while
  its progress reporting is stalled.** Each finished block queued one progress
  event until the reporter caught up; pending progress is now two counters.
  Progress calls may therefore jump by more than one block, and a successful warm
  still ends with done equal to total.

- **The operator now shows when the DittoFS API refuses its own account.**
  Applying `spec.snapshotPolicies` and turning on the NFS portmapper both need
  the admin role, so the operator's account (role `operator`) got 403 on each,
  and that showed only in a verbose log line. A new `Authorized` condition on
  the DittoServer is False and names each refused call. It turns True once
  every call gets its answer, and a transient API error leaves it as it was. The
  NFS adapter's Service now publishes port 111 only once the operator has seen
  the portmapper on, so it no longer points at a port nothing listens on. The
  operator still cannot apply these settings without the admin role.

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

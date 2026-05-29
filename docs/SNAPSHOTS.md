# Share Snapshots

Point-in-time, reference-based protection for a DittoFS share.
Operator guide: model, CLI, restore runbook, recovery paths, failure modes.

## Table of Contents

- [1. Overview](#1-overview)
- [2. Snapshot model](#2-snapshot-model)
- [3. CLI walkthrough](#3-cli-walkthrough)
- [4. Creating a snapshot](#4-creating-a-snapshot)
- [5. Listing and inspecting](#5-listing-and-inspecting)
- [6. Deleting a snapshot](#6-deleting-a-snapshot)
- [7. Restore runbook](#7-restore-runbook)
- [8. Recovering from the safety snapshot](#8-recovering-from-the-safety-snapshot)
- [9. The verify gate](#9-the-verify-gate)
- [10. GC hold semantics](#10-gc-hold-semantics)
- [11. Failure modes and recovery](#11-failure-modes-and-recovery)
- [12. Limitations](#12-limitations)
- [13. REST API reference](#13-rest-api-reference)

## 1. Overview

A **share snapshot** is a point-in-time reference to the content of a
share. It captures the metadata for every file in the share at the
moment of the snapshot, plus a manifest listing every content-addressed
(CAS) block those files reference. Block-store garbage collection
respects the manifest as a hold, so the referenced blocks remain
available on local and remote storage until the snapshot is explicitly
deleted.

Snapshots give two operator-level guarantees:

- **Metadata is fully restorable.** The metadata dump preserves the
  exact file tree, permissions, ACLs, timestamps, byte-range locks
  (where applicable), and `[]BlockRef` lists at snapshot time.
- **Referenced CAS blocks are held.** Even if every file in the live
  share is overwritten or deleted, the blocks needed to reconstruct
  the snapshot stay in the block store. There is no data copy: the
  hold is a reference, not a duplication.

Snapshots are **not** a portable archive: they live alongside the share
inside the daemon's storage directory and are not exportable. They are
**not** encrypted at rest (the block store's own encryption settings
apply transitively, but snapshots add nothing beyond that). They are
**not** cross-share: a snapshot of share `/photos` can only be restored
back into share `/photos`.

The deprecated v0.13.0 backup feature (removed in v0.15.0) wrote a
full byte-level copy of every share. Snapshots replace that approach:
no second copy of any block, no scheduler, no separate backup
namespace. The trade-off is that snapshots are intra-cluster — they
protect against accidental writes or deletes, not against losing the
underlying storage.

## 2. Snapshot model

Each snapshot is three artifacts on disk:

```
<localStoreDir>/snapshots/<share>/<snap-id>/
  ├─ metadata.dump          ← engine-native serialization of the metadata store
  ├─ manifest.hashes        ← BLAKE3 hashes of every CAS block the share references
  └─ (GC hold)              ← implicit: manifest-on-disk = held
```

The manifest is a plain-text file, not JSON: one 64-character
lowercase-hex BLAKE3 hash per line, LF-terminated, sorted in ascending
byte order. There is no header, footer, or comment.

The GC hold is **implicit** — there is no separate hold flag in any
database table. Garbage collection enumerates every manifest file
under `<localStoreDir>/snapshots/` at sweep start and excludes the
union of referenced hashes from the candidate set. A snapshot that
exists on disk is automatically protected; deleting a snapshot wipes
its directory and releases the hold on the next sweep.

The snapshot row in the control-plane database tracks lifecycle state:

```
state == creating   → orchestration in flight
state == ready      → manifest + metadata dump complete; safe to restore
state == failed     → orchestration failed; partial artifacts may exist
```

A snapshot transitions `creating → ready` on successful completion of
all orchestration steps, or `creating → failed` on any error. A
`failed` snapshot remains in the database — it is not silently
swept — so operators can inspect why it failed and decide whether to
delete it or retry from it.

The **manifest-on-disk = held** invariant is the central design rule.
Anything that needs to "protect blocks from GC" creates a manifest
file in the snapshots directory; anything that needs to "release the
hold" deletes the file. There is no separate locking protocol, no
held-by counter, no in-memory hold table. The disk is the source of
truth.

## 3. CLI walkthrough

All snapshot operations live under `dfsctl share snapshot`. The five
leaf commands are:

```
dfsctl share snapshot create <share>          # create a new snapshot
dfsctl share snapshot list <share>            # list snapshots for a share
dfsctl share snapshot show <share> <id>       # detail view for one snapshot
dfsctl share snapshot delete <share> <id>     # delete a snapshot (Y/N prompt)
dfsctl share snapshot restore <share> <id>    # restore a share from a snapshot
```

Every command accepts the global `--output, -o` flag (`table|json|yaml`).

### Worked transcript: create

```text
$ dfsctl share snapshot create /photos
Snapshot 7a3ec1b2-9c5e-4ab8-bd31-7f60c2e814a0 queued on share /photos (state: creating)
Snapshot 7a3ec1b2-9c5e-4ab8-bd31-7f60c2e814a0 -> ready
$
```

By default the command blocks until the snapshot reaches `ready` or
`failed`. Use `--no-wait` to return immediately:

```text
$ dfsctl share snapshot create /photos --no-wait
Snapshot 9f2dab17-1a8c-4e02-b6d4-0c2f7a91e3b5 queued on share /photos (state: creating)
$ dfsctl share snapshot show /photos 9f2dab17-1a8c-4e02-b6d4-0c2f7a91e3b5
ID              9f2dab17-1a8c-4e02-b6d4-0c2f7a91e3b5
STATE           creating
...
```

### Worked transcript: list

```text
$ dfsctl share snapshot list /photos
ID         NAME            STATE    DURABLE  CREATED  SIZE
7a3ec1b2   weekly-2026-05  ready    yes      2h ago   -
9f2dab17   pre-cleanup     ready    yes      4d ago   -
```

JSON mode round-trips through the same DTO the REST API returns:

```text
$ dfsctl share snapshot list /photos -o json
[
  {
    "id": "7a3ec1b2-9c5e-4ab8-bd31-7f60c2e814a0",
    "name": "weekly-2026-05",
    "share": "/photos",
    "state": "ready",
    "remote_durable": true,
    "created_at": "2026-05-27T18:14:22Z",
    "updated_at": "2026-05-27T18:14:25Z"
  },
  ...
]
```

### Worked transcript: show

```text
$ dfsctl share snapshot show /photos 7a3ec1b2-9c5e-4ab8-bd31-7f60c2e814a0
ID              7a3ec1b2-9c5e-4ab8-bd31-7f60c2e814a0
NAME            weekly-2026-05
SHARE           /photos
STATE           ready
REMOTE DURABLE  yes
MANIFEST COUNT  1842
DUMP BYTES      4.1 MiB
RETRY OF        -
ERROR           -
CREATED AT      2026-05-27T18:14:22Z
UPDATED AT      2026-05-27T18:14:25Z
```

`show` requires the **full** snapshot UUID, not the 8-character
prefix shown in `list` (see §5). It reports the manifest hash count
(`MANIFEST COUNT`) and the human-readable dump size (`DUMP BYTES`);
`list` omits them to keep the row count cheap.

### Worked transcript: delete

```text
$ dfsctl share snapshot delete /photos 9f2dab17
Delete snapshot 9f2dab17 from share /photos?
Type 'y' to confirm: y
Snapshot 9f2dab17 deleted.
$
```

Use `--yes` to skip the confirmation:

```text
$ dfsctl share snapshot delete /photos 9f2dab17 --yes
Snapshot 9f2dab17 deleted.
```

## 4. Creating a snapshot

```
dfsctl share snapshot create <share> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--name` | `""` | Human-friendly name. Stored alongside the snapshot for operator reference; does not need to be unique. |
| `--no-verify` | `false` | Skip the verify gate (upload drain + remote HEAD probes). GC hold still applies; remote-durability is not asserted. |
| `--retry` | `""` | Resume from a `failed` snapshot ID. The orchestrator re-runs the steps from the failure point; the original snapshot row transitions to `ready` on success. |
| `--no-wait` | `false` | Return immediately with the new snapshot ID and exit 0. Otherwise the command blocks on `WaitForSnapshot` and exits 0 on `ready` or non-zero on `failed`. |

### `--no-verify` semantics

Normally a snapshot orchestration runs in this order:

1. Persist the snapshot row in `state=creating`.
2. Drain pending rollups so all written data is persisted to CAS and
   reflected in each file's block list.
3. Write `metadata.dump` from the now-settled metadata store.
4. Compute the hash manifest from the share's referenced blocks.
5. Drain in-flight uploads to the remote block store.
6. Run the verify gate: HEAD-probe every block hash on the remote
   block store (concurrency = 16) to confirm remote durability.
7. Transition to `state=ready` (or `state=failed` on any error).

`--no-verify` skips step 5 (upload drain) and step 6 (HEAD probes).
The snapshot still completes, the GC hold still applies, but the
`remote_durable` flag is `false`. Use it when:

- You want a fast local-only snapshot for an imminent risky operation
  (e.g., a config push that might break an adapter).
- The remote block store is temporarily unreachable but the local
  block store is intact.

Restoring a `remote_durable=false` snapshot requires the explicit
`--force` flag (§7). Without `--force`, restore refuses with
`ErrSnapshotNotDurable` (HTTP 412).

### `--retry` semantics

Snapshots fail when the orchestration cannot complete — a drain
times out, the metadata dump errors, or the verify gate finds blocks
missing on the remote. The failure mode is recorded on the snapshot
row as `state=failed` plus an `error` string.

`--retry=<failed-id>` re-runs the orchestration against the same
snapshot row. The row's `state` flips back to `creating` while the
retry runs. On success the original ID stays — there is no second
snapshot record — and `remote_durable` reflects the retry's outcome.

```text
$ dfsctl share snapshot list /photos --state=failed
ID         NAME       STATE    DURABLE  CREATED  SIZE
4c19fbe0   nightly    failed   no       6h ago   -

$ dfsctl share snapshot create /photos --retry 4c19fbe0-...-full-uuid
Snapshot 4c19fbe0-...-full-uuid queued on share /photos (state: creating)
Snapshot 4c19fbe0-...-full-uuid -> ready
```

Retry refuses if the target ID does not exist (`404`) or is not in
`failed` state (`409 Conflict`).

## 5. Listing and inspecting

```
dfsctl share snapshot list <share> [flags]
dfsctl share snapshot show <share> <id>
```

`list` flags:

| Flag | Default | Description |
|---|---|---|
| `--state` | `""` | Filter by state. One of `creating`, `ready`, `failed`. |
| `--name-prefix` | `""` | Filter by name prefix (case-sensitive). |
| `--no-relative` | `false` | Render `CREATED` as ISO 8601 instead of relative ("2h ago"). |

Filters AND together. There is no pagination flag — snapshot counts
per share stay in the low hundreds in practice. There is no sort
flag; the list is always newest-first.

### Table columns

| Column | Meaning |
|---|---|
| `ID` | First 8 characters of the snapshot UUID — **truncated for display only**. See the note below. |
| `NAME` | Operator-set name from `--name`. Blank if unset. |
| `STATE` | `creating` / `ready` / `failed`. |
| `DURABLE` | `yes` if `remote_durable=true`; `no` otherwise. |
| `CREATED` | Relative time by default; ISO with `--no-relative`. |
| `SIZE` | Dump size, but always `-` in list mode — the list handler does not stat artifacts. Use `show` to see it. |

> **The `ID` column is truncated to 8 characters for readability, but
> `show`, `delete`, `restore`, and `create --retry` all require the
> *full* snapshot UUID.** Passing the 8-character prefix returns
> `404 ErrSnapshotNotFound` — the server matches snapshot IDs exactly
> and does not resolve prefixes. Get the full UUID from `list -o json`
> (the `id` field is never truncated).

The `SIZE` column is `-` in `list` because populating it would
require one `stat` per row (the manifest lives on disk, not in the
database). `show` resolves it on demand for a single record:

```text
$ dfsctl share snapshot show /photos 7a3ec1b2-9c5e-4ab8-bd31-7f60c2e814a0
ID              7a3ec1b2-9c5e-4ab8-bd31-7f60c2e814a0
...
MANIFEST COUNT  1842
DUMP BYTES      4.1 MiB
...
```

If `show` cannot stat the manifest or the dump (artifact missing,
permissions error), the corresponding field renders as `-` rather
than failing the command — the snapshot row may still be useful for
operator triage even when its artifacts are corrupt.

### JSON and YAML modes

`-o json` and `-o yaml` return the full DTO including disk fields
(populated for `show`, omitted for `list`):

```text
$ dfsctl share snapshot show /photos 7a3ec1b2 -o yaml
id: 7a3ec1b2-9c5e-4ab8-bd31-7f60c2e814a0
name: weekly-2026-05
share: /photos
state: ready
remote_durable: true
manifest_count: 1842
dump_bytes: 4302848
created_at: 2026-05-27T18:14:22Z
updated_at: 2026-05-27T18:14:25Z
```

## 6. Deleting a snapshot

```
dfsctl share snapshot delete <share> <id> [--yes]
```

`delete` removes the snapshot row, wipes the on-disk directory
(`<localStoreDir>/snapshots/<share>/<id>/`), and releases the GC
hold for any block referenced only by this snapshot.

By default the command prompts `Y/N`:

```text
$ dfsctl share snapshot delete /photos 9f2dab17
Delete snapshot 9f2dab17 from share /photos?
Type 'y' to confirm: n
Aborted.
```

`--yes` skips the prompt. Use it from scripts only.

### Safety snapshots are not special

Every successful restore creates a `pre-restore-*` safety snapshot
(§7) that captures the share's state immediately before the restore
overwrote it. These are normal snapshots — the `delete` command does
not refuse them, treat them specially, or warn. Operators are
expected to delete them explicitly after the restore is validated.
The reason for this design is uniformity: there is no separate
"safety" namespace, no `--really-yes` escape hatch, no second
confirmation. The `delete` command behaves the same way for every
snapshot.

### GC reclamation timing

Block-store GC runs on its own schedule (`dfsctl store block gc
<share>`). Deleting a snapshot releases the hold immediately, but
the underlying blocks remain in the block store until the next GC
sweep enumerates them as unreferenced. The window between `delete`
and reclamation is bounded only by your GC cadence; if you need to
reclaim space immediately, follow `delete` with an on-demand
`dfsctl store block gc <share>`.

If a block is referenced by another snapshot or by a live file in
the share, GC will still skip it — the hold semantics are union,
not exclusive.

## 7. Restore runbook

Restore replaces the share's metadata store with the snapshot's
saved state. It is a **destructive** operation against the live
share: any file changes between the snapshot and the restore are
discarded. To make the destruction recoverable, restore always
creates a `pre-restore-*` safety snapshot first.

### Order of operations

```
1. Stop traffic. Ensure no clients are writing to the share.
2. dfsctl share disable /<share>
3. dfsctl share snapshot restore /<share> <snap-id>
      (interactive Y/N; --yes to skip)
4. Verify data integrity (sample files, check timestamps,
   compare known checksums).
5. dfsctl share enable /<share>
6. Verify the safety snap exists, then delete it after the
   grace period you set internally:
      dfsctl share snapshot delete /<share> <safety-snap-id>
```

### Why the share must be disabled first

The restore handler refuses on an enabled share:

```text
$ dfsctl share snapshot restore /photos 7a3ec1b2
share /photos is enabled; run 'dfsctl share disable /photos' first
$ echo $?
1
```

There is no auto-disable / auto-enable wrapper around restore. The
explicit disable step exists so the operator unambiguously owns the
"this share is going down" decision; auto-enable would silently
return the share to service before integrity has been validated.

### Worked transcript: happy path

```text
$ dfsctl share disable /photos
Share /photos disabled.

$ dfsctl share snapshot restore /photos 7a3ec1b2
Restore snapshot 7a3ec1b2 into share /photos?
A safety snapshot of the current share state will be created first.
Type 'y' to confirm: y
Restored snapshot 7a3ec1b2 into share /photos.
Safety snap: c12e8d4f (delete with
'dfsctl share snapshot delete /photos c12e8d4f' after verifying).

$ # Sample some files to confirm the restore brought back what you expect.
$ ls /mnt/photos/2024/      # (after a temp mount or via another client)
...

$ dfsctl share enable /photos
Share /photos enabled.

$ # After validation, delete the safety snap:
$ dfsctl share snapshot delete /photos c12e8d4f --yes
Snapshot c12e8d4f deleted.
```

### Restore steps (what the server actually does)

1. Pre-flight: refuse if the share is enabled.
2. Verify the source snapshot is remotely durable (unless `--force`).
3. Create the pre-restore safety snapshot. Its ID is returned to
   the caller in the same response.
4. Close the metadata store cleanly.
5. Reset the metadata store via its `Resetable` interface.
6. Replay the snapshot's `metadata.dump` into the empty store.
7. Walk the restored metadata to build a hash set of every block
   the restored share now references.
8. Run a post-restore block verify against the block store to
   confirm every required hash is reachable.

If step 3 fails, the share is unchanged. If steps 4–8 fail, the
safety snapshot exists and can be restored to roll back; the
restored share may be in a partial state until you do so.

### `--force` for non-durable snapshots

```text
$ dfsctl share snapshot restore /photos 9f2dab17
Snapshot 9f2dab17 is not remotely durable. Re-run with --force to restore anyway.
$ echo $?
1

$ dfsctl share snapshot restore /photos 9f2dab17 --force --yes
Restored snapshot 9f2dab17 into share /photos.
Safety snap: e0a2b15c (delete with ...).
```

`--force` maps to `RestoreSnapshotOpts.AllowNonDurable=true` and
corresponds to `allow_non_durable=true` in the REST request body.
The verify gate's HEAD-probes are skipped on the source snapshot's
manifest, so the restore may fail later if a referenced block is
genuinely missing from the remote. The flag exists for the case
where you accept the risk knowingly — for example, the remote is
temporarily unreachable and you have local copies you trust.

### Restore is synchronous

The REST endpoint blocks until restore completes. The HTTP request
is bounded by the server's `snapshot.restore_http_timeout` config
(default 30 minutes); the CLI's HTTP client matches. For very
large shares with slow remotes, increase both before starting.

## 8. Recovering from the safety snapshot

The safety snapshot is the first line of recovery if a restore was
accepted but later found to have brought back the wrong state — for
example, the operator picked the wrong snapshot ID, or a downstream
service expected post-snapshot data that the restore overwrote.

To roll back:

```text
$ dfsctl share disable /photos
$ dfsctl share snapshot restore /photos <safety-snap-id>
Restore snapshot c12e8d4f into share /photos?
A safety snapshot of the current share state will be created first.
Type 'y' to confirm: y
Restored snapshot c12e8d4f into share /photos.
Safety snap: 88d40a73 (delete with ...).
$ dfsctl share enable /photos
```

Two important properties:

- **Each restore creates a fresh safety snap.** Restoring the safety
  snap creates ANOTHER safety snap that captures the
  post-first-restore state. The chain depth grows by one with every
  restore. There is no auto-cleanup — operators delete safety snaps
  explicitly after validation.
- **Safety snaps are full snapshots.** They occupy a normal slot in
  `list`, hold GC references, and respect every `--state` /
  `--name-prefix` filter. There is no separate query for "show me
  the safety snaps for share X" — convention names them
  `pre-restore-<source-id>-<timestamp>` so `--name-prefix=pre-restore-`
  filters them.

### When to delete safety snaps

Keep them until you have confidence the restored state is correct.
A reasonable cadence:

- Sample-verify the restored share immediately after `enable`.
- Run downstream consumers (the application stack that uses the
  share) for a grace period — for example, one business day or one
  full backup-window — and confirm no integrity issues surface.
- Delete the safety snap once the grace period elapses.

Failing to delete safety snaps eventually consumes GC budget
(blocks held by the chain cannot be reclaimed until the chain is
gone). Setting an internal SOP for deletion keeps that bounded.

## 9. The verify gate

The verify gate is the optional remote-durability check inside
snapshot create. The full create pipeline runs in this order so that
the manifest reflects every block the share actually references and
every referenced block is proven durable on the remote:

1. **Drain rollups.** Pending rollups are flushed so all written data
   is persisted to CAS and reflected in each file's block list. The
   manifest computed in the next step is taken from those settled
   block lists, not from in-flight state.
2. **Snapshot.** The metadata dump and the hash manifest are written
   from the now-settled metadata store.
3. **Drain uploads.** In-flight uploads to the remote block store are
   drained so every manifest hash has had a chance to land remotely.
   If the syncer cannot drain within its configured timeout, create
   fails with `ErrSnapshotDrainTimeout` (HTTP 504).
4. **HEAD probe.** Every block hash in the manifest is HEAD-probed
   on the remote block store, with parallelism of 16 in flight.
   Any missing hash fails the snapshot with `ErrSnapshotVerifyFailed`
   (HTTP 500, sanitized message).

If the upload drain and HEAD probe both pass, `remote_durable=true`
on the snapshot row. If `--no-verify` was passed, steps 3 and 4 are
skipped and `remote_durable=false` without testing.

The 16-way parallelism is hardcoded. It is well below typical
remote-store rate limits (S3, R2, Backblaze B2) and large enough
to fill bandwidth at typical chunk sizes. If a future workload
demonstrates a need to tune it, the knob can be re-introduced;
there is no operator setting today.

The verify gate is what makes `--force` necessary for restore of a
non-durable snapshot: a snapshot whose manifest was never
HEAD-validated against the remote may reference blocks that have
since been deleted out-of-band (lifecycle rule, bucket cleanup,
mis-configuration). The verify gate at snapshot time is the only
HEAD-probe pass; restore trusts the result.

## 10. GC hold semantics

The block-store GC and the snapshot subsystem coordinate through
one rule:

> **Manifest-on-disk = block held.**

Concretely:

- GC's mark phase enumerates `<localStoreDir>/snapshots/<share>/*/manifest.hashes`
  at sweep start and reads every hash referenced inside.
- Those hashes are unioned with the live `FileAttr.Blocks` hashes
  from the metadata store.
- Any block whose hash is in the union survives the sweep.
- Any block whose hash is in neither is unreferenced and is swept.

A `failed` snapshot whose orchestration crashed partway through may
have a partial manifest file. GC still respects it as a hold — better
to retain an extra block than to delete one a recovery might need.
Run `dfsctl share snapshot delete` to release the hold once the
failed snapshot is no longer useful.

### Delete-vs-GC race window

`delete` performs three steps:

1. Acquire a per-share delete lock.
2. Remove the snapshot row from the database.
3. Wipe `<localStoreDir>/snapshots/<share>/<id>/` from disk.

If GC starts a sweep between steps 2 and 3 (a narrow window), it
still sees the manifest file on disk and the block hashes inside
still count as held. The race direction is **safe**: GC never
deletes a block that delete had only just decided to release. The
worst-case outcome is a deferred reclamation, which the next sweep
fixes.

The reverse race — GC sweeping between step 3 and a subsequent
`create` — is impossible because `create` writes the new manifest
to disk before computing references; the new manifest is visible
to the next GC enumeration as soon as it lands.

## 11. Failure modes and recovery

Restore is the path most likely to surface real failures because it
combines durability assumptions, on-disk artifacts, and metadata
store internals. The 9 known failure modes, in operator language:

### share-enabled-at-restore

**Symptom.** `restore` returns exit 1 with the hint
`share /<name> is enabled; run 'dfsctl share disable /<name>' first`.
REST: HTTP 409 Conflict, `ErrShareEnabled`.

**Cause.** The pre-flight check refused because the share was
serving traffic.

**Recovery.** Run `dfsctl share disable /<name>`, then re-run
restore.

### snapshot-not-found

**Symptom.** `restore` or `show` returns HTTP 404,
`ErrSnapshotNotFound`.

**Cause.** The snapshot ID does not exist in the share's record
list. Often a typo, occasionally a snapshot that was deleted out
from under the operator.

**Recovery.** Run `dfsctl share snapshot list <share>` to find the
correct ID.

### snapshot-not-durable

**Symptom.** `restore` returns HTTP 412 with the hint to re-run
with `--force`. REST: `ErrSnapshotNotDurable`.

**Cause.** The snapshot's `remote_durable` flag is `false` — it was
created with `--no-verify`, or its verify gate failed and it was
walked back to `failed` then partially recovered.

**Recovery.** Confirm you accept the risk that some referenced
blocks may be missing from the remote, then re-run with
`--force --yes`. If the restore subsequently fails partway through
with a verify error, fall back to the safety snap.

### metadata-dump-missing

**Symptom.** Restore returns HTTP 500 with the sanitized message
`snapshot artifacts missing`. REST:
`ErrSnapshotMetadataDumpMissing`.

**Cause.** The on-disk `metadata.dump` file is gone (operator
cleanup, disk failure, lost share data directory). The snapshot row
still exists in the database but its replay artifact does not.

**Recovery.** The snapshot is unrestorable. Delete it
(`dfsctl share snapshot delete`) and restore from another snapshot
if available. If no other usable snapshot exists, this is a real
data-loss event — restore from off-cluster backups (out of scope
for this subsystem).

### metadata-store-not-resetable

**Symptom.** Restore returns HTTP 500 with the sanitized message
`backend does not support reset`. REST: `ErrMetadataStoreNotResetable`.

**Cause.** The metadata store backend in use does not implement the
`Resetable` interface required for in-place wipe-and-replay. As of
this release, all production backends (BadgerDB, PostgreSQL)
implement `Resetable`; the in-memory backend used for tests
implements it too. This error should not occur in production.

**Recovery.** File an issue with the backend name and version. If
the backend is correctly configured, this is a packaging bug.

### safety-snap-create-failed

**Symptom.** Restore returns HTTP 500. REST:
`ErrRestoreSafetySnapFailed`.

**Cause.** The pre-restore safety snapshot could not be created.
The most common cause is insufficient disk space in the snapshots
directory.

**Recovery.** The live share is unchanged — restore aborted before
touching it. Free disk space (`df`, `du
<localStoreDir>/snapshots/`), then re-run.

### restore-aborted-mid-flight

**Symptom.** Restore returns HTTP 500 after a delay. REST:
`ErrRestoreAborted`.

**Cause.** Restore was interrupted between safety-snap creation
and final verify — most often by HTTP timeout, container kill, or
manual cancel. The safety snap exists; the metadata store may be in
a partial state.

**Recovery.** Re-restore the safety snap to roll back. Investigate
the cause of the interruption (logs, resource limits, the
`snapshot.restore_http_timeout` config). Re-run the original restore
once the cause is fixed.

### post-restore-verify-failed

**Symptom.** Restore returns HTTP 500. REST: `ErrRestoreVerifyFailed`.

**Cause.** The metadata replay succeeded but the post-restore block
hash-set walk found a referenced block missing from the block
store. The snapshot's manifest claimed durability that the block
store cannot satisfy now.

**Recovery.** Re-restore the safety snap to roll back. Investigate
the missing blocks on the remote (lifecycle policy, bucket
cleanup, operator deletion). If the blocks are recoverable from
off-cluster backups, restore them and re-run.

### upload-drain-timeout

**Symptom.** Snapshot create returns HTTP 504, sanitized message
`upload drain timed out`. REST: `ErrSnapshotDrainTimeout`.

**Cause.** The verify gate's drain step could not complete within
the syncer's timeout — there is a backlog of uploads waiting on a
slow remote.

**Recovery.** Wait for the upload backlog to drain, or use
`--no-verify` to skip the drain (accepting that the snapshot is
not remotely durable). Re-run create with `--retry=<failed-id>` to
re-attempt against the same row.

## 12. Limitations

- **No cross-share restore.** A snapshot of `/photos` can only be
  restored back into `/photos`. There is no surface to clone or
  fork a share through the snapshots feature.
- **No encryption.** Snapshot artifacts inherit whatever
  encryption (or lack thereof) is configured on the block store
  and the file system holding `<localStoreDir>/snapshots/`. There
  is no snapshot-specific encryption today.
- **No auto-cleanup of safety snaps.** Each restore leaves a
  safety snap. Operators delete them after validation.
- **Synchronous restore.** The HTTP request blocks; the CLI
  blocks. Bounded by `snapshot.restore_http_timeout` (default 30
  minutes). There is no async restore with a poll endpoint.
- **Single-node only.** Snapshots live alongside the share inside
  one daemon's local store. There is no cluster-aware snapshot
  surface yet.
- **No scheduled snapshots.** Snapshot creation is on demand only.
  Wire `dfsctl share snapshot create` into your cron/systemd
  timer if you want a recurring cadence.
- **No portable archive format.** Snapshots cannot be exported,
  emailed, or restored on a different daemon's storage. They
  protect against accidental writes and deletes; they do not
  protect against losing the daemon's storage.

For background on these decisions, see
[ARCHITECTURE.md — Share Snapshots](ARCHITECTURE.md#share-snapshots).
For the CLI surface, see
[CLI.md — Share Snapshots](CLI.md#share-snapshots).

## 13. REST API reference

All snapshot endpoints live under the existing
`/api/v1/shares` admin group and inherit `RequireAdmin`. Auth is
JWT — pass an admin token via the `Authorization: Bearer ...`
header. A full OpenAPI spec is not in tree today; this section is
the brief reference.

| Method | Path | Purpose | Success |
|---|---|---|---|
| `POST` | `/api/v1/shares/{name}/snapshots` | Create a snapshot (async) | `202 Accepted` + `Location: /api/v1/shares/{name}/snapshots/{id}` + body `{snapshot_id, share}` |
| `GET` | `/api/v1/shares/{name}/snapshots` | List snapshots for a share | `200 OK` + JSON array of snapshot records |
| `GET` | `/api/v1/shares/{name}/snapshots/{id}` | Get one snapshot record | `200 OK` + full record |
| `DELETE` | `/api/v1/shares/{name}/snapshots/{id}` | Delete a snapshot | `204 No Content` |
| `POST` | `/api/v1/shares/{name}/snapshots/{id}/restore` | Restore a share from a snapshot (sync) | `200 OK` + body `{snapshot_id, safety_snapshot_id, share}` |

### Create body

```json
{ "name": "weekly-2026-05", "no_verify": false, "retry_of": "" }
```

All fields optional. `no_verify=true` skips the verify gate (§4).
`retry_of=<failed-id>` reattempts a prior failed snapshot.

### Restore body

```json
{ "allow_non_durable": false }
```

`allow_non_durable=true` is the equivalent of the CLI's `--force`
(§7). The default is `false` and restore refuses on a
`remote_durable=false` snapshot.

### Restore response

```json
{
  "snapshot_id": "7a3ec1b2-9c5e-4ab8-bd31-7f60c2e814a0",
  "safety_snapshot_id": "c12e8d4f-2c19-4a72-9e3f-44b1b8f60017",
  "share": "/photos"
}
```

`safety_snapshot_id` is the ID of the pre-restore safety snap. If
restore failed before safety-snap creation, the field is omitted.

### Error responses

Errors are returned as `application/problem+json` with sanitized
messages. The sentinel-to-status mapping is:

| Sentinel | Status | Sanitized message |
|---|---|---|
| `ErrSnapshotNotFound` | 404 | `snapshot not found` |
| `ErrShareNotFound` | 404 | `share not found` |
| `ErrShareEnabled` | 409 | `share is enabled; disable before restore` |
| `ErrSnapshotNotDurable` | 412 | `snapshot not remotely durable; pass allow_non_durable=true to force` |
| `ErrSnapshotRetryTargetNotFound` | 404 | `retry target snapshot not found` |
| `ErrSnapshotRetryTargetNotFailed` | 409 | `retry target is not in failed state` |
| `ErrSnapshotDrainTimeout` | 504 | `upload drain timed out` |
| `ErrSnapshotMetadataDumpMissing` | 500 | `snapshot artifacts missing` |
| `ErrMetadataStoreNotResetable` | 500 | `backend does not support reset` |
| `ErrSnapshotBackupFailed` | 500 | `snapshot operation failed` |
| `ErrSnapshotVerifyFailed` | 500 | `snapshot operation failed` |
| `ErrRestoreSafetySnapFailed` | 500 | `snapshot operation failed` |
| `ErrRestoreAborted` | 500 | `snapshot operation failed` |
| `ErrRestoreVerifyFailed` | 500 | `snapshot operation failed` |

The original error remains in the daemon's structured logs at
`Error` level for operator triage. The HTTP response is intentionally
generic to avoid leaking internal detail.

### Restore HTTP timeout

The restore endpoint wraps the request context in
`context.WithTimeout(ctx, cfg.Snapshot.restore_http_timeout)`. The
default is 30 minutes. Configure via the server YAML:

```yaml
snapshot:
  restore_http_timeout: 1h
```

For very large shares, raise the timeout on both the server config
and the CLI's HTTP client (`apiclient.WithRestoreTimeout`).

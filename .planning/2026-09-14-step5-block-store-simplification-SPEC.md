# Step 5 — one block store, one journal

Spec for retiring the configurable local block store and the local/remote kind discriminator.
Companion to `2026-09-01-block-dataflow-MASTER-PLAN.md` §15; decisions D11–D23 live in that
document's §8 and are summarised here. Where the two disagree, the master plan's §8 wins.

Status as of 2026-09-14: branch `chore/drop-local-blockstore-config`, 14 commits, tree green at
every commit. Not mergeable yet — see §7.

---

## 1. What changes, in one paragraph

A share's local tier is a journal, always. It is provisioned automatically under one server-level
directory and is no longer a configurable entity. A *block store* is therefore always the remote,
durable tier, so the local/remote kind discriminator disappears from the model, the REST API and
the CLI. Every share has exactly one block store. The vocabulary follows: what an operator
configures is a **journal** (local, automatic) and a **block store** (remote, named).

## 2. Target surface

### 2.1 Server config

```yaml
blockstore:
  journal:                      # was: blockstore.local
    path: /var/lib/dittofs/blocks   # NEW — the shared journal root
    chunk_size: 0                   # promoted from per-share config
    chunk_max: 0                    # promoted
    dirty_expire: 0s                # promoted
    max_log_bytes: 0
    backpressure_max_wait: 60s
    # REMOVED: default_remote_cache_size (folded into per-share journal_size, D22)
```

Each share's journal lives at `<path>/shares/<sanitised-name>/journal/`. The `shares/<name>` level
is deliberately preserved so a root adopted from an existing install addresses the directories it
always did.

A config file still carrying `blockstore.local.*` is **refused** with both key names (D18).
Unknown keys otherwise remain warn-and-ignore; a *renamed* key cannot ride that policy, because the
operator believes the setting is live and an equivalent exists under a new name.

### 2.2 CLI

| Before | After |
|---|---|
| `dfsctl store block local {add,edit,list,remove}` | *(deleted)* |
| `dfsctl store block remote {add,edit,list,remove}` | `dfsctl store block {add,edit,list,remove}` |
| `dfsctl store block health --kind <local\|remote>` | `dfsctl store block health` |
| `dfsctl share create --local X --remote Y` | `dfsctl share create --block-store Y` |
| `dfsctl share edit --local X --remote Y` | `dfsctl share edit --block-store Y` |
| `--local-store-size` | `--journal-size` |

Eight verbs are already kind-agnostic and **must come through unchanged**: `stats`, `evict`,
`health`, `gc`, `gc-status`, `audit-refcounts`, `reconcile`, `reclaim`. A collapse that reshapes
those is the most likely review miss. No name collides when the four verbs move up.

`share list` collapses its `LOCAL STORE` / `REMOTE STORE` columns to one `BLOCK STORE`, and stops
making two `ListBlockStores` round trips. `share show` collapses two rows to one.

### 2.3 REST

`/api/v1/store/block/{kind}/…` becomes `/api/v1/store/block/…`. Seven routes, one handler set,
unchanged verbs. `extractKind`, `validateBlockStoreType`'s local arm, `errWrongBlockStoreKind`,
`resolveBlockStoreRef`'s kind parameter and the seven `"must be 'local' or 'remote'"` strings all go.

**Do not delete the `/shares/{name}/blockstore/…` mounting workaround on the strength of its
comment.** The comment says it exists because chi cannot disambiguate `/store/block/{kind}` from
`/store/block/{name}`. Removing `{kind}` changes the stated cause but not the constraint — the new
`/store/block/{name}` collides with the same class of wildcard.

JSON fields: `local_block_store_id` removed; `remote_block_store_id` → `block_store_id`;
`local_store_size` → `journal_size`.

### 2.4 Model

```
Share.LocalBlockStoreID    -> removed
Share.RemoteBlockStoreID   -> BlockStoreID        (required)
Share.LocalStoreSize       -> JournalSize
Share.<new>                -> CommitAck           (D21, see §3)
BlockStoreConfig.Kind      -> removed
unique (name, kind)        -> unique (name)
```

## 3. Durability, and what a COMMIT promises

Per-share flag, default **journal**:

- **`journal`** (default) — NFS COMMIT / SMB Flush acknowledge once the write is durable in the
  journal. Faster; survives process and host crash, not device loss.
- **`block-store`** — acknowledge only once the data has reached the block store. Slower; survives
  device loss.

This replaces the `durable: false` override that auto-provisioning would otherwise have deleted.
It is a **durability promise**, and an operator cannot infer which one is in force by observing
behaviour, so it must be documented in the guides *and* surfaced in the product (`share show`,
`dfs status`), not only in a config reference.

## 4. Journal size and eviction

`journal_size` unset → the journal grows without a configured ceiling.
`journal_size` set → eviction runs as the journal approaches it, and **only blocks already
offloaded to the block store are eligible**. Anything else is the only copy.

Two consequences that are easy to get wrong:

1. A cap with nothing yet offloaded **cannot be honoured by eviction**. Write backpressure remains
   the only correct response there; it is not dead code under this decision.
2. Unset-means-unbounded is a real change from today's deduced 25%-of-RAM ceiling. A fast writer
   against a slow uploader can now fill the volume. This needs a release note.

`default_remote_cache_size` is deleted, along with its "10 GiB when a remote is configured,
deduced size otherwise" conditional.

## 5. Migration

Runs at startup, all of it **before** `AutoMigrate` where it touches columns.

| Condition | Behaviour |
|---|---|
| `shares.local_store_size` present | rename to `journal_size` — **must precede AutoMigrate**, or AutoMigrate adds an empty `journal_size` and the operator's ceiling is stranded in a dead column, reading as "never set" |
| both `local_store_size` and `journal_size` present | **refuse** — which is authoritative is not recoverable from the schema, and guessing changes every share's ceiling |
| shares disagree about their journal root | **refuse**, naming each share and its path. One shared location gets a "set `blockstore.journal.path` to X" directive; several get told plainly that one root cannot express them (D14) |
| a local and a remote store share a name | **refuse**, naming both rows (D23) |
| `block_store_configs.kind` present | drop, after the collision check |
| legacy `payload_store_id` upgrade path | `gorm.go:413` hard-codes `INSERT … kind='local'`; it fails once the column is gone and must be fixed or explicitly dropped in the same change |

The refusals are deliberate and consistent: relocating a journal moves the only copy of every byte
not yet offloaded, and a partial move during startup cannot be undone. Refusing is recoverable.

## 6. Work breakdown

| # | Item | State |
|---|---|---|
| 1 | `blockstore.journal.path` + renamed-key refusal | **done** |
| 2 | `blockstore.local` → `blockstore.journal` | **done** |
| 3 | `CheckJournalRoot` divergent-path refusal | **done** |
| 4 | `JournalSize` rename (19 files) | **done** |
| 5 | `local_store_size` → `journal_size` column migration | **done** |
| 6 | Delete `dfsctl store block local` (5 files + docs) | **done** |
| 7 | Promote chunk/dirty-expire knobs; plumb the journal root | **done** |
| 8 | `OpenShareJournal` | **done** |
| 9 | `CommitAck` column + migration (D21) | open |
| 10 | Switch `createBlockStoreForShare`; drop `LocalBlockStoreID` | open |
| 11 | Fold the ceiling into `journal_size`; gate eviction on offloaded blocks (D22) | open |
| 12 | Drop `Kind`; unique-index migration + collision refusal (D23) | open |
| 13 | REST route collapse + apiclient | open |
| 14 | dfsctl collapse (4 verbs up, 8 untouched) | open |
| 15 | Remove the ~10 dead `HasRemote` branches (D20) | open |
| 16 | `dfs status` two-fetch merge — **silent failure, do first** | open |
| 17 | Scripts, e2e helpers, workflow | open |
| 18 | Docs: 9 guides, README, gendocs regen, D21 durability promise | open |

## 7. Merge gate

The branch **cannot merge** until items 10, 14 and 17 land together.
`.github/workflows/smb-client-compat.yml:83,213,422` creates a local store with a deleted command
and then `share create --local`; there is no valid intermediate form. Same for
`test/e2e/helpers/stores.go`. Both compile — they fail at runtime — so CI will not catch this
before merge.

## 8. Accepted costs

- The evict safety guard loses its reachable condition (D20). It exists to prevent data loss;
  removing it is deliberate, not incidental.
- Breaking API change: 7 request/response types, the CLI flag surface, and three Go exports.
- Unbounded journal growth by default (D22).
- `chunk_size` stops being per-share, so a node mixing a VM-image share with a general-purpose one
  gets one chunking profile.

## 9. Verification

- Migration tested through the real `New()` path, not a copy of it: build a current schema, rename
  the column backwards to stand in for an old database, reopen, assert the value survived. Both
  cases confirmed red with the migration disabled.
- Every refusal has a test asserting `errors.Is` unwraps to its sentinel. This is not ceremony:
  the caller decides refuse-vs-warn by that match, and a wrapped copy of a different error
  degrades a refusal into a warning, which mounts the share anyway.
- A survey of this surface **cannot be done by grepping the CLI spelling**. Consumers using the
  typed client match nothing — `test/e2e/helpers/stores.go` carries 54 block-store references and
  no CLI grep sees them. Search `BlockStore`, `ListBlockStores`, `BlockStoreOption`,
  `WithBlockS3Config` alongside the command spelling.
- Any sweep must name the tree it scanned, as a SHA, in its output.

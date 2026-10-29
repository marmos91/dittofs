# RFC 17 filesystem service: implementation plan (stage 2 of #3119)

Stage 2 of wiring the RFC 16 store into the server (#3119, route B): the filesystem service of
[RFC 17](../rfcs/rfc-17-vfs.md), `*vfs.Server`, the one component that orders every call across
the metadata store, open state ([RFC 14](../rfcs/rfc-14-open-state.md), stage 1) and the engine
([RFC 8](../rfcs/rfc-8-engine.md), stage 3). Adapters call it through `vfs.Service`; they move onto
it in stage 5.

**Status:** draft for review. Nothing here is built yet.

**Terms.** A bare §N is RFC 17's; this plan's own sections are "plan §N". Vnn is a row of RFC 17
§6. Route B: the new service is built beside today's server, and nothing switches over until
stage 5. E11 is the item of #3152 that puts the engine on the RFC 16 store.

## 1. What already exists

- **The store** (`pkg/metadata/kvstore`, merged): the server calls only `Namespace`, `Files` and
  `Capacity` (RFC 7), short of the calls listed in plan §5 Q3.
- **Open state** (`pkg/openstate`): stage 1 piece 1 merged (#3256), pieces 2–8 in review (#3330,
  #3336, #3341, #3344, #3363, #3400; piece 8 as #3456, #3457, #3458, #3417 and #3459).
  `vfs.New` takes `*openstate.Table` in `Deps.Open` but keeps only its seven views (§2.3).
- **The engine:** not built. Journal v2 has its types, errors and data-path declarations
  (#3157, #3159, #3162, #3190); the engine itself is stage 3 (#3152, E11).
- **Today's server** on `develop`: `pkg/metadata.Service`, the protocol handlers that each order
  their own calls, and `pkg/metadata/lock`. Route B ports none of it (D1), except the
  pseudo-filesystem (piece 3).

## 2. Decisions to settle

**D1. Route B: the server is built over the new store, not over today's `metadata.Service`.**
- `Deps.Meta` is `*kvstore.Store` from the first piece; §2.3's `*metadata.Store` is edited to
  match (D7), since on `feat/storage-v2` that name is today's interface.
- §9's slices 1–3, which first delegate to today's `metadata.Service` and lock manager, are a
  migration of today's server; on `feat/storage-v2` they would build a second server that route B
  then deletes. §9 says the same in a `decision:`: slice 1's delegation and slice 3's shim are
  skipped, and slice 0's types come from the new store.

**D2. Single node only.** The server and nothing else of §2: no `Gateway`, `Serve`, `Router`,
`Transport` or `Peers`, which §9 puts in its own cluster slice. `Deps.Peers` is nil, and every
call that names a second primary on a cluster runs locally. Left out with it: the multi-node tier,
moving the adapters onto `vfs.Service` (stage 5) and RFC 20's shared pipeline, pNFS layouts
(§10.2) and compound batching (§10.1).

**D3. The engine through `Content`.** `internal/vfs` declares `Content`: exactly the RFC 8 methods
§3.5's table and §5.3 call, no more. A fake implements it until stage 3; the real
`*engine.Engine` then satisfies it unchanged. The method list (plan §4) is the contract to agree
with the journal v2 team before either side builds against it.

**D4. Identity through a minimal real resolver.**
- `Deps.Identity` is RFC 18 §6.1's `Resolver`, `Resolve(ctx, id Identity, p metadata.SharePolicy,
  readOnly bool) (*Caller, error)`.
- Until RFC 18's lands, a minimal one in `internal/identity` implements it over
  `Principals.SharePolicy`, `ByProtocolID` (a `Claimed` caller's uid), `Groups` and `Grant`, and
  `Records.User` and `Group` (refusing a disabled or deleted principal, RFC 18 §6.1), filling
  `Caller.Mapped` before squash and `Principal` after it, so open state receives the unsquashed
  principal (RFC 14's `Caller.Principal`). It is the resolver, not a fake of it, so squash is written once:

  ```go
  // ponytail: resolves from the store's SharePolicy, Groups and Grant alone,
  // uncached; RFC 18's resolver replaces it, adding the cache and directories.
  ```
- `Deps.Auth` and `Deps.Names` wait for RFC 18 with the `Principals` area (piece 3).

**D5. Packages as §2.4 lays them out.** `pkg/vfs` is the contract (types, errors, ID codecs,
`Service`, `Callbacks`, `Events`) plus `vfstest` (`Record`, `Null`, `RecordingCallbacks`) and its
`suite`. `internal/vfs` is the server, one file per operation, with `admit`, `pseudofs`,
`lockorder`, `quota` and `authz` as subpackages.

**D6. Tests in §8's tiers, cheapest first.** Every piece lands with its unit-tier cases over the
in-memory store, the fake `Content` and the real open-state table on `sim.TestClock`
(`pkg/sim/clock.go`); no area fakes an open-state view. The integration tier needs the real
engine, so it starts with stage 3; until then its cases, and so the V-rows, run against the fake.

**D7. The RFC moves with the code** (§9): a piece that needs a signature change edits the RFC in
the same PR. Known now, and more will come as pieces are built:
- §2.3's `Deps.Meta` becomes `*kvstore.Store` (D1, piece 2);
- RFC 7 §11.1's `Capacity` gains `Quota`, the read view the server already uses (piece 9);
- §3.5 gains `Watches.Changed` after each change committed in a directory (RFC 14 §2.5), and
  §2.3 gives `Names`, `Attributes` and `Data` the `Watches` view (piece 10);
- from piece 1's build: §3.4's `DecodeStateID` returns `(OpenRef, error)`, and every encoder
  returns an error for a value that doesn't fit;
- from piece 1's build: §3.0's `Protocol` aliases `entity.Protocol`, `GrantKind` and
  `RevokeReason` alias open state's, and RFC 18's `Service` is `SignInService` beside
  `vfs.Service`.

The questions piece 1's build left open, which may add more, are plan §5 Q4.

## 3. Pieces, in build order

| # | Piece | RFC 17 | Needs |
|---|---|---|---|
| 1 | Contract | §3.0–§3.4, §4.3 | `develop`'s RFCs on `feat/storage-v2` |
| 2 | Server skeleton | §2.3, §2.5, §5.0 | piece 1; the `Content` contract (plan §4); stage 1 piece 2 (`Table.Start`) |
| 3 | Shares, admission and principals | §3.1 `Shares`, `Principals`, §4.6, §4.9 | piece 2; the store's read views (plan §5 Q3) |
| 4 | Clients and callbacks | §3.1 `Clients`, §3.2 | piece 2; stage 1 pieces 2, 4, 5, 6, 7 |
| 5 | Names | §5.4, §5.10 | piece 3; stage 1 pieces 3 (`CheckChange`), 6 (`Renamed`) |
| 6 | Attributes | §5.5, §5.7 | piece 3; the store's read views (`Streams`); stage 1 pieces 3 (`CheckChange`, `Update`) |
| 7 | Open, close and release recovery | §2.5, §5.3 | pieces 3, 4, 5; stage 1 pieces 3, 6 (`ServeShard`), 7 (`GraceEnded`); #3202 piece 0's follow-up (`RaiseIncarnations`) |
| 8 | Data | §5.1, §5.2, §5.8 | piece 7 |
| 9 | Quota | §5.6 | piece 8; the store's read views (plan §5 Q3) |
| 10 | Locks, grants, watches | §5.11, §3.2 | pieces 4, 7, 8; stage 1 pieces 4, 5, 8 |
| 11 | Copy and clone | §5.9 | piece 8; stage 3; stage 1 piece 8 (`CloneOpen`, `CloneClose`) |

"Done when" means every listed row passes and each of its cases is revert-checked. A row may be
split: its first piece lands the cases it can reach, and the piece that completes it is named
beside it, e.g. V31 (→8). V4, V11 and V41 are the cluster's; V19, V20 and V25 complete in
stage 3, against the real engine. Of the rows outside the V-list: RFC 14's L1 is piece 10's
and L20 piece 8's, and L29's restart case piece 7's; §6's §4.6 and §5.3 rows are piece 7's, its two §3.2 rows piece 10's;
its §5.9 row (an `Incomplete` source) waits for RFC 12's snapshot view.

1. **Contract:** `pkg/vfs`'s value types and the aliases §3.0 lists from the metadata store and
   open state (`StateRef`, `Revocation`, `RevokeReason`, `ClientStatus`, `LockRange`,
   `LockWaiter` among them, and `ClientRecord` for RFC 14's `Client`); the error set with
   `Delay`, `StateError`, `CrossError`, `AccessError` and `QuotaError`, `vfs.Errors()` and
   `vfs.Mutates`; the codecs of every client-held ID, at RFC 14 §2.11's settled widths (a 24-bit
   shard, 5 grace-instance bits, a 40-bit `OpenID`, §10.3); `Service` as its seven areas;
   `vfstest`'s `Record`, `Null`, `RecordingCallbacks` and `suite.Run`; §8's fuzz targets for
   every ID decoder.
   Done when: every codec matches bytes written out by hand from §3.4's layout and RFC 14 §2.11's
   widths (no RFC carries vectors yet, plan §5 Q4), and `vfs.Errors()` holds every sentinel and
   typed kind of §4.3.
2. **Server skeleton:** `Deps` and `New`; `Deps.Events`, the null sink by default, its queue
   started at `Start` and flushed at `Close`, its drops counted in `vfs_events_dropped_total`;
   the four states (built, serving, draining, closed) and
   the `Start`, `Drain` and `Close` transitions; the server's RFC 14
   [`Sink`](../rfcs/rfc-14-open-state.md#11.1%20Package%2C%20dependencies%20and%20wiring), every
   method stubbed, which the node hands to `Table.Start` with the server's admission lock, since
   `Table.Start` refuses a nil one; the `lockorder` sorter that takes open-state steps, then
   [fold admission](../rfcs/rfc-7-namespace-metadata.md#9.2%20Timestamps), then accept locks
   ([§5.0](../rfcs/rfc-17-vfs.md#5.0%20One%20lock%20order)), in one order; `Content`
   and its fake; the architecture test (imports and call sites).
   Done when: V29 (→11), V32, V33 (→7), V34 (→10) pass.
3. **Shares, admission and principals:** `Root`, `SecInfo`, `ListShares`, `StatFS` and `Quota`;
   admission on every call, uncached; `authz` as the one caller of `Authorize` and `Granted`,
   emitting the `AccessEvent`s the audit policy selects; §8's fuzz target for handles.
   - The pseudo-filesystem is ported from `internal/adapter/nfs/v4/pseudofs`, already rebuilt
     from the share list, to `internal/vfs/pseudofs`, with §3.4's digest, and file IDs and change
     attributes derived from the path and the share list's version, not counters; today's copy
     stays under today's server.
   - `Snapshots` and the `Principals` area (`SignIn`,
     [`Spell`](../rfcs/rfc-17-vfs.md#3.1%20Operations), `Find`) are stubs, since nothing calls
     them before stage 5 and nothing is under them yet: `Snapshots` answers an empty list
     until RFC 12's store view exists, and the three `Principals` calls answer `ErrAccess` until
     RFC 18 lands, with `Deps.Auth` and `Deps.Names`.
   - §4.9 allows admission's steps 1–4 to be cached; stage 2 does not, so it has no cache key to
     get wrong:

     ```go
     // ponytail: admission's steps 1-4 run on every call, uncached; cache them
     // per (share, identity) with every version §4.9 lists in the key once a
     // profile shows them.
     ```

     §8's cache-key unit case, its cached-against-uncached benchmark, and V9's and V12's cached
     cases wait with the cache.
   Done when: V9 (→7), V12 (→10), V13, V14, V39, V40 (→5) pass.
4. **Clients and callbacks:** the `Clients` area over open state's `Clients` view, the per-client
   `Callbacks` registry, which `Connect` fills and a second `Connect` replaces, and the `Sink`'s
   delivery to it. The `Sink`'s `Closed` and `GraceEnded` stay stubs until piece 7 builds
   release.
   Done when: V36 passes.
5. **Names:** `Lookup`, `ReadDir`, `Create`, `Link`, `Unlink`, `Rmdir` and `Rename` over the
   store's `Namespace`, with open state's `CheckChange` before each change; §8's fuzz targets
   for cursors and names.
   Done when: V3 (→8), V28 (→7), V31 (→8), V40 pass.
6. **Attributes:** `GetAttr`, `SetAttr` with size changes, ACLs, xattrs, streams and `Access`,
   each returning the `WCC` its reply needs.
   Done when: V22 (→8), V31 (→8) pass.
7. **Open, close and release recovery:** `Start`'s step 1, `Recovery.ServeShard` for each shard
   (RFC 14 §11.3); opens, deny modes, delete on close and the four moments of release; the pending
   releases a shard holds run when its grace ends (`Sink.GraceEnded`);
   `vfs_deferred_unlink_refusals_total`.
   Done when: V8 (→10), V9, V17, V21, V28, V33, V35 (→10), V38, and RFC 14's L29 pass: open then close, and delete
   on close, run in the unit tier, and each open meeting a deny mode held only in a handle cache
   gets `ErrDelay`, the lease is broken, and the retry succeeds (V21); `ErrShareViolation` comes
   only after handle caching is broken (§4.3).
8. **Data:** `Write`, `Read`, `Commit`, `Deallocate` and `Seek` in §5.1's steps, and the write
   verifier. `WriteOpts.Done` is a no-op until piece 9:

   ```go
   // ponytail: Done releases nothing, as no quota is reserved yet; once writes
   // reserve quota, Done releases the reservation here.
   ```
   Done when: V3 (→9), V7 (→10), V16, V18, V19 (→stage 3), V20 (→9), V22 (→11), V25 (→stage 3),
   V31 (→11) pass; here V20 and V25 test only that `DelayCapacity` passes through, not mapped to
   `ErrNoSpace`.
9. **Quota:** reservations and their release through `WriteOpts.Done`, a create charged to the
   new file's owner; `QuotaEvent`s where the quota is enforced.
   Done when: V3, V10, V20 (→stage 3), V37 pass.
10. **Locks, grants, watches:** byte-range locks with waiters, `AckRecall`, `Watch`/`Unwatch`, and
    `Watches.Changed` after each change committed in a directory, from `Names`, `Attributes` and
    `Data` (D7).
    Done when: V6, V7, V8, V12, V23, V24, V34, V35 pass.
11. **Copy and clone:** synchronous, through the engine's `Copy` and `Clone`, after stage 3:
    against the fake, `Copy` is a memcpy whose tests can only pass.
    Done when: V22, V26, V29, V30, V31 pass.

## 4. The engine contract

`Content` is the one interface between stage 2 and stage 3, and the one thing both teams build
against before either is done. From §3.5 and RFC 8 §12.1:

| Method | The server calls it for |
|---|---|
| `Draw` | a version for any change, under the file's accept lock |
| `Write`, `Overlay`, `Commit` | the write path (§5.1) and stable commits (§5.8) |
| `Read`, `Allocation` | reads, SEEK and READ_PLUS (§5.2) |
| `LossSeq` | the loss sequence an open records, which a flush through it compares (§5.3) |
| `Truncate`, `Deallocate` | size changes and hole punching (§5.5) |
| `Delete` | an orphaned file's content, at its last close or unlink (§5.3); kept apart from the journal's own `Release`, which evicts |
| `Copy`, `Clone` | server-side copy and clone (§5.9) |

The signatures are RFC 8 §12.1's: `Write(ctx, f, off, p []byte, o WriteOpts{Suspend, Done})
(Verifier, error)`, where `Done` releases the quota reservation, and `Commit(ctx, f, off, n)
(Verifier, lossSeq uint64, error)`, whose loss sequence `Opens.Flushed` reads.

Stage 2 writes the fake, since neither today's `engine.Store` nor `journalv2test/model` can stand
in for it.

A check that any `Content` passes (verified bytes back, a version above the last drawn, nothing
returned before its stage) waits for stage 3: run against the fake alone, it only proves the fake
agrees with itself.

Not the server's to call: `Apply`, `Start`, `Close`, the storage-binding calls `Bind`, `Unbind`
and `Drop`, and the controls `PrepareCut`, `CommitCut`, `PreWarm`, `WaitOffloaded` and `Stats`,
which are the node's, a removal's or a cut's.

## 5. Open questions

1. **The engine contract (plan §4):** is `Content` frozen against E11 of #3152 before piece 2?
2. **D4:** when does RFC 18's resolver land to replace the minimal one?
3. **Gaps below the server** (§10.4): the store calls the server needs that `feat/storage-v2`
   lacks. All are store work, and so ours. Quota records already have a read view,
   `Capacity.Quota` (`pkg/metadata/kvstore/quota.go`); the share indexes have none but
   `ControlPlane`, which the server must not call.

   | Needed by | Missing on `feat/storage-v2` |
   |---|---|
   | D4's resolver (admission's step 4) | `Principals.SharePolicy`; `Grant` returns `(ShareAccess, error)`, not RFC 16's `(ShareAccess, bool, error)`; RFC 16's `Records` (`User`, `Group`) |
   | piece 3 `authz`, piece 6 streams | `Files.Granted`, `Files.Streams` (RFC 7) |
   | piece 3 `StatFS` | `Capacity.Info`; `Usage` has a different signature |
   | piece 3, admission's steps 1–3 | a read view of the share list, path index, export policy, client rules and netgroups |
   | piece 7 `Start`, V17 and V35's restarts | a per-shard grace instance and incarnation, raised by the node's start transaction (RFC 14 §4.4, §11.2) |

4. **Piece 1's open questions** (posted on #3264): RFC 21's stateid type tags; the byte order
   inside `other` and `FileId`'s bit layout; when a `FileId` is stale; `ErrLocked`'s holder; one
   hex vector per ID in RFC 21; whether `Record` and `Spell`, which change nothing, belong on
   `Mutates`' read-only list, and where `Find`, which can mint, goes; whether `vfs.ErrLost` aliases `openstate.ErrLost`; and §4.3
   answers for `ErrStopped`, `ErrLost` and `DelayFrozen`. Until §4.3 answers `ErrStopped`, the
   server's mapping of open state's errors is not total (#3414 maps `ErrCreateInUse` as
   `StateError` `CreateInUse`).

## 6. What sets the pace

What blocks what is the Needs column of plan §3.

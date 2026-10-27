# RFC 17 filesystem service: implementation plan (stage 2 of #3119)

Stage 2 of wiring the RFC 16 store into the server (#3119, route B): the filesystem service of
[RFC 17](../rfcs/rfc-17-vfs.md), `*vfs.Server`, the one component that orders every call across
the metadata store, open state ([RFC 14](../rfcs/rfc-14-open-state.md), stage 1) and the engine
([RFC 8](../rfcs/rfc-8-engine.md), stage 3). Adapters call it through `vfs.Service`; they move onto
it in stage 5.

**Status:** draft for review. Nothing here is built yet.

## 1. What already exists

- **The store** (`pkg/metadata/kvstore`, merged) has the RFC 16 views, short of the calls listed
  in open question 3; the server calls only
  `Namespace`, `Files` and `Capacity` (RFC 7). The others are its dependencies' (RFC 16 §3):
  open state holds `OpenRecords` (`Clients`, `Opens`), the engine `Existence` and `Content`,
  identity `Principals`, `Records` and `Minter`, and the API `ControlPlane`.
- **Open state** (`pkg/openstate`): pieces 1–7 of stage 1 in review (#3256, #3330, #3336,
  #3341, #3344, #3363, #3400), piece 8 planned in #3202. `vfs.New` takes `*openstate.Table` in `Deps.Open` but keeps only its
  seven views (RFC 17 §2.3); the server implements its `Sink`, which the node hands to
  `Table.Start` with the server's admission lock.
- **The engine:** not built. Journal v2 has its types, errors and data-path declarations
  (#3157, #3159, #3162, #3190); the engine itself is stage 3 (#3152, E11).
- **Today's server** on `develop`: `pkg/metadata.Service`, the protocol handlers that each
  order their own calls, and `pkg/metadata/lock`. Route B ports none of it (D1).

## 2. Decisions to settle

**D1. Route B: the server is built over the new store, not over today's `metadata.Service`.**
`Deps.Meta` is `*kvstore.Store` from the first piece; RFC 17 §2.3's `*metadata.Store` is edited
to match (D7), since on `feat/storage-v2` that name is today's interface. RFC 17 §9's slices 1–3, which first
delegate to today's `metadata.Service` and lock manager, are a migration of today's server;
on `feat/storage-v2` they would build a second server that route B then deletes. RFC 17 §9 now
says the same in a `decision:`: slice 1's delegation and slice 3's shim are skipped, and slice 0's
types come from the new store.

**D2. Single node only.** The server and nothing else of §2: no `Gateway`, `Serve`, `Router`,
`Transport` or `Peers`, which RFC 17 §9 puts in its own cluster slice. `Deps.Peers` is nil, and
every call that names a second primary on a cluster runs locally.

**D3. The engine through `Content`.** `internal/vfs` declares `Content`: exactly the RFC 8
methods §3.5's table calls, no more. A fake implements it until stage 3; the real
`*engine.Engine` then satisfies it unchanged. The method list (§4) is the contract to agree
with the journal v2 team before either side builds against it.

**D4. Identity through a minimal real resolver.** `Deps.Identity` is RFC 18 §6.1's `Resolver`,
`Resolve(ctx, id Identity, p metadata.SharePolicy, readOnly bool) (*Caller, error)`. Until RFC
18's lands, a minimal one in `internal/identity` implements it over `Principals.SharePolicy`,
`Groups` and `Grant`, filling `Caller.Mapped` before squash and `Principal` after it, so open
state receives the unsquashed principal (RFC 14's `Caller.Principal`). It is the resolver, not a
fake of it, so squash is written once:

```go
// ponytail: resolves from the store's SharePolicy, Groups and Grant alone,
// uncached; RFC 18's resolver replaces it, adding the cache and directories.
```

`Deps.Auth` and `Deps.Names` wait for RFC 18 with the `Principals` area (piece 3).

**D5. Packages as RFC 17 §2.4 lays them out.** `pkg/vfs` is the contract (types, errors, ID
codecs, `Service`, `Callbacks`, `Events`) plus `vfstest` (`Record`, `Null`,
`RecordingCallbacks`) and its `suite`. `internal/vfs` is the server, one file per operation,
with `admit`, `pseudofs`, `lockorder`, `quota` and `authz` as subpackages.

**D6. Tests in RFC 17 §8's tiers, cheapest first.** Every piece lands with its unit-tier cases
over the in-memory store, the fake `Content` and the real open-state table on `sim.TestClock`
(`pkg/sim/clock.go`); no area fakes an open-state view. The
integration tier needs the real engine, so it starts with stage 3; until then its cases run
against the fake. Every case is revert-checked, and tests that cannot fail are not written.

**D7. The RFC moves with the code** (RFC 17 §9): a piece that needs a signature change edits
the RFC in the same PR. Two are known: §2.3's `Deps.Meta` becomes `*kvstore.Store` (D1, piece
2), and §3.5 gains `Watches.Changed` after each change it commits in a watched directory, which
RFC 14 §2.5 asks for and §3.5's rows lack (pieces 5 and 6).

## 3. Pieces, in build order

| # | Piece | RFC 17 | Needs |
|---|---|---|---|
| 1 | Contract | §3.0–§3.4, §4.3 | nothing |
| 2 | Server skeleton | §2.3, §2.5, §5.0 | piece 1 |
| 3 | Shares, admission and principals | §3.1 `Shares`, `Principals`, §4.6, §4.9 | piece 2 |
| 4 | Clients and callbacks | §3.1 `Clients`, §3.2 | stage 1 pieces 2, 4, 5, 6, 7 |
| 5 | Names | §5.4, §5.10 | piece 3; stage 1 pieces 3 (`CheckChange`), 6 (`Renamed`), 8 (`Changed`) |
| 6 | Attributes | §5.5, §5.7 | piece 3; stage 1 pieces 3 (`CheckChange`, `Opens.Update`), 8 (`Changed`) |
| 7 | Open, close and release recovery | §2.5, §5.3 | pieces 3, 5; stage 1 pieces 3, 6, 7 |
| 8 | Data | §5.1, §5.2, §5.8 | piece 7 |
| 8b | Quota | §5.6 | piece 8; the store's quota read views |
| 9 | Locks, grants, watches | §5.11, §3.2 | stage 1 pieces 4, 5, 8 |
| 10 | Copy and clone | §5.9 | piece 8; stage 3; stage 1 piece 8 (`CloneOpen`, `CloneClose`) |

What each piece carries:

1. **Contract:** `pkg/vfs`'s value types and the aliases RFC 17 §3.0 lists from the metadata
   store and open state (`StateRef`, `Revocation`, `RevokeReason`, `ClientStatus`, `LockRange`,
   `LockWaiter` among them, and `ClientRecord` for RFC 14's `Client`); the error set with
   `Delay`, `StateError`, `CrossError`, `AccessError` and `QuotaError`, `vfs.Errors()` and
   `vfs.Mutates`; the codecs of every client-held ID, checked against RFC 21's and RFC 22's
   golden bytes; `Service` as its seven areas; `vfstest`'s `Record`, `Null`,
   `RecordingCallbacks` and `suite.Run`.
2. **Server skeleton:** `Deps`, `New`, and the four states (`Start`, `Drain`, `Close`), `Start`'s
   step 1 calling `Recovery.ServeShard` for each shard, so open state refuses nothing it serves
   (RFC 14 §11.3); the
   `lockorder` sorter that takes open-state steps, fold admission and accept locks in one
   order; `Content` and its fake; the architecture test (imports and call sites).
3. **Shares, admission and principals:** `Root`, `SecInfo`, `ListShares`, `StatFS` and `Quota`;
   the pseudo-filesystem, ported from `internal/adapter/nfs/v4/pseudofs` (already rebuilt from
   the share list) to `internal/vfs/pseudofs` with only the handle encoding changed to §3.4's
   digest, today's copy staying under today's server; admission on every call, uncached;
   `authz` as the one caller of `Authorize` and `Granted`. `Snapshots` and the `Principals` area
   are stubs, since nothing calls them before stage 5 and nothing is under them yet: `Snapshots`
   answers an empty list until RFC 12's store view exists, and `SignIn`, `Spell` and `Find`
   answer `ErrAccess` until RFC 18 lands, `Deps.Auth` and `Deps.Names` with it:

   ```go
   // ponytail: admission's steps 1-4 run on every call, uncached; cache their
   // result per (share, client, identity) as RFC 17 §4.6 allows once a profile
   // shows them, with a key that names every field the result depends on.
   ```
4. **Clients and callbacks:** the `Clients` area over open state's `Clients` view, the per-client
   `Callbacks` registry, which `Connect` fills and a second `Connect` replaces, and the server's
   `Sink`, handed to `Table.Start` with the server's admission lock; its `Closed` and
   `GraceEnded` are stubs until piece 7 builds release. Each area holds only the open
   state views it calls (`Opens`, `Locks`, `Grants`, `Watches`, `Recovery`, `Checks`); its tests
   use the real table on `sim.TestClock`.
5. **Names:** `Lookup`, `ReadDir`, `Create`, `Link`, `Unlink`, `Rmdir` and `Rename` over the
   store's `Namespace`, with open state's `CheckChange` before each change and
   `Watches.Changed` after each commit (a D7 edit to RFC 17 §3.5).
6. **Attributes:** `GetAttr`, `SetAttr` with size changes, ACLs, xattrs, streams and `Access`,
   each returning the `WCC` its reply needs, and `Watches.Changed` after each commit.
7. **Open and close, grace and release recovery:** opens, deny modes, delete on close and the
   four moments of release; the pending releases run when a shard's grace ends
   (`Sink.GraceEnded`), which is release's own work, as grace itself is `Start`'s step 1.
8. **Data:** `Write`, `Read`, `Commit`, `Deallocate` and `Seek` in §5.1's steps, and the write
   verifier. `WriteOpts.Done` is a no-op until piece 8b:

   ```go
   // ponytail: Done releases nothing, as no quota is reserved yet; piece 8b
   // reserves quota and releases it here.
   ```
8b. **Quota:** quota reservations and their release through `WriteOpts.Done`.
9. **Locks, grants, watches:** byte-range locks with waiters, `AckRecall`, `Watch`/`Unwatch`.
10. **Copy and clone:** synchronous, through the engine's `Copy` and `Clone`, after stage 3:
    against the fake, `Copy` is a memcpy whose tests can only pass.

## 4. The engine contract

`Content` is the one interface between stage 2 and stage 3, and the one thing both teams
build against before either is done. From RFC 17 §3.5 and RFC 8 §12.1:

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

Stage 2 writes the fake; neither today's `engine.Store` nor `journalv2test/model` can stand in
for it. A check that any `Content` passes (verified bytes back, a version above the last drawn,
nothing returned before its stage) waits for stage 3: run against the fake alone it only proves
the fake agrees with itself. `Apply`,
`Start`, `Close`, the storage-binding calls `Bind`, `Unbind` and `Drop`, and the controls
`PrepareCut`, `CommitCut`, `PreWarm`, `WaitOffloaded` and `Stats` are the node's (or a removal's,
or a cut's) to call, not the server's.

## 5. What stays out of stage 2

- The cluster: the gateway, `Serve`, routing, transport, `Peers` and the multi-node tier.
- Moving the adapters onto `vfs.Service` (stage 5), and RFC 20's shared pipeline.
- pNFS layouts (RFC 17 §10.2) and compound batching (§10.1).
- RFC 17 §9's slices that delegate to today's `metadata.Service` (D1).

## 6. Open questions

1. **The engine contract (§4):** who writes the fake, and when is `Content` frozen against
   E11 of #3152?
2. **D4:** when does RFC 18's resolver land, and is the store-backed fake enough until then?
3. **Gaps below the server** (RFC 17 §10.4): the store calls the server needs that
   `feat/storage-v2` lacks. All are store work, and so ours. Quota records already have a read
   view, `Capacity.Quota` (`pkg/metadata/kvstore/quota.go`); the share indexes have none but
   `ControlPlane`, which the server must not call.

   | Needed by | Missing on `feat/storage-v2` |
   |---|---|
   | D4's resolver (admission's step 4) | `Principals.SharePolicy`; `Grant` returns `(ShareAccess, error)`, not RFC 16's `(ShareAccess, bool, error)` |
   | piece 3 `authz`, piece 6 streams | `Files.Granted`, `Files.Streams` (RFC 7) |
   | piece 3 `StatFS` | `Capacity.Info`; `Usage` has a different signature |
   | piece 3, admission's steps 1–3 | a read view of the share list, path index, export policy, client rules and netgroups |

ID widths are settled: RFC 14 §2.11 fixes a 24-bit shard, 5 grace-instance bits and a 40-bit
`OpenID` in the stateid and the SMB `FileId` (RFC 17 §10.3).

## 7. Size and what sets the pace

About fifteen PRs, roughly one per piece and two for the larger ones (5, 8, 9). Review sets the
pace, as for stage 1. What blocks what:

- `develop`'s RFCs (#3142) merged into `feat/storage-v2`, before piece 1;
- the `Content` contract agreed before piece 2, which writes its fake;
- stage 1 pieces 2–8 before stage 2 pieces 4 (2, 4, 5, 6, 7), 5–7 (3), 9 (4, 5, 8), 10 (6)
  and 11 (6);
- the store's share and quota read views (open question 3) before pieces 3 and 8.

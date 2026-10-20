# RFC 17 filesystem service: implementation plan (stage 2 of #3119)

Stage 2 of wiring the RFC 16 store into the server (#3119, route B): the filesystem service of
[RFC 17](../rfcs/rfc-17-vfs.md), `*vfs.Server`, the one component that orders every call across
the metadata store, open state ([RFC 14](../rfcs/rfc-14-open-state.md), stage 1) and the engine
([RFC 8](../rfcs/rfc-8-engine.md), stage 3). Adapters call it through `vfs.Service`; they move onto
it in stage 5.

**Status:** draft for review. Nothing here is built yet.

## 1. What already exists

- **The store** (`pkg/metadata/kvstore`, merged): the views the server calls. `Namespace`,
  `Files` and `Capacity` (RFC 7); `Existence` and `Content` (RFC 6); `Clients` and `Opens`
  (RFC 14's durable records); `Principals`; `ControlPlane`.
- **Open state** (`pkg/openstate`): pieces 1 and 2 of stage 1 in review (#3256, #3258), the
  rest planned in #3202. The server holds `*openstate.Table` and implements its `Sink`.
- **The engine:** not built. Journal v2 has its types, errors and data-path declarations
  (#3157, #3159, #3162, #3190); the engine itself is stage 3 (#3152, E11).
- **Today's server** on `develop`: `pkg/metadata.Service`, the protocol handlers that each
  order their own calls, and `pkg/metadata/lock`. Route B ports none of it (D1).

## 2. Decisions to settle

**D1. Route B: the server is built over the new store, not over today's `metadata.Service`.**
`Deps.Meta` is `*kvstore.Store` from the first piece. RFC 17 §9's slices 1–3, which first
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

**D4. Identity through a resolver fake.** `Deps.Identity` is RFC 18's `Resolver` (#3144, in
review). Until it exists, the server takes a small resolver over `kvstore.Principals` that maps
an `Identity` to principals and groups, enough for admission and the chokepoint, replaced
when RFC 18 lands.

**D5. Packages as RFC 17 §2.4 lays them out.** `pkg/vfs` is the contract (types, errors, ID
codecs, `Service`, `Callbacks`, `Events`) plus `vfstest` (`Record`, `Null`,
`RecordingCallbacks`) and its `suite`. `internal/vfs` is the server, one file per operation,
with `admit`, `pseudofs`, `lockorder`, `quota` and `authz` as subpackages.

**D6. Tests in RFC 17 §8's tiers, cheapest first.** Every piece lands with its unit-tier cases
over the in-memory store, the fake `Content` and real open state on a fake clock. The
integration tier needs the real engine, so it starts with stage 3; until then its cases run
against the fake. Every case is revert-checked, and tests that cannot fail are not written.

**D7. The RFC moves with the code** (RFC 17 §9): a piece that needs a signature change edits
the RFC in the same PR.

## 3. Pieces, in build order

| # | Piece | RFC 17 | Needs |
|---|---|---|---|
| 1 | Contract | §3.0–§3.4, §4.3 | nothing |
| 2 | Server skeleton | §2.3, §2.5, §5.0 | piece 1 |
| 3 | Shares and admission | §4.6, §4.9 | piece 2 |
| 4 | Clients and callbacks | §3.1 `Clients`, §3.2 | stage 1 pieces 2, 5 |
| 5 | Names | §5.4, §5.10 | piece 3; stage 1 piece 3 (`CheckChange`) |
| 6 | Attributes | §5.5, §5.7 | piece 3 |
| 7 | Open and close | §5.3 | stage 1 piece 3 |
| 8 | Data | §5.1, §5.2, §5.6, §5.8 | piece 7; the `Content` contract (§4) |
| 9 | Locks, grants, watches | §5.11, §3.2 | stage 1 pieces 4, 5, 8 |
| 10 | Copy and clone | §5.9 | piece 8 |
| 11 | Grace and release recovery | §2.5, §5.3 | stage 1 piece 6 |

What each piece carries:

1. **Contract:** `pkg/vfs`'s value types and the aliases of open state's (`StateRef`,
   `Revocation`, `ClientStatus`, `LockRange`); the error set with `Delay`, `StateError` and
   `CrossError`; the codecs of every client-held ID, checked against RFC 21's and RFC 22's
   golden bytes; `Service` as its six areas; `vfstest`'s `Record`, `Null` and
   `RecordingCallbacks`.
2. **Server skeleton:** `Deps`, `New`, and the four states (`Start`, `Drain`, `Close`); the
   `lockorder` sorter that takes open-state steps, fold admission and accept locks in one
   order; `Content` and its fake; the architecture test (imports and call sites).
3. **Shares and admission:** `Root`, `SecInfo`, `ListShares`, the pseudo-filesystem as a pure
   function of the share list, admission on every call with its cache key, and `authz` as the
   one caller of `Authorize`.
4. **Clients and callbacks:** the `Clients` area over open state's `Clients` view, the per-client
   `Callbacks` registry, which `Connect` fills and a second `Connect` replaces, and the server's
   `Sink`, handed to `Table.Start` with the server's admission lock. Each area holds only the open
   state views it calls (`Opens`, `Locks`, `Grants`, `Watches`, `Recovery`, `Checks`), and fakes
   only those in its unit tests.
5. **Names:** `Lookup`, `ReadDir`, `Create`, `Link`, `Unlink`, `Rmdir` and `Rename` over the
   store's `Namespace`, with open state's `CheckChange` before each change.
6. **Attributes:** `GetAttr`, `SetAttr` with size changes, ACLs, xattrs, streams, `Access` and
   `StatFS`, each returning the `WCC` its reply needs.
7. **Open and close:** opens, deny modes, delete on close and the four moments of release.
8. **Data:** `Write`, `Read`, `Commit`, `Deallocate` and `Seek` in §5.1's steps, the write
   verifier, and quota reservations.
9. **Locks, grants, watches:** byte-range locks with waiters, `AckRecall`, `Watch`/`Unwatch`.
10. **Copy and clone:** synchronous, through the engine's `Copy` and `Clone`.
11. **Grace and release recovery:** grace per shard at `Start`, and the pending releases run
    when it ends.

## 4. The engine contract

`Content` is the one interface between stage 2 and stage 3, and the one thing both teams
build against before either is done. From RFC 17 §3.5 and RFC 8 §12.1:

| Method | The server calls it for |
|---|---|
| `Draw` | a version for any change, under the file's accept lock |
| `Write`, `Overlay`, `Commit` | the write path (§5.1) and stable commits (§5.8) |
| `Read`, `Allocation` | reads, SEEK and READ_PLUS (§5.2) |
| `Truncate`, `Deallocate` | size changes and hole punching (§5.5) |
| `Delete` | an orphaned file's content, at its last close or unlink (§5.3); kept apart from the journal's own `Release`, which evicts |
| `Copy`, `Clone` | server-side copy and clone (§5.9) |

Proposal: stage 2 writes the fake and a short check that any `Content` passes, such as
verified bytes back, a version above the last drawn, and nothing returned before its stage.
Stage 3's engine runs the same check, so the fake cannot drift from the engine. `SetShare`,
`StopShare`, `ForgetShare`, `Start` and `Close` are the node's to call, not the server's.

## 5. What stays out of stage 2

- The cluster: the gateway, `Serve`, routing, transport, `Peers` and the multi-node tier.
- Moving the adapters onto `vfs.Service` (stage 5), and RFC 20's shared pipeline.
- pNFS layouts (RFC 17 §10.2) and compound batching (§10.1).
- RFC 17 §9's slices that delegate to today's `metadata.Service` (D1).

## 6. Open questions

1. **The engine contract (§4):** who writes the fake, and when is `Content` frozen against
   E11 of #3152?
2. **D4:** when does RFC 18's resolver land, and is the store-backed fake enough until then?
3. **Gaps below the server** (RFC 17 §10.4): RFC 16 has no read view of the share indexes or
   of quota records other than `ControlPlane`, which the server must not call. Both are store
   work, and so ours, before pieces 3 and 8. RFC 16 on #3142 now adds `SharePolicy`, read
   through identity's `Records` view: a share's squash, anonymous principal and cache versions
   in one snapshot. That covers admission's policy read; the share list and path index, and
   quota records, are still without one.
4. **ID widths** (RFC 17 §10.3): the shard and `OpenID` widths in the stateid and the SMB
   `FileId` are one choice, which stage 1's stateid layout also depends on.

## 7. Size and what sets the pace

About fifteen PRs, roughly one per piece and two for the larger ones (5, 8, 9). Review sets the
pace, as for stage 1. What blocks what:

- the `Content` contract agreed before piece 8;
- stage 1 pieces 2, 3, 4, 5 and 6 before stage 2 pieces 4, 5, 7, 9 and 11;
- the store's share and quota read views (open question 3) before pieces 3 and 8.

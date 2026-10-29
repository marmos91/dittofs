# RFC 14 open state: implementation plan (stage 1 of #3119)

Stage 1 of wiring the RFC 16 store into the server (#3119, route B): the open-state service of
[RFC 14](../rfcs/rfc-14-open-state.md), the one table per file, held at the file's primary, of
who has the file open, who locked which bytes, what each client may cache, and what happens to
all of it when a client, a server or a node goes away. Its only caller is the filesystem service
of [RFC 17](../rfcs/rfc-17-vfs.md) (stage 2); adapters reach it only through that service.

**Status:** draft for review. Piece 1 has merged (#3256), and so has the store's first PR (#3329);
pieces 2–8 are in review (#3330, #3336, #3341, #3344, #3363, #3400; piece 8 as #3456, #3457, #3458,
#3417 and #3459), with the store's side in
#3335, #3362 and #3399. The RFC 14, 17 and 22 edits this plan names go in #3414.
The RFCs cited are `develop`'s, since #3142 merged; `feat/storage-v2` needs `develop` merged in
before these links resolve to them.

**Terms.** A bare §N is RFC 14's; this plan's own sections are "plan §N". Lnn is a row of RFC 14
§12. The stages are #3119's: 1 open state, 2 the filesystem service, 3 the engine, 4 the new store
replacing Badger, 5 the adapters. Route B: the new service is built beside today's server, and
nothing switches over until stage 5.

## 1. What already exists

- **The durable records** (step 9 of #3119, merged with #3120). `kvstore` stores what §8 makes
  durable: client records, durable opens and their locks, delete pending, ID allocation and the
  restart-notice number. Their shape trails RFC 16; piece 0 reshapes them.
- **Today's `pkg/metadata/lock`** (about 9.9k lines plus tests) serves today's server and stays on
  that path untouched. Its design is not RFC 14's:
  - one manager and one mutex per share, state keyed by handle strings, two parallel lock tables;
  - workers block on recalls (up to 10 s), which RFC 17 §3.2 forbids;
  - grace per share without instances; every lock persisted, where RFC 14 keeps a lock volatile
    unless its open is persistent;
  - opens, deny modes, stateids and lock waiters live in the adapters, not here.

  So the service is new, and ports only pure pieces and tests (D3).

## 2. Decisions to settle

**D1. Package `pkg/openstate`, one `*Table` behind seven views.**
- It imports `entity` and `kvstore`, never an adapter or the filesystem service; recalls and lock
  grants go up through its `Sink` (D5). Its errors are named `openstate: …`.
- `*Table` exports `New`, `Start`, `Stop` and the seven view accessors, `Clients()`, `Opens()`,
  `Locks()`, `Grants()`, `Watches()`, `Recovery()` and `Checks()`, and nothing else, so each area
  of the service sees only the calls it makes (§11.3).
- `Deps` are `Records`, the `*kvstore.Store`, reached through §11.1's `Clients()` and `Opens()`
  accessors; the `Clock`; and `Rand` for every random bit. `Metrics`
  and `Logger` join with the first metric and log line.
- RFC 24's `Budget` is not a dependency: its pool does not exist on `feat/storage-v2`, so
  `DelayResources` is answered at RFC 14's fixed caps alone (§5.5's per-client caps, §5.4's 4,096
  unsent recalls, §2.9's proposal caps):

  ```go
  // ponytail: grants, watches and recalls are refused at RFC 14's fixed caps
  // alone; take RFC 24's Budget as a Dep once its pool exists, so a node-wide
  // limit can refuse before a per-client cap is reached.
  ```
- Adapters never import it (RFC 20 §2.3). The service maps each `openstate` error once to RFC 17
  §4.3's neutral errors. For that mapping to be total, every `ErrDelay` the table returns is a
  `DelayError` naming its cause, one the service maps to one of RFC 17 §4.3's `Delay…` values; a
  bare `ErrDelay` would reach an adapter as a server fault (RFC 20 §7.3). Each cause is named after
  RFC 17's: `DelayFrozen` for a shard not yet served here, `DelayCommit` for a durable record still
  to be written again, and `DelayConflict` for an owner's own numbered request in flight. A test
  fails on any `ErrDelay` not wrapped in a `DelayError`.

**D2. Single node first, cluster rules behind seams.** Every rule RFC 14 marks "(cluster)" is out of
stage 1: one lease owner per client (§4.1), handover and batch moves (§10) with
`Recovery.ReleaseShard`, layouts (§2.6), a shard's state lost to a takeover rather than a restart.
On one node a shard stops only at `Stop` or `DropShare`, and both drop its state. Grace still runs
per shard after a restart (§4.4), so a cluster takes the same code with more shards owned
elsewhere. The table never calls another node (§3), so the seams are the service's: RFC 17's
`Peers` is nil on one node, and a client's `Lease.Node` and `Reach.Callback` are always this node.

Inside the table the only placement rule is that a call naming a shard not begun with
`ServeShard` is refused (§2.11, §4.4): `ErrDelay` for a call that mints state, `ErrBadState` for a
reference that names one. The refusal arrives with `ServeShard` in piece 6; pieces 1–5 serve every
shard under instance zero, which nothing can reach before stage 2.

**D3. Port the pure logic, not the manager.** From `pkg/metadata/lock`:
- range math (`RangesOverlap`, `SplitLock`, merge); today's merge wraps (`UnifiedLock.End` is
  `Offset + Length`), so the port saturates instead, as `pkg/openstate/ranges` does;
- the SMB conflict and mandatory-I/O rules (`IsLockConflicting`, `CheckIOConflict`: Samba's
  stacking and zero-byte rules, MS-FSA 2.1.4.10);
- the SMB lease state machine (valid states, `ComputeLeaseBreakTo`, progressive breaks, upgrades,
  late-ack classification, the epoch rule), which encodes smbtorture's expectations;
- the shape of `ValidateReconnect`.

Not ported: the manager, its blocking waits, its grace manager, lock persistence, and its deadlock
detector: §2.10 decides against deadlock detection, so smbtorture's open-brlock-deadlock, which
expects a refusal, is recorded as a known failure. Route B skips RFC 17 §9's slice 3 shim (its `decision:`): the filesystem
service is built against the seven views from its first piece, and `pkg/metadata/lock` stays only
under today's server until stage 5, then is deleted. Stage 4 carries none of its lock persistence
over.

From today's NFSv4 state (`internal/adapter/nfs/v4/state`, 12.6k lines plus 20.8k of tests, one
manager under one lock):
- **Ports, as rules rather than code:** the owner seqid checks (`ValidateSeqID`; the table exempts
  §2.8's four, `ErrStaleClient`, `ErrStaleState`, `ErrBadState` and `ErrBadSeqID`, and the adapter
  maps `failedSeqidReply`'s wire list onto them, RFC 21 §4.2), the stateid seqid check (made
  wrap-aware), the principal-hijack rule (§2.1), the CREATE_SESSION sequence check, the first
  LOCK's sequencing by the open-owner, and the delegation grant and conflict rules, which already
  recall and answer DELAY without waiting. Stateid minting ports with §2.1's layout.
- **Rewritten:** client IDs (today boot epoch and counter, lost at restart; §2.1 needs them durable
  through `kvstore`), leases (per-client timers; §4.1 runs on the declared clock), grace
  (server-wide; §4.4 is per shard), revocation (§5.3's revoked set), the stateid owner check
  (skipped for a v4.0 caller, making the stateid a bearer token; §2.2 checks the principal), and
  every lock path, which waits on `pkg/metadata/lock`'s recalls.
- **Stays in the adapter:** sessions, slot tables, the reply cache's bytes, connections and the back
  channel, special stateids, and XDR.
- **Tests worth porting as scenarios:** the SETCLIENTID and EXCHANGE_ID cases, the seqid and stateid
  validity tests, grace (rewritten per shard), delegation conflicts, and lease expiry, whose
  released stateid becomes bad rather than expired.

**D4. Concurrency: one entry per file.** The file map is 256 stripes by a `FileID`'s first byte
("stripes", to keep them apart from `ShardID`), each entry with its own mutex. Lock order, as the
code has it:

1. admission lock (the service's, RFC 17 §5.0);
2. client step `cstep`, never under a file's step;
3. file stripe, only to find, add or free an entry;
4. entry mutex;
5. `idx`, then `cmu`; `smu` under a step or nothing. The outbox is per call and unlocked.

Several files: their steps in `FileID` order, then fold admission, then accept locks, as RFC 17
§5.0 orders every call.
- No call waits on a recall or a client while holding a step: a conflict with a caching grant
  starts the recall and returns `ErrDelay` at once, except a callback lock request, which becomes a
  waiter answered `Waiting` (L44).
- Events go to the `Sink` after the entry mutex is released, so a break is queued ahead of the
  reply that triggered it (RFC 20 §6.2).

The pure parts that port existing tests are leaf packages tested alone: `ranges`, `conflict`,
`grantfsm` and `sequence`. `grace/` stays in the table, and there is no deadline heap: a 10 s sweep (`sweepEvery`'s `ponytail:`) fires each deadline
up to one interval late, so stage 2's fake-clock cases advance past one interval.

**D5. Open state reports to the service through RFC 14's `Sink`.**
- Delivery to adapters is the service's (RFC 17 §3.2); the table sees only the `Sink`.
- The `Sink` is an outbox with eight events, `RecallGrant`, `RecallAny`, `Notify`, `Revoked`,
  `LockWaitOver`, `Closed`, `ClientEnded` and `GraceEnded` (never dropped, like `Closed`). Each
  queues and returns, and a failed or late delivery counts as the client not answering.
- The node hands the service to `Start` as the `Sink`, with its per-file admission lock as `order`,
  since the table is built before the service. The table takes `order` itself wherever it may
  grant or revoke on a file the call does not name: in the timer's steps, and in `AckRecall`,
  `FreeState`, `Clients.Connect` (whose reboot or lapsed-lease release frees another client's
  waiters), `Expire`, `Revoke`, `Disconnect` and `Locks.ReleaseLockOwner`, which serve the waiters
  their releases free. The service must not hold it when making those calls:

  ```go
  func New(d Deps) (*Table, error)                                       // starts nothing; open state takes no settings
  func (t *Table) Start(ctx context.Context, sink Sink, order Order) error // starts the timers
  func (t *Table) Stop()
  ```
- The service's own `Start` then calls `Recovery.ServeShard(ctx, share, s, instance)` for each
  shard this node serves (§11.2), passing the shard's grace instance, which the node's start
  transaction raises (piece 0's follow-up). The table installs the shard's persistent opens (its
  clone-source opens from piece 8) before anything there is answered, enters grace and arms the
  shard's timers.

**D6. Time through the shared clock.** Leases (90 s), grace, recall deadlines (35 s for an SMB
break, one lease for NFS) and NFSv4 waiter polls are `Deps.Clock.Elapsed` readings, monotonic;
`Now` is read only for a time a record stores. As RFC 8 §2.1 has every component do, `openstate`
declares in its own package the subset of the node's `Clock` it reads (`Elapsed`, `After`, and
`Now` once a record stores a time); the node's one clock satisfies it as is. Tests drive a fake
clock, so a 90 s lease runs in microseconds.

**D7. Durable writes only through `kvstore`'s views.** The service never writes keys itself. A
volatile open is never written, beyond the client record's shard list on the client's first state
in a shard (L7, L60). An open is written only when it is persistent, is a clone's source, or keeps
an unlinked file alive (§8); the last is written by `Unlink`/`Rename` when they remove the file's
last name.

## 3. Pieces, in build order

Each piece lands as its PR into `feat/storage-v2`, with its tests (plan §4), each after the
one before it. A row may be split: its first piece lands the cases it can reach, and the piece that
completes it is named beside it, e.g. L27 (→6). The piece an arrow names lists the row again in its
own "Done when", so the row's completion is gated there. "Done when" means every listed row passes
and each of its cases is revert-checked.

**Piece 0: Store reshape** (RFC 16 §4.2, RFC 14 §11.1; 4 PRs, and a follow-up after piece 7)
Builds the store calls the table makes. Each open-state PR carries its store PR's commits (#3330
carries #3329's, and so on to #3400 and #3399), so piece 0 lands beside pieces 2–7, not before:

| RFC 16 / 14 needs | `feat/storage-v2` has | Where |
|---|---|---|
| `ClientRecord`; `Clients`: `ByName`, `Put(prev, next)`, `MarkShard`, `DropShards`; `NextID(kind)`; `Protocol` bytes as RFC 16's `CLO` table (NFSv3 0x01, NFSv4 0x02, NLM 0x03, SMB 0x04) | `ByOwner`, `Put`, `PutShard`, `Shards`; `NextOpenID`, `NextLockID`; NFSv4 1, SMB 2, NLM 3 | #3329 |
| `OpenRecord`, `LockRecord`, `DeleteRecord`; a stream's open keeps its base file | `Open`, `Lock`, `DeletePending` | #3335 |
| `Clients.ByShard` (L3: tell every named client to reclaim) | `Shards(id)` only | #3362 |
| a durable open's name and lease version; a persistent open's lock change in one transaction | none; `PutLock`, `DeleteLock`, a range each | #3399 |
| `RaiseIncarnations` in the node's start transaction; `GraceInstance` per shard, the instance `ServeShard` is given | none | the follow-up below |

`ClientRecord` is `ID`, `Protocol`, `Name`, `Boot`, `Owner`, `Protect`, `Recovery`, `Lease`,
`Reach`, `Ended` and `Adapter`, with each shard of `Shards` a `CL‖id‖sh‖shard` key holding its
`Done`. An `Owner` that maps to the anonymous principal is recorded as zero with `Protect` false
(§2.1, L25), and uid and gid 0 and 65534 are root's and anonymous's by rule (RFC 16 §2.2).

RFC 14 §11.1 matches (#3414):
- `Records` is reached through `Clients()` and `Opens()` (D1);
- `Opens.SetLocks(ctx, f, o, rec *OpenRecord, put, del)` replaces `PutLock` and `DeleteLock`: one
  transaction, only under a persistent open, never split, and `rec`, when set, writes the open's
  `LockSeq` and `Channel.Seq` with its locks (§8.1).

Where the built calls still differ from §11.1, one store PR after piece 7 closes the gap:
- `RaiseIncarnations(ctx)` and `GraceInstance(shard)` are built (RFC 16 §4.2): the node's start
  transaction raises every shard's instance, and the service passes it to `ServeShard`. Without
  them a restart serves a shard under the instance it had, and once grace ends an old stateid's
  instance bits match, so the miss answers bad state rather than stale and the client never
  reclaims. Nothing calls `ServeShard` before stage 2, so this lands before stage 2's open piece;
- `NSMState` and `RaiseNSMState(ctx, n)` become `NoticeState` and `RaiseNoticeState(ctx)`, which
  picks the number, so `recovery.go`'s retry loop goes;
- `Opens.Durable` returns index entries, so each install reads `Get`, `Locks` and `DeletePending`
  per record; `ByShard` returning records drops `Get`, and §11.1 gains the two reads it has none
  of, an open's locks and a file's delete pending;
- `Of` becomes `ByFile`, and `ClearDeletePending` becomes `SetDeletePending(nil)`;
- `Clients.AddSession` goes from the store: the table's `AddSession` is §11.1's compare-and-swap
  `Put(prev, next)`.

Done when: `kvstoretest` covers each call above.

**Piece 1: Skeleton and state references** (§2.11, §3, §11; #3256, merged)
Builds: the errors, the per-file table and its step, the file map's 256 stripes; `StateRef`
(`Type`, `Shard`, `Instance`, `Token`, `Seq`) and an index keyed by shard and token; a miss is bad state until
piece 6 adds stale; the stateid seqid check (`ErrOldState`); every random bit from `Deps.Rand`.
The wire layout is `pkg/vfs`'s (RFC 17 §3.4). `GrantID` and `WatchID` arrive with the grants and
watches that use them.
Done when: L26, L27 (→6), L49 (→6), L56 (→8), L57 (→8) pass.

**Piece 2: Clients and leases** (§2.1, §2.9, §4.1, §4.3; #3330)
Builds: `Start`, `Stop` and the `Sink`; `Clients.Propose/Connect/Record/AddSession/Renew/Expire/Destroy`;
the `ClientEnded` event. `Propose` holds a client pending confirmation at the node that answered
it, capped per node and per source address (§2.9), and `Connect` confirms it; an NLM host's
restart notice is `Connect` with a new `Boot` (§4.5). `CREATE_SESSION`'s sequence and reply are the
adapter's, kept in the record's opaque `Adapter` field and written through `AddSession` as a
compare-and-swap.
Done when: L25 (→7), L38 (→4), L41 (→8), L60 (→8), L64, L65 pass: `ErrStopped` before `Start` and
after `Stop`, and no `Sink` call under a lock.

**Piece 3: Opens and deny modes** (§2.2, §6, §7, §9; #3336)
Builds: `Opens.Open/Downgrade/Update/Close/Durable`, `Checks.CheckIO/CheckChange`, the `Closed`
event, which every later piece that closes an open itself also raises. Deny modes at open;
open-unlinked kept alive; delete on close; open children refuse an SMB rename or delete of their
directory.
Done when: L6, L7 (→7), L14 (→7), L21 (→7), L23, L34, L36, L66, L67 pass.

**Piece 4: Byte-range locks and waiters** (§2.3, §2.8, §2.10, §4.5; #3341)
Builds: `Locks.Lock/CancelLock/WaitAnswered/TestLock/Unlock/ReleaseLockOwner`, `Opens.ConfirmOpen`,
the `LockWaitOver` event. `Lock` takes every range of a request, each shared or exclusive, in one
step, all or none, and only a single range may wait (§11.3, RFC 17 §5.11); FIFO waiters with no
adapter timeout; NFSv4.0 owner sequences and their confirm; NLM.
Done when: L17 (→6), L22 (→6), L38, L39, L44 (→5), L58 (→6), L68 pass.

**Piece 5: Caching grants and recalls** (§2.4, §5; #3344)
Builds: `Grants.AckRecall(ctx, who, g, to)`, `Clients.Revoke/ChannelDown/Rebind`,
`Recovery.TestState/FreeState`, the `RecallGrant`, `RecallAny` and `Revoked` events. Grants offered
at open; a return is `AckRecall` to `GrantNone`; recalls with deadlines; revocation into the
revoked set; no grant for a lease period after a revoke, nor while the back channel is down
(`ChannelDown` until `Rebind`); the per-client grant budget.
Done when: L5, L5b, L16, L33, L40, L44, L45, L50 (→6), L51, L62, L63 pass.

**Piece 6: Grace and reclaim** (§4.2, §4.4, §8, §9.2; #3363)
Builds: `Recovery.ServeShard/ReclaimComplete`, `Opens.Renamed`. The refusal of a shard not begun
(D2); grace per shard after a restart, reclaims only, early end; every client a shard's records
name told to reclaim (`Clients.ByShard`); a rename keeps an open's name for its deferred unlink.
Done when: L2, L3, L3b, L17, L22, L24 (→RFC 24's `Budget`, after stage 1: its pool case), L27,
L29 (→#3264 piece 7: its restart case needs `RaiseIncarnations`), L29b, L47 (→7), L49, L50, L58 pass, L3b as a direct case of two reclaims rather than §12's model fuzzing.

**Piece 7: SMB durable and persistent opens** (§8.1, §9.2; #3400)
Builds: `Opens.Reconnect/Flushed`, `Clients.Disconnect`, the `GraceEnded` event. Timeouts,
disconnect and reconnect, `Flushed`'s loss; persistent opens installed from the store's per-shard
durable-open index (`S‖id‖do‖shard`) before the shard answers anything; grace's end closes every
durable record nobody reclaimed, then raises `GraceEnded`, and a new primary releases nothing
before.
Done when: L7, L14, L15 (→8), L21, L25, L30, L30b (→8), L43, L47, L48 (→8) pass.

**Piece 8: The rest** (§2.5, §4.3, §5.5, §8.1, §9.2; five PRs, split from #3417)
Builds: `Watches.Watch/Unwatch/Changed`, `Recovery.DropShare`, `Opens.CloneOpen/CloneClose`, the
`Notify` event; §5.5's other per-client caps (opens, ranges, owners, waiters); channel sequence,
create and lock replay and app instance. It lands as five PRs, each after the one before:
- #3456, the per-client caps, every `ErrDelay`'s cause, plan §4's fixture, fuzz targets and the
  heap-per-state benchmark. Done when: L54, L56, L57, L60 pass.
- #3457, watches. Done when: L55 passes.
- #3458, channel sequence, create and lock replay and app instance. Done when: L15 passes.
- #3417, `DropShare` and the clone holds. Done when: L30b, L48 pass.
- #3459, a restart's serve leasing each NFSv4 client it lists. Done when: L41's restart case
  passes, along with a silent client expiring one lease after it.

The store's clone already writes the source's hold in its
first phase (`clonePhase1`, `pkg/metadata/kvstore/clone.go`), so `CloneOpen` loads that record
through piece 7's install path and `CloneClose` drops it. #3414 edits §9.2, §11.1 and §11.3 to
match.
Piece 8 is done when its five PRs are. L52 is not the table's: the table has no
logoff call, since a `LOGOFF` is a `Close` of each of the session's opens (MS-SMB2 3.3.5.6; RFC 22
Appendix G says so once #3414 lands), so its case lands with the SMB adapter in stage 5.

Layouts (§2.6) and every "(cluster)" row wait for the cluster stages, L8 among them; piece 8 tests
`DropShare`'s part of it. Three rows are other layers': L1 and L20 are the service's, tested in
stage 2 (§13.1), and L9 is the engine's, in stage 3.

## 4. The conformance suite

`pkg/openstate`'s own tests, in the layers §13.1 lists: the table suite runs `*Table` over the
in-memory store, with a virtual clock the cases advance and a recording sink (D5) that can answer,
refuse or stay silent, to drive recalls to their deadlines. Pieces 1–7 build the table with
`rand.Reader`; piece 8 adds the rest of the fixture:
- a seeded `Deps.Rand`;
- `Check`, run after every case: no state held against a file with no table, no lock under a
  closed open, every durable open matching its record, every revoked stateid in its client's set;
- `export_test.go`'s rebuild of the indexes, which L54 compares against as a table-suite case.

L56 and L57 are "model layer" rows, but model fuzzing comes after stage 1, so both run as `-race`
loops, as L3b runs as a direct case: L56's 10⁴ races and L57's watchdog land with piece 8.

§13.1's other two layers:
- **Fault injection.** A fault store under `kvstore` fails chosen reads and write transactions:
  piece 7's `failing`, which every durable-open case that writes runs over. The recording sink can
  refuse or stay silent, which is what a failing `Sink` amounts to, since a `Sink` returns nothing
  and a lost delivery counts as the client not answering (D5). Piece 8 adds L60's case: a dropped
  shard-list commit is refused, and never answered as granted.
- **Fuzz.** A `testing.F` target for each client-chosen input that reaches arithmetic, with piece 8:
  `ranges` (offsets and lengths up to 2⁶⁴−1, checked against an interval oracle) and `sequence`
  (seqids across the 2³² wrap). The stateid and FileId codecs are `pkg/vfs`'s, fuzzed in stage 2's
  first piece.

Every case is revert-checked: break the rule it pins, watch it fail on its assertion. Tests that
cannot fail are not written (the team's rule), so a row the compiler or another case already
covers gets no case of its own.

Ported test scenarios, rewritten to RFC 14's semantics (a recall answers `ErrDelay`, never waits):
`pkg/metadata/lock`'s lock and split tests (owners), lease key and epoch tests, progressive-break
and late-ack tests (recall deadline), cross-protocol tests, and reconnect-matching tests.

Of §13.3's benchmarks, heap per state (RFC 24 M6) is built (#3456); the rest follow once the
service can drive them.

## 5. Open questions

1. **Persistent handles in stage 1:** kept: on one node they are the only open that survives a
   restart (§8.1), and piece 7 (#3400) already builds them, with `SetLocks` and the
   persistent-open install.
2. **Seven views, one implementation each:** kept as interfaces because §11.3 declares them.
   Since stage 2 runs the real table (#3264's D6) and no area fakes a view, concrete types would
   do; that is an RFC 14 edit, made with stage 2's first area if agreed.
3. **Behaviours today's code has and RFC 14 does not state:** courtesy clients (RFC 7530
   §9.6.3.1), an NFSv4.1 client taking new state before its RECLAIM_COMPLETE outside grace
   (RFC 8881 §18.51.3), and `RENEW`'s principal check. Should RFC 14 state them?
4. **Client records no serve lists** (#3459): an NFSv4 client holding no shard mark keeps its
   record after a restart until it renews. Leaked records, never state; closing it needs a store
   call listing a node's client records, which RFC 14 §11.1 lacks.

## 6. What sets the pace

What blocks what: `develop`'s RFCs on `feat/storage-v2`; each piece after the one before it,
carrying its store PR.

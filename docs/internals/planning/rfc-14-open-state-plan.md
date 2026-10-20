# RFC 14 open state: implementation plan (stage 1 of #3119)

Stage 1 of wiring the RFC 16 store into the server (#3119, route B): the open-state service of
[RFC 14](../rfcs/rfc-14-open-state.md), the one table per file, held at the file's primary, of
who has the file open, who locked which bytes, what each client may cache, and what happens to
all of it when a client, a server or a node goes away. Its only caller is the filesystem service
of [RFC 17](../rfcs/rfc-17-vfs.md) (stage 2); adapters reach it only through that service.

**Status:** draft for review. Pieces 1 and 2 are in review (#3256, #3258). The RFC 14 cited here
is the one on #3142 (`docs/rfc-20-adapters`), which has the open-state review folded in; `develop`
has the older text until #3142 merges.

## 1. What already exists

- **The durable records (step 9 of #3119, merged with #3120).** `kvstore` stores what RFC 14 §8 makes
  durable, with views the service reads and writes:
  - `Clients`: `Create`, `Reboot`, `Get`, `ByOwner`, `Put`, `Delete`, `CreateSession`, `PutShard`, `Shards`;
  - `Opens`: `Put`, `Get`, `Of`, `Delete` (reports an orphan), `Durable(share, shard)`, locks under persistent
    opens (`PutLock`, `DeleteLock`, `Locks`), delete pending, and ID allocation (`NextOpenID`, `NextLockID`);
  - `NSMState` and `RaiseNSMState`;
  - `Namespace.Unlink` and `Rename` already take a file's opens and write them durable when the last entry goes.
- **Today's `pkg/metadata/lock`** (about 9.9k lines plus tests) serves today's server and stays on
  that path untouched. Its design is not RFC 14's:
  - one manager and one mutex per share, state keyed by handle strings, two parallel lock tables;
  - workers block on recalls (up to 10 s), which RFC 17 §3.2 forbids;
  - grace per share without instances; every lock persisted, where RFC 14 keeps a lock volatile
    unless its open is persistent;
  - opens, deny modes, stateids and lock waiters live in the adapters, not here.

  So the service is new, and ports only pure pieces and tests (D3, §4, §7).

## 2. Decisions to settle

**D1. Package `pkg/openstate`, one `*Table` behind seven views.** RFC 14's errors are named
`openstate: …`. It imports `entity` and `kvstore`, never an adapter or the filesystem service;
recalls and lock grants go up through its `Sink` (D5). `*Table` exports `New`, `Start`, `Stop` and
seven view accessors, `Clients()`, `Opens()`, `Locks()`, `Grants()`, `Watches()`, `Recovery()` and
`Checks()`, and nothing else, so each area of the service fakes only the calls it makes (RFC 14
§11.3). Its `Deps` are the store's `Clients` and `Opens` (`Records`), RFC 24's `Budget`, the
`Clock`, `Rand` for every random bit, `Metrics` and `Logger`. Adapters never import it (RFC 20 §2.3): only the filesystem service
calls it, and the service maps each `openstate` error once to RFC 17 §4.3's neutral errors. So every
`ErrDelay` carries its cause (RFC 17 §4.3's `Delay…` values), or the mapping is not total and an
adapter answers it as a server fault (RFC 20 §7.3).

**D2. Single node first, cluster rules behind seams.** Every rule RFC 14 marks "(cluster)" is out of
stage 1: one lease owner per client (§4.1), handover and batch moves (§10), layouts (§2.6), a
shard's state lost to a takeover rather than a restart. On one node every file's primary is the
local node, and grace still runs per shard after a restart (§4.4), so a cluster takes the same
code with more shards owned elsewhere. The seams: a `Primary` check that is always true on one
node, and a lease owner that is always local.

**D3. Port the pure logic, not the manager.** From `pkg/metadata/lock`:
- range math (`RangesOverlap`, `SplitLock`, merge), which saturates rather than overflows;
- the SMB conflict and mandatory-I/O rules (`IsLockConflicting`, `CheckIOConflict`: Samba's
  stacking and zero-byte rules, MS-FSA 2.1.4.10);
- the SMB lease state machine (valid states, `ComputeLeaseBreakTo`, progressive breaks, upgrades,
  late-ack classification, the epoch rule), which encodes smbtorture's expectations;
- the shape of `ValidateReconnect`.

Not ported: the manager, its blocking waits, its grace manager, lock persistence, and its deadlock
detector (RFC 14 has none; SMB's open-brlock-deadlock refusal can port it later if wanted).

Today's manager is replaced, not moved. Until stage 1 lands, RFC 17 §9's slice 3 keeps
`pkg/metadata/lock` where it is, behind the service's own small lock interface; then
`*openstate.Table` takes its place behind that interface and `pkg/metadata/lock` is deleted.
Nothing imports `pkg/openstate` before then, and stage 4 carries none of today's lock persistence over:
RFC 14 keeps a lock volatile unless its open is persistent, and rebuilds the rest by reclaim.

**D4. Concurrency: one entry per file, decided, persisted, applied.** The file map is split 256
ways by `FileID`; each entry has its own mutex, the open-state step (RFC 14 §3). Each operation is
a pure `decide(state, request) → (mutations, events, result)`, which the entry runs, persists
through `Records` and applies, and which the model fuzzer calls directly (RFC 14 §11.1). A call
touching several files takes their steps in `FileID` order, then fold admission, then accept
locks, as RFC 17 §5.0 orders every call. No call waits on a recall or a client while holding a
step: a conflict with a caching grant starts the recall and returns `ErrDelay` at once. Events
are queued in the step and handed to the `Sink` after the entry mutex is released, so the break
is queued ahead of the reply that triggered it (RFC 20 §6.2). The pure parts are leaf packages
tested alone: `ranges`, `conflict`, `grantfsm`, `sequence`, `deadline` and `grace`.

**D5. Open state reports to the service through RFC 14's `Sink`.** Adapters implement RFC 17
§3.2's `Callbacks` and may not import `openstate` (RFC 20 §2.3), so the service keeps the
per-client registry: an adapter registers a client's `Callbacks` with RFC 17's `Connect`, and calls
`Connect` again to replace them. The `Sink` is an outbox with six events, `RecallGrant`,
`Notify`, `Revoked`, `LockWaitOver`, `Closed` and `ClientEnded`; each queues and returns, and a
failed or late delivery counts as the client not answering. The node hands the service to
`Start` as the `Sink`, with its per-file admission lock as `order` for the one timer step that adds
state, since the table is built before the service:

```go
func New(d Deps) (*Table, error)                                       // starts nothing; open state takes no settings
func (t *Table) Start(ctx context.Context, sink Sink, order Order) error // starts the deadline goroutine
func (t *Table) Stop()
```

The service's own `Start` then calls `Recovery.ServeShard` for each shard, which installs its
durable opens and enters grace. RFC 14's `Rebind(ctx, who, node)` is a different call: it
records a client's new back channel and clears `Reach.Down`.

**D6. Time through the shared clock.** Leases (90 s), grace, recall deadlines (35 s for an SMB
break, one lease for NFS) and NFSv4 waiter polls are `Deps.Clock.Elapsed` readings, monotonic;
`Now` is read only for a time a record stores, and a handover ships remaining durations. The clock
is RFC 8's; until `pkg/clock` exists, `openstate` declares the subset it reads and switches to the
shared type when it lands. Tests drive a fake clock, so a 90 s lease runs in microseconds.

**D7. Durable writes only through `kvstore`'s views.** The service never writes keys itself. A
volatile open is never written (RFC 14 §8); `Unlink`/`Rename` already make an open durable when
it is the file's last holder.

## 3. Pieces, in build order

Each piece lands as one or two PRs into `feat/storage-v2`, with its tests (§4). Rows are RFC 14
§12's L-numbers.

| # | Piece | RFC 14 | Main calls (view) |
|---|---|---|---|
| 1 | Skeleton and state references | §2.11, §3, §11 | the table, its entries and references; `Start`, `Stop` |
| 2 | Clients and leases | §2.1, §2.9, §4.1, §4.3 | `Clients`: `Propose`, `Connect`, `Record`, `AddSession`, `Renew`, `Expire`, `Destroy` |
| 3 | Opens and deny modes | §2.2, §6, §7, §9 | `Opens`: `Open`, `Downgrade`, `ConfirmOpen`, `Update`, `Close`, `Flushed`, `Durable`; `Checks`: `CheckIO`, `CheckChange` |
| 4 | Byte-range locks and waiters | §2.3, §2.8, §2.10, §4.5 | `Locks`: `Lock`, `CancelLock`, `WaitAnswered`, `TestLock`, `Unlock`, `ReleaseLockOwner` |
| 5 | Caching grants and recalls | §2.4, §5 | `Grants`: `AckRecall`; `Clients`: `Revoke`, `Rebind`; `Recovery`: `TestState`, `FreeState`; the `RecallGrant`, `Revoked` and `ClientEnded` events |
| 6 | Grace and reclaim | §4.2, §4.4, §8, §9.2 | `Recovery`: `ServeShard`, `ReleaseShard`, `ReclaimComplete`; `Opens`: `CloneOpen`, `CloneClose` |
| 7 | SMB durable and persistent opens | §8.1 | `Opens`: `Reconnect`; `Clients`: `Disconnect` |
| 8 | The rest | §2.5, §2.7, §4.3 | `Watches`: `Watch`, `Unwatch`, `Changed`; `Recovery`: `DropShare`; the `Notify` and `Closed` events |

What each piece also carries, and its §12 rows:

1. **Skeleton and state references:** the errors, the per-file table and its step; `StateRef`
   (`Type`, `Shard`, `Instance`, `Token`, `Seq`) minted with its shard and grace instance, and
   `GrantID` and `WatchID` minted with their shard, so `AckRecall` and `Unwatch` route with no
   handle; the per-shard index, how a miss is classified, and the stateid seqid check
   (`ErrOldState`); every random bit from `Deps.Rand`, and the file map split 256 ways. The wire
   layout is `pkg/vfs`'s (RFC 17 §3.4). *Rows:* L1, L13, L26, L27, L49.
2. **Clients and leases:** `Propose` holds a client pending confirmation at the node that answered
   it, capped per node and per source address (RFC 14 §2.9), and `Connect` confirms it; an NLM
   host's restart notice is `Connect` with a new `Boot` (§4.5). `CREATE_SESSION`'s sequence and
   reply are the adapter's, kept in the record's opaque `Adapter` field and written through
   `AddSession` as a compare-and-swap. It needs the store's client record reshaped first (§8).
   *Rows:* L25, L38, L41.
3. **Opens and deny modes:** deny modes at open; open-unlinked kept alive; delete on close; open
   children refuse an SMB rename or delete of their directory; `CheckIO` and `CheckChange`, which
   have something to check once opens exist. *Rows:* L6, L7, L14, L21, L23, L31, L34, L36.
4. **Byte-range locks and waiters:** `Lock` takes every range of a request, each shared or
   exclusive, in one step, all or none, and only a single range may wait (RFC 14 §11.3, RFC 17
   §5.11); FIFO waiters with no adapter timeout; NFSv4.0 owner sequences; NLM. *Rows:* L17, L22,
   L39, L44.
5. **Caching grants and recalls:** grants offered at open; a return is `AckRecall(…, GrantNone)`;
   recalls with deadlines; revocation into the revoked set; no grant to a client for a lease
   period after one is revoked; the per-client budget. *Rows:* L5, L16, L33, L40, L42, L45, L50,
   L51.
6. **Grace and reclaim:** grace per shard after a restart, reclaims only, early end; durable opens
   rebuilt from `Opens.Durable`; a new primary releases nothing before grace ends. *Rows:* L2, L3,
   L8, L24, L29, L47, L48.
7. **SMB durable and persistent opens:** timeouts, create and lock replay, app instance, channel
   sequence, logoff. *Rows:* L15, L30, L43, L52.
8. **The rest:** watches, async copies, `DropShare`. *Rows:* L10, L54.

Layouts (§2.6) and every "(cluster)" row wait for the cluster stages.

## 4. The conformance suite

`pkg/openstate`'s own tests, in the layers RFC 14 §13.1 lists: the table suite runs `*Table` over the
in-memory store, with:
- a virtual clock the cases advance;
- a recording sink (D5) that can answer, refuse or stay silent, to drive recalls to their deadlines;
- `Check`, run after every case: no state held against a file with no table, no lock under a
  closed open, every durable open matching its record, every revoked stateid in its client's set.

Each case names its §12 row. Every case is revert-checked: break the rule it pins, watch it fail
on its assertion. Tests that cannot fail are not written (the team's rule), so a row the compiler or
another case already covers gets no case of its own.

Ported test scenarios, rewritten to RFC 14's semantics (a recall answers `ErrDelay`, never waits):
`pkg/metadata/lock`'s lock and split tests (owners), lease key and epoch tests, progressive-break
and late-ack tests (recall deadline), cross-protocol tests, and reconnect-matching tests.

The §13.3 benchmarks come with the pieces they measure.

## 5. What stays out of stage 1

- Wiring into today's server: route B builds the service for stage 2 to call, and today's
  adapters keep `pkg/metadata/lock` until stage 5.
- The cluster rules (D2) and layouts.
- A deadlock detector, which RFC 14 does not ask for.

## 6. Open questions

1. **D2:** is single node first, with the cluster rules behind seams, the right cut?
2. **SMB deadlock refusal:** smbtorture's open-brlock-deadlock expects one. Port `deadlock.go`, or
   record it as a known failure?

## 7. What ports from today's NFSv4 state

`internal/adapter/nfs/v4/state` is 12.6k lines plus 20.8k of tests, one manager under one lock.
- **Ports, as rules rather than code:** the owner seqid checks and the errors exempt from advancing
  them (`ValidateSeqID`, `failedSeqidReply`: exactly §2.8's list), the stateid seqid check (made
  wrap-aware), the principal-hijack rule (§2.1), the CREATE_SESSION sequence check, the first LOCK's
  sequencing by the open-owner, and the delegation grant and conflict rules, which already recall
  and answer DELAY without waiting. Stateid minting ports with the layout changed to §2.1's.
- **Rewritten:** client IDs (today boot epoch and counter, lost at restart; §2.1 needs them durable
  through `kvstore`), leases (per-client timers; §4.1 runs on the declared clock), grace (server-wide;
  §4.4 is per shard), revocation (§5.3's revoked set), the stateid owner check (skipped for a v4.0
  caller, making the stateid a bearer token; §2.2 checks the principal), and every lock path, which
  waits on `pkg/metadata/lock`'s recalls.
- **Stays in the adapter:** sessions, slot tables, the reply cache's bytes, connections and the back
  channel, special stateids, and XDR.
- **Tests worth porting as scenarios:** the SETCLIENTID and EXCHANGE_ID cases, the seqid and stateid
  validity tests, grace (rewritten per shard), delegation conflicts, and lease expiry, whose released
  stateid becomes bad rather than expired.

Behaviours today's code has and RFC 14 does not state, to raise against RFC 14: courtesy clients
(RFC 7530 §9.6.3.1), an NFSv4.1 client taking new state before its RECLAIM_COMPLETE outside grace
(RFC 8881 §18.51.3), `RENEW`'s principal check, and a stateid naming an unknown shard. RFC 14 now
covers the other three: the revoked-state flags (`ClientStatus.AdminRevoked`,
`RecallableRevoked`), the callback path down (`PathDown`), and the grant fence after a revoke.

## 8. Size and what sets the pace

Writing a piece is not what takes the time; review is. Stage 1 lands as about twelve PRs, each read
by its author and reviewed in two or three rounds, so it takes about as long as twelve review cycles.
Rough size per piece, code and tests together, so reviewers can plan:

| # | Piece | PRs | Lines, roughly |
|---|---|---|---|
| 1 | Skeleton and stateids | 1 | 1,500 |
| 2 | Clients and leases | 1–2 | 2,000 |
| 3 | Opens and deny modes | 2 | 2,500 |
| 4 | Byte-range locks and waiters | 2 | 3,000 |
| 5 | Caching grants and recalls | 2 | 2,500 |
| 6 | Grace and reclaim | 1–2 | 2,000 |
| 7 | SMB durable and persistent opens | 1 | 2,000 |
| 8 | The rest | 1 | 1,500 |

What blocks what: #3142 merged, since this plan follows its RFC 14; then each piece after the one
before it, pieces 3 to 8 needing piece 1's references. Before piece 2's rework, the store's client
record becomes RFC 14's `ClientRecord` (`Name`, `Boot`, `Owner`, `Protect`, `Recovery`, `Lease`,
`Reach`, `Shards`, `Ended`, `Adapter`), with RFC 16's `NextID`, Done marks and install-before-serve
for the open records.

For teams planning against stage 2, the useful answer is these merges and decisions, not a date.

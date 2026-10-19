# RFC 14 open state: implementation plan (stage 1 of #3119)

Stage 1 of wiring the RFC 16 store into the server (#3119, route B): the open-state service of
[RFC 14](../rfcs/rfc-14-open-state.md), the one table per file, held at the file's primary, of
who has the file open, who locked which bytes, what each client may cache, and what happens to
all of it when a client, a server or a node goes away. Its only caller is the filesystem service
of [RFC 17](../rfcs/rfc-17-vfs.md) (stage 2); adapters reach it only through that service.

**Status:** draft for review. Nothing here is built yet.

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
  - grace per share without instances; locks persisted, where RFC 14 keeps them volatile;
  - opens, deny modes, stateids and lock waiters live in the adapters, not here.

  So the service is new, and ports only pure pieces and tests (D3, §4, §7).

## 2. Decisions to settle

**D1. Package `pkg/openstate`.** RFC 14's errors are named `openstate: …`. It imports `entity`
and `kvstore`, never an adapter or the filesystem service; recalls and lock grants go up through
an interface of its own (D5). Adapters never import it (RFC 20 §2.3): only the filesystem service
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

**D4. Concurrency: one table per file, one step per file.** A sharded map from `FileID` to the
file's table; each table has its own mutex, the "open-state step" of RFC 14 §11. A call touching
several files takes their steps in `FileID` order, then fold admission, then accept locks, as
RFC 17 §5.1 orders every call. No call waits on a recall or a client while holding a step: a
conflict with a caching grant starts the recall and returns `ErrDelay` at once (RFC 17 §3.2,
"no worker waits on a recall"). The recall is handed up after the step is released and before the
call returns `ErrDelay`, so the break is queued ahead of the reply that triggered it (RFC 20 §6.2).

**D5. Open state reports to the service, not to clients.** RFC 17 §3.2's `Callbacks`
(`RecallGrant`, `Notify`, `Revoked`, `LockWaitOver`) are implemented by adapters, which may not
import `openstate` (RFC 20 §2.3), so the interface lives in `pkg/vfs`, and the service keeps the
per-client registry: `Rebind(c, cb)` is already an RFC 17 call, and RFC 14's `Connect` takes no
callbacks. `openstate` declares a small sink of its own, keyed by `ClientID`, with the same four
events; the service implements it and routes each event to the client's latest `Callbacks`. The
sink never blocks: it queues and returns, failing when the client has no live channel (RFC 20
§3.3), and a failure or a missed deadline counts as the client not answering.

**D6. Time through a declared clock.** Leases (90 s), grace, recall deadlines (35 s for an SMB
break, one lease for NFS) and NFSv4 waiter polls all run on a clock the service declares with
standard-library types (`Now`, `Elapsed`, `After`), as RFC 8 and #3189 settled for every
component. Tests drive a virtual clock, so a 90 s lease runs in microseconds. The system clock may
land here as its first caller.

**D7. Durable writes only through `kvstore`'s views.** The service never writes keys itself. A
volatile open is never written (RFC 14 §8); `Unlink`/`Rename` already make an open durable when
it is the file's last holder.

## 3. Pieces, in build order

Each piece lands as one or two PRs into `feat/storage-v2`, with its rows of the conformance suite
(§4). Rows are named as RFC 14 §13.1 names them.

| # | Piece | RFC 14 | Main calls |
|---|---|---|---|
| 1 | Skeleton and stateids | §2.1, §3, §11 | `CheckIO`, `CheckChange` |
| 2 | Clients and leases | §2.1, §2.9, §4.1, §4.3 | `Connect`, `CreateSession`, `Renew`, `Expire`, `Revoke`, `FreeRevoked`, `Destroy`, `Rebind` |
| 3 | Opens and deny modes | §2.2, §6, §7, §9 | `Open`, `Close`, `SetDisposition`, `SuspendTimes`, `Flushed` |
| 4 | Byte-range locks and waiters | §2.3, §2.8, §2.10, §4.5 | `Lock`, `Unlock`, `TestLock`, `CancelLock`, `HostRestarted` |
| 5 | Caching grants and recalls | §2.4, §5 | `Return`, `AckBreak`, `RecallGrant` |
| 6 | Grace and reclaim | §4.2, §4.4, §8, §9.2 | `ReclaimComplete` |
| 7 | SMB durable and persistent opens | §8.1 | `Reconnect`, `Disconnect` |
| 8 | The rest | §2.5, §2.7, §11 | `Watch`, `Unwatch`, `Notify`, `DropShare` |

What each piece also carries, and its §13.1 rows:

1. **Skeleton and stateids:** the `OpenState` interface, errors, the per-file table and step, the
   clock, and `CheckIO`/`CheckChange` with nothing to conflict yet; the stateid layout and its
   per-shard index, with how a miss is classified. *Rows:* one table, client check.
2. **Clients and leases:** NFSv4.0 unconfirmed clients (open question 4). *Rows:* lease expiry,
   server owner, client check.
3. **Opens and deny modes:** deny modes at open; open-unlinked kept alive; delete on close; open
   children refuse an SMB rename or delete of their directory. *Rows:* no downgrade, anonymous
   I/O, cross-protocol, open-unlinked (and across protocols), lazy, delete pending, remove against
   deny-delete.
4. **Byte-range locks and waiters:** `Lock` and `Unlock` take a list of ranges, all or nothing,
   each shared or exclusive, since an SMB LOCK of several elements is one call (RFC 20 §4.3); FIFO
   waiters with no adapter timeout; NFSv4.0 owner sequences; NLM. *Rows:* owners, NLM restart, the
   waiter rows.
5. **Caching grants and recalls:** grants offered at open; recalls with deadlines; revocation into
   the revoked set; no more grants to a client after one revoke; the per-client budget. *Rows:*
   recall deadline, flush on recall, no blocked worker, lease key, no write grant on a setuid file.
6. **Grace and reclaim:** grace per shard after a restart, reclaims only, early end; durable opens
   rebuilt from `Opens.Durable`; a new primary releases nothing before grace ends. *Rows:* grace,
   reclaim needs a record, reclaim grants no more, grace per shard, reclaim scoped, I/O in grace,
   early end, new primary, durable open prefix.
7. **SMB durable and persistent opens:** timeouts, create and lock replay, app instance, channel
   sequence. *Rows:* reconnect matching, timeout, create replay, lock replay, app instance,
   persistent.
8. **The rest:** watches, async copies, `DropShare`. *Rows:* lost copy, the watch and drop-share
   rows.

Layouts (§2.6) and every "(cluster)" row wait for the cluster stages.

## 4. The conformance suite

`pkg/openstate/openstatetest`, built like `kvstoretest`: `Run(t, open)` runs every case against a
service opened by the caller, over a fresh store, with:
- a virtual clock the cases advance;
- a recording sink (D5) that can answer, refuse or stay silent, to drive recalls to their deadlines;
- `Check`, run after every case: no state held against a file with no table, no lock under a
  closed open, every durable open matching its record, every revoked stateid in its client's set.

Each case names its §13.1 row. Every case is revert-checked: break the rule it pins, watch it fail
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
2. **Multi-range `Lock`:** RFC 14 §11 still takes one range. This plan follows the `LockRange`
   shape of the RFC 14 changes in review (a range plus its own `Exclusive`), since a single
   `exclusive` flag cannot carry an SMB LOCK that mixes shared and exclusive elements.
3. **SMB deadlock refusal:** smbtorture's open-brlock-deadlock expects one. Port `deadlock.go`, or
   record it as a known failure?
4. **Unconfirmed NFSv4 clients (§2.9):** SETCLIENTID and EXCHANGE_ID must return an existing
   client's ID, or refuse with `ErrClientInUse`, before any `Connect`, but §11 has no call that finds
   a client by owner. Either §11 gains one, or the adapter's handler reads `Clients.ByOwner` through
   the service. Settle before piece 2; in `openstate` it adds two or three days.

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
(RFC 8881 §18.51.3), `RENEW`'s principal check, `ClientStatus` flags for revoked state and a
callback path down, a stateid naming an unknown shard, and no re-delegation for a while after a
recall.

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

What blocks what:
- open question 4 (finding an unconfirmed client) before piece 2;
- piece 1's stateid layout before piece 2, whose `Revoke` needs it;
- the RFC 14 `LockRange` change merged before piece 4 freezes `Lock`;
- RFC 20 merged before piece 5 fixes the sink, which mirrors its `Callbacks`.

For teams planning against stage 2, the useful answer is these merges and decisions, not a date.

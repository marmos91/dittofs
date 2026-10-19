# RFC 14 open state: implementation plan (stage 1 of #3119)

Stage 1 of wiring the RFC 16 store into the server (#3119, route B): the open-state service of
[RFC 14](../rfcs/rfc-14-open-state.md), the one table per file, held at the file's primary, of
who has the file open, who locked which bytes, what each client may cache, and what happens to
all of it when a client, a server or a node goes away. Its only caller is the filesystem service
of [RFC 17](../rfcs/rfc-17-vfs.md) (stage 2); adapters reach it only through that service.

**Status:** draft for review. Nothing here is built yet.

## 1. What already exists

- **The durable records (stage 9.1a, merged with #3120).** `kvstore` stores what RFC 14 §8 makes
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

  So the service is new, and ports only pure pieces and tests (§5).

## 2. Decisions to settle

**D1. Package `pkg/openstate`.** RFC 14's errors are named `openstate: …`. It imports `entity`
and `kvstore`, never an adapter or the filesystem service; callbacks go up through an interface
(D5).

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
"no worker waits on a recall").

**D5. Callbacks as RFC 17 §3.2 declares them**, `RecallGrant`, `Notify`, `Revoked`,
`LockWaitOver`, registered per client at `Connect`. Stage 1 declares the interface in
`openstate`; stage 2 decides whether RFC 17's package re-exports it. A callback failure or
timeout counts as the client not answering.

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

| # | Piece | RFC 14 | Main calls | §13.1 rows |
|---|---|---|---|---|
| 1 | **Skeleton** | §3, §11 | the `OpenState` interface, errors, the per-file table and step, the clock, `CheckIO` and `CheckChange` with nothing to conflict yet | one table, client check |
| 2 | **Clients and leases** | §2.1, §2.9, §4.1, §4.3 | `Connect`, `CreateSession`, `Renew`, `Expire`, `Revoke`, `FreeRevoked`, `Destroy`, `Rebind`; NFSv4.0 unconfirmed clients | lease expiry, server owner, client check |
| 3 | **Opens and deny modes** | §2.2, §6, §7, §9 | `Open`, `Close`, `SetDisposition`, `SuspendTimes`, `Flushed`; deny modes at open; open-unlinked kept alive; delete on close; open children refuse an SMB rename or delete of their directory | no downgrade, anonymous I/O, cross-protocol, open-unlinked (and across protocols), lazy, delete pending, remove against deny-delete |
| 4 | **Byte-range locks and waiters** | §2.3, §2.8, §2.10, §4.5 | `Lock`, `Unlock`, `TestLock`, `CancelLock`; FIFO waiters with no adapter timeout; NFSv4.0 owner sequences; NLM and `HostRestarted` | owners, NLM restart, the waiter rows |
| 5 | **Caching grants and recalls** | §2.4, §5 | grants offered at open, `Return`, `AckBreak`; recalls through `RecallGrant` with deadlines; revocation into the revoked set; no more grants to a client after one revoke; the per-client budget | recall deadline, flush on recall, no blocked worker, lease key, no write grant on a setuid file |
| 6 | **Grace and reclaim** | §4.2, §4.4, §8, §9.2 | grace per shard after a restart, reclaims only, `ReclaimComplete`, early end; durable opens rebuilt from `Opens.Durable`; a new primary releases nothing before grace ends | grace, reclaim needs a record, reclaim grants no more, grace per shard, reclaim scoped, I/O in grace, early end, new primary, durable open prefix |
| 7 | **SMB durable and persistent opens** | §8.1 | `Reconnect`, `Disconnect`, timeouts, create and lock replay, app instance, channel sequence | reconnect matching, timeout, create replay, lock replay, app instance, persistent |
| 8 | **The rest** | §2.5, §2.7, §11 | watches (`Watch`, `Unwatch`, `Notify`), async copies, `DropShare` | lost copy, the watch and drop-share rows |

Layouts (§2.6) and every "(cluster)" row wait for the cluster stages.

## 4. The conformance suite

`pkg/openstate/openstatetest`, built like `kvstoretest`: `Run(t, open)` runs every case against a
service opened by the caller, over a fresh store, with:
- a virtual clock the cases advance;
- a recording `Callbacks` that can answer, refuse or stay silent, to drive recalls to their deadlines;
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
2. **D5:** where does the `Callbacks` interface live once stage 2 exists: in `openstate`, with RFC 17's
   package using it, or in RFC 17's package with `openstate` importing it?
3. **SMB deadlock refusal:** smbtorture's open-brlock-deadlock expects one. Port `deadlock.go`, or
   record it as a known failure?
4. **NFSv4 state today:** `internal/adapter/nfs/v4/state` (about 33k lines with tests) holds today's
   stateids, client and session handling. It needs its own survey for what `openstate` can port
   (stateid layout, seqid handling) before piece 2.

## 7. Size

The largest stage so far, comparable to the store: pieces 1–3 about a week each, 4–6 a week or
more each, 7–8 a week together. About six to eight weeks, landing as twelve or so PRs.

# NFSv4 state machine — findings

Audit of `internal/adapter/nfs/v4/state/` plus the OPEN/LOCK/state-touching handler
code in `internal/adapter/nfs/v4/handlers/` and `internal/adapter/nfs/v4/v41/handlers/`.
Cross-checked against RFC 7530 §§8–9,16, RFC 8881 §2.10/§8/§18, and Linux
`fs/nfsd/nfs4state.c`. READ-ONLY audit; no source modified.

Scope note: a per-share crash-safety/persistence finding (audit item 8) is HIGH but
largely *known and documented* — recorded below for completeness.

---

## HIGH

### [HIGH] Share reservations (share_deny) are never enforced across open-owners — `internal/adapter/nfs/v4/handlers/open.go:331` / `internal/adapter/nfs/v4/state/manager.go:944`

OPEN accumulates `ShareAccess`/`ShareDeny` per open-state (`manager.go:1035-1037`,
`1057-1058`) and `NFS4ERR_SHARE_DENIED` is defined (`types/constants.go:192`), but
**nothing ever computes a share-reservation conflict against opens held by *other*
open-owners/clients, and `NFS4ERR_SHARE_DENIED` is never returned anywhere** (grep
confirms zero call sites). `OpenFile` only OR-merges bits for the *same* owner on the
*same* file; it does not scan `openStateByOther` for files opened by a different owner
with a conflicting deny mode.

Why it matters: this is a core NFSv4 correctness/data-integrity guarantee (RFC 7530
§9.7, §16.16.5). A client that does `OPEN(... share_deny=WRITE)` expects the server to
reject a second client's `OPEN(... share_access=WRITE)` with `NFS4ERR_SHARE_DENIED`.
DittoFS silently grants both, so DENY_* share reservations are a no-op — Windows-style
"open with deny" semantics (relied on by SMB/NFS cross-protocol and by some apps) are
broken, and concurrent writers can corrupt a file the owner believed was exclusively
reserved. Compare Linux nfsd `nfs4_share_conflict`/`test_share`.

Fix: in `OpenFile` (under `sm.mu`), before creating/merging state, iterate the file's
existing open-states (ideally via an `openStatesByFile` index — see MED memory finding)
and reject when `requestedAccess & existingDeny != 0` or `requestedDeny &
existingAccess != 0` for opens owned by a *different* open-owner. Return
`NFS4ERR_SHARE_DENIED`. Also validate `share_access != 0` and `share_access ⊆ {READ,
WRITE,BOTH}` (currently unvalidated at decode).

### [HIGH] READ-bypass special stateid (all-ones) is accepted on WRITE/SETATTR/LOCK — `internal/adapter/nfs/v4/state/stateid.go:127` + `internal/adapter/nfs/v4/handlers/write.go:144`

`ValidateStateid` short-circuits **any** special stateid to `(nil, nil)`
(`stateid.go:127-129`), and `IsSpecialStateid` (`types/types.go:193`) treats the
all-ones stateid (seqid=0xFFFFFFFF, other=all-ones) as special. In WRITE, a `nil`
openState bypasses the `OPEN4_SHARE_ACCESS_WRITE` check (`write.go:144-156`).

Per RFC 7530 §9.1.4.3 the all-ones stateid is the **READ-bypass** stateid — valid only
for READ. Using it for WRITE/SETATTR(size)/LOCK MUST return `NFS4ERR_BAD_STATEID`.
DittoFS lets a client write with the all-ones stateid, bypassing both share-mode
enforcement and byte-range locks. This is a correctness + integrity hole (and a way to
defeat the read-only-open OPENMODE guard).

Fix: distinguish the two special stateids at validation time. Allow all-ones only on
READ paths; for WRITE/SETATTR/LOCK/LOCKU/CLOSE/OPEN_DOWNGRADE return
`NFS4ERR_BAD_STATEID`. The anonymous all-zeros stateid is permitted on READ/WRITE per
RFC but still bypasses locks — that is acceptable, but the all-ones case is not.

### [HIGH] Per-open-owner replay cache is shared across op types → wrong/stale replay result — `internal/adapter/nfs/v4/state/manager.go:1074` / `open.go:486`

The open-owner replay cache (`OpenOwner.LastResult`) is a single slot populated **only
by OPEN** (`CacheOpenResult`, `open.go:486`). But every owner-seqid op — OPEN, CLOSE,
OPEN_DOWNGRADE, OPEN_CONFIRM — advances `owner.LastSeqID` (`manager.go:1074, 1265,
1360, 1161`). RFC 7530 §9.1.7 requires the *last operation's* result be replayed for a
retransmit at `LastSeqID`.

Consequences:
1. After a CLOSE/DOWNGRADE/CONFIRM advances `LastSeqID`, a retransmit of that op hits
   `SeqIDReplay` and returns the **stale cached OPEN data** (or `ErrBadSeqid` if no OPEN
   ran yet — `manager.go:996`, `CloseFile` returns a zeroed stateid which happens to be
   correct, but DOWNGRADE/CONFIRM replay at `manager.go:1140-1143`/`1320-1322` return
   the *current* stateid rather than the originally-sent bytes).
2. Cross-op contamination: a replayed OPEN that follows an intervening CLOSE at the same
   seqid returns the CLOSE-era state.

Why it matters: replay caches are the v4.0 exactly-once mechanism; returning the wrong
result on a legitimate TCP retransmit corrupts the client's stateid/seqid bookkeeping
and can cascade into `NFS4ERR_BAD_SEQID` storms. (v4.1 is unaffected — it uses the slot
table and sets `SkipOwnerSeqid`.)

Fix: cache the actual encoded result + status for **every** owner-seqid op (store on the
owner keyed by the op, or store the last encoded op-result regardless of opcode), and
return those exact bytes on replay. Mirror Linux nfsd's per-stateowner `so_replay`.

### [HIGH] LOCK replay (exactly-once) is not honored — replayed LOCK/LOCKU returns NFS4ERR_BAD_SEQID — `internal/adapter/nfs/v4/state/manager.go:1577` / `1655` / `1889`

`LockOwner.LastResult` is declared (`lockowner.go:32`) but **never populated**. On a
lock-owner seqid replay, `LockNew`/`LockExisting` unconditionally return `ErrBadSeqid`
(`manager.go:1577-1582`, `1655-1656`) — the `if lockOwner.LastResult != nil` branch is
dead because nothing ever sets it. LOCKU's replay path returns the *current* stateid
(`manager.go:1889-1892`), which is wrong if the original LOCKU advanced the seqid and
modified ranges.

Why it matters: a retransmitted LOCK (lost reply) is a normal occurrence over TCP. The
client resends with the same lock-owner seqid expecting the cached success/denied
result; DittoFS instead returns `NFS4ERR_BAD_SEQID`, which the Linux client treats as a
fatal lock-owner error → it abandons/recovers the lock-owner, potentially dropping locks
the application believes it holds (silent lock loss / data race). RFC 7530 §9.1.7 + §8.20
require replaying the cached result.

Fix: populate `LockOwner.LastResult` with the encoded LOCK/LOCKU result on every
success/denial and return it on `SeqIDReplay` (mirror the open-owner replay fix).

### [HIGH] NFSv4.1 slot replay is not validated against the original request → SEQ_FALSE_RETRY never detected — `internal/adapter/nfs/v4/state/slot_table.go:177` + `internal/adapter/nfs/v4/v41/handlers/sequence.go:54`

On a slot retry (`seqID == slot.SeqID`), `ValidateSequence` returns the cached reply if
present (`slot_table.go:177-192`) **without comparing the new request to the original**.
RFC 8881 §2.10.6.1.3 requires the server to detect when a retried `seqid` carries a
*different* request (different ops/args) and return `NFS4ERR_SEQ_FALSE_RETRY`. DittoFS
stores no request fingerprint (the `Slot` struct caches only `SeqID`/`InUse`/
`CachedReply`), so a buggy/malicious client reusing a slot+seqid with new operations
silently receives the stale cached reply — a correctness and potential
confused-deputy/data-exposure issue.

Fix: cache a digest of the request (or at least op-count + opcodes + the COMPOUND arg
bytes) alongside `CachedReply`; on retry, compare and return `NFS4ERR_SEQ_FALSE_RETRY`
on mismatch.

### [HIGH] Grace period is skipped whenever there are zero expected clients, even on a real restart — `internal/adapter/nfs/v4/state/grace.go:104`

`StartGrace` returns immediately (no grace) when `expectedClientIDs` is empty
(`grace.go:103-107`). Expected clients come from a client-state file written at graceful
shutdown (`SaveClientState`, `manager.go:907`). On an **ungraceful** crash (kill -9,
power loss) that file is stale/absent, so the server starts with no expected clients and
**no grace period** — yet clients that held opens/locks before the crash will attempt
`CLAIM_PREVIOUS`, which then hits `ErrNoGrace` (`manager.go:961-962`) and fails reclaim.

Why it matters: a crash is exactly when grace matters most (RFC 7530 §9.6.2 — grace
exists to let clients reclaim after server failure). DittoFS provides grace only after a
clean shutdown, defeating the purpose. Worse, because v4 state is in-memory only (see
persistence finding), even *with* grace the reclaim cannot succeed — but the API contract
is still wrong (`NFS4ERR_NO_GRACE` instead of `NFS4ERR_GRACE`/`NFS4ERR_STALE_CLIENTID`).

Fix: start a grace period on every cold start where there is any indication prior state
may have existed (e.g., presence of confirmed shares / a "dirty restart" marker), not
just when a clean snapshot listed clients. At minimum, gate the "skip grace" purely on a
verified clean shutdown, and during grace return `NFS4ERR_GRACE` to non-reclaim opens
regardless of the (untrustworthy) expected-client set.

### [HIGH] All NFSv4 state is in-memory only — silent total state loss on restart — `internal/adapter/nfs/v4/state/manager.go:31` (whole StateManager)

`StateManager` holds every map (clients, open-owners, open-states, lock-owners,
lock-states, delegations, sessions, slots) purely in RAM; only a thin `ClientSnapshot`
list (clientid/verifier/addr — *not* the opens/locks themselves) is persisted at clean
shutdown (`grace.go:333`, `manager.go:907`). There is no per-share-backend persistence of
open/lock/stateid state. After any restart the boot epoch changes
(`manager.go:197`), so old stateids correctly become `NFS4ERR_STALE_STATEID`
(`stateid.go:145-149`) — but the client's only recovery path (CLAIM_PREVIOUS reclaim) has
nothing to reclaim against, because the server kept no record of what was open/locked.

Why it matters: byte-range locks and share reservations do not survive restart even for
persistent backends (badger/postgres), so a window exists where a reclaiming client
re-acquires a lock that conflicts with a *new* client that opened in the meantime — or
believes it still holds a lock that the server has forgotten (silent lock loss). This is
inherent to a single-node in-memory state machine and is partly documented
(docs/NFS.md notes directory delegations are ephemeral; FAQ notes single-node), but the
broader open/lock-state non-persistence is not called out for v4 and is a v1.0 hardening
gap.

Fix (scoped): document explicitly that NFSv4 open/lock state is non-durable; for v1.0,
either (a) persist a stateid/lock journal per share backend to make CLAIM_PREVIOUS
meaningful, or (b) on cold start enter a full grace window that blocks all new
state-creating opens/locks until grace expires, so reclaim races cannot occur.

---

## MED

### [MED] SETCLIENTID / SETCLIENTID_CONFIRM omit the callback-path / CLID_INUSE principal check — `internal/adapter/nfs/v4/state/manager.go:296` / `client.go:18`

`ErrClientIDInUse` (`NFS4ERR_CLID_INUSE`) is defined (`client.go:18-21`) but never
returned. The five-case SETCLIENTID algorithm (`manager.go:291-318`) keys solely on the
client-id string + verifier and does **not** compare the requesting principal/`clientAddr`
to the existing confirmed record. RFC 7530 §9.1.1 requires `NFS4ERR_CLID_INUSE` when a
*different* principal presents an in-use id string. Without it, any client that learns
another client's id string can hijack/replace its client record (the new unconfirmed
record replaces the confirmed one on CONFIRM, `manager.go:530-538`). Lower severity than
the share/lock findings because AUTH_SYS is already spoofable and the docs flag AUTH_SYS
as untrusted, but it is a real spec gap and an avenue for accidental cross-client
clobbering.

### [MED] EXCHANGE_ID has no principal binding; client-reboot purge is unconditional on verifier change — `internal/adapter/nfs/v4/state/v41_client.go:200`

`ExchangeID` (`v41_client.go:182-215`) keys only on `co_ownerid` + verifier. A different
verifier for the same owner id unconditionally **purges all sessions/state** of the
existing client (`purgeV41Client`, `v41_client.go:208`) with no principal check. RFC 8881
§18.35.4 ties reboot detection to the same principal; a client (or attacker) that reuses
another's `co_ownerid` with a fresh verifier can forcibly evict the legitimate client's
sessions and state. Add a principal/cred comparison and return `NFS4ERR_CLID_INUSE` on
mismatch.

### [MED] SeqRetry returns cached reply without echoing fresh SEQUENCE status flags / lease renewal — `internal/adapter/nfs/v4/v41/handlers/sequence.go:54`

On a slot retry the handler returns the byte-for-byte cached COMPOUND
(`sequence.go:61-63`). That is correct for exactly-once *payload*, but RFC 8881 §18.46.3
notes the SEQUENCE reply also carries `sr_status_flags` and that retries still renew the
lease. DittoFS does **not** renew the lease on a pure retry (renewal only happens in the
`SeqNew` branch, `sequence.go:97`) and replays stale status flags (e.g. a
`CB_PATH_DOWN`/`RECALLABLE_STATE_REVOKED` that has since cleared/set). A client that only
ever retransmits could see its lease expire. Low blast radius but spec-divergent.

### [MED] No eviction / unbounded growth of v4.0 open-owner replay caches and lock-state maps — `internal/adapter/nfs/v4/state/manager.go:46-58`

`openStateByOther`, `lockStateByOther`, `openOwners`, `lockOwners`, and per-owner
`LastResult` byte buffers are only reclaimed on CLOSE/LOCKU/RELEASE_LOCKOWNER or lease
expiry. A v4.0 client that opens-then-leaks (never closes, keeps renewing) grows these
maps without bound; there is no global cap analogous to `maxDelegations`/
`maxSessionsPerClient`. The session reaper (`manager.go:2350`) only handles **v4.1**
clients (`v41ClientsByID`) — v4.0 leases are reaped via per-client `time.AfterFunc`
timers (`lease.go:48`), which is fine for expiry but provides no back-pressure cap on
live, renewing clients. Consider a per-client/global open & lock-state ceiling returning
`NFS4ERR_RESOURCE`.

### [MED] LOCKT/LOCK conflict owner is reported with ClientID=0 and raw OwnerID bytes — `internal/adapter/nfs/v4/state/manager.go:1735`

In `acquireLock`'s denied path the `LOCK4denied.Owner` is filled with
`ClientID = 0` and `OwnerData = []byte(el.Owner.OwnerID)` (`manager.go:1735-1736`) — the
internal `"nfs4:{id}:{hex}"` string, **not** the parsed clientid/owner. (LOCKT uses
`parseConflictOwner` correctly at `manager.go:1826`, but the LOCK path does not.) RFC 7530
§16.10.4 expects the real conflicting `lock_owner4`. Some clients log/branch on the
returned owner; emitting clientid=0 + a mangled owner blob is a conformance defect.
Reuse `parseConflictOwner` in `acquireLock`.

### [MED] Lease timer per client uses time.AfterFunc with a guard re-check, but Renew/expiry has a benign race window — `internal/adapter/nfs/v4/state/lease.go:48`

`NewLeaseState`'s callback re-checks `time.Since(LastRenew) < Duration` under `ls.mu`
(`lease.go:53-58`) before expiring — good. But `IsExpired` (`lease.go:85-89`) and the
callback both read `LastRenew` while `Renew` (`lease.go:72-82`) writes it; the timer can
fire, pass the guard, drop `ls.mu`, then race a concurrent `Renew` and call
`onLeaseExpired` on a just-renewed client (`onLeaseExpired` then tears down live state at
`manager.go:629`). The window is small but on a busy server can cause spurious state
revocation. Consider an epoch/generation counter compared inside `onLeaseExpired` under
`sm.mu`, or renew-by-deadline comparison there.

---

## LOW

### [LOW] `DowngradeOpen`/`ConfirmOpen` replay returns current stateid, not original encoded result — `internal/adapter/nfs/v4/state/manager.go:1320` / `1140`

Subsumed by the HIGH replay-cache finding but worth noting independently: even once a
replay cache exists, these branches synthesize a stateid rather than returning the
op's original bytes, so a replayed DOWNGRADE after a subsequent DOWNGRADE could return a
seqid the client never saw.

### [LOW] `generateConfirmVerifier` time-based fallback is predictable — `internal/adapter/nfs/v4/state/manager.go:263`

If `crypto/rand` fails, the confirm verifier falls back to `time.Now().UnixNano()`
bytes (`manager.go:263-267`). The comment acknowledges "degraded security." crypto/rand
failure is near-impossible on supported platforms, but the fallback defeats the very
unpredictability the verifier exists for. Prefer failing the operation
(`NFS4ERR_SERVERFAULT`) over emitting a guessable verifier.

### [LOW] Boot-epoch in stateid "other" is only the low 24 bits — collision/aliasing risk across restarts — `internal/adapter/nfs/v4/state/stateid.go:74`

The stale-stateid check (`isCurrentEpoch`) compares only the low 24 bits of
`bootEpoch` (`stateid.go:74-77, 95-104`). Two restarts ~194 days apart (2^24 s) can
share the low-24 epoch, so a stateid from the older incarnation could be mis-classified
as current and routed to a map lookup that returns `NFS4ERR_BAD_STATEID` instead of the
more-correct `NFS4ERR_STALE_STATEID`. Cosmetic for correctness (both are errors the
client recovers from) but weakens the documented intent. The full 32-bit epoch *is*
embedded in the 64-bit clientid, so the risk is isolated to stateid stale-vs-bad
classification.

### [LOW] `IsSpecialStateid` does not recognize the v4.1 "current stateid" sentinel — `internal/adapter/nfs/v4/types/types.go:193`

RFC 8881 §16.2.3.1.2 defines a current-stateid sentinel (seqid=1, other=all-zeros) used
inside COMPOUNDs to chain ops. `IsSpecialStateid` only matches all-zeros/seqid=0 and
all-ones (`types.go:193-223`). If a v4.1 client uses the current-stateid convention,
validation will mis-handle it. Low priority because the Linux client passes real
stateids in practice, but it is a latent v4.1 conformance gap.

---

## Severity tally

- HIGH: 7  (share_deny not enforced; all-ones stateid accepted on WRITE; open-owner
  replay cache cross-op contamination; lock-owner replay not honored; SEQ_FALSE_RETRY
  not detected; grace skipped on crash restart; in-memory-only state / no durable
  open-lock state)
- MED: 7  (no CLID_INUSE principal check on SETCLIENTID; EXCHANGE_ID unconditional
  reboot purge w/o principal binding; SeqRetry no lease renewal / stale status flags;
  unbounded v4.0 state-map growth; LOCK denied owner = clientid 0 / raw bytes; lease
  timer expiry race window)
- LOW: 4  (downgrade/confirm replay synthesizes stateid; predictable verifier fallback;
  24-bit boot-epoch aliasing; missing v4.1 current-stateid sentinel)

Top finding: **share_deny share reservations are never enforced across open-owners**
(`open.go:331` / `manager.go:944`) — `NFS4ERR_SHARE_DENIED` exists but is never returned,
so a core NFSv4 data-integrity guarantee is silently a no-op.

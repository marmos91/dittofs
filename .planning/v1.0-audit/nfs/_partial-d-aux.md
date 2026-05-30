# v1.0 Audit — Area #4 (NFS), batch 2, sub-area D: auxiliary protocols (NLM + NSM + portmap)

Scope: `internal/adapter/nfs/nlm/` (~3.4K), `internal/adapter/nfs/nsm/` (~1.7K), `internal/adapter/nfs/portmap/` (~1K). READ-ONLY audit.
Cross-checked against X/Open NLMv4, RFC 1813 (statd), RFC 1833 / RFC 1057 (RPCBIND/portmap), Linux `fs/lockd/` + `utils/statd` semantics.

Headline: **4 HIGH findings** clustered in lock *recovery* + statd security. (1) `handleClientCrash` is a **no-op stub** — a crashed client's byte-range locks are logged but never released, so they are held forever. (2) The **grace period is fully built but never wired** — no `EnterGracePeriod` caller, every lock Manager has `gracePeriod==nil`, and `LockFileNLM` never calls `IsOperationAllowed`, so `NLM4_DENIED_GRACE_PERIOD` is unreachable and `reclaim` is a no-op. (3) **NSM NOTIFY is an inert TODO stub with no sender authentication** — the classic statd forged-reboot lock-drop primitive is latent (relay not implemented yet, but the design has no auth gate, no monitored-host check). (4) **No SM_NOTIFY state-number monotonicity** — replays re-trigger lock release. Net: NLM locks are silently lost/stranded across restart and on client crash, and the statd security gates must land before the relay ships. Positives confirmed in code (not first-pass assumptions): **portmap SET/UNSET are localhost-gated** (`handlers.IsLocalhost`, threaded through dispatch), **PMAP_CALLIT is omitted**, NLM byte-range locking correctly delegates to the unified `pkg/metadata/lock` manager with cross-protocol conflict detection, owner identity uses the full `caller_name+svid+oh` triple, and the blocking-lock queue + grant drain + GRANTED callback are wired end-to-end (the drain has races/starvation, not a leak). None of this is bloat — the code is appropriately minimal; the gaps are unfinished recovery + unwired security gates.

---

## NLM (Network Lock Manager)

### [HIGH] Client-crash lock cleanup is a no-op stub — `handleClientCrash` logs but never releases the crashed client's locks — `pkg/adapter/nfs/nlm.go:479-519`

`handleClientCrash` is the callback wired into both the NSM crash detector (failed SM_NOTIFY) and FREE_ALL. It iterates shares but **does nothing**: there is no `RemoveUnifiedLock` / release call, `totalReleased` is hard-coded 0, and the body's own comment admits "This is a simplified implementation … A production enhancement would be to iterate the LockStore and explicitly release all locks matching the prefix" (nlm.go:497-508). So when a client crashes (or FREE_ALL is invoked), the server logs the event and **leaves all of that client's NLM byte-range locks held forever**. Combined with the grace finding, this means crashed-client locks are never reclaimed *and* never released → permanent deadlock of any range the dead client held; another client blocking on that range waits indefinitely (or, with blocking unimplemented for that path, gets `NLM4_DENIED` forever). Note the boot-side recovery *is* wired (`nlm.go:540 LoadRegistrationsFromStore` → `546 IncrementServerState` → `552 NotifyAllClients`), so own-reboot notification works; the gap is specifically the *release* side of crash handling. Fix: implement prefix-scoped lock release (`nlm:{clientID}:`) across each share's lock manager, then drain the blocking queue for affected files.

### [HIGH] Grace period is built and delegated through the Manager but never *entered*, and `LockFileNLM` never consults it — `NLM4_DENIED_GRACE_PERIOD` is unreachable — `pkg/adapter/nfs/nlm.go:74-118`, `pkg/metadata/lock/manager.go:2282-2318`, `pkg/metadata/lock/grace.go`

Correction/refinement of the grace finding above: the wiring is *more* built than first appears — `Manager` exposes `EnterGracePeriod` / `IsOperationAllowed` / `MarkReclaimed` / `IsInGracePeriod` delegating to a `GracePeriodManager` (manager.go:2282-2318), and `NewManagerWithGracePeriod` exists (manager.go:626). **But the gate is still dead in the NLM path for two independent reasons**: (1) no non-test code ever calls `NewGracePeriodManager`/`EnterGracePeriod` — every lock Manager is constructed *without* a grace period (`m.gracePeriod == nil`), so all the delegation methods are no-ops and `IsInGracePeriod()` is always false; and (2) even if grace were entered, `nlmService.LockFileNLM` (nlm.go:74-118) goes straight to `lockMgr.AddUnifiedLock` and **never calls `IsOperationAllowed`** with the reclaim/new distinction — it sets `unifiedLock.Reclaim = reclaim` (nlm.go:91) but nothing downstream reads it for grace gating (`AddUnifiedLock` has no grace check). So `NLM4_DENIED_GRACE_PERIOD` (status 4) is unreachable and `reclaim` is a no-op. Boot does bump state + notify peers, but with no grace window and no crash-release (above), a restart still drops/strands every lock. Fix: construct lock managers with a grace period, `EnterGracePeriod` on boot seeded from persisted locks, and make `LockFileNLM` call `IsOperationAllowed({IsReclaim:reclaim, IsNew:!reclaim})` and surface `ErrGracePeriod` → `NLM4_DENIED_GRACE_PERIOD`.

### [MED] Blocking-lock grant drain runs unsynchronized with the queue mutex and re-acquires per-waiter without ordering guarantees — `pkg/adapter/nfs/nlm.go:300-347`, `internal/adapter/nfs/nlm/handlers/unlock.go:66-105`

The blocking machinery is real and **fully wired**: enqueue (`lock.go:144-193` → `NLM4_BLOCKED`, 100-cap → `NLM4_DENIED_NOLOCKS`) and the grant drain live in the adapter, which on lock release calls `blockingQueue.GetWaiters` (nlm.go:303), re-attempts `lockManager.AcquireLock` per waiter (nlm.go:320), and on success `RemoveWaiter` + `ProcessGrantedCallback` (nlm.go:332-340). (Contrary to a first-pass read of `unlock.go` — which doesn't drain itself — the adapter does drive grants; no leak there.) The remaining concern is correctness of the drain: it operates on a **copied slice** from `GetWaiters` (queue.go:128-131) while holding *no* queue lock during the per-waiter `AcquireLock`+callback, so a concurrent `Enqueue`/`Cancel` races the drain; and the loop grants in copied-FIFO order but a waiter that still conflicts is left in the queue (nlm.go:326-329) with no re-arm, so it is only retried on the *next* unlock — a blocked exclusive waiter behind a just-released shared lock can be skipped indefinitely (starvation). Verify the drain is invoked on **all** release paths (UNLOCK, FREE_ALL, NSM crash cleanup) and consider holding the queue lock across the grant decision or re-arming skipped waiters. MED: works in the common case, races/starves under contention.

### [MED] NLM_GRANTED callback has no retransmit / ack tracking — a single lost GRANTED strands the lock — `internal/adapter/nfs/nlm/callback/client.go:53-110`, `internal/adapter/nfs/nlm/callback/granted.go:33-89`

`SendGrantedCallback` makes a single fresh-TCP attempt with a 5s total deadline and, on failure, `ProcessGrantedCallback` immediately releases the lock (granted.go:68-80). There is **no retransmit loop and no GRANTED_RES ack correlation** (NLMProcGrantedRes=15 is defined but unused). X/Open requires GRANTED_MSG to be retransmitted until the client replies GRANTED_RES, with a bounded give-up. Two consequences: (a) a transient network blip on the single attempt makes the server silently *release a lock it told the queue it would grant* (the waiter is dropped, lock removed) — the client that asked to block gets neither GRANTED nor a re-offer; (b) the inbound `Granted` handler (handlers/granted.go) just logs and returns `NLM4_GRANTED` — it does not match the ack to any in-flight grant, so even a successful GRANTED_RES is ignored. Fix: bounded retransmit with ack matching; only release the lock after the give-up bound, not after the first failure.

### [MED] TEST holder and GRANTED owner info are lossy — `svid`/`oh` dropped because UnifiedLock doesn't carry them — `internal/adapter/nfs/nlm/handlers/test.go:146-160`

`conflictToHolder` hard-codes `Svid: 0, OH: nil` with the comment "We don't track svid in UnifiedLock - would need to parse OwnerID" (test.go:155-156). The owner key *is* built as `nlm:{caller_name}:{svid}:{oh_hex}` (test.go:142-143), so the data exists in the OwnerID string but is thrown away on the way back to the client. Linux `F_GETLK` clients expect the holder's `l_pid` (svid) in the NLM4_DENIED holder. Returning svid=0/oh=nil gives the client a useless holder (it can't identify the blocker). Same lossiness in the cross-protocol denied paths (`cross_protocol.go` only logs holder info; `LockResponse` has no holder field so `buildDeniedResponseFromSMBLease`/`...ByteRangeLock` return a bare `NLM4_DENIED`). Fix: parse svid/oh back out of the OwnerID (or extend UnifiedLock to carry them) and populate the holder.

### [MED] CANCEL/GRANT race is unguarded across the two mutexes — `internal/adapter/nfs/nlm/handlers/cancel.go:73-107`, `internal/adapter/nfs/nlm/blocking/queue.go:85-105`

`Cancel` dequeues the waiter under the `BlockingQueue` mutex (queue.go:85). The grant path (adapter-driven `GetWaiters` → `ProcessGrantedCallback` → `lm.RemoveUnifiedLock`) holds the *lock-manager* mutex, not the queue mutex, while sending the callback. A `NLM_CANCEL` arriving after `GetWaiters` copied the slice but before the grant completes will mark the waiter cancelled (the `IsCancelled` check in granted.go:39 catches it before send) — but if the grant already acquired the unified lock and *then* the cancel returns success to the client, the lock is orphaned (client thinks it cancelled; server granted). The waiter copy in `GetWaiters` (queue.go:128-131) returns pointers, so `Cancel` and the grant operate on the same `Waiter` object, which mitigates but does not fully close the post-acquire window. Fix: perform enqueue/grant/cancel under one mutex, and on a cancel that races a completed grant, release the just-acquired lock.

### [MED] `extractCallbackAddr` hard-codes the callback port to "12049" — wrong for any non-default deployment, and ignores the client's advertised callback port — `internal/adapter/nfs/nlm/handlers/lock.go:234-245`

The blocking-lock callback target is built by taking the client IP and forcing port `12049` (lock.go:244), with a comment admitting "For now, use the standard approach". Real NLM clients listen for GRANTED on a dynamically chosen port advertised via the portmapper / the NSM `my_id` callback info, not the server's NFS port. Hard-coding 12049 means GRANTED callbacks will be sent to the wrong port and silently fail (→ lock released per the no-retransmit finding) on essentially every real client. Fix: resolve the client's NLM callback port via its registration / portmap GETPORT, or at minimum make it configurable; do not assume 12049.

### [LOW] Owner identity is correctly composed from the full triple — `internal/adapter/nfs/nlm/handlers/test.go:142-143` (positive)

`buildOwnerID` correctly combines `caller_name + svid + oh` into the lock-manager OwnerID, so distinct client processes don't collapse into one owner and UNLOCK/CANCEL match the exact owner. This is correct per X/Open. Noted as a positive; no action.

### [LOW] NLM XDR length caps are partial — `caller_name`/`name` are bounded, but `fh`/`oh`/`cookie` opaque fields are not capped before alloc — `internal/adapter/nfs/nlm/xdr/decode.go:30-78,91-99`

`caller_name` and FREE_ALL `name` are checked against `LMMaxStrLen` (decode.go:38, 291). But `fh`, `oh`, and `cookie` go through bare `xdr.DecodeOpaque` with no cap (decode.go:44,51,95 etc.) — `MaxOpaqueLen=1024` is defined (types.go:383) but never applied. A malformed huge length prefix is bounded only by whatever `core` XDR `DecodeOpaque` enforces against the remaining buffer (verify it caps allocation and returns an error rather than pre-allocating). NLM fh must be ≤64 (RFC 1813), oh/cookie ≤1024. Fix: apply `MaxOpaqueLen`/64 caps in `DecodeNLM4Lock` and reject overruns as GARBAGE_ARGS.

### [LOW] NM_LOCK (proc 22) and async _MSG/_RES variants are unimplemented — acceptable but document — `internal/adapter/nfs/nlm/dispatch.go:60-103`

The dispatch table implements NULL/TEST/LOCK/CANCEL/UNLOCK/SHARE/UNSHARE/FREE_ALL but not NM_LOCK (non-monitored lock, proc 22) nor any async _MSG/_RES procedures. Most Linux clients use the sync procedures, so this is fine for v1.0, but NM_LOCK absence means a client requesting a non-monitored lock gets PROC_UNAVAIL. Low impact; just confirm the dispatcher returns a clean RPC PROC_UNAVAIL (not a panic) for unimplemented procs.

### [LOW] SHARE/UNSHARE unconditionally grant without tracking — `internal/adapter/nfs/nlm/handlers/share.go:89-119`

DOS-style share-mode locks always return `NLM4_GRANTED` with no conflict detection and no state tracked (share.go:97-101). This is documented as intentional (rare with modern clients) and is safe given advisory-only semantics, but a Windows NFS client relying on share-deny semantics would get a false grant. Acceptable for v1.0; flagged for completeness.

---

## NSM (Network Status Monitor / statd)

### [HIGH] SM_NOTIFY is an inert TODO stub AND has no sender authentication — the forged-SM_NOTIFY lock-drop primitive is latent and must be gated before the relay is implemented — `internal/adapter/nfs/nsm/handlers/notify.go:14-53`

`Notify` decodes `stat_chge`, logs it, and returns `StatSucc` — the entire relay is a `TODO` (notify.go:34-46): it does **not** find local registrations for the rebooted host, does **not** send SM_NOTIFY callbacks, and does **not** trigger NLM lock cleanup. So today an inbound NOTIFY does nothing (third-party peer-reboot recovery is simply broken — RFC 1813 statd is supposed to relay). Critically, the handler also performs **zero source validation**: no comparison of the RPC source IP (`ctx.ClientAddr`) to the claimed `mon_name`, no check that `mon_name` is actually in the monitored list, no privileged-port check, and NSM procedures are all `NeedsAuth:false` (dispatch.go:60-90). 

When the TODO is implemented as written, this becomes the **classic statd spoofing vector**: any host able to send an RPC packet to the statd port could forge `SM_NOTIFY mon_name=<victim>` and cause the server to drop that victim's NLM byte-range locks (another client then grabs the range the victim still holds → silent corruption). Flagging now as HIGH because the security gate must land *with* the relay, not after. Fix: (1) only act on NOTIFY for hosts currently in the monitored (`SM_MON`) list — ignore unknown `mon_name`; (2) validate the RPC source address resolves to / matches `mon_name` (or restrict the statd listener to a trusted network); (3) enforce state-number monotonicity (next finding).

### [HIGH] No SM_NOTIFY state-number monotonicity — replays re-trigger lock release (and there is no per-host stored state to compare against) — `internal/adapter/nfs/nsm/handlers/notify.go`, `internal/adapter/nfs/nsm/handlers/mon.go:71`

RFC 1813 statd: each monitored host has a monotonically increasing state number; a NOTIFY is a genuine reboot only when its state exceeds the last observed value, and duplicates/replays must be dropped. `Mon` stores a per-client SM state via `tracker.UpdateSMState(clientID, state)` (mon.go:71) — but that records *the server's own* state at registration time, not the monitored peer's last-seen state, and `Notify` never reads or compares any stored state before acting. Once the relay (previous finding) is implemented, a replayed NOTIFY (even a legitimately re-sent one) would re-fire lock release every time, and an attacker could replay endlessly. Fix: store last-seen state per monitored `mon_name`; act only when `incoming_state > stored_state`, then update; otherwise drop silently.

### [MED] Server NSM state is in-memory only — `InitialState` resets to 1 every boot, so the monotonic guarantee across restarts is broken — `internal/adapter/nfs/nsm/handlers/handler.go:84-99`, `pkg/adapter/nfs/nlm.go:438-546`

Own-reboot recovery *is* wired (correcting a first-pass concern): the adapter boot path calls `LoadRegistrationsFromStore` → `IncrementServerState` → `NotifyAllClients` (nlm.go:540,546,552). The remaining gap is that the NSM server state is **not persisted** — `initNSMHandler` always sets `InitialState: 1` (nlm.go:442) and `Handler` holds it in an in-memory `atomic.Int32` (handler.go:37,97). So every restart begins at state 1 and `IncrementServerState` produces 2, 3, … within a single process lifetime but **resets to 1+1=2 on the next boot**. RFC 1813 requires statd's own state to be monotonic *across* reboots (read from disk, bumped, written back). A client that saw state 5 before the crash and then receives a NOTIFY claiming state 2 will (correctly) treat it as stale and not reclaim → lock recovery silently fails after the second restart. Fix: persist the server state number (sm/state-equivalent) and load it in `initNSMHandler` instead of hard-coding `InitialState: 1`.

### [MED] SM_MON / SM_UNMON / SM_UNMON_ALL accept any remote caller — `internal/adapter/nfs/nsm/handlers/mon.go:18`, `internal/adapter/nfs/nsm/handlers/unmon.go:14`, `internal/adapter/nfs/nsm/handlers/unmon_all.go:13`

These are registration-control operations (statd is normally invoked by the *local* lockd, not by remote peers). None restrict the caller to loopback (NeedsAuth:false; no `ctx.ClientAddr` loopback check). A remote attacker can (a) flood the mon list toward the `maxClients=10000` cap (mon.go:44-51 bounds it, good, but it's still a remote-fillable table), or (b) issue `SM_UNMON_ALL` for a victim callback host (unmon_all.go:35-51 matches purely on `CallbackInfo.Hostname == myID.MyName`, which is attacker-supplied) to disable that host's monitoring and defeat its lock recovery. Fix: gate SM_MON/UNMON/UNMON_ALL to loopback / local lockd. Note: UNMON/UNMON_ALL correctly do *not* release NLM locks (only clear callback info), which is right.

### [LOW] NSM `priv` is correctly fixed-16, but `mon_name`/`my_name` opaque handling otherwise fine — `internal/adapter/nfs/nsm/xdr/decode.go:119-135` (mostly positive)

`DecodeMon` reads `priv` as a fixed `[16]byte` via `io.ReadFull` (decode.go:126-129) — correct per RFC (opaque[16], no length prefix), and `mon_name`/`my_name` are length-capped at `SMMaxStrLen=1024` (decode.go:32,53,93,150). Good. Minor: a truncated `priv` returns a wrapped error (good, not a panic). No fix needed; noted to confirm bounds are clean.

### [LOW] `SM_NOTIFY` callback `encodeStatus` hand-rolls padding instead of using the shared XDR string helper — `internal/adapter/nfs/nsm/callback/client.go:148-169`

`encodeStatus` (client.go:148) duplicates string-length+padding logic by hand, whereas the response encoder `EncodeStatus` (nsm/xdr/encode.go:66) uses `xdr.WriteXDRString`. The two should produce identical bytes, but the hand-rolled version is an easy place for a future padding bug and is redundant. Low/maintainability: route the callback through the shared encoder.

---

## portmap (rpcbind)

### [GOOD] PMAP_SET / PMAP_UNSET are localhost-gated — matches documented posture — `internal/adapter/nfs/portmap/handlers/set.go:16-20`, `internal/adapter/nfs/portmap/handlers/unset.go:16-20`, `internal/adapter/nfs/portmap/handlers/handler.go:38-49`

Both SET and UNSET check `IsLocalhost(clientAddr)` and return `false` to non-loopback callers; the client address is correctly threaded from the accept loop (`server.go:182,353`) through the dispatch closure (`dispatch.go:38-46`) into the handlers. `IsLocalhost` parses host:port and falls back to bare-IP, then `ip.IsLoopback()`. This correctly enforces the `docs/NFS.md` "SET/UNSET restricted to localhost" claim. (Contrary to a first-pass suspicion, this *is* implemented.) No action. Minor nit: a SET/UNSET from a non-loopback caller returns RPC SUCCESS with body `false` rather than a denial — acceptable per RFC (boolean result), but the rejection is only visible in logs.

### [GOOD] PMAP_CALLIT (proc 5) is omitted — no reflection/amplification vector — `internal/adapter/nfs/portmap/dispatch.go:23-60`, `internal/adapter/nfs/portmap/types/constants.go:53-56`

The dispatch table registers only NULL/SET/UNSET/GETPORT/DUMP; proc 5 (CALLIT/INDIRECT) is intentionally absent and an unknown proc returns RPC PROC_UNAVAIL (server.go:337-340). This is the correct hardening against the classic rpcbind amplification DoS, confirmed by tests (`portmap_integration_test.go:165`, `server_test.go:340`). No action.

### [MED] PMAP_DUMP is unrestricted information disclosure — composes with the NSM attack surface — `internal/adapter/nfs/portmap/handlers/dump.go:9-12`

DUMP returns the full service map (NFS/MOUNT/NLM/NSM program/version/proto/port) to any caller, loopback or not (dispatch.go:54-58 passes `_` for clientAddr). This is standard rpcbind behavior and necessary for `rpcinfo`/`showmount`, so it's defensible — but it hands a remote attacker an exact inventory of the NLM/NSM ports needed to mount the SM_NOTIFY attack above. Given DittoFS's "minimize surface" posture, consider gating DUMP to loopback or documenting it as intentionally open. MED because it composes with the NSM HIGH; the server.go comment (line 252-255) already recommends firewalling the UDP port.

### [MED] UDP DUMP is an unauthenticated amplification surface even without CALLIT — `internal/adapter/nfs/portmap/server.go:249-311`

The server.go comment (lines 252-255) correctly notes that DUMP over UDP produces a response larger than the request and recommends firewalling. The TCP path has a 64-conn semaphore (server.go:155-164, good) and a 64KB fragment cap (server.go:218), but the **UDP path has no rate limit and no per-source throttle** — each datagram spawns synchronous processing in the single serveUDP loop and replies to the spoofable source address. A spoofed-source UDP DUMP flood turns the portmapper into a (modest) reflection amplifier. Fix: cap UDP reply size / disable DUMP over UDP, or bind UDP to loopback by default. At minimum the doc-comment recommendation should be the default, not advice.

### [LOW] portmap mapping decoder accepts trailing garbage and does not validate `prot` — `internal/adapter/nfs/portmap/xdr/decode.go:14-27`

`DecodeMapping` requires ≥16 bytes and ignores trailing bytes (decode.go:12-17) — fine and panic-safe (bounds-checked before each `Uint32`). But it does not validate that `prot` is TCP(6)/UDP(17); a SET with an arbitrary `prot` creates a junk registry key (harmless but pollutes DUMP). Low. Optionally reject non-{6,17} `prot` in SET. No panic risk here.

---

## Severity tally

| Severity | NLM | NSM | portmap | Total |
|----------|-----|-----|---------|-------|
| HIGH     | 2   | 2   | 0       | **4** |
| MED      | 5   | 2   | 2       | **9** |
| LOW      | 4   | 2   | 1       | **7** |

(Two portmap items — SET/UNSET localhost gating and CALLIT omission — are positive [GOOD] findings, not counted. The NLM owner-triple composition and the NSM fixed-16 `priv`/loopback-correct-on-locks items are also positives, counted under their LOW rows as informational.)

### Top finding
**SM_NOTIFY is an unauthenticated, un-gated TODO stub** (`nsm/handlers/notify.go:14-53`). The third-party reboot relay is not implemented (so peer-reboot lock recovery is currently broken), and when it is implemented there is no source-address validation, no monitored-host check, and no state-number monotonicity — the canonical statd spoofing primitive that lets any reachable host forge a peer-reboot and silently drop a victim client's byte-range locks. The auth + monotonicity gates must be designed *into* the relay before it ships.

### Cross-cutting theme
The aux-protocol **correctness backbone is sound**: NLM delegates byte-range locking to the unified `pkg/metadata/lock` manager, cross-protocol conflict detection exists, owner identity is built from the full `caller_name+svid+oh` triple, and portmap correctly hardens SET/UNSET (localhost) and CALLIT (omitted). The gaps are concentrated in the **recovery half** of locking: grace-period/reclaim is plumbed but never enforced (`NLM4_DENIED_GRACE_PERIOD` is dead), the NLM_GRANTED callback has no retransmit and targets a hard-coded port, the blocking-lock *grant drain* is delegated out of this package (verify the adapter calls it on every release path), and NSM own-reboot + peer-reboot recovery is partially-built (state not bumped on boot, NOTIFY relay is a stub). Net effect: NLM locks are silently lost across restart with no working reclaim. None of this is bloat — the code is appropriately minimal — but the locking *recovery* and *security gating* must be completed before v1.0 claims NFSv3 locking is production-grade.

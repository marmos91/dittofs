# NFS adapter MED triage — #2407's tranches #2400-#2406

Triaged 2026-09-14 against `develop` @ `221e9939b`. Every finding was re-derived by symbol; the
audits are pinned at `40884ad4f` (196 commits back) so no quoted line number was trusted.

This closes a gap in the program's own tracking: the ROADMAP's wave table runs 0-7 and Wave 4 is
SMB-only, so these seven tranches had no wave. They are the NFS mirror of Wave 4's SMB triage.

## Verdict counts

61 findings triaged. Every finding carries exactly one verdict, so the rows below sum to 61.

| Tranche | Area | LIVE | Fixed | Dup | Other | Total |
| --- | --- | --- | --- | --- | --- | --- |
| #2400 | MOUNT and export access control | 5 | — | — | 1 split | 6 |
| #2401 | v4 state: session, grace, lease, client lifecycle | 3 | 4 | 2 | 1 partial | 10 |
| #2402 | v4 OPEN/LOCK and delegation callbacks | 6 | 1 | — | — | 7 |
| #2403 | v4 COMPOUND, pseudo-fs, attributes | 6 | 3 | — | — | 9 |
| #2404 | v3 handlers: CREATE, attrs/access, namespace, I/O | 9 | — | 1 | — | 10 |
| #2405 | RPC wire, XDR codecs, GSS, transport lifecycle | 10 | 1 | 1 | — | 12 |
| #2406 | NLM/NSM bridge and dispatch tables | 7 | — | — | — | 7 |
| | **Total** | **46** | **9** | **4** | **2** | **61** |

Closed by earlier waves without the issues being updated: #2491, #2529, #2459, #2448/#2466 (#2401),
#2377 (#2402 and #2403), #2481, #2368 (#2403), #2547 (#2405), and the doc half of 2400-5 by #2504.

**"Refuted" is not a verdict class here.** Four LIVE rows carry a refuted *sub-claim* — the finding
stands but part of its stated evidence does not (2406-5, 2404-5, 2401-2, 2405-10, listed under
corrections below). Counting those separately is what made an earlier draft of this table fail to
add up; a refuted rationale does not make a live defect disappear.

## Shipped from this triage

**#2405-5/6 — unbounded pre-auth XDR allocation.** Fixed in PR #2552 (`fix/nfs-rpc-decode-bounds`).
A declared opaque length sized the decode buffer before the bytes it described were read: a 32-byte
call claiming a 64 MiB credential allocated 134 MB, measured. `ReadCall` is the first code to touch
attacker bytes on TCP, UDP and portmap, and runs before any credential is inspected;
`ValidateFragmentSize` caps the record, not the lengths declared inside it. `UnmarshalLimited` caps
each decode at the delivered bytes. The same defect existed at two MOUNT sites the issue did not
name (`mount.go`, `umount.go`) and was fixed there too.

The audit graded this MED reasoning that Go's lazy mapping softens the RSS impact. The measured
allocation is the counter-evidence; it is the only finding in the set reachable with no
authentication at all.

## Proposed fix waves

Groups are file-disjoint and parallelise, except where noted.

### Wave A — security, one PR each

| Finding | What | Size |
| --- | --- | --- |
| 2403-7 | Junction LOOKUP skips the netgroup check PUTFH performs — `putfh.go:85`'s own comment asserts this branch cannot exist | ~8 lines, red test achievable |
| 2400-2 | GSS MNT fails open when `si == nil` instead of denying; twin at `v4/handlers/helpers.go:145` must go in the same diff | ~6 lines × 2 files |
| 2400-4 | A disabled user stays authorized over NFS — `user.Enabled` is never read | 3 lines, red test achievable |
| 2400-3 | A store error on UID lookup falls open to guest, overriding an explicit `none` | ~10 lines |
| 2402-2 | OPEN CLAIM_PREVIOUS does no permission check at all | ~15 lines |

2400-2/-3/-4 share `share_permission.go` and `mount.go`; 2400-4 + 2400-3 are one PR, 2400-1 + 2400-2
another. 2402-2 collides with 2402-1/-3 in `open.go` and must land first.

### Wave B — correctness, cheap and testable

| Finding | What | Size |
| --- | --- | --- |
| 2402-7 | Recall-send failure revokes after 5 s instead of the lease the same file's doc quotes | 6 one-line swaps |
| 2402-5 | Backchannel retry mints a fresh CB_SEQUENCE seqid per attempt | ~10 lines |
| 2404-7 | SETATTR size/other split skips block reclaim when phase 2 fails — permanent per-share leak | ~8 lines |
| 2400-6 | UMNT deletes every mount for a client IP, across shares *and* protocols | ~20 lines, 4 files |
| 2401-4 | `ValidateSequence` returns a live `*Slot` past its own lock — race on the v4.1 hot path | ~15 lines |
| 2406-4 | Blocking LOCK has no retransmit dedup; duplicate waiters pile up | ~15 lines, best-isolated |
| 2403-9 | xattr handlers coarsen auth errors to SERVERFAULT; the mapper already defaults there | 4 lines, 4 files |

### Wave C — needs a decision, not a patch

- **2406-2** NLM LOCK/TEST/UNLOCK grant byte-range locks with no permission check. The
  `AuthContext` fields exist with zero consumers. The fix shape is a policy choice — knfsd requires
  read-or-owner, not write.
- **2400-5** export policy inlined in the RPC handler. Doc half already fixed by #2504; the
  extraction is a new runtime seam with no behaviour delta.
- **2401-10 residue** → #2437, the known v4.0 ceiling. Not a new defect.

### Deferred — pure structure, no test can go red

2402-1 (39 literal result sites), 2402-3, 2401-9, 2404-1/2/4/6, 2404-5, 2404-9, 2403-1, 2403-2,
2403-5, 2405-1/2/3/4/7, 2406-5, 2406-7. Several are large: 2406-7 means rewriting 120 lines of
security-critical auth plumbing for zero functional gain.

`2406-6` (a `doc.go` documenting a package that does not exist) is a two-minute delete — fold it
into whichever PR next touches that package.

## Corrections to carry back into the issues

- **#2404-3 is a duplicate of #2404-1**; **#2405-6 is a duplicate of #2405-5**; **#2401-3 and
  #2401-8 duplicate #2401-1 and #2401-6.**
- **#2404-2's prescription is wrong.** It says delete "the three" `< 8` handle checks; there are
  **sixteen**. Either route all sixteen through one helper or do none.
- **#2406-5's corroborating evidence is refuted.** "READ calls `Release()`, WRITE never does" — 
  `ReadResponse` is the only type in the package with a `Release()` method, so WRITE has nothing to
  leak. Cosmetic duplication, no latent bug.
- **#2404-5's rationale is unearned.** Both copies already map not-found to `NFS3ErrStale`; nothing
  has drifted. Keep it only for the `GetFileForRead` win.
- **#2401-2's sub-claim is stale.** `StartBackchannelSender` does have a production caller
  (`pkg/adapter/nfs/handlers.go:653`).
- **#2405-10's quoted code no longer exists** — #2547 rewrote `udpSidecar.Stop`. The missing *wait*
  is still real; re-read before fixing.
- **#2400/#2406's line numbers are not stale.** Of the 196 commits since the pin, 44 touched the NFS
  adapter and **zero** touched those seven files.

## Seam noted, not fixed

Two v4.1-detection mechanisms coexist: handlers gate on `ctx.SkipOwnerSeqid`
(`v4/handlers/close.go:48`) while the state manager derives v4.1 from the client record via
`sm.v41ClientLocked` (`openowner.go:470`). They agree today only because both read the same
underlying truth. Worth collapsing when 2401-5 is fixed rather than patching around.

# #749 — Replay pending-oplock/lease matrix: scope & plan

**Status:** scoping only (no code written this pass)
**Validation gate:** `smbtorture smb2.replay.*` — requires Linux + Docker + Samba; **cannot run on the current Windows/arm64 host**
**Related open features:** DH V2 (#741), multichannel session binding (#747)

---

## 1. Current state on `develop` (after pulling 505 commits)

The SMB3 replay machinery is already substantially implemented and merged. Do **not** rebase the
stale `feat/smb-*-replay-749` branches — they predate `#866`/`#867` and show ~15k–26k-line deletions
against current `develop` (i.e. develop is *ahead* of them, not behind).

Already merged and relevant:

| PR | What it landed |
|----|----------------|
| `#725` (`#482`) | Base SMB3 replay protection (CREATE + LOCK replay caches) |
| `#864` | Channel-sequence verification (MS-SMB2 §3.3.5.2.10) |
| `#866` | DH2Q durable-V2 create-replay protection (state-restoring replay) |
| `#867` | Walked back 7 CI-confirmed durable-V2 replay row flips |

Key existing code (all on `develop`, all unit-tested):

- `internal/adapter/smb/v2/handlers/replay_cache.go` — `CreateReplayCache` (entries keyed by
  CreateGuid + `reserved` set keyed by `{SessionID, CreateGuid}`) and `LockReplayCache`.
  Already exposes `Reserve` / `Release` / `IsReserved` / `LookupEntry` — the *exact* primitives the
  pending matrix needs.
- `internal/adapter/smb/v2/handlers/channel_sequence.go` (+ root-level) — stale-ChannelSequence →
  `STATUS_FILE_NOT_AVAILABLE` gate.
- `internal/adapter/smb/v2/handlers/create.go:1421-1461` — reserves the DH2Q CreateGuid across the
  **whole** blocked window (both async lease-park and inline no-oplock sharing-violation paths), and
  releases on the right owner (parked resume goroutine vs inline `completeCreateAfterBreak`).
- `internal/adapter/smb/v2/handlers/create.go:1671-1683` — `resolveCreateReplay`: a replay
  (`FLAGS_REPLAY_OPERATION`) that arrives while the GUID is reserved returns
  `STATUS_FILE_NOT_AVAILABLE`; otherwise replays the cached response rebuilt from the live `OpenFile`.
- `internal/adapter/smb/v2/handlers/create_post_break.go:1350-1362` — release-on-completion of the
  parked path.

**Conclusion:** the *mechanism* (reserve-while-parked → FILE_NOT_AVAILABLE on replay; cached-response
replay with live lease/oplock rebuild) exists and passes its Go unit tests. The remaining #749 work is
**correctness tuning of matrix corners**, not green-field implementation.

## 2. Remaining gap (KNOWN_FAILURES.md, ~37 rows)

`test/smb-conformance/smbtorture/KNOWN_FAILURES.md:230-275` — all reference `#749`:

- `smb2.replay.replay-dhv2-oplock2`
- `dhv2-pending{1,2,3}{n,l,o}-vs-{oplock,lease}-{sane,windows}` (the bulk)
- `dhv2-pending1n-vs-violation-lease-{close,ack}-{sane,windows}`

Decoding the names:
- `pending1/2/3` — *when* the original create is parked relative to the conflicting break (timing variant).
- trailing `n`/`l`/`o` — what the **replayed** create requests: **n**one / **l**ease / **o**plock.
- `-vs-oplock` / `-vs-lease` — the type of the **conflicting holder** already on the file.
- `-sane` / `-windows` — two server **policy modes** smbtorture checks: the spec-"sane" reference state
  machine vs the observed Windows-policy behaviour (Samba implements both; they differ in whether/when
  the parked create is granted, broken, or rejected).

The `-windows` variants are the long tail — they encode Windows-specific quirks that diverge from the
clean MS-SMB2 reading, and are the rows most likely to need byte-level pcap diffing against a real
Windows server or Samba reference.

## 3. Why this can't be finished in this environment

1. **No smbtorture here.** The gate is `smb2.replay.*` from Samba's smbtorture, run via
   `test/smb-conformance/run.sh` against a Dockerised DittoFS + Samba reference. That needs
   Linux + Docker + `linux/amd64`. This host is Windows/arm64 — any state-machine change to the
   pending matrix would be **unverifiable** against the actual pass/fail criterion. Writing it blind
   would violate "don't ship code you can't verify."
2. **Cross-feature coupling.** #749 is explicitly "heavily intertwined with DH V2 (#741) and
   multichannel session binding (#747)." Several `-windows` rows only become reachable / correct once
   those land. Sequencing matters.

## 4. Recommended path (when on a Linux/Docker host)

Work the matrix in dependency order, smallest verifiable slice first. Each step is a
park→replay→assert cycle against one smbtorture row family.

1. **Stand up the harness.** `cd test/smb-conformance && ./bootstrap.sh && ./run.sh -- smb2.replay`
   to get a baseline pass/fail snapshot on current `develop`. Confirm which of the 37 rows already
   pass un-tracked (the `#867` walk-back note says `oplock1`/`oplock3` pass but aren't KF rows — the
   real current count may already be < 37).
2. **`pending1n-vs-{oplock,lease}-sane` first.** The "n" (no-lease/oplock requested by the replay) +
   "sane" policy is the cleanest corner and the one the inline-reservation fix at `create.go:1441`
   was aimed at. Verify it flips; if not, the reserve/release window boundaries are the suspect.
3. **`pending1{l,o}-vs-*-sane`.** Lease/oplock-requesting replays — exercises the `LookupEntry`
   live-`OpenFile` lease-rebuild path. Compare against Samba `smb2srv_open_lookup_replay_cache` and
   `source3/smbd/smb2_create.c` lease-rebuild.
4. **`pending2*` / `pending3*` -sane.** Timing variants — likely the same code paths once 1* is
   correct; mostly confirms no ordering regressions.
5. **`-windows` policy variants.** These need the dual-mode policy switch. Decide whether DittoFS
   targets sane-only or also Windows-policy (Samba gates this on `smb2 disable lock sequence checking`
   / server role). **Pcap-diff against a real Windows server or `quay.io/samba/samba` reference** per
   the CLAUDE.md "Debugging protocol interop" playbook — source-reading alone will miss the quirks.
6. **`violation-lease-{close,ack}`.** The break-then-{close,ack} race rows — depends on the lease
   manager's break-ack accounting; revisit after #741/#747 status is known.

Per-row: update `KNOWN_FAILURES.md` only when the row is CI-confirmed (follow the existing
"walk-back" discipline — flip on green CI, never speculatively).

## 5. Suggested issue comment

Post a status note on #749: replay *mechanism* is merged (#725/#864/#866/#867); remaining rows are
matrix-corner correctness gated by smbtorture; stale `feat/smb-*-replay-749` branches should be
closed/deleted (develop is ahead of them); next actionable slice is `pending1n-vs-*-sane` on a
Linux/Docker host. Keep #749 blocked-on/related-to #741 and #747.

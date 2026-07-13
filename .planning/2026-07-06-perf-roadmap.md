# DittoFS performance roadmap (2026-07-06)

Derived from the #1466 real-hardware benchmark (SCW POP2-HC-32C-64G → SCW S3 fr-par, develop `0b2361b8`) and six pprof-verified investigations in `.planning/investigations/` (see `00-HANDOFF.md`). Report: https://claude.ai/code/artifact/afb8557d-1b06-4ab7-b2e8-69b212850323

Each PR is architecture-planned first (`.planning/prs/PRnn-*.md`, competitor-informed) and ships through the standard flow: sim + review → CI green → Copilot → squash-merge to develop, with its before/after bench + pprof captured as merge evidence.

## Shared root cause

3 of 6 findings are one bug — synchronous per-op durability NFS defers to COMMIT: append-log `fsync` per write (write path), append-log `fsync` per payload (metadata write leg — `groupCommit` = one coordinator per open log, never coalesces across files = #1416's "why only 64/s"), and `FlushPendingWriteForFile` + atime per op (SMB). One principle fixes all three: **per-store fsync leader + honor UNSTABLE (fsync only at COMMIT/CLOSE/drain, durable-before-return).**

## Waves

### Wave 1 — Read path (independent, highest ROI, root cause pprof-confirmed)
| PR | Scope | Competitor inspiration | Gate |
|---|---|---|---|
| **PR1** #1572-A | `fmt.Sscanf`→`strconv.Atoi` in 3 `parseBlockIdx` sites | — (mechanical) | read-load pprof: `Fscanf` gone (−~32% read CPU) |
| **PR2** #1572-B | `GetFileChunkAtOffset` (indexed single-Get, `off ≤ T < off+DataSize` covering check); route read + prefetch paths through it | JuiceFS `pkg/meta` slice-by-offset index; S3QL block DB | fio randread 2 → thousands IOPS; `ListFileChunks` out of profile; storetest hole/EOF green (no corruption) |

> PR1 note: swap was mechanical + zero-risk; skipped a new regression test (existing storetest `ListFileChunks_Ordering` already pins the numeric-ordering contract across all 3 backends). Also folded in a sqlite rename `pgParseBlockIdx`→`parseBlockIdx` (copy-paste artifact) per code-simplifier.

### Wave 2 — Durability principle (one root, three wins; PR3 first, then PR4∥PR5)
| PR | Scope | Competitor inspiration | Gate |
|---|---|---|---|
| **PR3** write path | Per-store append-log fsync leader; honor UNSTABLE — fsync at COMMIT/CLOSE/drain | Linux knfsd UNSTABLE+COMMIT; JuiceFS writeback; badger group-commit | seq-write 149 → ~400 MB/s; `syscall.Fsync` collapsed; block-durability suite green |
| **PR4** SMB | Drop per-op flush; lazy/cached atime; remove parent-dir atime | Samba atime/write-behind, oplock semantics | SMB seqR 224→≥450, randW 315→≥800 |
| **PR5** #1573-S4 | Dir-level create batching / lazy parent-mtime → kill same-dir badger SSI conflict | JuiceFS metadata engine batching; avoid parent-inode contention | metadata 168→~1200 ops/s; fsyncs/file 3→1 |

**Wave-2 invariant (blocks PR4/PR5 until PR3 lands it):** every durability point must fsync; crash-loss window stays protocol-legal (write `Verf` already emitted). Block-durability conformance suite must stay green.

### Wave 3 — Upload ceiling + niche (independent)
| PR | Scope | Competitor inspiration | Gate |
|---|---|---|---|
| **PR6** #1466 | S3 multipart on `PutBlock` + carve 16→64 MiB (do NOT touch the adaptive window — 24 is the correct knee) | rclone `backend/s3` multipart (ChunkSize/UploadConcurrency); AWS `feature/s3/manager` | parity upload-large 561→≥900 Mbit/s; pprof c24/c64 = network-wall not CPU-wall |
| **PR7** #1569 | Optional per-share smaller FastCDC floor for random-access shares | restic/JuiceFS chunker params; SeaweedFS | cold randread amplification down; dedup trade-off documented |

## Sequencing
- Wave 1 first (independent, confirmed, unblocks worst gap).
- Wave 2: PR3 → {PR4 ∥ PR5} (shared durability invariant).
- Wave 3 anytime (touches only upload/chunk paths).

## Status
- [x] PR1 · #1572-A strconv — SHIPPED PR#1575 (develop `d3a2683a`); microbench ~21× / 442834→3 allocs
- [ ] PR2 · #1572-B GetFileChunkAtOffset
- [ ] PR3 · write-path fsync leader / honor UNSTABLE
- [ ] PR4 · SMB per-op flush + lazy atime
- [ ] PR5 · #1573 dir-create OCC
- [ ] PR6 · #1466 S3 multipart
- [ ] PR7 · #1569 per-share chunk size

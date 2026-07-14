# Re-profile: group-commit-at-syncIfRelaxed does NOT move the metadata wall (#1573)

Tracking issue for the real wall: **#1687**. Thesis refuted; branch shelved (not merged).

## Same-VM A/B (dittofs-s3-nfs3, metadata workload, 4 threads, 30s, evict-cache=false)

| size | baseline (develop) | treatment (this branch) | delta |
|------|--------------------|-------------------------|-------|
| large  | 254 | 285 | +12% |
| medium | 211 | 251 | +19% |

Within rig noise; far below the plan's 2–4× / clear-ZeroFS-354 bar.

## Mutex profile (treatment binary, mutex rate=1, 4-thread load) — `t-mutex.pb.gz`

- `badger DB.Sync` = **63.8%** of mutex delay, reached via `syncIfRelaxed` (43% WithTransaction, 20% SetRollupOffset). **Unchanged from baseline** → the group-commit leader did not collapse it.
- Contending write frames present: `doWrites` / `ensureRoomForWrite` = 14.45% (Lock side of the same value-log RWMutex).
- **Conclusion:** contention is `DB.Sync` (RLock) vs badger's write pipeline (Lock) — *sync-vs-write*, not *sync-vs-sync*. Coalescing syncs (proven working locally: 32 concurrent → 7 barrier passes) cannot fix a sync-vs-write wall.

## CPU (`t-cpu.pb.gz`) / Block (`t-block.pb.gz`)

- CPU ~0.5 core busy (47% of one core), dominated by `Syscall6` (fsync) + `futex` → blocking-bound.
- Block profile dominated by badger `doWrites` / `Update` + channel sends.

See #1687 for structurally-different directions (batch commits, badger vlog tuning, alternative engine).

# SMB2 vs NFS throughput gap — investigation & fix plan

Box: SCW POP2-HC-32C-64G. Branch: `origin/develop` @ `0b2361b8`. Same DittoFS backend.

| Workload   | SMB2 | NFSv3 | ratio |
|------------|------|-------|-------|
| seq-read   | 224 MB/s | 543 MB/s | 2.4× |
| rand-write | 315 IOPS | 927 IOPS | 2.9× |
| seq-write  | 136 MB/s | 149 MB/s | ~1.1× (close) |

**Not** negotiated I/O size: SMB advertises MaxRead/Write/Transact = 1 MB
(`internal/adapter/smb/handlers/handler.go` ~L904-906), same ballpark as NFS rsize/wsize.
**Not** credits or the per-conn goroutine cap: requests run concurrently
(`pkg/adapter/smb/connection.go:356` — goroutine per request, `requestSem` = 100), and credits
ramp via `echo` strategy to 8192 (`internal/adapter/smb/session/credits.go`,
`pkg/adapter/smb/config.go:316,357`). Neither is the ceiling for these single-mount runs.

The gap is **per-request work the SMB adapter does that the NFS adapter defers or omits.** The
shape of the numbers proves it: seq-write (data-bound, per-op cost amortized over 1 MB) is nearly
equal, while rand-write (per-op cost dominates) is ~3× — exactly the ratio of extra metadata ops.

---

## Root causes (ranked)

### Cause 1 — WRITE flushes metadata durably on every op; NFS defers to COMMIT (dominant, rand-write)

`internal/adapter/smb/handlers/write.go` (Step 11, `Handler.Write`) does, **per WRITE**:

1. `PrepareWrite` (metadata)
2. `WriteToBlockStore` (data)
3. `CommitWrite` (metadata)
4. **`FlushPendingWriteForFile`** — synchronous metadata flush, *every* write. Code comment:
   "SMB has no COMMIT". False premise — SMB has FLUSH (`flush.go`) and flushes at CLOSE.
5. **`SetFileAttributes{Atime}` on the file** — a metadata write
6. **`SetFileAttributes{Atime}` on the parent dir** + `restoreParentDirFrozenTimestamps`
7. `NotifyRegistry.NotifyChange`

NFS v3 WRITE uses `WriteUnstable` (`internal/adapter/nfs/types/constants.go:243`,
`internal/adapter/nfs/v3/handlers/write.go`) — data cached, metadata **not** flushed per op.
`FlushPendingWriteForFile` runs only in the NFS **COMMIT** handler
(`internal/adapter/nfs/v3/handlers/commit.go:214`). So NFS pays ~1 metadata op per write and one
flush per COMMIT (once per file); SMB pays ~3-4 metadata ops **+ a durable flush** on *every*
write. That is the ~3× rand-write gap.

### Cause 2 — READ writes atime metadata on every op; NFS reads touch no metadata (dominant, seq-read)

`internal/adapter/smb/handlers/read.go` (Step 12, `Handler.Read`): after every successful read,

```go
if !openFile.IsAtimeFrozen() {
    now := time.Now()
    _, _ = metaSvc.SetFileAttributes(authCtx, openFile.MetadataHandle, &metadata.SetAttrs{Atime: &now})
}
```

a full metadata-store write **per READ**. NFS v3 `read.go` does **zero** per-read metadata writes
(confirmed: no `SetFileAttributes`/`Atime`/`FlushPending` in the file). Real servers (Samba,
Windows) treat atime like `relatime`/`noatime` — they do not synchronously persist it per read.
This is the seq-read differentiator (metadata-store round-trip serialized into the read hot path).

### Cause 3 — redundant metadata lookups per op (secondary, both paths)

READ calls both `GetFile` **and** `PrepareRead` (two metadata fetches) plus `CheckLockForIO`.
WRITE adds a `GetFile` "preWriteMtime" probe (Step 9) on top of `PrepareWrite`/`CommitWrite`.
Each is a store round-trip on the hot path. NFS collapses more of this into a single call.

### Not the cause (checked, deprioritised)

- **Signing** (`internal/adapter/smb/signing/*`): CMAC/GMAC is per-message CPU, symmetric and cheap
  on a 32C box for a single stream; would not produce a 2-3× gap while seq-write stays equal.
- **CLOSE synchronous flush** (`close.go:223,246` — `CommitBlockStore` + `FlushPendingWriteForFile`):
  real, but it mirrors NFS COMMIT (once per file), so it is **not** a streaming-throughput
  differentiator. It only bites open→write→close small-file loops, where it's ~equal to NFS's
  close-time COMMIT. Leave as-is; revisit only if the small-file benchmark shows it.
- **Compound / per-op serialization**: compounds run sequentially inside `ProcessCompoundRequest`
  but standalone READ/WRITE do not compound; not implicated here.

---

## Fixes (ranked by expected win)

### Fix A — treat SMB WRITE as unstable; stop per-op metadata flush (biggest win: rand-write → ~NFS)

In `write.go Handler.Write`, **remove the per-write `FlushPendingWriteForFile`** and let durability
land at SMB **FLUSH** (`flush.go`) and **CLOSE** (`close.go` already flushes) — the exact contract
NFS uses (UNSTABLE + COMMIT). `CommitWrite` still updates in-memory size/mtime so QUERY_INFO stays
correct; only the durable flush moves to FLUSH/CLOSE. Symmetric crash-durability window with NFS,
which is already the accepted trade-off. (Related lever: #1416 batching.)

Expected: rand-write 315 → ~800-900 IOPS.

### Fix B — make atime lazy on READ and WRITE (biggest win: seq-read → ~NFS)

Stop the synchronous `SetFileAttributes{Atime}` store write in the READ and WRITE hot paths. Options,
laziest first:
- **B1 (lazy/relatime):** update a cached atime on `OpenFile` in memory (`recordReadProgress`
  already mutates `OpenFile` under no store I/O — piggyback atime there) and persist it once at
  CLOSE (fold into the existing close flush). QUERY_INFO/GetInfo reads the cached value, so
  `smb2` atime assertions still pass.
- **B2 (config knob, if B1 is too much):** gate the per-op atime write behind a `relatime`-style
  setting, default off. Match NFS behaviour by default.

Also drop the **parent-dir** atime write in WRITE (Cause 1 step 6) — NFS does not do it per write.

Expected: seq-read 224 → ~450-540 MB/s.

### Fix C — collapse redundant metadata reads (secondary)

READ: fold `GetFile` + `PrepareRead` into one call (symlink-type is already available from
`PrepareRead`'s attrs, or add type to its return) so the hot path is one store fetch + one lock
check. WRITE: drop the `preWriteMtime` `GetFile` probe when delayed-write is already triggered
(the guard `probeArm` mostly avoids it, but confirm it's skipped after the first write). Small,
do after A/B and only if the profile still shows metadata-read time.

---

## Implementation plan

| # | File / function | Change |
|---|-----------------|--------|
| A | `internal/adapter/smb/handlers/write.go` → `Handler.Write` (Step 11) | Delete the per-op `FlushPendingWriteForFile` call. Keep `CommitWrite`. Verify `flush.go`/`close.go` cover durability. |
| B1 | `internal/adapter/smb/handlers/read.go` → `Handler.Read` (Step 12) & `recordReadProgress` | Replace store `SetFileAttributes{Atime}` with an in-memory cached-atime bump on `OpenFile`; persist at CLOSE. |
| B1 | `internal/adapter/smb/handlers/write.go` → `Handler.Write` (post-Step 11) | Same for the file atime; **remove** the parent-dir atime write per write. |
| B1 | `internal/adapter/smb/handlers/close.go` (~L237-246 flush block) | Flush cached atime alongside the existing `FlushPendingWriteForFile`. |
| B1 | `internal/adapter/smb/handlers/query_info.go` | Ensure GetInfo returns cached atime so `smb2.read.position` / atime torture tests still pass. |
| C | `read.go` / `write.go` | Collapse `GetFile`+`Prepare*` duplicate fetches (optional, post-A/B). |

Gate walk: after edits run `go test ./internal/adapter/smb/...` (atime, read.position, durable-open,
flush, notify torture suites), then `go vet ./...` and `gofmt -s`. KNOWN_FAILURES untouched until CI.

---

## BEFORE/AFTER benchmark + pprof

**Goal:** close SMB2 seq-read to ≥450 MB/s and rand-write to ≥800 IOPS (NFS = 543 / 927), seq-write
stays ≥136.

### Setup (on the SCW box)

Build + run the server with pprof on (`pkg/controlplane/api/config.go:72` `Pprof: true`, exposed at
`/debug/pprof/*` via `pkg/controlplane/api/router.go`):

```bash
go build -o dfs cmd/dfs/main.go && go build -o dfsctl cmd/dfsctl/main.go
DITTOFS_LOGGING_LEVEL=INFO DITTOFS_CONTROLPLANE_API_PPROF=true ./dfs start   # confirm env key vs config.yaml api.pprof:true
```

Mount both protocols against the **same share / same data set**:

```bash
sudo mount -t nfs  -o vers=3,nolock,rsize=1048576,wsize=1048576 127.0.0.1:/<share> /mnt/nfs   # port 12049
sudo mount -t cifs //127.0.0.1/<share> /mnt/smb -o vers=3.0,port=12445,cache=none,username=...,password=...
```

### Measure — `dfsctl bench` (identical config both mounts)

`dfsctl bench` reports `throughput` (MB/s), `iops`, `ops/sec`, `p50/p95` (`cmd/dfsctl/commands/bench/table.go`).
Point it at each mount path and run the same three workloads:

```bash
# SMB
dfsctl bench run --path /mnt/smb --size 1G --block 1m --workload seq-read   --json > smb-seqread.json
dfsctl bench run --path /mnt/smb --size 1G --block 4k --workload rand-write --json > smb-randwrite.json
dfsctl bench run --path /mnt/smb --size 1G --block 1m --workload seq-write  --json > smb-seqwrite.json
# NFS (baseline / target)
dfsctl bench run --path /mnt/nfs --size 1G --block 1m --workload seq-read   --json > nfs-seqread.json
dfsctl bench run --path /mnt/nfs --size 1G --block 4k --workload rand-write --json > nfs-randwrite.json
dfsctl bench run --path /mnt/nfs --size 1G --block 1m --workload seq-write  --json > nfs-seqwrite.json
dfsctl bench compare smb-*.json nfs-*.json        # side-by-side table
```

(Confirm exact `bench run` flag names against `cmd/dfsctl/commands/bench/run.go` — use
`dfsctl bench run --help`.) Metric of record: **MB/s** (seq-read/seq-write) and **IOPS** (rand-write).

### Localise the overhead — CPU profile under SMB load

While an SMB seq-read (then rand-write) run is in flight, capture a 30 s CPU profile:

```bash
go tool pprof -http=:0 http://127.0.0.1:<api-port>/debug/pprof/profile?seconds=30
# expect BEFORE: SetFileAttributes / FlushPendingWriteForFile / metadata-store commit dominating
# the SMB READ/WRITE stacks. AFTER Fix A+B those frames should collapse out of the hot path.
go tool pprof http://127.0.0.1:<api-port>/debug/pprof/mutex   # confirm no store-lock contention remains
```

### Pass criteria

- BEFORE profile shows `SetFileAttributes` (atime) on the READ stack and
  `FlushPendingWriteForFile` on the WRITE stack as top self-time frames.
- AFTER Fix A+B: those frames gone from the hot path; SMB seq-read ≥ 450 MB/s, rand-write ≥ 800
  IOPS, seq-write unchanged (≥136). Re-run `dfsctl bench compare` to confirm the gap closed to
  within ~15% of NFS.

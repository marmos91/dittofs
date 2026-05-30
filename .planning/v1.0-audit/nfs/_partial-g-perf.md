# NFS adapter — performance audit (partial G)

Branch `v1.0/nfs-audit`. Captured 2026-05-30. Host: Apple M1 Max,
darwin/arm64, Go 1.26.3.

All `ns/op` / `B/op` / `allocs/op` below are from **fresh local runs**
this session. Profiles are committed under `_prof/`. Static hot-path
analysis is from source reads with file:line.

---

## Benchmarks found & run

### Found (exhaustive grep)

`grep -rn "func Benchmark" internal/adapter/nfs pkg/adapter/nfs` returns
**three benchmarks, all in one file**:

- `internal/adapter/nfs/v4/handlers/sequence_handler_test.go`
  - `BenchmarkSequenceValidation` (line 771)
  - `BenchmarkCompoundDispatch` (line 798) — v4.1 COMPOUND w/ SEQUENCE
  - `BenchmarkCompoundDispatch_V40` (line 825) — v4.0 COMPOUND (no SEQUENCE)

That is the **entire** benchmark coverage of the NFS adapter. There is
**zero** benchmark for the per-RPC primitives that actually dominate a
real workload:

- no XDR decode/encode benchmark (`internal/adapter/nfs/xdr` has
  `xdr_test.go` + `time_test.go`, no `Benchmark*`),
- no v3 handler benchmark (READ/WRITE/LOOKUP/READDIRPLUS/GETATTR all
  have `*_test.go`, **no** `Benchmark*`),
- no auth-context build benchmark,
- no dispatch-routing benchmark,
- no WCC-capture benchmark.

### Run — real numbers (`-benchmem -count=3`)

```
goos: darwin  goarch: arm64  cpu: Apple M1 Max
pkg: .../internal/adapter/nfs/v4/handlers
BenchmarkSequenceValidation-10     ~895k    1288 ns/op    872 B/op    50 allocs/op
BenchmarkCompoundDispatch-10       ~785k    1556 ns/op   1208 B/op    62 allocs/op
BenchmarkCompoundDispatch_V40-10  ~2490k     483 ns/op    432 B/op    25 allocs/op
```

(Three runs each; medians shown — variance < 2%.)

**Read these numbers as a per-RPC allocation indictment, not a latency
result:**

- A *single SEQUENCE op* allocates **872 B / 50 allocs**.
- A *single v4.1 COMPOUND* (SEQUENCE + 1 op) allocates **1208 B / 62
  allocs** — i.e. ~62 heap allocations to process **one** RPC before any
  real filesystem work happens.
- The v4.0 path (no slot table / SEQUENCE) is **3.2× faster and 2.5×
  fewer allocs** (483 ns / 25 allocs) — the v4.1 SEQUENCE/slot-table
  machinery roughly **triples** the per-RPC cost. That delta is pure
  adapter overhead.

These exercise only the SEQUENCE / COMPOUND-dispatch path (no metadata
store, no blockstore), so the ns/op is the *floor* — real RPCs add the
store round-trip on top.

---

## Profiles captured

Under `.planning/v1.0-audit/nfs/_prof/`:

- `seq-cpu.pprof` — 14.21 s CPU, 13.06 s samples (91.9%).
- `seq-mem.pprof` — alloc profile, 5215 MB total alloc over the run.

Reproduce:

```bash
go test -run='^$' -bench=. -benchmem -count=3 \
  -cpuprofile=.planning/v1.0-audit/nfs/_prof/seq-cpu.pprof \
  -memprofile=.planning/v1.0-audit/nfs/_prof/seq-mem.pprof \
  ./internal/adapter/nfs/v4/handlers/
```

### CPU top (flat, 14.62 s total)

```
25.85%  runtime.kevent              ← netpoll/scheduler idle (bench is alloc-bound)
10.74%  runtime.pthread_cond_wait
 8.82%  runtime.madvise             ← GC returning memory to OS
 7.46%  runtime.pthread_cond_signal
 3.28%  runtime.mallocgc  (cum 12.45%)  ← allocation churn
 ...
 9.37%  encoding/binary.Write (cum) ← BUT see caveat: mostly test harness
 7.73%  runtime.makeslice (cum)
```

**The hot path is allocation-bound, not compute-bound** — `mallocgc`
+ `madvise` + `makeslice` dominate the non-idle CPU, all GC pressure
from the 62-allocs/op figure. (`slog` INFO logging also fires per
session — visible flooding the bench stdout; should be DEBUG-gated.)

> **Test-harness caveat (corrected after reading source).** A large
> share of the allocation in this benchmark is the **request builder in
> the test**, not production code. `buildCompoundArgs`,
> `encodeSequenceArgs`, `handlePutRootFH` are **test-only** helpers
> (in `*_test.go`) that build the input COMPOUND via `encoding/binary.Write`
> + `bytes.NewReader` each iteration. So the `encoding/binary.Write`
> (3.32 GB cum) / `bytes.NewReader` (0.73 GB) / `encoding/binary.Read`
> (0.39 GB) frames are **mostly the harness**, and production uses
> `binary.Write` in exactly **one** file (`v4/types/session_common.go`).
> The production *encode* methods (`SequenceRes.Encode`,
> `encodeCompoundResponse`) are **hand-rolled** `xdr.WriteUint32` into a
> `bytes.Buffer` — *not* reflective. The real production allocation
> signal is therefore the **unsized `bytes.Buffer`** (`bytes.Buffer.grow`
> 2.92 GB cum, 27% flat) feeding both the production encoder and the
> harness, not reflection.

### Mem top (alloc_space, 9.78 GB total over the run)

```
27.27% 2.92GB  bytes.(*Buffer).grow            ← UNSIZED buffer growth (prod + harness)
22.78% 2.23GB  v41/handlers.HandleSequenceOp (cum)
33.96% 3.32GB  encoding/binary.Write (cum)     ← mostly TEST harness (see caveat)
 7.47% 0.73GB  bytes.NewReader                 ← test harness
22.35% 2.19GB  v4/handlers.encodeCompoundResponse (cum)  ← PRODUCTION encode
 6.06% 0.59GB  v4/handlers.handlePutRootFH     ← test stub
 5.42% 0.53GB  v4/types.(*SequenceRes).Encode (cum)      ← PRODUCTION encode
 2.47% 0.24GB  xdr/core.DecodeOpaque (cum)     ← PRODUCTION decode
 2.39% 0.23GB  state.StateManager.RenewV41Lease (cum)
```

Production-attributable allocation, ranked:

1. **Unsized `bytes.Buffer` growth — `bytes.Buffer.grow` 2.92 GB cum
   (27% flat).** Both `encodeCompoundResponse` (`compound.go:600`
   `var buf bytes.Buffer` — never `Grow(n)`'d) and every per-op
   `*.Encode(buf)` write into a zero-cap buffer that re-allocates as it
   grows. This is the **single largest production allocation source**
   and it is a pure pre-sizing fix.
2. **`encodeCompoundResponse` 2.19 GB cum** — the production reply
   encoder; its allocation is the buffer growth in #1 plus the final
   `bytes.Buffer.Bytes()` copy out.
3. **`SequenceRes.Encode` 0.53 GB cum** — production, hand-rolled, but
   still writes into the unsized shared buffer.
4. **`DecodeOpaque` 0.24 GB + `RenewV41Lease` 0.23 GB** — per-op decode
   scratch and per-SEQUENCE lease bookkeeping.

---

## Hot-path static analysis (ranked)

Path: `accept → read RPC fragment → XDR decode → dispatch →
buildAuthContext → handler → metadata/blockstore → XDR encode → write`.

### 1. Unsized `bytes.Buffer` in the encode path = the allocation hot spot — HIGH (MEASURED)

`bytes.Buffer.grow` is **27% flat / 2.92 GB cum** — the single largest
allocation source in the profile. The production reply encoder
`encodeCompoundResponse` (`v4/handlers/compound.go:600`) does
`var buf bytes.Buffer` with **no `Grow(n)`**, then every per-op
`*.Encode(buf)` appends into it, forcing repeated reallocation as the
buffer doubles, followed by a `Bytes()` copy out. The encoders
themselves are correctly hand-rolled (`SequenceRes.Encode` uses
`xdr.WriteUint32`, not reflection) — the waste is purely the
**un-presized backing array**.

- `internal/adapter/nfs/v4/handlers/compound.go:600`
  (`encodeCompoundResponse`, `var buf bytes.Buffer`).
- `internal/adapter/nfs/v4/types/sequence.go:119` and every sibling
  `*.Encode(buf *bytes.Buffer)`.

**Fix the redesign enables:** `buf.Grow(estimatedSize)` (the encoded
length of a COMPOUND reply is cheaply computable from the result set),
or encode into a pooled `[]byte` via `binary.BigEndian.PutUint32`. This
is the single biggest **measured** production win and it directly cuts
the `mallocgc`/`madvise` CPU.

> Note: do **not** chase the `encoding/binary.Write` (3.32 GB) frame as
> a production fix — it is overwhelmingly the test harness
> (`buildCompoundArgs`/`encodeSequenceArgs` in `*_test.go`). Production
> uses `binary.Write` in one place only (`v4/types/session_common.go`).
> The honest production target is the unsized buffer above.

### 2. Auth-context rebuilt UNCACHED on 7 of 10 v3 ops — HIGH (CONFIRMED by grep)

Confirmed call-site split (`grep -rln` over `v3/handlers/`):

- **Cached** (`GetCachedAuthContext`, defined `v3/handlers/doc.go:44`,
  which wraps `BuildAuthContextWithMapping` behind a cache):
  **getattr, read, write** — 3 ops (`getattr.go`, `read.go`,
  `write.go:197`; create.go:332 also touches it).
- **Uncached** (`BuildAuthContextWithMapping`, defined
  `v3/handlers/auth_helper.go:52`, called directly): **access
  (access.go:157), commit (commit.go:243), link (link.go:138), lookup
  (lookup.go:175), readdir (readdir.go:185), readdirplus
  (readdirplus.go:235), readlink (readlink.go:109)** — 7 ops.

Every uncached build does, per RPC: a share lookup + `ApplyIdentityMapping`
(export squash/mapping, `auth_helper.go:96`) + identity-store resolution.
**LOOKUP and READDIR/READDIRPLUS — the highest-frequency ops in any real
directory walk — are in the uncached set.** v4 mirrors this:
`buildV4AuthContext` (`v4/handlers/helpers.go:24`) rebuilds per op inside
every COMPOUND.

- v3: `internal/adapter/nfs/v3/handlers/doc.go:44` (`GetCachedAuthContext`)
  vs `auth_helper.go:52` (`BuildAuthContextWithMapping`, called directly
  by the 7 uncached ops above).
- v4: `internal/adapter/nfs/v4/handlers/helpers.go:24`
  (`buildV4AuthContext` — uncached, rebuilt per op inside every COMPOUND).

(Already flagged in `REVIEW.md` §3 residual — promoted to HIGH here on
perf grounds. The squash *correctness* is fine; the *redundant rebuild*
is the perf bug.)

### 3. READDIRPLUS per-entry GetFile/Lookup fan-out — HIGH (O(n) round-trips)

READDIRPLUS exists to return attrs+handle inline so the client skips a
LOOKUP per entry — but the handler does a per-entry `Lookup`/`GetFile`
against the metadata store (`readdirplus.go:191/326/341`, cited in
REVIEW.md §4), reintroducing exactly the N serialized round-trips it is
meant to eliminate. Scales with directory size; for a 4096-entry dir
that is ~4095 store round-trips inside one RPC.

### 4. Per-SEQUENCE lease bookkeeping + decode scratch — MED (MEASURED)

`RenewV41Lease` (0.23 GB cum) runs per SEQUENCE and allocates;
`DecodeOpaque` (0.24 GB cum, `xdr/core`) allocates fresh scratch per
opaque field decoded. `hex.EncodeToString` (0.25 GB) + `fmt.Sprintf`
(0.22 GB) appear too — these are session-ID/handle stringification,
likely from the per-session slog INFO lines (#5) and debug formatting,
not strictly required on the hot path.

- `internal/adapter/nfs/v4/state` (`StateManager.RenewV41Lease`);
  `internal/adapter/nfs/xdr/core` (`DecodeOpaque`).

### 5. slog INFO logging on the connect/session hot path — MED

`log/slog.(*Logger).log` appears in the CPU flat profile, and the bench
stdout is flooded with INFO lines for every EXCHANGE_ID / CREATE_SESSION.
Structured logging at INFO on per-RPC/per-session events is avoidable
overhead; should be DEBUG-gated.

### 6. Per-connection 1.25 MB fragment buffer × unbounded connections — MED

`MaxConnections` defaults to 0 / unlimited (REVIEW.md H13,
`pkg/adapter/nfs/adapter.go:236`); each connection holds a buffer sized
to the ~1.25 MB fragment cap. Memory cost + DoS. Bound connections and
pool the large fragment buffers.

### 7. Lock contention — UNMEASURED

`dfs` never calls `runtime.SetMutexProfileFraction` /
`SetBlockProfileRate` (baseline note B3), so the v4 state-machine
mutexes (client/session/slot-table/lease under `v4/state/`) and the
auth cache have **no contention data**. This is itself a finding: NFS
lock contention is currently unprofileable. Wire these to the pprof
flag before claiming any lock win.

---

## Benchmark gaps (micro-benchmarks that should exist)

The adapter has 3 benchmarks, all on the v4.1 COMPOUND/SEQUENCE path.
Missing, in priority order:

1. **`BenchmarkXDREncode_*` / `BenchmarkXDRDecode_*`** for the common
   reply/arg types (READDIRPLUS reply, fattr3/fattr4, LOOKUP args,
   COMPOUND). Directly tracks finding #1 (the 50%+ allocation source).
2. **`BenchmarkBuildAuthContext_Uncached` vs `_Cached`** — puts ns +
   allocs on finding #2 (justifies caching the other 7 v3 ops + v4).
3. **`BenchmarkReaddirplus_NEntries`** (N = 16/256/4096, mem store) —
   quantifies finding #3's O(n) store fan-out.
4. **`BenchmarkV3Dispatch_Route`** and a v3 handler-level READ/WRITE
   bench (data-path alloc per op, rsize/wsize buffer).
5. **`BenchmarkWCCCapture`** — the pre-op attr capture on mutating ops.

All in-process; no kernel client / sudo. They belong in CI.

---

## Perf wins the redesign enables (quantified)

1. **Pre-size (or pool) the encode `bytes.Buffer`.** MEASURED: unsized
   `bytes.Buffer.grow` is 2.92 GB cum / 27% — the largest single
   allocation source. `encodeCompoundResponse` (`compound.go:600`) and
   every `*.Encode(buf)` share a zero-cap buffer that reallocates as it
   grows. Adding `buf.Grow(estimatedSize)` (or a pooled `[]byte` encoder)
   directly cuts the `mallocgc`/`madvise` CPU. Single highest-value,
   profile-backed change.

2. **Cache auth-context per (connection, auth-flavor) for the 7 uncached
   v3 ops + the v4 per-op rebuild.** CONFIRMED: today only
   getattr/read/write cache; access/commit/link/lookup/readdir/
   readdirplus/readlink each pay 1 share-lookup + 1 ApplyIdentityMapping
   + 1 identity-store resolve **per RPC**. LOOKUP + READDIRPLUS (the
   hottest metadata ops) are in the uncached set, so a directory walk
   pays this on every step. Magnitude needs gap-bench #2, but it is
   structurally 3 store/map operations removed from the hottest RPC
   class.

3. **Batch READDIRPLUS entry resolution → N round-trips collapse to 1.**
   Replace the per-entry `Lookup`/`GetFile` loop with a batched
   `GetFiles` (or attrs-in-listing). For a 4096-entry directory that is
   ~4095 serialized store round-trips removed per READDIRPLUS.

4. **Trim per-SEQUENCE allocations: pooled decode scratch in
   `DecodeOpaque`, and drop the `hex.EncodeToString`/`fmt.Sprintf`
   session-ID stringification off the hot path** (it feeds the slog INFO
   lines). MEASURED ~0.7 GB combined.

5. **(Enablers)** DEBUG-gate the per-session slog INFO lines (finding
   #5); bound `MaxConnections` + pool fragment buffers (finding #6);
   wire mutex/block profiling so contention (finding #7) becomes
   measurable before/after.

### Honesty about what is measured vs inferred

- Findings #1, #4, #5 and win #1, #4 are **measured** from the committed
  CPU + mem profiles (numbers above). Note the harness caveat: the
  `encoding/binary.Write` frames are mostly test-side; the honest
  production allocation target is the unsized `bytes.Buffer`.
- Finding #2 / win #2 (auth-context caching split) is **confirmed by
  grep** of the exact call sites — but the ns/op cost of one uncached
  build is **not** isolated (no benchmark exists; that is gap #2).
- Finding #3 / win #3 (READDIRPLUS fan-out) is **confirmed by source**
  (REVIEW.md §4 line cites) — magnitude needs gap-bench #3.
- The only adapter-isolated profiles that exist are on the v4.1
  SEQUENCE/COMPOUND path; the v3 data-path (READ/WRITE/LOOKUP) has **no**
  benchmark or isolated profile — its allocation behavior is inferred
  from the shared XDR-encode pattern, not measured. Closing gap-benches
  #1–#5 is the prerequisite to quantifying the v3 wins.
- Lock contention (#7) is **unmeasured** by construction (profiling not
  wired).

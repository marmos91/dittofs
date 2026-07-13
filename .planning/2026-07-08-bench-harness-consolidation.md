# Plan: Consolidate benchmarking into ONE fio-based competitor harness + relocate component benches

## Context

Benchmarking in this repo is a mess of **three disjoint harnesses plus a byte-for-byte
duplicated infra tree**, with two drifted competitor source-of-truths and staged-but-never-run
fio files:

- **Harness A — mounted-FS competitor comparison** (`pkg/bench/` Go workloads + `bench/scripts/run-all.sh` + `bench/infra/scripts/*.sh`): the one we actually want, but it runs its **own Go re-implementation of fio** (real syscalls in `pkg/bench/workload_*.go`), never invokes `fio` (installed but unused), has no s3fs, uses `rclone serve nfs` instead of `rclone mount` (FUSE), and has **no crash-resume**.
- **Harness B — in-process blockstore orchestrator** (`bench/orchestrator/` + `bench/remote/` + `cmd/bench/{orchestrate,remote}.go`): SSH-runs in-process manifests. Obviated by plain `go test -bench ./pkg/...`.
- **Harness C — in-process engine parity** (`bench/parity/` + `dfsbench parity`, dittofs-vs-rclone, no mount): what I built recently.
- **Duplication**: repo-root `infra/` duplicates `bench/infra/` (own `go.mod`/Pulumi); competitor lists drifted between `run-all.sh` `ALL_SYSTEMS` (16 entries) and `bench/infra/systems.go` `allSystems` (10, stale).

**Goal (user):** collapse all of this into **ONE fio-based, Go-orchestrated, single-disposable-SCW-VM
comparison harness** that measures DittoFS against real competitors with **standard fio metrics
across the board**, plus a uniform **real server↔S3 bandwidth** number. Reliable (resume on crash,
partial results saved), extensible (add a competitor = 1 registry line + 1 setup script), easy to use
(one command provisions → runs → gathers → prints a clean comparison table).

**Hypothesis under test (measure, don't assume):** DittoFS's native userspace NFS/SMB server should
beat the FUSE competitors *when they're re-exported over the same protocol*, by cutting the FUSE
context-switch tax. So the harness must capture **server-side context-switch + CPU evidence** per cell
and **pair native-vs-re-export cells** at the same protocol/workload — to show the delta *and its cause*
(structure over point-IOPS; "see it before partying").

**User directives baked in:**
- **fio is THE load generator** for every system → retire the Go-native `pkg/bench` workloads.
- **Component microbenchmarks live in their home Go package** (chunker/blockstore/engine/metadata),
  run via `go test -bench`. `bench/` holds *only* the cross-system harness.
- **Single disposable SCW VM** via `scw` CLI (reuse the validated `parity-scw.sh` robustness), not Pulumi.
- **Go orchestrator** in `cmd/bench` (fill the `e2e` stub), not bash/Python.
- **xfstests is out of scope here** — it belongs next to pjdfstest (`test/posix/`) as a correctness
  suite; open a tracking issue to evaluate it there.

## Systems under comparison — Backend × Protocol matrix (single Go registry)

Two axes, so we compare **both technologies and access protocols** (mount each tech over NFSv3, NFSv4.1, SMB3).

**Backends (what stores the bytes):**
- DittoFS badger+S3 (subject); DittoFS badger+local-fs (no-S3 control).
- S3-backed competitors (all FUSE): `rclone mount` (**FUSE**, switch from serve-nfs), `s3ql`, `juicefs`, **`s3fs`** (new script).
- Local-disk control (no S3 — the ceiling).

**Access protocols (how the client mounts):** `nfs3`, `nfs4.1`, `smb3`.

**Capability map — how each backend is exported over a protocol:**
- **DittoFS** serves nfs3 / nfs4 / smb3 **natively** (its design point) — no re-export layer.
- **FUSE backends** (rclone/s3ql/juicefs/s3fs) are **re-exported**: nfs3/nfs4 via **knfsd**, smb3 via **Samba** (harness already does this for juicefs/s3ql). Direct-FUSE (no network protocol) is also captured as each FUSE tech's ceiling baseline.
- **Local-disk control** exported the same way → these ARE the kernel-NFS / NFS-Ganesha (nfs3/nfs4) and kernel-SMB/Samba (smb3) protocol-overhead baselines, zero S3.

Concrete runnable cells = `(backend, protocol)`, named `<backend>-<protocol>` (`dittofs-s3-nfs3`, `juicefs-smb3`, `s3fs-nfs4`, …). The registry carries a capability map marking each combo **native / re-export / N/A** so invalid combos auto-skip.

**Fairness note (honest comparison):** FUSE techs pay a re-export tax (knfsd/Samba over FUSE) that DittoFS's native server does not — the table **flags native-vs-re-export per cell** so cross-tech protocol comparisons aren't misread. (That tax is the realistic cost of network-serving juicefs/s3ql anyway.) Consistent with the "bench rig lies — trust structure, label confounds" discipline.

## Target layout

**Pure Go — no shell/`.conf` files in the repo.** Go still `exec`s the unavoidable external CLIs on
the VM (`scw`, `ssh`/`scp`, `fio`, `mount`, `apt-get`, `docker`), but *all* orchestration, per-competitor
install/start/stop/evict recipes, config generation (`text/template` + `go:embed`), eviction, and
reporting live in Go — one source of truth (kills the `run-all.sh`↔`systems.go` drift we found).

```
bench/                          # data-only assets (no shell)
  workloads/*.fio               # fio job files — go:embed'd into the binary, materialized on the VM
  README.md
cmd/bench/                      # the `dfsbench` binary — the WHOLE harness, pure Go
  main.go
  compare.go / baseline.go / report.go / result.go   # run/baseline/report + scorecard schema
  backends.go                   # Backend registry: Install/Start/Stop/Evict/Mount per tech (recipes in Go)
  configs/*.tmpl (go:embed)     # ganesha.conf / smb.conf / rclone.conf / s3ql authinfo — rendered by Go
  exec.go                       # SSH/scp executor (salvaged from bench/remote/exec.go)
  scw.go                        # provisioning — shells the `scw` CLI from Go
  fio.go / s3meter.go / instrument.go
pkg/block/chunker|engine, pkg/metadata, ...   # component Benchmark* funcs live here
```

## Work

### Phase 0 — tracking issue (first, on execution)
Open a GH issue (`--assignee marmos91`): "Evaluate xfstests (generic/ + fsstress/fsx) alongside
pjdfstest in `test/posix/` for broader FS-correctness/crash-consistency coverage." Note the
complementarity (pjdfstest = POSIX syscall semantics; xfstests = broader generic/ + stress). Out of
scope for the perf harness; tracked for the correctness suite.

### Phase 1 — the fio comparison harness (core deliverable)
Fill `cmd/bench`'s `e2e` stub into the single `dfsbench compare` command (Go):
1. **Systems registry** (`cmd/bench/systems.go`): `Backend{Name, SetupScript, S3Backed}` × `Protocol{nfs3,nfs4,smb3}` with a capability map (native / re-export / N/A). Extension point = append one backend entry + drop `bench/scripts/systems/<name>.sh` implementing `start`/`stop`/`evict`; protocols come free via shared knfsd/Samba re-export helpers.
2. **fio driver** (`fio.go`): run each `bench/workloads/*.fio` against `/mnt/bench` with `--output-format=json`; parse with our own `encoding/json` struct (no canonical fio Go lib exists). Matrix covers all access patterns × sizes (64 KiB → 1 GiB) via the existing fio env vars. Sits behind a small `LoadGen` interface so **elbencho** could swap in later (file:// + s3:// in one tool) without touching the orchestrator.
3. **Result schema** (`result.go`): reuse the shape of `pkg/bench/types.go` `WorkloadResult` (throughput_mbps, iops, ops_per_sec, latency_p50/p95/p99/avg_us, total_ops/bytes, errors) + **`s3_bytes`** (server↔S3) — but **populated from fio JSON + the S3 meter**, not the retired Go engine.
4. **Uniform S3 bandwidth** (`s3meter.go`): snapshot OS network counters to the S3 endpoint (nftables counter on endpoint IP:443, or `/proc/net/dev`/`nstat` delta) before/after each cell → real bytes moved, **uniform for every system** (dittofs + juicefs/s3ql/rclone/s3fs; pure-FS controls read ~0). Cross-check DittoFS against its Prometheus `dittofs_datapath_*` counters.
5. **Reliability**: write each `(system × workload × size)` result JSON **immediately on completion**; skip cells whose result already exists → `--resume` re-runs are idempotent (borrowed from xfstests' per-test-result model). A run manifest tracks progress; partial runs still emit a table.
6. **Report** (`report.go`): join fio JSON + s3_bytes + server-resource counters → one clean markdown+CSV comparison table, all systems side-by-side, best-per-metric highlighted (reimplements `bench/analysis/report.sh` in Go — no jq/bash). Includes a **native-vs-re-export pairing view**: same protocol+workload, DittoFS-native vs each FUSE-over-knfsd, with **context-switch + CPU columns beside throughput**, so the architectural delta and its cause are legible (the FUSE-tax thesis).
7. **Per-cell cycle** (reuse run-all.sh logic, in Go): setup → wait port → mount (loopback) → **warm fio** → **force cold** (per-system `evict` hook + drop client+server OS page cache) → **cold fio** (read workloads, true from-S3) → capture s3_bytes → unmount → `stop`.
8. **Baselines / reference ceilings** (`baseline.go`, run before the FS matrix unless `--skip-baseline`): (a) **raw S3** PUT/GET throughput + latency straight at the bucket, no FS layer, via **MinIO warp** (JSON out, `--autoterm`, obj/s + MiB/s + pctiles) — a purpose-built S3 bench, not hand-rolled; (b) **local disk** bandwidth + IOPS via fio against the VM's scratch volume. Emitted at the top of the scorecard so every FS number reads as a fraction of its ceiling (S3-backed → % of raw-S3; controls → % of local-disk). These are the "is the bottleneck the FS or the pipe?" anchors.
9. **DittoFS observability — prod binary, opt-in instrumentation** (`instrument.go`): benchmark the **release build** with DittoFS's **existing production** instrumentation *enabled via config* (no debug/instrumented build that would skew results): Prometheus `dittofs_datapath_*` (via `metrics.enabled` / `DITTOFS_METRICS_*`) + on-demand **pprof**. Per DittoFS cell, snapshot the full datapath metric set into the result JSON (upload window / queue-depth / goodput / inflight, `block_range_read_bytes`, upload duration, evictions, GC) and capture a CPU pprof as an artifact — so an off-looking number is read as **structure**, not rig IOPS (standing discipline). DittoFS-only (competitors have no equivalent; the subject gets the deep view). No new DittoFS code unless a specific blind spot appears — then one low-overhead counter in the owning package (the `RecordBlockRangeRead` pattern), measured, never debug-only. If in doubt about overhead, run the DittoFS cell instrumented + not and confirm no skew. Toggle: `--instrument` (default on for DittoFS cells).
10. **Server-side resource metering — universal, the FUSE-tax evidence** (`resmeter.go`): per cell, snapshot the **serving process's** context switches (voluntary + involuntary, `/proc/<pid>/status`) and CPU% — DittoFS server / FUSE daemon (juicefs/s3ql/rclone/s3fs) / knfsd / ganesha / samba — **plus system-wide ctxsw** (`vmstat`/`pidstat`) as a uniform fallback for kernel-thread servers where per-process ctxsw is fuzzy. This is what turns "DittoFS avoids the FUSE context-switch tax" into a measured column, comparable across all server types. Prod-safe (procfs only); applies to every system, not just DittoFS.

### Phase 1b — cold-from-S3, uniform eviction (`--evict-cache`)
The cold read pass forces every system genuinely cold-from-S3, not just OS-cache-cold. Each per-system
script grows an **`evict` action** (alongside `start`/`stop`) encapsulating that tech's cache eviction;
the harness calls `<system>.sh evict` then drops client+server OS page cache before the cold fio pass.
Per-tool eviction: **DittoFS** `dfsctl store block evict` + #1595 `DrainLocalSynced`; **rclone mount**
clear `--cache-dir` (or `--vfs-cache-mode off`); **s3ql** `s3qlctrl flushcache` + clear `--cachedir`;
**juicefs** clear `--cache-dir` (or `--cache-size 0`); **s3fs** clear the `use_cache` dir; **local-disk
controls** OS-page-drop only (no S3 — "cold" = from-disk-not-RAM, documented). Behind `--evict-cache`
(default on for S3-backed cold passes). Exact per-tool commands pinned during implementation against
each tool's installed version.

### Phase 2 — provisioning lifecycle (single disposable SCW VM), owned by the Go binary
`setup` / `run` / `teardown` shell the `scw` CLI from Go, reusing the validated `parity-scw.sh` robustness: create VM with a large root volume (`root-volume=sbs:100GB:5000`), IP-poll, ssh-wait + keepalive, cross-build+push `dfsbench`, **detached driver + DONE sentinel + poll** (survives ssh drops), env-only creds via ssh stdin, transient-aware teardown. VM id/IP persisted to `.bench-vm.json` so run/teardown reattach. Everything colocated on the VM (FS server + fio client + loopback mount); competitors run one at a time with full teardown between cells. `bench/scripts/compare-scw.sh` = thin one-shot wrapper (setup→run→teardown); `compare-smoke.sh` = secret-free localhost MinIO for CI.

### Phase 3 — relocate component benches, then delete the mess
- **Audit each in-process benchmark individually.** For every workload in `bench/blockstore/`, `bench/metadata/`, `bench/snapshots/`, and the `bench/parity/` engine paths: decide keep-vs-retire on its own merit (ongoing diagnostic value, not covered by an existing in-package bench). **Move the keepers into their reference package** as `Benchmark*` funcs (`bench/blockstore/` → `pkg/block/engine/`; `bench/metadata/` → `pkg/metadata/`; `bench/snapshots/` → its package; parity engine throughput → `pkg/block/engine/`); **retire everything else.** (Existing `pkg/block/{chunker,engine,hash}` + `pkg/block/local/fs` benches already follow the pattern — leave them.) End state: **`<root>/bench` contains ONLY the fio harness** — no Go microbenchmark code.
- **Delete** (dead / duplicated / superseded by fio + `go test -bench`):
  - repo-root `infra/` (duplicate), `bench/infra/` Go+Pulumi program.
  - `bench/orchestrator/`, `bench/remote/`, `bench/parity/` (after salvage), `bench/adapters/`, `bench/gc/`.
  - `pkg/bench/` (Go fio-alike) + `cmd/dfsctl/commands/bench/` (`dfsctl bench run/compare/storage-tiers`).
  - `cmd/bench` in-process subcommands (`blockstore, orchestrate, remote, snapshots, metadata, parity, gc, adapters`) — keep only `compare`.
  - **All `bench/scripts/*.sh`** (run-all, parity-scw, parity-smoke, s3-baseline) + `bench/analysis/*` — logic folded into Go; raw-S3 baseline is now MinIO warp. The drifted `systems.go` too.
  - Update `bench/README.md` + `.planning/perf/BENCHMARK-PLAN.md` to the new single-harness reality; drop `.github/workflows/parity.yml` refs if present.

## CLI surface (draft)

`dfsbench` — DittoFS vs competitors, one fio benchmark across the board. Cobra, dfsctl-style.

```
COMMANDS
  setup      Provision a Scaleway VM, install deps, build+push dfsbench (leave it ready)
  run        Mount → fio → gather → compare (auto-setup if no VM; --local/--smoke skip provisioning)
  baseline   Measure raw-S3 + local-disk ceilings (also run by `run` unless --skip-baseline)
  report     Re-render the comparison table from a results dir
  teardown   Destroy the benchmark VM
  list       List available systems / workloads / sizes (discoverability)

Lifecycle: `setup` once → `run --resume` many times → `teardown`. The active VM's id/IP is
recorded in a state file under --results (`.bench-vm.json`) so run/teardown find it without
re-passing --scw-id (creds stay env-only). `run` with no VM auto-provisions and, unless --keep,
tears down at the end (one-shot mode).

GLOBAL
  -o, --output table|json|yaml     (default table)
      --results DIR                (default ./bench-results)
      --config FILE                dfsbench.yaml — declares the run; CLI flags override; creds env-only

run — selection (default = everything; this is the "run specific benchmarks" surface)
      --systems   LIST   comma list or glob: dittofs-s3-nfs3,juicefs,s3fs  |  'juicefs-*'
      --group     NAME   named bundle: all | dittofs | s3-backed | controls
      --workloads LIST   seq-write,seq-read,rand-read-4k,rand-write-4k,mixed-rw,metadata,small-files
      --sizes     LIST   small(64k) | medium(1m) | large(1g)  |  explicit: 64k,1g
      --protocols LIST   nfs3,nfs4,smb3   (mount each tech over each; invalid combos auto-skipped)
run — where it runs
      --provider  scw    disposable Scaleway VM (default; auto-setup unless a VM already exists)
      --scw-id    ID     target an existing VM from `setup` (else read from .bench-vm.json)
      --local            run on this host (you supply mounts)
      --smoke            localhost MinIO, secret-free (CI; tiny matrix)
      --target    PATH   benchmark an already-mounted FS as an ad-hoc system
run — control
      --resume           skip cells whose result JSON already exists (crash-safe)
      --skip-baseline    don't (re)measure raw-S3 + local-disk ceilings
      --warm / --cold    which passes (default both)
      --evict-cache      deep per-tool cache eviction before cold reads (default on, S3-backed)
      --instrument       enable DittoFS prod metrics + pprof capture per cell (default on for DittoFS)
      --duration 60s  --threads 4  --iodepth 32     fio pass-through knobs
      --keep             leave the VM up   |   --dry-run   print the matrix and exit
setup / teardown
      --scw-type / --scw-zone / --scw-root-volume    (setup; S3+SCW creds via env only, never argv)
      --scw-id ID                                    (teardown; else the VM in .bench-vm.json)
```

Examples:
```
dfsbench setup                                                 # provision VM once, install deps, ready
dfsbench run --group s3-backed --protocols nfs3,smb3 --resume  # iterate against it, crash-safe
dfsbench run --systems 'juicefs-*' \
             --workloads rand-read-4k --sizes large            # juicefs over all protocols, one cell
dfsbench run --smoke                                           # secret-free localhost MinIO (CI)
dfsbench run --local --target /mnt/juicefs                     # fio an FS you mounted yourself
dfsbench report --results ./bench-results                      # re-render table from saved JSON
dfsbench teardown                                              # destroy the VM (from .bench-vm.json)
```

Sample output (`rand-read-4k · large · cold`) — protocol + access mechanism are explicit, anchored to ceilings.
`SERVER` = which NFS/SMB server and its space (kernel/user); `FUSE` = FUSE in the data path. Carried in the
result JSON as `server`, `server_space` (kernel|userspace|native), `fuse` (bool), and an `access_stack` string.
```
BASELINES (ceilings)
  local-disk  seq 2,400 MB/s   rand-4k 180,000 IOPS      raw-s3  PUT 620 / GET 890 Mbit/s  (16 MiB obj)

BACKEND      WORKLOAD      SIZE  PROTO PASS SERVER         FUSE  IOPS   MB/s  OPS/s p50µs p99µs S3MB CTXSW/s err
dittofs-s3   rand-read-4k  large nfs3  cold dittofs(user)  no   8,940   349    —    112  2310 1024  18,400  0
dittofs-s3   rand-read-4k  large smb3  cold dittofs(user)  no   7,850   307    —    131  2540 1030  19,600  0
juicefs      rand-read-4k  large nfs3  cold knfsd(kernel)  yes 11,200   437    —     98  1870 1088  96,300  0  ←FUSE tax
s3fs         rand-read-4k  large nfs3  cold knfsd(kernel)  yes  1,290    50    —    690 14200 4096 141,000  0  ←FUSE tax
kernel-nfs   rand-read-4k  large nfs3  cold knfsd(kernel)  no  61,400  2398    —     14   210    0   9,200  0  (control)
dittofs-s3   small-files   small nfs3  cold dittofs(user)  no      —      —  4,210    —     —  128  22,100  0
juicefs      small-files   small nfs3  cold knfsd(kernel)  yes     —      —  1,760    —     —  141 108,400  0  ←FUSE tax
```
Access stacks made explicit: `dittofs-*` = kernel NFS/CIFS **client → DittoFS userspace server** (no FUSE);
FUSE competitors over NFS = **client → knfsd (kernel) → FUSE → S3**; over SMB = **client → Samba (user) → FUSE → S3**;
direct-FUSE baseline = **FUSE → S3** (no network server).

**Column dictionary (every column defined, with source + units):**
- **BACKEND** — storage technology under test.
- **WORKLOAD** — fio access pattern: `seq-write` | `seq-read` | `rand-read-4k` | `rand-write-4k` | `mixed-rw` | `metadata` | `small-files`.
- **SIZE** — file/dataset class: `small`=64 KiB | `medium`=1 MiB | `large`=1 GiB.
- **PROTO** — client mount: `nfs3` | `nfs4.1` | `smb3` | `fuse` (direct).
- **PASS** — `warm` (caches primed) | `cold` (after per-tool eviction + page-cache drop → served from S3).
- **SERVER** — serving NFS/SMB impl + space: `dittofs(user)` | `knfsd(kernel)` | `ganesha(user)` | `samba(user)`.
- **FUSE** — FUSE in the data path (yes/no).
- **IOPS** — completed I/O ops per second at `BLOCK_SIZE` (fio `iops`). *Primary for rand-\* (4 KiB); "—" where not the workload's headline (seq/meta).*
- **MB/s** — throughput in **MiB/s (2²⁰ B/s)** from fio `bw_bytes`. *Primary for seq-\*.*
- **OPS/s** — filesystem **metadata/file ops per second** (create+stat+delete for `metadata`; create/read/stat/delete for `small-files`). *Primary for meta & small-files.*
- **p50µs / p99µs** — per-operation **completion latency** (fio `clat`, ns→µs) percentiles over all ops in the cell; p50 = median op, p99 = tail. Op = the workload's dominant op (read for reads, write for writes).
- **S3MB** — real bytes moved **server↔S3** during the cell (MiB), from the OS network counter to the S3 endpoint (nftables / `/proc/net/dev` delta). On cold reads this exposes **read amplification** (fetched vs requested); controls (no S3) = 0.
- **CTXSW/s** — **server-side context switches per second** = Δ(voluntary+involuntary `ctxt_switches`) ÷ wall-time, from `/proc/<server-pid>/status` (per-process), with `vmstat` system-wide as the uniform fallback. The FUSE-tax indicator.
- **err** — failed I/O ops in the cell (fio error count); nonzero flags an unreliable/excluded cell.

Each workload populates its headline rate column (seq→MB/s, rand→IOPS+MB/s, meta/small-files→OPS/s); non-applicable rate cells render "—". Every cell always carries latency, S3MB, CTXSW/s, err.

## Execution sequence (shippable, reviewable increments)
Not one mega-PR — each ships green through simplifier → reviewer → CI → squash-merge:
0. **xfstests tracking issue** (no code).
1. **PR1 — delete duplication + relocate microbenches.** Remove repo-root `infra/`, `bench/orchestrator|remote` (salvage `exec.go` first), `pkg/bench`, `cmd/dfsctl/commands/bench`, `bench/parity`, cmd/bench in-process subcommands. Audit each in-process bench → move keepers into `pkg/...`, retire the rest. Net: `bench/` = fio assets only; `go build ./... && go test ./...` green. (Large but low-risk — mostly deletion.)
2. **PR2 — Go harness skeleton + fio + local/smoke.** `dfsbench` binary: YAML config, `LoadGen`(fio) driver, result schema, report table, `run --local`/`--smoke` (MinIO via docker). Runs DittoFS + one FUSE competitor locally, no cloud.
3. **PR3 — competitors × protocols + eviction.** Backend registry (Install/Start/Stop/Evict/Mount) + `go:embed` config templates: kernel-nfs, ganesha, samba, rclone-mount, s3ql, juicefs, s3fs; knfsd/Samba re-export; capability map; `--evict-cache`.
4. **PR4 — SCW provisioning + resume.** `setup`/`run`/`teardown`, `.bench-vm.json`, detached-driver robustness, `--resume`.
5. **PR5 — baselines + instrumentation.** warp raw-S3 + fio local-disk ceilings; DittoFS metrics/pprof; universal ctxsw/CPU metering; native-vs-re-export pairing view (the thesis payoff).

## Prior art / OSS reuse (researched)
- **fio** — kept as the uniform load generator for mounted FS (your directive; the credibility standard; job files exist). Load-gen behind a `LoadGen` interface for swappability.
- **[MinIO warp](https://github.com/minio/warp)** — the raw-S3 baseline tool (external CLI; JSON, `--autoterm`, `warp cmp`). Not hand-rolled.
- **[elbencho](https://github.com/breuner/elbencho)** — NOT adopted now (fio is the directive); the migration target if we ever want native-`s3://` + `file://` in one tool with unified metrics. The `LoadGen` interface keeps the door open.
- **[nsdf-fuse](https://github.com/nsdf-fabric/nsdf-fuse)** + **[rclone-vs-s3fs-bench](https://github.com/eran132/rclone-vs-s3fs-bench)** — methodology prior-art: mine for validated mount configs + cold-read pitfalls before finalizing competitor setup. Don't fork.
- Competitors (juicefs/s3ql/rclone/s3fs/ganesha) + `scw`/`docker` — all driven as external CLIs from Go.

## Verification
- `go build ./...` green; `grep -r "pkg/bench\|dfsctl bench\|bench/parity" ` clean; no `infra/` at root.
- Component benches: `go test -bench=. ./pkg/block/... ./pkg/metadata/...` compile + run.
- `dfsbench compare --smoke` (localhost MinIO, no secrets): full matrix vs dittofs + rclone-mount + s3fs; **kill mid-run, re-run with `--resume` → skips completed cells**; markdown+CSV table renders with an s3_bytes column (pure-FS controls show ~0).
- Real run: `bench/scripts/compare-scw.sh` on SCW vs real S3, one competitor at a time → comparison table across all named systems. Tear the VM down after (cost).
- Issue opened + assigned; `.claude/settings.json` etc. untouched.

## Scope / deferred
- **In scope:** the one fio harness (Go orchestrator + fio + single-VM scw + resume + uniform s3 meter + table), delete the 3 dead harnesses + duplicate infra + Go-native runner, relocate the few unique component benches, open the xfstests issue.
- **Deferred (follow-ups):** (1) **prefetch tuning for big-file stream download** — once we have the comparison numbers (esp. vs JuiceFS, the architectural leader on serial cold reads), tune `cache.go` `seqThreshold`/`maxPrefetchDepth`; data-driven, not guessed. (2) xfstests wiring next to pjdfstest (tracked by the Phase-0 issue). (3) a scheduled WAN tripwire in CI (needs SCW creds as GH secrets).

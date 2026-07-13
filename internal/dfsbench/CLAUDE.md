# CLAUDE.md — `dfsbench` benchmark harness

Run playbook for the DittoFS-vs-competitors benchmark. The *design* is in
`.planning/2026-07-08-bench-harness-consolidation.md` (#1602); this file is the
"how do I actually run it and read the result" that isn't in the code.

Harness = `cmd/bench` (binary `dfsbench`), library here in `internal/dfsbench/`.
`fio` is the **only** load generator; competitors are mounted FUSE filesystems
re-exported over kernel NFS/Samba so fio hits them identically. Latest published
run: `docs/BENCHMARKS.md`.

## Where we stand (2026-07-10, `develop@0d79ede1`, NFSv3, 7 systems)

- **Metadata is THE deficit** — DittoFS **239 ops/s, dead last** (ZeroFS 371,
  s3ql 700, JuiceFS 1824). Only axis we lose to the whole field. Fix in flight:
  metadata group-commit **#1573** (per-create fsync is the wall).
- Random-read **much improved** post #1648/#1651 (warm fast path, −256× amp):
  7437 large / 24862 medium IOPS, ~56–58% of JuiceFS. Remaining gap is per-read
  **NFS RPC + metadata lookup**, not the block store (engine is 20× faster).
- We **lead the durable-write cohort** on random-write (2365 IOPS); seq-write is
  low (156 MB/s) because it's a real through-to-disk durable write.
- **Cold-read DittoFS = pending** — `dfsctl system drain-uploads` evict errored
  that run. Re-run needed for the cold column.

## Commands

```sh
go build -o dfsbench ./cmd/bench      # fio must be on PATH (use `nix develop`)

dfsbench list                          # backend × workload × size matrix
dfsbench run --smoke                   # tiny self-contained CI run on a temp dir
dfsbench run --local --target /mnt/x   # fio an already-mounted FS
dfsbench run --dry-run ...             # print the cell matrix, run nothing

# Full cloud run: provision one disposable VM → managed matrix → tear down
dfsbench setup                         # SCW_* env picks type/zone/image; writes .bench-vm.json
dfsbench run --remote --config bench.yaml \
  --systems dittofs-s3-nfs3,dittofs-sqlite-s3-nfs3,dittofs-postgres-s3-nfs3,zerofs-nfs3,s3ql-nfs3,juicefs-nfs3,rclone-nfs3,s3fs-nfs3,local-disk-nfs3 \
  --sizes medium,large
dfsbench report --results ./bench-results   # re-render the comparison table
dfsbench teardown                      # terminate the VM in .bench-vm.json
```

**GOTCHA — `--config` is REQUIRED for any S3 backend.** `bucket`/`endpoint`
have no CLI flags; without a config every S3 system fails setup with
`Custom endpoint \`\` was not a valid URI` and only `local-disk` runs. Minimal
config (creds still come from env):

```yaml
# bench.yaml
bucket: dittofs-bench                    # canonical SCW bench bucket (per-backend prefixes, auto-wiped)
endpoint: https://s3.fr-par.scw.cloud
```

`--remote` launches the matrix **detached on the VM and polls** — no interim
local result files; they're pulled back only at the end. `--resume` skips cells
whose JSON already exists (e.g. `local-disk` from a prior partial run). The three
`dittofs-{s3,sqlite-s3,postgres-s3}` backends give the badger-vs-sqlite-vs-postgres
metadata comparison. **`run` exits 0 even if 8/9 systems fail setup** — always
check `ls bench-results/ | sed 's/__.*//' | sort -u` covers every system before
trusting the report.

Key `run` flags: `--resume` (skip cells whose result JSON exists — crash-safe),
`--evict-cache` (default true; adds the cold post-evict read pass),
`--skip-baseline` (skip local-disk ceiling), `--threads` (4), `--runtime` (60s;
smoke 3s), `--results` (`./bench-results`). One JSON per cell, slug-named.

## Environment

- **S3 creds**: `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` (`_SESSION_TOKEN`
  optional) in the env. Bucket + endpoint (`s3.fr-par.scw.cloud`) are in run config.
  Use **`scw` CLI**-issued S3 keys, **not** Pulumi outputs (Pulumi keys 403 on S3).
  The `scw` CLI's own object-storage keys work — pull them at run time so no
  secret is stored: prefix the run with
  `AWS_ACCESS_KEY_ID=$(scw config get access-key) AWS_SECRET_ACCESS_KEY=$(scw config get secret-key)`.
- **VM**: `scw` CLI must be authed. Knobs (defaults): `SCW_ZONE=fr-par-1`,
  `SCW_INSTANCE_TYPE=POP2-8C-32G`, `SCW_IMAGE=ubuntu_noble`,
  `SCW_ROOT_VOLUME=sbs:100GB:5000`, `SCW_NAME`, `SCW_SSH_KEY`.
- `setup` cross-builds `dfsbench`/`dfs`/`dfsctl` (CGO-free linux), pushes over
  SSH, apt-installs fio + nfs/samba + s3fs/rclone/s3ql/juicefs on the VM.

## Operational gotchas (learned the slow way)

- **`--runtime` defaults to 60s/cell** → the full 9-system × 2-size matrix (~180
  cells) is ~4h. Pass **`--runtime 30`** to match the published baseline and
  halve it (~1.5–2h). Don't `--resume` across a runtime change — wipe
  `bench-results/` first so you don't mix 30s and 60s cells.
- **Aborting a `--remote` run needs TWO kills.** The local `dfsbench run` only
  polls; the matrix runs as the **`dfsbench` binary detached on the VM**
  (`/root/dfsbench run …`, log `/root/run.log`, sentinel `/root/DONE`). Kill both:
  `pkill -f 'dfsbench run'` locally, then on the VM
  `pkill -9 -x dfsbench; pkill -9 -x fio` — use **`-x` (exact name)**, because a
  `pkill -f dfsbench` also matches your own ssh shell (its cmdline contains the
  word) and kills the session (ssh exits 255) before finishing.
- **Manual SSH to the bench VM:** user is `root`; the SCW key lives in the
  **1Password agent**, not the default macOS agent —
  `export SSH_AUTH_SOCK="$HOME/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock"`.
  SCW reuses IPs, so a stale `known_hosts` entry triggers "HOST KEY CHANGED";
  add `-o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no` (disposable VM).

## Reading results — the rig lies, so don't trust it blindly

- Each cell is **one short fio run on a shared cloud VM** (`--runtime`, default
  60s; the published baselines pass `--runtime 30`). Trust **shape and order of
  magnitude, not the third digit**. Read-IOPS especially is noisy (±70% seen).
  For a real A/B on the read path, trust **pprof + unit/engine benchmarks**, not
  rig IOPS — the rig has confounds (rollup CPU, restart re-seed, pin-can't-force-
  S3-only) that defeat clean comparisons.
- Native-S3 servers (DittoFS, ZeroFS) get **no page-cache free ride**; the FUSE
  re-exports do. Compare DittoFS to ZeroFS (the other native server) to isolate
  design from the native-S3 handicap.
- **Cold-read barrier runs before EACH read cell** (`managed.go` `coldBarrier`),
  not once — a single evict warms later cells and inflates cold rand-read.
- Superseded: `bench/results/*.json` + `bench/orchestrator/` are the **old**
  harness (March 2026, dittofs 47 ops/s). Ignore them; current truth is
  `internal/dfsbench` + `docs/BENCHMARKS.md`.

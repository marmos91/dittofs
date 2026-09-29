# DittoFS local test harness

`dt` runs every DittoFS test tier from one place, with CI's commands, service images and
settings: unit, integration, pynfs, pjdfstest, SMB conformance (WPTS, smbtorture), e2e,
a Compose stack for hands-on work, and git hooks. It drives the repo's own scripts
(`test/conformance/run.sh`, `test/posix/setup-posix.sh`, `test/kmip/start-pykmip.sh`, ...)
rather than reimplementing them.

It runs two ways:
- **In a Linux dev container, `dtc`.** This is the recommended way on macOS. The Mac
  needs only Docker Desktop and git, the NFS and SMB clients are the Linux kernel's (as
  in CI), and nothing is installed on the host.
- **Natively on macOS, `dt`.** This needs the toolchain in [SETUP.md](SETUP.md), and it
  exercises the macOS NFS and SMB clients.

Setup for both is in [SETUP.md](SETUP.md).

```
test/harness/
├── bin/dt             # the runner: one entry point for every tier
├── bin/dtc            # the same runner inside the Linux dev container
├── bin/dt-batch       # batches of suite/profile runs, one verdict line each
├── bin/pynfs-4.0|4.1  # pynfs wrappers for macOS (mirror the Nix flake's)
├── bin/timeout        # GNU timeout for macOS (setup-posix.sh needs it)
├── githooks/          # hook chain: the repo's .githooks, then harness extras
├── docker/            # dev image (dtc), pjdfstest and e2e-linux images, entry scripts
├── repro/             # standalone reproductions
├── logs/              # one log per run (gitignored)
├── state/             # locks, KMIP certs, dfsctl login (gitignored)
└── tools/             # pynfs checkout + venv for macOS (gitignored)
```

## Quick reference

```bash
dtc doctor                       # toolchain, kernel clients, Docker host, ports, locks
dtc unit                         # CI PR mode: -race -short, all packages
dtc unit --full --cover          # CI push mode (no -short) + coverage profile
dtc unit --changed               # only packages changed vs origin/develop
dtc unit --fresh                 # -count=1: no test cache (use for baselines)
dtc integration                  # -tags=integration, CI's derived package list, + KMIP interop
dtc quick                        # vet + lint + unit tests of changed packages
dtc pynfs --minor 4.0            # NFSv4.0 conformance, graded vs KNOWN_FAILURES_V40.md
dtc pynfs --minor 4.1 --profile postgres-s3
dtc posix --nfs 4.1              # pjdfstest, CI's own command
dtc e2e [--test P] [--nightly] [--require-nlm]
dtc smb wpts|smbtorture --profile memory
dtc stack up                     # dfs + Localstack S3 in Compose; then `dt stack mount` on the Mac
dtc services up|down|status      # Localstack, 2x Postgres, PyKMIP
dtc cleanup [--volumes]          # stop everything the harness runs; refuses while a suite runs
dtc shell                        # interactive; dtc shell -c 'go test -run X ./pkg/y'
dtc build | dtc clean            # rebuild the image / drop image + cache volumes
dt hooks install [--container]   # the hook chain; --container runs it through dtc
dt-batch matrix|extras|container [--dry-run]
```

Every `dtc` command is the same `dt` command, run inside the container. The native
commands are the same with `dt`.

## Container mode: `dtc`

`dtc` runs `dt` in a privileged Linux container on Docker Desktop's kernel. The image
(`docker/dev.Dockerfile`, tag `dittofs-harness-dev:go1.26`) pins what CI uses:
- Go 1.26.8 (`go.mod`) and golangci-lint v2.12.2 (`lint.yml`);
- pjdfstest `03eb257` and pynfs `cd47018` (`flake.lock`);
- the NFS, CIFS and Kerberos clients, rpcbind, `nfs4-acl-tools`, `iptables`,
  `iproute2`, `smbclient`, `fio`, `jq`, `xmlstarlet`, shellcheck, gawk and the Docker
  CLI.

The image rebuilds itself when `dev.Dockerfile` changes: a label carries the file's hash.

How it is wired:
- **Same paths as the host.** The checkout and `/tmp/dtc` are bind-mounted at their host
  paths. So a bind mount that a suite asks the Docker daemon for resolves on the host
  too. Examples: the SMB conformance compose files, PyKMIP's certs, and the Kerberos
  tests' KDC keytab dir under the e2e `TMPDIR=/tmp/dtc/e2e`. For a worktree, dtc also
  mounts the main checkout's `.git`.
- **Sibling containers, not nested ones.** The host's Docker socket is passed in. The
  services, the Compose stack, the SMB conformance stack and testcontainers are
  ordinary containers on the host's daemon.
- **Services on localhost, as in CI.** The entry script forwards `localhost:4566`,
  `5432`, `15432` and `5696` to `host.docker.internal`, where the services publish. So
  the repo's scripts, which hard-code `localhost`, work unchanged. The testcontainers
  host is `host.docker.internal`.
- **Host-side dfs inside the container.** pynfs, pjdfstest and e2e start their `dfs` in
  the container's network and PID namespaces. So `setup-posix.sh`'s
  `pkill -f "dfs start"` cannot kill a `dfs` running on the host, and a leaked `dfs` dies
  with the container (`--rm`).
- **Caches in volumes.** Go modules, the build cache and the lint cache live in the
  `dtc-gomod`, `dtc-gobuild` and `dtc-lint` volumes, not in `~/go`.
- **Host timezone and uid are passed in.** Log names use local time, and the stack's SMB
  user gets your uid.
- **Locks record where they were taken** (`mac` or `container:<name>`). A pid is only
  checked where it was recorded. A container's lock is checked by whether that
  container still runs, and a lock from elsewhere is never taken for stale. One suite at
  a time holds across the host and every dtc container, because the suites share
  `test/**/results/`, the services' databases and the Docker host's ports.
- **Port checks by place.** A host-side `dfs` binds in dt's own namespace. The Compose
  stack and the SMB conformance stack publish on the Docker host, which dtc probes
  through `host.docker.internal`.
- **Mounts stay on the host.** `dtc stack mount` says so: a mount inside a throwaway
  container is visible to no host program. `dt stack mount` on a Mac needs only macOS's
  `mount` and `mount_smbfs`.
- **`harness/bin` is last on PATH** in the container. Otherwise the macOS pynfs and
  `timeout` shims would shadow the image's own tools.

Limits of Docker Desktop's kernel (7.0.12-linuxkit, probed):

| Kernel support | Present | Effect |
|---|---|---|
| `nfs`, `nfs4`, `cifs`, `smb3`, `auth_rpcgss` | yes | NFSv3/v4.x and multi-user SMB clients work |
| `rpcsec_gss_krb5` | no | NFS Kerberos (`sec=krb5`) needs a real Linux VM |
| `dm_flakey` | no | `test/crash` needs a real Linux VM |

The SMB conformance images are `linux/amd64`. On an arm64 Mac they run emulated, and
timing-sensitive cases may flake, so an x86_64 Linux host is the authoritative place for
them.

## Shared services

`dt services up|down|status [localstack|pg|pg-it|kmip]`. Each one mirrors the CI service
it replaces:

| Service | Container | Port | Used by | Mirrors |
|---|---|---|---|---|
| Localstack 4.13.1 (S3 only) | `dittofs-localstack` | 127.0.0.1:4566 | `*-s3` profiles of pynfs/posix, e2e | conformance.yml, e2e-tests.yml |
| Postgres 16-alpine | `dittofs-postgres-test` | 127.0.0.1:5432 | `postgres*` profiles (`dittofs`/`dittofs`, db `dittofs_test`), with `--data-checksums` and CI's tuning | conformance.yml |
| Postgres 16 | `dittofs-harness-pg-it` | 127.0.0.1:15432 | integration tests (`postgres`/`postgres`, db `dittofs_test`) | integration-tests.yml |
| PyKMIP 0.10.0 | `dittofs-pykmip` | **127.0.0.1**:5696 | KMIP key-provider interop tests | integration-tests.yml |

There are two Postgres instances on purpose. The integration tests create and drop
databases as the `postgres` superuser, while the conformance profiles connect as
`dittofs` to the same database name. Sharing one instance would let leftover state from
one tier leak into the other.

`dt` runs the steps of `test/kmip/start-pykmip.sh` itself, reading its pinned image and
version. The reason is that the script publishes 5696 on every interface, which exposes
a key server on the laptop's network.

## The tiers

### Unit: `dt unit`

The same command as `unit-tests.yml`:
`go test -race [-short] -timeout=25m ./pkg/... ./internal/... ./cmd/...`. `-short` is
the pull-request mode and the default; `--full` is the push-to-develop mode. `dt unit`
keeps Go's test cache so pre-push stays fast. Use `--fresh` for a baseline.

### Integration: `dt integration`

The same derivation as `integration-tests.yml`: the packages whose test set changes
under `-tags=integration`, run with `-tags=integration -count=1 -timeout=20m -p 1`
against Postgres and PyKMIP. `--changed` narrows the run to changed packages.

Two things it does that CI's YAML does not show:
- **Postgres host and port.** `DITTOFS_TEST_POSTGRES_DSN` is only an on/off gate for the
  Postgres metadata-store suite, which takes its host, port and database from
  `DITTOFS_TEST_PG_HOST/PORT/DBNAME` (default `localhost:5432`). The harness sets both,
  because its integration Postgres is on 15432. Without them the suite reaches the
  other Postgres and fails authentication.
- **KMIP interop.** CI starts PyKMIP for `pkg/block/middleware/encryption/keyprovider`.
  But that package has no `integration`-tagged file, so it is not in CI's derived list,
  and the unit job has no KMIP environment. The interop tests therefore never run in CI.
  `dt integration` runs them as a separate `kmip-interop` step.

`test/integration/portmap` (`-tags=portmap_system`) needs a system rpcbind and is not
run.

### pynfs (NFSv4 protocol conformance): `dt pynfs`

pynfs is its own NFSv4 client over TCP, so it needs no mount and no root. `dt pynfs`
goes through `test/conformance/run.sh --suite pynfs`, which does three things:
1. provisions a host `dfs` with `setup-posix.sh <profile> --no-mount` (API 8080, NFS
   12049);
2. runs the suite;
3. grades it against `KNOWN_FAILURES_V40.md` / `KNOWN_FAILURES_V41.md`.

`--tests "CODES"` runs a subset, ungraded.

**What the default run covers.** `run-pynfs.sh` passes pynfs's `all` group: 601 of 689
tests on 4.0 and 184 of 266 on 4.1. `pynfs-4.x --showcodesflags` lists what `all` leaves
out:
- **4.0:** the 26 `delegations` and 7 `writedelegations` tests, plus `reboot`,
  `fairlocks`, `fslocations`, `utf8` and `spoof`.
- **4.1:** pNFS (`flex`, `layoutreturn`, `pnfs`, `block`), `reboot` and
  `destroy_session`.

`dt-batch extras` runs the relevant groups.

**Delegations need a non-loopback address.** DittoFS refuses loopback NFSv4.0 callback
addresses (`validateCallbackHost`). Over localhost no delegation is ever granted, and the
delegation tests can only warn. `dt pynfs --lan` points pynfs at a non-loopback address
instead: the Mac's LAN IP natively, or the container's bridge IP in dtc. The callback is
then dialable:

```bash
dt pynfs --minor 4.0 --lan --tests "delegations writedelegations"   # ungraded
```

**Server cleanup.** Without root and without passwordless sudo, `run-pynfs.sh` falls back
to `dfs stop --force`. That reads the default PID file, while `setup-posix.sh` wrote the
PID to `/tmp/dittofs-server.pid`, so the server keeps running and holds 8080, 12049 and
12445. `dt pynfs` always finishes with `dt teardown`, which stops it through the right
PID file. Run `dt teardown` by hand after a direct `run-pynfs.sh`.

### POSIX (pjdfstest): `dt posix`

pjdfstest checks file-system semantics through a kernel NFS client; CI uses Linux's.
`test/posix/README.md`'s macOS route mounts the export on the Mac and runs pjdfstest in a
container over that mount. That tests the macOS NFS client and Docker Desktop's file
sharing, not DittoFS alone. The harness does it the CI way:
- **In dtc:** CI's own command, `test/conformance/run.sh --suite pjdfstest --profile P
  --variant V`, as root with the kernel NFS client. With test names
  (`dtc posix chmod`), it runs `setup-posix.sh`, `run-posix.sh --grade` and
  `teardown-posix.sh` directly.
- **Natively:** `setup-posix.sh --no-mount` starts a host `dfs`. Then a privileged Linux
  container (`docker/pjdfstest.Dockerfile`) mounts `host.docker.internal:/export` with
  the kernel client and runs `run-posix.sh --grade`.

```bash
dt posix                                 # NFSv3, memory profile, full suite
dt posix --nfs 4.1 --profile postgres-s3
dt posix chmod                           # one test directory
```

pjdfstest is built at the `flake.lock` revision. The repo's
`test/posix/Dockerfile.pjdfstest` clones HEAD unpinned. Two image details would fail
silently or look like DittoFS bugs:
- **`netbase`.** `mount.nfs` resolves `tcp` through `/etc/protocols`. Without it an
  NFSv3 mount fails with `Protocol not supported`, while NFSv4.1 still mounts.
- **`openssl`.** pjdfstest's `misc.sh` generates file names with `openssl rand`. Without
  it every test prints `openssl: not found`.

`dt posix` keeps a copy of `test/posix/results/{summary.txt,prove.log}` per run, but only
when the run wrote them. The conformance runner grades from its own results tree.

### SMB conformance (WPTS, smbtorture): `dt smb`

Both suites go through `test/conformance/run.sh`, which runs the repo's
`test/smb-conformance/` Compose setup. That setup is a `dfs` container built from the
checkout plus the test-client container. The results are graded against the suites'
`KNOWN_FAILURES.md`.

```bash
dt smb smbtorture --profile memory     # profiles: memory badger sqlite postgres
dt smb wpts --profile memory           # profiles: memory badger badger-s3 postgres-s3
```

The Compose file publishes 8080 and 12445, so those ports must be free (no `dt stack`,
no host `dfs`). The repo's cleanup runs `docker compose down -v` without the profiles
the run started with, so after an `*-s3` or `postgres` run the profile's container
survives. Every later run then refuses with "another instance of this stack is live".
`dt smb` tears the stack down with every profile active, before and after each run.

### e2e: `dt e2e`

`dt e2e` runs the suite the way `e2e-tests.yml` does:
`go test -tags=e2e -count=1 -v -timeout 30m ./test/e2e/...`, from the repo root, inside
`.github/scripts/run-e2e.sh`.
- **From the repo root.** The invocation in `README.md` and the e2e test headers,
  `cd test/e2e && sudo ./run-e2e.sh`, fails every time. `run-e2e.sh` does not change
  directory, and hands `go test` the path `./test/e2e/...`.
- **Output to a file, not a pipe.** A process the suite leaves behind can hold a pipe's
  write end open and hang the reader. The wrapper also caps the run at `E2E_WALL`
  (default 45m). The log appears in `logs/e2e-<ts>.log` at the end.
- **The harness services.** Like CI, the suite gets `POSTGRES_*` and `LOCALSTACK_ENDPOINT`.
  Without them it starts its own containers, and its fallback Localstack is 3.0.
- **`--test` narrows the packages** to those with a matching top-level test. Otherwise
  every other package prints `testing: warning: no tests to run`.
- **A run that ran nothing fails.** CI's wrapper passes a `-run` filter that matches no
  test, because `go test` still prints `ok ... [no tests to run]`. `dt e2e` fails such a
  run (exit 3) when the log has no `=== RUN` line.
- **`--nightly`** sets `DITTOFS_E2E_NIGHTLY=1` for the dedup nightly tier, which CI never
  sets.

In dtc the suite runs as root in the container, and `dt e2e-linux` is the same command.
- `TMPDIR` is `/tmp/dtc/e2e`, shared with the host at the same path, so the KDC bind
  mounts work.
- `GOTMPDIR` is deliberately not set: `go test` would hand it to the test binary as its
  temp dir.
- rpcbind is started for the NLM cases.
- NFS mounts the suite leaves behind are force-unmounted before cleanup, and reported.

Natively on macOS the suite needs root. Run `dt` as yourself in your own terminal: it
asks for the sudo password once, and only the test command runs as root. Root gets a
separate build cache, an offline read-only module cache, and `TMPDIR=/tmp/dte`, which
you own with an inheritable ACL entry. Docker Desktop's file sharing runs as you and
cannot see root's `0700` temp dirs. On macOS only the NFSv3 and SMB cases run:
`framework/helpers.go` skips NFSv4 on darwin, and the `e2e && linux` files are excluded.
`--minio` is refused because its pinned image can no longer be pulled.

`dt e2e --list` lists the tests runnable on the current OS, by package.

### Compose stack (manual NFS + SMB): `dt stack`

The repo's `docker-compose.yml` with the `s3-backend` profile:
- `dfs` built from the checkout, with Badger metadata;
- an S3 block store on the stack's own Localstack (bucket `dittofs`);
- share `/export`, read-write, NFS squash `root_to_admin`.

`dt stack up` also creates an SMB user `dev` with your uid.

```bash
dtc stack up           # or dt stack up
dt stack mount         # on the Mac: NFSv3 -> ~/mnt/dittofs-nfs, SMB -> ~/mnt/dittofs-smb (no sudo)
dt stack umount
dtc stack down         # keeps volumes; 'dt stack down -- -v' drops them
dt stack up --memory   # default profile: memory block store, no S3
```

| | |
|---|---|
| API | http://127.0.0.1:8080, `admin` / `dittofs-compose-dev-password` |
| NFS | 127.0.0.1:12049, `vers=3,tcp,port=12049,mountport=12049,nolocks,noresvport` |
| SMB | 127.0.0.1:12445, user `dev` / `dittofs-dev-password-123` |
| dfsctl | `XDG_CONFIG_HOME=test/harness/state/dfsctl test/harness/state/bin/<os-arch>/dfsctl ...` |

### Repros: `repro/`

Standalone scripts that run their own `dfs` on separate ports (no root):
- **`cold-read-tamper.sh`.** Against `dt stack`, it writes 16 MiB over NFS, flips 64
  bytes in one of the file's `blocks/` objects in S3, evicts, and reads back through the
  same mount and after a remount. The control read matches. The tampered read fails
  with EIO at the tampered chunk. It copies with `cp`/`dd` and checks the exit status:
  `shasum` on a mount hashes whatever it read before an I/O error and exits 0.
- **`gc-repeated-content.sh`** (#2909). For unique, repeating-random and all-zero data,
  it writes a file, deletes it, runs `dfsctl store block gc --grace-period 0`, and
  counts the `blocks/` objects left. Unique and repeating-random data are reclaimed. A
  64 MiB zero file leaves its 16 MiB zero-chunk block, because the carve-time dedup
  adoption holds that hash for an hour whatever the grace period.
- **`gc-adoption-delay.sh`.** Deletes the zero file, then only watches for 80 minutes
  while auto-GC runs (every 15 min, grace 1 h). The block is reclaimed on the first
  auto-GC after the hour: a delay, not a leak. `gc-status` shows only manual runs;
  auto-GC runs appear in the server log.

## Batches: `dt-batch`

`dt-batch SET` runs dt commands one after another and writes one verdict line per run to
`logs/batch-<set>-<ts>.summary`:
- **`matrix [SUITE...]`:** every profile and variant `test/conformance/suites.json`
  defines, for pjdfstest, pynfs, wpts and smbtorture (28 runs).
- **`extras`:** the pynfs groups CI's `all` selection skips.
- **`container`:** one run per tier through dtc, to compare with a baseline.

`DT_RUNNER=test/harness/bin/dtc` runs any set in the container, and `--dry-run` prints
the commands.

## Git hooks

The repo's hooks (`.githooks/`) stay the source of truth. `dt hooks install` points
`core.hooksPath` at `test/harness/githooks/`, which runs them unchanged and adds harness
steps:

| Hook | Runs |
|---|---|
| `pre-commit` | the repo's pre-commit: conflict markers, >1 MiB guard, gofmt, vet, build, golangci-lint, `go mod tidy -diff`, shellcheck |
| `commit-msg` | the repo's commit-msg: conventional subject and trailer rules |
| `pre-push` | 1. `dt unit --changed`: the repo pre-push's `go test -short -race`, with CI's `-timeout=25m`. 2. `dt integration --changed`, with Postgres and PyKMIP started on demand; skipped with a warning when Docker is down. |

Why step 1 is not the repo's pre-push as is: the repo hook runs the changed packages in
parallel with `-timeout=60s`. With several heavy packages changed (the SMB or NFSv4
handlers, the block engine, the journal), most of them exceed that budget, although each
fits when run alone. `DT_PREPUSH_REPO_HOOK=1` runs the repo hook as is instead.

Skip switches:
- `git push --no-verify` skips everything.
- `DITTOFS_SKIP_PREPUSH=1` skips step 1.
- `DT_SKIP_PREPUSH_INTEGRATION=1` skips step 2.
- The repo's `DITTOFS_SKIP_*` variables cover pre-commit.

The base is `origin/develop`, or `DT_BASE` / `DITTOFS_PREPUSH_BASE`.
`dt hooks uninstall` restores `.githooks`.

**Container hooks.** `dt hooks install --container` also sets `git config dt.hooksmode
container`. Each hook then re-enters itself through `dtc hook NAME` inside the Linux
container, so a Mac without Go can commit and push with the same checks. Git's hook
variables are passed in, and the pushed refs arrive on stdin. Container start adds about
1 s per hook.

The protocol suites are deliberately not in the hooks. They take minutes to an hour and
need exclusive ports; run them on demand.

## Safety rails

- **One suite at a time.** The protocol suites and the stack share ports 8080, 12049 and
  12445 and state under `/tmp/dittofs-*`. `dt` holds `state/suite.lock` and refuses to
  start when those ports are in use.
- **Logs.** Every run writes `logs/<tier>-<timestamp>.log`, with the repo commit and the
  exit status and elapsed time on the last line. Suite result trees stay where the repo
  puts them (`test/**/results/`, gitignored).
- **Go's test cache.** A cached `ok ... (cached)` replays a past result for identical
  code and environment. It cannot see mounts, Docker, Postgres or S3. `dt e2e` and
  `dt integration` always pass `-count=1`, and every run reports how many packages came
  from the cache.
- **`dt cleanup`** stops everything the harness can leave running: the services, a host
  `dfs`, the stack, a leftover SMB conformance stack and the e2e-linux container. It
  refuses while a suite holds a lock (`--force` overrides). `--volumes` also drops the
  stack's data and the e2e-linux build cache.

## Known gaps and traps

| Gap | Effect | Harness handling |
|---|---|---|
| MinIO image `minio/minio:RELEASE.2024-09-13T20-26-02Z` can no longer be pulled | `run-e2e.sh --minio` and the MinIO fixture fail without a cached image | `--minio` refused |
| e2e falls back to Localstack 3.0; CI and Compose use 4.13.1 | two S3 emulator versions | `dt e2e` provides 4.13.1 |
| `test/kmip/start-pykmip.sh` publishes 5696 on all interfaces | a test key server on the local network | bound to 127.0.0.1 |
| KMIP interop tests are not in CI's derived integration list | they never run in CI | `kmip-interop` step |
| Repo pre-push `-timeout=60s` | pushes touching several heavy packages time out | CI's 25 min |
| pynfs non-root cleanup reads the wrong PID file | leaks a `dfs` holding 8080/12049/12445 | `dt teardown` after every pynfs run |
| `setup-posix.sh` runs `pkill -f "dfs start"` | kills any `dfs` on the machine, including your own | in dtc it only sees the container |
| SMB conformance cleanup omits the run's profiles | the next run refuses to start | full-profile teardown |
| `test/posix/Dockerfile.pjdfstest` is unpinned and lacks `netbase`/`openssl` | results not comparable to CI | pinned image with both |
| Debian 12's `mawk` has no regex intervals | the pynfs grader's `/^\*{50}$/` never matches, and every run is graded "no results block" | the image installs gawk |
| The four e2e tests using `mountNFSExport` never unmount | on Linux, NFS hard mounts to stopped servers are left behind (9 per full run), and a later `stat` under TMPDIR blocks | `dtc e2e` force-unmounts them |
| `TestBlocksFlipLifecycle_*` does not drop the NFS client's page cache before a "cold" read | the fail-closed step can be served from the client cache, so it is intermittent | `repro/cold-read-tamper.sh` remounts |
| `ENF-02` counts any mount failure as "access denied" | passes without testing anything on a client that cannot mount twice (macOS) | Linux client in dtc |
| macOS `mount_smbfs` allows one mount per server share | multi-user SMB e2e cases fail natively on macOS | pass in dtc |
| The e2e nightly tier (`DITTOFS_E2E_NIGHTLY=1`) never runs in CI | its tests fail at setup: they build the S3 store without `WithBlockAllowPrivateEndpoint`, which the S3 endpoint guard needs for Localstack | `dt e2e --nightly` runs them |
| NFS Kerberos, `test/crash`, `test/edge` | need `rpcsec_gss_krb5`, `dm_flakey`, or Scaleway | a real Linux VM or cloud |

## Reference results (`d562e665`, 2026-09-28/29)

Apple Silicon, Docker Desktop with 8 CPUs and 16 GB.

| Tier | Result | dtc | macOS native |
|---|---|---|---|
| unit (`--fresh`) | 145 packages pass; 55.5% statement coverage | 6 min 21 s (cold caches) | 5 min 44 s |
| integration | 7 packages + KMIP interop pass | 2 min 55 s | 2 min 20 s |
| lint | 0 issues | 28 s | 18 s |
| pynfs 4.0 / memory | graded pass: 589 passed, 1 failed (known), 3 warned, 8 skipped | 13 min 22 s | 8 min 38 s |
| pynfs 4.1 / memory | graded pass: 182 passed, 2 failed (known) | 4 min 22 s | 4 min 18 s |
| pynfs 4.0 delegations (`--lan`) | 21 passed, 10 failed (5 need a human at the server) | 5 min 38 s | not measured |
| pjdfstest v3 / memory | 8,789 tests, 1 known failure (`utimensat/09.t` #5, NFSv3 32-bit time), 0 new | 3 min 7 s | 3 min 19 s |
| pjdfstest v4.0 and v4.1 | 1 known failure (`unlink/14.t` #4, NFSv4 silly-rename), 0 new | 7 min 13 s (4.1, postgres-s3) | about 3 min 50 s (memory) |
| WPTS BVT / memory (emulated) | 226 passed, 39 known, 70 skipped, 0 new | 3 min 37 s | 4 min 1 s |
| smbtorture / memory (emulated) | 550/633 passed, 43 known, 40 skipped, 0 new | 20 min 3 s | 19 min 24 s |
| e2e, full suite | dtc: 130 passed, 3 failed, 4 skipped. macOS: 75 passed, 4 failed, 56 skipped | 14 min 24 s | 6 min 1 s |

All with 0 new failures:
- pjdfstest is at 8,788/8,789 over NFSv3 on every profile (memory, badger, postgres,
  postgres-s3), and over v4.0 and v4.1 on memory, badger and postgres. v4.1 was also run
  on postgres-s3.
- WPTS and smbtorture pass on every profile the suites define.

The e2e failures come from the environment:
- **In dtc**, the two NFS Kerberos tests (no `rpcsec_gss_krb5`) and `TestNLMAxisInterop`,
  whose driver cannot create veth pairs inside the container. The skips are the 3
  nightly-tier tests and a subprocess helper.
- **On macOS**, the one-SMB-mount-per-share limit (`TestPermissionEnforcement`,
  `TestSMBByteRangeLocking`), `EACCES` instead of `EROFS` on a read-only NFS export, and
  the intermittent `TestBlocksFlipLifecycle_NFS`. Most skips are NFSv4 cases.

Every test that fails on macOS because of its clients passes in dtc. Most macOS skips
run there too: the NFSv4 ACL, NFSv4.1 EOS, SMB Kerberos and smbclient tests. NFS
Kerberos is the exception, since it skips on macOS and fails in dtc for lack of
`rpcsec_gss_krb5`.

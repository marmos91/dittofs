# RFC: one test harness — a suite registry, one runner, one result format

**Status:** proposed. Nothing here is implemented yet; §8 orders the work.
**Revised:** 2026-10-08, after the first review. Fixtures replace the suites' setup scripts and
the tools are called directly (§3, §4.2); results are CTRF, with JUnit generated from it (§4.6);
pull requests get no retries (§4.7).
**Discussion:** the pull request that adds this file.
**Builds on:** the `dt` harness on `dev/test-harness` (`1b9d49b9`), the conformance graders and
their tests, the system scenarios in `test/scenarios/`, and `dfsbench` (`cmd/bench`).
**Checked against:** `develop` at `d0efaa75` (at `e33edad6` for #2955) and `dev/test-harness` at
`1b9d49b9`, on 2026-10-07; the conformance scripts again at `613cf6db`, with develop's CI logs, on
2026-10-08. §10 lists what was checked and what turned out different from what was assumed.

Read this before adding a test suite, changing a test workflow, or touching `test/harness/`. It
describes the harness we want to end up with, and how to get there from what exists, one suite at
a time.

---

## 1. Why

Every suite has its own entry point, its own setup and its own way of saying how it went:

- **Many entry points.** Unit, integration and e2e tests run as inline `go test` lines in their
  workflows, and lint as inline steps in `lint.yml`. Conformance runs through
  `test/conformance/run.sh`. Scenarios run through
  `test/scenarios/setup.sh`, and `dfsbench` through `cmd/bench`. `dt` wraps most of them, but it
  lives on an unmerged branch. The repository has 16 workflows, and a non-docs PR starts about 30
  jobs across them.
- **Wrappers around wrappers.** A conformance cell in CI runs `test/conformance/run.sh`, which runs
  the suite's scripts, which run the tool. The Makefile's `test-*` targets wrap the same scripts
  once more, and no workflow uses them.
- **The same work written several times.** The server setup (log in, add the stores) is written
  in at least five places: the two `bootstrap.sh`, `setup-posix.sh`, and two jobs of
  `smb-client-compat.yml`. The conformance matrix is resolved separately in `conformance.yml` and
  `nfs-pynfs.yml`. Ten workflow steps write their own job summary inline, and two of them
  (`smb-client-compat.yml`, Linux and macOS) print a hard-coded all-green table under
  `if: always()`, so they report a pass when the job failed.
- **Some green results test nothing.** The pjdfstest `-s3` cells never reach S3, because nothing
  creates their bucket. On develop run 37661151652 the NFSv4 `badger-s3` cell graded 8,789 tests
  green while localstack answered every S3 request it got, 9 `HeadBucket` calls, with
  `NoSuchBucket`, and received no upload. The NFS Kerberos test exits 0 when its container can't
  load the NFS module; it did so, and passed, on each of the last four green develop runs. In
  both, a setup problem reads as a pass.
- **The tools' own results go unread.** Nothing writes JUnit XML or a run summary. pynfs writes
  JSON, and its grader parses the text log instead. pjdfstest is graded from prove's text summary,
  and smbtorture's subunit lines by a hand-written parser. Only fio's JSON is read, by `dfsbench`.
  Nothing records which tests failed in which run, so "is this flaky?" has no answer except
  reading old logs.
- **Tests don't gate merges.** The required checks are Go Checks, Repo Checks, ShellCheck,
  gitleaks and Analyze (go): lint and security only. A PR whose unit tests fail can merge.
- **Nobody hears when develop goes red.** The develop-red e-mail and issue were removed in #1705,
  because flaky smbtorture subtests set them off constantly. What remains is one retry and a
  warning comment on PRs, for three workflows. On 2026-10-06/07, `AD-DC PAC Test` failed on 5 of
  its 10 completed develop runs, in at least two different tests (one is #2968). `ci-health`
  doesn't watch that workflow, so nothing said so.
- **Scenarios carry no signal.** The 42 system scenarios run twice a day on a dedicated host, and
  in no CI job. About half fail by design, because they reproduce open issues, but no list says
  which. A red run looks the same whether something regressed or not.
- **PRs are slow.** On 2026-10-07 (300 PR workflow runs) Conformance Suites took 23 min median
  and 39 min at p90, NFS Protocol Conformance 17 and 25, Unit Tests 14. A PR waits 25–40 min for
  its slowest workflow. The workflows' own comments estimate 40–45 min.

## 2. Requirements

| # | Requirement | Met when |
|---|---|---|
| R1 | One entry point, run the same way on a laptop and in CI | every CI test step is a `dt` command a developer can paste |
| R2 | One registry file of suites, in YAML, with its format specified | §4.1 is the format; `dt` refuses a file that doesn't match it |
| R3 | A new suite is one line | the one-line form in §4.1 runs with defaults |
| R4 | Runs and failures are tracked by the harness itself | every run writes §4.6's results; `dt history` answers "since when, how often" |
| R5 | Everything else is automatic | services, matrix, grading, retries and reports need no per-suite code |
| R6 | A quick tier on PRs, the full suite after every merge to develop | §4.3's tiers and §4.8's workflow; no selection by changed layer |
| R7 | An alert when develop goes red, without the flood that removed the last one | §4.8's single issue, after retry, excluding quarantined tests |
| R8 | External-tool suites first, and green | pjdfstest, pynfs, WPTS, smbtorture, scenarios and fio report no new failures |
| R9 | Adapt the harness; keep what's tested, replace what isn't | `dt` and its paths carry over; the graders' rules carry over with their tests; each suite's setup scripts give way to a fixture (§4.2) once it gives the same results, and are deleted in the same change |
| R10 | Not run is never passed | an unmet need, a fixture that fails its own check, or a suite that ran no tests ends in exit 2 (§4.4) |

"Green" means no new failures: every failing test is either fixed or listed, with an issue, as a
known failure. That's how the conformance suites already work, and it's the only definition that
lets a suite which reproduces open bugs be green.

## 3. Decision: a command-line tool, written in Go

### A CLI, not an API service

The entry point is a CLI, `dt`, as it is today. An HTTP service that runs tests would need a host,
authentication and a deployment, and a developer's laptop would talk to it differently from CI.
That breaks R1. The tools this harness is compared with are all commands run the same way
everywhere: `bazel test //...`, Kubernetes' `make test` over `hack/` scripts, xfstests'
`./check -g quick`, LTP's runtest files.

The machine-facing part, what other programs build on, is a set of files with versioned formats:

- **input:** the registry (§4.1);
- **output:** `summary.json` in CTRF, JUnit XML generated from it, and a Markdown report per run
  (§4.6);
- **status:** the exit code (§4.4).

CI, the dedicated host, an issue bot or a future dashboard read those files. If a dashboard is ever
built, it reads results; it doesn't run tests.

### In Go, not by growing the bash script

`dt` today is one 1,270-line bash script. What R2–R10 add is data handling: parsing YAML,
expanding a matrix, setting up fixtures, reading `go test -json`, TAP, subunit, TRX, pynfs and fio
output, grading against lists, writing JSON and JUnit, and reading history. In bash that needs `yq`
and `jq` (the Nix dev shell has neither) and can't be unit-tested in any reasonable way. In Go:

- the dependencies are already direct ones in `go.mod`: `gopkg.in/yaml.v3` and `cobra`;
- every piece is a package with `go test` tests, which the repository already expects of its code;
- it runs natively on Linux, macOS and Windows, where the Windows unit tests and the SMB
  client-compatibility job run.

Shell keeps what is shell by nature: the `dtc` container, the service containers and the
scenario host. Starting a DittoFS server on a store profile, adding its shares and users, and
mounting it move into Go as the fixture (§4.2). Today that setup is shell, copied into each
suite's scripts, and it is where §1's silent passes come from.

What carries over:

- `test/harness/bin/dt` stays the path; CI on `dev/test-harness` already calls it. It becomes a
  short shim that builds `./test/harness/cmd/dt` once per source change and runs it.
- Today's commands stay as aliases: `dt unit` is `dt run unit`, and
  `dt pynfs --profile badger` is `dt run pynfs/badger`.
- The grading rules: known-failure matching, smbtorture's connection-failure filter and
  truncation handling, and pynfs's refusal to pass a scoped run that skipped the tests it named.
  Each moves into Go, with the graders' existing test cases as its test data; until a kind is
  ported, its script grader runs as it is.
- The known-failure files, `setup.sh` for the scenarios, `dfsbench` and the integration package
  list.

What goes, one suite at a time (§8):

- `test/conformance/run.sh`, which dispatches cells: `dt` plans and runs them, and `suites.json`'s
  profiles, PR subsets and known-failure paths move into the registry.
- Each suite's setup scripts (`setup-posix.sh`, `teardown-posix.sh`, the two `bootstrap.sh`,
  `compose-env.sh`) and the setup half of `smb-conformance/run.sh`, `smbtorture/run.sh` and
  `run-pynfs.sh`. The fixture replaces them, and the tools are called directly.
- The Makefile's `test-*` targets and `flake.nix`'s `dfs-posix`, which only call scripts.

## 4. Design

```mermaid
flowchart LR
  R[test/suites.yaml] --> P[dt plan]
  P --> X{where}
  X -->|host| F[fixture: server, stores, shares, users, mount]
  X -->|dtc container| F
  X -->|dedicated host| F
  F --> E[the tools, called directly]
  E -->|test2json, TAP, subunit, TRX, pynfs and fio JSON, exit codes| N[normalise and grade]
  K[known failures and quarantine] --> N
  N --> S[results/RUN: summary.json, junit, report.md, logs]
  S --> T[terminal]
  S --> J[job summary]
  S --> I[develop-status issue]
  S --> H[dt history]
```

The tools are the ones the suites already use: `go test`, `prove` with pjdfstest, `pynfs`,
`smbtorture` and WPTS in their containers, `fio`, `dfsbench`, and `setup.sh` for the scenarios.
The registry, the planner, the fixtures, normalising, grading and the result files are the new
parts.

### 4.1 The registry: `test/suites.yaml`

One file lists every suite. A suite is a command that runs many tests. Tests inside a suite are
found by the suite, not listed here: Go tests by `go test`, scenarios and fio jobs by a file glob,
pynfs tests by pynfs. Adding a test is adding a file, and touches no shared file.

This is the format. The values show the intended first version.

```yaml
# test/suites.yaml: every test suite, one entry each. `dt list` prints it; `dt plan` expands it.
version: 1

defaults:            # any suite field; a suite's own value wins
  kind: cmd          # how results are read (see the table below)
  tier: full         # the smallest tier that runs the suite: pr < full < nightly; manual never auto
  timeout: 30m       # for the whole suite, or for each test when `tests` is set

suites:
  # The one-line form, for a hypothetical suite: a name and a command. Exit status 0 is a pass.
  nfs-mount-smoke: { cmd: test/nfs/mount-smoke.sh, tier: pr, needs: [linux, root, nfs-client] }

  # Lint. `job` keeps the check names the develop ruleset requires; each job runs `dt run NAME`.
  # The scripts hold what lint.yml runs inline today, so a laptop and CI run the same steps.
  lint-go:    { cmd: test/lint/go-checks.sh, tier: pr, job: Go Checks }      # gofmt, vet, citations, required tests, golangci-lint
  lint-repo:  { cmd: "test/lint/repo-checks.sh {base}", tier: pr, job: Repo Checks }
  shellcheck: { cmd: test/lint/shellcheck.sh, tier: pr, job: ShellCheck }

  unit:
    kind: go
    cmd: go test -race {args} -timeout=25m ./pkg/... ./internal/... ./cmd/... ./test/...
    args: { pr: "-short" }              # pull requests run the short suite, as today
    tier: pr
  unit-windows:
    kind: go
    cmd: go test -short -race -p 2 -timeout=25m ./...
    tier: pr
    needs: [windows]                    # dt plan gives the cell a Windows runner
  integration:                          # includes the KMIP interop tests since #2955
    kind: go
    cmd: test/integration/packages.sh | xargs go test -tags=integration -p 1 -timeout=20m
    tier: pr
    needs: [docker, service:postgres, service:kmip]
  integration-portmap:                  # the tests skip without rpcbind; `needs` makes that a refusal
    kind: go
    cmd: go test -tags=portmap_system -timeout=2m ./test/integration/portmap/...
    tier: pr
    needs: [linux, rpcbind]
  operator:                             # its own Go module; the kustomize render check becomes a make target
    cmd: make -C k8s/dittofs-operator test lint manifests-verify
    tier: pr
  e2e:
    kind: go
    cmd: .github/scripts/run-e2e.sh go test -tags=e2e -timeout=30m ./test/e2e/...
    env: { nightly: { DITTOFS_E2E_NIGHTLY: "1" } }   # the dedup tests no workflow runs today
    needs: [linux, root, nfs-client, smb-client, service:localstack]

  # Conformance. Each tool is called directly, against the suite's fixture (§4.2). `matrix` and
  # `select.pr` replace suites.json's profiles and PR lists. Commands are shortened: the flags each
  # tool needs are in its script today, and move here with it.
  pjdfstest:
    kind: tap                           # each test file prints TAP; no prove summary to parse
    cmd: "cd {mount} && prove -v {test}"
    tests: "{tools}/pjdfstest/tests/*/*.t"
    matrix: { profile: [memory, badger, badger-s3], nfs: ["3", "4", "4.1"] }
    select: { pr: { profile: [memory, badger-s3] } }
    fixture:
      profile: "{profile}"
      nfs: "{nfs}"                      # mounted at this version; {mount} is where
      squash: root_to_admin             # pjdfstest needs a privileged root
      users: [65532, 65533, 65534]
      delegations: false                # the server must see every operation
    known: { nfs: { "3": test/posix/KNOWN_FAILURES.md, "4": test/posix/KNOWN_FAILURES_V4.md, "4.1": test/posix/KNOWN_FAILURES_V4.md } }
    tier: pr
    needs: [linux, root, nfs-client]
  pynfs:
    kind: pynfs                         # reads the --json file pynfs already writes
    cmd: "pynfs-{minor} {server}/export --security sys --maketree --json {out}/pynfs.json all"
    matrix: { profile: [memory, badger, badger-s3], minor: ["4.0", "4.1"] }
    select: { pr: { profile: [memory, badger-s3] } }
    fixture: { profile: "{profile}", nfs: export, lease: 30s, users: [1] }   # pynfs mounts nothing; uid 1 is its second client
    known: { minor: { "4.0": test/nfs-conformance/pynfs/KNOWN_FAILURES_V40.md, "4.1": test/nfs-conformance/pynfs/KNOWN_FAILURES_V41.md } }
    tier: pr
    needs: [linux]
  wpts:
    kind: trx                           # WPTS's own TRX file
    image: mcr.microsoft.com/windowsprotocoltestsuites:fileserver-v8   # its entrypoint runs the tests
    env: { Usage: RunTestCases, Filter: TestCategory=BVT }
    matrix: { profile: [memory, badger, badger-s3] }
    select: { pr: { profile: [memory, badger-s3] } }
    fixture:
      profile: "{profile}"
      smb: true
      shares: test/smb-conformance/shares.yaml    # the 8 shares and their flags, as data
      setup: test/smb-conformance/wpts-config.sh  # writes WPTS's own config files; see §9
    known: test/smb-conformance/KNOWN_FAILURES.md
    tier: pr
    needs: [linux, docker]
  smbtorture:
    kind: subunit                       # smbtorture's test:, success: and failure: lines
    image: quay.io/samba.org/samba-toolbox:v0.8
    cmd: "smbtorture //{server}/smbbasic -U{user}%{password} {test}"
    tests: test/smb-conformance/smbtorture/tests.txt   # the list smbtorture/run.sh holds today, with each test's timeout
    matrix: { profile: [memory, badger] }
    fixture: { profile: "{profile}", smb: true, shares: test/smb-conformance/shares.yaml, reset: each }
    known: test/smb-conformance/smbtorture/KNOWN_FAILURES.md
    tier: pr
    needs: [linux, docker]
  nfs-kerberos:                         # a test of our own; its exit 0 without the NFS module goes (R10)
    cmd: test/nfs-conformance/nfs-client/test-nfs-krb5.sh
    fixture: { profile: memory, nfs: export, kerberos: true }
    needs: [linux, docker, nfs-client]

  scenarios:
    kind: scenarios
    cmd: test/scenarios/setup.sh {test}
    tests: test/scenarios/[0-9][0-9]-*.sh
    select: { full: "[0-8]?-*.sh" }     # nightly runs all; the 0x smoke group joins pr once timed
    known: test/scenarios/KNOWN_FAILURES.md
    needs: [linux, podman]
    timeout: 15m
    shards: 4                           # split over 4 CI jobs
  fio-verify:
    kind: fio
    cmd: test/harness/fio/run.sh {test}
    tests: bench/workloads/*.fio
    tier: nightly
    needs: [linux, root, nfs-client, smb-client]

  bench-smoke: { kind: dfsbench, cmd: go run ./cmd/bench run --smoke --local, tier: nightly, gate: false }

  # Suites that have workflows of their own today.
  ad-dc:                { kind: go, cmd: test/integration/ad-dc/run.sh, needs: [linux, docker] }
  kerberos-integration: { kind: go, cmd: test/integration/kerberos/run.sh, tier: nightly, needs: [linux, docker] }  # run by no CI job today
  smbtorture-kerberos:
    kind: subunit
    image: quay.io/samba.org/samba-toolbox:v0.8
    cmd: "smbtorture //{server}/smbbasic -U{user}@{realm}%{password} --use-kerberos=required {test}"
    tests: [smb2.session, smb2.read, smb2.lock]
    fixture: { profile: memory, smb: true, shares: test/smb-conformance/shares.yaml, kerberos: true }
    needs: [linux, docker]
  smb-client-linux:   { cmd: test/smb-client-compat/linux.sh, tier: nightly, needs: [linux, root, smb-client] }
  smb-client-macos:   { cmd: test/smb-client-compat/macos.sh, tier: nightly, needs: [macos] }
  smb-client-windows: { cmd: pwsh -File test/smb-client-compat/windows.ps1, tier: nightly, needs: ["windows", "port:445"] }
  combined-tree-selftest: { cmd: .github/scripts/combined-tree.sh selftest, tier: pr }

  # Baselines record what a reference implementation does; they never fail a run.
  smbtorture-baseline:  { cmd: test/smb-conformance/smbtorture/refresh-baseline.sh, tier: nightly, gate: false, needs: [linux, docker] }
  pynfs-knfsd-baseline: { cmd: test/nfs-conformance/pynfs/baseline-knfsd.sh, tier: nightly, gate: false, needs: [linux, root, knfsd] }

  # Rigs that need special hardware or infrastructure: listed, run by hand, refused elsewhere with the reason.
  crash-device-loss:   { cmd: "sudo test/crash/device-loss.sh {bin}/dfs {bin}/dfsctl", tier: manual, needs: [linux, root, dm-flakey, smb-client] }
  crash-cold-loss:     { cmd: "sudo test/crash/invalidate-cold-loss.sh {bin}/dfs {bin}/dfsctl", tier: manual, needs: [linux, root, smb-client] }
  edge:                { cmd: test/edge/edge-test.sh all, tier: manual, needs: ["cloud:scaleway"] }
  repro:
    cmd: test/harness/repro/{test}
    tests: test/harness/repro/*-*.sh
    tier: manual
    needs: [docker]
```

| Field | Meaning | Default |
|---|---|---|
| `cmd` | Shell command, run with bash from the repository root (Git Bash on Windows), or inside `image`. `{test}` is one test's file name or list entry, `{args}` the tier's `args`, `{base}` the ref a change is compared against (the PR's base in CI, `origin/develop` locally), `{bin}` a directory with `dfs` and `dfsctl` built from the checkout, `{tools}` the pinned test tools (pjdfstest, pynfs) from `flake.lock`, `{out}` the cell's results directory, and each `matrix` key its value. The fixture adds `{server}`, `{mount}`, `{user}`, `{password}` and `{realm}` (§4.2) | required, unless the kind or the image provides one |
| `kind` | How results are read: `cmd`, `go`, `tap`, `subunit`, `trx`, `pynfs`, `scenarios`, `fio`, `dfsbench` (§4.6) | `cmd` |
| `tier` | The smallest tier that runs the suite (§4.3) | `full` |
| `timeout` | A duration, for the suite, or for each test when `tests` is set | `30m` |
| `needs` | What the machine must offer (§4.5) | none |
| `tests` | A glob, one test per matching file; a list of names; or a `.txt` file of names, one per line, each with an optional timeout | none: the suite is one test, or finds its own |
| `matrix` | Axes and their values; `dt plan` makes one cell per combination | one cell |
| `fixture` | The DittoFS server the tests run against (§4.2) | none: the suite starts what it needs itself |
| `image` | A container image to run `cmd` in, on the fixture's network | none: the host, or `dtc` |
| `env` | Environment variables, for every tier or per tier | none |
| `select` | Per tier below the suite's widest: a narrower glob for `tests`, or the `matrix` values that tier runs | every match, every value |
| `args` | Text per tier, put where `{args}` appears | empty |
| `known` | A known-failure list, in the Markdown format `test/common/known-failures.sh` reads; or a map from one `matrix` axis's values to lists | none |
| `gate` | `false`: results are recorded but never fail a run (benchmarks) | `true` |
| `shards` | Split `tests` over this many CI jobs | `1` |
| `job` | Run in a CI job of this fixed name, outside the matrix: for checks the branch ruleset requires by name | none: a matrix cell |

Rules `dt` enforces when it loads the file: names are `[a-z0-9-]+`; unknown fields are refused, so
a misspelt field fails rather than being ignored; `version` newer than `dt` knows is refused; every
`known`, `shares` and `setup` path exists; every `{test}` has a `tests`; every placeholder is one
the suite defines.

**One file, or a file per test?** One registry of suites, and no per-test file. Merge conflicts in
a central file come from adding tests, and tests are found from their own files. The registry only
changes when a suite is added, which is rare. What belongs to a single test lives in that test:
a scenario's size is in its file name, a fio job's parameters in its job file.

**YAML or JSON?** `suites.json` chose JSON so that Actions could `fromJSON()` it and scripts could
read it with `jq`. Workflows never parse the file itself, though; they parse `run.sh --matrix`
output. `dt plan --json` gives them the same, from YAML. `suites.json` goes with `run.sh`: its
profiles become `matrix`, its PR lists `select.pr`, its known-failure paths `known`. The two other
readers, `ci-health.yml` and `test/conformance/check-docs.sh`, read `dt list --json` instead.

### 4.2 Fixtures: the DittoFS a suite runs against

A fixture is the server a suite's tests run against: `dfs` built from the checkout, on a store
profile, with its shares and users, and mounted when the suite needs a mount. `dt` sets it up in Go
before a cell's first test and removes it after the last. A suite says what it needs; it doesn't
carry a script that builds it.

| Field | Meaning |
|---|---|
| `profile` | The store profile: `memory`, `badger` or `badger-s3`. For an S3 profile `dt` creates the bucket |
| `nfs` | Mount the share over NFS at this version (`3`, `4`, `4.1`); `export` exports it without a mount, for pynfs |
| `smb` | `true` enables the SMB adapter |
| `shares` | A file of shares and their flags, as data; the default is one share, `/export` |
| `users` | UIDs to create, each with a read-write grant on every share |
| `squash`, `delegations`, `lease` | Export and adapter settings, set through `dfsctl` |
| `kerberos` | `true` starts the KDC service, writes the keytabs and turns Kerberos on |
| `reset` | `each`: recreate the shares before every test, as smbtorture's runner does today |
| `setup` | A script run after the rest, for what the fields can't say; it gets the placeholders as environment variables |

It gives the command `{server}` (the address the tools reach it at), `{mount}`, `{realm}`, and
`{user}` and `{password}`: a test user whose password is generated per run, in place of the WPTS
password now written in six places.

Before the first test, the fixture checks itself: the server answers, an S3 profile's bucket
exists, the mount is there at the version asked for, and every `needs` holds. A failed check ends
the cell with exit 2, never a pass (R10). That check is what the two silent passes in §1 lacked.

`dt` stops only the processes it started. Today `teardown-posix.sh` kills every `dfs start` on the
machine, a developer's own server included. Each cell's results go to its `{out}`, the same layout
for every suite, where today five workflow steps look for the newest directory with `ls -td`.

### 4.3 Tiers

| Tier | Runs on | Contains | Budget (wall clock) |
|---|---|---|---|
| `pr` | every pull request (and `merge_group`, if a merge queue is ever available) | suites with `tier: pr`, narrowed by `select.pr` | p90 ≤ 20 min |
| `full` | every push to develop | `pr` plus `tier: full` | ≤ 60 min |
| `nightly` | schedule, and the dedicated host | everything except `manual` | ≤ 4 h |
| `manual` | `dt run NAME`, or `workflow_dispatch` | benchmarks and long simulations | none |

Each tier contains the one below it. For conformance suites, `select.pr` holds `suites.json`'s
`pull_request` profile lists, and `full` and `nightly` run every profile, as `push` and `schedule`
do today. Which profiles run on a PR doesn't change.

The budgets are proposals, against today's 25–40 min. Every result records its duration, and
`dt history` gives p50 and p90 per suite, so moving a suite between tiers is a one-word change
made on measured numbers.

There's no selection by changed layer. The components depend on each other too much for "this PR
only touched NFS" to be safe. The full tier after every merge is what catches the cross-component
regressions a small PR tier misses.

### 4.4 The command line

```text
dt list [--tier T] [--json]                    suites, their tier, kind and needs, and whether they can run here
dt plan --tier T [--shard I/N] [--json]        the cells a tier runs; CI turns --json into its matrix
dt run SUITE[/CELL] [TEST...] | --tier T       run, grade and write results; exit status below
dt report [RUN] [--format text|md|json]        show a run; CI appends --format md to the job summary
dt history [SUITE[/TEST]] [--ci] [--last N]    pass, fail and flaky counts over past runs, with durations
dt doctor                                      which `needs` this machine meets, and how to meet the rest
dt known prune SUITE                           remove known failures that now pass
```

The environment commands stay as they are: `dt services`, `dt stack`, `dt cleanup`, `dt hooks`,
and `dtc` for the container.

| Flag | Effect |
|---|---|
| `--ci` | non-interactive; GitHub annotations for new failures; writes the job summary; a suite that can't run here is an error, not a skip |
| `--retry N` | re-run failed tests up to N times (default 0; CI passes 1 on develop and the schedule, never on pull requests, §4.7) |
| `--in host\|container\|auto` | where to run; `auto` uses `dtc` when the host lacks a need the container provides |
| `--results DIR` | where results go (default `test/harness/results/`, gitignored) |

| Exit status | Meaning |
|---|---|
| 0 | no new failures (known, quarantined and flaky ones are reported, not counted) |
| 1 | at least one new failure |
| 2 | something couldn't run: unmet needs, a fixture that failed its check, setup failed, the harness's own timeout, or nothing ran |
| 3 | usage or registry error |

Status 1 is a regression; status 2 is an environment problem. The alert in §4.8 treats them
differently, which is the main defence against another flood.

### 4.5 Where a suite runs

`needs` names what a suite requires: `linux`, `windows`, `macos`, `root` (passwordless sudo),
`docker`, `podman`, `rpcbind`, `knfsd`, `nfs-client`, `smb-client`, `kerberos`, `dm-flakey`,
`disk:<GB>`, `port:<n>` (free to bind), `cloud:<provider>` (credentials and provisioned
infrastructure), and `service:<name>` for the services `dt services` starts (`localstack`,
`postgres`, `kmip`).
`dt doctor` reports which are met. For an unmet need, `dt run` either moves to the `dtc` container
(when it provides it and `--in` allows), or reports the suite as not runnable with the reason. It is
never reported as passed. In CI, `dt plan` gives each cell its runner from the same list:
`windows` and `macos` get those hosted runners, everything else Ubuntu.

There are three places to run:

| Place | Gives | Used for |
|---|---|---|
| A developer's machine | the host, or `dtc` (pinned Linux toolchain, privileged) | anything the machine can meet; macOS runs Linux-only suites in `dtc` |
| GitHub-hosted runner | Linux, 4 CPU, 16 GB RAM, 14 GB SSD (public repositories); the Ubuntu 24.04 image has Docker, podman 4.9.3 and Go 1.26.8 | the `pr` and `full` tiers, and the nightly suites that fit |
| Dedicated test host | a large disk and long run times | nightly suites a runner can't hold: the 9x long runs, such as scenario 92, which grows a 30 GB file |

Scenarios need podman 5.6 or later (`test/scenarios/README.md`), and the hosted image has 4.9.3.
On a runner they use the path the README already gives for macOS: `setup.sh` inside
`quay.io/podman/stable`, privileged. The README limits that to a VM, never a shared host; a hosted
runner is a single-use VM. So macOS and CI share one code path.

The dedicated host is **not** a self-hosted runner. GitHub's guidance is that "self-hosted runners
should almost never be used for public repositories", because any pull request could run code on
them. The host runs `dt run --tier nightly` from cron, on develop's tip only, and publishes its
summary to the develop-status issue (§4.8) with a token that can only write issues.

Versions come from one place. Today Go is `1.26.0` in `go.mod`, `1.26.x` in the workflows, `1.26.8`
in the `dtc` image, and a floating `1.26` in the e2e-linux image. The pjdfstest and pynfs revisions
are copied into Dockerfiles from `flake.lock`. Proposed: one Go pin (a `toolchain` line in `go.mod`),
read by the workflows and the images, and the test-tool revisions read from `flake.lock`.

### 4.6 Results

Each run writes one directory:

```text
test/harness/results/20261007T201500Z-d0efaa75/
  summary.json        CTRF: the run, every test of every cell, and how each was graded
  junit/*.xml         one file per cell, generated from summary.json
  report.md           what `dt report --format md` prints
  logs/*.log          one per cell, with the command, commit, exit status and duration
```

```json
{
  "reportFormat": "CTRF",
  "specVersion": "0.1.0",
  "generatedBy": "dt",
  "results": {
    "tool": { "name": "dt" },
    "summary": { "tests": 8789, "passed": 8787, "failed": 1, "skipped": 1, "pending": 0,
                 "other": 0, "flaky": 1, "start": 1791404100000, "stop": 1791405934000 },
    "tests": [
      { "name": "chmod/12.t", "suite": ["pjdfstest", "badger-s3", "4.1"], "status": "failed",
        "duration": 2140, "message": "not ok 7", "extra": { "grade": "new" } },
      { "name": "open/03.t", "suite": ["pjdfstest", "badger-s3", "4.1"], "status": "skipped",
        "rawStatus": "failed", "duration": 860,
        "extra": { "grade": "known", "reason": "PATH_MAX: the mount-point prefix pushes the path over the limit" } },
      { "name": "rename/09.t", "suite": ["pjdfstest", "badger-s3", "4.1"], "status": "passed",
        "duration": 3310, "flaky": true, "retries": 1 }
    ],
    "environment": { "commit": "d0efaa75", "branchName": "develop", "osPlatform": "linux" },
    "extra": {
      "run": "20261007T201500Z-d0efaa75", "dirty": false, "tier": "full", "in": "host", "exit": 1,
      "cells": [
        { "suite": "pjdfstest", "cell": "badger-s3/4.1", "result": "fail", "seconds": 912,
          "log": "logs/pjdfstest-badger-s3-4.1.log", "junit": "junit/pjdfstest-badger-s3-4.1.xml" }
      ]
    }
  }
}
```

The numbers and outcomes above are illustrative; the passing tests are left out. How each kind
becomes test cases:

| Kind | Reads | Test case | Change needed |
|---|---|---|---|
| `go` | `go test -json` (test2json events) | each test and subtest | `dt` adds `-json`; no new tool |
| `tap` | the TAP each pjdfstest file prints | each test file, as the known-failure lists name them | none; prove's text summary is no longer parsed |
| `subunit` | smbtorture's `test:`, `success:`, `failure:`, `error:` and `skip:` lines | each smbtorture test | the connection-failure filter and the truncation handling move from `parse-results.sh` into Go, with its test cases |
| `trx` | WPTS's TRX file | each WPTS test | none; the workflow's second parse of the TRX goes |
| `pynfs` | the `--json` file pynfs writes | each pynfs test code | none in pynfs; nothing parses its text log any more |
| `scenarios` | `setup.sh`'s PASS/FAIL/SLOW lines, `failed/<name>/`, `times/<name>.tsv` | each scenario; the failing line is the message | none in `setup.sh`. `dt` checks a `times` row exists for this run, because `setup.sh` exits 0 without running anything when another run holds its lock |
| `fio` | `fio --output-format=json` | each job | data checks (`verify=`) fail the case; bandwidth, IOPS and latency are recorded as metrics |
| `dfsbench` | its JSON result per cell | each cell | recorded only (`gate: false`) |
| `cmd` | the exit status | the command | none |

**Grades in CTRF and JUnit.** A new failure is `failed` in CTRF and a `<failure>` in JUnit. A known
failure is `skipped` with `rawStatus: failed` and its reason and issue in `extra`, and `<skipped>`
with the reason in JUnit; so is a quarantined test. A test that failed and then passed on retry is
`passed` with `flaky: true` and its `retries`, and a pass with a `flaky` property in JUnit. An
unexpected pass is `passed`, marked in `extra`. CTRF and JUnit readers then count only new
failures as failures.

**Why CTRF, with JUnit beside it.** JUnit XML began with Java's JUnit. It has no formal
specification, tools write it in their own dialects, and it has no field for a retry or a flaky
pass. It is still what CI services read, whatever the language: GitLab's test reports and Codecov
Test Analytics accept JUnit XML and nothing else, and GitHub has no reader of its own, so the
reporting actions read JUnit. CTRF (Common Test Report Format) is a JSON schema with retries, flaky
passes and the tool's own status as fields, which is what grading needs, and
`ctrf-io/github-test-reporter` writes job summaries and PR comments from it. No CI service reads it
natively yet. So `summary.json` is CTRF, with our grading in its `extra` fields, and the JUnit
files are generated from it for whatever reads JUnit: Codecov, for one, which the unit job already
uploads coverage to. CTRF is at version 0.1.0, so `dt` writes one pinned version (§7).

In CI, `dt report --format md` goes to the job summary. One shared step replaces the ten inline
summary steps, including the two that always print green. `summary.json` and `junit/` are uploaded
as artifacts. `dt history --ci` reads the last N develop runs' artifacts through the GitHub API;
the default 90-day retention covers flake rates. Benchmark trends need longer, and can move to a
data branch when they do.

### 4.7 Grading: known failures, quarantine, retries

- **Known failure:** a deterministic failure tracked by an issue. It's listed in the suite's
  known-failure file, in the existing Markdown format. The Go grader shares golden fixtures with
  `test/common/known-failures_test.sh`, so the two parsers can't drift. Today `run.sh` resolves a
  suite's `known_failures` path but never passes it to the steps, each runner picks its own file
  again, and WPTS's grader doesn't use the shared matcher, so wildcard rows wouldn't apply to it.
  With `known` in the registry, `dt` reads the list itself and is the only matcher.
- **Quarantine:** a flaky test, failing some runs and not others. `test/harness/quarantine.yaml`
  lists it with the suite, the test, an issue and an expiry date. A quarantined test still runs and
  its result is recorded, but it never fails a run and never alerts. An expired entry is an error
  from `dt doctor` and the registry check, so the list can't become a dumping ground. The AD-DC
  failures in §1 are what it's for, once that workflow reports through `dt`:
  `TestADCombinedKeytabAndDomainAwareSMB` (#2968) and `TestSMBNTLMNetlogonPassthrough`, which has
  no issue yet.
- **Retry:** none on pull requests. A test that fails on a PR fails the PR unless it's
  quarantined, so a PR can't merge a flaky test it introduced. On develop and the schedule,
  `--retry 1` runs only the failed tests again: `go test -run '^(A|B)$'`, the one scenario, the one
  test file or test code. A pass on retry there is flaky: it doesn't open the develop-red issue,
  and `dt history` counts it. A test flaky twice in a week on develop is a quarantine candidate.
- **Unexpected pass:** a known failure that passes is reported, and doesn't fail the run.
  `dt known prune` removes it. A fix PR removes its own row.
- **Scenarios:** each one that fails today gets a known-failure row naming its issue. The suite is
  then green, a regression shows as a new failure, and a fix shows as an unexpected pass.

### 4.8 CI

One workflow, `tests.yml`, runs every registered suite:

- **Triggers:** `pull_request` runs tier `pr`, a push to develop `full`, the schedule `nightly`,
  and `workflow_dispatch` the tier it's given.
- **Jobs:** `plan` runs `dt plan --tier T --json`. `run` is a matrix over its cells, each
  `dt run CELL --ci` (with `--retry 1` on develop and the schedule, never on PRs), on the runner
  `dt plan` named. `gate` needs `run`, always runs,
  merges the summaries into one report, and fails unless every cell exited 0.
- **Lint:** the suites with a `job` run as fixed jobs of that name: `Go Checks`, `Repo Checks` and
  `ShellCheck`, each `dt run NAME --ci`. They move here from `lint.yml` with their names, so the
  develop ruleset's required checks don't change.
- **Docs-only PRs:** `dt plan` returns no cells, and `gate` passes.

`Tests / gate` becomes the one required test check, once the `pr` tier has been green on develop for
two weeks. A single gate that always runs also avoids GitHub's path-filter trap: a required check
from a workflow skipped by a path filter stays pending and blocks the merge.

Every suite is in the registry, including those with special hosts or schedules: AD-DC, the
Kerberos suites, SMB client compatibility on Linux, macOS and Windows, the baseline refreshes, and
the manual rigs. Their workflows move into `tests.yml` after the main ones (§8 step 4); until
then they keep running as they are. The client-compatibility jobs (601 lines across three jobs)
and the smbtorture baseline refresh first move their inline steps into scripts. The nightly job
that refreshes the smbtorture baseline still commits the refreshed file, as today.

Three things stay outside the registry, because they aren't test suites:
- **the security scans,** gitleaks and CodeQL (Analyze (go)), which are GitHub actions;
- **combined-tree,** which picks open PRs to merge and build together (see Merging below); its
  self-test is a suite;
- **the canary** on `dev/test-harness`, which watches a live deployment rather than a commit.

**Develop goes red.** This extends `ci-health.yml` and watches `tests.yml`, plus any workflow still
outside it.

- After its existing retry, a full-tier run that still has new failures (exit 1) opens one issue
  labelled `develop-red`, or updates it if one is open.
- The issue body is the current failing set, from `summary.json`. It's rewritten, not commented,
  so watchers get one notification per change, not per run. A comment is added only when the set
  of failing tests changes.
- The next green run on develop closes it.
- Exit 2 (an environment problem) alerts only after two consecutive runs.
- Known failures and quarantined tests never alert.
- E-mail comes from GitHub's own issue notifications: repository watchers, and anyone the issue
  mentions. No mail secrets in the repository.

The removed alarm already retried once and kept a single issue. It still flooded, because it sent
mail on every red run and couldn't tell a flaky subtest from a regression (#1705). Here a flaky
test is quarantined and never counts, a known failure never counts, and a notification goes out
only when the failing set changes.

**Merging.** GitHub's merge queue is "available in any public repository owned by an organization".
This repository is owned by a user account, so it can't be enabled here. Since 2026-10-07 develop
requires PR branches to be up to date and the required checks to pass. Every merge to develop
therefore makes open PRs re-run, which a fast `pr` tier keeps cheap. `combined-tree.yml` already
covers part of what a queue would: every three hours from 07:20 to 19:20 UTC, it merges
combinations of open same-repository PRs and builds, vets and tests them, catching two green PRs
that break each other. If the
repository moves to an organization: enable the queue, add `merge_group` to `tests.yml`'s
triggers, and drop the up-to-date rule.

### 4.9 Adding things

- **A suite:** one line in `test/suites.yaml`, such as `nfs-mount-smoke` above. `dt list`
  shows it, and `dt run nfs-mount-smoke` runs it.
- **A test to an existing suite:** add its file. A scenario script, a fio job file
  (`bench/workloads/*.fio`, which `dfsbench` also embeds), or a Go test. No registry change.
- **New fio workloads,** such as mailbox-style I/O mixes: job files in `bench/workloads/`.
  `fio-verify` checks their data, and `dfsbench` can measure them.
- **A known failure:** one row in the suite's list, naming the issue.

### 4.10 Benchmarks and long runs

Performance is tracked, not gated. Hosted runners share hardware and are too noisy to fail a PR
on a percentage. `bench-smoke` (`dfsbench --smoke`, documented as needing no secrets) runs nightly
on a runner, as a check that the benchmark still works. Comparisons run on the dedicated host. A
suite whose result is more than an agreed margin below its 7-day median for three nights in a row
adds a note to the develop-status issue.

The metadata-scale benchmark (many files, metadata only) and the journal simulation (many small
files against large segments under heavy writes) are `manual` suites on the dedicated host. They
use the same result format, so runs compare over time.

## 5. What this doesn't change

- **The tests and what they are graded against.** pjdfstest, pynfs, WPTS, smbtorture, the
  scenarios and the Go tests run as they are, against the same known-failure files and the same
  grading rules. What changes is around them: their setup scripts give way to fixtures (§4.2).
- **`setup.sh`.** The scenarios kind reads what it already writes.
- **The required check names.** Go Checks, Repo Checks and ShellCheck keep their names and their
  steps, which run through `dt` from scripts instead of inline YAML. The security scans, gitleaks
  and CodeQL (Analyze (go)), stay GitHub actions as they are.
- **The workflows with special hosts, for a while.** Their suites are registered at once, but their
  workflows keep running until §8's step 4 moves them.

## 6. Alternatives considered

- **An API service that runs tests.** Rejected in §3: it breaks "same command everywhere", and
  needs hosting.
- **Growing `dt` in bash.** Rejected in §3: YAML, JSON and JUnit handling in shell, without tests.
- **A descriptor file per test.** Rejected in §4.1: tests are already found from their own files.
- **Selecting suites by changed layer.** Rejected in §4.3: the components depend on each other too
  much.
- **Keeping `suites.json` as the registry.** Rejected in §4.1: it was JSON for `fromJSON()`, and
  `dt plan --json` covers that. Its content moves into the registry.
- **Keeping each suite's scripts and wrapping them.** This was the first draft's R9. Rejected: a CI
  cell would run `dt`, then `run.sh`, then the suite's scripts, then the tool, and the setup copied
  between those scripts is where the silent passes in §1 come from. The grading rules, which have
  tests, carry over (§3).
- **make or a task runner (Taskfile) as the structure.** Rejected: they give named commands, which
  `dt run NAME` gives too, but not the matrix, `needs`, fixtures, grading or results. The
  Makefile's `test-*` targets show the limit: they pass `$(ARGS)` to the scripts, and no workflow
  uses them. Task isn't in the dev shell either.
- **JUnit XML as the main result format, or a JSON schema of our own.** JUnit has no field for a
  retry or a flaky pass, and our own schema would be one more format to specify and render. CTRF
  has both fields and a GitHub reporter, and JUnit is generated from it (§4.6).
- **Retrying failed tests on pull requests.** Rejected in §4.7: a PR could merge a flaky test it
  introduced.
- **A self-hosted runner on the dedicated host.** Rejected in §4.5, on GitHub's own guidance for
  public repositories.
- **A merge queue now.** Not available, for the owner-type reason in §4.8.

## 7. Risks

- **One bug in `dt` breaks all of CI.** Workflows move one at a time (§8), each compared against
  its old run for a week. `dt`'s own tests run in CI with the unit tests: `./test/...` is added to
  the unit job, which today covers only `./pkg`, `./internal` and `./cmd`. That also brings in
  `test/spec-citations`' tests, which no job runs today.
- **The `pr` tier misses something.** The full tier runs after every merge, and the alert names
  the failing set. The cost is a red develop for one merge, not a lost regression.
- **The known-failure lists rot.** Every row names an issue, unexpected passes are reported, and
  `dt known prune` removes them.
- **Quarantine hides real bugs.** Entries expire, need an issue, and quarantined results are still
  recorded and shown in `dt history`.
- **A fixture misses something a setup script did.** Each suite moves on its own (§8), and its old
  job runs beside its `dt` cell for a week; the scripts are deleted only after the two agree. The
  scripts' behaviours that have tests move with those tests.
- **CTRF changes under us.** It is at 0.1.0. `dt` writes one pinned version, and the JUnit files,
  which carry the same results, don't depend on it.

## 8. Plan

Each step ships on its own and has an exit criterion.

Two fixes don't wait for any step, because today they report passes for tests that didn't run
(§1): create the bucket the pjdfstest `-s3` cells name, and make the NFS Kerberos test fail, not
exit 0, when it can't load the NFS module.

1. **Land the harness branch.** Rebase `dev/test-harness` on develop, and fix what has gone stale
   or wrong on it:
   - its Postgres tuning and services, and the `postgres` and `postgres-s3` profiles, predate
     #2914, which removed them; `dt-batch container`'s `pjdfstest v4.1 / postgres-s3` run becomes
     `badger-s3`;
   - its separate KMIP interop step repeats tests that CI's integration list includes since #2955;
   - `dt-batch` exits 0 whatever its runs did; it exits non-zero when any run failed;
   - its pre-push replaces the repository's hook, and stops testing `k8s/dittofs-operator/`
     packages on push: restore the repository hook, then add `dt`'s steps to it;
   - the references to `LOCAL-DEV-SETUP.md` and `TEST-HARNESS.md` point at files that don't exist.

   Until step 2, add a `dt lint` command and a `dt-batch full` set as the one command for a
   complete run: unit (`--full --fresh`), integration, lint, then the `container` set, with the
   three e2e failures the README expects inside `dtc` (no `rpcsec_gss_krb5`, no veth pairs)
   counted as expected. Roughly 90 minutes on the README's reference machine, from its times; the
   unit run without `-short` wasn't measured there.
   *Exit:* CI's unit, integration and e2e jobs call `dt`, and stay green for a week.
2. **Registry and runner.** `test/suites.yaml`, and `dt list`, `plan` and `run` in Go, for the
   `go` and `cmd` kinds. `suites.json`'s profiles, PR lists and known-failure paths move into the
   registry; `ci-health.yml` and `check-docs.sh` read `dt list --json`. `lint.yml`'s inline steps
   move into `test/lint/`, run by the `lint-go`, `lint-repo` and `shellcheck` suites. Every other
   suite is registered too, the manual rigs included, so `dt list` shows everything there is.
   Today's commands become aliases.
   *Exit:* `dt plan --tier pr --json` yields the same cells as today's PR matrix.
3. **Fixtures, kinds and results.** The fixture (§4.2); the `tap`, `subunit`, `trx` and `pynfs`
   kinds, with the graders' rules moved into Go against their existing tests; `summary.json` in
   CTRF, JUnit generated from it, and `dt report`. One report step replaces the inline summaries.
   *Exit:* each conformance suite gives the same results through `dt` as through its scripts, on
   every profile, and every CI test job publishes through `dt report`.
4. **One workflow and a gate.** `tests.yml` with plan, run and gate. Move `nfs-pynfs.yml` first,
   as the smallest, then `conformance.yml`, then the unit, Windows, integration, operator and lint
   jobs, the lint jobs keeping their names. Then AD-DC, the Kerberos suites, client compatibility
   (its steps moved into scripts first) and the baselines. Each conformance suite's old job runs
   beside its `dt` cell for a week; the PR that removes the old job deletes the scripts the fixture
   replaced. `run.sh`, `suites.json` and the Makefile's `test-*` targets go with the last of them.
   Make the gate required.
   *Exit:* PR p90 ≤ 20 min over a week, the gate required, and no conformance setup script left.
5. **Scenarios and fio.** The scenarios kind, its known-failure list, and the podman-in-container
   path on runners. Groups 0x–8x run in the full tier, sharded over runners; the 9x long runs run
   nightly on the dedicated host. Then `fio-verify`.
   *Exit:* the full tier includes scenarios, and has no new failures.
6. **Alerting and flakes.** The develop-status issue, quarantine, and `dt history`.
   *Exit:* two weeks with one alert per real regression and none from flakes.
7. **Benchmarks.** `bench-smoke` nightly; tracked runs on the dedicated host.

Conformance comes first, in steps 2–4, because its suites are already graded, then scenarios and
fio (R8).

## 9. Open questions

- Are the tier budgets right: 20 min p90 for `pr`, 60 for `full`, 4 h for `nightly`?
- Which conformance profiles stay on PRs? Today wpts, pjdfstest and pynfs run two of their three;
  smbtorture runs both of its two.
- Should `nfs-kerberos` drop its PR path filter and run only in `full`?
- The operator, Windows, unit and integration jobs only run on PRs that touch their paths today.
  With no selection by layer they run on every PR, adding about 3 min of operator and 17 min of
  Windows runner time per PR, in parallel. Keep them in `pr`, or move them to `full`?
- SMB client compatibility runs weekly today, plus on pushes that touch the SMB adapter. Is
  nightly right, given that it takes a macOS and a Windows runner?
- `kerberos-integration` and the e2e nightly tier have never run in CI, so their state is unknown.
  The harness README records the e2e nightly tests failing at setup. They start in `nightly` with
  whatever known-failure rows the first runs call for. Is that acceptable, or should they be
  fixed first?
- Who runs the dedicated host, and holds its issue-writing token?
- When does the gate become required: after two green weeks, or on a flake-rate figure?
- Does history stay in artifacts, or move to a data branch once benchmarks need longer than 90
  days?
- Do the fixture's fields cover WPTS's setup (8 shares with their flags, and an encryption toggle
  that restarts the adapter), or does WPTS keep a `setup:` script beside writing its own config?
- CTRF is at 0.1.0. Is a pre-1.0 schema acceptable for `summary.json`, given that the JUnit files
  carry the same results, or should `summary.json` stay a schema of our own?

## 10. What was checked

These were checked against the repository, at the commits in the header, before writing this.
Where the assumption was wrong, the design follows what was found.

| Assumed | Found | Where |
|---|---|---|
| `dt` covers every tier, on `dev/test-harness` | True: 48 files, +4,445 lines; `test/harness/bin/dt` is 1,270 lines. No `wpts` or `smbtorture` command; they are `dt smb wpts\|smbtorture` | `test/harness/bin/dt` |
| CI's unit, integration and e2e jobs call `dt` | True on the branch only; develop calls `go test` inline | branch `unit-tests.yml:75`, `integration-tests.yml:71`, `e2e-tests.yml:117` |
| `dt` runs unit, integration and lint | Unit and integration yes. Lint only as `dt quick` (vet and golangci-lint); CI's gofmt, citation, required-test, repo and ShellCheck steps aren't in `dt`, nor are the portmap, Windows and operator tests | branch `dt:1127-1131`; `lint.yml`, `integration-tests.yml:178-182`, `windows-build.yml:76-79`, `operator-tests.yml` |
| One command runs everything | None does. The closest, `dt-batch container`, runs 8 protocol and e2e runs (about 72 min) without unit, integration or lint, and exits 0 whatever they did | branch `dt-batch:79-94`, README reference results |
| `dt-batch matrix` is 28 runs | 28 with the branch's profiles; 20 on develop, where #2914 removed `postgres`, `postgres-s3` and `sqlite`. `dt-batch container`'s `postgres-s3` run no longer has a profile | `suites.json` on each |
| KMIP interop runs only in the harness | Since #2955 CI's integration list includes it, so `dt`'s separate step would run it twice | `e33edad6` |
| Every test in the repository runs somewhere | Not all: `test/integration/kerberos` uses its own `kerberos` build tag, which the integration job doesn't select, and no workflow sets `DITTOFS_E2E_NIGHTLY` for the e2e dedup tests | `kerberos_integration_test.go:1`, `dedup_race_nfsv4_test.go:80` |
| The client-compatibility checks can be run locally | Only by hand: they are 601 lines of inline steps across three jobs, not scripts | `smb-client-compat.yml` |
| `dt` grades conformance results | No: `run.sh` and the per-suite graders do; `dt` calls them | `run.sh:18-20`, `test/common/known-failures.sh` |
| `suites.json` covers 5 suites, with per-event tiers | True for 5 suites. Only `pull_request` is ever narrowed, and smbtorture's PR set is both its profiles | `suites.json:31-33, 39, 56` |
| A suite entry is about 10 lines | No: 15 to 38 lines | `suites.json:36-148` |
| JSON so Actions can `fromJSON()` it | Stated, but Actions parse `run.sh --matrix` output, not the file; and `jq` is not in the Nix dev shell | `suites.json:9-10`, `conformance.yml:106` |
| Postgres tuning is written 3 times | Not on develop: #2914 removed it. The branch still has all three copies | `77062d90` |
| 43 scenarios | 42 scripts plus `setup.sh`, sizes xs to xxl | `test/scenarios/` |
| Scenarios can run in CI as they are | They need podman ≥ 5.6; the hosted image has 4.9.3 | `test/scenarios/README.md`, runner image notes |
| `dt scenarios` reports what ran | Not always: `setup.sh` exits 0 having run nothing when another run holds its lock | `setup.sh:101` |
| The fio bench is not wired to anything | It has a `Makefile` target, path filters and unit tests, but no CI job runs it. `dfsbench` lives in `cmd/bench`, not `bench/` | `Makefile:77-78`, `cmd/bench` |
| Machine-readable results exist somewhere | No JUnit and no run summary. The tools write some: WPTS's TRX, pynfs's JSON (which nothing reads) and fio's JSON (read by `dfsbench`); the canary's JSON on the branch. The first draft of this RFC missed pynfs and fio | `run-pynfs.sh:283`, `internal/dfsbench/fio/run.go:87` |
| Develop-red alerting was removed after floods | True: added in #1170 (e-mail and issue), removed in #1705 after flaky smbtorture subtests | `684f97f4`, `ci-health.yml:3-5` |
| `ci-health` watches develop | Three workflows only; `AD-DC PAC Test` failed 5 of 10 completed develop runs on 2026-10-06/07 with no signal | `ci-health.yml:22-30`, Actions history |
| Enable the merge queue | Not available on a user-owned repository; up-to-date branches are required instead, since 2026-10-07 | GitHub docs, ruleset |
| `smb-client-compat` runs on every develop push | Only when SMB adapter paths change; Windows mount tests skip when port 445 is taken | `smb-client-compat.yml:14-19, 451-455` |
| The harness's pre-push adds to the repository hook | It replaces it, and no longer tests operator packages | branch `githooks/pre-push:34-36`, `dt:434` |
| Tool versions are pinned once | Go is pinned four different ways; pjdfstest and pynfs revisions are copied from `flake.lock` | `go.mod:3`, `lint.yml:56`, `dev.Dockerfile:12, 24, 54` |
| The conformance cells test what their names say | Not all. The pjdfstest `-s3` cells name a bucket nothing creates: on develop run 37661151652, NFSv4 `badger-s3` graded 8,789 tests green while localstack answered 9 `HeadBucket` calls with `NoSuchBucket` and received no upload. The NFS Kerberos test exits 0 when its container can't load the NFS module, and did so, graded a pass, on runs 37621639205, 37626825247, 37657021104 and 37661151652 | `setup-posix.sh:282`, `test-nfs-krb5.sh:52-56`, `conformance.yml:418-419` |
| The suite scripts can be reused as they are (R9 as first written) | No. The server setup is written in at least five places. The Kerberos bootstrap calls `dfsctl adapter create` and `dfsctl adapter identity-map add`, which don't exist, with the errors discarded. The POSIX teardown kills every `dfs start` on the machine. Five workflow steps find results with `ls -td`. The WPTS password is written in six places | `nfs-conformance/bootstrap.sh:54, 63`, `teardown-posix.sh:64`, `conformance.yml:176, 244, 477, 530`, `nfs-pynfs.yml:182` |
| Every grader uses the shared known-failure matcher | WPTS's doesn't call `kf_is_known`, so wildcard rows wouldn't apply to it; WPTS's list has none today | `smb-conformance/parse-results.sh`, `test/common/known-failures.sh:133` |
| The Makefile's test targets are an entry point | They pass `$(ARGS)` to the scripts, and no workflow calls them | `Makefile:90-116` |

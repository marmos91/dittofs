# pynfs — NFSv4 Protocol Conformance

## Overview

[pynfs](https://linux-nfs.org/wiki/index.php/Pynfs) is the reference NFSv4
conformance suite, maintained alongside the Linux NFS implementation. It is its
own NFSv4 client: it speaks the protocol directly over TCP rather than going
through a kernel mount.

That is the whole point of running it. `test/posix/` (pjdfstest) and `test/e2e/`
prove *filesystem semantics* through a kernel client, and a kernel client only
ever sends the subset of the protocol it happens to need. pynfs exercises the
protocol itself — state, sessions, owners, locking, and wire error codes — which
is where interop bugs live and where every POSIX test passes straight over.

Because it needs no mount, it also needs no root and no kernel NFS client, and
it runs on macOS as happily as on Linux.

| | |
|---|---|
| Suite | pynfs (`github:kofemann/pynfs`, pinned in `flake.lock`) |
| Versions | NFSv4.0 (689 tests), NFSv4.1 (269 tests) |
| Known failures | [`KNOWN_FAILURES_V40.md`](KNOWN_FAILURES_V40.md), [`KNOWN_FAILURES_V41.md`](KNOWN_FAILURES_V41.md) |
| Baseline | [`baseline-knfsd.md`](baseline-knfsd.md) — the same suite against Linux knfsd |
| CI | [`.github/workflows/nfs-pynfs.yml`](../../../.github/workflows/nfs-pynfs.yml) |

## Prerequisites

Nix. Everything else is fetched by the flake:

```bash
nix build .#pynfs        # or just let run-pynfs.sh do it
```

The package pins Python 3.12 deliberately — pynfs unpacks XDR with the stdlib
`xdrlib` module, which was removed in 3.13.

## Running

```bash
# Full NFSv4.0 run against a memory-backed server, provisioned and torn down
# for you.
./run-pynfs.sh --profile memory --minor-version 4.0

# NFSv4.1.
./run-pynfs.sh --profile memory --minor-version 4.1

# A handful of tests, streaming output, against a server you already started.
./run-pynfs.sh --no-setup --minor-version 4.1 --tests "SEQ1 SEQ2" --verbose

# An external server.
./run-pynfs.sh --no-setup --server 10.0.0.5:2049 --export /export
```

To keep a server between runs, provision it once and pass `--no-setup`:

```bash
../../posix/setup-posix.sh memory --no-mount
./run-pynfs.sh --no-setup --minor-version 4.0
./run-pynfs.sh --no-setup --minor-version 4.1
sudo ../../posix/teardown-posix.sh
```

The server comes from `test/posix/setup-posix.sh --no-mount`, which owns the
backend profiles. This suite reuses it rather than keeping a second copy that
would drift; `--no-mount` skips the mount and the root check, since nothing here
needs either.

### Flags Reference

```
--profile PROFILE        Storage profile (default: memory)
                         Valid: memory badger postgres postgres-s3
--minor-version VERSION  NFSv4 minor version (default: 4.0). Valid: 4.0, 4.1
--server HOST:PORT       Server to test (default: localhost:12049)
--export PATH            Export path on the server (default: /export)
--tests "CODES"          pynfs test codes or flags (default: all)
--no-setup               Test whatever is already listening on --server
--keep                   Leave the server running afterwards
--verbose                Stream pynfs output as it runs
--help                   Show help
```

### Available Sub-Suites

pynfs selects tests by **code** (`LOOK1`) or by **flag** (`lookup`, `open`,
`lock`, …). List them:

```bash
nix run .#pynfs -- --showcodes          # every test code
$(nix build .#pynfs --print-out-paths --no-link)/bin/pynfs-4.0 --showflags
$(nix build .#pynfs --print-out-paths --no-link)/bin/pynfs-4.0 --showcodesflags
```

A code is the stable identifier: it is what `KNOWN_FAILURES` is keyed on and
what you pass to `--tests` to re-run one test.

## Known Failures

Graded on the blacklist model the POSIX and SMB suites already use, through the
shared `test/common/known-failures.sh` parser: a run is green when every failing
test is on the version's table, and red on any failure that is not. Shell-glob
wildcards work (`LAYOUT*`).

Categories:

- **proto** — NFSv4 protocol behaviour, not a server bug.
- **feature** — something DittoFS deliberately does not implement (pNFS,
  named attributes, `SET_SSV`).
- **suite** — the assertion is not one a conformant server is expected to
  satisfy. **Only valid with knfsd evidence**: see `baseline-knfsd.md`.
- **bug** — a real DittoFS defect, tracked by the linked issue. These are meant
  to leave the list.

A `suite` row without a baseline entry backing it is how a blacklist quietly
becomes a place to park bugs. Refresh the evidence with:

```bash
sudo ./baseline-knfsd.sh              # Linux only, needs the in-kernel nfsd
gh workflow run nfs-pynfs.yml -f baseline=true    # or from CI
```

## Understanding Results

Each run writes `results/pynfs-v<version>-<profile>-<timestamp>/`:

| File | Contents |
|------|----------|
| `pynfs.log` | Full suite output — one line per test, plus failure detail |
| `pynfs.json` | The same results as JSON |
| `summary.txt` | The graded table CI puts in its step summary |
| `dittofs.log` | Server log for the run |

pynfs prints nothing until the end, so a quiet run is a running run, not a hung
one — watch `/tmp/dittofs-posix-server.log` if you want progress.

Per-test lines look like:

```
LOOK1    st_lookup.testDir                                     : FAILURE
```

Only `FAILURE` is graded. `WARNING` and `UNSUPPORTED` mean the server declined
something optional, which pynfs counts as "Warned"; `OMIT` means a dependency
did not run.

To reproduce one failure:

```bash
./run-pynfs.sh --no-setup --minor-version 4.0 --tests LOOK1 --verbose
```

The grader exits with the number of new failures, and prints a copy-pasteable
row to append if the failure is expected. Its own behaviour is pinned by
`./parse-results_test.sh`, which runs in CI as a separate job so a grading
regression is caught without waiting for the full suite.

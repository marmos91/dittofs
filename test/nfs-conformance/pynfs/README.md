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
| Known failures and expected skips | [`known.yaml`](known.yaml) |
| Comparison | `dt run pynfs --knfsd`: the same suite against Linux knfsd |
| CI | [`.github/workflows/tests.yml`](../../../.github/workflows/tests.yml), through `dt` |

## Running

pynfs is a suite of `test/suites.yaml`, and `dt` runs it: it builds pynfs from
`flake.lock` with nix, starts a DittoFS server for the cell, runs pynfs against
it with `--xml`, and grades each test against `known.yaml`.

```bash
test/harness/bin/dt run pynfs/badger-s3/1     # NFSv4.1 on badger-s3
test/harness/bin/dt run pynfs                 # both minor versions
test/harness/bin/dtc run pynfs                # in the Linux dev container
```

`badger-s3` needs Localstack (`dt services up localstack`). Each run writes
`test/harness/results/RUN/`: `summary.json`, the JUnit files, `report.md`, the
logs, and the cell's directory, with pynfs's `junit.xml` and the server's
`dfs.log`. `dt report` shows the latest run.

pynfs's `FAILURE` fails a test and `OMIT` skips it; `WARNING` and `UNSUPPORTED`,
where the server declined something optional, pass.

### Test codes

pynfs selects tests by **code** (`LOOK1`) or by **flag** (`lookup`, `open`,
`lock`, and so on). List them:

```bash
nix run .#pynfs -- --showcodes          # every test code
$(nix build .#pynfs --print-out-paths --no-link)/bin/pynfs-4.0 --showflags
$(nix build .#pynfs --print-out-paths --no-link)/bin/pynfs-4.0 --showcodesflags
```

A code is the stable identifier: it is what `known.yaml` is keyed on.

## Known failures and expected skips

A run is green when every failing test is a known failure and every skipping
test an expected skip, each with its reason, where its `where` applies. An entry
of category `proto` with `fix: wontfix` says the assertion is not one a
conformant server is expected to satisfy: it is only valid with knfsd evidence,
that the kernel's server fails it too. Without it, such an entry is how a known
list quietly becomes a place to park bugs. The comparison grades the kernel's
server against this same list: a test both servers fail is a known failure
there, and an entry the kernel's server passes is an unexpected pass, a failure
of DittoFS's alone. Run it with:

```bash
test/harness/bin/dt run pynfs --knfsd                       # Linux, passwordless sudo, the in-kernel nfsd
gh workflow run tests.yml -f tier=manual -f suite=pynfs     # or from CI, beside DittoFS's own cells
```

# NFS mount-option suites: cthon04 and nfstest

Two upstream NFS test suites, run against DittoFS through the Linux kernel client under a set of
named mount option sets.

The other NFS harnesses here all mount with client caching and locking switched off. pjdfstest uses
`noac,nolock,sync,lookupcache=none`, e2e uses `actimeo=0`, and pynfs does no kernel mount at all.
That keeps them precise about what the server does, but none of them sees what an untuned client
sees. These suites mount the way a user would, and vary one option at a time.

| Suite | What it is | Upstream |
|---|---|---|
| `cthon04` | The Connectathon suite: basic, general, special and lock tests. Run once per option set, which is how interop events use it. | `git://git.linux-nfs.org/projects/steved/cthon04.git` |
| `nfstest` | NetApp's NFS test modules. They mount with given options themselves and check the result, many of them against a packet capture. | `git://git.linux-nfs.org/projects/mora/nfstest.git` |

Both are pinned to a commit in [`fetch.sh`](fetch.sh). The server answers `git://` only; `https`
returns 403.

## Option sets

The variant axis. They are defined in [`option-sets.sh`](option-sets.sh); the port is added by the
runners.

| Name | Options | Adds over its base |
|---|---|---|
| `default` | none | what a plain `mount -t nfs` negotiates: NFSv4.2 against DittoFS (nfstest names it explicitly) |
| `v3` | `vers=3,tcp,mountproto=tcp` | — |
| `v4.0` | `vers=4.0` | — |
| `v4.1` | `vers=4.1` | — |
| `v4.1-noac` | `v4.1` + `noac` | no attribute caching |
| `v4.1-smallio` | `v4.1` + `rsize=32768,wsize=32768` | many small READ/WRITEs |

No set uses `nolock`: NFSv3 locks reach DittoFS's lock manager, because the client has a network
stack of its own (below).

## Client and user

The server runs on the host. The client runs in a network namespace of its own, joined to the host
by a veth pair on the bridge `dfsnfs0`. See [`netns.sh`](netns.sh).

| Who | Namespace | Address |
|---|---|---|
| `dfs`, and the host's `rpcbind` | host | `10.51.0.1` |
| the client under test | `dfsnfs1` | `10.51.0.2` |
| nfstest's second client, reached over ssh | `dfsnfs2` | `10.51.0.3` |

Each client namespace gets a private `/run`, its own hostname, and its own `rpcbind` and
`rpc.statd`. A client that shared the server's stack would fall short in three ways:
- Its lockd would take over DittoFS's NLM registration in their shared rpcbind, so a v3 lock would
  reach the client's own kernel.
- nfstest could not tell client traffic from server traffic in a capture with `127.0.0.1` at both
  ends.
- A second client would be the same NFSv4 client to the server.

The setup step registers DittoFS's MOUNT and NLM services with the host's rpcbind
(`setup-posix.sh --system-rpcbind`).

Only the mount is root's; the tests run as `dfsnfstest` (uid 64000). See
[`test-user.sh`](test-user.sh).
- **Squash:** the export squashes root (`root_to_guest`).
- **Access:** `dfsnfstest` gets a read-write grant from the setup step (`--test-user`).
- **cthon04:** runs each program as that user.
- **nfstest:** runs as that user with passwordless `sudo`, which is how it expects to run. It stops
  its packet captures with `sudo kill` on every process the capture started, and logs in to the
  second client with an ssh key.

## What runs

| Suite | Option set | Runs |
|---|---|---|
| cthon04 | all | basic `test1`–`test9`, general, special (15 programs), lock `tlocklfs`, `tlock64` |
| nfstest | all | `nfstest_posix`, `nfstest_dio` |
| nfstest | `v3` | + `nfstest_cache` |
| nfstest | v4 sets | + `nfstest_delegation` |
| nfstest | `v3`, `v4.0`, `v4.1` | + `nfstest_lock` |

cthon04's own `runtests` stops a group at its first failure. [`cthon04/run.sh`](cthon04/run.sh)
runs each program on its own instead, so one failure cannot hide the tests after it.

`nfstest_cache` and the recall tests of `nfstest_delegation` drive the second client. The second
client's traffic is mirrored into the first client's interface, because nfstest checks both in the
one capture it takes, as on a shared segment. `nfstest_cache` runs on NFSv3 only: on NFSv4 the
second client's change recalls the first client's delegation, and the first revalidates at once,
which is correct and defeats every timing check the module makes. Any other
module runs with `--modules`, e.g. `--modules posix,alloc`.

## Grading

Each runner turns its suite's output into one verdict per test and grades it with
[`grade.sh`](grade.sh). It uses the same table format and parser as pjdfstest and the SMB suites
(`test/common/known-failures.sh`). Each suite has a shared table, `<suite>/KNOWN_FAILURES.md`, and
small per-version overlays: `_V3.md`, and for nfstest `_V4.md` plus `_V40.md` for `v4.0` alone. An
option set is graded against the shared table plus its overlays. Which tables grade which set is
declared in `test/conformance/suites.json`; `test/conformance/run.sh` passes them to the runner in
`DITTOFS_KNOWN_FAILURES`.

- A failure on the table is reported and does not fail CI. Any other failure does.
- A test that timed out, or a module whose own total disagrees with the lines parsed for it, fails even when the names are on the table.
- A tabled test that passes is listed under "consider removing".
- A log without its completion marker is not graded at all.

nfstest names each assertion `<module>/<subtest>: <message>`, e.g.
`posix/read: file st_atime should be updated`, and a Python traceback
`<module>/<subtest>: traceback: <exception line>`. So a table entry excuses one assertion or one
exception, not a whole subtest. cthon04 is graded per program (`basic/test6`), so its rows excuse
any failure of that program.

## Running

In CI the suites are entries in [`test/conformance/suites.json`](../conformance/suites.json) and run
in the `nfs-mount` job of `.github/workflows/conformance.yml`. That is an Ubuntu runner, with
`sudo` for the mount and the namespaces. Both suites run on `badger-s3`, the production profile.
PRs run cthon04 on every option set, six short jobs. nfstest runs only in the nightly, because a v4
cell takes tens of minutes. Merges to develop run neither.

On a Linux host with root, and a localstack on localhost:4566
(`docker run -d -p 4566:4566 -e SERVICES=s3 localstack/localstack:4.13.1`):

```bash
test/conformance/run.sh --suite cthon04 --profile badger-s3 --variant v4.1
test/conformance/run.sh --suite nfstest --profile badger-s3 --variant v3
```

Anywhere with Docker, a Mac included:

```bash
test/nfs-mount/dev-docker.sh cthon04 badger-s3 v4.1
test/nfs-mount/dev-docker.sh nfstest badger-s3 v3
test/nfs-mount/dev-docker.sh shell
```

`dev-docker.sh` runs the same `run.sh` inside a privileged Ubuntu container. That container uses the
Docker VM's kernel NFS client, so it needs nothing installed on the host, and a localstack beside it
for the `-s3` profiles. Results land in `test/nfs-mount/results/`.

Containers share the Docker VM's kernel, so `dev-docker.sh` runs one suite at a time. If a dfs
hangs or dies under a mounted `hard` share, the kernel's writeback to it never finishes. Containers
then can't be removed, and later runs block in `sync()`. Restarting the Docker VM (for example
`colima restart`) clears it.

## Comparing against knfsd

Many nfstest `dio`, `delegation` and `cache` assertions check what the kernel *client* sends or
sees: which WRITEs, whether it caches them, when it returns a delegation, when it notices another
client's change. `cache` runs on NFSv3 only, as in the DittoFS cells. A failure there only points at
DittoFS if the Linux kernel server passes the same assertion on the same kernel.
[`nfstest/baseline-knfsd.sh`](nfstest/baseline-knfsd.sh) runs those modules through the same
`run.sh` against knfsd, under every option set, grades each set against DittoFS's tables, and
writes [`nfstest/baseline-knfsd.md`](nfstest/baseline-knfsd.md). A row knfsd fails every time is
about the client; a row knfsd passes is ours. It needs the in-kernel nfsd, so it runs on a Linux
host, or in CI on demand:

```bash
gh workflow run nfs-knfsd-baseline.yml            # every option set
gh workflow run nfs-knfsd-baseline.yml -f variants=v4.0,v4.1
```

The workflow uploads the report and the run's results. Commit the report from the artifact. After
editing a known-failure table, rebuild the report from those results instead of running again,
which needs neither Linux nor root:

```bash
test/nfs-mount/nfstest/baseline-knfsd.sh --report-only <artifact>/results/baseline-knfsd-<time>
```

A row's pattern can cover assertions knfsd fails and others it passes, and one assertion name
recurs within a module, so a row knfsd fails only in part does not cover every DittoFS failure
under it. `--compare` sets DittoFS nfstest results beside knfsd's, assertion by assertion: pass it
one or more nightly nfstest artifacts, or a local `test/conformance/results/nfstest`, and the
report lists, per option set, every `dio` or `delegation` assertion DittoFS fails more often than
knfsd, where knfsd passed it at least once or never reached it:

```bash
gh run download <nightly-run-id> --pattern 'nfstest-*' --dir <run-id>
test/nfs-mount/nfstest/baseline-knfsd.sh --report-only <baseline-results> --compare <run-id>
```

Under `default` (NFSv4.2) knfsd never reaches the `DELEGRETURN should be sent after the close`
assertions. It grants no write delegation that nfstest recognises at 4.2, and the client sends no
CLOSE while it holds the read delegation. Those rows rest on the `v4.1` evidence.

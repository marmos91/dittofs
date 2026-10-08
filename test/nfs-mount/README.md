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
| `v3` | `vers=3,tcp,mountproto=tcp,nolock` | — |
| `v4.0` | `vers=4.0` | — |
| `v4.1` | `vers=4.1` | — |
| `v4.1-noac` | `v4.1` + `noac` | no attribute caching |
| `v4.1-smallio` | `v4.1` + `rsize=32768,wsize=32768` | many small READ/WRITEs |

The NFSv3 sets use `nolock`, because on a single host the client's own lockd takes over the NLM
registration and a v3 lock never reaches DittoFS. The reasoning is in `option-sets.sh`. Locking is
tested on the v4 sets.

## What runs

| Suite | Option set | Runs |
|---|---|---|
| cthon04 | all | basic `test1`–`test9`, general, special (15 programs) |
| cthon04 | v4 sets | + lock `tlocklfs`, `tlock64` |
| nfstest | all | `nfstest_posix`, `nfstest_dio` |
| nfstest | v4 sets | + `nfstest_lock`, `nfstest_delegation` |

cthon04's own `runtests` stops a group at its first failure. [`cthon04/run.sh`](cthon04/run.sh)
runs each program on its own instead, so one failure cannot hide the tests after it.

`nfstest_cache`, the module that tests the `ac*` options directly, is not run. It needs a second
client host, and on one host its two mounts collide. Any other module runs with
`--modules`, e.g. `--modules posix,cache`.

## Grading

Each runner turns its suite's output into one verdict per test and grades it with
[`grade.sh`](grade.sh) against `<suite>/KNOWN_FAILURES_V3.md` or `_V4.md`. It uses the same table
format and parser as pjdfstest and the SMB suites (`test/common/known-failures.sh`).

- A failure on the table is reported and does not fail CI. Any other failure does.
- A test that timed out fails even when its name is on the table.
- A tabled test that passes is listed under "consider removing".
- A log without its completion marker is not graded at all.

nfstest names each assertion `<module>/<subtest>: <message>`, e.g.
`posix/read: file st_atime should be updated`. So a table entry excuses one assertion, not a whole
subtest.

## Running

In CI the suites are entries in [`test/conformance/suites.json`](../conformance/suites.json) and run
in the `nfs-mount` job of `.github/workflows/conformance.yml`. That is an Ubuntu runner, with
`sudo` for the mount. PRs run cthon04 on `memory`, five short jobs, one per option set. Everything
else (nfstest, and both suites on every profile) runs only in the nightly, because a v4 nfstest
cell takes tens of minutes. Merges to develop run neither.

On a Linux host with root:

```bash
test/conformance/run.sh --suite cthon04 --profile memory --variant v4.1
test/conformance/run.sh --suite nfstest --profile memory --variant v3
```

Anywhere with Docker, a Mac included:

```bash
test/nfs-mount/dev-docker.sh cthon04 memory v4.1
test/nfs-mount/dev-docker.sh nfstest memory v3
test/nfs-mount/dev-docker.sh shell
```

`dev-docker.sh` runs the same `run.sh` inside a privileged Ubuntu container. That container uses the
Docker VM's kernel NFS client, so it needs nothing installed on the host. It supports the `memory`
and `badger` profiles; the `-s3` profiles need CI's localstack service. Results land in
`test/nfs-mount/results/`.

Containers share the Docker VM's kernel, so `dev-docker.sh` runs one suite at a time. If a dfs
hangs or dies under a mounted `hard` share, the kernel's writeback to it never finishes. Containers
then can't be removed, and later runs block in `sync()`. Restarting the Docker VM (for example
`colima restart`) clears it.

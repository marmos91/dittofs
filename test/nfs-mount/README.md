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

Both are pinned in `flake.lock`, and `dt` builds them with Nix. The server answers `git://` only;
`https` returns 403.

## Option sets

The `mount-options` axis of each suite in [`test/suites.yaml`](../suites.yaml). The knfsd baseline
keeps the same sets in [`option-sets.sh`](option-sets.sh).

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

The server runs on the host. `dt`'s fixture puts each client in a network namespace of its own
(`client: namespace`), with a private `/run`, its own hostname, and its own `rpcbind` and
`rpc.statd`, and registers DittoFS's MOUNT and NLM services with the host's rpcbind
(`rpcbind: host`). nfstest gets a second client, which it reaches over ssh (`clients: 2`). A client
that shared the server's stack would fall short in three ways:
- Its lockd would take over DittoFS's NLM registration in their shared rpcbind, so a v3 lock would
  reach the client's own kernel.
- nfstest could not tell client traffic from server traffic in a capture with `127.0.0.1` at both
  ends.
- A second client would be the same NFSv4 client to the server.

Only the mount is root's; the tests run as the user `dt` runs as (`users: [self]`), which gets a
read-write grant. The export squashes root, and everyone else reads ([`shares.yaml`](shares.yaml)).
nfstest needs that user to have passwordless `sudo`, which is how it expects to run: it stops its
packet captures with `sudo kill` on every process the capture started.

## What runs

| Suite | Option set | Runs |
|---|---|---|
| cthon04 | all | basic `test1`–`test9`, general, special (15 programs), lock `tlocklfs`, `tlock64` |
| nfstest | all | `nfstest_posix`, `nfstest_dio` |
| nfstest | `v3` | + `nfstest_cache` |
| nfstest | v4 sets | + `nfstest_delegation` |
| nfstest | `v3`, `v4.0`, `v4.1` | + `nfstest_lock` |

cthon04's own `runtests` stops a group at its first failure. [`cthon04/tests.txt`](cthon04/tests.txt)
lists each program on its own instead, so one failure cannot hide the tests after it.

`nfstest_cache` and the recall tests of `nfstest_delegation` drive the second client. The second
client's traffic is mirrored into the first client's interface, because nfstest checks both in the
one capture it takes, as on a shared segment. `nfstest_cache` runs on NFSv3 only: on NFSv4 the
second client's change recalls the first client's delegation, and the first revalidates at once,
which is correct and defeats every timing check the module makes. Another module runs once
[`nfstest/tests.txt`](nfstest/tests.txt) lists it.

## Grading

`dt` grades every test against the suite's `known.yaml`, whose entries may name the option sets
they hold on (`where`). cthon04 is graded per program (`basic/test6`), so an entry excuses any
failure of that program.

nfstest names each assertion `<module>/<subtest>: <message>`, e.g.
`posix/read: file st_atime should be updated`, and a Python traceback
`<module>/<subtest>: traceback: <exception line>`. So an entry excuses one assertion or one
exception, not a whole subtest.

## Running

In CI both run through `dt` in `.github/workflows/tests.yml`, on `badger-s3`, the production
profile: cthon04 on every pull request, after a merge and nightly, six short cells, and nfstest
nightly, because a v4 cell takes tens of minutes.

On a Linux host with Nix, as a user with passwordless `sudo`, and a localstack on localhost:4566
(`docker run -d -p 4566:4566 -e SERVICES=s3 localstack/localstack:4.13.1`):

```bash
test/harness/bin/dt run cthon04/badger-s3/v4.1
test/harness/bin/dt run nfstest/badger-s3/v3
```

## Comparing against knfsd

Many nfstest `dio`, `delegation` and `cache` assertions check what the kernel *client* sends or
sees: which WRITEs, whether it caches them, when it returns a delegation, when it notices another
client's change. `cache` runs on NFSv3 only, as in the DittoFS cells. A failure there only points at
DittoFS if the Linux kernel server passes the same assertion on the same kernel.
[`nfstest/baseline-knfsd.sh`](nfstest/baseline-knfsd.sh) runs those modules through
[`nfstest/run.sh`](nfstest/run.sh), which stays for it, against knfsd, under every option set,
grades each set against the Markdown tables beside it (`nfstest/KNOWN_FAILURES*.md`), and
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
one or more results directories as `nfstest/run.sh` writes them, a `results.tsv` or `nfstest.log`
per option set, and the report lists, per option set, every `dio` or `delegation` assertion
DittoFS fails more often than knfsd, where knfsd passed it at least once or never reached it:

```bash
test/nfs-mount/nfstest/baseline-knfsd.sh --report-only <baseline-results> --compare <dittofs-results>
```

Under `default` (NFSv4.2) knfsd never reaches the `DELEGRETURN should be sent after the close`
assertions. It grants no write delegation that nfstest recognises at 4.2, and the client sends no
CLOSE while it holds the read delegation. Those rows rest on the `v4.1` evidence.

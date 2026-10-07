# DittoFS scenarios

```bash
test/scenarios/setup.sh                                 # a bash shell in a set-up container
test/scenarios/setup.sh 00-smb-nfs-roundtrip-gc-xs.sh   # runs one scenario
test/scenarios/setup.sh a.sh b.sh                       # runs several scenarios
test/scenarios/setup.sh all                             # runs every scenario, 9x too (see Names)
cd test/scenarios && ./setup.sh [0-8]?-*.sh             # runs every scenario but the long ones
cd test/scenarios && ./setup.sh *-xs.sh                 # runs every xs scenario
```

## Names

A scenario is named `<number>-<what it does>-<size>.sh`, for example `00-smb-nfs-roundtrip-gc-xs.sh`.

The number says when a scenario runs. Several scenarios run in name order, so the number is the
sequence too, and a schedule picks scenarios by their first digit, as `[0-8]?-*.sh` does:

| Number | Group |
|---|---|
| 0x | smoke: the basics, quick, so they run first |
| 1x | the control plane, stores and their options |
| 2x | file operations, SMB and NFS together |
| 3x | permissions, identity and the recycle bin |
| 4x | GC and dedup on one share |
| 5x | several shares and stores |
| 6x | the journal and the block store |
| 7x | S3 outages and crashes |
| 8x | free |
| 9x | long runs, left out of the schedule; each says the `SCENARIO_TIMEOUT` it needs |

The size says how long the scenario takes, not counting the container setup (about 13 s):

| Size | Takes |
|---|---|
| xs | under 10 s |
| s | under 1 min |
| m | under 5 min |
| l | under 15 min |
| xl | under 1 h |
| xxl | 1 h or more |

The size picks scenarios the same way: `*-xs.sh` is every xs scenario, `*-xs.sh *-s.sh` every
one under a minute, and `4?-*-xs.sh` the xs ones of group 4.

A new scenario takes the next free number in its group, and the size its first runs show.

Each scenario runs in its own throwaway rootless podman container (clean `ubuntu:26.04`),
which builds DittoFS from this checkout and starts SeaweedFS as the S3 store and the DittoFS
server. With several scenarios, `setup.sh` prints one PASS, FAIL or SLOW line per scenario; one
scenario that passes prints nothing. The first run takes minutes (image pulls, apt, the Go build)
and is silent too: `tail -F /var/tmp/dittofs-scenarios/run/run.log` follows a run, across scenarios.

With no scenario, it does the same setup and then opens a bash shell in that container, as
`tester` for `smbclient`, with the server logs in `/logs` (kept in
`/var/tmp/dittofs-scenarios/shell/logs`). `scenario NAME` runs `/src/test/scenarios/NAME.sh`
there as a batch run would, traced and stopping at the first failing command; a failure ends
only that command, not the shell. Scenarios expect a fresh setup, so a second run in the same
shell may trip over what the first created. Leaving the shell removes the container. The shell
takes no lock, so it does not block a scheduled run.

## What setup.sh gives a scenario

- Share `/test`: metadata store `md`, block store `s3` (bucket `dittofs`, prefix `test/`).
- User `tester` (uid/gid 1000, password `tester-secret`), read-write on `/test`, the default SMB user.
- SMB on 445 (SMB3) and NFS on 2049: `smbclient //127.0.0.1/test -c 'ls'`,
  `nfs-ls 'nfs://127.0.0.1/test?version=4&uid=1000&gid=1000'`. Another SMB user: `-U bob%bob-secret`.
- `dfsctl`, logged in as admin, and `dfsctl system drain-uploads`, which waits until every
  share's pending uploads are done.
- `/etc/dittofs-s3.json`, the S3 block-store config without a bucket:
  `dfsctl store block add --name s3-x --type s3 --config "$(jq -c '.bucket = "x"' /etc/dittofs-s3.json)"`.
- The `s3:` rclone remote, for the buckets (`rclone mkdir s3:x`, `rclone size --json s3:x`).
- Userspace clients: `smbclient`, `nfs-cp`, `nfs-ls`, libnfs's `nfs-io` (mkdir, unlink, rmdir), `jq`.
- The tools in `lib/tools` (each file says how to use it): `s3 start | stop | pause | resume`,
  `dfs-server start | stop | kill`, `nfs-truncate`, `smb-truncate`, `nfs-hold` (keeps a file open
  over NFS) and `smb-open` (an SMB open with a given access and share mode, held open).
- One volume, the cache (`/var/tmp/dittofs-scenarios`) at `/cache`. Whatever a scenario keeps
  goes under it: `/inputs` is `/cache/inputs`, for large inputs worth keeping between runs, and
  `/logs` is the run's logs directory, kept when the run fails or is slow, and otherwise left in
  the cache's `run/` until the next run.
- A run is capped at `SCENARIO_TIMEOUT` (900) seconds.

## Writing one

A scenario is a bash script next to `setup.sh`, run with `set -euxo pipefail`: one command
per line, no functions, and the first failing command fails it. When it checks several
things, it prints the outcomes first and checks them after, so a failing run shows all of
them. Copy `00-smb-nfs-roundtrip-gc-xs.sh` to add one, and name it as above.

## Host side

Needs a Linux host with rootless podman 5.6 or later (it pulls with `pull --policy`), bash 5,
`flock` (util-linux), `timeout` (coreutils) and, for Slack, `curl`; nothing is installed on it. It
uses your normal podman storage, so `podman images` lists the three images it pulls
(`ubuntu:26.04`, `golang:1.26`, `seaweedfs:4.48`) and `podman ps` shows a running container, named
`dittofs-<scenario>`. Its Go
caches, downloads (apt packages, the libnfs source), inputs and kept logs are in the cache,
`/var/tmp/dittofs-scenarios`.

On a laptop where Docker runs in a Linux VM (Docker Desktop, Colima), as on macOS, the host can be a
container: run `setup.sh` in `quay.io/podman/stable`, as its user `podman`. Run this from the
checkout's root, which it mounts as `$PWD`:

```bash
docker run --rm -it --privileged -e CONTAINERS_CONF=/etc/containers/containers.conf \
  -v "$PWD":/src/dittofs -w /src/dittofs/test/scenarios --user podman \
  -v dittofs-podman-store:/home/podman/.local/share/containers \
  -v dittofs-scenarios-cache:/var/tmp/dittofs-scenarios \
  quay.io/podman/stable ./setup.sh 00-smb-nfs-roundtrip-gc-xs.sh
```

- `--privileged` lets the podman inside make user namespaces, mounts and its network. It gives up
  what running rootless on the host keeps, so it belongs only inside such a VM, never on a shared
  host; a server runs `setup.sh` on its own rootless podman.
- `CONTAINERS_CONF` loads only the image's system config. The image also has one for `podman` that
  mounts the outer `/proc` into every container, so a scenario sees the outer container's processes
  and `pkill` signals PIDs that are not its own: `s3 pause` and `s3 stop` fail.
- The two volumes keep the pulled images and the cache between runs. Docker makes them owned by root,
  so before the first run: `docker run --rm -v dittofs-podman-store:/a -v dittofs-scenarios-cache:/b
  quay.io/podman/stable chown -R podman:podman /a /b`.
- A glob such as `[0-8]?-*.sh` must expand inside the container: `bash -c './setup.sh [0-8]?-*.sh'`.
- To follow a run from another terminal: `docker exec -it $(docker ps -q --filter
  ancestor=quay.io/podman/stable) tail -F /var/tmp/dittofs-scenarios/run/run.log`.
- Times there are not comparable with a host's: two container layers and a VM slow builds and I/O,
  and Apple silicon runs arm64. They stay in the volume's own `times/`, apart from any host's.

Times (`lib/report.sh`): every run adds its times to `times/<scenario>.tsv` in the cache, which
its first run creates: one row for the scenario and one for each of its lines that ran, with the
date, the commit and the result. To read one:
`column -t -s $'\t' /var/tmp/dittofs-scenarios/times/00-smb-nfs-roundtrip-gc-xs.tsv`. A renamed
scenario starts a new record, unless its file in `times/` is renamed too.

Slack alerts (`lib/report.sh`): put a bot token with the `chat:write` scope in
`~/.config/dittofs-scenarios/slack-token` and the channel ID in `slack-channel` next to it (both
mode 0600), and invite the bot to the channel. They stay outside the cache, so no container sees
them; `SLACK_DIR` points elsewhere (a test run can point it at an empty directory). A scenario's first failure is posted, with the
failing command; in its thread go a later failure at a different point and the pass that ends it,
which also shows in the channel. Repeats of the same failure are not posted. A pass that is slow
is posted once too: the scenario, or one of its lines, took more than twice its usual time (the
median of its last 10 normal passes) and at least 10 s more. Slow passes do not count toward the
usual time, so a lasting slowdown stays slow; to accept a new usual time, delete the scenario's
record in `times/`. The pass back at normal speed replies in its thread. A run of
several scenarios (`setup.sh all`) posts one message instead and puts all of these in its thread,
then ends with a summary there (passed, slow, failed, how long) that shows in the channel too.

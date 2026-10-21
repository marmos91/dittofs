# Nightly performance run on a dedicated host

`night.sh` measures DittoFS, built from `develop`, against Samba every night on one quiet Linux
host, and boils the result down to one figure: the geometric mean of DittoFS's performance over
Samba's, point by point.

## What runs

Both targets serve one share over SMB on loopback. The kernel client mounts each with the same
options: SMB 3.1.1, `cache=none`, signing on, encryption and compression off. The two targets run
one after the other, never together:

- **DittoFS**: a Badger metadata store and the local journal. The block store is S3, since a share
  needs one, at an endpoint nothing answers: the server starts with it unhealthy, the sync to it
  stays paused and every write stays in the journal. So the night measures the frontend only.
- **Samba**: its own `smbd`, from `smb.conf` here, started for the run on its own port.

On each target, every step in its own time box:

| Step | What | Points |
|---|---|---|
| `matrix` | fio: read and write, sequential and random, 4 KiB, 64 KiB and 1 MiB blocks, 1, 2, 4 and 8 threads. Buffered I/O. Reads run for 30 s; writes are one pass over each thread's 256 MiB file and fsync it at the end | 48 |
| `mailbox` | The mailbox simulator (`../workloads/mailbox`) with 2, 8 and 16 users, files of 512 MiB, fixed-time scenarios of 60 s | 16 |
| `files` | 5,000 files of 4 KiB, 64 KiB and 1.75 MiB, created (each fsynced at its close), then read back | 6 |

Before the targets, the night fetches `develop`, builds `dfs`, and records the commit, the host's
state and each NVMe drive's `smart-log`. A repeat writes about 140 GB to each drive, both targets
together: a calibration night about 420 GB. Each night's `summary.json` has the measured figure. The night takes the lock of the scenario runs, so the two
never overlap. Every step is cut to end by the hard stop, 05:45; a step that reaches its box is
killed and recorded as `timeout`.

## The figure

A point's value on a target is the median of its repeats. Its ratio is DittoFS's performance over
Samba's: a throughput as it is, a time or a latency inverted, so 100% means as fast as Samba. The
score is the geometric mean of all the ratios, with sub-scores for the sequential, random, small
files and mailbox points. The mailbox scenarios score as follows:

| Scenario | Value |
|---|---|
| logon, folder re-sort, logoff compaction (fixed work) | wall time until the last user is done |
| mail flow, full sync (fixed time) | 99th percentile latency of all their I/O |
| search re-index (fixed time) | throughput of its mailbox reads |

## Calibration and flags

`CALIBRATION=yes` in the cron file runs every point three times and posts nothing. That is the
first week. When it is set to `no`, the night runs every point once, and `score.py` first sets
each point's noise band from the calibration nights' repeats (`band.json`). A point is flagged
when it falls below its 7-day median by more than its band.

Slack gets the night by the scenarios' rules, through their reporting
(`test/scenarios/lib/report.sh`): the same bot, the same channel (`SLACK_DIR`).

- A quiet night posts nothing. It is still recorded, in its directory and `trend.csv`.
- The first night with a new flag or failure (one the 7 nights before did not have) posts its
  summary: the figure, the sub-scores, the worst point, the new flags and failures and the develop
  commits since the last night. `slack-ts` beside the nights keeps that message.
- While that alert is open, a night with another new flag or failure replies in its thread, and
  a night that repeats the same ones posts nothing.
- The first night with no flag and no failure replies in the thread, shown in the channel too,
  and closes the alert.

Smoke runs and calibration nights never post.

## Changing what a night measures

The band and the 7-day medians compare a night with the nights before it, so they only hold while
the steps measure the same thing. After changing what a step runs (a size, a list, a scenario), take
the earlier nights out of the comparison before the next night runs: rename their directories out of
the `<date>` form with a suffix (they are still removed after 30 days), and drop their rows from
`trend.csv`. If that leaves fewer than a week of
calibration nights, keep `CALIBRATION=yes` until there are seven. The first night, 2026-10-20, ran
the matrix with 512 MiB files and does not count:

```sh
cd /var/tmp/dittofs-perf
mv 2026-10-20 2026-10-20-512m
grep -v '^2026-10-20,' trend.csv >trend.csv.new && mv trend.csv.new trend.csv
```

## Results

`/var/tmp/dittofs-perf/<date>/`, kept 30 days:

- `steps.tsv` and `night.log`: each step's status and time, and the night's log;
- `r<repeat>/<target>/`: fio's JSON per point, its per-second bandwidth and IOPS logs, the
  server's log and the mount;
- `points.csv`: one row per point, target and repeat (also appended to `../trend.csv`);
- `summary.json`: CTRF, with the steps as tests and the scores in `extra`;
- `slack.txt`: the summary as posted, or as it would be;
- `host-before.txt`, `host-after.txt`, `nvme-*.json`: the host and its drives.

## Setting up a host

As root, on Linux:

```sh
git clone https://github.com/marmos91/dittofs /srv/perf/dittofs
/srv/perf/dittofs/bench/ditto/setup.sh            # packages, user, directories
SMOKE=yes /srv/perf/dittofs/bench/ditto/night.sh  # a few minutes, one point of each kind
/srv/perf/dittofs/bench/ditto/setup.sh cron       # the cron file, in calibration mode
```

`setup.sh remove` takes it all away again, apart from the packages. `setup.sh split-drive DEV --yes`
takes one drive out of the host's RAID1 mirror for the runs' data. It is a separate step, since it
leaves the OS without redundancy.

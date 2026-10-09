# Mailbox workload simulator (fio)

fio job files that simulate what an Outlook mailbox on an FSLogix profile
container does to the storage behind an SMB share. Use them to check that
DittoFS stays stable under that load: no I/O errors, no hangs, no crash.

As of now, performance comparison is out of scope (see #3001). The sizes and rates in these
files are estimates, not measurements (see [Calibration](#calibration)).

## What the server actually sees

The stack on the client side:

```
Outlook (OST file)
  -> NTFS inside the user's VHDX
    -> VHDX virtual disk, attached by the session host
      -> SMB
        -> DittoFS
```

FSLogix stores one VHDX disk image per user on the share. The session host
opens that file over SMB and attaches it as a local disk. DittoFS never sees
emails, folders or the OST. It sees **one large file per user, with reads and
writes at scattered offsets**. This is block storage over a file protocol.

So one simulated user is one file, `user.<n>.vhdx`. A scenario can have several
job sections (for example `receive` and `send`). Clone `n` of every section
works on the same `user.<n>.vhdx`.

Why a mailbox is a hard case: an OST is a small database (B-trees, tables,
allocation maps). One arriving email is modeled as a burst of about 30 small
random I/Os spread across the file, mostly writes. That figure is an estimate,
not an official measurement. Over SMB each of those waits on a network round
trip and on the server's storage.

## Modeling decisions

| Decision | Reason |
|---|---|
| One file per user, sized like an allocated VHDX | That is the only thing the share holds |
| Reads are small compared to the mailbox | The session host and Outlook cache most reads. Only cache misses reach the share |
| `direct=1` and `sync=1` | A virtual disk over SMB is written through, for crash consistency |
| `iodepth` 1 or 2 per user | Outlook store access is mostly synchronous. Load comes from the number of users, not deep queues. Logon and compaction are the exceptions |
| Real data in the files, not sparse | A read of a sparse hole can be answered with no I/O at all |

There are two kinds of scenario:

- **Fixed work**: a user waits for it to finish (logon, re-sort, logoff). It
  does a fixed amount of I/O. The number to look at is how long each user's job
  took.
- **Fixed time**: background activity (mail flow, re-index, full sync). It runs
  for `RUNTIME` seconds at a paced rate. The numbers to look at are latency
  percentiles and throughput.

## Scenarios

| File | Simulates | I/O that reaches the share | Kind |
|---|---|---|---|
| `00-prepare.fio` | Setup, not a scenario | Sequential 1 MiB writes that create `user.0.vhdx` to `user.N-1.vhdx` with real data | Run once |
| `01-logon-outlook-start.fio` | Sign-in and Outlook start. With many users and `STORM_WINDOW`, a logon storm | Mostly reads (about 88%), 4K to 1 MiB, concentrated in hot areas of the file, plus a few small writes | Fixed work |
| `02-mail-flow.fio` | Receiving and sending mail | Bursts of small random I/O (4K to 64K, up to 256K on send), mostly writes, with a pause between bursts | Fixed time |
| `03-folder-resort.fio` | Sorting a large folder by another column | Phase 1: small random reads over the folder's table. Phase 2: small random writes for the new index. Runs for one user only, `USERS` is ignored | Fixed work |
| `04-search-reindex.fio` | Windows Search re-indexing the mailbox | Random reads (8K to 64K) over the whole file, plus paced 32K mixed I/O (40% reads) on the search catalog and paced sequential 64K writes to its log | Fixed time |
| `05-ost-full-sync.fio` | Downloading a full mailbox into a new OST | Rate limited sequential 256K writes appended past the end of the file, plus small random metadata I/O | Fixed time |
| `06-logoff-compaction.fio` | Sign-out with FSLogix VHDX compaction | 1 MiB sequential reads from the tail of the file and 1 MiB sequential writes toward the front, at the same time | Fixed work |

## Running

All job files read their parameters from environment variables, so always run
them through `run.sh`. Running `fio <file>` directly fails to parse.

Use a share backed by an S3 block store. The memory block store keeps every
block in RAM and is only meant for unit tests, so the server runs out of
memory under these workloads.

```sh
# 1. Create the per-user files once, with the largest user count you will test
TARGET_DIR=/mnt/mail USERS=4 VHDX_SIZE=512m ./run.sh 00-prepare.fio

# 2. Run any scenario against the same files
TARGET_DIR=/mnt/mail USERS=4 VHDX_SIZE=512m ./run.sh 01-logon-outlook-start.fio
```

`TARGET_DIR` must be a directory on a mounted DittoFS share. For the mailbox
case that is an SMB mount:

```sh
sudo mount -t cifs //localhost/mail /mnt/mail \
  -o port=12445,credentials=$HOME/.smbcredentials,vers=3.1.1,uid=$(id -u),gid=$(id -g)
```

`00-prepare` writes `USERS x VHDX_SIZE` of real data. 200 users at 30g is 6 TB.
Start small.

Extra arguments after the file name are passed to fio, for example
`./run.sh 02-mail-flow.fio --parse-only` to check syntax only.

### Cold logon

Right after `00-prepare` the data is still in the server's local store, so
`01-logon-outlook-start` reads it locally and never touches the remote. A real
logon storm reads from the remote. To test that, upload everything and drop the
local copy before running `01`:

```sh
dfsctl system drain-uploads
dfsctl store block evict --share <share>
```

Eviction never drops a block that has not been uploaded, and it reclaims whole
segments, so one pending record keeps its segment local. Repeat the drain until
`dfsctl store block stats` reports 0 pending remote bytes, then evict.

### Variables

| Variable | Default | Meaning |
|---|---|---|
| `TARGET_DIR` | required | Directory under test |
| `USERS` | `1` | Number of simulated users (one file each) |
| `VHDX_SIZE` | `30g` | Size of each user's file |
| `IOENGINE` | `libaio` | fio I/O engine |
| `RUNTIME` | `600` | Seconds, for the fixed time scenarios |
| `STORM_WINDOW` | `0` | Seconds over which logons start (01) |
| `LOGON_IO` | `300m` | I/O per user at logon (01) |
| `MAIL_INTERVAL` | `120s` | Pause between received messages (02) |
| `SEND_INTERVAL` | `900s` | Pause between sent messages (02) |
| `SORT_READ` | `150m` | Bytes read in a re-sort (03) |
| `SORT_WRITE` | `30m` | Bytes written in a re-sort (03) |
| `CATALOG_IOPS` | `50` | Search catalog I/O rate, 40% reads and 60% writes (04) |
| `CATALOG_LOG_IOPS` | `10` | Search catalog log write rate (04) |
| `SYNC_RATE` | `10m` | Download rate in a full sync (05) |
| `SYNC_GROW` | `6g` | Bytes appended per user in a full sync (05) |
| `SYNC_META_IOPS` | `200` | Metadata I/O rate in a full sync (05) |
| `COMPACT_MOVE` | `4g` | Bytes moved by compaction (06) |
| `COMPACT_BS` | `1m` | Block size for compaction (06) |
| `COMPRESS_PCT` | `30` | How compressible the written data is |

With a small `VHDX_SIZE`, lower `LOGON_IO`, `SORT_READ`, `SORT_WRITE`,
`COMPACT_MOVE` and `SYNC_GROW` to match.

`05-ost-full-sync` makes each file `SYNC_GROW` larger on every run. The download
stops after `SYNC_GROW` bytes or `RUNTIME` seconds, whichever comes first. Delete
the files and run `00-prepare` again to get back to `VHDX_SIZE`.

In `02-mail-flow` each user starts at a random point in the first 60 seconds
and then pauses `MAIL_INTERVAL` or `SEND_INTERVAL` between bursts. For a short
run, lower both intervals, or a user does one burst and waits out the rest.

### Output

Each run writes one JSON file to `results/` and prints one line per job
(needs `jq`): per-user run time (p50, p90, max), total IOPS and MiB/s, and the
worst p99 latency.

## What counts as a pass

The goal is stability, so a run passes when:

- `run.sh` exits 0
- every job in the JSON has `"error": 0` and no short I/O
- the scenario finishes (it does not hang)
- the server is still running and the mount still answers afterwards
- the server log and `dmesg` (CIFS lines) show no errors

```sh
jq '.jobs[] | {jobname, error, short: (.read.short_ios + .write.short_ios)}' results/*.json
```

## Reading S3 usage after a run

Right after a run the bucket holds roughly everything the run ever wrote, not
the live size of the files:

- Overwritten data is only reclaimed by a GC pass, which runs every 15 minutes
  by default.
- Orphaned blocks are kept for a 1 hour grace period (default) before GC deletes them.
- A block that is only partly dead is never repacked unless
  `gc.compaction_live_ratio` is set. It defaults to 0, so by default nothing is
  repacked.

To compare bucket usage with the live file sizes, set
`gc.compaction_live_ratio` (for example `0.5`), wait out the grace period, run
`dfsctl store block gc <share>`, and measure after that.

## Running scenarios together

Real load is a mix. Two useful pairs, started as two `run.sh` commands in
parallel against the same files:

- `03-folder-resort` while `02-mail-flow` runs at the real user count
- `01-logon-outlook-start` with a storm window while `06-logoff-compaction`
  runs for the previous shift

## Not modeled

- SMB opens, durable handles and lease breaks while the container is attached
- VHDX metadata updates as a dynamic disk grows
- The final truncate at the end of compaction
- Server side dedupe. `COMPRESS_PCT` only roughly controls compressibility

## Calibration

Microsoft does not publish OST I/O traces, so every size and rate here is an
educated guess exposed as a variable. To replace the guesses with measurements:

- On a session host, filter Process Monitor on the user's `.vhdx` path and
  record one logon, one folder re-sort and one sign-out with compaction. The
  offset and length columns give the real block size mix, read/write ratio and
  bytes moved.
- On a Windows file server, the "SMB Server Shares" performance counters give
  the aggregate picture at real user counts.
- fio can replay a recorded I/O log (`read_iolog`) instead of a model.

# Task 6 rig run — provenance

VM (the ONLY instance this task created or may destroy):
  server_id  d8a08b01-84e8-40b7-9d78-568b257d52df
  name       dfsbench-1872-packing
  type       POP2-8C-32G, fr-par-1, ubuntu_noble, 50 GB data volume
             0829be48-f3ed-4d5a-a8ed-31efe27cd142 at /bench-data
Coder VMs that must never be touched (verified running, untouched):
  d9f39027-487e-4523-8033-1488eb3c3639  scw-coder-coder-primary-cce7acdd730c425ca2de66
  fb430ccc-8c94-4892-ad60-9b689f0b4732  scw-coder-coder-primary-c7fa0a9b284e43fe97ea0f

Binaries under test (linux/amd64, CGO_ENABLED=0):
  packed  = perf/1872-block-packing @ 5b43a9ae
            dfs    sha256 535b38efd0d1f167f75f27297f7ad9a8b16612740301de1dcd4b59d151bc1fc1
            dfsctl sha256 594f9af663fd1fbfa3d6bb7b503c9822c935e328497315a7488b84cfddc5d29d
  develop = origin/develop @ 47a2d1fb
            dfs    sha256 9e051f7bdebac72fe2885bda3a0889f77394a48b72224cf32a529e469e1e022f
            dfsctl sha256 4d26b2f2aabd6cd733e96736a1d39b943934179d3e07f23c20c898c0d4a39869

The dfsbench HARNESS is origin/develop @ 47a2d1fb on both sides, so the A/B
differs only in the server binary. develop carries #2079's cold-barrier
diagnostics, which the branch (based on merge-base 4f2fe25b) does not; nothing
between 4f2fe25b and 47a2d1fb touches the carve/upload path
(`git log 4f2fe25b..origin/develop -- pkg/block/` is empty).

Probe: dittofs-s3-writeback-nfs3, --sizes large, --workloads seq-read,rand-write-4k,
--skip-baseline, default --runtime 60, --threads 4, --evict-cache (default true).

Object size on the wire = S3 object size under `blocks/`: pkg/block/remote/s3
PutBlock writes one whole block with a single PutObject under
`blocks/<blockID>`, so listing that prefix IS the PUT-size distribution.
s3snap.sh captures Size + LastModified; hist.py windows by LastModified so each
run's objects are separable.

## Bucket hygiene (a caveat, stated plainly)

`blocks/` was NOT emptied before the runs. Three attempts to wipe ~250k stale
objects (the previous #2070 probe's 4 KiB objects, plus July runs) were
abandoned: `aws s3 rm --recursive` deletes at ~3k/min against this endpoint
(~80 min), and a batched `delete-objects` fan-out reported success while
deleting nothing.

This does not contaminate the result. Object size is read from the S3 listing's
`Size`, and every object carries `LastModified`, so each run's objects are
selected by a time window and the stale set is excluded exactly. The windows:

  aborted first attempt (packed binaries)  17:30:05Z – 17:30:36Z  — discarded
  Run A  packed  (5b43a9ae)                started 2026-08-24T17:37:28Z
  Run B  develop (47a2d1fb)                started (see below)

The first attempt was aborted because a bucket-wipe pass was still running when
it began uploading, so some of its blocks were deleted underneath it. Nothing
from that window is used.

## Coder-VM status — one disappeared, and it was not me

At session start `scw instance server list zone=fr-par-1` showed both named
coder VMs running:

```
"id":"fb430ccc-8c94-4892-ad60-9b689f0b4732",
"name":"scw-coder-coder-primary-c7fa0a9b284e43fe97ea0f",
"tags":["kapsule=b9c08ebb-…","pool=324460e1-…","pool-name=coder-primary",
        "runtime=containerd","managed=true","node=c7fa0a9b-…"],
"creation_date":"2026-08-24T07:00:51.085841Z"
```

Later in the session `scw instance server get fb430ccc-…` returns "Cannot find
resource", and it is absent from every zone (fr-par-1/2/3, nl-ams-1/2,
pl-waw-1/2/3). `d9f39027-…` is still running.

I did not touch it. The only mutating Scaleway call made in this session is the
`dfsbench setup` that created `d8a08b01-…` plus its data volume; every other
call was `instance server list` / `get` / `iam ssh-key list`. `dfsbench
teardown` had not been run at that point, and `.bench-vm.json` has only ever
named `d8a08b01-…`. The VM is a **Kapsule-managed node-pool member**
(`managed=true`, `pool-name=coder-primary`), and the pool visibly churns — its
surviving sibling `d9f39027-…` was itself created the same morning at 07:35,
35 minutes after the one that vanished. Kapsule recycling it is the explanation
that fits; flagging it anyway rather than staying quiet about a named VM
changing state on my watch.

## Teardown

`dfsbench teardown` ran after asserting `.bench-vm.json`'s `server_id` was
neither coder VM and was `d8a08b01-…`, and after re-reading the live instance's
name. Verified afterwards, not assumed:

```
scw instance server get d8a08b01-…  -> Cannot find resource 'instance_server'
scw block volume get 0829be48-…     -> Cannot find resource 'volume'
scw instance server get d9f39027-…  -> scw-coder-coder-primary-… running
```


## Reading the evidence

The `.tsv` and `.log` files under `data/` are gzipped. Decompress before
rerunning the histogram script:

```bash
gunzip -k rig-1872/data/objects-run*.tsv.gz
python3 rig-1872/hist.py rig-1872/data/objects-runC.tsv
```

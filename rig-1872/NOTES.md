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

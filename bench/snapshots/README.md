# bench/snapshots

Scale/perf workloads for the reference-CAS snapshot pipeline. The same
library functions power both the `dfsbench snapshots` CLI subcommand and the
Go `Benchmark*` tests in this package.

## What it measures

A snapshot `create` is a metadata `Backup` (streamed dump + an in-RAM
`HashSet` of every referenced block hash) → manifest write → drain → verify
(HEAD-probe every manifest hash, concurrency 16). `restore` reads the
manifest back, resets, restores the dump, and re-verifies. The three cost
centers, isolated from the Runtime orchestration so one benchmark can sweep
file counts without standing up adapters / the control-plane DB / real S3:

| Workload         | Cost center                                              | Custom metric            |
|------------------|----------------------------------------------------------|--------------------------|
| `backup`         | dump stream + resident `HashSet` (create-path RAM)       | `dump_bytes`, `manifest_hashes` |
| `write-manifest` | sorted hex-line manifest, streamed                       | `manifest_bytes`         |
| `read-manifest`  | parse manifest back into a resident `HashSet` (restore)  | —                        |
| `verify`         | HEAD-probe every hash at concurrency 16                  | `probes`                 |

Backends: an in-memory remote (no S3 request cost) plus the memory or badger
metadata engine. `--dedup N` shares every Nth block hash to shrink the
unique-hash count; the default (`Dedup=1`, all-unique) is the worst case for
`HashSet` + manifest RAM.

## Running

CLI (one seed → backup → manifest → verify pass, wall-clock per stage):

```sh
go build -o dfsbench ./cmd/bench
./dfsbench snapshots --files 100000 --blocks-per-file 1
./dfsbench snapshots --files 100000 --blocks-per-file 8 --engine badger
./dfsbench snapshots --files 1000000 --dedup 4
```

Go benchmarks (benchstat-friendly; large 1e6 cases gated behind `-short`):

```sh
# CI-safe sweep (skips 1e6):
go test -bench=. -benchmem -short -run=^$ ./bench/snapshots/

# Full sweep including 1e6-file scales (heavy — seconds + hundreds of MB):
go test -bench=. -benchmem -benchtime=1x -run=^$ -timeout=900s ./bench/snapshots/
```

## Library API

```go
import snap "github.com/marmos91/dittofs/bench/snapshots"

store, uniqueHashes, cleanup, _ := snap.NewStore(ctx, snap.SeedOpts{
    Engine: snap.EngineMemory, Files: 1e5, BlocksPerFile: 1, Dedup: 1,
})
defer cleanup()

backup, _ := snap.RunBackup(ctx, store)          // dump bytes + HashSet
manifestBytes, _ := snap.RunWriteManifest(backup.HashSet)
snap.SeedRemote(ctx, rs, backup.HashSet)         // all-present remote
probes, _ := snap.RunVerify(ctx, rs, backup.HashSet)
```

## Established limits & memory ceiling

See [`docs/BENCHMARKS.md`](../../docs/BENCHMARKS.md#snapshot-scale-limits)
for the measured ceiling and the per-block verify budget. Summary:

- The metadata dump is **streamed by the badger engine** (KV-by-KV; the
  dump byte count never lands in a single buffer). On the badger path the
  dominant create-path allocation is the returned `HashSet`: one 32-byte
  `ContentHash` per **unique** referenced block, ~26 B/entry in the Go map.
  Plan ~25 MB of HashSet RAM per 1 M unique blocks. (The memory engine does
  NOT stream — see the last bullet.)
- The manifest is streamed on write (65 bytes/hash on disk: 64 hex + LF).
- Verify allocates per probe but holds nothing across probes; cost is N
  HEAD round-trips at concurrency 16. The in-memory remote number is a
  floor — multiply by real per-HEAD S3 RTT for a deployment budget.
- The memory metadata engine gob-encodes its whole snapshot into one
  buffer (expected for an in-RAM backend) — it is **not** suitable for
  TB-scale shares. Use badger for large shares; badger streams.

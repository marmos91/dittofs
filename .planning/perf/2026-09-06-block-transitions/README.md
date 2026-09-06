# Block state-transition baseline — recorded 2026-09-06

The before-numbers for the pier/crane/ferry refactor. Design-plan §10 asks for one
benchmark per state transition, allocations on every one, write amplification on the
write path, and a recorded baseline on real hardware. This is that baseline.

Raw `go test -bench` output is in `bench-5k.txt`, `bench-15k.txt`, `bench-tmpfs.txt`;
machine and method in `bench-env.txt`. Compare a later run with:

```sh
benchstat sbs5k=bench-5k.txt new=<later>.txt
```

## Machine

AMD EPYC 7543, 32 cores, 62 GiB, Linux 6.8.0-138, Go 1.25.0, Scaleway `fr-par-1`
`POP2-HC-32C-64G` — the `vmType` already recorded in `bench/infra/Pulumi.bench.yaml`,
so these sit alongside the earlier DittoFS baselines. Commit `74a74e812`.

Not a laptop and not Docker Desktop, per the plan's admissibility rule.

## Method

Two invocations per tier, `-count=6` on both:

```sh
go test ./pkg/block/journal/ -run '^$' -bench . -skip "$HEAVY" -benchtime=1s  -count=6
go test ./pkg/block/journal/ -run '^$' -bench "$HEAVY"          -benchtime=200x -count=6
HEAVY='BenchmarkCarve$|BenchmarkCarveScatteredPass$|BenchmarkEvict$|BenchmarkGCRepack$|BenchmarkTruncate$|BenchmarkDelete$'
```

Those six rebuild their state inside `StopTimer` every iteration. Go sizes `b.N` from
the timed region alone, so on a fast device it picks a count whose *untimed* setup runs
for tens of minutes — `Truncate` reached 28k iterations on tmpfs, each rebuilding 4096
intervals. They get an explicit iteration cap. `ns/op` stays comparable across the
split; only the sample count differs.

## The tiers, and a correction to what they were meant to show

Three store locations, one machine, run sequentially so they never contend:

| tier | device | `fdatasync=1` 4k IOPS (fio control) |
|---|---|---|
| `sbs5k` | Scaleway SBS, default | 476 |
| `sbs15k` | Scaleway SBS, provisioned 15000 IOPS | 451 |
| `ram` | tmpfs | 455,000 |

**The two block tiers are the same device for this workload.** 476 vs 451 IOPS, and the
benchmark geomean differs by +4.78% in the *slower* direction for the provisioned volume
— noise, not signal. Provisioned IOPS raises a parallel-throughput ceiling; it does not
move the ~2.1 ms network round-trip that a serialised fsync chain pays per barrier. So
the two-tier disk-sensitivity curve this run was commissioned to produce does not exist,
and `sbs15k` is retained only as evidence of that.

The RAM tier is what supplies the fast end. It is not a machine anyone should run on —
it is an upper bound that separates *device* cost from *code* cost, which is the split
the block-size decision actually needs.

## Results — `sbs5k` is the baseline, RAM shows what the device is buying

| transition | benchmark | sbs5k | ram | device share |
|---|---|---|---|---|
| tombstone | `Delete` | 14.13 ms | 14.03 µs | **1007×** |
| clip | `Truncate` | 24.62 ms | 36.67 µs | **671×** |
| durability, tiny writes | `TinyWritesCommit` | 1.714 ms | 6.071 µs | **282×** |
| durability, depth 1 | `ConcurrentCommit1` | 1.818 ms | 7.971 µs | **228×** |
| Resident → Remote | `Evict` | 2.063 ms | 194.7 µs | 10.6× |
| durability, depth 32 | `ConcurrentCommit32` | 125.9 µs | 11.75 µs | 10.7× |
| compaction | `GCRepack` | 4.828 ms | 1.004 ms | 4.8× |
| durability, depth 128 | `ConcurrentCommit128` | 42.56 µs | 12.91 µs | 3.3× |
| dirty write | `WriteAt` (64 KiB) | 99.63 µs | 35.09 µs | 2.8× |
| Remote → Resident | `Hydrate` | 55.09 µs | 35.42 µs | 1.6× |
| seq write | `SeqWrite4K` | 11.33 µs | 7.725 µs | 1.5× |
| bounded rand write | `RandWriteBounded` | 11.15 µs | 7.631 µs | 1.5× |
| **code-bound below this line** | | | | |
| carve | `Carve` (8 MiB) | 11.71 ms | 11.70 ms | 1.0× |
| carve, scattered | `CarveScatteredPass` | 406.7 ms | 405.2 ms | 1.0× |
| residency | `FileSize` | 23.93 µs | 23.91 µs | 1.0× |
| residency | `DataExtents` | 21.49 µs | 21.45 µs | 1.0× |
| residency | `ColdExtents` | 10.31 µs | 10.34 µs | 1.0× |
| residency | `DurableExtent` | 16.80 ns | 16.83 ns | 1.0× |
| cold read | `ReadCold` | 771.8 ns | 789.0 ns | 1.0× |
| warm read | `ReadWarm` | 4.744 µs | 5.719 µs | 0.8× |
| unbounded rand write | `RandWrite4K` | 103.7 µs | 118.4 µs | 0.9× |
| recovery | `OpenRecovery` (512 files) | 18.74 ms | 21.11 ms | 0.9× |

A ratio below 1.0 means RAM was *slower*: the work is CPU- or allocation-bound and the
32 GiB tmpfs competes with the process for memory. Those are not disk measurements at all.

## What this settles

**1. The durability path is the device, and group commit is why it is survivable.**
`ConcurrentCommit1` at 1.818 ms sits on the disk's synchronous-append floor. At depth 128
the same work costs 42.56 µs — one barrier amortised across the batch, a 43× reduction.
Any change to batching gets measured here first.

**2. `Truncate` and `Delete` are almost pure fsync.** 671× and 1007×. Both append one
marker and fsync it; on a network-backed volume the marker's barrier is the entire cost.
Worth knowing before anyone optimises the index-clip half, which is under 0.2% of it.

**3. The residency queries are pure CPU, and `FileSize` is the outlier.** Identical on
every tier, to three digits. `DurableExtent` answers in 16.8 ns; `FileSize` takes
23.93 µs — **1424× more** for a narrower question, because it scans every interval
(#2366). That gap is code, and no disk will close it.

**4. Carve is CPU-bound.** `Carve` is within noise across a 1000× device range: FastCDC
plus BLAKE3 plus dedup dominate, not the write-out. `CarveScatteredPass` is flat by
construction — the injected 50 µs deduper latency is most of its 406 ms.

**5. Recovery is allocation-bound, not read-bound.** `OpenRecovery` is *slower* on RAM.
36 MiB and 3426 allocations to replay 512 files, and it is a startup-latency SLO. It
lands next to #2366 on the share-start thread, not on the I/O thread.

## The stop condition: does the protocol layer bind before the block engine?

**Yes, on the write path.** #1735 measured the create wall at ~1062 ops/s ≈ 940 µs per
create, with block COMMIT fsync ~20% of it at nj=8. `ConcurrentCommit32` at 125.9 µs is
the right order for that share, from an independent direction. Roughly four fifths of a
create is spent above the block engine.

So the refactor cannot be justified on create-path throughput — the ceiling it could
move is about a fifth, and only at low queue depth. It has to stand on residency
correctness, which is what the plan claims for it anyway. The one place block-engine work
*does* pay is the code-bound set above: `FileSize`, recovery, and the carve CPU path.

## Caveats

- One machine, one run each. Six samples per point, sequential tiers, no other load.
- The remote store is `fakeRemote` (in-memory). Nothing here measures S3.
- `CarveScatteredPass` measures the packer against an *injected* 50 µs oracle latency,
  not a real one. Re-measure against the real oracle on this hardware before quoting it
  as an absolute.
- `sbs15k` is retained as evidence that the tier is not a variable, not as a data point.

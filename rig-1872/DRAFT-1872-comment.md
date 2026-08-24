<!-- DRAFT ONLY — not posted. Posting and closing are the maintainer's call. -->

Rig measurement for the block-packing branch (`perf/1872-block-packing@5b43a9ae`),
run back to back against `origin/develop@47a2d1fb` on one disposable SCW VM
(POP2-8C-32G, fr-par-1), same `dfsbench` harness binary on both sides, only
`dfs`/`dfsctl` swapped. Probe as specified: `dittofs-s3-writeback-nfs3`,
`--sizes large`, `--workloads seq-read,rand-write-4k`, default 60 s runtime,
cold barrier on.

Object size on the wire is read from an S3 listing of the `blocks/` prefix:
`PutBlock` writes one whole block with a single `PutObject`, so an object's
`Size` **is** the PUT size. Each run's objects are selected by `LastModified`.

## Object size on the scattered-write path

| | develop `47a2d1fb` | packed `5b43a9ae` |
| --- | --- | --- |
| objects | B_OBJ | 52 |
| bytes | B_BYTES | 200.5 MiB |
| median size | B_MED | **4,226,761 B** |
| min size | B_MIN | **2,496,751 B** |
| max size | B_MAX | 4,235,140 B |

The pre-fix quantisation this issue names — 4168 / 6216 / 8264 B — is gone. The
smallest object the branch produced on this path is 2.50 MB.

## Drain, and the cold barrier

Neither side's cold barrier passed.

* **develop** entered the barrier with **1035 MiB** unsynced (the original
  report: 935 MiB) and drained at 3–10 MiB per 30 s — the same slow shape as the
  49 → 34 → 45 → 22 → 2 → 2 MiB series in the original run. At that rate the
  backlog needs about an hour; the barrier allows fifteen minutes.
* **the branch** entered with **115 MiB** and then made *no* progress at all:
  30 consecutive 30-second samples reporting a byte-identical
  `unsynced_bytes`, with `pending_uploads: 0` and `blocks_dirty: 0`. Nine
  `carve: commit block …: Conflict: badger WithTransaction: transaction
  conflict` failures preceded it.

Warm-cell numbers, for completeness — and one of them needs care:

| | develop | packed |
| --- | --- | --- |
| `rand-write-4k` IOPS | 4291 | 892 |
| bytes written in the 60 s cell | 1046 MiB | 219 MiB |
| bytes durable on S3 during that cell | ~12 MiB | **200.5 MiB** |
| latency p99 | 123 ms | 2.26 s |
| `seq-read` warm MB/s | 2705 | 2711 |

develop's 4291 IOPS is the speed of dirtying a local append log; it leaves
1035 MiB undurable to earn it. The branch acknowledged fewer writes and made far
more of them durable in the same window. The p99 regression is real, though, and
is not explained by the 1.22–1.30x serialisation cost the branch's own
benchmark measured.

## What I am not claiming

This is one run per side on one VM, and the rig is noisy — read shape and order
of magnitude, not third digits. The object-size result is the exception: those
are exact byte counts from the object store, not a rig-derived rate.

I am **not** closing this. The criterion was 4 KiB → ~4 MiB *and* the cold
barrier passing; the first is met, the second is not, on either binary. That
means a second binding constraint exists which the packing work does not
address, and the issue should stay open until it is named.

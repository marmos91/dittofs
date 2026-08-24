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
| objects | 20,391 | **52** |
| bytes | 123.0 MiB | 200.5 MiB |
| median size | 4,168 B | **4,226,761 B** |
| min size | 4,168 B | **2,496,751 B** |
| max size | 53,321 B | 4,235,140 B |
| objects per MiB written | 165.8 | **0.259** |

The pre-fix quantisation this issue names — 4168 / 6216 / 8264 B — reproduced
exactly on develop: 13,861 objects at 4,168 B and 4,114 at 8,264 B, the whole
distribution 4096n + 72, largest 53,321 B. On the branch the smallest object on
that path is 2.50 MB. **640x fewer PUTs per byte; the median object is 1014x
larger.**

Those are exact byte counts from the object store, not a rig-derived rate, so
unlike everything below they carry no rig-noise caveat.

## Drain, and the cold barrier

Neither side's cold barrier passed.

* **develop** entered the barrier with **1035 MiB** unsynced (the original
  report: 935 MiB) and drained 1035 → 924 MiB over 30 samples (3–10 MiB per 30 s) — the same slow shape as the
  49 → 34 → 45 → 22 → 2 → 2 MiB series in the original run. At that rate the
  backlog needs about four hours; the barrier allows fifteen minutes. It
  failed with 924 MiB still unsynced — the original report's figure was 935 MiB.
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
| bytes durable on S3 across the whole 18 min (cell + drain) | 123.0 MiB | 200.5 MiB in the 60 s cell alone |
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

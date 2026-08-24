<!-- DRAFT ONLY. Not posted. Posting and closing are the maintainer's. -->

Rig measurement for the block-packing branch (`perf/1872-block-packing`),
back to back against `origin/develop@47a2d1fb` on one disposable SCW VM
(POP2-8C-32G, fr-par-1), same harness binary on both sides, only `dfs`/`dfsctl`
swapped. Probe as specified: `dittofs-s3-writeback-nfs3`, `--sizes large`,
`--workloads seq-read,rand-write-4k`, default 60 s runtime, cold barrier on.

Object size on the wire is read from the S3 listing of the `blocks/` prefix —
`PutBlock` writes one whole block with a single `PutObject`, so an object's
`Size` **is** the PUT size. Each run's objects are selected by `LastModified`.

## Object size on the scattered-write path

(TABLE FILLED IN AFTER RUN B)

## What this does and does not settle

(FILLED IN AFTER RUN B)

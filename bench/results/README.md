# bench/results

Scorecard artifacts emitted by `dfsbench parity` (#1467). Each run writes
three files:

- `parity-<label>-<timestamp>.json` — full machine-readable result: run
  metadata (host, commit, endpoint host — never credentials), every cell
  (tool × quadrant × concurrency), and per-cell datapath gauge timelines
  (inflight, window, queue depth, goodput, uploaded bytes, remote block reads)
  sampled from the Prometheus registry during dittofs cells.
- `parity-<label>-<timestamp>.csv` — the cells as one flat CSV.
- `parity-<label>-<timestamp>.md` — the human scorecard: dittofs vs rclone
  with the parity ratio, one table per quadrant, one row per concurrency.

The directory is gitignored except for `parity-local-smoke-sample.*`: a
committed example produced by `bench/scripts/parity-smoke.sh` against a LOCAL
MinIO container on a dev laptop. **The sample's numbers are localhost loopback
numbers — they validate the harness itself and are meaningless as WAN
throughput.** Real runs (Scaleway VM → Cubbit / SCW S3 via
`bench/scripts/parity-scw.sh`) are executed manually and their artifacts can
be committed here for the record.

// Package snapshots provides scale/perf workloads for the reference-CAS
// snapshot pipeline: metadata Backup (dump + hash manifest), manifest
// write/read, and remote-durability verify.
//
// The workloads here measure the three cost centers the snapshot create /
// verify / restore path is built from, isolated from the full Runtime
// orchestration so a single Go benchmark can sweep file counts (1e5, 1e6)
// and block counts (multi-GB equivalents) without standing up adapters,
// the control-plane DB, or a real S3 backend:
//
//   - Backup: cost of streaming the metadata dump to an io.Writer plus the
//     in-RAM HashSet the engine returns (one 32-byte ContentHash per unique
//     referenced block). The HashSet is the dominant non-streamed allocation
//     on the create path; SeedStore + BackupToWriter quantify it.
//   - Manifest: WriteManifest (sorted hex lines, streamed) and ReadManifest
//     (parse back into a HashSet). Manifest on-disk size is reported as a
//     custom metric.
//   - Verify: VerifyRemoteDurability HEAD-probes every manifest hash at the
//     production concurrency (16). RunVerify reports wall time and probe
//     count so the per-block verify budget can be read directly.
//
// Backends: an in-memory remote store (no S3 cost) plus the memory or
// badger metadata engine. The badger engine streams the dump KV-by-KV to
// the writer, so its create-path RAM is bounded by the returned HashSet
// rather than the dump size. The memory engine instead gob-encodes its
// whole snapshot into one buffer during Backup (expected for an in-RAM
// backend) — it is the convenient fast default for sweeping scales, but it
// is NOT the streaming ceiling and is unsuitable for TB-scale shares. The
// benchmarks quantify both; README.md documents the limits.
package snapshots

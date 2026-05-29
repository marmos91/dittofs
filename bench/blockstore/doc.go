// Package blockstore contains exported workload drivers for the
// pkg/blockstore engine. Each workload composes the production
// FSStore + remote + in-memory metadata + Syncer stack and exposes
// a single-shot Run* function callable from both the cmd/bench
// orchestrator (macro / pprof / real backend) and Go Benchmark*
// tests in workloads_test.go (micro / benchstat-friendly).
//
// The package is intentionally backend-agnostic at the type level —
// SetupRemote selects memory vs S3 at runtime based on Opts. New
// workloads should follow the same shape: take an *engine.Store and
// Opts, return (bytes int64, err error), do no profiling, do no
// timing. The caller (cmd or test) is responsible for that envelope.
package blockstore

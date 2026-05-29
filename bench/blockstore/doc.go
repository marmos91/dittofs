// Package blockstore contains exported workload drivers for the
// pkg/blockstore engine. Engine-backed workloads share one entry
// point — RunWorkload(ctx, *engine.Store, Opts) Result — which
// dispatches on Opts.Workload, seeds the working set outside the
// timed region, runs Opts.Ops iterations, and returns Result
// (Duration, Ops, Bytes, StatsBefore, StatsAfter). The same API
// powers both the cmd/bench orchestrator (macro / pprof / real
// backend) and the Go Benchmark* tests in workloads_test.go
// (micro / benchstat-friendly).
//
// A small number of workloads bypass the engine and run directly
// against a local FSStore or a remote.RemoteStore (walk, delete,
// gc, raw-s3-put); those expose their own exported Run* helpers in
// workloads_extra.go and follow the same Opts / Result contract.
//
// The package is intentionally backend-agnostic at the type level:
// SetupRemote selects memory vs S3 at runtime based on Opts.Remote,
// and NewEngine wires the standard FSStore + remote + Syncer stack.
// RunWorkload does no profiling and owns no engine lifecycle — the
// caller (cmd or test) is responsible for the timing / pprof
// envelope.
package blockstore

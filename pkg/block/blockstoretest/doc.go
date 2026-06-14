// Package blockstoretest provides a unified conformance suite for the
// BlockStore and BlockStoreAppend contracts declared in
// pkg/block/blockstore.go.
//
// Two top-level entrypoints are exposed:
//
//   - BlockStoreConformance(t, factory) — runs the CAS-keyed contract
//     suite against any BlockStore implementation. The fs, s3, and
//     memory backends all call this entrypoint.
//   - BlockStoreAppendConformance(t, factory) — runs the random-write
//     absorber suite against any BlockStoreAppend implementation. Only
//     the fs backend implements BlockStoreAppend and therefore calls
//     this entrypoint.
//
// This package replaces pkg/block/local/localtest and
// pkg/block/remote/remotetest. The three fs-internal scenarios
// that cannot be expressed through the interface surface
// (PressureChannel_INV05, TornWriteRecovery_LSL06,
// RollupOffsetMonotone_INV03) live in
// pkg/block/local/fs/appendlog_internals_test.go.
//
// Each scenario uses a factory that returns a fresh (BlockStore
// cleanup) pair per subtest, so subtests do not share state and
// teardown is deterministic. See conformance.go and appendlog.go for
// the factory type definitions.
package blockstoretest

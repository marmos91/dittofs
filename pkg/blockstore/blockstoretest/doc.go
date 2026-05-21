// Package blockstoretest provides a unified conformance suite for the
// Phase 17 BlockStore and BlockStoreAppend contracts declared in
// pkg/blockstore/blockstore.go.
//
// Two top-level entrypoints are exposed (Phase 17 D-09):
//
//   - BlockStoreConformance(t, factory) — runs the CAS-keyed contract
//     suite against any BlockStore implementation. The fs, s3, and
//     memory backends all call this entrypoint.
//   - BlockStoreAppendConformance(t, factory) — runs the random-write
//     absorber suite against any BlockStoreAppend implementation. Only
//     the fs backend implements BlockStoreAppend and therefore calls
//     this entrypoint.
//
// This package replaces pkg/blockstore/local/localtest and
// pkg/blockstore/remote/remotetest, which were deleted by Plan 17-03
// (remotetest) and Plan 17-06 (localtest) after the fs / s3 / memory
// backends were wired against the new factories. The three
// fs-internal scenarios that cannot be expressed through the
// interface surface (PressureChannel_INV05, TornWriteRecovery_LSL06,
// RollupOffsetMonotone_INV03) live in
// pkg/blockstore/local/fs/appendlog_internals_test.go.
//
// Each scenario uses a factory that returns a fresh (BlockStore,
// cleanup) pair per subtest, so subtests do not share state and
// teardown is deterministic. See conformance.go and appendlog.go for
// the factory type definitions.
package blockstoretest

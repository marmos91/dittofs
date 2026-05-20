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
// pkg/blockstore/remote/remotetest, which are deleted in Plan 17-07
// after the fs / s3 / memory backends are wired against the new
// factories. Until then, the legacy suites continue to compile and
// pass; this package contains the scenarios only — backend factories
// land in Plan 17-06 / 17-07.
//
// Each scenario uses a factory that returns a fresh (BlockStore,
// cleanup) pair per subtest, so subtests do not share state and
// teardown is deterministic. See conformance.go and appendlog.go for
// the factory type definitions.
package blockstoretest

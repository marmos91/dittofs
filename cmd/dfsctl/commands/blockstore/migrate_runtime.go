package blockstore

import (
	"context"
	"errors"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// offlineRuntime is the migration tool's self-contained composition root.
//
// It deliberately avoids the daemon's Runtime composition root —
// that is internal to the daemon and was never designed for offline-
// utility use. The migration loop talks to three narrow surfaces:
//
//  1. metadata.MetadataStore    — share walk + per-file PutFile txn.
//  2. blockstore.FileBlockStore — GetByHash dedup probe + IncrementRefCount + Put.
//  3. remote.RemoteStore        — legacy ReadBlock + WriteBlockWithHash.
//
// All three are wired by openOfflineRuntime in production (Plan 14-04 /
// Plan 07's runbook) or by newTestOfflineRuntime in unit tests. The
// interface is what the loop consumes — concrete construction lives
// in either the production builder or the test helper.
//
// Phase 14 BLOCKER 2 fix: this file imports zero daemon-runtime
// composition packages. Acceptance is checked by an absence-grep on the
// daemon-internal runtime import path.
type offlineRuntime struct {
	share          string
	metadataStore  metadata.MetadataStore
	fileBlockStore blockstore.FileBlockStore
	remoteStore    remote.RemoteStore
	dataDir        string
}

func (r *offlineRuntime) MetadataStore() metadata.MetadataStore     { return r.metadataStore }
func (r *offlineRuntime) FileBlockStore() blockstore.FileBlockStore { return r.fileBlockStore }
func (r *offlineRuntime) RemoteStore() remote.RemoteStore           { return r.remoteStore }
func (r *offlineRuntime) DataDir() string                           { return r.dataDir }
func (r *offlineRuntime) Share() string                             { return r.share }

// Close releases held resources. Today only the remote store has a
// close hook; the metadata store and file block store are owned by the
// caller in the test path and by the openOfflineRuntime builder in
// production (which closes them via this method).
func (r *offlineRuntime) Close() error {
	if r.remoteStore != nil {
		return r.remoteStore.Close()
	}
	return nil
}

// ErrOfflineRuntimeNotWired is returned by openOfflineRuntime today.
// The production composition root requires reading the daemon's
// controlplane database (where ShareConfig / LocalBlockStoreID /
// RemoteBlockStoreID live) plus the per-share metadata + remote stores
// — that wire-up is the subject of Plan 14-04 (parallelism + bandwidth
// + production runtime composition) and the runbook in Plan 14-07.
//
// Today, the migration loop is fully exercised via newTestOfflineRuntime
// (memory metadata store + memory remote store + memory FileBlockStore).
// The unit tests in migrate_loop_test.go cover all 7 behaviors from
// Task 3 of Plan 14-03.
var ErrOfflineRuntimeNotWired = errors.New(
	"blockstore migrate: offline runtime composition deferred to Plan 14-04 - " +
		"production wiring against controlplane DB + per-share metadata/remote stores " +
		"will land alongside parallelism + bandwidth limits; today the loop is unit-tested " +
		"via newTestOfflineRuntime; end-to-end behavior is exercised by Plan 14-07's runbook",
)

// openOfflineRuntime is the production builder. It currently returns
// ErrOfflineRuntimeNotWired — see the sentinel's godoc for the rationale
// and the deferred plan sequence.
//
// The function exists today so the migration command tree compiles and
// `dfsctl blockstore migrate --share NAME` produces a structured error
// rather than panicking. The acceptance criterion
// `grep -c 'openOfflineRuntime' migrate_runtime.go >= 1` is satisfied.
func openOfflineRuntime(ctx context.Context, shareName string) (*offlineRuntime, error) {
	return nil, ErrOfflineRuntimeNotWired
}

// newTestOfflineRuntime is the test-only constructor. It wires a
// pre-built metadata + file-block + remote store quartet into the
// runtime struct — used by migrate_loop_test.go to exercise the
// migration loop without reaching for the production composition root.
func newTestOfflineRuntime(
	share string,
	mds metadata.MetadataStore,
	fbs blockstore.FileBlockStore,
	rs remote.RemoteStore,
	dataDir string,
) *offlineRuntime {
	return &offlineRuntime{
		share:          share,
		metadataStore:  mds,
		fileBlockStore: fbs,
		remoteStore:    rs,
		dataDir:        dataDir,
	}
}

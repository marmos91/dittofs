package engine

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journal/memory"
)

// nopDataplaneMetrics satisfies both journal.MetricsRecorder (what SetMetrics
// accepts) and DataplaneMetrics (the capability it probes for), which is what
// makes it reach the Store's metrics cell.
type nopDataplaneMetrics struct{}

func (nopDataplaneMetrics) RecordBackpressure(time.Duration)        {}
func (nopDataplaneMetrics) RecordEviction(int64)                    {}
func (nopDataplaneMetrics) RecordUpload(int, string, time.Duration) {}
func (nopDataplaneMetrics) UploadStarted()                          {}
func (nopDataplaneMetrics) UploadFinished()                         {}
func (nopDataplaneMetrics) SetUploadQueueDepth(int)                 {}
func (nopDataplaneMetrics) SetUploadWindow(int)                     {}
func (nopDataplaneMetrics) SetUploadGoodput(float64)                {}
func (nopDataplaneMetrics) RecordLocalCorruption(int)               {}
func (nopDataplaneMetrics) RecordSelfHealSuccess(int)               {}
func (nopDataplaneMetrics) RecordSelfHealFailure(int)               {}
func (nopDataplaneMetrics) RecordRemoteCorruption(int)              {}
func (nopDataplaneMetrics) RecordBlockRangeRead(int)                {}

// TestSetMetrics_ReachesSyncerAfterConstruction pins the late-binding property
// the syncer's metrics wiring exists for: the runtime builds shares before the
// metrics registry, so SetMetrics lands on an ALREADY-CONSTRUCTED Store and the
// syncer must observe that write.
//
// The syncer therefore has to hold the Store's metrics cell, not a copy of its
// value. Capturing the value at construction time compiles, passes every other
// test in the tree (all five read sites are `if mx := ...; mx != nil` guards, so
// a nil cell degrades silently to "no metrics"), and leaves the upload and
// self-heal instruments dead in production.
//
// The pre-SetMetrics assertion is what keeps this non-vacuous: it proves the
// sink observed below came from the back-fill and not from construction.
func TestSetMetrics_ReachesSyncerAfterConstruction(t *testing.T) {
	localStore := memory.New()
	syncer := NewRemoteSync(localStore, nil, newStubFileChunkStore(), DefaultConfig())

	bs, err := New(BlockStoreConfig{Local: localStore, RemoteSync: syncer})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer func() { _ = bs.Close() }()

	if got := syncer.dataplaneMetrics(); got != nil {
		t.Fatalf("dataplaneMetrics before SetMetrics = %v, want nil", got)
	}

	bs.SetMetrics(nopDataplaneMetrics{})

	if syncer.dataplaneMetrics() == nil {
		t.Fatal("syncer did not observe a SetMetrics back-fill on an already-constructed Store")
	}
}

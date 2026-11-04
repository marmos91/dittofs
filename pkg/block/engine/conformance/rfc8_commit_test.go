//go:build engine_conformance

package conformance

import (
	"bytes"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
)

// The group commit syncs a file in the journal before it commits its
// existence: once the File record carries a size, the bytes under it survive
// a crash, and read back after it.
func TestRFC8A_SyncBeforeExistence(t *testing.T) {
	f := enginetest.New(t)
	h, id := f.File("a")
	p := []byte("its size commits only after its sync")
	write(t, f, id, 0, p)
	f.Clock.Advance(enginetest.Settings.ExistenceAge)
	eventually(t, "the group commit commits the write's size", func() bool { return f.Committed(h).Size == int64(len(p)) })
	if !durable(f, p) {
		t.Fatal("the size is committed while the bytes under it are not durable")
	}
	f.Crash()
	if got := read(t, f, id, 0, len(p)); !bytes.Equal(got, p) {
		t.Errorf("after the crash: read %q, want %q", got, p)
	}
}

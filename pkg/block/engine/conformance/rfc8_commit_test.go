//go:build engine_conformance

package conformance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
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

// A Commit over an overwrite of committed content returns only once the
// overwrite's existence has committed: the File record carries it when
// Commit answers.
func TestRFC8A_OverwriteWaitsForExistence(t *testing.T) {
	f := enginetest.New(t)
	h, id := f.File("a")
	write(t, f, id, 0, []byte("committed"))
	f.Clock.Advance(enginetest.Settings.ExistenceAge)
	eventually(t, "the first write's existence commits", func() bool { return f.Committed(h).Size == 9 })
	at := f.Committed(h).Applied
	write(t, f, id, 0, []byte("overwrite"))
	if _, _, err := f.E.Commit(t.Context(), f.Share.ID, id, 0, 9); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := f.Committed(h).Applied; got.Compare(at) <= 0 {
		t.Errorf("Commit answered with applied still %v: the overwrite's existence had not committed", got)
	}
}

// Under a store that commits nothing, a Commit over an append answers after
// its sync, and one over an overwrite of committed content answers ErrDelay
// at its deadline, never success.
func TestRFC8A_StalledStore(t *testing.T) {
	f := enginetest.New(t)
	h, id := f.File("a")
	write(t, f, id, 0, []byte("committed"))
	f.Clock.Advance(enginetest.Settings.ExistenceAge)
	eventually(t, "the first write's existence commits", func() bool { return f.Committed(h).Size == 9 })
	f.Stall()

	write(t, f, id, 9, []byte("appended"))
	if _, _, err := f.E.Commit(t.Context(), f.Share.ID, id, 9, 8); err != nil {
		t.Errorf("an append's Commit under a stalled store: %v", err)
	}
	write(t, f, id, 0, []byte("overwrite"))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, _, err := f.E.Commit(ctx, f.Share.ID, id, 0, 9)
	var d *vfserr.Delay
	if !errors.As(err, &d) || d.Cause != vfserr.DelayCommit {
		t.Errorf("an overwrite's Commit under a stalled store: %v, want ErrDelay(DelayCommit)", err)
	}
}

// 64 writers flushing while chmod writes random files' records: every flush
// is answered, none waiting past its deadline on another file's conflict.
func TestRFC8A_GroupCommit(t *testing.T) {
	f := enginetest.New(t)
	type file struct {
		h  entity.Handle
		id entity.FileID
	}
	files := make([]file, 64)
	for i := range files {
		h, id := f.File(fmt.Sprintf("f%d", i))
		files[i] = file{h, id}
		write(t, f, id, 0, []byte("committed"))
	}
	f.Clock.Advance(enginetest.Settings.ExistenceAge)
	eventually(t, "every file's first write commits", func() bool { return f.Committed(files[63].h).Size == 9 })

	stop := make(chan struct{})
	var chmods sync.WaitGroup
	chmods.Add(1)
	go func() {
		defer chmods.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			h := files[rand.IntN(len(files))].h
			_, _ = f.FS.Store.Files().SetAttrs(context.Background(), f.FS.Admin, h, entity.Attrs{}.WithMode(uint32(0o600+i%2)))
		}
	}()
	var flushes sync.WaitGroup
	for _, fl := range files {
		flushes.Add(1)
		go func() {
			defer flushes.Done()
			for n := range 4 {
				write(t, f, fl.id, 0, []byte(fmt.Sprintf("overwrite%d", n)))
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				_, _, err := f.E.Commit(ctx, f.Share.ID, fl.id, 0, 10)
				cancel()
				if err != nil {
					t.Errorf("a flush beside the chmods: %v", err)
					return
				}
			}
		}()
	}
	flushes.Wait()
	close(stop)
	chmods.Wait()
}

// A sync window the journal fails is reported to the file's next Commit as a
// success with a verifier other than the write's and a raised loss sequence,
// and the verifier after a new write is that write's.
func TestRFC8A_FailedWindow(t *testing.T) {
	f := enginetest.New(t)
	_, id := f.File("a")
	v := write(t, f, id, 0, []byte("in a failed window"))
	f.FailNextSync()
	cv, seq, err := f.E.Commit(t.Context(), f.Share.ID, id, 0, 0)
	if err != nil || cv == v || seq == 0 {
		t.Errorf("Commit over a failed window: verifier %x, sequence %d, %v; want success, other than %x, raised", cv, seq, err, v)
	}
	w := write(t, f, id, 0, []byte("after it"))
	if cw, _, err := f.E.Commit(t.Context(), f.Share.ID, id, 0, 0); err != nil || cw != w {
		t.Errorf("the Commit after a new write: %x, %v; want the write's %x", cw, err, w)
	}
}

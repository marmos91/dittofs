package memory

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journal"
)

func TestFlushSnapshotCannotClearConcurrentMutation(t *testing.T) {
	for _, mutation := range []string{"overwrite", "replace", "truncate"} {
		t.Run(mutation, func(t *testing.T) {
			ctx := context.Background()
			s := New()
			if err := s.WriteAt(ctx, "file", 0, bytes.Repeat([]byte{'a'}, 4096)); err != nil {
				t.Fatal(err)
			}
			flushAll := func(_ context.Context, run journal.Run) ([]journal.Extent, error) {
				return []journal.Extent{run.Extent}, nil
			}
			if err := s.Flush(ctx, "file", journal.FlushOptions{Force: true}, flushAll); err != nil {
				t.Fatal(err)
			}
			// The new dirty charge is much smaller than the snapshot offered by
			// Flush, which includes the surrounding already-carved bytes.
			if err := s.WriteAt(ctx, "file", 0, []byte{'b'}); err != nil {
				t.Fatal(err)
			}
			entered, resume := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- s.Flush(ctx, "file", journal.FlushOptions{Force: true}, func(_ context.Context, run journal.Run) ([]journal.Extent, error) {
					close(entered)
					<-resume
					return []journal.Extent{run.Extent}, nil
				})
			}()
			<-entered
			wantByte, wantSize := byte('c'), 4096
			var err error
			switch mutation {
			case "overwrite":
				err = s.WriteAt(ctx, "file", 0, []byte{wantByte})
			case "replace":
				err = s.Delete(ctx, "file")
				if err == nil {
					err = s.WriteAt(ctx, "file", 0, bytes.Repeat([]byte{wantByte}, wantSize))
				}
				if err == nil {
					// More than one write may land on the recreated ID before the
					// retired snapshot completes; none belongs to the old pass.
					err = s.WriteAt(ctx, "file", 0, []byte{wantByte})
				}
			case "truncate":
				wantByte, wantSize = 'b', 2048
				err = s.Truncate(ctx, "file", int64(wantSize))
			}
			close(resume)
			if flushErr := <-done; flushErr != nil {
				t.Fatal(flushErr)
			}
			if err != nil {
				t.Fatal(err)
			}
			if dirty, err := s.HasDirty(ctx, "file"); err != nil || !dirty {
				t.Fatalf("old flush cleared the changed file: dirty=%v err=%v", dirty, err)
			}
			var uploaded []byte
			if err := s.Flush(ctx, "file", journal.FlushOptions{Force: true}, func(_ context.Context, run journal.Run) ([]journal.Extent, error) {
				uploaded = make([]byte, run.Extent.Len)
				_, err := run.ReadAt(uploaded, run.Extent.Off)
				return []journal.Extent{run.Extent}, err
			}); err != nil {
				t.Fatal(err)
			}
			if len(uploaded) != wantSize || uploaded[0] != wantByte {
				t.Fatalf("retry omitted current content: uploaded=%d bytes, want %d starting with %q", len(uploaded), wantSize, wantByte)
			}
			if dirty, err := s.HasDirty(ctx, "file"); err != nil || dirty {
				t.Fatalf("complete retry remains dirty: dirty=%v err=%v", dirty, err)
			}
		})
	}
}

func TestFlushPartialCreditKeepsUncoveredDirtyByte(t *testing.T) {
	ctx := context.Background()
	s := New()
	if err := s.Hydrate(ctx, "file", 0, make([]byte, 4096), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAt(ctx, "file", 4095, []byte{'z'}); err != nil {
		t.Fatal(err)
	}
	failed := errors.New("second half failed")
	err := s.Flush(ctx, "file", journal.FlushOptions{Force: true}, func(context.Context, journal.Run) ([]journal.Extent, error) {
		return []journal.Extent{{Off: 0, Len: 2048}}, failed
	})
	if !errors.Is(err, failed) {
		t.Fatalf("partial failure: %v", err)
	}
	if dirty, err := s.HasDirty(ctx, "file"); err != nil || !dirty {
		t.Fatalf("credit outside the dirty byte cleared it: dirty=%v err=%v", dirty, err)
	}
	if err := s.Flush(ctx, "file", journal.FlushOptions{Force: true}, func(context.Context, journal.Run) ([]journal.Extent, error) {
		return []journal.Extent{{Off: 2048, Len: 2048}, {Off: 0, Len: 2048}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if dirty, err := s.HasDirty(ctx, "file"); err != nil || dirty {
		t.Fatalf("complete segmented credit remains dirty: dirty=%v err=%v", dirty, err)
	}
}

func TestFlushSerializesAcrossConcurrentMutation(t *testing.T) {
	for _, replace := range []bool{false, true} {
		name := "overwrite"
		if replace {
			name = "delete-and-recreate"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := New()
			if err := s.Hydrate(ctx, "file", 0, make([]byte, 4096), 0); err != nil {
				t.Fatal(err)
			}
			if err := s.WriteAt(ctx, "file", 0, []byte{'b'}); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var published []byte
			var order []string
			publish := func(label string, run journal.Run) ([]journal.Extent, error) {
				data := make([]byte, run.Extent.Len)
				if _, err := run.ReadAt(data, run.Extent.Off); err != nil {
					return nil, err
				}
				mu.Lock()
				published = data
				order = append(order, label)
				mu.Unlock()
				return []journal.Extent{run.Extent}, nil
			}
			entered, resume := make(chan struct{}), make(chan struct{})
			var resumeOnce sync.Once
			release := func() { resumeOnce.Do(func() { close(resume) }) }
			defer release()
			firstDone := make(chan error, 1)
			go func() {
				firstDone <- s.Flush(ctx, "file", journal.FlushOptions{Force: true}, func(_ context.Context, run journal.Run) ([]journal.Extent, error) {
					close(entered)
					<-resume
					return publish("old", run)
				})
			}()
			<-entered
			if replace {
				if err := s.Delete(ctx, "file"); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.WriteAt(ctx, "file", 0, bytes.Repeat([]byte{'c'}, 4096)); err != nil {
				t.Fatal(err)
			}
			// This pass must wait for the first callback even after replacement.
			// Its deadline gives the test a deterministic end to that wait. Without
			// serialization it publishes the new bytes and clears dirty state first.
			waitCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
			defer cancel()
			waitErr := s.Flush(waitCtx, "file", journal.FlushOptions{Force: true}, func(_ context.Context, run journal.Run) ([]journal.Extent, error) {
				return publish("overtook", run)
			})
			release()
			if err := <-firstDone; err != nil {
				t.Fatal(err)
			}
			if dirty, err := s.HasDirty(ctx, "file"); err != nil || !dirty {
				t.Fatalf("old manifest published over newer clean state: dirty=%v err=%v", dirty, err)
			}
			if !errors.Is(waitErr, context.DeadlineExceeded) {
				t.Fatalf("overlapping flush bypassed the older pass: %v", waitErr)
			}
			if err := s.Flush(ctx, "file", journal.FlushOptions{Force: true}, func(_ context.Context, run journal.Run) ([]journal.Extent, error) {
				return publish("current", run)
			}); err != nil {
				t.Fatal(err)
			}
			if dirty, err := s.HasDirty(ctx, "file"); err != nil || dirty {
				t.Fatalf("final flush did not clean current bytes: dirty=%v err=%v", dirty, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if !bytes.Equal(published, bytes.Repeat([]byte{'c'}, 4096)) {
				t.Fatal("final manifest describes stale content")
			}
			if len(order) != 2 || order[0] != "old" || order[1] != "current" {
				t.Fatalf("flush publication order: %v", order)
			}
		})
	}
}

func TestFlushRetainsDirtyDataUntilAfterFileSucceeds(t *testing.T) {
	for _, panicCleanup := range []bool{false, true} {
		name := "error"
		if panicCleanup {
			name = "panic"
		}
		t.Run(name, func(t *testing.T) {
			ctx, s := context.Background(), New()
			if err := s.WriteAt(ctx, "file", 0, []byte("data")); err != nil {
				t.Fatal(err)
			}
			cleanupFailure := errors.New("cleanup failed")
			var err error
			var recovered any
			called := false
			func() {
				defer func() { recovered = recover() }()
				err = s.Flush(ctx, "file", journal.FlushOptions{AfterFile: func(ctx context.Context, id journal.FileID) error {
					called = true
					if dirty, err := s.HasDirty(ctx, id); err != nil || !dirty || s.UnsyncedBytes() != 4 {
						t.Errorf("cleanup began after dirty credit: dirty=%v bytes=%d error=%v", dirty, s.UnsyncedBytes(), err)
					}
					if panicCleanup {
						panic(cleanupFailure)
					}
					return cleanupFailure
				}}, func(_ context.Context, run journal.Run) ([]journal.Extent, error) {
					return []journal.Extent{run.Extent}, nil
				})
			}()
			if !called || (panicCleanup && recovered != cleanupFailure) || (!panicCleanup && !errors.Is(err, cleanupFailure)) {
				t.Fatalf("cleanup outcome: called=%v panic=%v error=%v", called, recovered, err)
			}
			if dirty, err := s.HasDirty(ctx, "file"); err != nil || !dirty || s.UnsyncedBytes() != 4 {
				t.Fatalf("failed cleanup lost retry data: dirty=%v bytes=%d error=%v", dirty, s.UnsyncedBytes(), err)
			}
			if err := s.Flush(ctx, "file", journal.FlushOptions{}, func(_ context.Context, run journal.Run) ([]journal.Extent, error) {
				return []journal.Extent{run.Extent}, nil
			}); err != nil {
				t.Fatal(err)
			}
			if dirty, err := s.HasDirty(ctx, "file"); err != nil || dirty || s.UnsyncedBytes() != 0 {
				t.Fatalf("successful retry remains dirty: dirty=%v bytes=%d error=%v", dirty, s.UnsyncedBytes(), err)
			}
		})
	}
}

func TestFlushCleanupRemainsActiveAfterConcurrentTruncate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := New()
	if err := s.WriteAt(ctx, "file", 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	err := s.Flush(ctx, "file", journal.FlushOptions{AfterFile: func(cleanupCtx context.Context, id journal.FileID) error {
		if err := cleanupCtx.Err(); err != nil {
			t.Fatalf("cleanup inherited cancelled request: %v", err)
		}
		if err := s.Truncate(cleanupCtx, id, 0); err != nil {
			return err
		}
		if dirty, err := s.HasDirty(cleanupCtx, id); err != nil || !dirty {
			t.Fatalf("active cleanup became invisible after truncate: dirty=%v error=%v", dirty, err)
		}
		return nil
	}}, func(_ context.Context, run journal.Run) ([]journal.Extent, error) {
		cancel()
		return []journal.Extent{run.Extent}, context.Canceled
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("flush lost original failure: %v", err)
	}
	if dirty, err := s.HasDirty(context.Background(), "file"); err != nil || dirty || s.UnsyncedBytes() != 0 {
		t.Fatalf("finished empty file remains dirty: dirty=%v bytes=%d error=%v", dirty, s.UnsyncedBytes(), err)
	}
}

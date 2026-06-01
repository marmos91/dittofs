package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// TestStore_CloseDrainsInFlightWriteAt is the area-7 H-A reproducer: a client
// WriteAt racing an admin RemoveShare → Store.Close must NEVER race or panic.
// The op either completes fully against a live store or fails fast with
// ErrStoreClosed — never a torn/partial write.
//
// Before the close-gate, WriteAt operated on local/syncer state with no lock
// while Close tore those down concurrently → -race flags the data race (and a
// nil-deref panic is possible). With the gate, Close takes closeMu.Lock and
// drains, so this passes under -race across many scheduler interleavings.
func TestStore_CloseDrainsInFlightWriteAt(t *testing.T) {
	for i := 0; i < 100; i++ {
		bs := newTestEngine(t, 0, 0)
		ctx := context.Background()
		payloadID := "race-file"
		data := []byte("hello concurrent world")

		var wg sync.WaitGroup
		wg.Add(2)

		var writeErr error
		go func() {
			defer wg.Done()
			_, writeErr = bs.WriteAt(ctx, payloadID, nil, data, 0)
		}()
		go func() {
			defer wg.Done()
			_ = bs.Close()
		}()

		wg.Wait()

		// The write either fully succeeded (ran before close drained) or was
		// refused with ErrStoreClosed (arrived after closed=true). Any other
		// error, or a torn write, is a failure.
		if writeErr != nil && !errors.Is(writeErr, ErrStoreClosed) {
			t.Fatalf("iter %d: unexpected WriteAt error: %v", i, writeErr)
		}
	}
}

// TestStore_CloseDrainsInFlightReadAt is the read-side twin of the H-A
// reproducer.
func TestStore_CloseDrainsInFlightReadAt(t *testing.T) {
	for i := 0; i < 100; i++ {
		bs := newTestEngine(t, 0, 0)
		ctx := context.Background()
		payloadID := "race-read-file"
		// Seed some data so ReadAt has something to walk. Assert the seed
		// succeeds so a seed failure can't make the lifecycle assertion
		// below pass/fail for the wrong reason — the seed runs before any
		// Close, so it must never error.
		if _, err := bs.WriteAt(ctx, payloadID, nil, []byte("0123456789"), 0); err != nil {
			t.Fatalf("iter %d: seed WriteAt failed: %v", i, err)
		}

		var wg sync.WaitGroup
		wg.Add(2)

		var readErr error
		go func() {
			defer wg.Done()
			buf := make([]byte, 10)
			_, readErr = bs.ReadAt(ctx, payloadID, nil, buf, 0)
		}()
		go func() {
			defer wg.Done()
			_ = bs.Close()
		}()

		wg.Wait()

		// The in-flight ReadAt either completed before Close drained (nil
		// error — it ran fully against a live store) or arrived after
		// closed=true and was refused with the typed ErrStoreClosed. Any
		// other error is a lifecycle bug: the gate must convert a
		// raced-Close into ErrStoreClosed, never a torn read or untyped
		// failure. (The seed above guarantees the content is present, so a
		// successful read can't be a spurious miss.)
		if readErr != nil && !errors.Is(readErr, ErrStoreClosed) {
			t.Fatalf("iter %d: unexpected ReadAt error: %v", i, readErr)
		}
	}
}

// TestStore_CloseIsIdempotent verifies a second Close is a no-op returning the
// first call's result (badger #900 pattern).
func TestStore_CloseIsIdempotent(t *testing.T) {
	bs := newTestEngine(t, 0, 0)

	first := bs.Close()
	second := bs.Close()
	if first != nil {
		t.Fatalf("first Close returned error: %v", first)
	}
	if second != nil {
		t.Fatalf("second Close should be a no-op returning the first result, got: %v", second)
	}

	// Concurrent double-Close must also be safe (no double-teardown panic).
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = bs.Close()
		}()
	}
	wg.Wait()
}

// TestStore_OpAfterCloseReturnsErrStoreClosed verifies every gated public data
// op fails fast with ErrStoreClosed once the store is closed.
func TestStore_OpAfterCloseReturnsErrStoreClosed(t *testing.T) {
	bs := newTestEngine(t, 0, 0)
	ctx := context.Background()
	if err := bs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	t.Run("WriteAt", func(t *testing.T) {
		if _, err := bs.WriteAt(ctx, "p", nil, []byte("x"), 0); !errors.Is(err, ErrStoreClosed) {
			t.Fatalf("want ErrStoreClosed, got %v", err)
		}
	})
	t.Run("ReadAt", func(t *testing.T) {
		buf := make([]byte, 4)
		if _, err := bs.ReadAt(ctx, "p", nil, buf, 0); !errors.Is(err, ErrStoreClosed) {
			t.Fatalf("want ErrStoreClosed, got %v", err)
		}
	})
	t.Run("Flush", func(t *testing.T) {
		if _, err := bs.Flush(ctx, "p"); !errors.Is(err, ErrStoreClosed) {
			t.Fatalf("want ErrStoreClosed, got %v", err)
		}
	})
	t.Run("Truncate", func(t *testing.T) {
		if _, err := bs.Truncate(ctx, "p", nil, 0); !errors.Is(err, ErrStoreClosed) {
			t.Fatalf("want ErrStoreClosed, got %v", err)
		}
	})
	t.Run("Delete", func(t *testing.T) {
		if err := bs.Delete(ctx, "p", nil); !errors.Is(err, ErrStoreClosed) {
			t.Fatalf("want ErrStoreClosed, got %v", err)
		}
	})
	t.Run("CopyPayload", func(t *testing.T) {
		if _, err := bs.CopyPayload(ctx, "s", "d", nil); !errors.Is(err, ErrStoreClosed) {
			t.Fatalf("want ErrStoreClosed, got %v", err)
		}
	})
	t.Run("GetSize", func(t *testing.T) {
		if _, err := bs.GetSize(ctx, "p"); !errors.Is(err, ErrStoreClosed) {
			t.Fatalf("want ErrStoreClosed, got %v", err)
		}
	})
	t.Run("Exists", func(t *testing.T) {
		if _, err := bs.Exists(ctx, "p"); !errors.Is(err, ErrStoreClosed) {
			t.Fatalf("want ErrStoreClosed, got %v", err)
		}
	})
	t.Run("DrainAllUploads", func(t *testing.T) {
		if err := bs.DrainAllUploads(ctx); !errors.Is(err, ErrStoreClosed) {
			t.Fatalf("want ErrStoreClosed, got %v", err)
		}
	})
	t.Run("DrainRollups", func(t *testing.T) {
		if err := bs.DrainRollups(ctx); !errors.Is(err, ErrStoreClosed) {
			t.Fatalf("want ErrStoreClosed, got %v", err)
		}
	})
	t.Run("ResetLocalState", func(t *testing.T) {
		if err := bs.ResetLocalState(ctx); !errors.Is(err, ErrStoreClosed) {
			t.Fatalf("want ErrStoreClosed, got %v", err)
		}
	})
	t.Run("EvictLocal", func(t *testing.T) {
		if err := bs.EvictLocal(ctx, "p"); !errors.Is(err, ErrStoreClosed) {
			t.Fatalf("want ErrStoreClosed, got %v", err)
		}
	})
}

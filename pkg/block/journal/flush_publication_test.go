package journal

import (
	"context"
	"errors"
	"testing"
)

func TestHasDirtyIncludesManifestPublication(t *testing.T) {
	for _, outcome := range []string{"success", "error", "cancel", "panic"} {
		t.Run(outcome, func(t *testing.T) {
			s := testStore(t, Config{ShardCount: 1})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := s.WriteAt(ctx, "f", 0, []byte("new bytes")); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("publication interrupted")
			var err error
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				err = s.Flush(ctx, "f", FlushOptions{Force: true, AfterFile: func(ctx context.Context, id FileID) error {
					if s.UnsyncedBytes() == 0 {
						t.Fatal("records became evictable before AfterFile succeeded")
					}
					if dirty, err := s.HasDirty(ctx, id); err != nil || !dirty {
						t.Fatalf("pending publication reported dirty=%v err=%v", dirty, err)
					}
					if dirty, err := s.HasDirty(ctx, "unrelated"); err != nil || dirty {
						t.Fatalf("another file on the shard reported dirty=%v err=%v", dirty, err)
					}
					switch outcome {
					case "error":
						return failure
					case "panic":
						panic(failure)
					}
					return nil
				}}, func(ctx context.Context, run Run) ([]Extent, error) {
					if outcome == "cancel" {
						cancel()
						return []Extent{run.Extent}, ctx.Err()
					}
					return []Extent{run.Extent}, nil
				})
			}()
			switch outcome {
			case "success":
				if err != nil || recovered != nil {
					t.Fatalf("Flush: err=%v panic=%v", err, recovered)
				}
			case "error":
				if !errors.Is(err, failure) {
					t.Fatalf("Flush: got %v, want %v", err, failure)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Flush: got %v, want cancellation", err)
				}
			case "panic":
				if recovered != failure {
					t.Fatalf("Flush panic: got %v, want %v", recovered, failure)
				}
			}
			wantDirty := outcome == "error" || outcome == "panic"
			if dirty, err := s.HasDirty(context.Background(), "f"); err != nil || dirty != wantDirty {
				t.Fatalf("publication completion: dirty=%v want=%v err=%v", dirty, wantDirty, err)
			}
			if wantDirty {
				// Retry with a fresh callback. An obsolete failed callback must
				// not survive to run against a later manifest replacement.
				var offered bool
				err = s.Flush(context.Background(), "f", FlushOptions{Force: true}, func(_ context.Context, run Run) ([]Extent, error) {
					offered = true
					return []Extent{run.Extent}, nil
				})
				if err != nil || !offered {
					t.Fatalf("publication retry did not offer current bytes: offered=%v err=%v", offered, err)
				}
				if dirty, err := s.HasDirty(context.Background(), "f"); err != nil || dirty {
					t.Fatalf("successful retry stayed dirty: dirty=%v err=%v", dirty, err)
				}
			}
		})
	}
}

func TestFlushMultiRunPrefixWaitsForSuccessfulPublication(t *testing.T) {
	for _, failPublication := range []bool{false, true} {
		name := "success"
		if failPublication {
			name = "error"
		}
		t.Run(name, func(t *testing.T) {
			s, _ := seamStore(t, Config{CarveBlockSize: 32 << 10})
			ctx := context.Background()
			writeRunAt(t, s, 0, 1)
			writeRunAt(t, s, 8192, 2)
			writeErr, reapErr := errors.New("later run failed"), errors.New("publication failed")
			calls := 0
			err := s.Flush(ctx, "f", FlushOptions{Force: true, AfterFile: func(context.Context, FileID) error {
				if calls != 2 || s.UnsyncedBytes() != 3*4096 {
					t.Fatalf("credit escaped before publication: calls=%d dirty=%d", calls, s.UnsyncedBytes())
				}
				if failPublication {
					return reapErr
				}
				return nil
			}}, func(_ context.Context, run Run) ([]Extent, error) {
				calls++
				if calls == 1 {
					return []Extent{run.Extent}, nil
				}
				return []Extent{{Off: run.Extent.Off, Len: 4096}}, writeErr
			})
			if !errors.Is(err, writeErr) || (failPublication && !errors.Is(err, reapErr)) {
				t.Fatalf("flush discarded a failure: %v", err)
			}
			wantDirty := int64(4096)
			if failPublication {
				wantDirty = 3 * 4096
			}
			if got := s.UnsyncedBytes(); got != wantDirty {
				t.Fatalf("dirty bytes after publication=%d, want %d", got, wantDirty)
			}
		})
	}
}

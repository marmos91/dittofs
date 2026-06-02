package blockstore

import (
	"context"
	"testing"

	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// TestRunWorkload_AllWorkloads exercises every workload dispatcher
// branch with a tiny Ops count so the CLI codepath stays covered by
// `go test` (not just `go test -bench`).
func TestRunWorkload_AllWorkloads(t *testing.T) {
	cases := []struct {
		name      string
		workload  string
		blockSize int
	}{
		{"sequential-write", WorkloadSequentialWrite, 4 * 1024},
		{"random-write", WorkloadRandomWrite, DefaultRandomBlockSize},
		{"dedup-heavy", WorkloadDedupHeavy, 4 * 1024},
		{"mixed-rw", WorkloadMixedRW, DefaultRandomBlockSize},
		{"flush-churn", WorkloadFlushChurn, DefaultRandomBlockSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bs, closeFn, err := NewEngine(t.TempDir(), remotememory.New())
			if err != nil {
				t.Fatalf("NewEngine: %v", err)
			}
			defer closeFn()

			res, err := RunWorkload(context.Background(), bs, Opts{
				Workload:   tc.workload,
				Ops:        4,
				BlockSize:  tc.blockSize,
				WorkingSet: 1,
				Seed:       1,
			})
			if err != nil {
				t.Fatalf("RunWorkload: %v", err)
			}
			if res.Ops != 4 {
				t.Errorf("Ops = %d, want 4", res.Ops)
			}
			if res.Duration <= 0 {
				t.Errorf("Duration = %v, want > 0", res.Duration)
			}
		})
	}
}

func TestRunWorkload_UnknownWorkload(t *testing.T) {
	bs, closeFn, err := NewEngine(t.TempDir(), remotememory.New())
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer closeFn()
	if _, err := RunWorkload(context.Background(), bs, Opts{
		Workload:  "bogus",
		Ops:       1,
		BlockSize: 4096,
	}); err == nil {
		t.Fatal("expected error for unknown workload")
	}
}

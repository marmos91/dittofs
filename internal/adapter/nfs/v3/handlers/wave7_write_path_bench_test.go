package handlers_test

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
)

// BenchmarkWave7WritePath_CreateWrite runs the per-operation write
// sequence -- CREATE, then a FILE_SYNC WRITE -- through the v3 handlers over a
// share whose journal is a real directory, so the block-journal fsync happens
// rather than being stubbed away.
//
// It exists to carry an execution trace, not to publish an ops/sec number.
// Run it under -trace and read the blocking profiles:
//
//	go test ./internal/adapter/nfs/v3/handlers/ -run xxx \
//	    -bench Wave7WritePath -benchtime 200x -trace /tmp/w7.trace
//	go tool trace -pprof=sync    /tmp/w7.trace > /tmp/w7.sync.pprof
//	go tool trace -pprof=syscall /tmp/w7.trace > /tmp/w7.syscall.pprof
//	go tool pprof -top -nodecount=25 /tmp/w7.sync.pprof
//
// The question it is pointed at is whether the metadata size/mtime commit and
// the journal fsync serialize. It cannot speak to the third suspect, the NFS
// WRITE/COMMIT round-trip: there is no client here and no network, so the
// round-trip does not exist in this process. Treat a result from this
// benchmark as covering two of the three legs and say which two.
//
// A wall-clock number taken from this on a laptop is not evidence about the
// rig -- fsync cost differs by an order of magnitude between an SSD here and
// the bench volume there. Ordering and blocking attribution are what transfer.
func BenchmarkWave7WritePath_CreateWrite(b *testing.B) {
	// UNSTABLE returns as soon as the record is appended; DATA_SYNC and
	// FILE_SYNC additionally run FlushStableWrite, which commits the block
	// store and (for FILE_SYNC) flushes the metadata. Comparing the three is
	// the probe that the durability path is actually reached: if the two sync
	// levels cost no more blocked-syscall time than UNSTABLE, nothing on this
	// path is fsyncing and a profile of it is measuring the wrong thing.
	for _, tc := range []struct {
		name   string
		stable uint32
	}{
		{"unstable", 0},
		{"datasync", 1},
		{"filesync", 2},
	} {
		b.Run(tc.name, func(b *testing.B) {
			fx := handlertesting.NewHandlerFixture(b)
			data := make([]byte, 4096)
			ctx := fx.ContextWithUID(0, 0)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				handle := fx.CreateFile(fmt.Sprintf("w7-%d.bin", i), []byte{})
				resp, err := fx.Handler.Write(ctx, &handlers.WriteRequest{
					Handle: handle,
					Offset: 0,
					Count:  uint32(len(data)),
					Stable: tc.stable,
					Data:   data,
				})
				if err != nil {
					b.Fatalf("WRITE: %v", err)
				}
				if resp.Status != types.NFS3OK {
					b.Fatalf("WRITE status = %d, want NFS3OK", resp.Status)
				}
			}
		})
	}
}

// BenchmarkWave7WritePath_FileSyncParallel answers the overlap question the
// serial benchmark raises: a FILE_SYNC write costs ~130x an UNSTABLE one, and
// almost none of that is this goroutine's own blocked syscall time -- it is
// spent waiting for the journal shard's group-commit leader to fsync on its
// behalf.
//
// If that batching works, adding concurrent writers amortises one fsync over
// many operations and per-op cost falls as -cpu rises. If instead the fsyncs
// serialise, per-op cost stays flat and total throughput is pinned to one
// fsync per operation no matter how many writers there are.
//
//	go test ./internal/adapter/nfs/v3/handlers/ -run xxx \
//	    -bench Wave7WritePath_FileSyncParallel -benchtime 200x -cpu 1,2,4,8
//
// Read the ns/op trend across -cpu, not any single value.
func BenchmarkWave7WritePath_FileSyncParallel(b *testing.B) {
	fx := handlertesting.NewHandlerFixture(b)
	data := make([]byte, 4096)
	ctx := fx.ContextWithUID(0, 0)
	var seq atomic.Uint64

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			handle := fx.CreateFile(fmt.Sprintf("w7p-%d.bin", seq.Add(1)), []byte{})
			resp, err := fx.Handler.Write(ctx, &handlers.WriteRequest{
				Handle: handle,
				Offset: 0,
				Count:  uint32(len(data)),
				Stable: 2, // FILE_SYNC
				Data:   data,
			})
			if err != nil {
				b.Fatalf("WRITE: %v", err)
			}
			if resp.Status != types.NFS3OK {
				b.Fatalf("WRITE status = %d, want NFS3OK", resp.Status)
			}
		}
	})
}

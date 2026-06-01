package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// BenchmarkEncodeCompoundResponse measures allocations of the COMPOUND reply
// encoder. The buffer is presized to the exact wire length so a typical reply
// allocates once instead of growing repeatedly.
func BenchmarkEncodeCompoundResponse(b *testing.B) {
	tag := []byte("compound-tag")
	// A representative compound reply: SEQUENCE + PUTFH + a few ops each
	// carrying a small result payload (status + op-specific data).
	results := make([]types.CompoundResult, 8)
	for i := range results {
		data := make([]byte, 64)
		results[i] = types.CompoundResult{
			Status: types.NFS4_OK,
			OpCode: uint32(i + 1),
			Data:   data,
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := encodeCompoundResponse(types.NFS4_OK, tag, results)
		if err != nil {
			b.Fatal(err)
		}
		if len(out) == 0 {
			b.Fatal("empty encode")
		}
	}
}

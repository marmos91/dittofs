package smb

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/pool"
	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// benchRawMessage models the read-loop per-request raw-message reconstruct
// (header + body) exercised in Connection.handleRequests. It compares the
// pooled path against a fresh allocation per request.
func benchRawMessage(b *testing.B, bodyLen int, pooled bool) {
	b.Helper()
	hdr := &header.SMB2Header{Command: types.CommandWrite, MessageID: 42}
	body := make([]byte, bodyLen)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var raw []byte
		if pooled {
			raw = pool.Get(header.HeaderSize + len(body))
		} else {
			raw = make([]byte, header.HeaderSize+len(body))
		}
		copy(raw, hdr.Encode())
		copy(raw[header.HeaderSize:], body)
		if pooled {
			pool.Put(raw)
		}
	}
}

func BenchmarkRawMessageUnpooled(b *testing.B) {
	b.Run("512B", func(b *testing.B) { benchRawMessage(b, 512, false) })
	b.Run("64KB", func(b *testing.B) { benchRawMessage(b, 64<<10, false) })
}

func BenchmarkRawMessagePooled(b *testing.B) {
	b.Run("512B", func(b *testing.B) { benchRawMessage(b, 512, true) })
	b.Run("64KB", func(b *testing.B) { benchRawMessage(b, 64<<10, true) })
}

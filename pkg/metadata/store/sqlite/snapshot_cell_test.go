package sqlite

import (
	"bytes"
	"encoding/binary"
	"runtime"
	"testing"
)

// A corrupt cell length must not be trusted for the allocation: decoding a
// truncated stream fails without reserving the declared size.
func TestReadCell_TruncatedLengthDoesNotPreallocate(t *testing.T) {
	for _, kind := range []byte{cellText, cellBlob} {
		var stream bytes.Buffer
		stream.WriteByte(kind)
		var lenBuf [4]byte
		binary.LittleEndian.PutUint32(lenBuf[:], 1<<30)
		stream.Write(lenBuf[:])
		stream.WriteString("short")

		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		if _, err := readCell(bytes.NewReader(stream.Bytes())); err == nil {
			t.Fatalf("kind %d: expected an error on a truncated cell", kind)
		}
		runtime.ReadMemStats(&after)

		if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
			t.Fatalf("kind %d: decoder allocated %d bytes from the declared length", kind, allocated)
		}
	}
}

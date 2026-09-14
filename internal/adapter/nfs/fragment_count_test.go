package nfs

import (
	"io"
	"testing"
	"time"
)

// zeroFragments is an endless stream of not-last, zero-length fragment
// headers: the four bytes 0x00000000 repeated forever.
type zeroFragments struct{ reads int }

func (z *zeroFragments) Read(p []byte) (int, error) {
	z.reads++
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// TestReadRPCRecord_BoundsFragmentCount pins the second half of the record
// bound. The cumulative byte check only advances when a fragment carries
// payload, so a peer that sends nothing but zero-length continuation headers
// never grows the record, never trips the size limit and never sets the
// last-fragment flag. Without a separate cap on how many fragments one record
// may span, that peer holds the connection and its goroutine forever on four
// bytes at a time, before it has authenticated anything.
func TestReadRPCRecord_BoundsFragmentCount(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		_, err := ReadRPCRecord(&zeroFragments{}, &FragmentHeader{Length: 0, IsLast: false}, "10.0.0.1:1")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an endless run of zero-length fragments was accepted as a record")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadRPCRecord did not return: the fragment count is unbounded")
	}
}

// TestReadRPCRecord_MultiFragmentStillReassembles guards the bound against
// being set so low that a legitimate multi-fragment record is refused.
func TestReadRPCRecord_MultiFragmentStillReassembles(t *testing.T) {
	var stream []byte
	payload := []byte("abcd")
	for i := 0; i < 8; i++ {
		stream = append(stream, 0x00, 0x00, 0x00, 0x04)
		stream = append(stream, payload...)
	}
	stream = append(stream, 0x80, 0x00, 0x00, 0x04)
	stream = append(stream, payload...)

	got, err := ReadRPCRecord(newByteReader(stream), &FragmentHeader{Length: 0, IsLast: false}, "10.0.0.1:1")
	if err != nil {
		t.Fatalf("legitimate 9-fragment record refused: %v", err)
	}
	if len(got) != 9*len(payload) {
		t.Fatalf("reassembled %d bytes, want %d", len(got), 9*len(payload))
	}
}

func newByteReader(b []byte) io.Reader { return &sliceReader{b: b} }

type sliceReader struct{ b []byte }

func (s *sliceReader) Read(p []byte) (int, error) {
	if len(s.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.b)
	s.b = s.b[n:]
	return n, nil
}

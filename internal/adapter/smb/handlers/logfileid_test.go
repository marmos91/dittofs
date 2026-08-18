package handlers

import (
	"fmt"
	"testing"
)

// TestLazyFileIDMatchesEager guards that the deferred rendering is byte-for-byte
// what the eager fmt.Sprintf at the call sites produced.
func TestLazyFileIDMatchesEager(t *testing.T) {
	ids := [][16]byte{
		{},
		{0x01},
		{0xde, 0xad, 0xbe, 0xef, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff},
	}
	for _, id := range ids {
		want := fmt.Sprintf("%x", id)
		if got := lazyFileID(id).LogValue().String(); got != want {
			t.Errorf("LogValue() = %q, want %q", got, want)
		}
	}
}

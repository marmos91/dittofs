package xdr

import (
	"fmt"
	"testing"
)

// TestLazyHandleMatchesEager guards that the deferred rendering is byte-for-byte
// what the eager fmt.Sprintf at the call sites produced.
func TestLazyHandleMatchesEager(t *testing.T) {
	for _, h := range [][]byte{nil, {}, {0x00}, {0xde, 0xad, 0xbe, 0xef}, []byte("share:file")} {
		want := fmt.Sprintf("0x%x", h)
		if got := LazyHandle(h).String(); got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
		if got := LazyHandle(h).LogValue().String(); got != want {
			t.Errorf("LogValue() = %q, want %q", got, want)
		}
	}
}

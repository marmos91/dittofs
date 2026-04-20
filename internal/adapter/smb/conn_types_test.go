package smb

import "testing"

// TestTryReserveAsync_CapsAtMaxMinusOne verifies the per-connection async slot
// cap matches Samba behavior: outstanding ops max at MaxAsyncCredits-1 (511 when
// MaxAsyncCredits=512). Smbtorture smb2.credits.*_max_async_credits asserts
// num_status_pending == max_async_credits - 1 (credits.c:641, :1281).
func TestTryReserveAsync_CapsAtMaxMinusOne(t *testing.T) {
	c := &ConnInfo{}

	for i := range MaxAsyncCredits - 1 {
		if !c.TryReserveAsync() {
			t.Fatalf("TryReserveAsync returned false at i=%d; want true for all i<MaxAsyncCredits-1", i)
		}
	}

	if c.TryReserveAsync() {
		t.Fatalf("TryReserveAsync returned true at cap; want false (outstanding=%d)", c.asyncPendingCount.Load())
	}
	if got := c.asyncPendingCount.Load(); got != MaxAsyncCredits-1 {
		t.Fatalf("asyncPendingCount=%d, want %d", got, MaxAsyncCredits-1)
	}

	c.ReleaseAsync()
	if !c.TryReserveAsync() {
		t.Fatalf("TryReserveAsync returned false after ReleaseAsync; want true")
	}
}

package xdr

import (
	"log/slog"
	"testing"
)

// BenchmarkClientIP compares the old eager per-op ExtractClientIP (which runs
// net.SplitHostPort on every NFS op regardless of log level) against building
// a LazyClientIP whose SplitHostPort is deferred to log-emit time. "lazy_dropped"
// models the common case: the log line is filtered out, so LogValue is never
// called and the split never runs.
func BenchmarkClientIP(b *testing.B) {
	const addr = "192.168.1.100:2049"

	b.Run("eager", func(b *testing.B) {
		b.ReportAllocs()
		var sink string
		for i := 0; i < b.N; i++ {
			sink = ExtractClientIP(addr)
		}
		_ = sink
	})
	b.Run("lazy_dropped", func(b *testing.B) {
		b.ReportAllocs()
		var sink LazyClientIP
		for i := 0; i < b.N; i++ {
			sink = LazyClientIP(addr) // built per op; String/LogValue never called
		}
		_ = sink
	})
	b.Run("lazy_emitted", func(b *testing.B) {
		b.ReportAllocs()
		var sink slog.Value
		for i := 0; i < b.N; i++ {
			sink = LazyClientIP(addr).LogValue() // cost paid only when actually logged
		}
		_ = sink
	})
}

// TestLazyClientIPMatchesEager guards that the lazy paths return exactly what
// the eager helper does, across host:port, bare-host, and empty inputs.
func TestLazyClientIPMatchesEager(t *testing.T) {
	for _, addr := range []string{"192.168.1.100:2049", "10.0.0.1", "", "[::1]:2049"} {
		want := ExtractClientIP(addr)
		if got := LazyClientIP(addr).String(); got != want {
			t.Errorf("String(%q) = %q, want %q", addr, got, want)
		}
		if got := LazyClientIP(addr).LogValue().String(); got != want {
			t.Errorf("LogValue(%q) = %q, want %q", addr, got, want)
		}
	}
}

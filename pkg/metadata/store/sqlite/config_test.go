package sqlite

import (
	"strings"
	"testing"
	"time"
)

// TestDSNPragmas locks in the per-connection pragmas. synchronous(NORMAL) in
// particular is load-bearing for write-heavy flushes (issue #1667): without it
// modernc.org/sqlite defaults to FULL and fsyncs every commit.
func TestDSNPragmas(t *testing.T) {
	c := &SQLiteMetadataStoreConfig{Path: "/tmp/x.db", BusyTimeout: 5 * time.Second}
	dsn := c.DSN()
	for _, want := range []string{
		"_pragma=foreign_keys(1)",
		"_pragma=busy_timeout(5000)",
		"_pragma=journal_mode(WAL)",
		"_pragma=synchronous(NORMAL)",
	} {
		if !strings.Contains(dsn, want) {
			t.Errorf("DSN missing %q\n got: %s", want, dsn)
		}
	}
}

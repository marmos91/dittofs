package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The quarantine list is read and checked as the registry loads, and its
// entries kept by suite. An entry names a suite and keeps the list's own
// rules. It expires at the start of its date in UTC, at most 90 days ahead,
// so it can't stay forever: from then on it excuses nothing, and the registry
// says so, for dt doctor, but still loads. Here it is 00:30 on 23 October
// where the clock is, two hours ahead, so 22 October in UTC.
func TestQuarantine(t *testing.T) {
	now = func() time.Time { return time.Date(2026, 10, 23, 0, 30, 0, 0, time.FixedZone("UTC+2", 2*60*60)) }
	t.Cleanup(func() { now = time.Now })
	const y = "version: 1\ndefaults: { format: exit, tier: merge, timeout: { test: 30m, cell: 60m } }\nsuites:\n  s: { cmd: run }\n"
	for _, c := range []struct {
		name, expires, suite string
		kept                 bool   // the entry grades s's results
		expired, err         string // what r.Expired, or the load's error, holds
	}{
		{"an entry holds until its date comes in UTC", "2026-10-23", "s", true, "", ""},
		{"an entry expires at the start of its date, and excuses nothing", "2026-10-22", "s", false,
			"test/harness/quarantine.yaml:1: t expired on 2026-10-22", ""},
		{"an entry names a suite", "2026-10-23", "nope", false, "", `test/harness/quarantine.yaml:1: no suite "nope"`},
		{"an entry lasts up to 90 days from today in UTC", "2027-01-20", "s", true, "", ""},
		{"and no longer", "2027-01-21", "s", false, "", "test/harness/quarantine.yaml:1: expires 2027-01-21 is more than 90 days away"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, err := Parse(quarantined(t, "- { suite: "+c.suite+", test: t, issue: '#1', expires: "+c.expires+" }\n"), []byte(y))
			if err != nil || c.err != "" {
				if err == nil || c.err == "" || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("got %v, want an error containing %q", err, c.err)
				}
				return
			}
			expired := strings.Join(r.Expired, "\n")
			if kept := len(r.Quarantine["s"]) == 1; kept != c.kept || (c.expired == "") != (expired == "") || !strings.Contains(expired, c.expired) {
				t.Errorf("kept %v, expired %q; want kept %v, expired %q", kept, expired, c.kept, c.expired)
			}
		})
	}
	if _, err := Parse(quarantined(t, "- { suite: s, test: t, expires: 2026-10-23 }\n"), []byte(y)); err == nil || !strings.Contains(err.Error(), "test/harness/quarantine.yaml:1: issue is required") {
		t.Errorf("the list's own rules: got %v", err)
	}
}

// quarantined returns a repository root whose quarantine list is list.
func quarantined(t *testing.T, list string) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, QuarantinePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(list), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

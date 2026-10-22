package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The quarantine list is read and checked as the registry loads, and its
// entries kept by suite. An entry names a suite, keeps the list's own rules,
// and expires at the start of its date in UTC: from then on it's a registry
// error, so the list can't keep a flake forever. Here it is 00:30 on 23
// October where the clock is, two hours ahead, so 22 October in UTC.
func TestQuarantine(t *testing.T) {
	now = func() time.Time { return time.Date(2026, 10, 23, 0, 30, 0, 0, time.FixedZone("UTC+2", 2*60*60)) }
	t.Cleanup(func() { now = time.Now })
	const y = "version: 1\ndefaults: { format: exit, tier: merge, timeout: { test: 30m, cell: 60m } }\nsuites:\n  s: { cmd: run }\n"
	for _, c := range []struct {
		name, list, want string
	}{
		{"an entry holds until its date comes in UTC", "- { suite: s, test: t, issue: '#1', expires: 2026-10-23 }\n", ""},
		{"an entry expires at the start of its date", "- { suite: s, test: t, issue: '#1', expires: 2026-10-22 }\n",
			"test/harness/quarantine.yaml:1: t expired on 2026-10-22"},
		{"an entry names a suite", "- { suite: nope, test: t, issue: '#1', expires: 2026-10-23 }\n", `test/harness/quarantine.yaml:1: no suite "nope"`},
		{"the list's own rules apply", "- { suite: s, test: t, expires: 2026-10-23 }\n", "test/harness/quarantine.yaml:1: issue is required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, QuarantinePath)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(c.list), 0o644); err != nil {
				t.Fatal(err)
			}
			r, err := Parse(root, []byte(y))
			switch {
			case c.want == "" && (err != nil || len(r.Quarantine["s"]) != 1 || r.Quarantine["s"][0].Test != "t"):
				t.Fatalf("got %v, want the entry kept for s", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("got %v, want an error containing %q", err, c.want)
			}
		})
	}
}

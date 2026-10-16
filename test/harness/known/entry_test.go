package known

import (
	"strings"
	"testing"
)

// entry is a valid entry as one line of YAML; each case changes one field.
const entry = "- { test: smb2.lock.rw, status: fail, fix: wontfix, category: proto, reason: no byte-range locks }\n"

// Each rule a known file is read with refuses what it should, at the
// entry's line, and every problem is reported, not only the first.
func TestParseRules(t *testing.T) {
	for _, c := range []struct {
		name, yaml, want string
	}{
		{"a list of entries", "test: a\n", "k.yaml:1: expected a list of entries"},
		{"an entry is a mapping", "- a\n", "k.yaml:1: an entry is a mapping"},
		{"unknown fields are refused", strings.Replace(entry, "reason:", "owner: me, reason:", 1), `k.yaml:1: unknown field "owner"`},
		{"a missing fix is an error", strings.Replace(entry, "fix: wontfix, ", "", 1), "k.yaml:1: fix is required: planned or wontfix"},
		{"fix is planned or wontfix", strings.Replace(entry, "wontfix", "later", 1), `fix "later" is not planned or wontfix`},
		{"a planned entry names its issue", strings.Replace(entry, "wontfix", "planned", 1), `a planned entry names its issue`},
		{"an issue is #N", strings.Replace(entry, "fix: wontfix", "fix: planned, issue: '3136'", 1), `issue "3136" is not written as "#1234"`},
		{"status is fail or skip", strings.Replace(entry, "status: fail", "status: flaky", 1), `status "flaky" is not fail or skip`},
		{"category is one of five", strings.Replace(entry, "proto", "suite", 1), `category "suite" is not bug or feature or env or proto or permanent`},
		{"every entry has a reason", strings.Replace(entry, "reason: no byte-range locks", "reason: ' '", 1), "reason is required"},
		{"every entry names its test", strings.Replace(entry, "test: smb2.lock.rw, ", "", 1), "test is required"},
		{"where lists values", strings.Replace(entry, "reason:", "where: { nfs: [] }, reason:", 1), "where nfs lists no value"},
		{"each problem at its entry's line", entry + strings.Replace(entry, "fix: wontfix, ", "", 1) + strings.Replace(entry, "proto", "", 1),
			"k.yaml:2: fix is required: planned or wontfix\nk.yaml:3: category is required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse("k.yaml", []byte(c.yaml))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error containing %q", err, c.want)
			}
		})
	}
}

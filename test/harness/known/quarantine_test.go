package known

import (
	"slices"
	"strings"
	"testing"
)

// quarantined is a valid quarantine entry as one line of YAML, its date
// unquoted; each case changes one field.
const quarantined = "- { suite: s, test: a, issue: '#12', expires: 2026-12-31 }\n"

// Each rule the quarantine list is read with refuses what it should, at the
// entry's line, and every problem is reported, not only the first. A valid
// entry is read whole.
func TestParseQuarantineRules(t *testing.T) {
	for _, c := range []struct {
		name, yaml, want string
	}{
		{"an entry is a mapping", "- a\n", "q.yaml:1: an entry is a mapping"},
		{"unknown fields are refused", strings.Replace(quarantined, "expires:", "reason: r, expires:", 1), `q.yaml:1: unknown field "reason"`},
		{"every field is required", "- { test: a }\n", "q.yaml:1: suite is required; issue is required; expires is required"},
		{"a test is named", strings.Replace(quarantined, "test: a", "test: ' '", 1), "q.yaml:1: test is required"},
		{"an issue is #N", strings.Replace(quarantined, "'#12'", "'12'", 1), `issue "12" is not written as "#1234"`},
		{"expires is a date", strings.Replace(quarantined, "2026-12-31", "2026-12-31T00:00:00Z", 1), `expires "2026-12-31T00:00:00Z" is not a date`},
		{"each problem at its entry's line", quarantined + strings.Replace(quarantined, "suite: s, ", "", 1),
			"q.yaml:2: suite is required"},
		{"a test is one name, not a glob", strings.Replace(quarantined, "test: a", "test: 'smb2.lock*'", 1), `q.yaml:1: test "smb2.lock*" is a pattern`},
		{"nor a whole suite", strings.Replace(quarantined, "test: a", "test: '*'", 1), `test "*" is a pattern`},
		{"nor a regex-like .*", strings.Replace(quarantined, "test: a", "test: smb2.lock.*", 1), `test "smb2.lock.*" is a pattern`},
		{"nor a ?", strings.Replace(quarantined, "test: a", "test: 'chmod/0?.t'", 1), `test "chmod/0?.t" is a pattern`},
		{"nor a set", strings.Replace(quarantined, "test: a", "test: 'chmod/0[0-9].t'", 1), `test "chmod/0[0-9].t" is a pattern`},
		{"a test is listed once per suite", quarantined + strings.Replace(quarantined, "2026-12-31", "2026-12-01", 1), "q.yaml:2: s a is listed twice, first at line 1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseQuarantine("q.yaml", []byte(c.yaml))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error containing %q", err, c.want)
			}
		})
	}
	got, err := ParseQuarantine("q.yaml", []byte("# a comment\n"+quarantined))
	if err != nil || len(got) != 1 || got[0] != (Quarantine{Suite: "s", Test: "a", Issue: "#12", Expires: "2026-12-31", Line: 2}) {
		t.Errorf("got %+v, %v", got, err)
	}
}

// A quarantine entry grades a failure of its test quarantined, with a reason
// that says so and names its issue, and its parent through it, as a known
// failure would. Its pass passes, graded quarantined too, so the results say
// whether the test still fails. It grades nothing else: a crash or a hang
// stays new, and a skip is graded as any skip.
func TestGradeQuarantined(t *testing.T) {
	for _, c := range []struct {
		name    string
		results []string
		want    []string
	}{
		{"a failure is quarantined, with a reason naming its entry's issue", []string{"a failed", "b failed"},
			[]string{"a: skipped (failed) [quarantined] quarantined as flaky, see #12 #12", "b: failed [new]"}},
		{"a pass is graded quarantined too, and passes", []string{"a passed"}, []string{"a: passed [quarantined] quarantined as flaky, see #12 #12"}},
		{"a parent that failed only through it is known", []string{"TestP/a failed", "TestP failed"},
			[]string{"TestP/a: skipped (failed) [quarantined] quarantined as flaky, see #12 #12", "TestP: skipped (failed) [known] " + throughSubtests}},
		{"a crash or a hang stays new", []string{"a failed new"}, []string{"a: failed [new]"}},
		{"a skip or a stopped result is graded as any", []string{"a skipped", "a other"},
			[]string{"a: other (skipped) [unexpected_skip]", "a: other [incomplete]"}},
		{"an entry names its test alone, not its subtests or a .t file", []string{"a/b failed", "a.t failed"},
			[]string{"a/b: failed [new]", "a.t: failed [new]"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			list, err := ParseQuarantine("q.yaml", []byte("- { suite: s, test: a, issue: '#12', expires: 2026-12-31 }\n- { suite: s, test: TestP/a, issue: '#12', expires: 2026-12-31 }\n"))
			if err != nil {
				t.Fatal(err)
			}
			var entries []Entry
			for _, q := range list {
				entries = append(entries, q.Entry())
			}
			tests := results(c.results...)
			Grade(tests, true, entries, nil)
			if got := shown(tests); !slices.Equal(got, c.want) {
				t.Errorf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

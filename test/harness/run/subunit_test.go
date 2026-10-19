package run

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/test/harness/known"
	"github.com/marmos91/dittofs/test/harness/report"
)

// subunitKnown is the known-failure list of parse-results_test.sh: a name, a
// pattern, and a name smbtorture reports several results under.
const subunitKnown = `- { test: smb2.lock.lock1, status: fail, fix: wontfix, category: proto, reason: expected }
- { test: smb2.oplock.*, status: fail, fix: wontfix, category: proto, reason: wildcard row }
- { test: smb2.charset.Testing, status: fail, fix: wontfix, category: proto, reason: unpaired surrogate }
`

// The subunit reader keeps the rules of smbtorture's grader,
// parse-results.sh, case by case from its test, each result graded against
// the same list: a name with its grade, or its status when nothing graded it.
// Where dt grades differently, as the RFC says, the case says how: a test with
// no result is incomplete, a timed-out entry fails the test it had open
// whatever the list says, a known failure that passes fails, and no timeout is
// signed off. The grader's cases on its timeouts.txt rows and on its one-byte
// exit status have no counterpart in dt.
func TestReadSubunit(t *testing.T) {
	entries, err := known.Parse("known.yaml", []byte(subunitKnown))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		prefix   string
		timedOut bool
		exit     int
		output   string
		want     []string
	}{
		{name: "known failures only, by name and by pattern", output: `test: smb2.connect.connect1
success: smb2.connect.connect1
test: smb2.lock.lock1
failure: smb2.lock.lock1 [ expected ]
test: smb2.oplock.batch1
failure: smb2.oplock.batch1 [ wildcard ]
`, want: []string{"smb2.connect.connect1 passed", "smb2.lock.lock1 known", "smb2.oplock.batch1 known"}},
		{name: "a failure off the list is new, in any order", output: `test: smb2.rename.rename1
failure: smb2.rename.rename1 [ regression ]
test: smb2.oplock.batch1
failure: smb2.oplock.batch1 [ wildcard ]
test: smb2.lock.lock1
failure: smb2.lock.lock1 [ expected ]
test: smb2.connect.connect1
success: smb2.connect.connect1
`, want: []string{"smb2.rename.rename1 new", "smb2.oplock.batch1 known", "smb2.lock.lock1 known", "smb2.connect.connect1 passed"}},
		{name: "an unfinished test is incomplete, not a pass", output: `test: smb2.connect.connect1
success: smb2.connect.connect1
test: smb2.notify.mask
`, want: []string{"smb2.connect.connect1 passed", "smb2.notify.mask incomplete"}},
		{name: "a test the next one cut short is incomplete", output: `test: smb2.notify.dir
test: smb2.notify.mask
success: smb2.notify.mask
`, want: []string{"smb2.notify.dir incomplete", "smb2.notify.mask passed"}},
		{name: "a result under another name leaves the open test incomplete", output: `test: smb2.notify.dir
success: smb2.notify.mask
`, want: []string{"smb2.notify.mask passed", "smb2.notify.dir incomplete"}},
		{name: "a timeout fails the open test, which no entry excuses, and keeps the failures before it", timedOut: true, output: `test: smb2.oplock.batch1
failure: smb2.oplock.batch1 [ wildcard ]
test: smb2.rename.rename1
failure: smb2.rename.rename1 [ regression ]
test: smb2.oplock.batch2
`, want: []string{"smb2.oplock.batch1 known", "smb2.rename.rename1 new", "smb2.oplock.batch2 new"}},
		{name: "a client that crashed fails nothing by its exit", exit: 139, output: `test: smb2.scan.find
success: smb2.scan.find
test: smb2.scan.scan
`, want: []string{"smb2.scan.find passed", "smb2.scan.scan incomplete"}},
		{name: "a known failure that passes is unexpected", output: `test: smb2.lock.lock1
success: smb2.lock.lock1
test: smb2.oplock.batch1
failure: smb2.oplock.batch1 [ wildcard ]
`, want: []string{"smb2.lock.lock1 unexpected_pass", "smb2.oplock.batch1 known"}},
		{name: "a name that both passes and fails stays known", output: `test: smb2.lock.lock1
success: smb2.lock.lock1
test: smb2.lock.lock1
failure: smb2.lock.lock1 [ expected ]
`, want: []string{"smb2.lock.lock1 passed", "smb2.lock.lock1 known"}},
		{name: "the self-contradictory draw is an expected skip", output: `test: smb2.replay.channel-sequence
Testing setinfo (replay: true) with CSN 0x7fff, expecting: NT_STATUS_FILE_NOT_AVAILABLE
failure: smb2.replay.channel-sequence [
failed to test CSN with replay flag
]
`, want: []string{"smb2.replay.channel-sequence expected_skip"}},
		{name: "a channel-sequence failure without the draw is new", output: `test: smb2.replay.channel-sequence
Testing setinfo (replay: true) with CSN 0x7ffe, expecting: NT_STATUS_FILE_NOT_AVAILABLE
failure: smb2.replay.channel-sequence [
failed to test CSN with replay flag
]
`, want: []string{"smb2.replay.channel-sequence new"}},
		{name: "one draw excuses one failure", output: `test: smb2.replay.channel-sequence
Testing setinfo (replay: true) with CSN 0x7fff, expecting: NT_STATUS_FILE_NOT_AVAILABLE
failure: smb2.replay.channel-sequence [
failed to test CSN with replay flag
]
failure: smb2.replay.channel-sequence [
a second, unrelated failure
]
`, want: []string{"smb2.replay.channel-sequence expected_skip", "smb2.replay.channel-sequence new"}},
		{name: "the draw doesn't reach the next test", output: `test: smb2.replay.channel-sequence
Testing setinfo (replay: true) with CSN 0x7fff, expecting: NT_STATUS_FILE_NOT_AVAILABLE
success: smb2.replay.channel-sequence
test: smb2.replay.channel-sequence
failure: smb2.replay.channel-sequence [
failed to test CSN with replay flag
]
`, want: []string{"smb2.replay.channel-sequence passed", "smb2.replay.channel-sequence new"}},
		{name: "a NO_MEMORY assertion is a failure", output: `test: charset.Testing composite character (a umlaut)
success: charset.Testing composite character (a umlaut)
test: charset.Testing partial surrogate
failure: charset.Testing partial surrogate [
../../source4/torture/smb2/charset.c:174: status was NT_STATUS_NO_MEMORY, expected NT_STATUS_OK: Failed to create partial surrogate 1
]
test: charset.Testing wide-a
success: charset.Testing wide-a
`, want: []string{"smb2.charset.Testing passed", "smb2.charset.Testing known", "smb2.charset.Testing passed"}},
		{name: "an unlisted NO_MEMORY assertion is new", output: `test: smb2.rename.rename1
failure: smb2.rename.rename1 [
../../source4/torture/smb2/rename.c:174: status was NT_STATUS_NO_MEMORY, expected NT_STATUS_OK: whatever
]
`, want: []string{"smb2.rename.rename1 new"}},
		{name: "NO_MEMORY in the connection setup is incomplete", output: `test: smb2.rename.rename1
failure: smb2.rename.rename1 [
../../source4/torture/smb2/rename.c:41: status was NT_STATUS_NO_MEMORY, expected NT_STATUS_OK: smb2_connect failed
]
`, want: []string{"smb2.rename.rename1 incomplete"}},
		{name: "a failed connection setup is incomplete: a failure or an error, a time: line between, no brackets", output: `test: smb2.connect.connect1
success: smb2.connect.connect1
test: rename.rename1
failure: rename.rename1 [
../../source4/torture/smb2/smb2.c:95: Establishing SMB2 connection failed
]
test: smb2.app-instance.app-instance
error: smb2.app-instance.app-instance [
time: 2026-10-19 10:00:00
../../source4/torture/smb2/smb2.c:95: Establishing SMB2 connection failed
]
test: smb2.durable-open.open1
failure: smb2.durable-open.open1
NT_STATUS_NO_MEMORY
Unable to connect
`, want: []string{"smb2.connect.connect1 passed", "smb2.rename.rename1 incomplete", "smb2.app-instance.app-instance incomplete", "smb2.durable-open.open1 incomplete"}},
		{name: "a new failure beside an incomplete one", output: `test: smb2.rename.rename1
failure: smb2.rename.rename1 [ regression ]
test: smb2.app-instance.app-instance
failure: smb2.app-instance.app-instance [
../../source4/torture/smb2/smb2.c:95: Establishing SMB2 connection failed
]
`, want: []string{"smb2.rename.rename1 new", "smb2.app-instance.app-instance incomplete"}},
		{name: "prefix= names what smbtorture reports below the suite it ran", prefix: "oplock", output: `test: batch1
failure: batch1 [
oplock break not received
]
test: batch2
skip: batch2 [
needs kernel oplocks
]
`, want: []string{"smb2.oplock.batch1 known", "smb2.oplock.batch2 ungraded"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tests, err := readSubunit(report.Ran{Test: "smb2.x", Prefix: tc.prefix, Output: []byte(tc.output), Exit: tc.exit, TimedOut: tc.timedOut})
			if err != nil {
				t.Fatal(err)
			}
			known.Grade(tests, false, entries, nil)
			var got []string
			for _, r := range tests {
				got = append(got, r.Name+" "+cmp.Or(r.Grade, r.Status))
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

// A result's message is what follows its name: the rest of its line and its
// details up to the "]" line, without the brackets.
func TestSubunitMessages(t *testing.T) {
	tests, err := readSubunit(report.Ran{Output: []byte(`test: charset.Testing partial surrogate
failure: charset.Testing partial surrogate [
charset.c:174: status was NT_STATUS_NO_MEMORY
]
not a detail
test: smb2.lock.lock1
skip: smb2.lock.lock1 [ no locks ]
`)})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range tests {
		got = append(got, fmt.Sprintf("%s %s %q", r.Name, r.Status, r.Message))
	}
	want := []string{
		`smb2.charset.Testing failed "partial surrogate\ncharset.c:174: status was NT_STATUS_NO_MEMORY"`,
		`smb2.lock.lock1 skipped "no locks"`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("got  %s\nwant %s", strings.Join(got, "\n     "), strings.Join(want, "\n     "))
	}
}

// A run that reports no test, as smbtorture does for a name it doesn't know,
// is an error, whatever else it printed.
func TestSubunitNoResults(t *testing.T) {
	for _, output := range []string{"", "smbtorture 4.22.6\nUnknown torture operation 'smb2.notify.security'\n"} {
		if tests, err := readSubunit(report.Ran{Output: []byte(output), Exit: 1}); err == nil {
			t.Errorf("%q: no error, results %+v", output, tests)
		}
	}
}

// A subunit cell reads each entry of its .txt list with the entry's prefix=,
// and an entry that reports no test didn't run.
func TestSubunitCell(t *testing.T) {
	root, r := repo(t, `suites:
  s: { format: subunit, cmd: "cat {test}", tests: list.txt }
`, map[string]string{"list.txt": "a prefix=p\nb\n", "a": "test: x\nfailure: x [\nboom\n]\n", "b": "Unknown torture operation\n"})
	rep, out := runTarget(t, root, r, "s", false)
	if o := rep.Outcomes[0]; o.Result != report.Failed || o.Note != "1 failed: smb2.p.x; b: no results" {
		t.Errorf("outcome %+v\n%s", o, out)
	}
}

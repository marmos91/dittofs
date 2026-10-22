package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A cell's results, written as a run and read back, give the baseline file
// smbtorture's scripts wrote for the same smbtorture output: testdata/
// baseline.md is parse-results.sh --emit-baseline's output for it, given
// KNOWN_FAILURES.md rows for lock1, charset.Testing, oplock.batch* and
// rename.simple, with dt's header: its note, the cell for the profile, no
// dashes before the known and new counts, and no blank line at the end. The
// results hold each class the scripts count: known and new failures, an
// unexpected pass, expected and unexpected skips, the draw the reader
// excuses, a failure of the connection setup, which has no result, and one
// name with several results. Their sub-suites and names arrive unsorted,
// with upper and lower case, and the per-sub-suite rates hold a half that
// rounds to even, a two-thirds and suites with only skips. A cell that didn't
// run to its end, not run or cut short by a limit, and a run with changes
// write nothing. A quarantined failure counts as known: batch7, which the
// scripts' list holds, is quarantined here.
func TestBaseline(t *testing.T) {
	pass := func(name string) Test { return Test{Name: name, Status: Passed} }
	known := func(name string) Test { return Test{Name: name, Status: Skipped, RawStatus: Failed, Grade: Known} }
	fresh := func(name string) Test { return Test{Name: name, Status: Failed, Grade: New} }
	tests := []Test{
		pass("smb2.connect"),
		known("smb2.lock.lock1"),
		pass("smb2.lock.lock2"),
		fresh("smb2.lock.lock10"),
		{Name: "smb2.lock.async", Status: Other, RawStatus: Skipped, Grade: UnexpectedSkip},
		pass("smb2.charset.Testing"),
		known("smb2.charset.Testing"),
		pass("smb2.charset.Testing"),
		pass("smb2.charset.Testing"),
		pass("smb2.oplock.exclusive1"),
		{Name: "smb2.oplock.batch1", Status: Other, Grade: Incomplete},
		known("smb2.oplock.batch2"), known("smb2.oplock.batch3"), known("smb2.oplock.batch4"),
		known("smb2.oplock.batch5"), known("smb2.oplock.batch6"),
		{Name: "smb2.oplock.batch7", Status: Skipped, RawStatus: Failed, Grade: Quarantined},
		fresh("smb2.oplock.stream1"),
		{Name: "smb2.rename.simple", Status: Failed, RawStatus: Passed, Grade: UnexpectedPass},
		{Name: "smb2.replay.channel-sequence", Status: Skipped, RawStatus: Failed, Grade: ExpectedSkip},
		{Name: "smb2.multichannel.interface_info", Status: Skipped, Grade: ExpectedSkip},
		pass("smb2.compound_async.flush_close"),
		pass("smb2.compound.related1"),
		pass("smb2.acls.inheritance"),
		pass("smb2.acls.OWNER"),
		fresh("smb2.acls.GENERIC"),
	}
	dir := t.TempDir()
	run := Run{Name: "20261019T231500Z-d6223c90", Start: time.Date(2026, 10, 19, 23, 15, 0, 0, time.UTC), Commit: "d6223c90", OS: "linux",
		Cells: []Cell{
			{Name: "smbtorture/badger", Result: "not run", Note: "the fixture: dfs didn't start"},
			{Name: "smbtorture/badger-s3", Result: "failed", Note: "1 failed: smb2.lock.lock10; smb2.lock: timed out after 5m0s", Tests: tests[:4]},
			{Name: "smbtorture/memory", Result: "failed", Log: "logs/smbtorture-memory.log", Tests: tests,
				Note: "3 failed: smb2.lock.lock10, smb2.oplock.stream1, smb2.acls.GENERIC; 1 passed unexpectedly: smb2.rename.simple"},
		}}
	if err := run.Write(dir); err != nil {
		t.Fatal(err)
	}
	writeLog := func(text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "logs", "smbtorture-memory.log"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeLog("[dt] cell smbtorture/memory, tier nightly, commit d6223c90\n[dt] $ smbtorture //127.0.0.1/smbbasic\nsmbtorture 4.22.6\nUsing seed 1\n")
	got, err := Baseline(dir, "smbtorture/memory")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "baseline.md", got)

	writeLog("[dt] cell smbtorture/memory\n")
	if got, err := Baseline(dir, "smbtorture/memory"); err != nil || !strings.Contains(string(got), "\n**smbtorture:** unknown\n") {
		t.Errorf("a log without the version: %v\n%s", err, got)
	}
	for cell, want := range map[string]string{
		"smbtorture/kerberos":  "run 20261019T231500Z-d6223c90 has no cell smbtorture/kerberos",
		"smbtorture/badger":    "smbtorture/badger didn't run to its end in run 20261019T231500Z-d6223c90: not run: the fixture: dfs didn't start",
		"smbtorture/badger-s3": "smbtorture/badger-s3 didn't run to its end in run 20261019T231500Z-d6223c90: failed: 1 failed: smb2.lock.lock10; smb2.lock: timed out after 5m0s",
	} {
		if _, err := Baseline(dir, cell); err == nil || err.Error() != want {
			t.Errorf("%s: got %v, want %q", cell, err, want)
		}
	}

	run.Dirty = true
	if err := run.Write(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := Baseline(dir, "smbtorture/memory"); err == nil || err.Error() != "run 20261019T231500Z-d6223c90 ran with changes its commit d6223c90 doesn't have" {
		t.Errorf("a run with changes: got %v", err)
	}
}

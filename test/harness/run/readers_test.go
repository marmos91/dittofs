package run

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/harness/report"
)

// fakeTAP has the tap format read by read until the test ends, so a test of
// how a cell uses its reader decides what the reader finds.
func fakeTAP(t *testing.T, read report.Reader) {
	real := readers["tap"]
	readers["tap"] = read
	t.Cleanup(func() { readers["tap"] = real })
}

// result shows one result as "name: status (the tool's status) [grade],
// duration | the first line of its message", leaving out what it doesn't
// have.
func result(t report.Test) string {
	line := t.Name + ": " + t.Status
	if t.RawStatus != "" {
		line += " (" + t.RawStatus + ")"
	}
	if t.Grade != "" {
		line += " [" + t.Grade + "]"
	}
	line += ", " + t.Duration.String()
	if first, _, _ := strings.Cut(t.Message, "\n"); first != "" {
		line += " | " + strings.TrimSpace(first)
	}
	return line
}

// prove's report on one test file is its one result: the shell grader's
// cases, its reports without -v, and reports prove -v printed. A bail-out, a
// parse error or a report prove didn't finish is incomplete, as is a file
// that couldn't find pjdfstest; a failed assertion or a dubious exit fails
// it, and a TODO assertion doesn't. A plan of 1..0 # SKIP skips it, with its
// reason. A file that timed out failed, which no entry excuses.
func TestReadTAP(t *testing.T) {
	const (
		didntFinish = "prove didn't finish its report"
		footer      = "Files=1, Tests=2,  0 wallclock secs ( 0.01 usr + 0.00 sys = 0.01 CPU)\n"
	)
	for _, c := range []struct {
		name, output string
		timedOut     bool
		status       string
		grade        string
		message      string
	}{
		{name: "complete pass", output: "/fixture/tests/pass/00.t .. ok\nAll tests successful.\n" + footer + "Result: PASS\n", status: report.Passed},
		{name: "complete failure", output: `Test Summary Report
-------------------
/fixture/tests/known/00.t (Wstat: 0 Tests: 2 Failed: 1)
  Failed test: 2
` + footer + "Result: FAIL\n", status: report.Failed, message: "Failed test: 2"},
		{name: "bad plan", output: `Test Summary Report
-------------------
/fixture/tests/known/00.t (Wstat: 0 Tests: 1 Failed: 0)
  Parse errors: Bad plan. You planned 2 tests but ran 1.
Files=1, Tests=1,  0 wallclock secs ( 0.01 usr + 0.00 sys = 0.01 CPU)
Result: FAIL
`, status: report.Other, message: "Parse errors: Bad plan. You planned 2 tests but ran 1."},
		{name: "missing plan", output: `Test Summary Report
-------------------
/fixture/tests/new/00.t (Wstat: 0 Tests: 1 Failed: 0)
  Parse errors: No plan found in TAP output
Files=1, Tests=1,  0 wallclock secs ( 0.01 usr + 0.00 sys = 0.01 CPU)
Result: FAIL
`, status: report.Other, message: "Parse errors: No plan found in TAP output"},
		{name: "bailout", output: `/fixture/tests/known/00.t ..
1..2
ok 1
Bail out! export disappeared
Bailout called. Further testing stopped: export disappeared
Test Summary Report
-------------------
/fixture/tests/known/00.t (Wstat: 0 Tests: 1 Failed: 0)
Files=1, Tests=1,  0 wallclock secs ( 0.01 usr + 0.00 sys = 0.01 CPU)
Result: FAIL
`, status: report.Other, message: "Bail out! export disappeared\nBailout called. Further testing stopped: export disappeared"},
		{name: "summary truncated before totals", output: "Test Summary Report\n-------------------\n/fixture/tests/known/00.t (Wstat: 0 Tests: 2 Failed: 1)\n",
			status: report.Other, message: didntFinish},
		{name: "summary truncated before result", output: "Test Summary Report\n-------------------\n/fixture/tests/known/00.t (Wstat: 0 Tests: 2 Failed: 1)\n" + footer,
			status: report.Other, message: didntFinish},
		{name: "success marker without footer", output: "All tests successful.\n", status: report.Other, message: didntFinish},
		{name: "failure without a gradable test", output: "Test Summary Report\n-------------------\nFiles=1, Tests=1,  0 wallclock secs ( 0.01 usr + 0.00 sys = 0.01 CPU)\nResult: FAIL\n",
			status: report.Other, message: "prove failed, and named no failed assertion or exit status"},

		{name: "-v pass", output: `/tools/pjdfstest/tests/chmod/00.t ..
1..2
ok 1
ok 2
ok
All tests successful.
Files=1, Tests=2,  0 wallclock secs ( 0.01 usr +  0.01 sys =  0.02 CPU)
Result: PASS
`, status: report.Passed},
		{name: "-v failure beside a TODO", output: `/tools/pjdfstest/tests/chmod/00.t ..
1..4
ok 1
not ok 2 # TODO later
not ok 3 - tried 'chmod x 0644', expected 0, got EPERM
ok 4
Failed 1/4 subtests

Test Summary Report
-------------------
/tools/pjdfstest/tests/chmod/00.t (Wstat: 0 Tests: 4 Failed: 1)
  Failed test:  3
Files=1, Tests=4,  0 wallclock secs ( 0.01 usr +  0.00 sys =  0.01 CPU)
Result: FAIL
`, status: report.Failed, message: "not ok 3 - tried 'chmod x 0644', expected 0, got EPERM\nFailed test:  3"},
		{name: "-v dubious", output: `/tools/pjdfstest/tests/chmod/00.t ..
1..2
ok 1
ok 2
Dubious, test returned 1 (wstat 256, 0x100)
All 2 subtests passed

Test Summary Report
-------------------
/tools/pjdfstest/tests/chmod/00.t (Wstat: 256 Tests: 2 Failed: 0)
  Non-zero exit status: 1
Files=1, Tests=2,  0 wallclock secs ( 0.01 usr +  0.00 sys =  0.01 CPU)
Result: FAIL
`, status: report.Failed, message: "Non-zero exit status: 1"},
		{name: "-v TODO only", output: `/tools/pjdfstest/tests/chmod/00.t ..
1..2
ok 1
not ok 2 # TODO later
ok
All tests successful.
Files=1, Tests=2,  0 wallclock secs ( 0.01 usr +  0.00 sys =  0.01 CPU)
Result: PASS
`, status: report.Passed},
		{name: "-v killed by a signal", output: `/tools/pjdfstest/tests/chmod/00.t ..
1..2
ok 1
Failed 1/2 subtests

Test Summary Report
-------------------
/tools/pjdfstest/tests/chmod/00.t (Wstat: 9 Tests: 1 Failed: 0)
  Non-zero wait status: 9
  Parse errors: Bad plan.  You planned 2 tests but ran 1.
Files=1, Tests=1,  0 wallclock secs ( 0.01 usr +  0.01 sys =  0.02 CPU)
Result: FAIL
`, status: report.Other, message: "Non-zero wait status: 9\nParse errors: Bad plan.  You planned 2 tests but ran 1."},
		{name: "-v bail-out", output: "Bailout called.  Further testing stopped:  export disappeared\nFAILED--Further testing stopped: export disappeared\n",
			status: report.Other, message: "Bailout called.  Further testing stopped:  export disappeared"},
		{name: "-v pjdfstest not found", output: `/tools/pjdfstest/tests/chmod/00.t ..
not ok - could not find pjdfstest app
Dubious, test returned 1 (wstat 256, 0x100)
Failed 1/1 subtests

Test Summary Report
-------------------
/tools/pjdfstest/tests/chmod/00.t (Wstat: 256 Tests: 1 Failed: 1)
  Failed test:  1
  Non-zero exit status: 1
  Parse errors: No plan found in TAP output
Files=1, Tests=1,  0 wallclock secs ( 0.01 usr +  0.01 sys =  0.02 CPU)
Result: FAIL
`, status: report.Other, message: "not ok - could not find pjdfstest app\nFailed test:  1\nNon-zero exit status: 1\nParse errors: No plan found in TAP output"},
		{name: "-v plan skipping every test", output: "/tools/pjdfstest/tests/chmod/00.t .. skipped: no ACL support\nFiles=1, Tests=0,  0 wallclock secs ( 0.01 usr +  0.01 sys =  0.02 CPU)\nResult: NOTESTS\n",
			status: report.Skipped, message: "no ACL support"},
		{name: "timed out", output: "/tools/pjdfstest/tests/chmod/00.t .. \n1..2\nok 1\n", timedOut: true,
			status: report.Failed, grade: report.New, message: "timed out"},
	} {
		got, err := readTAP(report.Ran{Test: "chmod/00.t", Output: []byte(c.output), TimedOut: c.timedOut, Took: 2 * time.Second})
		if err != nil || len(got) != 1 {
			t.Errorf("%s: %+v, %v", c.name, got, err)
			continue
		}
		if g := got[0]; g.Name != "chmod/00.t" || g.Duration != 2*time.Second || g.Status != c.status || g.Grade != c.grade || g.Message != c.message {
			t.Errorf("%s: %s %q %q, want %s %q %q", c.name, g.Status, g.Grade, g.Message, c.status, c.grade, c.message)
		}
	}
}

// writeFiles writes files under a new directory, as a tool writes its results
// into {out}, and returns it.
func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Every JUnit XML file under {out} is read, a result for each test case.
// pynfs's, testdata/pynfs/junit.xml, as its --xml writes it: each test named
// by its code; a FAILURE fails with its message, an OMIT skips, and a PASS,
// a WARNING (SEC2) and an UNSUPPORTED (OPENAT1) pass. A file without codes
// names its cases classname.name, at any depth, and an error fails one. A
// run in which every test skipped executed nothing, which is an error, and
// a file that isn't XML is too.
func TestReadJUnit(t *testing.T) {
	generic := writeFiles(t, map[string]string{
		"a.xml": `<testsuites><testsuite name="a"><testcase classname="pkg.A" name="ok" time="0.5"/>
<testcase classname="pkg.A" name="broke"><error message="panic: nil map">trace</error></testcase></testsuite></testsuites>`,
		"b.xml": `<testsuite name="b"><testcase classname="pkg.B" name="fails"><failure message="want 1"/></testcase>
<testcase classname="pkg.B" name="later"><skipped message="not yet"/></testcase></testsuite>`,
	})
	for _, c := range []struct {
		name, out string
		want      []string
		err       string
	}{
		{"pynfs", filepath.Join("testdata", "pynfs"), []string{
			"LOOK1: failed, 12.5ms | OP_LOOKUP should return NFS4ERR_NOENT, instead got NFS4ERR_INVAL : FAILURE of the whole compound",
			"LAYOUTGET1: failed, 3ms | LAYOUTGET returned NFS4ERR_NOTSUPP",
			"ACC1a: passed, 250ms",
			"OPENAT1: passed, 1ms",
			"SEC2: passed, 2ms",
			"DELEG1: skipped, 0s",
			"LOCK20: skipped, 0s",
		}, ""},
		{"every test skipped", filepath.Join("testdata", "pynfs-omit"), []string{"LOCK20: skipped, 0s", "LOCKMRG: skipped, 0s"}, "every test skipped, so nothing ran"},
		{"generic", generic, []string{
			"pkg.A.ok: passed, 500ms",
			"pkg.A.broke: failed, 0s | panic: nil map",
			"pkg.B.fails: failed, 0s | want 1",
			"pkg.B.later: skipped, 0s | not yet",
		}, ""},
		{"no file", t.TempDir(), nil, ""},
		{"cut inside a case", writeFiles(t, map[string]string{"junit.xml": `<testsuite><testcase code="A" name="a">`}), nil, "junit.xml: XML syntax error on line 1: unexpected EOF"},
		{"cut after a case", writeFiles(t, map[string]string{"junit.xml": `<testsuite><testcase code="A" name="a"/>`}), nil, "junit.xml: XML syntax error on line 1: unexpected EOF"},
	} {
		got, err := readJUnit(report.Ran{Out: c.out})
		var lines []string
		for _, r := range got {
			lines = append(lines, result(r))
		}
		said := ""
		if err != nil {
			said = err.Error()
		}
		if !slices.Equal(lines, c.want) || said != c.err {
			t.Errorf("%s: %q, %q\nwant %q, %q", c.name, lines, said, c.want, c.err)
		}
	}
}

// WPTS's TRX file is read wherever it is under {out}, a result for each
// test, the tests it didn't execute included, named by its testName and timed
// by its duration, and no other file is: testdata/wpts holds results of a
// WPTS run, four with their outcome changed to Error, Timeout, Aborted and
// Inconclusive, beside the server's dfs.log. Passed passes; Failed, Error,
// Timeout and Aborted fail; NotExecuted and any other outcome skip, keeping
// the outcome as the raw status. A message keeps no trailing line break, and
// the run's warnings aren't errors. A run whose testhost process crashed, as
// dotnet test wrote it when one was killed, returns its results and the run's
// error.
func TestReadTRX(t *testing.T) {
	got, err := readTRX(report.Ran{Out: filepath.Join("testdata", "wpts")})
	var lines []string
	for _, r := range got {
		lines = append(lines, result(r))
	}
	want := []string{
		"FileInfo_Set_FileBasicInformation_Timestamp_MinusOne_Dir_LastWriteTime: passed, 3.1675302s",
		"BVT_TunnelSCSIPersistentReserve_ReserveAndRelease: failed, 66.2605ms | Test method Microsoft.Protocols.TestSuites.FileSharing.RSVD.TestSuite.SCSIPersistentReservation.BVT_TunnelSCSIPersistentReserve_ReserveAndRelease threw exception:",
		"BVT_CopyOffload: skipped (NotExecuted), 62.0901ms | Initialization method Microsoft.Protocols.TestSuites.FileSharing.SMB2.TestSuite.CopyOffload.BaseTestInitialize threw exception. Assert.Inconclusive is inconclusive. Test case is applicable in servers that implement IOCTL FSCTL_OFFLOAD_READ.",
		"BVT_Replay_ReplayCreate: skipped (NotExecuted), 134.1729ms | Assert.Inconclusive is inconclusive. Test case is applicable in servers that support GLOBAL_CAP_MULTI_CHANNEL.",
		"BVT_Sqos_UpdateCounters: failed, 67.6998ms | Test method Microsoft.Protocols.TestSuites.FileSharing.SQOS.TestSuite.SqosBasic.BVT_Sqos_UpdateCounters threw exception:",
		"BVT_Sqos_ProbePolicy: failed, 70.0086ms | Test method Microsoft.Protocols.TestSuites.FileSharing.SQOS.TestSuite.SqosBasic.BVT_Sqos_ProbePolicy threw exception:",
		"BVT_OpenSharedVHDSetByTargetSpecifier: failed, 65.4726ms | Test method Microsoft.Protocols.TestSuites.FileSharing.RSVD.TestSuite.CreateAndDeleteCheckpoint.BVT_OpenSharedVHDSetByTargetSpecifier threw exception:",
		"BVT_TunnelValidateDiskToSharedVHD: skipped (Inconclusive), 69.8702ms | Test method Microsoft.Protocols.TestSuites.FileSharing.RSVD.TestSuite.TunnelOperationToSharedVHD.BVT_TunnelValidateDiskToSharedVHD threw exception:",
	}
	if err != nil || !slices.Equal(lines, want) {
		t.Errorf("%v\n%s\nwant\n%s", err, strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if len(got) > 3 && !strings.HasSuffix(got[3].Message, "=== End of Stack Trace ===") {
		t.Errorf("%s's message ends %q", got[3].Name, got[3].Message[max(0, len(got[3].Message)-30):])
	}
	crashed := writeFiles(t, map[string]string{"TestResults/run.trx": `<TestRun xmlns="http://microsoft.com/schemas/VisualStudio/TeamTest/2010">
  <Results><UnitTestResult testName="BVT_AppInstanceId" duration="00:00:00.0610000" outcome="Failed" /></Results>
  <ResultSummary outcome="Failed">
    <Counters total="1" executed="1" passed="0" failed="1" />
    <RunInfos><RunInfo computerName="bb7417e114da" outcome="Error" timestamp="2026-10-20T08:18:48.9693531+00:00">
      <Text>The active test run was aborted. Reason: Test host process crashed</Text>
    </RunInfo></RunInfos>
  </ResultSummary>
</TestRun>`})
	got, err = readTRX(report.Ran{Out: crashed})
	if len(got) != 1 || err == nil || err.Error() != "run.trx: The active test run was aborted. Reason: Test host process crashed" {
		t.Errorf("crashed: %+v, %v", got, err)
	}
	if got, err := readTRX(report.Ran{Out: t.TempDir()}); got != nil || err != nil {
		t.Errorf("no file: %+v, %v", got, err)
	}
	cut := writeFiles(t, map[string]string{"TestResults/run.trx": `<TestRun><Results><UnitTestResult testName="a" outcome="Passed">`})
	if _, err := readTRX(report.Ran{Out: cut}); err == nil || !strings.HasPrefix(err.Error(), "run.trx: XML syntax error") {
		t.Errorf("cut short: %v", err)
	}
}

// Each conformance format is read through its suite's cell and graded
// against its known file. pjdfstest's case from the shell grader's tests: a
// known failure passes and a new one fails the cell. pynfs's: known failures
// by an exact name and a wildcard, and its OMITs as expected skips, pass the
// cell. WPTS's: a NotExecuted test that isn't an expected skip leaves the
// cell not run, keeping NotExecuted as its raw status.
func TestReadersGradeTheirResults(t *testing.T) {
	testdata := func(name string) string {
		data, err := os.ReadFile(filepath.Join("testdata", filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	failure := func(file string) string {
		return "Test Summary Report\n-------------------\n/fixture/tests/" + file + " (Wstat: 0 Tests: 2 Failed: 1)\n  Failed test: 2\nFiles=1, Tests=2,  0 wallclock secs\nResult: FAIL\n"
	}
	const wontfix = "fix: wontfix, category: proto, reason: r"
	root, r := repo(t, `groups: { g: [pjdfstest, pynfs, wpts] }
suites:
  pjdfstest: { cmd: "cat {test}", tests: "pjd/*.t", format: tap, known: pjd.yaml }
  pynfs: { cmd: "cp pynfs.xml {out}/junit.xml", format: junit, known: pynfs.yaml }
  wpts: { cmd: { sh: "mkdir -p {out}/config/TestResults && cp wpts.trx {out}/config/TestResults/" }, format: trx, known: wpts.yaml }
`, map[string]string{
		"pjd/known.t": failure("known/00.t"),
		"pjd/new.t":   failure("new/00.t"),
		"pjd/pass.t":  "/fixture/tests/pass/00.t .. ok\nAll tests successful.\nFiles=1, Tests=2,  0 wallclock secs\nResult: PASS\n",
		"pjd.yaml":    "- { test: pjd/known.t, status: fail, " + wontfix + " }\n",
		"pynfs.xml":   testdata("pynfs/junit.xml"),
		"pynfs.yaml": "- { test: LOOK1, status: fail, " + wontfix + " }\n- { test: LAYOUT*, status: fail, " + wontfix + " }\n" +
			"- { test: DELEG1, status: skip, " + wontfix + " }\n- { test: LOCK20, status: skip, " + wontfix + " }\n",
		"wpts.trx": testdata("wpts/config/TestResults/wpts.trx"),
		"wpts.yaml": "- { test: BVT_Tunnel*, status: fail, " + wontfix + " }\n- { test: BVT_Sqos_*, status: fail, " + wontfix + " }\n" +
			"- { test: BVT_OpenSharedVHDSetByTargetSpecifier, status: fail, " + wontfix + " }\n" +
			"- { test: BVT_CopyOffload, status: skip, " + wontfix + " }\n- { test: BVT_TunnelValidateDiskToSharedVHD, status: skip, " + wontfix + " }\n",
	})
	rep := runIn(t, root, r, "g")
	want := []report.Cell{
		{Name: "pjdfstest", Result: report.Failed, Note: "1 failed: pjd/new.t"},
		{Name: "pynfs", Result: report.Passed},
		{Name: "wpts", Result: report.NotRun, Note: "1 skipped unexpectedly: BVT_Replay_ReplayCreate"},
	}
	if len(rep.Outcomes) != len(want) {
		t.Fatalf("outcomes %+v", rep.Outcomes)
	}
	for i, o := range rep.Outcomes {
		if o.Name != want[i].Name || o.Result != want[i].Result || o.Note != want[i].Note {
			t.Errorf("%s: %s %q, want %s %q", o.Name, o.Result, o.Note, want[i].Result, want[i].Note)
		}
	}
	var pjd []string
	for _, tc := range rep.Outcomes[0].Tests {
		pjd = append(pjd, tc.Name+": "+tc.Status+" ["+tc.Grade+"]")
	}
	if want := []string{"pjd/known.t: skipped [known]", "pjd/new.t: failed [new]", "pjd/pass.t: passed []"}; !slices.Equal(pjd, want) {
		t.Errorf("pjdfstest: %q, want %q", pjd, want)
	}
	i := slices.IndexFunc(rep.Outcomes[2].Tests, func(tc report.Test) bool { return tc.Name == "BVT_Replay_ReplayCreate" })
	if i < 0 || rep.Outcomes[2].Tests[i].Status != report.Other || rep.Outcomes[2].Tests[i].RawStatus != "NotExecuted" {
		t.Errorf("wpts: %+v", rep.Outcomes[2].Tests)
	}
}

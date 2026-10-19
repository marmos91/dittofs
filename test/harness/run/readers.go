package run

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/marmos91/dittofs/test/harness/report"
)

var (
	// proveFooter is the line before the last of prove's report, which a
	// report cut short lacks.
	proveFooter = regexp.MustCompile(`^Files=[1-9][0-9]*, Tests=[0-9]+,`)
	// tapIncomplete is a line saying the TAP can't be trusted: a parse error,
	// such as a plan it didn't meet, or a bail-out.
	tapIncomplete = regexp.MustCompile(`^(Parse errors:|Bail out!|Bailout called\.)`)
	// tapFailed is a line saying why a file failed: a failed assertion, or
	// prove's summary of the file's failed assertions or its exit.
	tapFailed = regexp.MustCompile(`^(not ok|Failed tests?:|Non-zero (exit|wait) status:)`)
)

// readTAP reads what prove printed for one test file, the file's TAP with
// -v, as one result named after the test. A file whose TAP has a bail-out or
// a parse error, a plan it didn't meet included, is incomplete, and so is one
// whose report prove didn't finish. Otherwise prove's result decides: a file
// with a failed assertion fails, and so does one with clean TAP that exited
// non-zero, which prove calls dubious. A file that timed out failed, a hang
// no entry excuses.
func readTAP(r report.Ran) ([]report.Test, error) {
	t := report.Test{Name: r.Test, Status: report.Other, Duration: r.Took}
	if r.TimedOut {
		t.Status, t.Grade, t.Message = report.Failed, report.New, "timed out"
		return []report.Test{t}, nil
	}
	var why []string   // the lines that say why it failed or is incomplete
	var last [2]string // the last two lines with text
	incomplete := false
	for _, line := range strings.Split(string(r.Output), "\n") {
		s := strings.TrimSpace(line)
		if s == "" {
			continue
		}
		last[0], last[1] = last[1], s
		switch {
		case tapIncomplete.MatchString(s):
			incomplete = true
			why = append(why, s)
		case tapFailed.MatchString(s) && !strings.Contains(s, "# TODO"):
			why = append(why, s)
		}
	}
	finished := proveFooter.MatchString(last[0]) && (last[1] == "Result: PASS" || last[1] == "Result: FAIL")
	switch {
	case incomplete:
		t.Message = strings.Join(why, "\n")
	case !finished:
		t.Message = "prove didn't finish its report"
	case last[1] == "Result: PASS":
		t.Status = report.Passed
	case len(why) == 0:
		t.Message = "prove failed, and named no failed assertion or exit status"
	default:
		t.Status, t.Message = report.Failed, strings.Join(why, "\n")
	}
	return []report.Test{t}, nil
}

// readJUnit reads every JUnit XML file under {out}: a result for each test
// case, named by its code attribute when it has one, as pynfs's lists are
// keyed on it, else by classname.name. A failure or an error fails it, a skip
// skips it, and a case with none passes, as pynfs's WARNING and UNSUPPORTED
// do. pynfs writes its file only after a clean finish, so a run it didn't
// finish has no results. A file in which every test skipped executed nothing,
// so it doesn't count as a run.
func readJUnit(r report.Ran) ([]report.Test, error) {
	var tests []report.Test
	for _, f := range resultFiles(r.Out, ".xml") {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		d := xml.NewDecoder(bytes.NewReader(data))
		for {
			tok, err := d.Token()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("%s: %w", filepath.Base(f), err)
			}
			start, ok := tok.(xml.StartElement)
			if !ok || start.Name.Local != "testcase" {
				continue
			}
			type result struct {
				Message string `xml:"message,attr"`
			}
			var c struct {
				Code      string  `xml:"code,attr"`
				Name      string  `xml:"name,attr"`
				Classname string  `xml:"classname,attr"`
				Time      string  `xml:"time,attr"` // in seconds
				Failure   *result `xml:"failure"`
				Error     *result `xml:"error"`
				Skipped   *result `xml:"skipped"`
			}
			if err := d.DecodeElement(&c, &start); err != nil {
				return nil, fmt.Errorf("%s: %w", filepath.Base(f), err)
			}
			seconds, _ := strconv.ParseFloat(c.Time, 64)
			t := report.Test{Name: c.Code, Status: report.Passed, Duration: time.Duration(seconds * float64(time.Second))}
			if t.Name == "" {
				t.Name = c.Classname + "." + c.Name
			}
			switch {
			case c.Failure != nil:
				t.Status, t.Message = report.Failed, c.Failure.Message
			case c.Error != nil:
				t.Status, t.Message = report.Failed, c.Error.Message
			case c.Skipped != nil:
				t.Status, t.Message = report.Skipped, c.Skipped.Message
			}
			tests = append(tests, t)
		}
	}
	if len(tests) > 0 && !slices.ContainsFunc(tests, func(t report.Test) bool { return t.Status != report.Skipped }) {
		return tests, errors.New("every test skipped, so nothing ran")
	}
	return tests, nil
}

// readTRX reads every TRX file under {out}, as WPTS writes one for its run
// with a result for each test, the tests it didn't execute included, named by
// its testName. Passed passes; Failed, Error, Timeout and Aborted fail;
// NotExecuted and any other outcome skip, with the outcome kept as the raw
// status.
func readTRX(r report.Ran) ([]report.Test, error) {
	var tests []report.Test
	for _, f := range resultFiles(r.Out, ".trx") {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var run struct {
			Results []struct {
				TestName string `xml:"testName,attr"`
				Outcome  string `xml:"outcome,attr"`
				Duration string `xml:"duration,attr"` // hh:mm:ss.fffffff
				Message  string `xml:"Output>ErrorInfo>Message"`
			} `xml:"Results>UnitTestResult"`
		}
		if err := xml.Unmarshal(data, &run); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		for _, res := range run.Results {
			h, rest, _ := strings.Cut(res.Duration, ":")
			m, s, _ := strings.Cut(rest, ":")
			took, _ := time.ParseDuration(h + "h" + m + "m" + s + "s")
			t := report.Test{Name: res.TestName, Status: report.Passed, Duration: took, Message: strings.TrimSpace(res.Message)}
			switch res.Outcome {
			case "Passed":
			case "Failed", "Error", "Timeout", "Aborted":
				t.Status = report.Failed
			default:
				t.Status, t.RawStatus = report.Skipped, res.Outcome
			}
			tests = append(tests, t)
		}
	}
	return tests, nil
}

// resultFiles lists the files with extension ext under dir, where a tool
// writes its results. A directory it can't read is passed over: what it
// holds isn't read, and a cell with no results didn't run.
//
// decision: a suite in a format read from files runs its command once per
// cell, as pynfs and WPTS do; each test of a suite with tests would read the
// files the tests before it wrote too.
func resultFiles(dir, ext string) []string {
	var files []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(p) == ext {
			files = append(files, p)
		}
		return nil
	})
	return files
}

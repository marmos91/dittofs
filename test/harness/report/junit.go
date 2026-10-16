package report

import (
	"cmp"
	"encoding/xml"
	"strconv"
	"strings"
	"time"
)

// junitPath is a cell's JUnit file, relative to the run's directory: its name
// as one path element, as its log's is.
func junitPath(cell string) string {
	return "junit/" + strings.ReplaceAll(cell, "/", "-") + ".xml"
}

// JUnit is the cell as a JUnit XML file, for what reads JUnit and not CTRF:
// one testsuite named by the cell, with a testcase for each result. A result's
// CTRF status, which its grade decides, gives its JUnit one, so a reader of
// either counts the same: failed is a failure, other an error, and skipped is
// skipped, with the grade's reason, else the tool's. A flaky pass is a pass
// with a flaky property.
//
// decision: a testcase's classname is the test's suite in summary.json,
// joined with /, so it names the cell and, for a Go test, its package. The
// testsuite carries the cell's result and note as properties, so a cell with
// no tests, one that didn't run, still says why.
func (c Cell) JUnit() ([]byte, error) {
	s := junitSuite{Name: c.Name, Tests: len(c.Tests), Time: seconds(c.Took.Round(time.Second))}
	s.Properties = &junitProperties{[]junitProperty{{"result", c.Result}}}
	if c.Note != "" {
		s.Properties.List = append(s.Properties.List, junitProperty{"note", c.Note})
	}
	for _, t := range c.Tests {
		tc := junitCase{Name: t.Name, Classname: strings.Join(append([]string{c.Name}, t.Suite...), "/"), Time: seconds(t.Duration)}
		switch t.Status {
		case Failed:
			s.Failures++
			tc.Failure = &junitResult{t.Message}
		case Skipped:
			s.Skipped++
			tc.Skipped = &junitResult{cmp.Or(t.Reason, t.Message)}
		case Other:
			s.Errors++
			tc.Error = &junitResult{t.Message}
		}
		if t.Flaky {
			tc.Properties = &junitProperties{[]junitProperty{{"flaky", "true"}}}
		}
		s.Cases = append(s.Cases, tc)
	}
	data, err := xml.MarshalIndent(junitDoc{Suites: []junitSuite{s}}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(append([]byte(xml.Header), data...), '\n'), nil
}

// seconds is a duration as JUnit writes one, in seconds to the millisecond,
// as summary.json keeps it.
func seconds(d time.Duration) string {
	return strconv.FormatFloat(float64(d.Milliseconds())/1000, 'f', 3, 64)
}

type junitDoc struct {
	XMLName xml.Name     `xml:"testsuites"`
	Suites  []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name       string           `xml:"name,attr"`
	Tests      int              `xml:"tests,attr"`
	Failures   int              `xml:"failures,attr"`
	Errors     int              `xml:"errors,attr"`
	Skipped    int              `xml:"skipped,attr"`
	Time       string           `xml:"time,attr"`
	Properties *junitProperties `xml:"properties"`
	Cases      []junitCase      `xml:"testcase"`
}

type junitProperties struct {
	List []junitProperty `xml:"property"`
}

type junitProperty struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type junitCase struct {
	Name       string           `xml:"name,attr"`
	Classname  string           `xml:"classname,attr"`
	Time       string           `xml:"time,attr"`
	Properties *junitProperties `xml:"properties"`
	Failure    *junitResult     `xml:"failure"`
	Error      *junitResult     `xml:"error"`
	Skipped    *junitResult     `xml:"skipped"`
}

type junitResult struct {
	Message string `xml:"message,attr,omitempty"`
}

package run

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/marmos91/dittofs/test/harness/report"
)

// subunitLine matches a line of smbtorture's output that starts a test or
// gives its result: the keyword, the test's name and the rest of the line.
var subunitLine = regexp.MustCompile(`^(test|success|failure|error|skip):\s+(\S+)(.*)$`)

// The connection-failure filter: what smbtorture prints when its own
// connection setup failed, so the server gave no result. The first line says
// so alone. NT_STATUS_NO_MEMORY doesn't: smbtorture's iconv also returns it
// for a genuine assertion, smb2.charset's partial surrogate, so it counts only
// in a result that also names the connection setup.
var connSetup = regexp.MustCompile(`Establishing SMB2 connection|smb2_connect|Unable to connect|Failed to connect`)

const (
	connFailed = "Establishing SMB2 connection failed"
	noMemory   = "NT_STATUS_NO_MEMORY"
)

// csnDraw is what smb2.replay.channel-sequence prints before a row it drew at
// random. Both of the rows that expect STATUS_FILE_NOT_AVAILABLE can draw
// 0x7fff, the largest legal forward step, which another row of the same table
// expects to succeed, so no server passes that draw.
const csnDraw = "CSN 0x7fff, expecting: NT_STATUS_FILE_NOT_AVAILABLE"

// subunitResult is a failure, error or skip whose details are still being
// read.
type subunitResult struct {
	keyword, name string
	details       []string
	noMemory      bool // a detail is NT_STATUS_NO_MEMORY
	setup         bool // a detail names the connection setup
	conn          bool // the connection setup failed
	draw          bool // it follows the self-contradictory draw
}

func (p *subunitResult) add(line string) {
	p.details = append(p.details, line)
	p.noMemory = p.noMemory || strings.Contains(line, noMemory)
	p.setup = p.setup || connSetup.MatchString(line)
	p.conn = p.conn || strings.Contains(line, connFailed) || p.noMemory && p.setup
}

func (p *subunitResult) test() report.Test {
	t := report.Test{Name: p.name, Status: report.Failed, Message: strings.TrimSpace(strings.Join(p.details, "\n"))}
	switch {
	case p.keyword == "skip":
		t.Status = report.Skipped
	case p.conn:
		t.Status = report.Other
	case p.draw:
		t.Status, t.RawStatus, t.Grade = report.Skipped, report.Failed, report.ExpectedSkip
		t.Reason = "the self-contradictory ChannelSequence draw: 0x7fff must fail here and succeed in another row"
	}
	return t
}

// readSubunit reads smbtorture's results from its test:, success:, failure:,
// error: and skip: lines. A name gets the test's prefix= before it, and then
// smb2. when it lacks one, as the known list names it. A failure's, an
// error's or a skip's details, its message, run to a line "]" or the next
// result. A test: with no result is incomplete, unless the test's limit
// stopped it while it ran, which fails it whatever the known list says. A
// failure in smbtorture's own connection setup is incomplete too: the server
// gave no result. One failure after the self-contradictory ChannelSequence
// draw, in the test that printed it, is an expected skip. A client that
// crashed, exit 129 or more, fails nothing by its exit: the test it left open
// is incomplete, and with none open, as when it died between two tests, so is
// the entry, under its own name, since what it would have reported after its
// last result is lost. A run that reports no test at all is an error.
func readSubunit(r report.Ran) ([]report.Test, error) {
	name := func(s string) string {
		if r.Prefix != "" {
			s = r.Prefix + "." + s
		}
		if !strings.HasPrefix(s, "smb2.") {
			s = "smb2." + s
		}
		return s
	}
	var (
		tests   []report.Test
		open    string // the test whose test: line has no result yet
		drawn   bool   // the running test printed the self-contradictory draw
		pending *subunitResult
	)
	result := func(t report.Test) {
		tests = append(tests, t)
		if t.Name == open {
			open = ""
		}
	}
	end := func() {
		if pending != nil {
			result(pending.test())
			pending = nil
		}
	}
	for _, line := range strings.Split(string(r.Output), "\n") {
		m := subunitLine.FindStringSubmatch(line)
		switch {
		case m == nil && pending != nil:
			if line == "]" {
				end()
			} else {
				pending.add(line)
			}
			continue
		case m == nil:
			drawn = drawn || strings.Contains(line, csnDraw)
			continue
		}
		end()
		keyword, test := m[1], name(m[2])
		switch keyword {
		case "test":
			if open != "" {
				tests = append(tests, report.Test{Name: open, Status: report.Other, Message: "the next test started before its result"})
			}
			open, drawn = test, false
		case "success":
			result(report.Test{Name: test, Status: report.Passed})
		default:
			// What the line holds after the name begins the details, but only
			// the lines after it count for the connection-failure filter.
			pending = &subunitResult{keyword: keyword, name: test, details: []string{strings.Trim(m[3], "[] ")}}
			// One draw excuses one failure.
			if drawn && keyword != "skip" {
				pending.draw, drawn = true, false
			}
		}
	}
	end()
	if open != "" {
		t := report.Test{Name: open, Status: report.Other, Message: "its output stopped before its result"}
		if r.TimedOut {
			t.Status, t.Grade, t.Message = report.Failed, report.New, "its limit stopped it"
		}
		tests = append(tests, t)
	}
	if len(tests) == 0 {
		return nil, errors.New("no results")
	}
	if open == "" && r.Exit > 128 {
		tests = append(tests, report.Test{Name: r.Test, Status: report.Other, Message: fmt.Sprintf("smbtorture died on signal %d after its last result", r.Exit-128)})
	}
	return tests, nil
}

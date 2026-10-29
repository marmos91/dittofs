package run

import (
	"cmp"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/marmos91/dittofs/test/harness/report"
)

var (
	nfstestSubtest = regexp.MustCompile(`^[ \t]*TEST: Running test '`)
	nfstestPass    = regexp.MustCompile(`^[ \t]*PASS: `)
	nfstestFail    = regexp.MustCompile(`^[ \t]*FAIL: `)
	nfstestTrace   = regexp.MustCompile(`^[ \t]*FAIL: Traceback \(most recent call last\)`)
	nfstestTally   = regexp.MustCompile(`^[0-9]+ tests \(([0-9]+) passed, ([0-9]+) failed`)
	nfstestCounts  = regexp.MustCompile(` \([0-9]+ passed, [0-9]+ failed\)$`)
	exceptionType  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*: `)
	ipAddress      = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+`)
	digits         = regexp.MustCompile(`[0-9]+`)
	blanks         = regexp.MustCompile(`[ \t]+`)
)

// readNFStest reads one nfstest module's report, the test being the module:
// a result for each PASS: or FAIL: line, named "module/subtest: message"
// after the last TEST: Running test line, as the module's known list names
// them. A message loses the "(N passed, M failed)" a subtest's line ends
// with, and the "subtest - " it starts with, and has each "|" as "+", so a
// name stays the same from run to run and fits a Markdown table. A Python
// traceback after FAIL: is the result "traceback: Type: message", from the
// exception's first line, at ten spaces' indent, and its last, with addresses
// as <ip> and numbers as N, so an entry excuses that exception and no other.
// A module without its closing tally didn't finish, and one whose tally
// disagrees with the lines read may hide a failure: either is incomplete.
func readNFStest(r report.Ran) ([]report.Test, error) {
	module, subtest := r.Test, ""
	var tests []report.Test
	passed, failed := 0, 0
	emit := func(status, msg string) {
		msg = nfstestCounts.ReplaceAllString(strings.Trim(msg, " \t"), "")
		if subtest != "" {
			msg = strings.TrimPrefix(msg, subtest+" - ")
		}
		if status == report.Passed {
			passed++
		} else {
			failed++
		}
		msg = blanks.ReplaceAllString(strings.ReplaceAll(msg, "|", "+"), " ")
		tests = append(tests, report.Test{Name: module + "/" + cmp.Or(subtest, "-") + ": " + msg, Status: status, Message: msg})
	}
	inTrace, first, last := false, "", ""
	flush := func() {
		if !inTrace {
			return
		}
		inTrace = false
		exc := last
		if t := exceptionType.FindString(first); t != "" && first != last {
			exc = t + exc
		}
		exc = digits.ReplaceAllString(ipAddress.ReplaceAllString(exc, "<ip>"), "N")
		emit(report.Failed, "traceback: "+cmp.Or(exc, "?"))
	}
	finished, tally := false, [2]int{}
	for _, line := range strings.Split(strings.ReplaceAll(string(r.Output), "\r\n", "\n"), "\n") {
		if inTrace && strings.HasPrefix(line, "          ") {
			if s := strings.Trim(line, " \t"); s != "" {
				last = s
				if first == "" && line[10] != ' ' {
					first = s
				}
			}
			continue
		}
		flush()
		switch m := nfstestTally.FindStringSubmatch(line); {
		case nfstestSubtest.MatchString(line):
			subtest = line[strings.LastIndex(line, "Running test '")+len("Running test '"):]
			subtest, _, _ = strings.Cut(subtest, "'")
		case nfstestPass.MatchString(line):
			emit(report.Passed, nfstestPass.ReplaceAllString(line, ""))
		case nfstestTrace.MatchString(line):
			inTrace, first, last = true, "", ""
		case nfstestFail.MatchString(line):
			emit(report.Failed, nfstestFail.ReplaceAllString(line, ""))
		case m != nil:
			finished = true
			tally[0], _ = strconv.Atoi(m[1])
			tally[1], _ = strconv.Atoi(m[2])
		}
	}
	flush()
	incomplete := func(why string) report.Test {
		return report.Test{Name: module + ": " + why, Status: report.Other, Message: why}
	}
	switch {
	case !finished:
		tests = append(tests, incomplete("did not finish"))
	case tally != [2]int{passed, failed}:
		tests = append(tests, incomplete(fmt.Sprintf("tally says %d passed, %d failed; parsed %d, %d", tally[0], tally[1], passed, failed)))
	}
	return tests, nil
}

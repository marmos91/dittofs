package report

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
)

// NewFailure reports whether the result is a new failure: a failure no known
// entry matches, or one not graded yet. An unexpected pass fails too, but
// isn't one.
//
// decision: a result with status other isn't a new failure. It has no result,
// an incomplete test or an unexpected skip, which makes its cell not run, exit
// 2: lost coverage, not a regression. A crash of the code under test is a
// failure, not other. A cell not run is reported, and annotated, as itself.
func (t Test) NewFailure() bool {
	return t.Status == Failed && (t.Grade == "" || t.Grade == New)
}

// Title names the test within its cell: its own suite, such as a Go test's
// package, then its name.
func (t Test) Title() string {
	return strings.Join(append(slices.Clone(t.Suite), t.Name), " ")
}

// Formats are the formats dt report shows a run in.
var Formats = []string{"text", "md", "json"}

// Show writes the run as dt report shows it, as text, md or json. text and md
// give how each cell went, then each new failure; json gives the run as
// summary.json holds it. With failures, each gives only the new failures.
func (r Run) Show(w io.Writer, format string, failures bool) error {
	switch format {
	case "text":
		return r.text(w, failures)
	case "md":
		_, err := io.WriteString(w, r.markdown(failures))
		return err
	case "json":
		if failures {
			r = r.onlyNewFailures()
		}
		data, err := r.json()
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	return fmt.Errorf("format %q is not one of %s", format, strings.Join(Formats, ", "))
}

// failure is a new failure and its cell.
type failure struct {
	cell string
	test Test
}

func (r Run) newFailures() []failure {
	var out []failure
	for _, c := range r.Cells {
		for _, t := range c.Tests {
			if t.NewFailure() {
				out = append(out, failure{c.Name, t})
			}
		}
	}
	return out
}

// onlyNewFailures is the run with each cell's tests down to its new failures.
func (r Run) onlyNewFailures() Run {
	cells := make([]Cell, len(r.Cells))
	for i, c := range r.Cells {
		c.Tests = slices.DeleteFunc(slices.Clone(c.Tests), func(t Test) bool { return !t.NewFailure() })
		cells[i] = c
	}
	r.Cells = cells
	return r
}

// text is the run as tables: its name and exit status, a row for each cell,
// and one for each new failure with its message's first line.
func (r Run) text(w io.Writer, failuresOnly bool) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "run %s: exit %d\n", r.Name, r.Exit)
	if !failuresOnly {
		_, _ = fmt.Fprintln(tw, "CELL\tRESULT\tTIME\tNOTE")
		for _, c := range r.Cells {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", c.Name, c.Result, c.Took, oneLine.Replace(c.Note))
		}
	}
	failed := r.newFailures()
	switch {
	case len(failed) > 0:
		if !failuresOnly {
			_, _ = fmt.Fprintln(tw)
		}
		_, _ = fmt.Fprintln(tw, "CELL\tNEW FAILURE\tMESSAGE")
		for _, f := range failed {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", f.cell, f.test.Title(), FirstLine(f.test.Message))
		}
	case failuresOnly:
		_, _ = fmt.Fprintln(tw, "no new failures")
	}
	return tw.Flush()
}

// The limits of the new failures in a job summary, which must stay well
// under the 1 MiB a step's summary may hold, whatever the run.
const (
	maxRows    = 200
	maxMessage = 200 // characters of a message's first line
)

// markdown is the run for a job summary: a table of how each cell went, then
// a table of the new failures, up to maxRows, with their messages' first
// lines, cut to maxMessage. A name or message is shown as code, so what a
// tool prints can't turn into markup.
//
// decision: the title names the run's cells, since summary.json doesn't keep
// dt run's command line. A run of one suite, as each lint job's is, gets the
// title it had when dt run wrote the job summary itself.
func (r Run) markdown(failuresOnly bool) string {
	var b strings.Builder
	if !failuresOnly {
		names := make([]string, len(r.Cells))
		for i, c := range r.Cells {
			names[i] = c.Name
		}
		_, _ = fmt.Fprintf(&b, "### dt run %s\n\n| Cell | Result | Time | Note |\n|---|---|---|---|\n", strings.Join(names, ", "))
		for _, c := range r.Cells {
			_, _ = fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", Code(c.Name), c.Result, c.Took, escape(c.Note))
		}
	}
	failed := r.newFailures()
	switch {
	case len(failed) > 0:
		if !failuresOnly {
			b.WriteString("\n")
		}
		b.WriteString("#### New failures\n\n| Cell | Test | Message |\n|---|---|---|\n")
		for _, f := range failed[:min(len(failed), maxRows)] {
			_, _ = fmt.Fprintf(&b, "| %s | %s | %s |\n", Code(f.cell), Code(f.test.Title()), Code(cut(FirstLine(f.test.Message), maxMessage)))
		}
		if more := len(failed) - maxRows; more > 0 {
			_, _ = fmt.Fprintf(&b, "\nand %d more new failures, all in summary.json\n", more)
		}
	case failuresOnly:
		b.WriteString("No new failures.\n")
	}
	return b.String()
}

// oneLine puts text on one line.
var oneLine = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ")

// escape is text for a Markdown table's cell: on one line, and a | can't end
// it.
func escape(s string) string {
	return strings.ReplaceAll(oneLine.Replace(s), "|", `\|`)
}

// Code is s as code in a Markdown table's cell, fenced with one backquote
// more than the longest run of them in s, so none can end it; "" stays empty.
func Code(s string) string {
	if s == "" {
		return ""
	}
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	if longest > 0 {
		return fence + " " + escape(s) + " " + fence
	}
	return fence + escape(s) + fence
}

// cut keeps the first n characters of s, and says when it dropped the rest.
func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + " ..."
	}
	return s
}

// FirstLine is a message's first line that isn't blank.
func FirstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

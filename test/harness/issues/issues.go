// Package issues posts develop's runs to GitHub issues: the develop-status
// issue, which holds develop's failing set and is open while an entry of it
// alerts, and an issue for each test that fails 2 runs in a row
// (docs/internals/rfcs/rfc-29-test-harness.md).
package issues

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/marmos91/dittofs/test/harness/report"
)

// label marks the develop-status issue and each test's issue, and statusTitle
// names the develop-status issue. A test's issue is titled "CELL TEST fails on
// develop".
//
// decision: an issue is found by its label and its title. The label alone
// also marks the issues the earlier alarm opened and issues people label by
// hand. A test's title names its cell, as CTRF names a test by its suite, so a
// test failing in two cells has an issue in each.
const (
	label       = "develop-red"
	statusTitle = "develop is red"
)

const (
	// maxFailing is the most new failing tests a tier run may have and still
	// post an issue for each: more means something systemic broke, and the
	// develop-status issue is the one place they go.
	maxFailing = 10
	// maxEntries is the most entries the develop-status issue keeps.
	//
	// decision: 50 keep its body under the 65,536 characters GitHub allows
	// while an entry's cell and test names stay under 600 characters
	// together. A run with more broke something systemic: its job summaries
	// name the rest, and an entry left out counts its runs from 1 again.
	maxEntries = 50
	// maxLine is the most characters of a failing line a test's issue shows.
	maxLine = 200
	// marker begins the failing set, as JSON, in a comment at the end of the
	// develop-status issue's body, where the next post reads it.
	marker = "<!-- dt failing set: "
)

// Client posts to a repository's issues through GitHub's REST API.
type Client struct {
	API   string // the API's root, such as https://api.github.com
	Repo  string // owner/name
	Token string // one that can write issues
	Log   string // where the runs' logs are, such as a workflow run's page; "" when nowhere public
}

// FromEnv is the client GitHub's environment variables give: the API at
// GITHUB_API_URL, the repository GITHUB_REPOSITORY, DittoFS's by default, the
// token GITHUB_TOKEN, which must be set, and as Log the workflow run's page
// when GITHUB_RUN_ID names one.
func FromEnv() (Client, error) {
	c := Client{
		API:   cmp.Or(os.Getenv("GITHUB_API_URL"), "https://api.github.com"),
		Repo:  cmp.Or(os.Getenv("GITHUB_REPOSITORY"), "marmos91/dittofs"),
		Token: os.Getenv("GITHUB_TOKEN"),
	}
	if c.Token == "" {
		return Client{}, errors.New("posting needs a token that can write issues, in GITHUB_TOKEN")
	}
	if id := os.Getenv("GITHUB_RUN_ID"); id != "" {
		c.Log = os.Getenv("GITHUB_SERVER_URL") + "/" + c.Repo + "/actions/runs/" + id
	}
	return c, nil
}

// entry is one member of develop's failing set: a test's new failure or
// unexpected pass, or a cell that failed with no test to name or didn't run,
// as Kind says; in how many of its cell's runs in a row; and the test's own
// issue, once it has one.
type entry struct {
	Cell  string `json:"cell"`
	Test  string `json:"test,omitempty"` // "" for the cell itself
	Kind  string `json:"kind"`           // new, unexpected_pass, failed or not run
	Runs  int    `json:"runs"`
	Issue int    `json:"issue,omitempty"`
}

type key struct{ cell, test, kind string }

func (e entry) key() key { return key{e.Cell, e.Test, e.Kind} }

// name is the entry's cell and test, as code.
func (e entry) name() string {
	return strings.TrimSpace(report.Code(e.Cell) + " " + report.Code(e.Test))
}

// alerts reports whether the entry makes develop red: every entry does but
// a cell that didn't run in only one run in a row.
func (e entry) alerts() bool { return e.Kind != report.NotRun || e.Runs >= 2 }

// entries are a cell's members of the failing set, as dt run annotates them:
// each new failure and unexpected pass, else the cell itself when it failed
// or didn't run. A known, quarantined, flaky or skipped test is none.
func entries(c report.Cell) []entry {
	var out []entry
	for _, t := range c.Tests {
		switch {
		case t.NewFailure():
			out = append(out, entry{Cell: c.Name, Test: t.Title(), Kind: report.New})
		case t.Grade == report.UnexpectedPass:
			out = append(out, entry{Cell: c.Name, Test: t.Title(), Kind: report.UnexpectedPass})
		}
	}
	if len(out) == 0 && (c.Result == report.Failed || c.Result == report.NotRun) {
		out = append(out, entry{Cell: c.Name, Kind: c.Result})
	}
	return out
}

// next is the failing set after one tier run's cells, sorted. A cell that ran
// replaces its entries with its own, each counting on from the old entry of
// its test and kind; one that didn't run keeps its tests' entries, since it
// says nothing of them, and counts its own; and the entries of cells the run
// doesn't have stay as they are, so a run of one tier never clears another's.
func next(old []entry, cells map[string]report.Cell) []entry {
	before := map[key]entry{}
	for _, e := range old {
		before[e.key()] = e
	}
	now := map[key]bool{}
	var set []entry
	for _, c := range cells {
		for _, e := range entries(c) {
			if now[e.key()] {
				continue // a name with several results in the cell
			}
			now[e.key()] = true
			e.Runs, e.Issue = before[e.key()].Runs+1, before[e.key()].Issue
			set = append(set, e)
		}
	}
	for _, e := range old {
		// A cell the run doesn't have is the zero cell, which didn't run.
		if c := cells[e.Cell]; !now[e.key()] && c.Result != report.Passed && c.Result != report.Failed {
			set = append(set, e)
		}
	}
	slices.SortFunc(set, func(a, b entry) int {
		return cmp.Or(strings.Compare(a.Cell, b.Cell), strings.Compare(a.Test, b.Test), strings.Compare(a.Kind, b.Kind))
	})
	return set
}

// Post posts runs, the cells of one tier run of develop, as each CI job writes
// the run of its own cell; a cell in several runs, a job re-run by hand, counts
// from the last run by name, and a planned cell with no run, as one whose job
// died before it wrote its results, didn't run. The develop-status issue gets
// the failing set.
// Unless more than maxFailing tests failed anew, each new failure that has
// failed 2 runs in a row gets an issue of its own, or a comment on the one open
// for it, once: the develop-status issue links it from then on. Post refuses
// any run but develop's merge and nightly runs, and says on out what it
// posted.
//
// decision: two posts at once, as a merge run's and a nightly run's ending
// together, can lose one's update of the develop-status issue: the next run of
// its cells makes it again.
func (c Client) Post(ctx context.Context, runs []report.Run, planned []string, out io.Writer) error {
	runs = slices.SortedFunc(slices.Values(runs), func(a, b report.Run) int { return strings.Compare(a.Name, b.Name) })
	cells := map[string]report.Cell{}
	var last report.Run
	for _, r := range runs {
		// decision: a run is develop's when its checkout was on the branch
		// develop, so a laptop's run of develop at merge or nightly would post
		// too, given --post and a token that can write issues.
		if r.Branch != "develop" || r.Tier != "merge" && r.Tier != "nightly" {
			return fmt.Errorf("run %s is %s's %s run: only develop's merge and nightly runs post", r.Name, cmp.Or(r.Branch, "a detached checkout"), r.Tier)
		}
		for _, cell := range r.Cells {
			cells[cell.Name] = cell
		}
		last = r
	}
	for _, name := range planned {
		if _, ok := cells[name]; !ok {
			cells[name] = report.Cell{Name: name, Result: report.NotRun}
		}
	}
	failing := map[key]report.Test{}
	for _, cell := range cells {
		for _, t := range cell.Tests {
			if t.NewFailure() {
				failing[key{cell.Name, t.Title(), report.New}] = t
			}
		}
	}

	labelled, err := c.labelled(ctx)
	if err != nil {
		return err
	}
	var status *issue
	open := map[string]int{} // each open test issue's number, by title
	for i, is := range labelled {
		switch {
		case is.Title == statusTitle:
			if status == nil {
				status = &labelled[i]
			}
		case is.State == "open" && open[is.Title] == 0:
			open[is.Title] = is.Number
		}
	}
	var old []entry
	if status != nil {
		if old, err = failingSet(status.Body); err != nil {
			return fmt.Errorf("issue #%d: %w", status.Number, err)
		}
		if status.State != "open" {
			// decision: a closed issue keeps only its cells not run once.
			// dt closes it only when nothing else is left, so the rest were
			// closed by hand, which clears them: that is how an entry whose
			// cell is gone, renamed or no longer planned, leaves the set.
			old = slices.DeleteFunc(old, entry.alerts)
		}
	}
	set := next(old, cells)
	more := max(0, len(set)-maxEntries)
	set = set[:len(set)-more]

	for i, e := range set {
		t, now := failing[e.key()]
		if !now || e.Runs < 2 || e.Issue != 0 || len(failing) > maxFailing {
			continue
		}
		title := e.Cell + " " + e.Test + " fails on develop"
		text := c.testText(e, t, last)
		if n := open[title]; n != 0 {
			err = c.call(ctx, http.MethodPost, fmt.Sprintf("/issues/%d/comments", n), map[string]string{"body": text}, nil)
			set[i].Issue = n
			_, _ = fmt.Fprintf(out, "#%d: commented, %s\n", n, title)
		} else {
			var created issue
			err = c.call(ctx, http.MethodPost, "/issues", map[string]any{"title": title, "body": text, "labels": []string{label}}, &created)
			set[i].Issue = created.Number
			_, _ = fmt.Fprintf(out, "#%d: opened, %s\n", created.Number, title)
		}
		if err != nil {
			return err
		}
	}
	return c.writeStatus(ctx, status, old, set, more, last, out)
}

// writeStatus writes the failing set to the develop-status issue, creating the
// issue the first time: open while an entry alerts, else closed. While it
// stays open, a comment says how the alerting entries changed; opening and
// closing it notify by themselves.
func (c Client) writeStatus(ctx context.Context, is *issue, old, set []entry, more int, last report.Run, out io.Writer) error {
	body := statusBody(set, more)
	wasOpen := is != nil && is.State == "open"
	if is == nil {
		if len(set) == 0 {
			return nil
		}
		// decision: the first post creates the issue, closed unless an entry
		// alerts, so a cell that didn't run once has a place to be
		// remembered. That notifies once.
		is = &issue{}
		if err := c.call(ctx, http.MethodPost, "/issues", map[string]any{"title": statusTitle, "body": body, "labels": []string{label}}, is); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "#%d: opened, %s\n", is.Number, statusTitle)
	}
	state := "closed"
	if slices.ContainsFunc(set, entry.alerts) {
		state = "open"
	}
	if is.Body != body || is.State != state {
		if err := c.call(ctx, http.MethodPatch, fmt.Sprintf("/issues/%d", is.Number), map[string]string{"body": body, "state": state}, nil); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "#%d: updated, %s\n", is.Number, state)
	}
	if text := changes(old, set); wasOpen && state == "open" && text != "" {
		text = fmt.Sprintf("The failing set changed at %s (%s):\n\n%s", last.Commit, cmp.Or(c.Log, "run "+last.Name), text)
		if err := c.call(ctx, http.MethodPost, fmt.Sprintf("/issues/%d/comments", is.Number), map[string]string{"body": text}, nil); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "#%d: commented, the failing set changed\n", is.Number)
	}
	return nil
}

// statusBody is the develop-status issue's body: whether develop is red, a
// table of the failing set, and the set itself, for the next post. JSON
// escapes < and >, so nothing in it ends the comment that holds it.
func statusBody(set []entry, more int) string {
	var b strings.Builder
	if slices.ContainsFunc(set, entry.alerts) {
		b.WriteString("develop is red.\n")
	} else {
		b.WriteString("develop is green.\n")
	}
	if len(set) > 0 {
		b.WriteString("\nA new failure or an unexpected pass alerts at once, a cell that didn't run on its second run in a row. A test failing 2 runs in a row gets an issue of its own.\n\n")
		b.WriteString("| Cell | Test | Grade | Runs in a row | Issue |\n|---|---|---|---|---|\n")
		for _, e := range set {
			link := ""
			if e.Issue != 0 {
				link = fmt.Sprintf("#%d", e.Issue)
			}
			_, _ = fmt.Fprintf(&b, "| %s | %s | %s | %d | %s |\n", report.Code(e.Cell), report.Code(e.Test), e.Kind, e.Runs, link)
		}
	}
	if more > 0 {
		_, _ = fmt.Fprintf(&b, "\nand %d more, not kept: the runs' job summaries name them.\n", more)
	}
	data, _ := json.Marshal(set)
	_, _ = fmt.Fprintf(&b, "\n%s%s -->\n", marker, data)
	return b.String()
}

// failingSet is the failing set body holds, none when it holds none.
func failingSet(body string) ([]entry, error) {
	at := strings.LastIndex(body, marker)
	if at < 0 {
		return nil, nil
	}
	data, _, _ := strings.Cut(body[at+len(marker):], " -->")
	var set []entry
	if err := json.Unmarshal([]byte(data), &set); err != nil {
		return nil, fmt.Errorf("the failing set in its body: %w", err)
	}
	return set, nil
}

// changes lists how the alerting entries changed from old to set, "" when
// they didn't.
func changes(old, set []entry) string {
	alerting := func(set []entry) map[key]bool {
		m := map[key]bool{}
		for _, e := range set {
			m[e.key()] = e.alerts()
		}
		return m
	}
	was, is := alerting(old), alerting(set)
	var b strings.Builder
	for _, e := range set {
		if e.alerts() && !was[e.key()] {
			_, _ = fmt.Fprintf(&b, "- now failing: %s, %s\n", e.name(), e.Kind)
		}
	}
	for _, e := range old {
		if e.alerts() && !is[e.key()] {
			_, _ = fmt.Fprintf(&b, "- no longer failing: %s, %s\n", e.name(), e.Kind)
		}
	}
	return b.String()
}

// testText is what a test's issue and its comment say: the test, its failing
// line, the commit and where the logs are, and nothing else, since issues are
// public.
func (c Client) testText(e entry, t report.Test, last report.Run) string {
	line, _, _ := strings.Cut(strings.TrimSpace(t.Message), "\n")
	line = strings.TrimSpace(line)
	if r := []rune(line); len(r) > maxLine {
		line = string(r[:maxLine]) + " ..."
	}
	return fmt.Sprintf("%s in %s has failed %d runs in a row on develop.\n\n- Failing line: %s\n- Commit: %s\n- Log: %s\n",
		report.Code(e.Test), report.Code(e.Cell), e.Runs, report.Code(line), last.Commit, cmp.Or(c.Log, "run "+last.Name))
}

// issue is an issue as GitHub's API gives it, with the fields Post reads.
type issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Body   string `json:"body"`
}

// labelled lists every issue with label, open or closed.
func (c Client) labelled(ctx context.Context) ([]issue, error) {
	var all []issue
	for page := 1; ; page++ {
		var got []issue
		if err := c.call(ctx, http.MethodGet, fmt.Sprintf("/issues?labels=%s&state=all&per_page=100&page=%d", label, page), nil, &got); err != nil {
			return nil, err
		}
		all = append(all, got...)
		if len(got) < 100 {
			return all, nil
		}
	}
}

// call sends in, as JSON, to path in the repository, and decodes the answer
// into out unless out is nil.
func (c Client) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.API+"/repos/"+c.Repo+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %s %s", method, path, resp.Status, bytes.TrimSpace(msg))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

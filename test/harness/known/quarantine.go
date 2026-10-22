package known

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Quarantine is an entry of the quarantine list: a flaky test of a suite, the
// issue that tracks its flake, and the date the entry expires. The test still
// runs and its result is recorded, but its failure never fails a run.
type Quarantine struct {
	Suite   string `yaml:"suite"`
	Test    string `yaml:"test"`    // the test's name, as its results name it
	Issue   string `yaml:"issue"`   // as "#N"
	Expires string `yaml:"expires"` // a date, as 2026-12-31
	Line    int    `yaml:"-"`       // its line in the file
}

// quarantineFields are the keys an entry has, each required.
var quarantineFields = []string{"suite", "test", "issue", "expires"}

// ParseQuarantine reads the quarantine list, named name in its errors: a list
// of entries, each with its suite, its test, its issue and the date it
// expires, and a suite's test listed once. An empty list has no entries.
// Every problem is returned, joined, each as name:line: problem. Whether the
// suite exists and the entry has expired is for its caller to check.
func ParseQuarantine(name string, data []byte) ([]Quarantine, error) {
	var list []yaml.Node
	if err := yaml.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	var entries []Quarantine
	var errs []error
	first := map[[2]string]int{} // each suite and test's line
	for _, n := range list {
		q, problems := parseQuarantine(&n)
		if line, ok := first[[2]string{q.Suite, q.Test}]; ok && len(problems) == 0 {
			problems = append(problems, fmt.Sprintf("%s %s is listed twice, first at line %d", q.Suite, q.Test, line))
		}
		if len(problems) > 0 {
			errs = append(errs, fmt.Errorf("%s:%d: %s", name, n.Line, strings.Join(problems, "; ")))
			continue
		}
		first[[2]string{q.Suite, q.Test}] = q.Line
		entries = append(entries, q)
	}
	return entries, errors.Join(errs...)
}

// parseQuarantine reads one entry, and says what's wrong with it.
func parseQuarantine(n *yaml.Node) (Quarantine, []string) {
	if n.Kind != yaml.MappingNode {
		return Quarantine{}, []string{"an entry is a mapping"}
	}
	var problems []string
	for i := 0; i < len(n.Content); i += 2 {
		if key := n.Content[i].Value; !slices.Contains(quarantineFields, key) {
			problems = append(problems, fmt.Sprintf("unknown field %q", key))
		}
	}
	q := Quarantine{Line: n.Line}
	if err := n.Decode(&q); err != nil {
		return Quarantine{}, append(problems, err.Error())
	}
	for i, value := range []string{q.Suite, q.Test, q.Issue, q.Expires} { // in quarantineFields' order
		if strings.TrimSpace(value) == "" {
			problems = append(problems, quarantineFields[i]+" is required")
		}
	}
	// decision: a test is one name, matched exactly, since the list is per
	// test: a known entry's name also covers name.t and name/..., so a Go
	// parent's would quarantine all its subtests. A name holding a glob's *,
	// ? or [ is refused rather than matched as itself; revisit if a test's
	// own name holds one.
	if strings.ContainsAny(q.Test, "*?[") {
		problems = append(problems, fmt.Sprintf("test %q is a pattern: name one test, as its results name it", q.Test))
	}
	if q.Issue != "" && !issueRE.MatchString(q.Issue) {
		problems = append(problems, fmt.Sprintf("issue %q is not written as \"#1234\"", q.Issue))
	}
	if _, err := time.Parse(time.DateOnly, q.Expires); q.Expires != "" && err != nil {
		problems = append(problems, fmt.Sprintf("expires %q is not a date, as 2026-12-31", q.Expires))
	}
	return q, problems
}

// Entry is what Grade grades the test's results by, after the suite's known
// file's entries: a known failure that grades a failure quarantined, and is
// intermittent, so a pass is reported, not failed.
//
// decision: it grades failures only, as a known-failure entry does: a crash
// or a hang of the code under test, which its reader grades, stays new, since
// a hang is a regression until shown otherwise, and a skip is graded as any
// skip is. Between it and a known entry matching the same test, Grade picks
// as between two known entries, the known file's first. Revisit if a flake
// that hangs needs quarantining.
func (q Quarantine) Entry() Entry {
	return Entry{Test: q.Test, Status: "fail", Issue: q.Issue, Intermittent: true, quarantined: true}
}

package known

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Entry is one entry of a suite's known file: a known failure or an expected
// skip, why, and whether it will be fixed.
type Entry struct {
	Test         string              `yaml:"test"`     // the test's name, or a pattern Match takes
	Status       string              `yaml:"status"`   // fail or skip
	Fix          string              `yaml:"fix"`      // planned or wontfix
	Category     string              `yaml:"category"` // bug, feature, env, proto or permanent
	Reason       string              `yaml:"reason"`
	Issue        string              `yaml:"issue,omitempty"`        // its issue, as "#N"; a planned entry names one
	Where        map[string][]string `yaml:"where,omitempty"`        // where it applies; everywhere when empty
	Intermittent bool                `yaml:"intermittent,omitempty"` // it fails on some runs only, so a pass isn't unexpected
	Line         int                 `yaml:"-"`                      // its line in the file
	quarantined  bool                // it comes from the quarantine list, so it grades a failure quarantined
}

// The values an entry's fields take.
var (
	statuses   = []string{"fail", "skip"}
	fixes      = []string{"planned", "wontfix"}
	categories = []string{"bug", "feature", "env", "proto", "permanent"}
	issueRE    = regexp.MustCompile(`^#[0-9]+$`)
)

// fields are the keys an entry may have.
var fields = []string{"test", "status", "fix", "category", "reason", "issue", "where", "intermittent"}

// Parse reads a known file, named name in its errors: a list of entries,
// each with a test, a status, a fix, a category and a reason, and an issue
// when its fix is planned. An empty file has no entries. Every problem is
// returned, joined, each as name:line: problem. Which keys where may hold
// depends on the suite, so its caller checks them.
func Parse(name string, data []byte) ([]Entry, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	list := doc.Content[0]
	if list.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s:%d: expected a list of entries", name, list.Line)
	}
	var entries []Entry
	var errs []error
	for _, n := range list.Content {
		e, problems := parseEntry(n)
		if len(problems) > 0 {
			errs = append(errs, fmt.Errorf("%s:%d: %s", name, n.Line, strings.Join(problems, "; ")))
			continue
		}
		entries = append(entries, e)
	}
	return entries, errors.Join(errs...)
}

// parseEntry reads one entry, and says what's wrong with it.
func parseEntry(n *yaml.Node) (Entry, []string) {
	if n.Kind != yaml.MappingNode {
		return Entry{}, []string{"an entry is a mapping"}
	}
	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	for i := 0; i < len(n.Content); i += 2 {
		if key := n.Content[i].Value; !slices.Contains(fields, key) {
			fail("unknown field %q", key)
		}
	}
	if len(problems) > 0 {
		return Entry{}, problems
	}
	e := Entry{Line: n.Line}
	if err := n.Decode(&e); err != nil {
		return Entry{}, []string{err.Error()}
	}
	oneOf := func(field, value string, values []string) {
		switch {
		case value == "":
			fail("%s is required: %s", field, strings.Join(values, " or "))
		case !slices.Contains(values, value):
			fail("%s %q is not %s", field, value, strings.Join(values, " or "))
		}
	}
	if e.Test == "" {
		fail("test is required")
	}
	oneOf("status", e.Status, statuses)
	oneOf("fix", e.Fix, fixes)
	oneOf("category", e.Category, categories)
	if strings.TrimSpace(e.Reason) == "" {
		fail("reason is required")
	}
	switch {
	case e.Fix == "planned" && e.Issue == "":
		fail("a planned entry names its issue, as issue: \"#1234\"")
	case e.Issue != "" && !issueRE.MatchString(e.Issue):
		fail("issue %q is not written as \"#1234\"", e.Issue)
	}
	for _, k := range slices.Sorted(maps.Keys(e.Where)) {
		if len(e.Where[k]) == 0 {
			fail("where %s lists no value", k)
		}
	}
	return e, problems
}

// applies reports whether the entry applies where a cell runs: at gives the
// cell's value for each key where may hold, and each key the entry names
// must have one of the values it lists. A key the cell has no value for, such
// as an axis of another option's matrix, has none of them.
//
// decision: such an entry doesn't apply, rather than applying everywhere,
// so an entry written for one option's matrix never excuses a result in a
// cell that has no such axis. Revisit if a file needs an entry for every
// unit that takes it whatever their axes.
func (e Entry) applies(at map[string]string) bool {
	for k, values := range e.Where {
		if !slices.Contains(values, at[k]) {
			return false
		}
	}
	return true
}

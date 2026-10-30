package journalv2

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	rfcs        = "../../docs/internals/rfcs/"
	conformance = "../../pkg/block/journalv2/conformance"
)

// traceEntry maps one RFC row to the conformance tests that check it, to the epic that
// owns it, or to both when the row is split between them.
type traceEntry struct {
	Tests  []string `yaml:"tests"`
	Owner  string   `yaml:"owner"`
	Reason string   `yaml:"reason"`
}

var (
	link      = regexp.MustCompile(`^\[[^\]]*\]\([^)]*\)\s*`)
	group     = regexp.MustCompile(`^#+ Group ([A-Z]) `)
	heading   = regexp.MustCompile(`^\*\*([^*]+)\*\*(:.*)?$`)
	boldItem  = regexp.MustCompile(`^- \*\*([^*]+)\*\*`)
	boldKind  = regexp.MustCompile(`^\*\*([^*]+)\.\*\* `)
	tableCell = regexp.MustCompile(`^\| ([^|]+) \|`)
)

// section returns the lines of the section whose heading starts with title, up to the
// next heading at the same depth or shallower.
func section(t *testing.T, file, title string) []string {
	t.Helper()
	f, err := os.Open(rfcs + file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []string
	depth := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := sc.Text()
		d := len(line) - len(strings.TrimLeft(line, "#"))
		if d > 0 && strings.HasPrefix(line[d:], " ") {
			if depth > 0 && d <= depth {
				break
			}
			if depth == 0 && strings.HasPrefix(line, title) {
				depth = d
				continue
			}
		}
		if depth > 0 {
			out = append(out, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if depth == 0 {
		t.Fatalf("%s: no section %q", file, title)
	}
	return out
}

// firstCells returns the first cell of every body row of the section's tables, with a
// leading section link stripped, keyed by the group heading above the table, if any.
func firstCells(lines []string) [][2]string {
	var out [][2]string
	g := ""
	for _, line := range lines {
		if m := group.FindStringSubmatch(line); m != nil {
			g = m[1]
		}
		m := tableCell.FindStringSubmatch(line)
		if m == nil || strings.HasPrefix(m[1], "---") {
			continue
		}
		out = append(out, [2]string{g, link.ReplaceAllString(m[1], "")})
	}
	return out
}

func key(parts ...string) string { return strings.Join(parts, " / ") }

func quote(s string) string { return `"` + s + `"` }

// rfcRows parses every row of the traceability scope straight from the RFC files.
func rfcRows(t *testing.T) []string {
	var rows []string
	tables := func(prefix, file, title, header string) {
		for _, c := range firstCells(section(t, file, title)) {
			if c[1] != header {
				rows = append(rows, key(prefix, c[0], quote(c[1])))
			}
		}
	}
	tables("rfc1 / 9.4", "rfc-1-journal.md", "### 9.4 ", "Requirement")
	tables("rfc28 / 4.2", "rfc-28-journal-format.md", "### 4.2 ", "Requirement")

	// Edge cases: one row per bold heading, and one for any bullets above the first.
	edges := func(prefix, file, title string) {
		seen := false
		for _, line := range section(t, file, title) {
			if m := heading.FindStringSubmatch(line); m != nil {
				rows = append(rows, key(prefix, quote(m[1])))
				seen = true
			} else if !seen && strings.HasPrefix(line, "- ") {
				rows = append(rows, key(prefix, "-"))
				seen = true
			}
		}
	}
	edges("rfc1 / 9.2", "rfc-1-journal.md", "### 9.2 ")
	edges("rfc28 / 4.1", "rfc-28-journal-format.md", "### 4.1 ")

	for _, line := range section(t, "rfc-1-journal.md", "### 9.7 ") {
		if m := boldItem.FindStringSubmatch(line); m != nil {
			rows = append(rows, key("rfc1 / 9.7", quote(m[1])))
		}
	}

	damage := section(t, "rfc-28-journal-format.md", "### 4.4 ")
	for _, line := range damage {
		if m := boldKind.FindStringSubmatch(line); m != nil {
			rows = append(rows, key("rfc28 / 4.4", quote(m[1])))
		}
	}
	for _, c := range firstCells(damage) {
		if c[1] != "Edit" {
			rows = append(rows, key("rfc28 / 4.4 / edit", quote(c[1])))
		}
	}

	// From these tables only the named rows are in scope; the rest are other components'.
	named := func(prefix, file, title, row string) {
		for _, c := range firstCells(section(t, file, title)) {
			if c[1] == row {
				rows = append(rows, key(prefix, quote(row)))
				return
			}
		}
		t.Errorf("%s: no row %q", file, row)
	}
	named("rfc1 / 9.1", "rfc-1-journal.md", "### 9.1 ", "Model-based")
	named("rfc0 / 1.4", "rfc-0-data-lifecycle.md", "### 1.4 ", "clock bound")
	named("rfc8 / 15", "rfc-8-engine.md", "## 15. ", "one clock")
	return rows
}

// suiteTests returns the name of every test function of the conformance package.
func suiteTests(t *testing.T) map[string]bool {
	files, err := filepath.Glob(filepath.Join(conformance, "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]bool{}
	for _, file := range files {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil &&
				strings.HasPrefix(fn.Name.Name, "Test") && fn.Name.Name != "TestMain" {
				tests[fn.Name.Name] = true
			}
		}
	}
	if len(tests) == 0 {
		t.Fatalf("no tests found in %s", filepath.Clean(conformance))
	}
	return tests
}

// TestTrace checks that every row the journal's conformance suite answers to maps to a test
// or an owner, that every entry names a row that exists and tests that exist, and that
// every test of the suite checks some row.
func TestTrace(t *testing.T) {
	raw, err := os.ReadFile("trace.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var trace map[string]traceEntry
	if err := yaml.Unmarshal(raw, &trace); err != nil {
		t.Fatal(err)
	}
	tests := suiteTests(t)

	inScope := map[string]bool{}
	for _, row := range rfcRows(t) {
		if inScope[row] {
			t.Errorf("row %s appears twice in the RFC", row)
		}
		inScope[row] = true
		if _, ok := trace[row]; !ok {
			t.Errorf("row %s has no entry in trace.yaml", row)
		}
	}

	mapped := map[string]bool{}
	for row, e := range trace {
		if !inScope[row] {
			t.Errorf("trace.yaml names row %s, which the RFCs no longer have", row)
		}
		if len(e.Tests) == 0 && e.Owner == "" {
			t.Errorf("row %s maps to neither a test nor an owner", row)
		}
		if e.Owner != "" && e.Reason == "" {
			t.Errorf("row %s names owner %q without a reason", row, e.Owner)
		}
		for _, name := range e.Tests {
			if !tests[name] {
				t.Errorf("row %s maps to %s, which the conformance package does not have", row, name)
			}
			mapped[name] = true
		}
	}

	var unmapped []string
	for name := range tests {
		if !mapped[name] {
			unmapped = append(unmapped, name)
		}
	}
	sort.Strings(unmapped)
	for _, name := range unmapped {
		t.Errorf("%s checks no row: map it in trace.yaml", name)
	}
}

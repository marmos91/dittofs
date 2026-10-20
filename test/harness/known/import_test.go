package known

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// list has a row of each kind the lists hold, as the lists lay them out.
const list = `# Known failures

| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| CSESS26 | bug | session validation | [#1234](https://github.com/marmos91/dittofs/issues/1234) |
| flock/* | feature | NLM not implemented | - |
| unlink/14.t | proto | silly rename | - |
| open/03.t | env | PATH_MAX with the mount's prefix | - |
| RPLY8 | suite | knfsd fails this too | - |
| smb2.oplock.batch22b | Break timeout | 36s where 35s is the ceiling | #5678 |
| smb2.charset.Testing | Character set | fails against Samba too | #42 |

| Status | Meaning |
|--------|---------|
| **Expected** | fix planned |

## Permanently Unimplementable (Out of Scope)

Prose may wrap onto a line starting with an issue, like
#99, which isn't a heading.

| Test Name | Category | Reason |
|-----------|----------|--------|
| smb2.twrp.listdir | Previous Versions / TWRP | needs shadow copies |

## Changelog

| Row | Verdict |
| --- | --- |
| ` + "`notify.double`" + ` | removed |
`

// Each row of a table of tests becomes a known failure, whose category says
// whether it will be fixed, and the rows of other tables are left out.
func TestImport(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "known.yaml")
	left, err := Import(write(t, filepath.Join(dir, "list.md"), list), file, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Status", "**Expected**", "Row", "`notify.double`"}; !slices.Equal(left, want) {
		t.Errorf("left out %q, want %q", left, want)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(file, data)
	if err != nil {
		t.Fatal(err)
	}
	for i := range got {
		got[i].Line = 0
	}
	fail := func(test, category, fix, reason, issue string) Entry {
		return Entry{Test: test, Status: "fail", Category: category, Fix: fix, Reason: reason, Issue: issue}
	}
	want := []Entry{
		fail("CSESS26", "bug", "planned", "session validation", "#1234"),
		fail("flock/*", "feature", "wontfix", "NLM not implemented", ""),
		fail("unlink/14.t", "proto", "wontfix", "silly rename", ""),
		fail("open/03.t", "env", "wontfix", "PATH_MAX with the mount's prefix", ""),
		fail("RPLY8", "proto", "wontfix", "knfsd fails this too", ""),
		fail("smb2.oplock.batch22b", "bug", "planned", "36s where 35s is the ceiling", "#5678"),
		fail("smb2.charset.Testing", "proto", "wontfix", "fails against Samba too", "#42"),
		fail("smb2.twrp.listdir", "permanent", "wontfix", "needs shadow copies", ""),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("entries\n%+v\nwant\n%+v", got, want)
	}
}

// A list whose rows don't make valid entries, such as a bug with no issue,
// leaves the known file as it was.
func TestImportRefusesInvalidEntries(t *testing.T) {
	dir := t.TempDir()
	const before = "- { test: a, status: fail, fix: wontfix, category: env, reason: r }\n"
	file := write(t, filepath.Join(dir, "known.yaml"), before)
	md := write(t, filepath.Join(dir, "list.md"), "| Test Name | Category | Reason | Issue |\n| b | bug | r | - |\n")
	if _, err := Import(md, file, nil); err == nil || !strings.Contains(err.Error(), "a planned entry names its issue") {
		t.Errorf("got %v, want the planned entry's missing issue", err)
	}
	if data, _ := os.ReadFile(file); string(data) != before {
		t.Errorf("the known file changed:\n%s", data)
	}
}

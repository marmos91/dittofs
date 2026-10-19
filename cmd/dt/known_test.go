package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A suite's lists per version are imported one by one, each with --where:
// an entry that differs only in where's values merges into one, and a list
// imported without --where adds its own entries. Rows outside a table of
// tests are reported.
func TestKnownImport(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "known.yaml")
	md := func(name, body string) string {
		path := filepath.Join(dir, name)
		writeFile(t, path, "| Test Name | Category | Reason | Issue |\n|---|---|---|---|\n"+body)
		return path
	}
	v3 := md("v3.md", "| x | proto | same | - |\n| z | feature | on 3 | - |\n\n| Status | Meaning |\n|---|---|\n| **Expected** | fix planned |\n")
	v4 := md("v4.md", "| x | proto | same | - |\n| z | feature | on 4 | - |\n")
	all := md("all.md", "| x | proto | same | - |\n")

	out, err := dt(t, "known", "import", v3, file, "--where", "nfs=3")
	if err != nil {
		t.Fatal(err)
	}
	if want := "left out \"Status\": not in a table of tests\nleft out \"**Expected**\": not in a table of tests\n"; out != want {
		t.Errorf("output %q, want %q", out, want)
	}
	for _, args := range [][]string{{v4, file, "--where", "nfs=4,4.1"}, {all, file}} {
		if _, err := dt(t, append([]string{"known", "import"}, args...)...); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	entry := func(test, category, reason, where string) string {
		return "- test: " + test + "\n  status: fail\n  fix: wontfix\n  category: " + category + "\n  reason: " + reason + "\n" + where
	}
	want := entry("x", "proto", "same", "  where: {nfs: [\"3\", \"4\", \"4.1\"]}\n") +
		entry("z", "feature", "on 3", "  where: {nfs: [\"3\"]}\n") +
		entry("z", "feature", "on 4", "  where: {nfs: [\"4\", \"4.1\"]}\n") +
		entry("x", "proto", "same", "")
	if string(data) != want {
		t.Errorf("known file\n%s\nwant\n%s", data, want)
	}

	if _, err := dt(t, "known", "import", v3, file, "--where", "nfs"); err == nil || !strings.Contains(err.Error(), "expected AXIS=V") {
		t.Errorf("--where nfs: got %v", err)
	}
}

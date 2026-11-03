package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// docsRepo writes a registry, doc and RFC into a new checkout and runs dt
// there.
func docsRepo(t *testing.T, doc string) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "test/suites.yaml"), `version: 1
defaults: { format: exit, tier: merge, timeout: { test: 30m, cell: 60m } }
groups: { g: [a, b] }
suites:
  a:
    cmd: "run {nfs}"
    matrix: { profile: [memory, badger-s3], nfs: ["3", "4"] }
    fixture: { profile: "{profile}", nfs: export }
    known: a.yaml
    tier: pr
    options:
      o: { cmd: "true", matrix: {}, fixture: { profile: memory, smb: true }, known: o.yaml, tier: merge }
  b: { cmd: "true" }
`)
	writeFile(t, filepath.Join(root, "a.yaml"), "")
	writeFile(t, filepath.Join(root, "o.yaml"), "")
	writeFile(t, filepath.Join(root, suitesDoc), doc)
	writeFile(t, filepath.Join(root, rfcDir, "rfc-1-x.md"), "# RFC 1\n\n**Suites.** `a`, its option\n`a/o` and the group `g`.\n")
	t.Chdir(root)
	return filepath.Join(root, suitesDoc)
}

// Each RFC needs one suites paragraph, and each code span in it, on any of its
// lines, names a suite, an option or a group. Both dt docs and its check fail
// otherwise, the write still writing the table, and the index isn't an RFC.
func TestDocsRFCs(t *testing.T) {
	for _, c := range []struct{ rfc, want string }{
		{"# RFC 2\n\nNo suites named.\n", rfcDir + "/rfc-2-x.md: no paragraph starting **Suites.** names the suites"},
		{"**Suites.** `a` and\n`c`, as `g` does.\n", rfcDir + "/rfc-2-x.md:2: `c` is no suite, group or option"},
		{"**Suites.** `a`.\n\n**Suites.** `b`.\n", rfcDir + "/rfc-2-x.md: 2 paragraphs start **Suites.**"},
	} {
		path := docsRepo(t, suitesBegin+"\n"+suitesEnd+"\n")
		writeFile(t, filepath.Join(rfcDir, "rfc-index.md"), "no paragraph\n")
		writeFile(t, filepath.Join(rfcDir, "rfc-2-x.md"), c.rfc)
		for _, args := range [][]string{nil, {"--check"}} {
			out, err := docs(args...)
			if status := exitStatus(0); !errors.As(err, &status) || status != 1 || !strings.Contains(out, c.want) {
				t.Errorf("dt docs %v with %q: %v, %q; want exit status 1 and %q", args, c.rfc, err, out, c.want)
			}
		}
		if got, _ := os.ReadFile(path); !strings.Contains(string(got), "| `a` |") {
			t.Errorf("with %q, dt docs didn't write the table", c.rfc)
		}
		writeFile(t, filepath.Join(rfcDir, "rfc-2-x.md"), "**Suites.** `b`.\n")
		if out, err := docs("--check"); err != nil {
			t.Errorf("check with every RFC naming the registry's suites: %v, %q", err, out)
		}
	}
}

func docs(args ...string) (string, error) {
	var out bytes.Buffer
	root := newRoot(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"docs"}, args...))
	err := root.Execute()
	return out.String(), err
}

// dt docs --check fails on a stale table without touching the doc and says how
// to fix it; dt docs writes a row for each suite and option with a known file
// between the markers only; then the check passes.
func TestDocs(t *testing.T) {
	stale := "intro\n" + suitesBegin + "\nold\n" + suitesEnd + "\noutro\n"
	path := docsRepo(t, stale)

	out, err := docs("--check")
	if status := exitStatus(0); !errors.As(err, &status) || status != 1 || !strings.Contains(out, "run test/harness/bin/dt docs") {
		t.Errorf("check of a stale doc: %v, %q; want exit status 1 and the fix", err, out)
	}
	if got, _ := os.ReadFile(path); string(got) != stale {
		t.Error("the check wrote the doc")
	}

	if _, err := docs(); err != nil {
		t.Fatal(err)
	}
	want := "intro\n" + suitesBegin + `
<!-- Generated from test/suites.yaml by test/harness/bin/dt docs. Do not edit by hand. -->

| Suite | Protocol | Profiles | Variants | Tier | Known failures and skips |
|---|---|---|---|---|---|
| ` + "`a` | NFS | `memory`, `badger-s3` | nfs: `3`, `4` | `pr` | [`a.yaml`](../../a.yaml) |" + `
| ` + "`a/o` | SMB | `memory` | none | `merge` | [`o.yaml`](../../o.yaml) |" + `

From [` + "`test/suites.yaml`" + `](../../test/suites.yaml): its suites graded against a known file, and their options.
Each runs in its tier and the wider ones, ` + "`pr` then `merge` then `nightly`; a `manual` one only when asked.\n" + suitesEnd + "\noutro\n"
	if got, _ := os.ReadFile(path); string(got) != want {
		t.Errorf("written doc:\n%s\nwant:\n%s", got, want)
	}

	if _, err := docs("--check"); err != nil {
		t.Errorf("check after the write: %v", err)
	}
}

// A doc without the begin marker, or without an end marker after it, is an
// error and is left as it is, so a lost marker can't pass the check.
func TestDocsNeedsBothMarkers(t *testing.T) {
	for _, doc := range []string{"no table\n", suitesEnd + "\n" + suitesBegin + "\nno end after it\n"} {
		path := docsRepo(t, doc)
		if _, err := docs("--check"); err == nil {
			t.Errorf("%q: the check passed", doc)
		}
		if _, err := docs(); err == nil {
			t.Errorf("%q: the write passed", doc)
		}
		if got, _ := os.ReadFile(path); string(got) != doc {
			t.Errorf("%q was rewritten", doc)
		}
	}
}

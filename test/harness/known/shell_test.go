package known

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// shellRows loads the tables at paths, in order, through kf_load in bash, and
// returns its rows as rows does.
func shellRows(t *testing.T, paths ...string) []string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash isn't on the PATH, so kf_load can't run")
	}
	script := `source "$1"; shift
for f; do kf_load "$f"; done
for name in "${!KF_REASON[@]}"; do printf '%s\t%s\0' "$name" "${KF_REASON[$name]}"; done`
	args := []string{"-c", script, "bash", "../../common/known-failures.sh"}
	for _, p := range paths {
		args = append(args, filepath.ToSlash(p))
	}
	cmd := exec.Command("bash", args...)
	// A git hook running these tests exports GIT_DIR and the like.
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "GIT_") })
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil || stderr.Len() > 0 {
		t.Fatalf("kf_load %q: %v\n%s", paths, err, stderr.String())
	}
	got := strings.Split(string(out), "\x00")
	got = got[:len(got)-1]
	slices.Sort(got)
	return got
}

// realLists returns every KNOWN_FAILURES*.md in the checkout, by directory,
// outside hidden directories.
func realLists(t *testing.T) map[string][]string {
	t.Helper()
	root := "../../.."
	lists := map[string][]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && path != root && strings.HasPrefix(d.Name(), "."):
			return filepath.SkipDir
		case !d.IsDir() && strings.HasPrefix(d.Name(), "KNOWN_FAILURES") && strings.HasSuffix(d.Name(), ".md"):
			lists[filepath.Dir(path)] = append(lists[filepath.Dir(path)], path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lists) == 0 {
		t.Fatal("no KNOWN_FAILURES*.md in the checkout")
	}
	return lists
}

// Every real list loads the same rows through kf_load and Load, alone and
// after the lists before it in its directory, as the graders that take a
// base list and an overlay load them.
func TestShellAgreesOnRealLists(t *testing.T) {
	n := 0
	for _, paths := range realLists(t) {
		for i, path := range paths {
			var alone, merged List
			for _, p := range paths[:i+1] {
				if err := merged.Load(p); err != nil {
					t.Fatal(err)
				}
			}
			if err := alone.Load(path); err != nil {
				t.Fatal(err)
			}
			if d := differ(rows(&alone), shellRows(t, path)); d != "" {
				t.Errorf("%s, Load against kf_load:%s", path, d)
			}
			if d := differ(rows(&merged), shellRows(t, paths[:i+1]...)); d != "" {
				t.Errorf("%q, Load against kf_load:%s", paths[:i+1], d)
			}
			n++
		}
	}
	t.Logf("%d lists agree", n)
}

// The tables the test data can't hold load the same through kf_load.
func TestShellLineEnds(t *testing.T) {
	for _, c := range lineEnds {
		path := write(t, filepath.Join(t.TempDir(), "list.md"), c.table)
		if d := differ(shellRows(t, path), c.want); d != "" {
			t.Errorf("%s:%s", c.name, d)
		}
	}
}

package run

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A fixture that mounts the share gives {mount} to its cell's commands, and
// they can run there; where dt doesn't mount NFS yet, the cell doesn't run.
func TestFixtureMount(t *testing.T) {
	// What meets the root and nfs-client needs the mount implies.
	met := t.TempDir()
	for _, name := range []string{"sudo", "mount.nfs", "mount_nfs"} {
		if err := os.WriteFile(filepath.Join(met, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", met+string(os.PathListSeparator)+os.Getenv("PATH"))
	server := &fakeServer{mount: t.TempDir()}
	if err := os.WriteFile(filepath.Join(server.mount, "marker"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	fakeFixture(t, func(context.Context) (fixtureServer, error) { return server, nil })
	root, r := repo(t, `suites:
  m:
    cmd: ["test -f {mount}/marker", "test -f marker"]
    dir: "{mount}"
    fixture: { profile: memory, nfs: "4.1" }
`, withBins(map[string]string{"go.mod": goModule["go.mod"]}))
	rep, out := runTarget(t, root, r, "m", false)
	want, note := Passed, ""
	if runtime.GOOS != "linux" {
		want, note = NotRun, "dt mounts NFS only on Linux yet"
	}
	if o := rep.Outcomes[0]; o.Result != want || o.Note != note {
		t.Errorf("outcome %+v\n%s", o, out)
	}
}

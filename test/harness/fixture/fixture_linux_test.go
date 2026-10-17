package fixture

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/harness/needs"
	"github.com/marmos91/dittofs/test/harness/registry"
)

// When dt ends without stopping its fixture, as when it panics or is killed,
// dfs ends with it.
func TestDfsEndsWithDT(t *testing.T) {
	if bin := os.Getenv("DT_FIXTURE_TEST_BIN"); bin != "" {
		// The dt the test kills: its fixture never answers.
		_, _ = Start(context.Background(), registry.Fixture{Profile: "memory"}, bin, os.Getenv("DT_FIXTURE_TEST_OUT"), io.Discard)
		return
	}
	bin, starts := fakeDFS(t)
	dt := exec.Command(os.Args[0], "-test.run=^TestDfsEndsWithDT$")
	dt.Env = append(os.Environ(), "DT_FIXTURE_TEST_BIN="+bin, "DT_FIXTURE_TEST_OUT="+t.TempDir())
	if err := dt.Start(); err != nil {
		t.Fatal(err)
	}
	runs := started(starts)
	_ = dt.Process.Kill()
	_ = dt.Wait()
	if len(runs) != 1 {
		t.Fatalf("dfs started %d times", len(runs))
	}
	// The fake's own child isn't the server's, so the test ends it.
	pid, _ := strconv.Atoi(runs[0][0])
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	if !ended(runs[0][0]) {
		t.Errorf("dfs %s outlived the dt that started it", runs[0][0])
	}
}

// Where root and the NFS client are there, a real dfs's share is mounted at
// {mount} at each NFS version, with pjdfstest's options, and answers there;
// Stop unmounts it and removes {mount}.
func TestRealMount(t *testing.T) {
	for _, n := range []string{"root", "nfs-client"} {
		if err := needs.Check("", n); err != nil {
			t.Skipf("%s: %v", n, err)
		}
	}
	bin := realBin(t)
	for _, v := range []string{"3", "4", "4.1"} {
		t.Run(v, func(t *testing.T) {
			out := t.TempDir()
			var w bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			f := registry.Fixture{Profile: "memory", NFS: registry.NFS{Version: v, Options: "noac,sync,lookupcache=none"}, Squash: "none", Users: []int{65534}}
			s, err := Start(ctx, f, bin, out, &w)
			if err != nil {
				log, _ := os.ReadFile(filepath.Join(out, "dfs.log"))
				t.Fatalf("Start: %v\n%s\ndfs.log:\n%s", err, w.String(), log)
			}
			mount := s.Placeholders()["mount"]
			if _, err := os.ReadDir(mount); err != nil {
				t.Errorf("reading {mount}: %v", err)
			}
			// A suite's users reach the share through {mount}: the
			// directory under the mount is private to the user dt runs as.
			nobody := exec.Command("setpriv", "--reuid=65534", "--regid=65534", "--clear-groups", "ls", mount)
			if out, err := nobody.CombinedOutput(); err != nil {
				t.Errorf("a user other than root lists {mount}: %v\n%s", err, out)
			}
			if err := s.Stop(); err != nil {
				t.Errorf("Stop: %v", err)
			}
			if _, mounted, _ := mountedAt(mount); mounted {
				t.Errorf("%s is still mounted", mount)
			}
			if _, err := os.Stat(mount); !os.IsNotExist(err) {
				t.Errorf("{mount} stays: %v\n%s", err, w.String())
			}
		})
	}
}

package fixture

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"

	"github.com/marmos91/dittofs/test/harness/registry"
)

// When dt ends without stopping its fixture, as when it panics or is killed,
// dfs ends with it.
func TestDfsEndsWithDT(t *testing.T) {
	if bin := os.Getenv("DT_FIXTURE_TEST_BIN"); bin != "" {
		// The dt the test kills: its fixture never answers.
		_, _ = Start(context.Background(), registry.Fixture{Profile: "memory"}, nil, bin, os.Getenv("DT_FIXTURE_TEST_OUT"), io.Discard)
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

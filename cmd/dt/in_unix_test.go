//go:build unix

package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// A SIGTERM sent to dt alone reaches dtc, and dt gives back dtc's status
// rather than waiting for the whole container run.
func TestDtcPassesSIGTERMOn(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "test", "harness", "bin")
	writeFile(t, filepath.Join(bin, "dtc"), `#!/bin/sh
trap 'exit 1' TERM
: > "$(dirname "$0")/started"
i=0
while [ "$i" -lt 100 ]; do sleep 0.1; i=$((i + 1)); done
`)
	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	done := make(chan error, 1)
	go func() { done <- dtc(cmd, root, nil) }()
	for i := 0; ; i++ {
		if _, err := os.Stat(filepath.Join(bin, "started")); err == nil {
			break
		}
		if i == 100 {
			t.Fatal("dtc didn't start")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		var status exitStatus
		if !errors.As(err, &status) || status != 1 {
			t.Errorf("dtc after a SIGTERM: %v, want its exit status 1", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dt kept waiting for dtc after a SIGTERM")
	}
}

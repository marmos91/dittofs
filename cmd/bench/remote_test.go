package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/bench/remote"
)

type fakeStack struct{ outs map[string]string }

func (f fakeStack) Outputs(_ context.Context, _ string) (map[string]string, error) {
	return f.outs, nil
}

// TestRemoteCmd_DryRun drives the `remote` command in --dry-run with a fake
// Pulumi reader: no host is touched, but the plan (public SSH target, private
// mount, scp + bench command, fetch) is printed.
func TestRemoteCmd_DryRun(t *testing.T) {
	orig := newStackReader
	defer func() { newStackReader = orig }()
	newStackReader = func() remote.StackReader {
		return fakeStack{outs: map[string]string{
			remote.OutputServerIP:        "203.0.113.5",
			remote.OutputServerPrivateIP: "10.0.0.7",
		}}
	}

	// Reset the relevant flags to a known state for the test.
	remStack, remUser, remPrivateIP = "bench", "root", ""
	remBinary, remManifest = "", ""
	remRemoteBin = "/usr/local/bin/dfsbench"
	remRemoteOut, remLocalOut = "/tmp/r.json", "out.json"
	remDryRun = true
	defer func() { remDryRun = false }()

	var buf bytes.Buffer
	remoteCmd.SetOut(&buf)
	if err := runRemote(remoteCmd, nil); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"DRY RUN",
		"root@203.0.113.5",  // SSH uses the public IP
		"10.0.0.7",          // mount uses the private IP
		"orchestrate --out", // the planned remote command
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
	// The public IP must NOT be presented as the mount address.
	if strings.Contains(out, "bench mount (private):  203.0.113.5") {
		t.Error("public IP used as mount address")
	}
}

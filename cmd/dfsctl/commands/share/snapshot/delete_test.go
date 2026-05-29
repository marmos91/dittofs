package snapshot

import (
	"bytes"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

func resetDeleteFlags() {
	deleteYes = false
}

func TestDelete_YesFlagSkipsPromptAndDeletes(t *testing.T) {
	resetDeleteFlags()
	deleteYes = true
	fc := &fakeClient{snapshots: map[string]*apiclient.Snapshot{
		"snap-1": {ID: "snap-1"},
	}}
	withFakeClient(t, fc)

	// Discard stdout while running.
	prev := osStdout()
	_, w := setStdout()
	defer restoreStdout(prev)

	if err := runDelete(deleteCmd, []string{"/a", "snap-1"}); err != nil {
		t.Fatalf("runDelete: %v", err)
	}
	_ = w.Close()

	if len(fc.deleteCalls) != 1 || fc.deleteCalls[0] != "snap-1" {
		t.Errorf("DeleteSnapshot not called with snap-1: %v", fc.deleteCalls)
	}
}

func TestDelete_NoAnswerAborts(t *testing.T) {
	resetDeleteFlags()
	// Set ConfirmInput to "n\n".
	var outBuf bytes.Buffer
	origIn, origOut := cmdutil.ConfirmInput, cmdutil.ConfirmOutput
	cmdutil.ConfirmInput, cmdutil.ConfirmOutput = strings.NewReader("n\n"), &outBuf
	defer func() { cmdutil.ConfirmInput, cmdutil.ConfirmOutput = origIn, origOut }()

	fc := &fakeClient{snapshots: map[string]*apiclient.Snapshot{
		"snap-1": {ID: "snap-1"},
	}}
	withFakeClient(t, fc)

	prev := osStdout()
	_, w := setStdout()
	defer restoreStdout(prev)

	if err := runDelete(deleteCmd, []string{"/a", "snap-1"}); err != nil {
		t.Fatalf("runDelete: %v", err)
	}
	_ = w.Close()

	if len(fc.deleteCalls) != 0 {
		t.Errorf("DeleteSnapshot must not be called on abort; got %v", fc.deleteCalls)
	}
}

func TestDelete_YAnswerConfirms(t *testing.T) {
	resetDeleteFlags()
	var outBuf bytes.Buffer
	origIn, origOut := cmdutil.ConfirmInput, cmdutil.ConfirmOutput
	cmdutil.ConfirmInput, cmdutil.ConfirmOutput = strings.NewReader("y\n"), &outBuf
	defer func() { cmdutil.ConfirmInput, cmdutil.ConfirmOutput = origIn, origOut }()

	fc := &fakeClient{snapshots: map[string]*apiclient.Snapshot{
		"snap-1": {ID: "snap-1"},
	}}
	withFakeClient(t, fc)

	prev := osStdout()
	_, w := setStdout()
	defer restoreStdout(prev)

	if err := runDelete(deleteCmd, []string{"/a", "snap-1"}); err != nil {
		t.Fatalf("runDelete: %v", err)
	}
	_ = w.Close()

	if len(fc.deleteCalls) != 1 || fc.deleteCalls[0] != "snap-1" {
		t.Errorf("DeleteSnapshot should have been called once; got %v", fc.deleteCalls)
	}
}

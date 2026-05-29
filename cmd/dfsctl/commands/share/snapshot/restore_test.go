package snapshot

import (
	"bytes"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

func resetRestoreFlags() {
	restoreYes = false
	restoreForce = false
}

// captureStderr swaps os.Stderr for a pipe; returns the reader and a
// restore func.
func captureStderr() (read func() string, restore func()) {
	prev := osStderr()
	r, w, _ := osPipe()
	setStderr(w)
	return func() string {
			_ = w.Close()
			var buf bytes.Buffer
			_, _ = buf.ReadFrom(r)
			return buf.String()
		}, func() {
			restoreStderr(prev)
		}
}

func TestRestore_RefusesEnabledShare(t *testing.T) {
	resetRestoreFlags()
	restoreYes = true
	fc := &fakeClient{
		share:     &apiclient.Share{Name: "/archive", Enabled: true},
		snapshots: map[string]*apiclient.Snapshot{"snap-1": {ID: "snap-1"}},
	}
	withFakeClient(t, fc)

	read, restore := captureStderr()
	defer restore()

	err := runRestore(restoreCmd, []string{"/archive", "snap-1"})
	if err == nil {
		t.Fatal("expected error on enabled share")
	}
	stderr := read()
	const want = "share /archive is enabled; run 'dfsctl share disable /archive' first"
	if !strings.Contains(stderr, want) {
		t.Errorf("stderr missing hint: %q\ngot: %q", want, stderr)
	}
	if fc.restoreReq != nil {
		t.Errorf("RestoreSnapshot must not be called on enabled share")
	}
}

func TestRestore_DisabledShareYesFlag_PrintsSafetySnap(t *testing.T) {
	resetRestoreFlags()
	restoreYes = true
	fc := &fakeClient{
		share:     &apiclient.Share{Name: "/archive", Enabled: false},
		snapshots: map[string]*apiclient.Snapshot{"snap-1": {ID: "snap-1"}},
		restoreResp: &apiclient.RestoreSnapshotResponse{
			SnapshotID: "snap-1", Share: "/archive", SafetySnapshotID: "safety-abc",
		},
	}
	withFakeClient(t, fc)

	prev := osStdout()
	r, w := setStdout()
	defer restoreStdout(prev)

	if err := runRestore(restoreCmd, []string{"/archive", "snap-1"}); err != nil {
		t.Fatalf("runRestore: %v", err)
	}
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	out := buf.String()

	if !strings.Contains(out, "Safety snap: safety-abc") {
		t.Errorf("expected safety snap line in output; got: %s", out)
	}
	if fc.restoreReq == nil {
		t.Fatal("RestoreSnapshot was not invoked")
	}
}

func TestRestore_NoSafetySnapID_OmitsLine(t *testing.T) {
	resetRestoreFlags()
	restoreYes = true
	fc := &fakeClient{
		share:     &apiclient.Share{Name: "/archive", Enabled: false},
		snapshots: map[string]*apiclient.Snapshot{"snap-1": {ID: "snap-1"}},
		restoreResp: &apiclient.RestoreSnapshotResponse{
			SnapshotID: "snap-1", Share: "/archive", SafetySnapshotID: "",
		},
	}
	withFakeClient(t, fc)

	prev := osStdout()
	r, w := setStdout()
	defer restoreStdout(prev)

	if err := runRestore(restoreCmd, []string{"/archive", "snap-1"}); err != nil {
		t.Fatalf("runRestore: %v", err)
	}
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	out := buf.String()

	if strings.Contains(out, "Safety snap") {
		t.Errorf("safety snap line must be omitted when ID empty; got: %s", out)
	}
}

func TestRestore_PreconditionFailedHint(t *testing.T) {
	resetRestoreFlags()
	restoreYes = true
	fc := &fakeClient{
		share:      &apiclient.Share{Name: "/archive", Enabled: false},
		snapshots:  map[string]*apiclient.Snapshot{"snap-1": {ID: "snap-1"}},
		restoreErr: &apiclient.APIError{Code: "PRECONDITION_FAILED", Message: "not durable", StatusCode: 412},
	}
	withFakeClient(t, fc)

	read, restore := captureStderr()
	defer restore()

	err := runRestore(restoreCmd, []string{"/archive", "snap-1"})
	if err == nil {
		t.Fatal("expected error on 412 without --force")
	}
	stderr := read()
	if !strings.Contains(stderr, "--force") {
		t.Errorf("stderr must suggest --force; got: %s", stderr)
	}
}

func TestRestore_ForceMapsToAllowNonDurable(t *testing.T) {
	resetRestoreFlags()
	restoreYes = true
	restoreForce = true
	fc := &fakeClient{
		share:     &apiclient.Share{Name: "/archive", Enabled: false},
		snapshots: map[string]*apiclient.Snapshot{"snap-1": {ID: "snap-1"}},
		restoreResp: &apiclient.RestoreSnapshotResponse{
			SnapshotID: "snap-1", Share: "/archive", SafetySnapshotID: "safety-abc",
		},
	}
	withFakeClient(t, fc)

	prev := osStdout()
	_, w := setStdout()
	defer restoreStdout(prev)

	if err := runRestore(restoreCmd, []string{"/archive", "snap-1"}); err != nil {
		t.Fatalf("runRestore: %v", err)
	}
	_ = w.Close()

	if fc.restoreReq == nil || !fc.restoreReq.AllowNonDurable {
		t.Errorf("--force did not map to AllowNonDurable=true; got %+v", fc.restoreReq)
	}
}

func TestRestore_DisabledShareNAnswerAborts(t *testing.T) {
	resetRestoreFlags()
	var outBuf bytes.Buffer
	origIn, origOut := cmdutil.ConfirmInput, cmdutil.ConfirmOutput
	cmdutil.ConfirmInput, cmdutil.ConfirmOutput = strings.NewReader("n\n"), &outBuf
	defer func() { cmdutil.ConfirmInput, cmdutil.ConfirmOutput = origIn, origOut }()

	fc := &fakeClient{
		share:     &apiclient.Share{Name: "/archive", Enabled: false},
		snapshots: map[string]*apiclient.Snapshot{"snap-1": {ID: "snap-1"}},
	}
	withFakeClient(t, fc)

	prev := osStdout()
	_, w := setStdout()
	defer restoreStdout(prev)

	if err := runRestore(restoreCmd, []string{"/archive", "snap-1"}); err != nil {
		t.Fatalf("runRestore: %v", err)
	}
	_ = w.Close()

	if fc.restoreReq != nil {
		t.Errorf("RestoreSnapshot must not be called on 'n' answer")
	}
}

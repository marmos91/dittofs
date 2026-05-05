package blockstore

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestBlockstoreCmd_HelpListsMigrate asserts that the parent `blockstore`
// command lists `migrate` in its Available Commands. (Task 1 behavior 1.)
func TestBlockstoreCmd_HelpListsMigrate(t *testing.T) {
	cmd := Cmd
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("blockstore --help: unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "migrate") {
		t.Fatalf("blockstore --help did not list 'migrate' subcommand; got:\n%s", out)
	}
}

// TestMigrateCmd_HelpListsAllFlags asserts that the migrate command lists
// every flag the plan declares. (Task 1 behavior 2.)
//
// The flag introspection is done directly via cmd.Flags().Lookup rather
// than rendering --help: cobra's Help() method sends output through the
// parent command's stream when available, which makes interleaving with
// other --help tests fragile.
func TestMigrateCmd_HelpListsAllFlags(t *testing.T) {
	for _, flag := range []string{"share", "dry-run", "parallel", "bandwidth-limit", "state-dir"} {
		if migrateCmd.Flags().Lookup(flag) == nil {
			t.Errorf("migrate command missing flag %q", flag)
		}
	}
	// The --share flag must be marked required (D-A5: every migration
	// targets exactly one share).
	shareFlag := migrateCmd.Flags().Lookup("share")
	if shareFlag == nil {
		t.Fatal("expected --share flag to exist")
	}
	required := shareFlag.Annotations[cobra.BashCompOneRequiredFlag]
	if len(required) == 0 || required[0] != "true" {
		t.Errorf("expected --share flag to be marked required; annotations = %v", shareFlag.Annotations)
	}
}

// stubProbe is a daemonProbe whose answer is fixed at construction.
type stubProbe struct {
	running bool
	err     error
}

func (s *stubProbe) IsDaemonRunning(ctx context.Context) (bool, error) {
	return s.running, s.err
}

// TestEnsureDaemonOffline_DaemonActive asserts ErrDaemonActive when the
// stub probe says running. (Task 1 behavior 3a.)
func TestEnsureDaemonOffline_DaemonActive(t *testing.T) {
	restore := setDaemonProbeForTest(&stubProbe{running: true})
	defer restore()

	err := ensureDaemonOffline(context.Background(), "myshare")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrDaemonActive) {
		t.Fatalf("expected ErrDaemonActive, got %v", err)
	}
	if !strings.Contains(err.Error(), "myshare") {
		t.Errorf("expected error to mention share name; got %q", err.Error())
	}
}

// TestEnsureDaemonOffline_DaemonStopped asserts nil when the probe says
// not running. (Task 1 behavior 3b.)
func TestEnsureDaemonOffline_DaemonStopped(t *testing.T) {
	restore := setDaemonProbeForTest(&stubProbe{running: false})
	defer restore()

	if err := ensureDaemonOffline(context.Background(), "myshare"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestRunMigrate_DaemonActiveExitsCleanly verifies that runMigrate
// surfaces the offline-probe failure with a clear message. (Task 1
// behavior 4.)
func TestRunMigrate_DaemonActiveExitsCleanly(t *testing.T) {
	restore := setDaemonProbeForTest(&stubProbe{running: true})
	defer restore()

	cmd := migrateCmd
	cmd.SetContext(context.Background())
	if err := cmd.Flags().Set("share", "foo"); err != nil {
		t.Fatalf("set --share: %v", err)
	}
	defer func() { _ = cmd.Flags().Set("share", "") }()

	err := runMigrate(cmd, nil)
	if err == nil {
		t.Fatal("expected runMigrate to error when daemon is active")
	}
	if !errors.Is(err, ErrDaemonActive) {
		t.Fatalf("expected wrapped ErrDaemonActive, got %v", err)
	}
	if !strings.Contains(err.Error(), `"foo"`) {
		t.Errorf("expected share name %q in error; got %q", "foo", err.Error())
	}
}

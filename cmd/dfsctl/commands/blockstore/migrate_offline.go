package blockstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/marmos91/dittofs/pkg/config"
)

// ErrDaemonActive is returned when the offline migration tool detects
// that the dfs daemon is running — the migration cannot proceed because
// concurrent writers would race against the per-file metadata txn (D-A5).
var ErrDaemonActive = errors.New("daemon is active for share — stop it before migration (D-A5)")

// daemonProbe abstracts the daemon-running check so tests can stub a
// known answer without touching the real PID file or process table.
type daemonProbe interface {
	IsDaemonRunning(ctx context.Context) (bool, error)
}

// pidFileProbe is the production probe. It reads the daemon's PID file
// from the configured state directory (matches cmd/dfs/commands/util.go
// GetDefaultPidFile semantics) and signals 0 to verify the process is
// alive — the same mechanism cmd/dfs/commands/daemon_unix.go uses.
type pidFileProbe struct {
	pidFile string
}

func newPidFileProbe() *pidFileProbe {
	return &pidFileProbe{
		pidFile: filepath.Join(config.GetStateDir(), "dittofs.pid"),
	}
}

// IsDaemonRunning returns true when a live PID is recorded in the daemon
// PID file. Stale or missing PID files yield (false, nil): the absence
// of evidence is treated as evidence of absence — fail-open here is the
// wrong default given D-A5, but pidfile absence is the documented
// "daemon stopped" signal in cmd/dfs/commands/daemon_unix.go.
func (p *pidFileProbe) IsDaemonRunning(ctx context.Context) (bool, error) {
	data, err := os.ReadFile(p.pidFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read pid file %q: %w", p.pidFile, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return false, nil
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, nil
	}
	if err := process.Signal(syscall.Signal(0)); err != nil {
		return false, nil
	}
	return true, nil
}

// activeProbe is package-level so tests can swap it via a setter.
// Production use leaves it nil; ensureDaemonOffline lazy-creates a
// pidFileProbe in that case.
var activeProbe daemonProbe

// setDaemonProbeForTest replaces the package-level probe; tests use it
// to stub the result. Returns a restore func to defer.
func setDaemonProbeForTest(p daemonProbe) func() {
	prev := activeProbe
	activeProbe = p
	return func() { activeProbe = prev }
}

// ensureDaemonOffline returns nil if the dfs daemon owning shareName is
// not running, or ErrDaemonActive when it is (D-A5). The shareName arg
// is not currently used to discriminate per-share — there's only one
// daemon process per host — but it's threaded through for forensic
// logging and forward-compat with multi-daemon hosts.
func ensureDaemonOffline(ctx context.Context, shareName string) error {
	probe := activeProbe
	if probe == nil {
		probe = newPidFileProbe()
	}
	running, err := probe.IsDaemonRunning(ctx)
	if err != nil {
		return fmt.Errorf("daemon status probe: %w", err)
	}
	if running {
		return fmt.Errorf("daemon for share %q is active — stop it before migration: %w", shareName, ErrDaemonActive)
	}
	return nil
}

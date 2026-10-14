// Package needs tells which of a suite's needs this machine meets
// (docs/internals/rfc-test-harness.md, section 4.5).
package needs

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// platforms maps a platform need to Go's name for it.
var platforms = map[string]string{"linux": "linux", "windows": "windows", "macos": "darwin"}

// tools are the needs a program on the PATH meets, per operating system.
var tools = map[string]map[string]string{
	"nfs-client":     {"linux": "mount.nfs", "darwin": "mount_nfs"},
	"smb-client":     {"linux": "mount.cifs", "darwin": "mount_smbfs", "windows": "net"},
	"nfs4-acl-tools": {"linux": "nfs4_getfacl"},
	"kerberos":       {"linux": "kinit", "darwin": "kinit"},
	"dm-flakey":      {"linux": "dmsetup"},
	"knfsd":          {"linux": "exportfs"},
}

// Unmet returns each need the machine doesn't meet, with the reason.
func Unmet(list []string) []string {
	var out []string
	for _, n := range list {
		if err := Check(n); err != nil {
			out = append(out, n+": "+err.Error())
		}
	}
	return out
}

// Check returns why the machine doesn't meet need n, or nil when it does.
func Check(n string) error {
	kind, arg, _ := strings.Cut(n, ":")
	switch kind {
	case "linux", "windows", "macos":
		if platforms[kind] != runtime.GOOS {
			return fmt.Errorf("this machine runs %s", runtime.GOOS)
		}
	case "root":
		if os.Geteuid() != 0 && succeeds("sudo", "-n", "true") != nil {
			return errors.New("no passwordless sudo")
		}
	case "docker", "podman":
		return succeeds(kind, "info")
	case "rpcbind":
		return succeeds("rpcinfo", "-p")
	case "disk":
		want, _ := strconv.Atoi(arg)
		free, err := freeGB(".")
		if err != nil {
			return err
		}
		if free < want {
			return fmt.Errorf("%d GB free here", free)
		}
	case "port":
		// Every address and loopback alone: some systems let the first bind
		// while something listens on the second.
		for _, addr := range []string{":" + arg, "127.0.0.1:" + arg} {
			l, err := net.Listen("tcp", addr)
			if err != nil {
				return errors.New("the port is in use")
			}
			_ = l.Close()
		}
	case "host":
		return errors.New("only the dedicated host meets it")
	case "service", "cloud":
		return fmt.Errorf("dt doesn't check %s needs yet", kind)
	default:
		bin := tools[n][runtime.GOOS]
		if bin == "" {
			return fmt.Errorf("not available on %s", runtime.GOOS)
		}
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%s not found", bin)
		}
	}
	return nil
}

// succeeds runs a probe, which a stuck daemon can't hold for long.
func succeeds(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := exec.CommandContext(ctx, name, args...).Run()
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return fmt.Errorf("%s not found", name)
	case err != nil:
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

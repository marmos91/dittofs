// Package needs tells which of a suite's needs this machine meets
// (docs/internals/rfc-test-harness.md, section 4.5).
package needs

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
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
		l, err := net.Listen("tcp", ":"+arg)
		if err != nil {
			return errors.New("the port is in use")
		}
		_ = l.Close()
	case "host":
		return errors.New("only the dedicated host meets it")
	case "service":
		return checkService(arg)
	case "cloud":
		return errors.New("dt doesn't check cloud needs yet")
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

// services say, per service, the variable its tests read its address from,
// the variable that must be 1 to enable them, if any, and the prefix of the
// variables that belong to it.
var services = map[string]struct {
	address, enabled, prefix string
	parse                    func(string) (string, error)
}{
	"localstack": {address: "LOCALSTACK_ENDPOINT", prefix: "LOCALSTACK_ENDPOINT", parse: urlAddress},
	"postgres":   {address: "DITTOFS_TEST_POSTGRES_DSN", prefix: "DITTOFS_TEST_POSTGRES_DSN", parse: dsnAddress},
	"kmip":       {address: "DITTOFS_TEST_KMIP_ENDPOINT", enabled: "DITTOFS_TEST_KMIP", prefix: "DITTOFS_TEST_KMIP", parse: hostPort},
}

// checkService meets a service need when the variables its tests read are
// set and its address answers, so the tests can't skip it unseen.
func checkService(name string) error {
	s := services[name]
	if s.enabled != "" && os.Getenv(s.enabled) != "1" {
		return fmt.Errorf("%s isn't 1", s.enabled)
	}
	v := os.Getenv(s.address)
	if v == "" {
		return fmt.Errorf("%s isn't set", s.address)
	}
	addr, err := s.parse(v)
	if err != nil {
		return fmt.Errorf("%s: %w", s.address, err)
	}
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return fmt.Errorf("nothing answers at %s", addr)
	}
	return conn.Close()
}

// ServiceEnv returns the variables of the services among list, as
// NAME=value, for a command whose environment dt passes explicitly.
func ServiceEnv(list []string) []string {
	var out []string
	for _, n := range list {
		name, isService := strings.CutPrefix(n, "service:")
		if !isService {
			continue
		}
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, services[name].prefix) {
				out = append(out, kv)
			}
		}
	}
	return out
}

// urlAddress is the host and port of a URL, with the scheme's port when it
// names none.
func urlAddress(v string) (string, error) {
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%q isn't a URL", v)
	}
	if u.Port() != "" {
		return u.Host, nil
	}
	port := "80"
	if u.Scheme == "https" {
		port = "443"
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}

// dsnAddress is the host and port of a PostgreSQL connection string, as a
// URL or as key=value pairs, with PostgreSQL's defaults.
func dsnAddress(v string) (string, error) {
	if strings.Contains(v, "://") {
		u, err := url.Parse(v)
		if err != nil {
			return "", fmt.Errorf("%q isn't a URL", v)
		}
		port := u.Port()
		if port == "" {
			port = "5432"
		}
		return net.JoinHostPort(u.Hostname(), port), nil
	}
	host, port := "localhost", "5432"
	for _, field := range strings.Fields(v) {
		switch k, val, _ := strings.Cut(field, "="); k {
		case "host":
			host = val
		case "port":
			port = val
		}
	}
	return net.JoinHostPort(host, port), nil
}

// hostPort checks a host:port address.
func hostPort(v string) (string, error) {
	if _, _, err := net.SplitHostPort(v); err != nil {
		return "", err
	}
	return v, nil
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

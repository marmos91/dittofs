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
	"path/filepath"
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

// Unmet returns each need the machine doesn't meet, with the reason. root is
// the checkout, whose local services' values a service need falls back to.
func Unmet(root string, list []string) []string {
	var out []string
	for _, n := range list {
		if err := Check(root, n); err != nil {
			out = append(out, n+": "+err.Error())
		}
	}
	return out
}

// Check returns why the machine doesn't meet need n, or nil when it does.
func Check(root, n string) error {
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
	case "service":
		return checkService(root, arg)
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

// services say, per service, the prefix of the variables its tests read, the
// one holding its address, and the one that must be 1 to enable them, if
// any. When the address isn't set, the values are the local service's, as
// dt services starts it: written here, or in a file it writes.
var services = map[string]struct {
	prefix, address, enabled string
	local                    []string
	localFile                string
	parse                    func(string) (string, error)
}{
	"localstack": {prefix: "LOCALSTACK_ENDPOINT", address: "LOCALSTACK_ENDPOINT",
		local: []string{"LOCALSTACK_ENDPOINT=http://127.0.0.1:4566"}, parse: urlAddress},
	"postgres": {prefix: "DITTOFS_TEST_POSTGRES_DSN", address: "DITTOFS_TEST_POSTGRES_DSN",
		local: []string{"DITTOFS_TEST_POSTGRES_DSN=host=localhost port=15432 dbname=dittofs_test user=postgres password=postgres sslmode=disable"}, parse: dsnAddress},
	"kmip": {prefix: "DITTOFS_TEST_KMIP", address: "DITTOFS_TEST_KMIP_ENDPOINT", enabled: "DITTOFS_TEST_KMIP",
		localFile: "test/harness/state/kmip.env", parse: hostPort},
}

// IsService reports whether name is a service dt knows, which a
// service:<name> need may name.
func IsService(name string) bool {
	_, ok := services[name]
	return ok
}

// serviceVars are a service's variables, as NAME=value: dt's own when its
// address is set, else the local service's.
func serviceVars(root, name string) ([]string, error) {
	s := services[name]
	if os.Getenv(s.address) != "" {
		var vars []string
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, s.prefix) {
				vars = append(vars, kv)
			}
		}
		return vars, nil
	}
	if s.localFile == "" {
		return s.local, nil
	}
	data, err := os.ReadFile(filepath.Join(root, s.localFile))
	if err != nil {
		return nil, fmt.Errorf("%s isn't set, and %s doesn't exist (dt services up %s writes it)", s.address, s.localFile, name)
	}
	var vars []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); strings.Contains(line, "=") && !strings.HasPrefix(line, "#") {
			vars = append(vars, line)
		}
	}
	return vars, nil
}

// checkService meets a service need when its variables are there and its
// address answers, so the tests can't skip it unseen.
func checkService(root, name string) error {
	s, ok := services[name]
	if !ok {
		return errors.New("no such service")
	}
	vars, err := serviceVars(root, name)
	if err != nil {
		return err
	}
	value := func(key string) string {
		for _, kv := range vars {
			if k, v, _ := strings.Cut(kv, "="); k == key {
				return v
			}
		}
		return ""
	}
	if s.enabled != "" && value(s.enabled) != "1" {
		return fmt.Errorf("%s isn't 1", s.enabled)
	}
	addr, err := s.parse(value(s.address))
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
// NAME=value, for the cell's environment and for a command whose environment
// dt passes explicitly.
func ServiceEnv(root string, list []string) []string {
	var out []string
	for _, n := range list {
		if name, isService := strings.CutPrefix(n, "service:"); isService {
			vars, _ := serviceVars(root, name)
			out = append(out, vars...)
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

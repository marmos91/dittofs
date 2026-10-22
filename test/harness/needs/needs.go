// Package needs tells which of a suite's needs this machine meets
// (docs/internals/rfcs/rfc-29-test-harness.md).
package needs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// listen opens a listener; the tests replace it.
var listen = net.Listen

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

// debian names the Debian and Ubuntu package holding a tool's program, where
// its name isn't the program's own.
var debian = map[string]string{
	"nfs-client":     "nfs-common",
	"smb-client":     "cifs-utils",
	"nfs4-acl-tools": "nfs4-acl-tools",
	"kerberos":       "krb5-user",
	"knfsd":          "nfs-kernel-server",
}

// inDtc are the tools the dtc image installs.
var inDtc = map[string]bool{"nfs-client": true, "smb-client": true, "nfs4-acl-tools": true, "kerberos": true, "knfsd": true}

// hints say how to meet a need that isn't a tool, by its kind.
var hints = map[string]string{
	"linux":   "run it on Linux, or in dtc",
	"windows": "run it on Windows",
	"macos":   "run it on macOS",
	"root":    "allow passwordless sudo",
	"docker":  "install or start Docker",
	"podman":  "install podman 5.6 or later, or Docker, which runs it in podman's own image",
	"rpcbind": "install and start rpcbind",
	"disk":    "free disk space",
	"port":    "stop what listens on the port",
	"host":    "run it on the dedicated host",
	"cloud":   "provide the provider's credentials and infrastructure",
}

// osNames names the platforms a tool's program can be on.
var osNames = map[string]string{"darwin": "macOS", "linux": "Linux", "windows": "Windows"}

// Hint says how to meet need n. A service is started by dt services. A tool's
// hint names the program its check looks for, with its package from debian on
// the one platform that uses those packages; where this platform has no
// program for the tool, it names the platforms that have one, and dtc when
// its image installs it.
func Hint(n string) string {
	kind, arg, _ := strings.Cut(n, ":")
	bins, isTool := tools[n]
	switch {
	case kind == "service":
		return "dt services up " + arg
	case !isTool:
		return hints[kind]
	case bins[runtime.GOOS] != "":
		hint := "install " + bins[runtime.GOOS]
		if runtime.GOOS == "linux" && debian[n] != "" {
			hint += " (Debian and Ubuntu: " + debian[n] + ")"
		}
		return hint
	}
	var on []string
	for _, goos := range slices.Sorted(maps.Keys(bins)) {
		on = append(on, osNames[goos])
	}
	hint := "run it on " + strings.Join(on, " or ")
	if inDtc[n] {
		hint += ", or in dtc"
	}
	return hint
}

// Check returns why the machine doesn't meet need n, one the registry
// accepts, or nil when it does.
// root is the checkout, whose local services' values a service need falls
// back to.
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
	case "docker":
		return succeeds(kind, "info")
	case "podman":
		// decision: Docker meets it too, for a podman that is missing or
		// older than the scenarios need: dt then runs the cell's commands in
		// podman's own image. A podman that fails otherwise leaves it unmet,
		// so a host with its own never falls back to that privileged
		// container.
		err := HostPodman()
		_, missing := exec.LookPath("podman")
		if (missing != nil || errors.Is(err, errOldPodman)) && succeeds("docker", "info") == nil {
			return nil
		}
		return err
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
			l, err := listen("tcp", addr)
			if errors.Is(err, os.ErrPermission) {
				// decision: a port below those an ordinary user may bind is
				// checked by connecting to it on loopback, so something that
				// listens only on another address goes unseen, and the
				// fixture that binds it then fails to start. A knfsd fixture
				// starts instead, on the host's NFS server, and stops it.
				conn, err := net.DialTimeout("tcp", "127.0.0.1:"+arg, time.Second)
				if err != nil {
					return nil
				}
				_ = conn.Close()
				return errors.New("the port is in use")
			}
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
	s := services[name]
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
	if name == "localstack" {
		return localstackS3(value(s.address))
	}
	// decision: every other service is met by a connection to its port,
	// which dtc's port forwarder accepts whether or not the service runs.
	// Ask it in its own protocol, as localstack is asked, once a cell passes
	// its need with the service down.
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return fmt.Errorf("nothing answers at %s", addr)
	}
	return conn.Close()
}

// localstackS3 asks Localstack's health check at endpoint, as dt services
// does when it starts Localstack: S3 must be available or running.
func localstackS3(endpoint string) error {
	health := strings.TrimSuffix(endpoint, "/") + "/_localstack/health"
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(health)
	if err != nil {
		return fmt.Errorf("nothing answers at %s", health)
	}
	defer func() { _ = resp.Body.Close() }()
	var answer struct {
		Services map[string]string `json:"services"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&answer) != nil {
		return fmt.Errorf("%s answered %d, not with Localstack's health", health, resp.StatusCode)
	}
	if s3 := answer.Services["s3"]; s3 != "available" && s3 != "running" {
		return fmt.Errorf("%s says S3 is %q, not available or running", health, s3)
	}
	return nil
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

// errOldPodman is HostPodman's error for a podman older than the scenarios
// need.
var errOldPodman = errors.New("the scenarios need 5.6 or later")

// HostPodman says why this machine's own podman can't run the scenarios, or
// returns nil when it can: they need podman 5.6 or later, whose pull takes
// --policy.
func HostPodman() error {
	out, err := probe("podman", "info", "--format", "{{.Version.Version}}")
	if err != nil {
		return err
	}
	v := strings.TrimSpace(string(out))
	major, rest, _ := strings.Cut(v, ".")
	minor, _, _ := strings.Cut(rest, ".")
	ma, err1 := strconv.Atoi(major)
	mi, err2 := strconv.Atoi(minor)
	if err1 != nil || err2 != nil {
		return fmt.Errorf("podman's version %q isn't a number", v)
	}
	if ma < 5 || (ma == 5 && mi < 6) {
		return fmt.Errorf("podman %s: %w", v, errOldPodman)
	}
	return nil
}

// succeeds runs a probe.
func succeeds(name string, args ...string) error {
	_, err := probe(name, args...)
	return err
}

// probe runs a command and returns its output, under a limit a stuck daemon
// can't hold for long.
func probe(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return nil, fmt.Errorf("%s not found", name)
	case err != nil:
		return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

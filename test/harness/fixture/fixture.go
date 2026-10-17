// Package fixture sets up the DittoFS server a cell's tests run against: dfs
// from {bin}, on a store profile, with the default share, its users and its
// adapter settings. dt starts it before a cell's first test and stops it after
// the last.
package fixture

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/marmos91/dittofs/test/harness/registry"
)

// Share is the fixture's share, the default one when no shares file is given.
const Share = "/export"

// User is {user}: the test user every fixture creates, with {password} and
// UserUID as its UID and GID.
const (
	User    = "dt-user"
	UserUID = uint32(65535)
)

// stopGrace is how long dfs has to shut down before its group is killed.
const stopGrace = 10 * time.Second

// Unsupported says what of fixture f dt doesn't set up yet, or returns "". A
// field that holds a matrix placeholder is checked once a cell fills it.
func Unsupported(f *registry.Fixture) string {
	fixed := func(s string) bool { return len(registry.Placeholders(s)) == 0 }
	switch {
	case f.Server != "" && f.Server != "dfs" && fixed(f.Server):
		return fmt.Sprintf("dt doesn't set up a %s fixture yet", f.Server)
	case f.Profile != "memory" && f.Profile != "badger" && f.Profile != "badger-s3" && fixed(f.Profile):
		return fmt.Sprintf("dt doesn't set up the %q profile yet", f.Profile)
	case mounts(f.NFS.Version) && fixed(f.NFS.Version) && mountVers[f.NFS.Version] == "":
		return fmt.Sprintf("dt doesn't mount NFS %s yet", f.NFS.Version)
	case mounts(f.NFS.Version) && fixed(f.NFS.Version) && runtime.GOOS != "linux":
		return "dt mounts NFS only on Linux yet"
	case f.SMB.Enabled:
		return "dt doesn't serve SMB from fixtures yet"
	case f.Shares != "":
		return "dt doesn't create a shares file's shares yet"
	case f.Kerberos:
		return "dt doesn't set up Kerberos in fixtures yet"
	case f.Reset != "":
		return "dt doesn't reset shares yet"
	}
	return ""
}

// Placeholders names what fixture f gives a cell's commands, besides {bin}:
// {server}, {user} and {password}, {nfs-port} when it serves NFS, and
// {mount} when it mounts the share.
func Placeholders(f registry.Fixture) []string {
	names := []string{"server", "user", "password"}
	if f.NFS.Version != "" {
		names = append(names, "nfs-port")
	}
	if mounts(f.NFS.Version) {
		names = append(names, "mount")
	}
	return names
}

// Server is a running fixture.
type Server struct {
	vals    map[string]string
	cmd     *exec.Cmd
	exited  chan struct{} // closed once dfs has exited
	err     error         // how dfs exited, once exited is closed
	log     *os.File
	data    string
	api     string    // the API's base URL
	nfsPort int       // 0 when the fixture serves no NFS
	mount   string    // {mount}, once the fixture makes it
	out     io.Writer // dt's lines on the setup and the teardown
	stop    sync.Once
	stopErr error
}

// Placeholders returns what the server gives a cell's commands, by name.
func (s *Server) Placeholders() map[string]string { return s.vals }

// Start starts dfs from bin with its data under out, in a directory it
// removes when it stops, and its output in out/dfs.log, which it keeps. It
// sets the server up as f asks and checks that it answers. An S3 profile's
// bucket is created first, at the endpoint services give: the variables of
// the cell's services, as NAME=value. It makes one attempt: when a step
// fails, or ctx ends first, it stops the server and returns why. w gets dt's
// lines on the setup, and never a credential.
func Start(ctx context.Context, f registry.Fixture, services []string, bin, out string, w io.Writer) (*Server, error) {
	server, err := address()
	if err != nil {
		return nil, err
	}
	// The bucket comes first, so when it fails there is no server to stop.
	var b *bucket
	if f.Profile == "badger-s3" {
		if b, err = newBucket(ctx, services); err != nil {
			return nil, err
		}
		_, _ = fmt.Fprintf(w, "[dt] fixture: blocks in the bucket %s at %s, under %s\n", bucketName, b.endpoint, b.prefix)
	}
	ports, err := freePorts(2)
	if err != nil {
		return nil, err
	}
	s := &Server{data: filepath.Join(out, "dfs"), api: "http://127.0.0.1:" + strconv.Itoa(ports[0]), exited: make(chan struct{}), out: w}
	if f.NFS.Version != "" {
		s.nfsPort = ports[1]
	}
	// decision: the server's output stays with its cell as dfs.log in {out},
	// since the run's logs directory holds one log per cell.
	if s.log, err = os.Create(filepath.Join(out, "dfs.log")); err != nil {
		return nil, err
	}
	// decision: {password} is generated per fixture, so it changes with
	// every run and no two servers share one.
	admin, password := secret(), secret()
	config, err := writeConfig(s.data, ports[0])
	if err == nil {
		s.cmd = exec.Command(filepath.Join(bin, "dfs"), "start", "--foreground", "--config", config)
		s.cmd.Env = append(serverEnv(), "DITTOFS_ADMIN_INITIAL_PASSWORD="+admin)
		s.cmd.Stdout, s.cmd.Stderr = s.log, s.log
		ownGroup(s.cmd)
		err = s.cmd.Start()
	}
	if err != nil {
		_ = s.log.Close()
		_ = os.RemoveAll(s.data)
		return nil, err
	}
	go func() {
		s.err = s.cmd.Wait()
		close(s.exited)
	}()
	_, _ = fmt.Fprintf(w, "[dt] fixture: dfs started, pid %d, its API at %s, its output in %s\n", s.cmd.Process.Pid, s.api, s.log.Name())

	// The API client takes no context, so the setup runs aside: when ctx
	// ends first, stopping the server ends the request in flight.
	done := make(chan error, 1)
	go func() { done <- s.setUp(ctx, f, b, admin, password) }()
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = s.Stop()
		<-done
		err = ctx.Err()
	}
	if err != nil {
		_ = s.Stop()
		return nil, err
	}
	s.vals = map[string]string{"server": server, "user": User, "password": password}
	if s.nfsPort > 0 {
		s.vals["nfs-port"] = strconv.Itoa(s.nfsPort)
		_, _ = fmt.Fprintf(w, "[dt] fixture: NFS exports %s at %s:%d\n", Share, server, s.nfsPort)
	}
	if mounts(f.NFS.Version) {
		if err := s.mountShare(ctx, server, f.NFS); err != nil {
			_ = s.Stop()
			return nil, err
		}
		s.vals["mount"] = s.mount
		_, _ = fmt.Fprintf(w, "[dt] fixture: NFS %s mounted at %s\n", mountVers[f.NFS.Version], s.mount)
	}
	return s, nil
}

// setUp waits for the server's API, logs in as its admin, configures it and
// checks it.
func (s *Server) setUp(ctx context.Context, f registry.Fixture, b *bucket, admin, password string) error {
	if err := s.waitReady(ctx); err != nil {
		return err
	}
	c := apiclient.New(s.api)
	token, err := c.Login("admin", admin)
	if err != nil {
		return fmt.Errorf("logging in as admin: %w", err)
	}
	c.SetToken(token.AccessToken)
	if err := configure(c, f, b, s.data, password, s.nfsPort); err != nil {
		return err
	}
	return check(ctx, c, s.api, b, s.nfsPort)
}

// waitReady waits until the API's readiness probe answers 200.
func (s *Server) waitReady(ctx context.Context) error {
	for ready(ctx, s.api) != nil {
		select {
		case <-s.exited:
			return fmt.Errorf("dfs exited before its API answered (%v); its output is dfs.log in the cell's results", s.err)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil
}

// Stop unmounts the share, then stops the process group dt started dfs in,
// and nothing outside it, then removes the server's data; its output stays.
// The share goes first, since an unmount waits on a server that is gone. dfs
// gets SIGTERM and stopGrace to exit before SIGKILL, also when the setup's
// limit stops it, so a dfs that hangs there holds its cell up to stopGrace
// past that limit. It returns an error when dfs had exited before Stop: the
// server ended before its cell did.
func (s *Server) Stop() error {
	s.stop.Do(func() {
		if s.mount != "" {
			s.unmount()
		}
		select {
		case <-s.exited:
			s.stopErr = fmt.Errorf("dfs exited before its cell ended (%v); its output is dfs.log in the cell's results", s.err)
		default:
			stopGroup(s.cmd, false)
			select {
			case <-s.exited:
			case <-time.After(stopGrace):
				stopGroup(s.cmd, true)
				<-s.exited
			}
		}
		_ = s.log.Close()
		_ = os.RemoveAll(s.data)
	})
	return s.stopErr
}

// ready asks the API's readiness probe once.
func ready(ctx context.Context, api string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api+"/health/ready", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/health/ready answered %d", resp.StatusCode)
	}
	return nil
}

// writeConfig writes the server's configuration into a new directory data:
// its API on port, and its database and journal in data.
func writeConfig(data string, port int) (string, error) {
	if err := os.MkdirAll(data, 0o700); err != nil {
		return "", err
	}
	// decision: the server logs at INFO, which keeps its errors, warnings and
	// any panic trace without a line per operation in every cell's results.
	// Raise it when a failure needs the operations to be understood.
	config := fmt.Sprintf(`logging: { level: INFO, format: text, output: stdout }
controlplane:
  port: %d
  jwt: { secret: %q }
database:
  type: sqlite
  sqlite: { path: %q }
blockstore:
  journal: { path: %q }
`, port, secret()+secret(), filepath.Join(data, "controlplane.db"), filepath.Join(data, "journal"))
	path := filepath.Join(data, "config.yaml")
	return path, os.WriteFile(path, []byte(config), 0o600)
}

// serverEnv is dt's environment without the server's own variables.
func serverEnv() []string {
	// decision: the fixture's server is configured by dt's file alone, so a
	// DITTOFS_ variable set for a developer's own server can't move its ports
	// or its stores.
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "DITTOFS_") {
			env = append(env, kv)
		}
	}
	return env
}

// secret returns 32 random hex digits.
func secret() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// freePorts returns n different TCP ports that are free on every interface.
func freePorts(n int) ([]int, error) {
	// ponytail: a port is free when it's chosen, and another process can take
	// it before dfs binds it; the setup then fails and the cell isn't run.
	// Let dfs choose its own ports and report them if that shows up.
	var ports []int
	for range n {
		l, err := net.Listen("tcp", ":0")
		if err != nil {
			return nil, err
		}
		defer func() { _ = l.Close() }()
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	return ports, nil
}

// iface is what choosing {server} reads of a network interface.
type iface struct {
	flags net.Flags
	addrs []net.Addr
}

// interfaces lists this machine's interfaces; the tests replace it.
var interfaces = func() ([]iface, error) {
	list, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]iface, 0, len(list))
	for _, i := range list {
		addrs, _ := i.Addrs()
		out = append(out, iface{i.Flags, addrs})
	}
	return out, nil
}

// address is {server}: the IPv4 address of the first interface that is up
// and not loopback, leaving out link-local addresses. A client connecting to
// it has an address of the same kind, which the server dials for NFSv4.0
// callbacks; it refuses a loopback one.
func address() (string, error) {
	list, err := interfaces()
	if err != nil {
		return "", err
	}
	for _, i := range list {
		if i.flags&net.FlagUp == 0 || i.flags&net.FlagLoopback != 0 {
			continue
		}
		for _, a := range i.addrs {
			if n, ok := a.(*net.IPNet); ok {
				if ip := n.IP.To4(); ip != nil && !ip.IsLinkLocalUnicast() && !ip.IsLoopback() {
					return ip.String(), nil
				}
			}
		}
	}
	return "", errors.New("this machine has no IPv4 address on an interface that is up and not loopback")
}

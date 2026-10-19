// Package fixture sets up the DittoFS server a cell's tests run against: dfs
// from {bin}, on a store profile, with the default share, its users and its
// adapter settings. dt starts it before a cell's first test and stops it after
// the last.
package fixture

import (
	"cmp"
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

// User is {user} and User2 is {user2}: the test users every fixture creates,
// with {password}, each with its UID as its GID too.
const (
	User     = "dt-user"
	UserUID  = uint32(65535)
	User2    = "dt-user2"
	User2UID = uint32(65536)
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
	case f.SMB.Encryption != "" && f.SMB.Encryption != "preferred" && fixed(f.SMB.Encryption):
		return fmt.Sprintf("dt doesn't set SMB encryption %q", f.SMB.Encryption)
	case f.SMB.Mount:
		return "dt doesn't mount SMB yet"
	case f.Kerberos:
		return "dt doesn't set up Kerberos in fixtures yet"
	case f.Reset != "" && f.Reset != "each" && fixed(f.Reset):
		return fmt.Sprintf("dt doesn't reset shares %q", f.Reset)
	}
	return ""
}

// Placeholders names what fixture f gives a cell's commands, besides {bin}:
// {server}, {user}, {user2} and {password}, {nfs-port} when it serves NFS,
// {smb-port} when it serves SMB, and {mount} when it mounts the share.
func Placeholders(f registry.Fixture) []string {
	names := []string{"server", "user", "user2", "password"}
	if f.NFS.Version != "" {
		names = append(names, "nfs-port")
	}
	if f.SMB.Enabled {
		names = append(names, "smb-port")
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
	api     string // the API's base URL
	admin   string // the admin's password
	f       registry.Fixture
	shares  []apiclient.CreateShareRequest // the first is {share}
	nfsPort int                            // 0 when the fixture serves no NFS
	smbPort int                            // 0 when the fixture serves no SMB
	mount   string                         // {mount}, once the fixture makes it
	out     io.Writer                      // dt's lines on the setup and the teardown
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
	shares, err := loadShares(f.Shares)
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
	ports, err := freePorts(3)
	if err != nil {
		return nil, err
	}
	s := &Server{data: filepath.Join(out, "dfs"), api: "http://127.0.0.1:" + strconv.Itoa(ports[0]), f: f, shares: shares, exited: make(chan struct{}), out: w}
	if f.NFS.Version != "" {
		s.nfsPort = ports[1]
	}
	if f.SMB.Enabled {
		s.smbPort = cmp.Or(f.SMB.Port, ports[2])
	}
	// decision: the server's output stays with its cell as dfs.log in {out},
	// since the run's logs directory holds one log per cell.
	if s.log, err = os.Create(filepath.Join(out, "dfs.log")); err != nil {
		return nil, err
	}
	// decision: {password} is generated per fixture, so it changes with
	// every run and no two servers share one. It is hex, so it needs no
	// escaping in the XML of a tool's configuration.
	password := secret()
	s.admin = secret()
	config, err := writeConfig(s.data, ports[0])
	if err == nil {
		argv := []string{filepath.Join(bin, "dfs"), "start", "--foreground", "--config", config}
		if f.SMB.Port != 0 {
			// A set port is one a user may not bind, as 445: dfs runs as dt's
			// user with one capability, CAP_NET_BIND_SERVICE, held as an
			// ambient one. A file capability would put dfs in secure mode,
			// where Go prints no trace when it panics.
			//
			// decision: sudo resets the environment, so it is told to keep
			// the admin's password, which then never shows on its command
			// line, where any user could read it while the server runs.
			//
			// ponytail: a dfs started through sudo outlives a dt that is
			// killed, since the parent-death signal doesn't reach it, and
			// holds the port until it is stopped by hand. Have dt stop such a
			// server before it starts the next if that shows up.
			priv := []string{"setpriv", "--reuid=" + strconv.Itoa(os.Getuid()), "--regid=" + strconv.Itoa(os.Getgid()), "--init-groups", "--inh-caps=+net_bind_service", "--ambient-caps=+net_bind_service", "--"}
			if geteuid() != 0 {
				priv = append([]string{"sudo", "-n", "--preserve-env=DITTOFS_ADMIN_INITIAL_PASSWORD"}, priv...)
			}
			argv = append(priv, argv...)
			_, _ = fmt.Fprintf(w, "[dt] fixture: $ %s\n", strings.Join(argv, " "))
		}
		s.cmd = exec.Command(argv[0], argv[1:]...)
		s.cmd.Env = append(serverEnv(), "DITTOFS_ADMIN_INITIAL_PASSWORD="+s.admin)
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
	go func() { done <- s.setUp(ctx, b, password) }()
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
	s.vals = map[string]string{"server": server, "user": User, "user2": User2, "password": password}
	if s.nfsPort > 0 {
		s.vals["nfs-port"] = strconv.Itoa(s.nfsPort)
		_, _ = fmt.Fprintf(w, "[dt] fixture: NFS exports %s at %s:%d\n", Share, server, s.nfsPort)
	}
	if s.smbPort > 0 {
		s.vals["smb-port"] = strconv.Itoa(s.smbPort)
		names := make([]string, len(shares))
		for i, share := range shares {
			names[i] = share.Name
		}
		_, _ = fmt.Fprintf(w, "[dt] fixture: SMB serves %s at %s:%d\n", strings.Join(names, " "), server, s.smbPort)
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
func (s *Server) setUp(ctx context.Context, b *bucket, password string) error {
	if err := s.waitReady(ctx); err != nil {
		return err
	}
	c, err := s.login()
	if err != nil {
		return err
	}
	if err := s.configure(c, b, password); err != nil {
		return err
	}
	return s.check(c, b)
}

// login is an API client logged in as the server's admin.
func (s *Server) login() (*apiclient.Client, error) {
	c := apiclient.New(s.api)
	token, err := c.Login("admin", s.admin)
	if err != nil {
		return nil, fmt.Errorf("logging in as admin: %w", err)
	}
	c.SetToken(token.AccessToken)
	return c, nil
}

// Reset removes {share}, the first share, and creates it again with its
// settings and grants, as the conformance runner's reset does.
func (s *Server) Reset() error {
	c, err := s.login()
	if err != nil {
		return err
	}
	share := s.shares[0]
	if err := c.RemoveShare(share.Name); err != nil {
		return fmt.Errorf("removing the share %s: %w", share.Name, err)
	}
	if err := addShare(c, s.f, share); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(s.out, "[dt] fixture: the share %s recreated\n", share.Name)
	return nil
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

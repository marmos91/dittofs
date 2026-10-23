package fixture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// clients are a fixture's namespaced clients. Each is a network namespace of
// its own, joined to the host by a veth pair on a bridge, with a mount and a
// UTS namespace kept in files, so every command dt runs there enters the
// same ones: a private /run for its own rpcbind and rpc.statd, its own
// hostname, and the mounts made there. A client on the server's own network
// stack would lose NLM, since its lockd takes the server's registration, and
// its traffic couldn't be told from the server's.
type clients struct {
	bridge  string // dtnfsX, which names the rest
	subnet  string // 10.51.X
	dir     string // the namespace files, and the hosts file, keys and configs
	n       int
	forward bool // a FORWARD rule lets the clients reach each other
	out     io.Writer
	// userSSHConfig is the user's own ssh configuration, when the user has
	// one and there are two clients.
	userSSHConfig string
}

// currentUser is the user dt runs as; the tests replace it.
var currentUser = user.Current

// clientWaits is how long a client's checks retry, and how often; the tests
// shorten it.
var clientWaits = [2]time.Duration{10 * time.Second, 500 * time.Millisecond}

// name is client i's network namespace and its hostname; addr is its address.
func (c *clients) name(i int) string { return c.bridge + "-" + strconv.Itoa(i) }
func (c *clients) addr(i int) string { return c.subnet + "." + strconv.Itoa(i+1) }

// server is the host's address on the bridge, {server} from a client.
func (c *clients) server() string { return c.subnet + ".1" }

// in is argv run as root in client i's namespaces, in directory wd when it
// isn't "".
func (c *clients) in(i int, wd string, argv ...string) []string {
	enter := []string{"nsenter", "--net=/run/netns/" + c.name(i), "--mount=" + c.ns(i, "mnt"), "--uts=" + c.ns(i, "uts")}
	if wd != "" {
		enter = append(enter, "--wd="+wd)
	}
	return asRoot(append(append(enter, "--"), argv...)...)
}

// ns is the file that keeps client i's mount or UTS namespace.
func (c *clients) ns(i int, kind string) string {
	return filepath.Join(c.dir, "ns", strconv.Itoa(i)+"."+kind)
}

// asUser is argv run on client 1 as the user dt runs as, in dir, with env
// added to what identifies the user. sudo resets the environment, so it is
// passed explicitly; dt running as root runs it as root, with its own.
func (c *clients) asUser(argv []string, dir string, env []string) ([]string, error) {
	if geteuid() == 0 {
		return c.in(1, dir, argv...), nil
	}
	u, err := currentUser()
	if err != nil {
		return nil, err
	}
	who := []string{"setpriv", "--reuid=" + u.Uid, "--regid=" + u.Gid, "--init-groups", "--", "env",
		"PATH=" + os.Getenv("PATH"), "HOME=" + u.HomeDir, "USER=" + u.Username, "LOGNAME=" + u.Username}
	return c.in(1, dir, append(append(who, env...), argv...)...), nil
}

// upClients sets up n namespaced clients, which reach the server on port at
// the bridge's address, and with two, the second one's sshd, which the user dt
// runs as reaches from the first with a key of the fixture's own; its output
// goes to sshd-client2.log in logs. It first removes the clients of a dt that
// died without removing them. On an error, what was set up stays for down to
// remove.
func upClients(ctx context.Context, n, port int, logs string, out io.Writer) (*clients, error) {
	c := &clients{n: n, out: out}
	var err error
	if c.dir, err = os.MkdirTemp("", "dt-clients-"); err != nil {
		return nil, err
	}
	c.reap()
	if err := c.claimBridge(ctx); err != nil {
		return c, err
	}
	if err := c.writeFiles(); err != nil {
		return c, err
	}
	steps := [][]string{
		asRoot("ip", "addr", "add", c.server()+"/24", "dev", c.bridge),
		asRoot("ip", "link", "set", c.bridge, "up"),
		asRoot("mount", "--bind", filepath.Join(c.dir, "ns"), filepath.Join(c.dir, "ns")),
		// A namespace's file can be bound only on a mount that isn't shared.
		asRoot("mount", "--make-private", filepath.Join(c.dir, "ns")),
	}
	for i := 1; i <= n; i++ {
		host, peer := c.bridge+"h"+strconv.Itoa(i), c.bridge+"c"+strconv.Itoa(i)
		steps = append(steps,
			asRoot("ip", "netns", "add", c.name(i)),
			asRoot("ip", "link", "add", host, "type", "veth", "peer", "name", peer),
			asRoot("ip", "link", "set", peer, "netns", c.name(i)),
			asRoot("ip", "link", "set", host, "master", c.bridge, "up"),
			asRoot("ip", "-n", c.name(i), "link", "set", "lo", "up"),
			asRoot("ip", "-n", c.name(i), "addr", "add", c.addr(i)+"/24", "dev", peer),
			asRoot("ip", "-n", c.name(i), "link", "set", peer, "up"),
			// nfstest finds its own address from the route to a public one.
			asRoot("ip", "-n", c.name(i), "route", "add", "default", "via", c.server()),
			asRoot("touch", c.ns(i, "mnt"), c.ns(i, "uts")),
			asRoot("unshare", "--mount="+c.ns(i, "mnt"), "--uts="+c.ns(i, "uts"), "--propagation", "private", "true"),
			c.in(i, "", "mount", "-t", "tmpfs", "-o", "mode=0755", "tmpfs", "/run"),
			c.in(i, "", "mkdir", "-p", "/run/rpcbind", "/run/sshd", "/run/statd/sm", "/run/statd/sm.bak"),
			// NLM tells clients apart by their hostnames, and sudo resolves
			// its own.
			c.in(i, "", "hostname", c.name(i)),
			c.in(i, "", "mount", "--bind", filepath.Join(c.dir, "hosts"), "/etc/hosts"),
		)
		if i == 1 && n == 2 {
			// ssh reads the user's own configuration first, where a Host *
			// entry would win.
			config := filepath.Join(c.dir, "ssh_config")
			steps = append(steps, c.in(i, "", "mount", "--bind", config, "/etc/ssh/ssh_config"))
			if c.userSSHConfig != "" {
				steps = append(steps, c.in(i, "", "mount", "--bind", config, c.userSSHConfig))
			}
		}
		steps = append(steps, c.in(i, "", "rpcbind"), c.in(i, "", "rpc.statd", "--no-notify", "-P", "/run/statd"))
	}
	if n == 2 {
		// nfstest captures on the first client and expects the second's
		// calls in that capture too, as on a shared segment, so every frame
		// the second sends or receives is copied into the first's interface.
		h1, h2 := c.bridge+"h1", c.bridge+"h2"
		steps = append(steps, asRoot("tc", "qdisc", "add", "dev", h2, "clsact"),
			asRoot("tc", "filter", "add", "dev", h2, "ingress", "matchall", "action", "mirred", "egress", "mirror", "dev", h1),
			asRoot("tc", "filter", "add", "dev", h2, "egress", "matchall", "action", "mirred", "egress", "mirror", "dev", h1))
	}
	for _, argv := range steps {
		if err := c.run(ctx, argv); err != nil {
			return c, err
		}
	}
	for i := 1; i <= n; i++ {
		if err := c.identify(ctx, i); err != nil {
			return c, err
		}
		if err := c.retry(ctx, c.in(i, "", "rpcinfo", "-n", strconv.Itoa(port), "-t", c.server(), "100003")); err != nil {
			return c, fmt.Errorf("client %s doesn't reach the server's NFS at %s:%d: %w", c.name(i), c.server(), port, err)
		}
	}
	if n == 2 {
		return c, c.startSSHD(ctx, filepath.Join(logs, "sshd-client2.log"))
	}
	return c, nil
}

// claimBridge creates the bridge dtnfsX for the first X whose subnet,
// 10.51.X.0/24, no address of this machine is in and whose bridge doesn't
// exist yet, which another fixture's would. Its alias names its owner, dt's
// process, and the clients' directory, for reap.
func (c *clients) claimBridge(ctx context.Context) error {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return err
	}
	for x := 0; x < 256; x++ {
		_, subnet, _ := net.ParseCIDR(fmt.Sprintf("10.51.%d.0/24", x))
		taken := false
		for _, a := range addrs {
			if ip, ok := a.(*net.IPNet); ok && subnet.Contains(ip.IP) {
				taken = true
			}
		}
		if taken {
			continue
		}
		bridge := "dtnfs" + strconv.Itoa(x)
		if c.run(ctx, asRoot("ip", "link", "add", bridge, "type", "bridge")) == nil {
			c.bridge, c.subnet = bridge, fmt.Sprintf("10.51.%d", x)
			if err := c.run(ctx, asRoot("ip", "link", "set", bridge, "alias", fmt.Sprintf("dt-%d %s", os.Getpid(), c.dir))); err != nil {
				return err
			}
			// With br_netfilter, traffic between two ports of the bridge
			// passes the FORWARD chain, whose policy may drop it.
			c.forward = c.run(ctx, asRoot("iptables", "-I", "FORWARD", "-i", bridge, "-o", bridge, "-j", "ACCEPT")) == nil
			return nil
		}
	}
	return errors.New("no bridge dtnfs0 to dtnfs255 could be made for the clients")
}

// reap removes the clients of every bridge whose alias names a dt that no
// longer runs, as claimBridge writes it.
func (c *clients) reap() {
	out, err := exec.Command("ip", "-j", "link", "show", "type", "bridge").Output()
	var links []struct{ Ifname, Ifalias string }
	if err != nil || json.Unmarshal(out, &links) != nil {
		return
	}
	for _, l := range links {
		var pid int
		var dir string
		if _, err := fmt.Sscanf(l.Ifalias, "dt-%d %s", &pid, &dir); err != nil || !strings.HasPrefix(filepath.Base(dir), "dt-clients-") {
			continue
		}
		if p, err := os.FindProcess(pid); err == nil && !errors.Is(p.Signal(syscall.Signal(0)), os.ErrProcessDone) {
			continue
		}
		_, _ = fmt.Fprintf(c.out, "[dt] fixture: removing %s, whose dt (pid %d) is gone\n", l.Ifname, pid)
		(&clients{bridge: l.Ifname, dir: dir, n: 2, forward: true, out: c.out}).down()
	}
}

// writeFiles writes the clients' hosts file, and with two, the first one's
// ssh configuration, the second one's sshd's, and their keys.
func (c *clients) writeFiles() error {
	if err := os.Mkdir(filepath.Join(c.dir, "ns"), 0o755); err != nil {
		return err
	}
	hosts := "127.0.0.1 localhost\n"
	for i := 1; i <= c.n; i++ {
		hosts += c.addr(i) + " " + c.name(i) + "\n"
	}
	files := map[string]string{"hosts": hosts}
	if c.n == 2 {
		u, err := currentUser()
		if err != nil {
			return err
		}
		for _, key := range []string{"id", "host_key"} {
			if err := c.run(context.Background(), []string{"ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(c.dir, key)}); err != nil {
				return err
			}
		}
		pub, err := os.ReadFile(filepath.Join(c.dir, "id.pub"))
		if err != nil {
			return err
		}
		files["authorized_keys"] = string(pub)
		config := filepath.Join(u.HomeDir, ".ssh", "config")
		if _, err := os.Stat(config); err == nil {
			c.userSSHConfig = config
		}
		// The second client is new on every run, with a new host key.
		files["ssh_config"] = fmt.Sprintf("Host %s\n  IdentityFile %s\n  StrictHostKeyChecking no\n  UserKnownHostsFile /dev/null\n  BatchMode yes\n  LogLevel ERROR\n",
			c.addr(2), filepath.Join(c.dir, "id"))
		// decision: StrictModes is off, since the key lives in the fixture's
		// directory, not under the user's home, and sshd would otherwise
		// refuse a directory above it that others can write, as /tmp. PAM
		// stays on, so an account whose password is locked, as a CI
		// runner's can be, still logs in with its key.
		files["sshd_config"] = fmt.Sprintf("ListenAddress %s\nHostKey %s\nPidFile /run/sshd.pid\nAuthorizedKeysFile %s\nAllowUsers %s\nStrictModes no\nPubkeyAuthentication yes\nPasswordAuthentication no\nKbdInteractiveAuthentication no\nUsePAM yes\nPrintMotd no\n",
			c.addr(2), filepath.Join(c.dir, "host_key"), filepath.Join(c.dir, "authorized_keys"), u.Username)
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(c.dir, name), []byte(text), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// identify gives client i an NFSv4 client identifier of its own, which its
// network namespace keeps, so two clients' leases stay apart. Before the NFS
// module is loaded there is none to set, and the hostname tells them apart.
func (c *clients) identify(ctx context.Context, i int) error {
	const id = "/sys/fs/nfs/net/nfs_client/identifier"
	if _, err := os.Stat(id); err != nil {
		return nil
	}
	// ip netns exec mounts the namespace's own /sys, where the file is its own.
	argv := asRoot("ip", "netns", "exec", c.name(i), "tee", id)
	_, _ = fmt.Fprintf(c.out, "[dt] fixture: $ %s\n", strings.Join(argv, " "))
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(secret())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("`%s`: %w: %s", strings.Join(argv, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

// startSSHD starts sshd on the second client, its output in the file at log,
// and waits until the first client reaches it as the user dt runs as.
func (c *clients) startSSHD(ctx context.Context, log string) error {
	sshd := "/usr/sbin/sshd" // sshd re-executes itself, so it runs from its absolute path
	if p, err := exec.LookPath("sshd"); err == nil {
		sshd = p
	}
	f, err := os.Create(log)
	if err != nil {
		return err
	}
	argv := c.in(2, "", sshd, "-D", "-e", "-f", filepath.Join(c.dir, "sshd_config"))
	_, _ = fmt.Fprintf(c.out, "[dt] fixture: $ %s\n", strings.Join(argv, " "))
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		return err
	}
	// down ends it with the rest of the client's processes.
	go func() { _ = cmd.Wait(); _ = f.Close() }()
	ssh, err := c.asUser([]string{"ssh", c.addr(2), "true"}, "", nil)
	if err != nil {
		return err
	}
	if err := c.retry(ctx, ssh); err != nil {
		return fmt.Errorf("the first client doesn't reach the second's sshd at %s: %w; its output is %s", c.addr(2), err, log)
	}
	return nil
}

// down removes the clients and everything in them, the processes first, and
// then the bridge; each step is tried, whatever the ones before did.
func (c *clients) down() {
	quiet := func(argv ...string) { _ = runFor(context.Background(), 0, asRoot(argv...)) }
	for i := 1; i <= c.n && c.bridge != ""; i++ {
		// A process that forks between the listing and the kill, as sshd
		// does, leaves its child, so the listing is repeated until it's
		// empty, for clientWaits.
		pids := asRoot("ip", "netns", "pids", c.name(i))
		for end := time.Now().Add(clientWaits[0]); time.Now().Before(end); {
			out, err := exec.Command(pids[0], pids[1:]...).Output()
			if err != nil || len(bytes.TrimSpace(out)) == 0 {
				break
			}
			quiet(append([]string{"kill", "-9"}, strings.Fields(string(out))...)...)
		}
		quiet("umount", c.ns(i, "mnt"), c.ns(i, "uts"))
		// The kernel removes a namespace, and the veth pair in it, only
		// later, while the next fixture may claim the bridge's name at once.
		quiet("ip", "link", "del", c.bridge+"h"+strconv.Itoa(i))
		quiet("ip", "netns", "del", c.name(i))
	}
	if c.bridge != "" {
		quiet("umount", filepath.Join(c.dir, "ns"))
		quiet("ip", "link", "del", c.bridge)
	}
	if c.forward {
		quiet("iptables", "-D", "FORWARD", "-i", c.bridge, "-o", c.bridge, "-j", "ACCEPT")
	}
	_ = os.RemoveAll(c.dir)
}

// run runs a step of the clients' setup, logging it.
func (c *clients) run(ctx context.Context, argv []string) error { return step(ctx, c.out, argv) }

// step runs argv, a step of a fixture's setup, and logs it to w.
func step(ctx context.Context, w io.Writer, argv []string) error {
	_, _ = fmt.Fprintf(w, "[dt] fixture: $ %s\n", strings.Join(argv, " "))
	if err := runFor(ctx, 0, argv); err != nil {
		return fmt.Errorf("`%s`: %w", strings.Join(argv, " "), err)
	}
	return nil
}

// retry runs argv until it succeeds, for clientWaits, since a bridge's port
// and a new daemon take a moment to answer.
func (c *clients) retry(ctx context.Context, argv []string) error {
	ctx, cancel := context.WithTimeout(ctx, clientWaits[0])
	defer cancel()
	for {
		err := runFor(ctx, 0, argv)
		if err == nil || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(clientWaits[1]):
		}
	}
}

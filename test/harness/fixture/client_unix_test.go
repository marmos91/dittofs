//go:build !windows

package fixture

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeClientTools puts on PATH a sudo that runs its command, and an ip,
// nsenter, unshare, mount, umount, iptables, tc, kill and rpcinfo that log
// each call and succeed. ip fails to add a bridge DT_FAKE_TAKEN names, as an
// existing one, and lists as the machine's bridges the JSON in the file
// bridges in the returned directory. A namespace holds processes 4242 and
// 4243 from its creation until kill ends them or the namespace goes, and a
// listing gives one, as when a process forks after a listing. ssh run in a
// client fails until sshd has been started, as a real one does until sshd
// listens. dt runs as an ordinary user. It returns a function giving the
// calls so far.
func fakeClientTools(t *testing.T) (calls func() []string, dir string) {
	t.Helper()
	dir = t.TempDir()
	log := filepath.Join(dir, "log")
	fake := `echo "${0##*/} $*" >> "$DT_FAKE_LOG"
state="${DT_FAKE_LOG%/*}"
case "${0##*/} $*" in
"ip link add $DT_FAKE_TAKEN type bridge") exit 2 ;;
"ip -j link show type bridge") cat "$state/bridges" ;;
"ip netns add "*) printf '4242\n4243\n' > "$state/netns-$3" ;;
"ip netns del "*) rm -f "$state/netns-$3" ;;
"ip netns pids "*) [ ! -f "$state/netns-$3" ] || head -n 1 "$state/netns-$3" ;;
"nsenter "*" ssh "*" true") grep -q "sshd -D " "$DT_FAKE_LOG" || exit 255 ;;
"kill -9 "*) shift; for f in "$state"/netns-*; do for p; do grep -vx "$p" "$f" > "$f.new"; mv "$f.new" "$f"; done; done ;;
esac`
	for _, name := range []string{"ip", "nsenter", "unshare", "mount", "umount", "iptables", "tc", "kill", "rpcinfo"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+fake+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte("#!/bin/sh\n[ \"$1\" = -n ] || exit 1; shift; exec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DT_FAKE_LOG", log)
	realEuid, realWaits := geteuid, clientWaits
	geteuid = func() int { return 1000 }
	clientWaits = [2]time.Duration{time.Second, 10 * time.Millisecond}
	t.Cleanup(func() { geteuid, clientWaits = realEuid, realWaits })
	return func() []string {
		data, _ := os.ReadFile(log)
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}, dir
}

// Two clients: first the clients of a dt that is gone are removed, and a
// running one's stay; then the first bridge free of another fixture's is
// theirs, with its subnet, its alias naming dt and their directory; each
// client is a network namespace on it, with its own address, and a mount and
// a UTS namespace in files, which every command run there enters, so its
// private /run holds its own rpcbind and rpc.statd, started there, and its
// hostname is its own. Each reaches the server's NFS port. The second one's
// traffic is copied into the first one's interface, and its sshd lets the
// user dt runs as in from the first with the fixture's key, which only the
// first one's ssh configuration names, in place of the user's own there.
// down removes it all, every process in a namespace before the namespace.
func TestClients(t *testing.T) {
	calls, state := fakeClientTools(t)
	t.Setenv("DT_FAKE_TAKEN", "dtnfs0")
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	dead := filepath.Join(t.TempDir(), "dt-clients-dead")
	if err := os.Mkdir(dead, 0o755); err != nil {
		t.Fatal(err)
	}
	notOurs := t.TempDir()
	bridges := fmt.Sprintf(`[{"ifname":"docker0"},{"ifname":"dtnfs0","ifalias":"dt-%d /tmp/dt-clients-live"},{"ifname":"dtnfs5","ifalias":"dt-%[2]d %[3]s"},{"ifname":"dtnfs6","ifalias":"dt-%[2]d %[4]s"}]`,
		os.Getpid(), gone.Process.Pid, dead, notOurs)
	if err := os.WriteFile(filepath.Join(state, "bridges"), []byte(bridges), 0o644); err != nil {
		t.Fatal(err)
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	other := *u
	other.HomeDir = t.TempDir()
	userConfig := filepath.Join(other.HomeDir, ".ssh", "config")
	if err := os.MkdirAll(filepath.Dir(userConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userConfig, []byte("Host *\n  User someone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	realUser := currentUser
	currentUser = func() (*user.User, error) { return &other, nil }
	t.Cleanup(func() { currentUser = realUser })
	var w bytes.Buffer
	c, err := upClients(context.Background(), 2, 40001, t.TempDir(), &w)
	if err != nil {
		t.Fatalf("%v\n%s", err, w.String())
	}
	if c.bridge != "dtnfs1" || c.server() != "10.51.1.1" || c.addr(1) != "10.51.1.2" || c.addr(2) != "10.51.1.3" {
		t.Fatalf("bridge %s, server %s, clients %s and %s", c.bridge, c.server(), c.addr(1), c.addr(2))
	}
	up := calls()
	has := func(log []string, call string) bool { return slices.Contains(log, call) }
	if reaped, claimed := slices.Index(up, "ip link del dtnfs5"), slices.Index(up, "ip link add dtnfs0 type bridge"); reaped < 0 || reaped > claimed || !has(up, "ip netns del dtnfs5-2") || !has(up, "iptables -D FORWARD -i dtnfs5 -o dtnfs5 -j ACCEPT") {
		t.Errorf("the gone dt's clients not removed before the claim:\n%s", strings.Join(up, "\n"))
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Errorf("the gone dt's directory stays: %v", err)
	}
	if has(up, "ip link del dtnfs0") {
		t.Error("a running dt's clients removed")
	}
	if _, err := os.Stat(notOurs); has(up, "ip link del dtnfs6") || err != nil {
		t.Errorf("clients whose alias names no clients' directory removed: %v", err)
	}
	for _, call := range []string{"ip link add dtnfs0 type bridge", "ip link add dtnfs1 type bridge", "iptables -I FORWARD -i dtnfs1 -o dtnfs1 -j ACCEPT",
		fmt.Sprintf("ip link set dtnfs1 alias dt-%d %s", os.Getpid(), c.dir),
		"tc filter add dev dtnfs1h2 ingress matchall action mirred egress mirror dev dtnfs1h1",
		"tc filter add dev dtnfs1h2 egress matchall action mirred egress mirror dev dtnfs1h1"} {
		if !has(up, call) {
			t.Errorf("no %q", call)
		}
	}
	for i := 1; i <= 2; i++ {
		name := fmt.Sprintf("dtnfs1-%d", i)
		files := fmt.Sprintf("--mount=%s --uts=%s", c.ns(i, "mnt"), c.ns(i, "uts"))
		for _, call := range []string{"ip netns add " + name, fmt.Sprintf("ip -n %s addr add 10.51.1.%d/24 dev dtnfs1c%d", name, i+1, i),
			"unshare " + files + " --propagation private true"} {
			if !has(up, call) {
				t.Errorf("client %d: no %q", i, call)
			}
		}
		// What runs in the client, each command entering its namespaces.
		var in []string
		for _, call := range up {
			if rest, ok := strings.CutPrefix(call, "nsenter --net=/run/netns/"+name+" "+files+" -- "); ok {
				in = append(in, rest)
			}
		}
		run, rpcbind, statd := slices.Index(in, "mount -t tmpfs -o mode=0755 tmpfs /run"), slices.Index(in, "rpcbind"), slices.Index(in, "rpc.statd --no-notify -P /run/statd")
		if run < 0 || rpcbind < run || statd < rpcbind {
			t.Errorf("client %d: /run at %d, rpcbind at %d, rpc.statd at %d:\n%s", i, run, rpcbind, statd, strings.Join(in, "\n"))
		}
		for _, call := range []string{"hostname " + name, "mount --bind " + filepath.Join(c.dir, "hosts") + " /etc/hosts", "rpcinfo -n 40001 -t 10.51.1.1 100003"} {
			if !has(in, call) {
				t.Errorf("client %d: no %q in:\n%s", i, call, strings.Join(in, "\n"))
			}
		}
		for _, at := range []string{"/etc/ssh/ssh_config", userConfig} {
			if bound := has(in, "mount --bind "+filepath.Join(c.dir, "ssh_config")+" "+at); bound != (i == 1) {
				t.Errorf("client %d: its ssh configuration bound at %s: %t", i, at, bound)
			}
		}
		if sshd := slices.ContainsFunc(in, func(s string) bool { return strings.HasSuffix(s, "sshd -D -e -f "+filepath.Join(c.dir, "sshd_config")) }); sshd != (i == 2) {
			t.Errorf("client %d: runs sshd: %t", i, sshd)
		}
		if ssh := slices.ContainsFunc(in, func(s string) bool {
			return strings.HasPrefix(s, "setpriv --reuid="+u.Uid+" --regid="+u.Gid+" --init-groups -- env ") && strings.HasSuffix(s, " ssh 10.51.1.3 true")
		}); ssh != (i == 1) {
			t.Errorf("client %d: reaches the second's sshd as the user: %t", i, ssh)
		}
	}
	for name, want := range map[string]string{
		"hosts":       "10.51.1.2 dtnfs1-1\n10.51.1.3 dtnfs1-2\n",
		"ssh_config":  "Host 10.51.1.3\n  IdentityFile " + filepath.Join(c.dir, "id") + "\n",
		"sshd_config": "ListenAddress 10.51.1.3\n",
	} {
		if data, err := os.ReadFile(filepath.Join(c.dir, name)); err != nil || !strings.Contains(string(data), want) {
			t.Errorf("%s: %q, %v; want it to hold %q", name, data, err, want)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(c.dir, "sshd_config")); !strings.Contains(string(data), "AllowUsers "+u.Username+"\n") {
		t.Errorf("sshd_config admits another user:\n%s", data)
	}
	if data, _ := os.ReadFile(filepath.Join(c.dir, "ssh_config")); !strings.Contains(string(data), "  ControlMaster auto\n  ControlPath "+filepath.Join(c.dir, "ssh-%C")+"\n  ControlPersist yes\n") {
		t.Errorf("ssh_config doesn't keep one connection to the second client, in the fixture's directory:\n%s", data)
	}

	c.down()
	down := calls()[len(up):]
	for _, call := range []string{"kill -9 4242", "kill -9 4243", "ip link del dtnfs1h1", "ip link del dtnfs1h2", "ip netns del dtnfs1-1", "ip netns del dtnfs1-2", "umount " + c.ns(2, "mnt") + " " + c.ns(2, "uts"),
		"umount " + filepath.Join(c.dir, "ns"), "ip link del dtnfs1", "iptables -D FORWARD -i dtnfs1 -o dtnfs1 -j ACCEPT"} {
		if !has(down, call) {
			t.Errorf("down: no %q in:\n%s", call, strings.Join(down, "\n"))
		}
	}
	if slices.Index(down, "ip netns del dtnfs1-1") < slices.Index(down, "kill -9 4243") {
		t.Errorf("down: a namespace deleted before its processes end:\n%s", strings.Join(down, "\n"))
	}
	if _, err := os.Stat(c.dir); !os.IsNotExist(err) {
		t.Errorf("the clients' directory stays: %v", err)
	}
}

// A command of a test on a namespaced client runs there as the user dt runs
// as, with its environment given explicitly, since sudo resets it, in its
// directory; dt running as root runs it there as root, with its own.
func TestClientCommand(t *testing.T) {
	realEuid := geteuid
	t.Cleanup(func() { geteuid = realEuid })
	geteuid = func() int { return 1000 }
	s := &Server{clients: &clients{bridge: "dtnfs7", dir: "/c"}}
	got, err := s.Command([]string{"prog", "arg"}, "/work", []string{"A=1"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := user.Current()
	want := []string{"sudo", "-n", "nsenter", "--net=/run/netns/dtnfs7-1", "--mount=/c/ns/1.mnt", "--uts=/c/ns/1.uts", "--wd=/work", "--",
		"setpriv", "--reuid=" + u.Uid, "--regid=" + u.Gid, "--init-groups", "--", "env", "PATH=" + os.Getenv("PATH"), "HOME=" + u.HomeDir, "USER=" + u.Username, "LOGNAME=" + u.Username, "A=1", "prog", "arg"}
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	geteuid = func() int { return 0 }
	if got, _ := s.Command([]string{"prog"}, "", nil); !slices.Equal(got, []string{"nsenter", "--net=/run/netns/dtnfs7-1", "--mount=/c/ns/1.mnt", "--uts=/c/ns/1.uts", "--", "prog"}) {
		t.Errorf("as root: %q", got)
	}
	if got, _ := (&Server{}).Command([]string{"prog"}, "/work", []string{"A=1"}); !slices.Equal(got, []string{"prog"}) {
		t.Errorf("without a client: %q", got)
	}
}

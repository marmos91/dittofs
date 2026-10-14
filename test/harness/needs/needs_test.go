package needs

import (
	"net"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The machine's own platform is met and the others aren't.
func TestPlatforms(t *testing.T) {
	for need, goos := range platforms {
		if err := Check(need); (err == nil) != (goos == runtime.GOOS) {
			t.Errorf("%s on %s: %v", need, runtime.GOOS, err)
		}
	}
}

// A port is met while it's free, and unmet once something listens on it, on
// every address or on loopback alone.
func TestPort(t *testing.T) {
	for _, addr := range []string{":0", "127.0.0.1:0"} {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
		if err := Check("port:" + port); err == nil {
			t.Errorf("a port in use on %s is met", addr)
		}
		_ = l.Close()
		if err := Check("port:" + port); err != nil {
			t.Errorf("a free port: %v", err)
		}
	}
}

// A tool missing from the PATH is unmet, with the program it looked for.
func TestToolNotFound(t *testing.T) {
	if tools["kerberos"][runtime.GOOS] == "" {
		t.Skip("no kerberos tool on this platform")
	}
	t.Setenv("PATH", t.TempDir())
	if err := Check("kerberos"); err == nil || !strings.Contains(err.Error(), "kinit") {
		t.Errorf("got %v", err)
	}
}

// Needs dt can't meet or check yet are unmet, never met.
func TestUnmetUntilChecked(t *testing.T) {
	got := Unmet([]string{"host:dedicated", "service:localstack", "cloud:aws", "disk:999999"})
	if len(got) != 4 {
		t.Errorf("got %q", got)
	}
}

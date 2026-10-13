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

// A port is met while it's free and unmet once something listens on it.
func TestPort(t *testing.T) {
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := Check("port:" + strconv.Itoa(port)); err == nil {
		t.Error("a port in use is met")
	}
	_ = l.Close()
	if err := Check("port:" + strconv.Itoa(port)); err != nil {
		t.Errorf("a free port: %v", err)
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

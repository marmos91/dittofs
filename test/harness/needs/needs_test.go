package needs

import (
	"net"
	"runtime"
	"slices"
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
	got := Unmet([]string{"host:dedicated", "cloud:aws", "disk:999999"})
	if len(got) != 3 {
		t.Errorf("got %q", got)
	}
}

// A service is met when the variables its tests read are set and its address
// answers, and its variables pass to a command dt runs as root.
func TestServices(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	t.Setenv("LOCALSTACK_ENDPOINT", "http://127.0.0.1:"+port)
	t.Setenv("DITTOFS_TEST_POSTGRES_DSN", "host=127.0.0.1 port="+port+" dbname=x")
	t.Setenv("DITTOFS_TEST_KMIP", "")
	t.Setenv("DITTOFS_TEST_KMIP_ENDPOINT", "127.0.0.1:"+port)
	for need, want := range map[string]string{
		"service:localstack": "",
		"service:postgres":   "",
		"service:kmip":       "DITTOFS_TEST_KMIP isn't 1",
	} {
		if err := Check(need); (err == nil) != (want == "") || err != nil && err.Error() != want {
			t.Errorf("%s: %v, want %q", need, err, want)
		}
	}
	t.Setenv("LOCALSTACK_ENDPOINT", "")
	if err := Check("service:localstack"); err == nil || err.Error() != "LOCALSTACK_ENDPOINT isn't set" {
		t.Errorf("unset: %v", err)
	}
	_ = l.Close()
	if err := Check("service:postgres"); err == nil || !strings.Contains(err.Error(), "nothing answers") {
		t.Errorf("nothing listening: %v", err)
	}
	env := ServiceEnv([]string{"linux", "service:kmip"})
	if !slices.Contains(env, "DITTOFS_TEST_KMIP_ENDPOINT=127.0.0.1:"+port) || slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "DITTOFS_TEST_POSTGRES") }) {
		t.Errorf("service env %q", env)
	}
}

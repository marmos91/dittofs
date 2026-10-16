package needs

import (
	"cmp"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The machine's own platform is met and the others aren't.
func TestPlatforms(t *testing.T) {
	for need, goos := range platforms {
		if err := Check("", need); (err == nil) != (goos == runtime.GOOS) {
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
		if err := Check("", "port:"+port); err == nil {
			t.Errorf("a port in use on %s is met", addr)
		}
		_ = l.Close()
		if err := Check("", "port:"+port); err != nil {
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
	if err := Check("", "kerberos"); err == nil || !strings.Contains(err.Error(), "kinit") {
		t.Errorf("got %v", err)
	}
}

// Needs dt can't meet or check yet are unmet, never met.
func TestUnmetUntilChecked(t *testing.T) {
	for _, n := range []string{"host:dedicated", "cloud:aws", "disk:999999"} {
		if Check("", n) == nil {
			t.Errorf("%s is met", n)
		}
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
		if err := Check("", need); (err == nil) != (want == "") || err != nil && err.Error() != want {
			t.Errorf("%s: %v, want %q", need, err, want)
		}
	}
	_ = l.Close()
	if err := Check("", "service:postgres"); err == nil || !strings.Contains(err.Error(), "nothing answers") {
		t.Errorf("nothing listening: %v", err)
	}
	env := ServiceEnv("", []string{"linux", "service:kmip"})
	if !slices.Contains(env, "DITTOFS_TEST_KMIP_ENDPOINT=127.0.0.1:"+port) || slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "DITTOFS_TEST_POSTGRES") }) {
		t.Errorf("service env %q", env)
	}
}

// A service dt services doesn't start is unmet.
func TestUnknownService(t *testing.T) {
	if err := Check("", "service:nosuch"); err == nil {
		t.Error("an unknown service is met")
	}
}

// With its address unset, a service falls back to the local service's values,
// as dt services starts it: written here for localstack and postgres, read
// from the file it writes for KMIP. None of them is dialled here but KMIP's,
// which points at a listener of the test's own.
func TestServiceFallback(t *testing.T) {
	for _, k := range []string{"LOCALSTACK_ENDPOINT", "DITTOFS_TEST_POSTGRES_DSN", "DITTOFS_TEST_KMIP", "DITTOFS_TEST_KMIP_ENDPOINT"} {
		t.Setenv(k, "")
	}
	root := t.TempDir()
	if got, _ := serviceVars(root, "localstack"); !slices.Equal(got, []string{"LOCALSTACK_ENDPOINT=http://127.0.0.1:4566"}) {
		t.Errorf("localstack: %q", got)
	}
	if got, _ := serviceVars(root, "postgres"); len(got) != 1 || !strings.Contains(got[0], "port=15432 dbname=dittofs_test") {
		t.Errorf("postgres: %q", got)
	}
	if err := Check(root, "service:kmip"); err == nil || !strings.Contains(err.Error(), "test/harness/state/kmip.env") {
		t.Errorf("kmip without its file: %v", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	file := filepath.Join(root, "test/harness/state/kmip.env")
	lines := "DITTOFS_TEST_KMIP=1\nDITTOFS_TEST_KMIP_ENDPOINT=" + l.Addr().String() + "\nDITTOFS_TEST_KMIP_CA=/certs/ca.pem\n"
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Check(root, "service:kmip"); err != nil {
		t.Errorf("kmip from its file: %v", err)
	}
	if got := ServiceEnv(root, []string{"service:kmip"}); len(got) != 3 || got[2] != "DITTOFS_TEST_KMIP_CA=/certs/ca.pem" {
		t.Errorf("kmip's variables: %q", got)
	}
}

// A tool's hint names the program its check looks for, with a Debian and
// Ubuntu package only on the platform that uses those. With no program here,
// it names the platforms that have one, and dtc only when its image has the
// tool. A service is started with dt services. Fake tools check each rule on
// every platform.
func TestHint(t *testing.T) {
	elsewhere := map[string]string{"darwin": "x", "linux": "x", "windows": "x"}
	delete(elsewhere, runtime.GOOS)
	tools["elsewhere"] = elsewhere
	tools["here"] = map[string]string{runtime.GOOS: "prog"}
	debian["here"] = "pkg"
	t.Cleanup(func() {
		delete(tools, "elsewhere")
		delete(tools, "here")
		delete(debian, "here")
		delete(inDtc, "elsewhere")
	})
	others := map[string]string{"darwin": "run it on Linux or Windows", "linux": "run it on macOS or Windows", "windows": "run it on macOS or Linux"}[runtime.GOOS]
	install := map[bool]string{true: "install prog (Debian and Ubuntu: pkg)", false: "install prog"}[runtime.GOOS == "linux"]
	for need, want := range map[string]string{"elsewhere": others, "here": install, "linux": "run it on Linux, or in dtc", "service:kmip": "dt services up kmip"} {
		if got := Hint(need); got != want {
			t.Errorf("%s: %q, want %q", need, got, want)
		}
	}
	inDtc["elsewhere"] = true
	if got := Hint("elsewhere"); got != others+", or in dtc" {
		t.Errorf("elsewhere, in dtc: %q", got)
	}
}

// The tools the hints place in dtc are the ones whose packages its image
// installs.
func TestInDtc(t *testing.T) {
	data, err := os.ReadFile("../docker/dev.Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	words := strings.Fields(string(data))
	for need, bins := range tools {
		if pkg := cmp.Or(debian[need], bins["linux"]); pkg != "" && slices.Contains(words, pkg) != inDtc[need] {
			t.Errorf("%s: package %s in the image %t, in dtc %t", need, pkg, slices.Contains(words, pkg), inDtc[need])
		}
	}
}

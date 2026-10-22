package needs

import (
	"cmp"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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

// refusedPort is a port nothing on loopback listens on, and unlike a port a
// test frees, nothing else gets it meanwhile: it's never handed out as a free
// port, and an ordinary user may not bind it.
const refusedPort = "1"

// A port is met while it's free, and unmet once something listens on it, on
// every address or on loopback alone. The port is the first one met from
// 1024, below those a system hands out as free ports, so nothing else takes
// it between the test freeing it and the check binding it.
func TestPort(t *testing.T) {
	p := 1024
	for ; Check("", "port:"+strconv.Itoa(p)) != nil; p++ {
		if p == 32767 {
			t.Fatal("no port from 1024 to 32767 is met")
		}
	}
	port := strconv.Itoa(p)
	for _, addr := range []string{":", "127.0.0.1:"} {
		l, err := net.Listen("tcp", addr+port)
		if err != nil {
			t.Fatal(err)
		}
		if err := Check("", "port:"+port); err == nil {
			t.Errorf("a port in use on %s is met", addr+port)
		}
		_ = l.Close()
		if err := Check("", "port:"+port); err != nil {
			t.Errorf("a free port: %v", err)
		}
	}
}

// A port this user may not bind is met while nothing on loopback accepts a
// connection on it, and unmet once something does.
func TestPortNotBindable(t *testing.T) {
	realListen := listen
	t.Cleanup(func() { listen = realListen })
	listen = func(string, string) (net.Listener, error) { return nil, fmt.Errorf("bind: %w", os.ErrPermission) }
	l, err := realListen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	if err := Check("", "port:"+port); err == nil {
		t.Error("a port something accepts on is met")
	}
	if err := Check("", "port:"+refusedPort); err != nil {
		t.Errorf("a port nothing accepts on: %v", err)
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

// The machine's own podman runs the scenarios from 5.6 on, compared as
// numbers. An older one, or none, leaves the need met only where docker
// runs, in podman's own image, and unmet with the reason otherwise. A podman
// that fails leaves it unmet even with docker, never met in the image.
func TestPodman(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake podman and docker are sh scripts")
	}
	const failed = "podman info --format {{.Version.Version}}: exit status 1"
	for _, c := range []struct {
		podman string // the version the fake podman reports, or "fail"; none when empty
		docker bool
		image  bool   // met in podman's image
		need   string // the need's error, "" for none
	}{
		{podman: "5.6.0"},
		{podman: "5.10.1"},
		{podman: "6.0.0"},
		{podman: "5.6.0", docker: true},
		{podman: "4.9.3", need: "podman 4.9.3: the scenarios need 5.6 or later"},
		{podman: "4.9.3", docker: true, image: true},
		{podman: "fail", need: failed},
		{podman: "fail", docker: true, need: failed},
		{need: "podman not found"},
		{docker: true, image: true},
	} {
		bin := t.TempDir()
		t.Setenv("PATH", bin)
		switch c.podman {
		case "":
		case "fail":
			writeScript(t, filepath.Join(bin, "podman"), "exit 1")
		default:
			writeScript(t, filepath.Join(bin, "podman"), `[ "$*" = "info --format {{.Version.Version}}" ] && echo `+c.podman)
		}
		if c.docker {
			writeScript(t, filepath.Join(bin, "docker"), `[ "$*" = info ]`)
		}
		errText := func(err error) string {
			if err == nil {
				return ""
			}
			return err.Error()
		}
		if image, err := Podman(); image != c.image || errText(err) != c.need {
			t.Errorf("podman %q, docker %t: Podman %t, %q, want %t, %q", c.podman, c.docker, image, errText(err), c.image, c.need)
		}
		if got := errText(Check("", "podman")); got != c.need {
			t.Errorf("podman %q, docker %t: Check %q, want %q", c.podman, c.docker, got, c.need)
		}
	}
}

// writeScript writes an sh script that runs body.
func writeScript(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile(name, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
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
	t.Setenv("DITTOFS_TEST_POSTGRES_DSN", "host=127.0.0.1 port="+port+" dbname=x")
	t.Setenv("DITTOFS_TEST_KMIP", "")
	t.Setenv("DITTOFS_TEST_KMIP_ENDPOINT", "127.0.0.1:"+port)
	for need, want := range map[string]string{
		"service:postgres": "",
		"service:kmip":     "DITTOFS_TEST_KMIP isn't 1",
	} {
		if err := Check("", need); (err == nil) != (want == "") || err != nil && err.Error() != want {
			t.Errorf("%s: %v, want %q", need, err, want)
		}
	}
	t.Setenv("DITTOFS_TEST_POSTGRES_DSN", "host=127.0.0.1 port="+refusedPort+" dbname=x")
	if err := Check("", "service:postgres"); err == nil || !strings.Contains(err.Error(), "nothing answers") {
		t.Errorf("nothing listening: %v", err)
	}
	env := ServiceEnv("", []string{"linux", "service:kmip"})
	if !slices.Contains(env, "DITTOFS_TEST_KMIP_ENDPOINT=127.0.0.1:"+port) || slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "DITTOFS_TEST_POSTGRES") }) {
		t.Errorf("service env %q", env)
	}
}

// service:localstack is met only when Localstack's own health check says its
// S3 is up, as dt services waits for when it starts Localstack. A port that
// takes a connection isn't enough: dtc forwards Localstack's port, and its
// forwarder accepts a connection whether or not Localstack runs.
func TestLocalstackHealth(t *testing.T) {
	health := func(body string) string {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/_localstack/health" {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(s.Close)
		return s.URL
	}
	refused := "http://127.0.0.1:" + refusedPort
	other := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(other.Close)
	for _, c := range []struct{ name, endpoint, want string }{
		{"S3 running", health(`{"services": {"s3": "running", "sqs": "disabled"}}`), ""},
		{"S3 available", health(`{"services": {"s3": "available"}}`) + "/", ""},
		{"S3 off", health(`{"services": {"s3": "disabled"}}`), `says S3 is "disabled", not available or running`},
		{"another HTTP server", other.URL, "answered 404, not with Localstack's health"},
		{"nothing listening", refused, "nothing answers at " + refused + "/_localstack/health"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("LOCALSTACK_ENDPOINT", c.endpoint)
			err := Check("", "service:localstack")
			if (err == nil) != (c.want == "") || err != nil && !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want %q", err, c.want)
			}
		})
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

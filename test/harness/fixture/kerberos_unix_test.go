//go:build !windows

package fixture

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/marmos91/dittofs/test/harness/registry"
)

// fakeKDCDocker puts a docker first on the PATH whose run, of a KDC, writes
// the keytab and krb5.conf into the directory -v mounts, and its port and
// USER_PASSWORD into state, then runs until rm kills it, or with
// FAKE_KDC_FAIL exits at once. Each call's arguments go to state/calls.
func fakeKDCDocker(t *testing.T) (state string) {
	t.Helper()
	dir, state := t.TempDir(), t.TempDir()
	script := `#!/bin/sh
echo "$*" >> ` + state + `/calls
case $1 in
run) ;;
rm) kill "$(cat ` + state + `/pid)"; exit 0 ;;
*) exit 0 ;;
esac
while [ $# -gt 1 ]; do
	case $1 in
	-v) keytabs=${2%:/keytabs}; shift ;;
	-e) case $2 in KDC_PORT=*) port=${2#KDC_PORT=} ;; esac; shift ;;
	esac
	shift
done
[ -z "$FAKE_KDC_FAIL" ] || { echo "kdb5_util: broke"; exit 1; }
: > "$keytabs/krb5.conf"
: > "$keytabs/dittofs.keytab"
echo "$USER_PASSWORD" > ` + state + `/password
echo $$ > ` + state + `/pid
echo "$port" > ` + state + `/port
exec sleep 60
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_KDC_FAIL", "")
	return state
}

// waitFile waits up to a minute for path and returns what it holds, or "".
func waitFile(path string) string {
	for end := time.Now().Add(time.Minute); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}

// With Kerberos the KDC starts before dfs, on the server's network, with the
// server's principal named after {server}, the machine's name, and {user}'s,
// whose password reaches it through docker's environment, never on the
// command line. Once the KDC answers on its port, dfs starts with Kerberos
// on, the KDC's keytab and that principal. The setup's end stops both: the
// KDC's container is removed, as it is when dfs doesn't start. A KDC that
// exits before it answers fails the setup at once, before dfs starts, and
// one that never answers is removed when the setup ends.
func TestStartKerberos(t *testing.T) {
	realHostname := hostname
	hostname = func() (string, error) { return "kdc-host.test", nil }
	t.Cleanup(func() { hostname = realHostname })
	for _, tc := range []struct {
		name                 string
		answers, fail, noDFS bool
	}{
		{name: "answers", answers: true},
		{name: "dfs doesn't start", answers: true, noDFS: true},
		{name: "exits", fail: true},
		{name: "never answers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := fakeKDCDocker(t)
			if tc.fail {
				t.Setenv("FAKE_KDC_FAIL", "1")
			}
			bin := t.TempDir()
			if !tc.noDFS {
				dfs := "#!/bin/sh\ncp \"$4\" " + state + "/copy && mv " + state + "/copy " + state + "/config.yaml\nexec sleep 60\n"
				if err := os.WriteFile(filepath.Join(bin, "dfs"), []byte(dfs), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			go func() {
				port := waitFile(filepath.Join(state, "port"))
				if !tc.answers {
					time.Sleep(300 * time.Millisecond)
					cancel()
					return
				}
				l, err := net.Listen("tcp", "127.0.0.1:"+port)
				if err != nil {
					return
				}
				defer func() { _ = l.Close() }()
				go func() {
					for conn, err := l.Accept(); err == nil; conn, err = l.Accept() {
						_ = conn.Close()
					}
				}()
				for ctx.Err() == nil {
					if _, err := os.Stat(filepath.Join(state, "config.yaml")); err == nil {
						cancel()
					}
					time.Sleep(10 * time.Millisecond)
				}
			}()
			out := t.TempDir()
			var w bytes.Buffer
			_, err := Start(ctx, registry.Fixture{Profile: "memory", SMB: registry.SMB{Enabled: true}, Kerberos: true}, nil, bin, out, &w)
			data, _ := os.ReadFile(filepath.Join(state, "calls"))
			calls := strings.Split(strings.TrimSpace(string(data)), "\n")
			if tc.fail {
				log, _ := os.ReadFile(filepath.Join(out, "kdc.log"))
				if err == nil || !strings.Contains(err.Error(), "the KDC exited before it answered (exit status 1)") || !strings.Contains(string(log), "kdb5_util: broke") {
					t.Errorf("Start: %v; kdc.log %q", err, log)
				}
				if _, err := os.Stat(filepath.Join(state, "config.yaml")); err == nil {
					t.Error("dfs started")
				}
				return
			}
			if err == nil || errors.Is(err, context.Canceled) == tc.noDFS {
				t.Fatalf("Start: %v\n%s", err, w.String())
			}
			run := regexp.MustCompile(`^run --rm --name (dt-kdc-[0-9a-f]{8}) --network ` + regexp.QuoteMeta(Network()) +
				` -v (\S+):/keytabs -e KRB5_REALM=DITTOFS.TEST -e KDC_HOST=127.0.0.1 -e KDC_PORT=(\d+)` +
				` -e DITTOFS_SPN=cifs/kdc-host.test -e DITTOFS_UID=` + strconv.Itoa(os.Getuid()) + ` -e DITTOFS_GID=` + strconv.Itoa(os.Getgid()) +
				` -e USER_PRINCIPAL=dt-user -e USER_PASSWORD dittofs-dt-kdc$`).FindStringSubmatch(calls[0])
			if run == nil || len(calls) != 2 || calls[1] != "rm -f "+run[1] {
				t.Fatalf("docker calls:\n%s", data)
			}
			if !ended(waitFile(filepath.Join(state, "pid"))) {
				t.Error("the KDC still runs")
			}
			if _, err := os.Stat(run[2]); !os.IsNotExist(err) || !strings.HasPrefix(run[2], filepath.Join(out, "dfs")) {
				t.Errorf("the KDC's directory %s stays with the server's data: %v", run[2], err)
			}
			password := waitFile(filepath.Join(state, "password"))
			if len(password) != 32 || strings.Contains(string(data), password) || strings.Contains(w.String(), password) {
				t.Errorf("USER_PASSWORD %q, on the command line or in dt's lines:\n%s\n%s", password, data, w.String())
			}
			if !tc.answers || tc.noDFS {
				return
			}
			if want := fmt.Sprintf("[dt] fixture: the KDC of DITTOFS.TEST answers on port %s,", run[3]); !strings.Contains(w.String(), want) {
				t.Errorf("dt's lines lack %q:\n%s", want, w.String())
			}
			var config struct {
				Kerberos map[string]any `yaml:"kerberos"`
			}
			raw, _ := os.ReadFile(filepath.Join(state, "config.yaml"))
			if err := yaml.Unmarshal(raw, &config); err != nil {
				t.Fatal(err)
			}
			k := config.Kerberos
			if k["enabled"] != true || k["keytab_path"] != filepath.Join(run[2], "dittofs.keytab") || k["krb5_conf"] != filepath.Join(run[2], "krb5.conf") ||
				k["service_principal"] != "cifs/kdc-host.test@DITTOFS.TEST" {
				t.Errorf("dfs's kerberos: %v", k)
			}
		})
	}
}

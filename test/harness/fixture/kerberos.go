package fixture

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// Realm is {realm}, the realm of a fixture's KDC.
const Realm = "DITTOFS.TEST"

// KDCImage is the image a fixture's KDC runs, which dt builds from
// test/smb-conformance/kdc before the fixture starts.
const KDCImage = "dittofs-dt-kdc"

// kdc is a fixture's KDC: a container on the server's network that holds the
// server's principal and {user}'s, with {password}.
type kdc struct {
	name   string // the container's
	dir    string // where it writes the server's keytab and krb5.conf
	host   string // {server}, the name the server's principal holds
	client *exec.Cmd
	log    *os.File
	exited chan struct{} // closed once the docker client has exited
	err    error         // how it exited, once exited is closed
}

// principal is the server's principal, without its realm: an SMB client
// asks for a ticket to cifs/ and the name it connects to.
func (k *kdc) principal() string { return "cifs/" + k.host }

// startKDC starts a KDC for the server named host on port, its output in
// out/kdc.log, and waits until it answers, which it does once it has written
// the server's keytab into dir and added {user}: starting the KDC is its
// entrypoint's last step.
func startKDC(ctx context.Context, dir, host, password string, port int, out string) (*kdc, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	log, err := os.Create(filepath.Join(out, "kdc.log"))
	if err != nil {
		return nil, err
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	k := &kdc{name: "dt-kdc-" + hex.EncodeToString(b), dir: dir, host: host, log: log, exited: make(chan struct{})}
	// decision: -e names USER_PASSWORD without its value, which docker takes
	// from its own environment, so {password} stays off the command line.
	k.client = exec.Command("docker", "run", "--rm", "--name", k.name, "--network", Network(), "-v", dir+":/keytabs",
		"-e", "KRB5_REALM="+Realm, "-e", "KDC_HOST=127.0.0.1", "-e", "KDC_PORT="+strconv.Itoa(port),
		"-e", "DITTOFS_SPN="+k.principal(), "-e", "DITTOFS_UID="+strconv.Itoa(os.Getuid()), "-e", "DITTOFS_GID="+strconv.Itoa(os.Getgid()),
		"-e", "USER_PRINCIPAL="+User, "-e", "USER_PASSWORD", KDCImage)
	k.client.Env = append(os.Environ(), "USER_PASSWORD="+password)
	k.client.Stdout, k.client.Stderr = k.log, k.log
	if err := k.client.Start(); err != nil {
		_ = k.log.Close()
		return nil, err
	}
	go func() {
		k.err = k.client.Wait()
		close(k.exited)
	}()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for {
		if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			_ = conn.Close()
			return k, nil
		}
		select {
		case <-k.exited:
			_ = k.log.Close()
			return nil, fmt.Errorf("the KDC exited before it answered (%v); its output is kdc.log in the cell's results", k.err)
		case <-ctx.Done():
			k.stop()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// stop removes the KDC's container, which killing its docker client would
// leave running, and waits stopGrace for the client to exit before it kills
// it. Removing the container has a minute of its own.
//
// ponytail: a KDC whose dt was killed keeps running until it is removed by
// hand, by its name dt-kdc-*, as does one whose container docker created
// after the removal. Have dt remove such a KDC before it starts the next if
// that shows up.
func (k *kdc) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", k.name).Run()
	select {
	case <-k.exited:
	case <-time.After(stopGrace):
		_ = k.client.Process.Kill()
		<-k.exited
	}
	_ = k.log.Close()
}

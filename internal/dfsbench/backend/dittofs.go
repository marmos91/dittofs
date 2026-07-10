package backend

import (
	"context"
	"fmt"
	"os"

	"github.com/marmos91/dittofs/internal/dfsbench/exec"
)

// dittofs-s3 is the subject: DittoFS serving badger metadata + an S3 remote
// block store, mounted over its NATIVE nfs3/nfs4/smb3 servers (no re-export
// layer — that's the whole point of the comparison). Its cells pair against the
// FUSE competitors' re-exported cells to expose the FUSE context-switch tax.
//
// Mount strings and `store block evict` are the documented interface (see
// docs/guide/nfs.md, dfsctl). The server bringup (config schema + dfsctl
// bootstrap) is pinned against a live dfs on the VM — the first managed run is
// where it gets tuned.
const (
	dittofsNFSPort = "12049"
	dittofsSMBPort = "12445"
	dittofsShare   = "bench"
	dittofsDataDir = "/var/lib/bench-dittofs"
	dittofsAPIPort = "8080"
	dittofsAPIURL  = "http://127.0.0.1:" + dittofsAPIPort
	dittofsMeta    = "bench-meta"
	dittofsLocal   = "bench-local"
	dittofsRemote  = "bench-s3"
	// Throwaway control-plane secret (≥32 chars, required for the API server) and
	// admin password on a disposable single-tenant bench VM — same fixed-literal
	// convention as zerofsPassword. ponytail: no prod users; don't generate.
	dittofsSecret    = "dfsbench-controlplane-secret-0123456789ab"
	dittofsAdminPass = "dfsbench-admin-pw"
)

func init() {
	register(&Backend{
		Name:     "dittofs-s3",
		S3Backed: true,
		Support:  map[Protocol]Support{ProtoNFS3: Native, ProtoNFS4: Native, ProtoSMB3: Native},
		Setup:    dittofsSetup,
		Mount:    dittofsMount,
		Evict:    dittofsEvict,
		Unmount:  func(ctx context.Context, _ Protocol) error { return exec.Sh(ctx, "umount", clientMntDir) },
		Teardown: dittofsTeardown,
	})
}

func dittofsSetup(ctx context.Context, env BackendEnv) error {
	id, secret, err := s3Creds()
	if err != nil {
		return err
	}
	// Kill any dfs left over by a prior run and WAIT for it to actually die before
	// wiping state. This is what makes a same-VM re-run — e.g. an A/B binary swap —
	// safe. A still-live old dfs holds the BadgerDB directory lock and keeps
	// rewriting controlplane.db, so racing rm+start against it made the
	// metadata-store create fail "cannot acquire directory lock"; worse, the rm
	// deleted SST files out from under the live process (badger "no such file"
	// spam) while the new `dfs start` reported "DittoFS is already running". So kill
	// by both cmdline and exact name, free the NFS port, and if dfs still refuses to
	// die, ABORT the cell rather than wipe state under it — a clean FAIL beats
	// corrupting the store and mis-attributing the result to the binary under test.
	//
	// Match dfs by EXACT process name (-x dfs), never by `-f 'dfs start'`: the -f
	// pattern is matched against every process's full cmdline, and THIS cleanup
	// shell's own argv contains the literal "dfs start", so `pkill -f 'dfs start'`
	// SIGKILLs its own parent shell before the rm runs ("signal: killed"). -x dfs
	// matches the server's comm ("dfs") and can't self-match the "sh" running this.
	clean := "pkill -9 -x dfs 2>/dev/null; " +
		"for i in $(seq 1 40); do pgrep -x dfs >/dev/null 2>&1 || break; sleep 0.5; done; " +
		"if pgrep -x dfs >/dev/null 2>&1; then echo 'dfs still alive after SIGKILL — refusing to wipe state under it' >&2; exit 1; fi; " +
		"rm -rf ~/.config/dittofs ~/.local/state/dittofs ~/.config/dfsctl " + dittofsDataDir
	if err := exec.Sh(ctx, "sh", "-c", clean); err != nil {
		return fmt.Errorf("dittofs setup: pre-start cleanup failed (stale dfs?): %w", err)
	}
	if err := os.MkdirAll(dittofsDataDir+"/meta", 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(dittofsDataDir+"/blocks", 0o755); err != nil {
		return err
	}
	// Start the server with the required control-plane secret (else API-server
	// bringup fails "JWT secret must be at least 32 characters") and a known admin
	// password (background mode generates an unrecoverable one otherwise), then
	// wait for its NFS port before driving dfsctl.
	start := fmt.Sprintf("DITTOFS_CONTROLPLANE_SECRET=%s DITTOFS_ADMIN_INITIAL_PASSWORD=%s "+
		"dfs start >/var/log/bench-dittofs.log 2>&1 &", dittofsSecret, dittofsAdminPass)
	if err := exec.Sh(ctx, "sh", "-c", start); err != nil {
		return err
	}
	if err := waitPort(ctx, dittofsNFSPort); err != nil {
		return fmt.Errorf("dfs did not open NFS port %s: %w", dittofsNFSPort, err)
	}
	// The API server (8080) can come up after the NFS listener; wait for it too so
	// the login below doesn't race a not-yet-listening control plane.
	if err := waitPort(ctx, dittofsAPIPort); err != nil {
		return fmt.Errorf("dfs did not open API port %s: %w", dittofsAPIPort, err)
	}
	// dfsctl talks to the authenticated control-plane API — log in first, then
	// build the store stack a share needs: a metadata store, a local block store
	// (cache), and the S3 remote. Creds go on flags (not a config file); this is a
	// throwaway VM and they never hit the argv of a long-lived process.
	if err := exec.Sh(ctx, "dfsctl", "login",
		"--server", dittofsAPIURL, "--username", "admin", "--password", dittofsAdminPass); err != nil {
		return fmt.Errorf("dfsctl login: %w", err)
	}
	if err := exec.Sh(ctx, "dfsctl", "store", "metadata", "add",
		"--name", dittofsMeta, "--type", "badger", "--db-path", dittofsDataDir+"/meta"); err != nil {
		return err
	}
	if err := exec.Sh(ctx, "dfsctl", "store", "block", "local", "add",
		"--name", dittofsLocal, "--type", "fs", "--path", dittofsDataDir+"/blocks"); err != nil {
		return err
	}
	if err := exec.Sh(ctx, "dfsctl", "store", "block", "remote", "add",
		"--name", dittofsRemote, "--type", "s3", "--bucket", env.Bucket, "--endpoint", env.Endpoint,
		"--access-key", id, "--secret-key", secret, "--region", "us-east-1"); err != nil {
		return err
	}
	// --default-permission read-write so the AUTH_SYS root client (squashed to
	// nobody) can still write — the benchmark's whole job.
	return exec.Sh(ctx, "dfsctl", "share", "create", "--name", "/"+dittofsShare,
		"--metadata", dittofsMeta, "--local", dittofsLocal, "--remote", dittofsRemote,
		"--default-permission", "read-write")
}

func dittofsMount(ctx context.Context, proto Protocol) (string, error) {
	if err := prepareMountpoint(ctx); err != nil {
		return "", err
	}
	var typ, opts, src string
	switch proto {
	case ProtoNFS3:
		typ, src = "nfs", "127.0.0.1:/"+dittofsShare
		opts = "nfsvers=3,tcp,port=" + dittofsNFSPort + ",mountport=" + dittofsNFSPort + ",actimeo=0,nolock"
	case ProtoNFS4:
		typ, src = "nfs", "127.0.0.1:/"+dittofsShare
		opts = "vers=4.1,tcp,port=" + dittofsNFSPort
	case ProtoSMB3:
		typ, src = "cifs", "//127.0.0.1/"+dittofsShare
		opts = "port=" + dittofsSMBPort + ",guest,vers=3.0"
	default:
		return "", fmt.Errorf("dittofs-s3: unsupported protocol %s", proto)
	}
	if err := exec.Sh(ctx, "mount", "-t", typ, "-o", opts, src, clientMntDir); err != nil {
		return "", err
	}
	return clientMntDir, nil
}

// dittofsEvict drops locally-cached blocks so the next read is cold-from-S3.
// #1595's DrainLocalSynced is what makes `store block evict` actually force it.
//
// Drain queued uploads FIRST: `store block evict` (DrainLocalSynced) only drops
// blocks already synced to S3. A block whose upload is still in flight is left
// on local disk, so the "cold" read serves it from cache — the pass reads warm,
// S3MB stays 0, and cold≈warm. Draining first makes every block synced and
// therefore evictable, so the next read genuinely comes from S3.
func dittofsEvict(ctx context.Context) error {
	if err := exec.Sh(ctx, "dfsctl", "system", "drain-uploads"); err != nil {
		return err
	}
	return exec.Sh(ctx, "dfsctl", "store", "block", "evict")
}

func dittofsTeardown(ctx context.Context) error {
	// SIGKILL + wait for the process to actually exit so a subsequent same-VM run
	// (e.g. an A/B binary swap) starts clean instead of tripping dfs's "already
	// running" guard. Best-effort: a wedged dfs is caught+failed by the next
	// Setup's pre-start cleanup rather than silently wiped under.
	// Match by exact name (-x dfs); a `-f 'dfs start'` pattern would self-match the
	// shell running it (its argv contains the literal). See dittofsSetup's cleanup.
	_ = exec.Sh(ctx, "sh", "-c",
		"pkill -9 -x dfs 2>/dev/null; "+
			"for i in $(seq 1 20); do pgrep -x dfs >/dev/null 2>&1 || break; sleep 0.5; done; true")
	return os.RemoveAll(dittofsDataDir)
}

// waitPort blocks until 127.0.0.1:port accepts a connection or ~60s elapse.
func waitPort(ctx context.Context, port string) error {
	return exec.Sh(ctx, "sh", "-c",
		fmt.Sprintf("for i in $(seq 1 60); do nc -z 127.0.0.1 %s && exit 0; sleep 1; done; exit 1", port))
}

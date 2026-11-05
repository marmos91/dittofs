package backend

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cubbit/dittofs/internal/dfsbench/exec"
)

// benchDataMount is the isolated data volume `dfsbench setup --data-volume-gb`
// attaches (see cloud.attachAndMountDataVolume). Kept in sync with that const.
const benchDataMount = "/bench-data"

// benchDataMarker is the file the mount script drops after a successful mount
// (see cloud.benchDataMarker). Its presence — not the dir's — is what proves the
// volume is mounted, so a failed mount that left a bare /bench-data dir on the
// root disk doesn't silently re-contaminate the root disk. Kept in sync.
const benchDataMarker = ".dfsbench-data-volume"

// benchDataDir is benchDataMount when the data volume is mounted, else "" (fall
// back to legacy root-disk paths). Resolved once at startup: the remote dfsbench
// binary can't see setup's --data-volume-gb flag, so it detects the mount via
// the marker file the mount script wrote.
var benchDataDir = func() string {
	if _, err := os.Stat(filepath.Join(benchDataMount, benchDataMarker)); err == nil {
		return benchDataMount
	}
	return ""
}()

// benchPath returns sub under the isolated data volume when one is mounted, else
// legacyRoot — the exact root-disk path preserved for --data-volume-gb=0. This
// keeps all backend cache/data off the OS root disk when a data volume exists.
func benchPath(legacyRoot, sub string) string {
	if benchDataDir != "" {
		return filepath.Join(benchDataDir, sub)
	}
	return legacyRoot
}

// isUnderBenchData reports whether dir sits strictly inside the isolated data
// volume. It is false when no data volume is mounted (legacy root-disk fallback)
// and false for the mount root itself, so a caller can only ever target a
// backend's own subdir under the volume — never a real system dir.
func isUnderBenchData(dir string) bool {
	if benchDataDir == "" {
		return false
	}
	root := filepath.Clean(benchDataDir)
	return filepath.Clean(dir) != root &&
		strings.HasPrefix(filepath.Clean(dir), root+string(os.PathSeparator))
}

// reclaimBenchData deletes a finished backend's cache/data dir so the isolated
// data volume's usage stays bounded by the largest single system instead of the
// sum of every system that has run. It only removes a path under the data volume
// (isUnderBenchData), so it is a no-op on the legacy root-disk fallback and can
// never wipe a real system dir.
func reclaimBenchData(dir string) {
	if isUnderBenchData(dir) {
		_ = os.RemoveAll(dir)
	}
}

// dittofs-s3 is the subject: DittoFS serving badger metadata + an S3 remote
// block store, mounted over its NATIVE nfs3/nfs4/smb3 servers (no re-export
// layer — that's the whole point of the comparison). Its cells pair against the
// FUSE competitors' re-exported cells to expose the FUSE context-switch tax.
//
// Mount strings are the documented interface (see docs/guide/nfs.md, dfsctl).
// It has no cold barrier and no settle wait: both read the content store's
// stats and evict it, and no content store is wired (block.ErrNoContentStore). The server bringup (config schema + dfsctl
// bootstrap) is pinned against a live dfs on the VM — the first managed run is
// where it gets tuned.
const (
	dittofsNFSPort = "12049"
	dittofsSMBPort = "12445"
	dittofsShare   = "bench"
	dittofsAPIPort = "8080"
	dittofsAPIURL  = "http://127.0.0.1:" + dittofsAPIPort
	dittofsMeta    = "bench-meta"
	dittofsBlocks  = "bench-s3"
	// Throwaway control-plane secret (≥32 chars, required for the API server) and
	// admin password on a disposable single-tenant bench VM — same fixed-literal
	// convention as zerofsPassword. ponytail: no prod users; don't generate.
	dittofsSecret    = "dfsbench-controlplane-secret-0123456789ab"
	dittofsAdminPass = "dfsbench-admin-pw"
)

// dittofsDataDir holds the subject's metadata (/meta, /meta.db) and local block
// cache (/blocks). It lives on the isolated data volume when one is mounted,
// else the legacy root-disk path.
var dittofsDataDir = benchPath("/var/lib/bench-dittofs", "dittofs")

// dittofsServerLog is where dittofsSetup redirects the benched server's stdout
// and stderr.
const dittofsServerLog = "/var/log/bench-dittofs.log"

// dittofsUnboundedMaxSize is the default local-journal cap: generous headroom so
// a sustained write burst measures the tier's throughput, not its saturation
// cliff (see dittofsSetup). The cache-cap study variants pass a small cap instead.
const dittofsUnboundedMaxSize = int64(32) * 1024 * 1024 * 1024

// provisionPostgres installs+starts PostgreSQL (if needed) and (re)creates a
// clean role+database — DROPping any prior db so each run starts empty. Used by
// the juicefs-postgres meta store.
func provisionPostgres(ctx context.Context, db, user, pass string) error {
	script := fmt.Sprintf(`set -eu
command -v pg_isready >/dev/null 2>&1 || { apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y postgresql; }
service postgresql start 2>/dev/null || systemctl start postgresql 2>/dev/null || pg_ctlcluster "$(ls /etc/postgresql 2>/dev/null | head -1)" main start 2>/dev/null || true
# Wait on the local socket as the postgres user — the same path the SQL below
# connects through — for up to 60s. A fresh apt-install has just created the
# cluster, and a shorter TCP probe raced the cluster still coming up (psql exit 2).
for i in $(seq 1 60); do runuser -u postgres -- pg_isready -q 2>/dev/null && break; sleep 1; done
# Fail loudly with cluster diagnostics if it never came up, rather than letting
# the provisioning psql below fail with a cryptic connection error (exit 2).
runuser -u postgres -- pg_isready -q 2>/dev/null || { echo 'postgres cluster never became ready:'; pg_lsclusters 2>/dev/null; exit 1; }
runuser -u postgres -- psql -v ON_ERROR_STOP=1 <<'SQL'
DROP DATABASE IF EXISTS %[1]s;
DO $do$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='%[2]s') THEN CREATE ROLE %[2]s LOGIN PASSWORD '%[3]s'; END IF; END $do$;
CREATE DATABASE %[1]s OWNER %[2]s;
SQL`, db, user, pass)
	return exec.Sh(ctx, "sh", "-c", script)
}

func init() {
	// DittoFS is registered once per durability tier {local, writeback, remote}
	// (#1758). The metadata store (badger) and the block store (fs-local cache +
	// S3 remote) are identical across all three, so a run across them isolates
	// the tier axis, which governs the write-ack durability barrier.
	//
	// The local tier keeps the bare name dittofs-s3 for result-file/history
	// continuity; writeback/remote append a tier suffix. Neither suffix ends in a
	// protocol name, so splitSystemLabel never mis-peels them.
	tiers := []struct {
		suffix     string // appended to dittofs-s3 ("" for the default local tier)
		durability string // local block store "durability": local|writeback|remote
		behavior   string // tier phrase for the Tier string
	}{
		{"", "local", "durable-to-local (%s fsync + fs block cache) + async S3 writeback"},
		{"-writeback", "writeback", "local-ack (%s, metadata flush relaxed) + async S3 writeback"},
		{"-remote", "remote", "ack-on-S3 (%s, strict CLOSE/COMMIT sync to S3)"},
	}
	for _, t := range tiers {
		durability := t.durability
		register(&Backend{
			Name:     "dittofs-s3" + t.suffix,
			S3Backed: true,
			Tier:     fmt.Sprintf(t.behavior, "badger"),
			Support:  map[Protocol]Support{ProtoNFS3: Native, ProtoNFS4: Native, ProtoSMB3: Native},
			Setup: func(ctx context.Context, env BackendEnv) error {
				return dittofsSetup(ctx, env, durability, dittofsUnboundedMaxSize)
			},
			Mount:    dittofsMount,
			Unmount:  func(ctx context.Context, _ Protocol) error { return exec.Sh(ctx, "umount", clientMntDir) },
			Teardown: dittofsTeardown,
		})
	}

	// Cache-cap variants for the cache-fill study: on the writeback tier the
	// local-ack journal is what fills, so hard-cap max_size below the write
	// working set and observe whether the journal applies backpressure or errors.
	// writeback tier only (the write path under test); the 32 GiB
	// rows above are the unbounded scenario. Names don't end in a protocol, so
	// splitSystemLabel won't mis-peel them.
	for _, c := range []struct {
		suffix  string
		maxSize int64
		cap     string // human cap for the Tier string
	}{
		{"-cap256m", 256 * 1024 * 1024, "256 MiB"},
		{"-cap2g", 2 * 1024 * 1024 * 1024, "2 GiB"},
	} {
		maxSize := c.maxSize
		register(&Backend{
			Name:     "dittofs-s3-writeback" + c.suffix,
			S3Backed: true,
			Tier:     "local-ack (badger, metadata flush relaxed) + async S3 writeback; local cache capped at " + c.cap,
			Support:  map[Protocol]Support{ProtoNFS3: Native, ProtoNFS4: Native, ProtoSMB3: Native},
			Setup: func(ctx context.Context, env BackendEnv) error {
				return dittofsSetup(ctx, env, "writeback", maxSize)
			},
			Mount:    dittofsMount,
			Unmount:  func(ctx context.Context, _ Protocol) error { return exec.Sh(ctx, "umount", clientMntDir) },
			Teardown: dittofsTeardown,
		})
	}
}

func dittofsSetup(ctx context.Context, env BackendEnv, durability string, maxSize int64) error {
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
	// by exact name, free the NFS port, and if dfs still refuses to die, ABORT the
	// cell rather than wipe state under it — a clean FAIL beats corrupting the store
	// and mis-attributing the result to the binary under test.
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
	// DITTOFS_LOGGING_LEVEL=WARN pins per-op logging off for the benched server
	// regardless of the binary's default, so log volume is uniform across A/B runs
	// (belt-and-suspenders alongside #1738 demoting per-op logs to Debug).
	start := fmt.Sprintf("DITTOFS_LOGGING_LEVEL=WARN DITTOFS_CONTROLPLANE_SECRET=%s DITTOFS_ADMIN_INITIAL_PASSWORD=%s "+
		"dfs start >"+dittofsServerLog+" 2>&1 &", dittofsSecret, dittofsAdminPass)
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
	// NFS is served by default, but SMB is not — the smb3 cells mount //127.0.0.1
	// on dittofsSMBPort, so the SMB adapter must be enabled explicitly or every
	// smb3 mount fails ECONNREFUSED. Idempotent (enable creates the record if
	// absent); takes effect without a restart.
	if err := exec.Sh(ctx, "dfsctl", "adapter", "enable", "smb", "--port", dittofsSMBPort); err != nil {
		return fmt.Errorf("dfsctl adapter enable smb: %w", err)
	}
	if err := waitPort(ctx, dittofsSMBPort); err != nil {
		return fmt.Errorf("dfs did not open SMB port %s: %w", dittofsSMBPort, err)
	}
	if err := exec.Sh(ctx, "dfsctl", "store", "metadata", "add",
		"--name", dittofsMeta, "--db-path", dittofsDataDir+"/meta"); err != nil {
		return err
	}
	// The durability tier rides the share's own two axes: what a COMMIT waits
	// for, and whether the metadata fsync may be deferred. They are independent,
	// so the tier names below expand to a pair rather than to one enum.
	//
	// max_size caps the local journal. The unbounded default (dittofsUnboundedMaxSize,
	// 32 GiB) gives the writeback tier generous headroom: writeback relaxes the
	// metadata fsync that otherwise paces writes, so a sustained fio write burst
	// outruns the async S3 syncer; with a small capacity the journal saturates
	// ("all segments pinned by unsynced bytes") and writes fail EIO. A realistic
	// writeback cache is sized for the working set (JuiceFS --writeback does the
	// same), so the default measures the tier at throughput instead of its
	// saturation cliff. The cache-cap study variants pass a small maxSize on
	// purpose, to measure exactly that cliff (backpressure vs error) when it fills.
	commitAck, relaxedMetadata, err := dittofsDurabilityFlags(durability)
	if err != nil {
		return err
	}
	if err := exec.Sh(ctx, "dfsctl", "store", "block", "add",
		"--name", dittofsBlocks, "--type", "s3", "--bucket", env.Bucket, "--endpoint", env.Endpoint,
		"--access-key", id, "--secret-key", secret, "--region", "us-east-1"); err != nil {
		return err
	}
	// --default-permission read-write so the AUTH_SYS root client (squashed to
	// nobody) can still write — the benchmark's whole job.
	args := []string{"share", "create", "--name", "/" + dittofsShare,
		"--metadata", dittofsMeta, "--block-store", dittofsBlocks,
		"--journal-size", strconv.FormatInt(maxSize, 10),
		"--commit-ack", commitAck,
		"--default-permission", "read-write"}
	if relaxedMetadata {
		args = append(args, "--relaxed-metadata-commit")
	}
	return exec.Sh(ctx, "dfsctl", args...)
}

// dittofsDurabilityFlags expands a tier name into the two independent axes it
// stands for. An unknown name is an error rather than a default: every cell of
// the matrix is meant to measure a different write-ack barrier, and silently
// collapsing one onto another would report a tier the run never exercised.
func dittofsDurabilityFlags(tier string) (commitAck string, relaxedMetadata bool, err error) {
	switch tier {
	case "local":
		return "journal", false, nil
	case "writeback":
		return "journal", true, nil
	case "remote":
		return "block-store", false, nil
	}
	return "", false, fmt.Errorf("unknown durability tier %q", tier)
}

func dittofsMount(ctx context.Context, proto Protocol) (string, error) {
	if err := prepareMountpoint(ctx); err != nil {
		return "", err
	}
	var typ, opts, src string
	switch proto {
	case ProtoNFS3:
		typ, src = "nfs", "127.0.0.1:/"+dittofsShare
		// Mount as a real user would, and at the same caching tier as the FUSE
		// competitors we compare against. actimeo=1 gives a 1s attribute cache
		// (matching JuiceFS's default --attr-cache=1s); the old actimeo=0 disabled
		// it entirely, forcing a GETATTR revalidation RPC + metadata-store lookup on
		// essentially every op — a tax the FUSE re-exports never pay, which inflated
		// our metadata numbers. nconnect=4 parallelises RPCs over 4 TCP connections,
		// the wire analog of FUSE's inherent request parallelism. nolock stays: the
		// harness wires no NLM statd and locking doesn't affect create throughput.
		// Keep IDENTICAL to the zerofs nfs3 cell so the native-vs-native comparison
		// stays clean (see zerofs.go).
		opts = "nfsvers=3,tcp,port=" + dittofsNFSPort + ",mountport=" + dittofsNFSPort + ",actimeo=1,nconnect=4,nolock"
	case ProtoNFS4:
		typ, src = "nfs", "127.0.0.1:/"+dittofsShare
		// Same attr-cache + parallelism as the v3 cell and the re-export v4.1 mount
		// (no nolock — v4 has integrated locking), so native and re-export v4.1 are
		// identical and the comparison stays clean.
		opts = "vers=4.1,tcp,port=" + dittofsNFSPort + ",actimeo=1,nconnect=4"
	case ProtoSMB3:
		typ, src = "cifs", "//127.0.0.1/"+dittofsShare
		opts = "port=" + dittofsSMBPort + ",guest,vers=3.0"
	default:
		return "", fmt.Errorf("dittofs-s3: unsupported protocol %s", proto)
	}
	// Adding the metadata/block stores and creating the share hot-reloads the
	// running adapter, which briefly closes and reopens the SMB listener; a mount
	// that lands in that window fails "Host is down" (the port is momentarily
	// gone). waitPort above only proves the listener was up right after `adapter
	// enable`, not after the later share-create reload. Retry so the mount rides
	// out the reload instead of failing the whole cell over a ~few-second gap.
	const mountAttempts = 8
	var err error
	for attempt := 0; attempt < mountAttempts; attempt++ {
		if err = exec.Sh(ctx, "mount", "-t", typ, "-o", opts, src, clientMntDir); err == nil {
			return clientMntDir, nil
		}
		if attempt == mountAttempts-1 {
			break // don't sleep after the last try — surface the failure now
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return "", err
}

func dittofsTeardown(ctx context.Context) error {
	// SIGKILL + wait for the process to actually exit so a subsequent same-VM run
	// (e.g. an A/B binary swap) starts clean instead of tripping dfs's "already
	// running" guard. Match by exact name (-x dfs); a `-f 'dfs start'` pattern would
	// self-match the shell running it (its argv contains the literal). See
	// dittofsSetup's cleanup.
	//
	// Only wipe the data dir once dfs is confirmed dead: RemoveAll under a live dfs
	// deletes Badger SSTs out from under it and corrupts the store — the exact
	// failure Setup hardens against. If dfs survives SIGKILL, leave the dir intact
	// and let the next Setup's pre-start guard catch it and FAIL loudly.
	if err := exec.Sh(ctx, "sh", "-c",
		"pkill -9 -x dfs 2>/dev/null; "+
			"for i in $(seq 1 20); do pgrep -x dfs >/dev/null 2>&1 || break; sleep 0.5; done; "+
			"pgrep -x dfs >/dev/null 2>&1 && { echo 'dfs survived SIGKILL — leaving data dir for next Setup to catch' >&2; exit 1; }; true"); err != nil {
		return nil // best-effort teardown; don't wipe state under a wedged process
	}
	return os.RemoveAll(dittofsDataDir)
}

// waitPort blocks until 127.0.0.1:port accepts a connection or ~60s elapse.
func waitPort(ctx context.Context, port string) error {
	return exec.Sh(ctx, "sh", "-c",
		fmt.Sprintf("for i in $(seq 1 60); do nc -z 127.0.0.1 %s && exit 0; sleep 1; done; exit 1", port))
}

package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/dfsbench/exec"
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
// Mount strings and `store block evict` are the documented interface (see
// docs/guide/nfs.md, dfsctl). The server bringup (config schema + dfsctl
// bootstrap) is pinned against a live dfs on the VM — the first managed run is
// where it gets tuned.
const (
	dittofsNFSPort = "12049"
	dittofsSMBPort = "12445"
	dittofsShare   = "bench"
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

// dittofsDataDir holds the subject's metadata (/meta, /meta.db) and local block
// cache (/blocks). It lives on the isolated data volume when one is mounted,
// else the legacy root-disk path.
var dittofsDataDir = benchPath("/var/lib/bench-dittofs", "dittofs")

// dittofsServerLog is where dittofsSetup redirects the benched server's stdout
// and stderr. The cold barrier reads back the window it wrote while running, so
// a barrier failure carries the server's own account of it.
const dittofsServerLog = "/var/log/bench-dittofs.log"

// dittofsMetaKind selects the metadata-store engine. The block store (fs local
// cache + S3 remote) is identical across all three, so a badger/sqlite/postgres
// A/B isolates the metadata engine — the axis that dominates create/rename/
// dir-heavy workloads and that only badger had ever been measured on.
type dittofsMetaKind int

const (
	metaBadger dittofsMetaKind = iota
	metaSQLite
	metaPostgres
)

// Postgres bench DB provisioned on the (Debian) bench VM for the postgres
// metadata variant. Throwaway single-tenant VM — fixed literals, same
// convention as dittofsSecret.
const (
	dittofsPGDB   = "dittofs_bench"
	dittofsPGUser = "dittofs"
	dittofsPGPass = "dittofs"
)

// dittofsDrainTimeout bounds each `dfsctl system drain-uploads` in the cold-evict
// loop above the client's 6m default, so a large cold-evict working set drains
// instead of aborting the cell on a bare deadline (issue #1668).
const dittofsDrainTimeout = "15m"

// dittofsDrainProgressInterval is how often the drain loop reports the store's
// unsynced size while a drain-uploads call is in flight. The call prints nothing
// until it returns and may legitimately run for minutes, so on a timeout the log
// otherwise says only that the drain did not finish — not whether it was moving.
// A falling unsynced size means slow; a flat one means wedged, and those two
// need opposite fixes.
const dittofsDrainProgressInterval = 30 * time.Second

// dittofsDrainProgressSampleTimeout bounds one progress sample. Comfortably
// under the sampling interval so a slow sample cannot queue behind the next one,
// and short enough that an unresponsive server costs one skipped line rather
// than the rest of the log.
const dittofsDrainProgressSampleTimeout = 10 * time.Second

// dittofsUnboundedMaxSize is the default local-journal cap: generous headroom so
// a sustained write burst measures the tier's throughput, not its saturation
// cliff (see dittofsSetup). The cache-cap study variants pass a small cap instead.
const dittofsUnboundedMaxSize = int64(32) * 1024 * 1024 * 1024

// dittofsAddMetadataStore registers the subject's metadata store with the
// engine selected by kind. badger/sqlite need no server; postgres is
// provisioned on the (throwaway, single-tenant) bench VM first. All three pair
// with the same fs-local + S3 block store added by the caller, so a run across
// the three variants is a clean metadata-engine A/B.
func dittofsAddMetadataStore(ctx context.Context, kind dittofsMetaKind) error {
	switch kind {
	case metaSQLite:
		return exec.Sh(ctx, "dfsctl", "store", "metadata", "add",
			"--name", dittofsMeta, "--type", "sqlite",
			"--config", fmt.Sprintf(`{"path":%q}`, dittofsDataDir+"/meta.db"))
	case metaPostgres:
		if err := dittofsProvisionPostgres(ctx); err != nil {
			return err
		}
		return exec.Sh(ctx, "dfsctl", "store", "metadata", "add",
			"--name", dittofsMeta, "--type", "postgres",
			"--config", fmt.Sprintf(
				`{"host":"127.0.0.1","port":5432,"user":%q,"password":%q,"database":%q,"sslmode":"disable"}`,
				dittofsPGUser, dittofsPGPass, dittofsPGDB))
	default: // metaBadger
		return exec.Sh(ctx, "dfsctl", "store", "metadata", "add",
			"--name", dittofsMeta, "--type", "badger", "--db-path", dittofsDataDir+"/meta")
	}
}

// dittofsProvisionPostgres installs, starts PostgreSQL, and (re)creates a clean
// bench role+database. Idempotent; mirrors the apt install-on-demand idiom the
// re-export backends use. Debian/Ubuntu bench image assumed.
//
// The provisioning SQL runs as the `postgres` OS user over the local socket,
// which the default pg_hba `local all postgres peer` rule always admits — no
// pg_hba edit needed. dfsctl then connects over TCP (127.0.0.1) as the freshly
// created role, which the default `host all all 127.0.0.1/32 scram-sha-256` rule
// admits with the password set below.
//
// Every postgres-user command uses `runuser -u postgres --`, never `su - postgres
// -c`: the harness launches the whole matrix detached (nohup, no controlling tty
// / login session), where `su -`'s PAM login shell misbehaves (psql exit 2).
// runuser sets up no PAM/login/tty session, so it works identically detached or
// interactive (issue #1671).
func dittofsProvisionPostgres(ctx context.Context) error {
	return provisionPostgres(ctx, dittofsPGDB, dittofsPGUser, dittofsPGPass)
}

// provisionPostgres installs+starts PostgreSQL (if needed) and (re)creates a
// clean role+database — DROPping any prior db so each run starts empty. Shared
// by the dittofs-postgres metadata variant and the juicefs-postgres meta store.
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
	// DittoFS is registered as the full cross-product of its two performance axes:
	// metadata engine {badger, sqlite, postgres} × durability tier {local,
	// writeback, remote} (#1758). The block store (fs-local cache + S3 remote) is
	// identical across all nine, so a run across them isolates each axis cleanly —
	// the metadata-engine axis dominates create/rename, the tier axis governs the
	// write-ack durability barrier.
	//
	// Default names are unchanged for result-file/history continuity: the local
	// tier keeps the bare engine name (dittofs-s3, dittofs-sqlite-s3,
	// dittofs-postgres-s3); writeback/remote append a tier suffix. Neither suffix
	// ends in a protocol name, so splitSystemLabel never mis-peels them.
	engines := []struct {
		name string // base name = local-tier name
		kind dittofsMetaKind
		desc string // metadata-engine phrase for the Tier string
	}{
		{"dittofs-s3", metaBadger, "badger"},
		{"dittofs-sqlite-s3", metaSQLite, "sqlite"},
		{"dittofs-postgres-s3", metaPostgres, "postgres"},
	}
	tiers := []struct {
		suffix     string // appended to the engine base name ("" for the default local tier)
		durability string // local block store "durability": local|writeback|remote
		behavior   string // tier phrase for the Tier string
	}{
		{"", "local", "durable-to-local (%s fsync + fs block cache) + async S3 writeback"},
		{"-writeback", "writeback", "local-ack (%s, metadata flush relaxed) + async S3 writeback"},
		{"-remote", "remote", "ack-on-S3 (%s, strict CLOSE/COMMIT sync to S3)"},
	}
	for _, e := range engines {
		for _, t := range tiers {
			kind, durability := e.kind, t.durability
			register(&Backend{
				Name:     e.name + t.suffix,
				S3Backed: true,
				Tier:     fmt.Sprintf(t.behavior, e.desc),
				Support:  map[Protocol]Support{ProtoNFS3: Native, ProtoNFS4: Native, ProtoSMB3: Native},
				Setup: func(ctx context.Context, env BackendEnv) error {
					return dittofsSetup(ctx, env, kind, durability, dittofsUnboundedMaxSize)
				},
				Mount:       dittofsMount,
				Evict:       dittofsEvict,
				WaitSettled: dittofsWaitSettled,
				Unmount:     func(ctx context.Context, _ Protocol) error { return exec.Sh(ctx, "umount", clientMntDir) },
				Teardown:    dittofsTeardown,
			})
		}
	}

	// Cache-cap variants for the cache-fill study: on the writeback tier the
	// local-ack journal is what fills, so hard-cap max_size below the write
	// working set and observe whether the journal applies backpressure or errors.
	// badger engine + writeback tier only (the write path under test); the 32 GiB
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
				return dittofsSetup(ctx, env, metaBadger, "writeback", maxSize)
			},
			Mount:       dittofsMount,
			Evict:       dittofsEvict,
			WaitSettled: dittofsWaitSettled,
			Unmount:     func(ctx context.Context, _ Protocol) error { return exec.Sh(ctx, "umount", clientMntDir) },
			Teardown:    dittofsTeardown,
		})
	}
}

func dittofsSetup(ctx context.Context, env BackendEnv, kind dittofsMetaKind, durability string, maxSize int64) error {
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
	if err := dittofsAddMetadataStore(ctx, kind); err != nil {
		return err
	}
	// Pass the durability tier (#1758) through the local block store config. The
	// "durability" enum (local|writeback|remote) is read by resolveDurabilityTier
	// at share create; "local" is the default so it's harmless to set explicitly.
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
	localCfg, err := json.Marshal(map[string]any{
		"path":       dittofsDataDir + "/blocks",
		"durability": durability,
		"max_size":   maxSize,
	})
	if err != nil {
		return err
	}
	if err := exec.Sh(ctx, "dfsctl", "store", "block", "local", "add",
		"--name", dittofsLocal, "--type", "fs", "--config", string(localCfg)); err != nil {
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

// dittofsColdBarrierFloorBytes is the resident-bytes threshold below which the
// cold-barrier verification stops caring about an exact drop ratio — small
// datasets and metadata slack shouldn't fail the check.
const dittofsColdBarrierFloorBytes = 64 << 20 // 64 MiB

// dittofsColdBarrierBudgetBytes is the most local disk that may still be resident
// after the evict for the pass that follows to count as cold: the ≥80% drop the
// barrier verifies. The drain ahead of it has to leave a residue this same budget
// can absorb, so both derive the number here rather than each spelling it out.
func dittofsColdBarrierBudgetBytes(residentBytes int64) int64 { return residentBytes / 5 }

// dittofsBlockTotals is the subset of `dfsctl store block stats -o json` the
// cold barrier needs: how much is resident locally, and how much is not yet
// durable on the remote (so DrainLocalSynced can't drop it).
type dittofsBlockTotals struct {
	LocalDiskUsed  int64 `json:"local_disk_used"`
	UnsyncedBytes  int64 `json:"unsynced_bytes"`
	PendingUploads int   `json:"pending_uploads"`
}

func dittofsBlockStats(ctx context.Context) (dittofsBlockTotals, error) {
	_, totals, err := dittofsBlockStatsRaw(ctx)
	return totals, err
}

// dittofsBlockStatsRaw returns the parsed totals alongside the untouched JSON.
// dittofsBlockTotals keeps three fields; the response also carries
// eviction_suspended, remote_healthy, failed_syncs and the per-state block
// counts — the fields that say WHY an evict declined to drop anything. The cold
// barrier keeps the raw bytes so a failure can print those too.
func dittofsBlockStatsRaw(ctx context.Context) ([]byte, dittofsBlockTotals, error) {
	out, err := exec.Out(ctx, "dfsctl", "store", "block", "stats", "-o", "json")
	if err != nil {
		return nil, dittofsBlockTotals{}, fmt.Errorf("dfsctl store block stats: %w", err)
	}
	var resp struct {
		Totals dittofsBlockTotals `json:"totals"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return out, dittofsBlockTotals{}, fmt.Errorf("parse block stats json: %w\n%s", err, out)
	}
	return out, resp.Totals, nil
}

// dittofsSettleTimeout bounds how long the warm-read settle waits for the block
// store's async digestion to go idle before giving up and measuring anyway.
const dittofsSettleTimeout = 120 * time.Second

// dittofsSettleStableWindow is how long UnsyncedBytes must hold flat before the
// store counts as idle. The block store carves+uploads on a periodic ticker
// (UploadInterval, default 2s), so UnsyncedBytes only steps down once per tick
// and is naturally flat between ticks. The window must exceed one tick, or a
// sample landing in an inter-tick lull would declare idle while a full carve
// pass is still queued — restarting mid-measurement and re-contaminating the
// read. Poll spacing is a fraction of the window so several samples cover it.
const (
	dittofsSettleStableWindow = 4 * time.Second
	dittofsSettlePollInterval = 500 * time.Millisecond
)

// dittofsWaitSettled blocks until the block store's async carve/upload/rollup of
// freshly-written data goes idle, so the warm read pass measures a settled store
// (served from the local journal) instead of one still writing CAS chunks to disk
// and S3 — that concurrent digestion charges the read path for unrelated write
// I/O and CPU, tanking warm rand-read latency. "Idle" is UnsyncedBytes holding
// flat for longer than one carve/upload tick (see dittofsSettleStableWindow), not
// unsynced==0: a sub-GiB file's final bytes stay pinned in the unsealed append
// log indefinitely without another write. Polls passively — it never triggers a
// drain (which can stall) — and on timeout logs and returns nil so a contaminated
// measurement is surfaced rather than failing the cell.
func dittofsWaitSettled(ctx context.Context) error {
	start := time.Now()
	deadline := start.Add(dittofsSettleTimeout)
	prev := int64(-1)
	var lastChange time.Time
	for {
		st, err := dittofsBlockStats(ctx)
		if err != nil {
			return err
		}
		now := time.Now()
		if st.UnsyncedBytes != prev {
			prev = st.UnsyncedBytes
			lastChange = now
		} else if now.Sub(lastChange) >= dittofsSettleStableWindow {
			_, _ = fmt.Fprintf(exec.CmdOut, "settled: waited %dms for block-store sync to go idle (unsynced=%dMiB, flat %s)\n",
				time.Since(start).Milliseconds(), st.UnsyncedBytes>>20, dittofsSettleStableWindow)
			return nil
		}
		if now.After(deadline) {
			_, _ = fmt.Fprintf(exec.CmdOut, "settle: block store still busy after %s (unsynced=%dMiB) — warm read may be contaminated\n",
				dittofsSettleTimeout, st.UnsyncedBytes>>20)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(dittofsSettlePollInterval):
		}
	}
}

// dittofsEvictSegmentBytes is the granularity at which eviction actually frees
// local disk. The journal reclaims whole segments and refuses any segment still
// holding an unsynced record, so one straggler keeps its entire segment resident.
// pkg/block/local/fs leaves Config.SegmentSize unset, so the bench share runs on
// the journal's own default.
const dittofsEvictSegmentBytes int64 = 256 << 20

// dittofsDrainStragglerBytes is the smallest residue assumed able to pin a
// segment of its own. Unsynced bytes are spread over an unknown number of
// records, and only that count decides how many segments they pin, so the drain
// has to assume the finest spread it can plausibly see — the carver's minimum
// chunk (chunker.MinChunkSize).
//
// ponytail: an estimate standing in for a count the store already tracks; report
// unsynced records (or pinned segments) from `dfsctl store block stats` and the
// estimate collapses into reading that field.
const dittofsDrainStragglerBytes int64 = 1 << 20

// dittofsDrainResidueOK reports whether a drain residue is small enough to leave
// behind. Its size alone does not answer that: eviction frees segments, not
// bytes, so what a residue costs is the segments it pins, and a residue is
// harmless only when even the worst spread — every straggler alone in its own
// segment — still fits the budget the cold barrier allows to survive. Below the
// barrier's own floor that drop is never verified, so no residue there can
// invalidate a measurement that was never going to be checked.
func dittofsDrainResidueOK(unsyncedBytes, localResidentBytes int64) bool {
	if unsyncedBytes <= 0 || localResidentBytes <= dittofsColdBarrierFloorBytes {
		return true
	}
	return dittofsWorstCasePinnedBytes(unsyncedBytes) <= dittofsColdBarrierBudgetBytes(localResidentBytes)
}

// dittofsWorstCasePinnedBytes is the local disk an unsynced residue can hold down
// when every straggler in it sits alone in its own segment.
func dittofsWorstCasePinnedBytes(unsyncedBytes int64) int64 {
	if unsyncedBytes <= 0 {
		return 0
	}
	segments := (unsyncedBytes + dittofsDrainStragglerBytes - 1) / dittofsDrainStragglerBytes
	return segments * dittofsEvictSegmentBytes
}

// dittofsDrainResidueErr reports a residue the drain could not get below,
// spelling out the segment arithmetic that makes it fatal.
func dittofsDrainResidueErr(st dittofsBlockTotals, rounds int) error {
	pinned := dittofsWorstCasePinnedBytes(st.UnsyncedBytes)
	return fmt.Errorf("drain-uploads left %dMiB unsynced after %d rounds (local=%dMiB pending_uploads=%d) — eviction frees whole %dMiB segments and skips any holding an unsynced record, so that residue can pin up to %dMiB locally, against the %dMiB the cold barrier lets survive",
		st.UnsyncedBytes>>20, rounds, st.LocalDiskUsed>>20, st.PendingUploads,
		dittofsEvictSegmentBytes>>20, pinned>>20, dittofsColdBarrierBudgetBytes(st.LocalDiskUsed)>>20)
}

// dittofsDrainUntilSynced loops `dfsctl system drain-uploads` until the residue
// left unsynced can no longer pin enough segments to warm the cold pass, stable
// across two polls. A single drain is not enough: rollup is async and carveFlush
// is snapshot-at-claim, so one pass misses chunks that roll up mid-drain —
// leaving them locally resident and un-evictable, so the "cold" pass silently
// reads them from local disk (the confound that made several cold-read A/Bs
// meaningless). The short settle between rounds lets the async rollup produce the
// next batch of CAS chunks for the following drain to upload.
func dittofsDrainUntilSynced(ctx context.Context) error {
	const maxRounds = 60
	// Rounds an intolerable residue may sit unchanged before the loop gives up.
	// Each round costs a full drain-uploads timeout, so riding out every round on
	// a residue that is not moving turns a failed barrier into a many-hour one.
	const maxFlatRounds = 3
	stable, flat := 0, 0
	prevUnsynced := int64(-1)
	for i := 0; i < maxRounds; i++ {
		// Bound each drain explicitly (issue #1668): a cold-evict drain of a large
		// working set can exceed the client's 6m default, and the failure surfaces
		// as a bare "context deadline exceeded" that aborts the cell.
		stop := dittofsReportDrainProgress(ctx, i)
		err := exec.Sh(ctx, "dfsctl", "system", "drain-uploads", "--timeout", dittofsDrainTimeout)
		stop()
		if err != nil {
			return fmt.Errorf("dfsctl system drain-uploads: %w", err)
		}
		st, err := dittofsBlockStats(ctx)
		if err != nil {
			return err
		}
		switch {
		case dittofsDrainResidueOK(st.UnsyncedBytes, st.LocalDiskUsed):
			flat = 0
			if stable++; stable >= 2 {
				return nil
			}
		case prevUnsynced >= 0 && st.UnsyncedBytes >= prevUnsynced:
			stable = 0
			if flat++; flat >= maxFlatRounds {
				return dittofsDrainResidueErr(st, i+1)
			}
		default:
			stable, flat = 0, 0
		}
		prevUnsynced = st.UnsyncedBytes
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	st, _ := dittofsBlockStats(ctx)
	return dittofsDrainResidueErr(st, maxRounds)
}

// dittofsReportDrainProgress samples the store's unsynced size every
// dittofsDrainProgressInterval and prints it, until the returned function is
// called. round labels the samples so consecutive drains stay distinguishable
// in the log. A sampling error is skipped rather than reported: the sampler is
// commentary on the drain, and must never be the thing that fails the barrier.
func dittofsReportDrainProgress(ctx context.Context, round int) func() {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(dittofsDrainProgressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Bound each sample. The drain this reports on is exactly the
				// situation where the server may be unresponsive, so an unbounded
				// stats call could hang here for the rest of the run and take the
				// stop function down with it — the progress log would go quiet at
				// the moment it is most worth reading, and the barrier would then
				// block waiting for a sampler that never returns.
				sctx, cancel := context.WithTimeout(ctx, dittofsDrainProgressSampleTimeout)
				st, err := dittofsBlockStats(sctx)
				cancel()
				if err != nil {
					continue
				}
				_, _ = fmt.Fprintf(exec.CmdOut, "drain round %d: unsynced=%dMiB pending_uploads=%d\n",
					round, st.UnsyncedBytes>>20, st.PendingUploads)
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// dittofsBarrierLogTailBytes caps how much server log a failed cold barrier
// reproduces. The barrier can run for minutes across many drain rounds, so it
// keeps the most recent output — the window around the failure — rather than the
// first bytes after the barrier started.
const dittofsBarrierLogTailBytes = 256 << 10

// dittofsBarrierDiag collects what the bare "local disk only fell X→Y" error
// throws away: the full block-store stats JSON at each step, what the evict
// reported freeing, and the server log written while the barrier ran. A step
// left empty is one the barrier never reached, which is itself part of the
// answer — a step that was reached but whose capture failed carries the note
// dittofsCapture built for it, so the two never read alike.
type dittofsBarrierDiag struct {
	logOffset int64  // server-log size when the barrier started
	entry     string // stats before the drain — the "before" the ratio is measured against
	postDrain string // stats after the drain loop, i.e. immediately before the evict
	evictOut  string // `store block evict -o json`: files evicted, bytes freed
	postEvict string // stats after the evict — the "after" of the ratio
}

// dittofsCapture renders one captured command output for the dump: the output
// itself, or a note naming the error when the capture failed. An empty string is
// reserved for a step the barrier never reached.
func dittofsCapture(out []byte, err error) string {
	if err != nil {
		return fmt.Sprintf("(capture failed: %v)", err)
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		return s
	}
	return "(command produced no output)"
}

// dump prints the collected evidence to the run log, which is pulled back off
// the bench VM with the results. cause is the failure being explained.
func (d *dittofsBarrierDiag) dump(cause error) {
	_, _ = fmt.Fprintf(exec.CmdOut, "cold barrier diagnostics — %v\n", cause)
	for _, step := range []struct {
		label string
		text  string
	}{
		{"block stats at barrier entry", d.entry},
		{"block stats after drain (pre-evict)", d.postDrain},
		{"evict result", d.evictOut},
		{"block stats after evict", d.postEvict},
	} {
		text := step.text
		if text == "" {
			text = "not reached"
		}
		_, _ = fmt.Fprintf(exec.CmdOut, "  %s: %s\n", step.label, text)
	}
	_, _ = fmt.Fprintf(exec.CmdOut, "  surviving local tier: %s\n", dittofsResidentFiles(dittofsDataDir+"/blocks"))
	_, _ = fmt.Fprintf(exec.CmdOut, "  server log (%s) since barrier entry:\n%s\n",
		dittofsServerLog, dittofsLogSince(dittofsServerLog, d.logOffset))
}

// dittofsResidentSampleFiles is how many of the largest surviving files the
// failure dump names.
const dittofsResidentSampleFiles = 20

// dittofsResidentFiles summarises what is still on the local tier under dir: the
// file count, their total size, and the largest few by name and size. The block
// stats JSON reports only an aggregate byte count, which cannot distinguish a
// remainder made of whole journal segments (eviction's unit is a segment, so one
// unsynced record keeps its entire segment resident) from one spread thinly
// across many files. Best-effort: it explains a barrier that already failed and
// must never add a second one, so every error becomes a parenthesised note.
func dittofsResidentFiles(dir string) string {
	type entry struct {
		name string
		size int64
	}
	if _, err := os.Stat(dir); err != nil {
		// Distinguish "the tier emptied" from "the directory is not there" — the
		// dump is read by someone deciding whether the evict worked.
		return fmt.Sprintf("(unreadable: %v)", err)
	}
	var files []entry
	var total int64
	unreadable := 0
	// The callback swallows every error so one bad subtree costs a line of the
	// sample rather than the whole dump, but it counts them: a partial scan that
	// printed as a complete one would misdescribe the tier the barrier left behind.
	_ = filepath.WalkDir(dir, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			unreadable++
			return nil //nolint:nilerr // counted above and reported below, not silently dropped
		}
		if e.IsDir() {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			unreadable++
			return nil //nolint:nilerr // ditto
		}
		files = append(files, entry{strings.TrimPrefix(path, dir+string(os.PathSeparator)), info.Size()})
		total += info.Size()
		return nil
	})
	if len(files) == 0 {
		if unreadable > 0 {
			return fmt.Sprintf("(unreadable: %d entries under %s could not be scanned)", unreadable, dir)
		}
		return fmt.Sprintf("%s is empty", dir)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].size > files[j].size })
	var b strings.Builder
	fmt.Fprintf(&b, "%d files, %dMiB total; largest:", len(files), total>>20)
	for _, f := range files[:min(len(files), dittofsResidentSampleFiles)] {
		fmt.Fprintf(&b, " %s=%dMiB", f.name, f.size>>20)
	}
	if len(files) > dittofsResidentSampleFiles {
		fmt.Fprintf(&b, " (+%d more)", len(files)-dittofsResidentSampleFiles)
	}
	if unreadable > 0 {
		fmt.Fprintf(&b, "; %d entries unreadable, so the totals are a floor", unreadable)
	}
	return b.String()
}

// dittofsLogSize reports the current size of path, or 0 when it cannot be
// stat'd. A wrong offset only widens the window the failure dump prints, so this
// never fails the barrier.
func dittofsLogSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// dittofsLogSince returns the bytes of path written after offset off, keeping at
// most dittofsBarrierLogTailBytes of the most recent output. It returns a
// parenthesised note rather than an error for every failure: this runs only to
// explain a barrier that already failed, and must never become a second failure.
func dittofsLogSince(path string, off int64) string {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("(unreadable: %v)", err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return fmt.Sprintf("(unreadable: %v)", err)
	}
	if st.Size() <= off {
		return "(the server logged nothing while the barrier ran)"
	}
	start := max(off, st.Size()-dittofsBarrierLogTailBytes)
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return fmt.Sprintf("(unreadable: %v)", err)
	}
	// Cap the read itself, not just the seek: the file can grow between the Stat
	// above and this read, and a busy server would otherwise flood the run log.
	b, err := io.ReadAll(io.LimitReader(f, dittofsBarrierLogTailBytes))
	if len(b) == 0 && err != nil {
		return fmt.Sprintf("(unreadable: %v)", err)
	}
	return string(b)
}

// dittofsEvict drops locally-cached blocks so the next read is cold-from-S3, and
// on any failure prints the evidence explaining it. The barrier's own error
// carries two byte counts, which say the local tier did not empty but not why.
func dittofsEvict(ctx context.Context) error {
	diag := dittofsBarrierDiag{logOffset: dittofsLogSize(dittofsServerLog)}
	err := dittofsColdBarrier(ctx, &diag)
	if err != nil {
		diag.dump(err)
	}
	return err
}

// dittofsColdBarrier drains, evicts and then verifies that the local tier
// actually emptied.
//
// The drain comes FIRST: `store block evict` drops only blocks already synced to
// S3. A block whose upload is still in flight is left on local disk, so the
// "cold" read serves it from cache — the pass reads warm, S3MB stays 0, and
// cold≈warm. Draining first makes every block synced and therefore evictable, so
// the next read genuinely comes from S3.
//
// diag accumulates the evidence for the failure dump as each step completes.
func dittofsColdBarrier(ctx context.Context, diag *dittofsBarrierDiag) error {
	// Cold-read barrier. Force the whole warm-written file durable on S3, evict the
	// local tier, then VERIFY it actually emptied — otherwise the cold pass reads
	// locally-resident bytes and silently measures a warm read (the confound the
	// DiskWr/NetRx meter exposed: cold pulled only ~15-33 MB/s from S3 while the
	// data sat on the block volume).
	raw, before, err := dittofsBlockStatsRaw(ctx)
	diag.entry = dittofsCapture(raw, err)
	if err != nil {
		return err
	}
	if err := dittofsDrainUntilSynced(ctx); err != nil {
		return err
	}
	// Sample between the drain and the evict: this separates "the drain left bytes
	// the evict was right to refuse" from "the evict declined bytes the drain had
	// already made durable" — which the before/after pair alone cannot tell apart.
	raw, _, err = dittofsBlockStatsRaw(ctx)
	diag.postDrain = dittofsCapture(raw, err)
	if err != nil {
		return err
	}
	// Evict local blocks + read buffer. DrainLocalSynced drops only synced blocks;
	// now that everything is synced it can drop the whole file. Take the JSON
	// result rather than the success line: bytes_freed says directly whether the
	// evict declined to drop anything or dropped bytes that came back.
	evictOut, err := exec.Out(ctx, "dfsctl", "store", "block", "evict", "-o", "json")
	diag.evictOut = dittofsCapture(evictOut, err)
	if err != nil {
		return fmt.Errorf("dfsctl store block evict: %w", err)
	}
	raw, after, err := dittofsBlockStatsRaw(ctx)
	diag.postEvict = dittofsCapture(raw, err)
	if err != nil {
		return err
	}
	// Verify: the local tier must have shed the bulk of its resident bytes. If a
	// large remainder survives (>20% of a non-trivial starting size), the cold pass
	// would read it from disk — FAIL LOUDLY rather than emit a warm number labelled
	// "cold". The DiskWr/NetRx columns independently confirm coldness post-hoc.
	if before.LocalDiskUsed <= dittofsColdBarrierFloorBytes {
		// Below the floor the drop ratio is noise, so the assertion below cannot
		// say whether the evict worked. Say that out loud: a silent return reads
		// in the log exactly like a verified barrier, and the cold cell it labels
		// may have been served warm.
		_, _ = fmt.Fprintf(exec.CmdOut,
			"warn: cold barrier UNVERIFIED — local disk %dMiB→%dMiB, under the %dMiB floor at which the drop is meaningful; treat this cold cell as unconfirmed\n",
			before.LocalDiskUsed>>20, after.LocalDiskUsed>>20, dittofsColdBarrierFloorBytes>>20)
		return nil
	}
	if after.LocalDiskUsed > dittofsColdBarrierBudgetBytes(before.LocalDiskUsed) {
		return fmt.Errorf("cold barrier failed: local disk only fell %dMiB→%dMiB (want ≥80%% drop); the cold pass would measure locally-served reads, not S3",
			before.LocalDiskUsed>>20, after.LocalDiskUsed>>20)
	}
	_, _ = fmt.Fprintf(exec.CmdOut, "cold barrier ok: local disk %dMiB→%dMiB\n",
		before.LocalDiskUsed>>20, after.LocalDiskUsed>>20)
	return nil
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

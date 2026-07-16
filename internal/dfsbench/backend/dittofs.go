package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

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
runuser -u postgres -- psql -v ON_ERROR_STOP=1 <<SQL
DROP DATABASE IF EXISTS %[1]s;
DO $do$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='%[2]s') THEN CREATE ROLE %[2]s LOGIN PASSWORD '%[3]s'; END IF; END $do$;
CREATE DATABASE %[1]s OWNER %[2]s;
SQL`, dittofsPGDB, dittofsPGUser, dittofsPGPass)
	return exec.Sh(ctx, "sh", "-c", script)
}

func init() {
	// badger keeps the existing "dittofs-s3" name (result files key off it);
	// sqlite/postgres get explicit variant names. Same S3 block store, so the
	// only axis that moves between them is the metadata engine.
	for _, v := range []struct {
		name string
		kind dittofsMetaKind
	}{
		{"dittofs-s3", metaBadger},
		{"dittofs-sqlite-s3", metaSQLite},
		{"dittofs-postgres-s3", metaPostgres},
	} {
		kind := v.kind
		register(&Backend{
			Name:     v.name,
			S3Backed: true,
			Support:  map[Protocol]Support{ProtoNFS3: Native, ProtoNFS4: Native, ProtoSMB3: Native},
			Setup:    func(ctx context.Context, env BackendEnv) error { return dittofsSetup(ctx, env, kind) },
			Mount:    dittofsMount,
			Evict:    dittofsEvict,
			Unmount:  func(ctx context.Context, _ Protocol) error { return exec.Sh(ctx, "umount", clientMntDir) },
			Teardown: dittofsTeardown,
		})
	}
}

func dittofsSetup(ctx context.Context, env BackendEnv, kind dittofsMetaKind) error {
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
	if err := dittofsAddMetadataStore(ctx, kind); err != nil {
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
// dittofsColdBarrierFloorBytes is the resident-bytes threshold below which the
// cold-barrier verification stops caring about an exact drop ratio — small
// datasets and metadata slack shouldn't fail the check.
const dittofsColdBarrierFloorBytes = 64 << 20 // 64 MiB

// dittofsBlockTotals is the subset of `dfsctl store block stats -o json` the
// cold barrier needs: how much is resident locally, and how much is not yet
// durable on the remote (so DrainLocalSynced can't drop it).
type dittofsBlockTotals struct {
	LocalDiskUsed  int64 `json:"local_disk_used"`
	UnsyncedBytes  int64 `json:"unsynced_bytes"`
	PendingUploads int   `json:"pending_uploads"`
}

func dittofsBlockStats(ctx context.Context) (dittofsBlockTotals, error) {
	out, err := exec.Out(ctx, "dfsctl", "store", "block", "stats", "-o", "json")
	if err != nil {
		return dittofsBlockTotals{}, fmt.Errorf("dfsctl store block stats: %w", err)
	}
	var resp struct {
		Totals dittofsBlockTotals `json:"totals"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return dittofsBlockTotals{}, fmt.Errorf("parse block stats json: %w\n%s", err, out)
	}
	return resp.Totals, nil
}

// dittofsDrainDriftFloorBytes is the residual unsynced size the drain loop
// tolerates as "done". A freshly-written file's final bytes sit in the append log
// until the next rollup boundary, so a few chunks stay un-carved/un-synced
// indefinitely without another write — requiring EXACTLY 0 loops forever. A few
// MiB out of a multi-GiB file is <0.1% and does not meaningfully warm the cold
// pass (the post-evict ≥80%-drop check is the real cold gate).
const dittofsDrainDriftFloorBytes = 32 << 20 // 32 MiB

// dittofsDrainUntilSynced loops `dfsctl system drain-uploads` until the store's
// unsynced bytes fall to the drift floor, stable across two polls. A single drain
// is not enough: rollup is async and carveFlush is snapshot-at-claim, so one pass
// misses chunks that roll up mid-drain — leaving them locally resident and
// un-evictable, so the "cold" pass silently reads them from local disk (the
// confound that made several cold-read A/Bs meaningless). The short settle between
// rounds lets the async rollup produce the next batch of CAS chunks for the
// following drain to upload.
func dittofsDrainUntilSynced(ctx context.Context) error {
	const maxRounds = 60
	stable := 0
	for i := 0; i < maxRounds; i++ {
		// Bound each drain explicitly (issue #1668): a cold-evict drain of a large
		// working set can exceed the client's 6m default, and the failure surfaces
		// as a bare "context deadline exceeded" that aborts the cell.
		if err := exec.Sh(ctx, "dfsctl", "system", "drain-uploads", "--timeout", dittofsDrainTimeout); err != nil {
			return fmt.Errorf("dfsctl system drain-uploads: %w", err)
		}
		st, err := dittofsBlockStats(ctx)
		if err != nil {
			return err
		}
		if st.UnsyncedBytes <= dittofsDrainDriftFloorBytes {
			if stable++; stable >= 2 {
				return nil
			}
		} else {
			stable = 0
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	st, _ := dittofsBlockStats(ctx)
	return fmt.Errorf("drain-uploads never fell below %dMiB unsynced after %d rounds (unsynced=%dMiB pending_uploads=%d) — cannot force a cold read",
		dittofsDrainDriftFloorBytes>>20, maxRounds, st.UnsyncedBytes>>20, st.PendingUploads)
}

func dittofsEvict(ctx context.Context) error {
	// Cold-read barrier. Force the whole warm-written file durable on S3, evict the
	// local tier, then VERIFY it actually emptied — otherwise the cold pass reads
	// locally-resident bytes and silently measures a warm read (the confound the
	// DiskWr/NetRx meter exposed: cold pulled only ~15-33 MB/s from S3 while the
	// data sat on the block volume).
	before, err := dittofsBlockStats(ctx)
	if err != nil {
		return err
	}
	if err := dittofsDrainUntilSynced(ctx); err != nil {
		return err
	}
	// Evict local blocks + read buffer. DrainLocalSynced drops only synced blocks;
	// now that everything is synced it can drop the whole file.
	if err := exec.Sh(ctx, "dfsctl", "store", "block", "evict"); err != nil {
		return fmt.Errorf("dfsctl store block evict: %w", err)
	}
	after, err := dittofsBlockStats(ctx)
	if err != nil {
		return err
	}
	// Verify: the local tier must have shed the bulk of its resident bytes. If a
	// large remainder survives (>20% of a non-trivial starting size), the cold pass
	// would read it from disk — FAIL LOUDLY rather than emit a warm number labelled
	// "cold". The DiskWr/NetRx columns independently confirm coldness post-hoc.
	if before.LocalDiskUsed > dittofsColdBarrierFloorBytes && after.LocalDiskUsed > before.LocalDiskUsed/5 {
		return fmt.Errorf("cold barrier failed: local disk only fell %dMiB→%dMiB (want ≥80%% drop); the cold pass would measure locally-served reads, not S3",
			before.LocalDiskUsed>>20, after.LocalDiskUsed>>20)
	}
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

package backend

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/marmos91/dittofs/internal/dfsbench/exec"
)

// zerofs is the one competitor that isn't a FUSE-over-knfsd hack: like DittoFS
// it's an integrated userspace server + S3 storage engine (a log-structured
// SlateDB/LSM store), serving NFS from its own process — so it mounts NATIVELY,
// the same head-to-head shape as dittofs-s3. Its NFS server is v3-only (it also
// speaks 9P/NBD, which aren't on our protocol axis), and it has no SMB, so nfs3
// is its only cell here — itself a differentiator worth showing in the table.
//
// Bringup (install channel, config schema, the cold-restart flush) is pinned
// against a live zerofs on the VM — the first managed run is where it's tuned,
// same convention as dittofs.go and the FUSE recipes.
const (
	zerofsNFSPort  = "2049" // zerofs default; free because one backend runs at a time
	zerofsRPCPort  = "7000" // checkpoint/flush/monitor RPC
	zerofsCacheDir = "/var/cache/bench-zerofs"
	zerofsConf     = "/etc/bench-zerofs.toml"
	zerofsPrefix   = "zerofs" // s3://<bucket>/zerofs
	// Encryption is mandatory in zerofs; this is a throwaway VM + throwaway
	// bucket prefix, so a fixed bench password is fine. Passed via env (below),
	// never written into the toml, per the S3-creds-stay-in-env invariant.
	zerofsPassword = "dfsbench-throwaway"
)

func init() {
	register(&Backend{
		Name:     "zerofs",
		S3Backed: true,
		// Native NFSv3 only. nfs4/smb3 are NA (zerofs speaks neither) and auto-skip.
		Support:  map[Protocol]Support{ProtoNFS3: Native},
		Setup:    zerofsSetup,
		Mount:    zerofsMount,
		Unmount:  func(ctx context.Context, _ Protocol) error { return exec.Sh(ctx, "umount", clientMntDir) },
		Teardown: zerofsTeardown,
		// zerofs is native, not FUSE, but it keeps a local LSM block cache of
		// decrypted SST blocks — so dropping the OS page cache alone still serves
		// reads warm. The only way to force cold-from-S3 is to flush + restart the
		// server on an empty cache dir; that requires the unmount→rebuild→remount
		// bounce, which is exactly what the runner drives through FlushFUSE.
		FlushFUSE: zerofsColdBarrier,
	})
}

func zerofsSetup(ctx context.Context, env BackendEnv) error {
	// Official install script (verifies the published SHA-256, drops a prebuilt
	// binary) — zerofs isn't in Ubuntu apt.
	if err := exec.Sh(ctx, "sh", "-c",
		"command -v zerofs >/dev/null || curl -sSfL https://sh.zerofs.net | sh"); err != nil {
		return err
	}
	id, secret, err := s3Creds()
	if err != nil {
		return err
	}
	// zerofs.toml references ${...} and substitutes from the process env at
	// runtime, so the secret + password never touch the file (env-only invariant).
	_ = os.Setenv("AWS_ACCESS_KEY_ID", id)
	_ = os.Setenv("AWS_SECRET_ACCESS_KEY", secret)
	_ = os.Setenv("ZEROFS_PASSWORD", zerofsPassword)
	benchEnv = env

	// Own prefix, cleaned so a re-run isn't served stale segment objects. zerofs
	// writes fresh immutable segments, so a residual-listing miss (SCW's eventual
	// LIST-after-DELETE) doesn't block bringup — unlike juicefs's format gate.
	if err := cleanS3Prefix(ctx, zerofsPrefix+"/"); err != nil {
		return err
	}
	if err := os.WriteFile(zerofsConf, []byte(zerofsConfig(env)), 0o644); err != nil {
		return err
	}
	if err := os.MkdirAll(zerofsCacheDir, 0o755); err != nil {
		return err
	}
	return zerofsStart(ctx)
}

// zerofsConfig renders the TOML. Non-secret fields are filled here; the three
// ${...} refs stay literal for zerofs to substitute from env at runtime.
func zerofsConfig(env BackendEnv) string {
	return fmt.Sprintf(`[storage]
url = "s3://%s/%s"
encryption_password = "${ZEROFS_PASSWORD}"

[cache]
dir = "%s"
disk_size_gb = 10

[servers.nfs]
addresses = ["127.0.0.1:%s"]

[servers.rpc]
addresses = ["127.0.0.1:%s"]

[aws]
access_key_id = "${AWS_ACCESS_KEY_ID}"
secret_access_key = "${AWS_SECRET_ACCESS_KEY}"
endpoint = "%s"
default_region = "us-east-1"
`, env.Bucket, zerofsPrefix, zerofsCacheDir, zerofsNFSPort, zerofsRPCPort, env.Endpoint)
}

func zerofsMount(ctx context.Context, proto Protocol) (string, error) {
	if proto != ProtoNFS3 {
		return "", fmt.Errorf("zerofs: unsupported protocol %s (native nfs3 only)", proto)
	}
	if err := os.MkdirAll(clientMntDir, 0o755); err != nil {
		return "", err
	}
	// zerofs serves NFSv3 on :2049 from its own userspace server (no knfsd) — the
	// same native path dittofs-s3 takes. Use the IDENTICAL option set as the
	// dittofs-s3 nfs3 cell (actimeo=0,nolock) so attribute-cache behavior can't
	// skew the native-vs-native comparison; rsize/wsize negotiate to the kernel
	// default (1 MiB over TCP) for both.
	opts := "nfsvers=3,tcp,port=" + zerofsNFSPort + ",mountport=" + zerofsNFSPort + ",actimeo=0,nolock"
	if err := exec.Sh(ctx, "mount", "-t", "nfs", "-o", opts, "127.0.0.1:/", clientMntDir); err != nil {
		return "", err
	}
	return clientMntDir, nil
}

// zerofsColdBarrier forces the next read cold-from-S3. NFS COMMIT lets zerofs
// ack before data reaches S3, so flush the memtable via its RPC first (nothing
// un-uploaded is lost across the restart), then restart on a wiped cache dir so
// no decrypted SST block is served from local disk. The runner unmounts before
// this and remounts after (Mount).
func zerofsColdBarrier(ctx context.Context) error {
	// Best-effort flush; exact subcommand pinned on the VM. Data written by the
	// layout/warm pass should already be draining, so a miss here at worst leaves
	// the read slightly less cold — it never corrupts.
	_ = exec.Sh(ctx, "sh", "-c", "zerofs flush 127.0.0.1:"+zerofsRPCPort+" 2>/dev/null || true")
	_ = zerofsStop(ctx)
	if err := clearDir(ctx, zerofsCacheDir); err != nil {
		return err
	}
	return zerofsStart(ctx)
}

func zerofsStart(ctx context.Context) error {
	// Truncate the log each start (single '>') so repeated runs don't interleave
	// old and new output — matches dittofs.go.
	if err := exec.Sh(ctx, "sh", "-c",
		"zerofs run -c "+zerofsConf+" >/var/log/bench-zerofs.log 2>&1 &"); err != nil {
		return err
	}
	if err := waitPort(ctx, zerofsNFSPort); err != nil {
		return fmt.Errorf("zerofs did not open NFS port %s: %w", zerofsNFSPort, err)
	}
	return nil
}

// zerofsStop signals the server and waits for it to actually exit — pkill only
// sends the signal, so without the wait a following cache wipe or restart races
// a process still holding the LSM cache dir open.
func zerofsStop(ctx context.Context) error {
	_ = exec.Sh(ctx, "sh", "-c", "pkill -f 'zerofs run' || true")
	for i := 0; i < 50; i++ {
		if exec.Sh(ctx, "sh", "-c", "! pgrep -f 'zerofs run' >/dev/null 2>&1") == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil
}

func zerofsTeardown(ctx context.Context) error {
	_ = zerofsStop(ctx)
	_ = cleanS3Prefix(ctx, zerofsPrefix+"/")
	return os.RemoveAll(zerofsCacheDir)
}

package backend

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/marmos91/dittofs/internal/dfsbench/exec"
)

// FUSE competitors: each mounts its own S3-backed FUSE filesystem at srcDir,
// which the shared re-export layer then serves over nfs3/nfs4/smb3 (the FUSE
// tax the report flags against DittoFS's native path). S3 creds come from the
// process environment (invariant); bucket/endpoint from config.
//
// Recipe flags/URLs are pinned against each tool's installed version on the VM
// (the first managed run is where they get tuned — measure, don't assume).

const (
	rcloneMnt, rcloneCache   = "/mnt/fuse-rclone", "/var/cache/bench-rclone"
	s3qlMnt, s3qlCache       = "/mnt/fuse-s3ql", "/var/cache/bench-s3ql"
	juicefsMnt, juicefsCache = "/mnt/fuse-juicefs", "/var/cache/bench-juicefs"
	s3fsMnt, s3fsCache       = "/mnt/fuse-s3fs", "/var/cache/bench-s3fs"
)

func init() {
	all := []Protocol{ProtoNFS3, ProtoNFS4, ProtoSMB3}

	register(newSrcBackend(srcBackend{
		name: "rclone", s3Backed: true, protos: all, srcDir: rcloneMnt,
		setup: rcloneSetup, teardown: fuseUnmount(rcloneMnt),
		evict: clearCache(rcloneCache),
	}))
	register(newSrcBackend(srcBackend{
		name: "s3ql", s3Backed: true, protos: all, srcDir: s3qlMnt,
		setup: s3qlSetup, teardown: s3qlTeardown,
		evict: func(ctx context.Context) error {
			_ = exec.Sh(ctx, "s3qlctrl", "flushcache", s3qlMnt) // tool cache first, then OS drop
			return clearDir(ctx, s3qlCache)
		},
	}))
	register(newSrcBackend(srcBackend{
		name: "juicefs", s3Backed: true, protos: all, srcDir: juicefsMnt,
		setup: juicefsSetup, teardown: fuseUnmount(juicefsMnt),
		evict: clearCache(juicefsCache),
	}))
	register(newSrcBackend(srcBackend{
		name: "s3fs", s3Backed: true, protos: all, srcDir: s3fsMnt,
		setup: s3fsSetup, teardown: fuseUnmount(s3fsMnt),
		evict: clearCache(s3fsCache),
	}))
}

// s3Creds reads the S3 credentials from the environment (never from config).
func s3Creds() (id, secret string, err error) {
	id, secret = os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY")
	if id == "" || secret == "" {
		return "", "", fmt.Errorf("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY must be set for S3-backed backends")
	}
	return id, secret, nil
}

// ensureInstalled installs pkg via apt if cmd is not already on PATH.
func ensureInstalled(ctx context.Context, cmd, pkg string) error {
	return exec.Sh(ctx, "sh", "-c",
		fmt.Sprintf("command -v %s >/dev/null || { apt-get update && apt-get install -y %s; }", cmd, pkg))
}

// clearDir empties dir (keeping the dir itself); clearCache adapts it to an Evict.
func clearDir(ctx context.Context, dir string) error {
	return exec.Sh(ctx, "sh", "-c", fmt.Sprintf("rm -rf %q/* %q/.[!.]* 2>/dev/null || true", dir, dir))
}
func clearCache(dir string) func(context.Context) error {
	return func(ctx context.Context) error { return clearDir(ctx, dir) }
}

// fuseUnmount lazily unmounts a FUSE mountpoint (best-effort).
func fuseUnmount(mnt string) func(context.Context) error {
	return func(ctx context.Context) error {
		cleanMount(ctx, mnt)
		return nil
	}
}

// cleanMount force-unmounts any stale FUSE mount an aborted run left behind, so
// the next mount doesn't hit "directory already mounted / not empty".
func cleanMount(ctx context.Context, mnt string) {
	// fuse3-only distros ship `fusermount3` and may lack `fusermount`; try both.
	if exec.Sh(ctx, "fusermount3", "-uz", mnt) != nil {
		_ = exec.Sh(ctx, "fusermount", "-uz", mnt)
	}
	_ = exec.Sh(ctx, "umount", "-lf", mnt)
}

// waitMounted blocks until mnt answers as a mountpoint. FUSE tools that
// daemonize (rclone --daemon, juicefs -d) return before the mount is serving;
// re-exporting too early yields an empty/racy export (the juicefs-nfs3 failure).
func waitMounted(ctx context.Context, mnt string) error {
	for i := 0; i < 30; i++ {
		if exec.Sh(ctx, "mountpoint", "-q", mnt) == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("mount %s not ready after wait", mnt)
}

func rcloneSetup(ctx context.Context, env BackendEnv) error {
	if err := ensureInstalled(ctx, "rclone", "rclone"); err != nil {
		return err
	}
	id, secret, err := s3Creds()
	if err != nil {
		return err
	}
	conf := fmt.Sprintf("[bench]\ntype = s3\nprovider = Other\naccess_key_id = %s\nsecret_access_key = %s\nendpoint = %s\nforce_path_style = true\n",
		id, secret, env.Endpoint)
	if err := os.WriteFile("/etc/bench-rclone.conf", []byte(conf), 0o600); err != nil {
		return err
	}
	if err := os.MkdirAll(rcloneCache, 0o755); err != nil {
		return err
	}
	return exec.Sh(ctx, "rclone", "mount", "bench:"+env.Bucket, rcloneMnt,
		"--config", "/etc/bench-rclone.conf", "--cache-dir", rcloneCache,
		"--vfs-cache-mode", "writes", "--daemon")
}

func s3fsSetup(ctx context.Context, env BackendEnv) error {
	if err := ensureInstalled(ctx, "s3fs", "s3fs"); err != nil {
		return err
	}
	id, secret, err := s3Creds()
	if err != nil {
		return err
	}
	if err := os.WriteFile("/etc/passwd-s3fs", []byte(id+":"+secret+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.MkdirAll(s3fsCache, 0o755); err != nil {
		return err
	}
	return exec.Sh(ctx, "s3fs", env.Bucket, s3fsMnt,
		"-o", "passwd_file=/etc/passwd-s3fs", "-o", "url="+env.Endpoint,
		"-o", "use_path_request_style", "-o", "use_cache="+s3fsCache)
}

func juicefsSetup(ctx context.Context, env BackendEnv) error {
	// juicefs isn't in apt — use its install script (idempotent).
	if err := exec.Sh(ctx, "sh", "-c",
		"command -v juicefs >/dev/null || curl -sSL https://d.juicefs.com/install | sh -"); err != nil {
		return err
	}
	id, secret, err := s3Creds()
	if err != nil {
		return err
	}
	meta := "sqlite3:///var/lib/bench-juicefs.db"
	// format is safe to re-run against an existing volume (idempotent).
	if err := exec.Sh(ctx, "juicefs", "format", "--storage", "s3",
		"--bucket", env.Endpoint+"/"+env.Bucket, "--access-key", id, "--secret-key", secret,
		meta, "bench"); err != nil {
		return err
	}
	if err := os.MkdirAll(juicefsCache, 0o755); err != nil {
		return err
	}
	// --writeback: local-ack writes + async upload, matching DittoFS's local
	// store + syncer and rclone's --vfs-cache-mode writes. Off by default in
	// JuiceFS (default flushes to S3 on fsync/close), so without it the write
	// pass compares different durability tiers.
	return exec.Sh(ctx, "juicefs", "mount", "-d", "--writeback", "--cache-dir", juicefsCache, meta, juicefsMnt)
}

func s3qlSetup(ctx context.Context, env BackendEnv) error {
	if err := ensureInstalled(ctx, "mkfs.s3ql", "s3ql"); err != nil {
		return err
	}
	id, secret, err := s3Creds()
	if err != nil {
		return err
	}
	// s3ql addresses generic S3 as s3c://<host>/<bucket>/<prefix>.
	host := stripScheme(env.Endpoint)
	url := fmt.Sprintf("s3c://%s/%s/bench", host, env.Bucket)
	authinfo := fmt.Sprintf("[bench]\nstorage-url: %s\nbackend-login: %s\nbackend-password: %s\n", url, id, secret)
	if err := os.WriteFile("/etc/bench-s3ql-authinfo2", []byte(authinfo), 0o600); err != nil {
		return err
	}
	if err := os.MkdirAll(s3qlCache, 0o755); err != nil {
		return err
	}
	// mkfs is a no-op if the filesystem already exists (--force off).
	_ = exec.Sh(ctx, "mkfs.s3ql", "--authfile", "/etc/bench-s3ql-authinfo2", "--plain", url)
	return exec.Sh(ctx, "mount.s3ql", "--authfile", "/etc/bench-s3ql-authinfo2",
		"--cachedir", s3qlCache, url, s3qlMnt)
}

func s3qlTeardown(ctx context.Context) error {
	_ = exec.Sh(ctx, "umount.s3ql", s3qlMnt)
	return nil
}

// stripScheme drops a leading http(s):// so endpoints slot into tool-specific
// URL forms that want a bare host.
func stripScheme(endpoint string) string {
	for _, p := range []string{"https://", "http://"} {
		if len(endpoint) >= len(p) && endpoint[:len(p)] == p {
			return endpoint[len(p):]
		}
	}
	return endpoint
}

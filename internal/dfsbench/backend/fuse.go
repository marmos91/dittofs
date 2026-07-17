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

	// remount is the cold-read barrier for every FUSE backend: flush writeback to
	// S3 + remount empty (see FlushFUSE). No evict needed — the remount replaces
	// the local cache wholesale.
	register(newSrcBackend(srcBackend{
		name: "rclone", s3Backed: true, protos: all, srcDir: rcloneMnt,
		tier:  "durable-to-local (vfs-cache-mode=writes) + async S3; knfsd sync export",
		setup: rcloneSetup, teardown: fuseUnmount(rcloneMnt), remount: rcloneRemount,
	}))
	register(newSrcBackend(srcBackend{
		name: "s3ql", s3Backed: true, protos: all, srcDir: s3qlMnt,
		tier:  "durable-to-local cache + async S3; knfsd sync export",
		setup: s3qlSetup, teardown: s3qlTeardown, remount: s3qlRemount,
	}))
	register(newSrcBackend(srcBackend{
		name: "juicefs", s3Backed: true, protos: all, srcDir: juicefsMnt,
		tier:  "durable-to-local (--writeback local cache) + async S3; knfsd sync export",
		setup: juicefsSetup, teardown: juicefsTeardown, remount: juicefsRemount,
	}))
	register(newSrcBackend(srcBackend{
		name: "s3fs", s3Backed: true, protos: all, srcDir: s3fsMnt,
		tier:  "durable-on-close to S3 (no writeback); knfsd sync export",
		setup: s3fsSetup, teardown: fuseUnmount(s3fsMnt), remount: s3fsRemount,
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

// s3qlVenv/s3qlTarball: s3ql was dropped from the Ubuntu archive (no apt
// candidate on noble) and its PyPI sdist won't resolve on Python 3.12, so the
// only reliable install is the upstream release tarball into a dedicated venv.
const (
	s3qlVenv    = "/opt/s3ql-venv"
	s3qlTarball = "https://github.com/s3ql/s3ql/releases/download/s3ql-6.2.2/s3ql-6.2.2.tar.gz"
)

// ensureS3QL installs s3ql from the upstream tarball into a venv and exposes its
// CLIs on PATH. Idempotent: a no-op once mkfs.s3ql resolves.
func ensureS3QL(ctx context.Context) error {
	return exec.Sh(ctx, "sh", "-c", `command -v mkfs.s3ql >/dev/null && exit 0
set -e
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq python3-venv python3-dev libsqlite3-dev libfuse3-dev fuse3 pkg-config build-essential
python3 -m venv `+s3qlVenv+`
`+s3qlVenv+`/bin/pip install -q --upgrade pip wheel
`+s3qlVenv+`/bin/pip install -q pyfuse3 "`+s3qlTarball+`"
for b in mkfs.s3ql mount.s3ql umount.s3ql fsck.s3ql s3qladm s3qlctrl; do
  ln -sf `+s3qlVenv+`/bin/"$b" /usr/local/bin/"$b"
done`)
}

// clearDir empties dir (keeping the dir itself).
func clearDir(ctx context.Context, dir string) error {
	return exec.Sh(ctx, "sh", "-c", fmt.Sprintf("rm -rf %q/* %q/.[!.]* 2>/dev/null || true", dir, dir))
}

// fuseUnmount lazily unmounts a FUSE mountpoint (best-effort).
func fuseUnmount(mnt string) func(context.Context) error {
	return func(ctx context.Context) error {
		cleanMount(ctx, mnt)
		return nil
	}
}

// benchEnv is the current run's S3 target (one backend runs at a time, so a
// package var suffices); it lets recipes reach S3 without threading env through
// every one. juicefsVol is the current juicefs volume name.
var (
	benchEnv   BackendEnv
	juicefsVol string
)

// flushUnmount unmounts a FUSE mountpoint non-lazily, so the tool flushes its
// writeback cache to S3 before exiting — a lazy `-uz` would detach and keep
// uploading in the background, which is exactly the race we're avoiding.
// Best-effort: fuse3 then fuse2.
func flushUnmount(ctx context.Context, mnt string) {
	if exec.Sh(ctx, "fusermount3", "-u", mnt) != nil {
		_ = exec.Sh(ctx, "fusermount", "-u", mnt)
	}
	// Only force-detach if the graceful unmount didn't take — a `-lf` while the
	// tool is still flushing its writeback would abandon the un-uploaded tail
	// (rclone's cold-read EIO), the very thing we're unmounting to avoid.
	if exec.Sh(ctx, "mountpoint", "-q", mnt) == nil {
		_ = exec.Sh(ctx, "umount", "-lf", mnt)
	}
}

// juicefsTeardown stops juicefs gracefully, then best-effort-cleans the volume's
// S3 data. `juicefs umount` stops the writeback daemon and flushes its cache (a
// lazy fusermount would leave it uploading); fall back to a force unmount if it
// can't. The prefix clean is hygiene only — a new setup uses a new volume name,
// so a stale-listing miss here never blocks the next format.
func juicefsTeardown(ctx context.Context) error {
	if exec.Sh(ctx, "juicefs", "umount", juicefsMnt) != nil {
		cleanMount(ctx, juicefsMnt)
	}
	if juicefsVol != "" {
		_ = cleanS3Prefix(ctx, juicefsVol+"/")
	}
	return nil
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
	// Own prefix + clean it, so a re-run isn't served stale files and rclone
	// doesn't share the bucket root with other backends.
	benchEnv = env
	if err := cleanS3Prefix(ctx, "rclone/"); err != nil {
		return err
	}
	return rcloneMountFUSE(ctx)
}

func rcloneMountFUSE(ctx context.Context) error {
	return exec.Sh(ctx, "rclone", "mount", "bench:"+benchEnv.Bucket+"/rclone", rcloneMnt,
		"--config", "/etc/bench-rclone.conf", "--cache-dir", rcloneCache,
		"--vfs-cache-mode", "writes", "--daemon")
}

// rcloneRemount flushes rclone's vfs write cache to S3 (non-lazy unmount waits
// for the daemon to upload + exit), clears the on-disk cache, and remounts empty
// so the next read is cold-from-S3.
func rcloneRemount(ctx context.Context) error {
	// Drain the vfs write-back to S3 before unmounting: rclone keeps uploading in
	// the background after `fusermount -u` returns, so without this the remount +
	// cache-wipe races an in-flight upload and the cold read EIOs (rclone-nfs3).
	_ = waitS3Settled(ctx, "rclone/")
	flushUnmount(ctx, rcloneMnt)
	if err := clearDir(ctx, rcloneCache); err != nil {
		return err
	}
	if err := rcloneMountFUSE(ctx); err != nil {
		return err
	}
	// Warm the vfs dir cache from S3: after a fresh mount the listing is empty,
	// so over NFSv3 fio sees the read target as absent and ftruncates it → EIO
	// (rclone-nfs3). A stat/list populates it with the real size first.
	_ = waitMounted(ctx, rcloneMnt)
	_ = exec.Sh(ctx, "ls", rcloneMnt)
	return nil
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
	benchEnv = env
	if err := os.MkdirAll(s3fsCache, 0o755); err != nil {
		return err
	}
	// Clear the on-disk cache before each protocol's mount: the managed run reuses
	// the same filenames across nfs3/nfs4/smb3, and s3fs's stale use_cache entries
	// from a prior protocol EIO the third writer under Samba.
	if err := clearDir(ctx, s3fsCache); err != nil {
		return err
	}
	return s3fsMountFUSE(ctx)
}

func s3fsMountFUSE(ctx context.Context) error {
	// allow_other: smbd's concurrent writers hit EIO on the re-exported mount
	// without it (numjobs>1 SMB writes fail); knfsd tolerates its absence but
	// Samba does not. s3fs runs as root here, so no /etc/fuse.conf edit is needed.
	return exec.Sh(ctx, "s3fs", benchEnv.Bucket, s3fsMnt,
		"-o", "passwd_file=/etc/passwd-s3fs", "-o", "url="+benchEnv.Endpoint,
		"-o", "use_path_request_style", "-o", "use_cache="+s3fsCache, "-o", "allow_other")
}

// s3fsRemount is uniform with the writeback backends' bounce; s3fs is already
// durable-on-close, so this just guarantees a genuinely cold cache for the read.
func s3fsRemount(ctx context.Context) error {
	flushUnmount(ctx, s3fsMnt)
	if err := clearDir(ctx, s3fsCache); err != nil {
		return err
	}
	return s3fsMountFUSE(ctx)
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
	// A fresh volume name per setup sidesteps `format`'s "not empty" gate: SCW's
	// S3 LIST is eventually consistent after DELETE (deleted keys reappear in
	// listings for seconds), so cleaning a fixed prefix then formatting races
	// phantom entries. A never-used prefix is unambiguously empty. Teardown
	// best-effort-cleans this volume's data (stored below for that).
	juicefsVol = fmt.Sprintf("bench-%d", time.Now().UnixNano())
	benchEnv = env
	// juicefs reads creds from ACCESS_KEY/SECRET_KEY — pass via env, never argv,
	// so the secret stays out of the process list and run.log.
	_ = os.Setenv("ACCESS_KEY", id)
	_ = os.Setenv("SECRET_KEY", secret)
	_ = os.Remove("/var/lib/bench-juicefs.db") // fresh meta db for the fresh volume
	if err := exec.Sh(ctx, "juicefs", "format", "--storage", "s3",
		"--bucket", env.Endpoint+"/"+env.Bucket, juicefsMeta, juicefsVol); err != nil {
		return err
	}
	if err := os.MkdirAll(juicefsCache, 0o755); err != nil {
		return err
	}
	return juicefsMountFUSE(ctx)
}

const juicefsMeta = "sqlite3:///var/lib/bench-juicefs.db"

func juicefsMountFUSE(ctx context.Context) error {
	// --writeback: local-ack writes + async upload, matching DittoFS's local
	// store + syncer and rclone's --vfs-cache-mode writes. Off by default in
	// JuiceFS (default flushes to S3 on fsync/close), so without it the write
	// pass compares different durability tiers. --cache-size caps the on-disk
	// cache (default 100 GiB would exceed the VM disk).
	return exec.Sh(ctx, "juicefs", "mount", "-d", "--writeback",
		"--cache-dir", juicefsCache, "--cache-size", "10240", juicefsMeta, juicefsMnt)
}

// juicefsRemount flushes writeback fully to S3, then remounts with an empty
// cache so the next read is cold-from-S3. `juicefs umount` ABANDONS whatever
// writeback hasn't uploaded, and the clearDir would then wipe it — EIOing the
// cold read past that offset — so first wait for the staging cache to drain.
func juicefsRemount(ctx context.Context) error {
	juicefsWaitUploaded(ctx)
	if exec.Sh(ctx, "juicefs", "umount", juicefsMnt) != nil {
		cleanMount(ctx, juicefsMnt)
	}
	if err := clearDir(ctx, juicefsCache); err != nil {
		return err
	}
	return juicefsMountFUSE(ctx)
}

// juicefsWaitUploaded blocks until the writeback staging cache is empty — every
// chunk uploaded to S3 — so the following umount + cache-wipe loses nothing.
// Bounded to ~180s; best-effort.
func juicefsWaitUploaded(ctx context.Context) {
	check := fmt.Sprintf("[ -z \"$(find %q -path '*rawstaging*' -type f -print -quit 2>/dev/null)\" ]", juicefsCache)
	for i := 0; i < 180; i++ {
		if exec.Sh(ctx, "sh", "-c", check) == nil {
			return
		}
		time.Sleep(time.Second)
	}
}

func s3qlSetup(ctx context.Context, env BackendEnv) error {
	if err := ensureS3QL(ctx); err != nil {
		return err
	}
	id, secret, err := s3Creds()
	if err != nil {
		return err
	}
	// Own prefix + clean it (idempotent mkfs on a fresh prefix; no collision with
	// juicefs's bench/).
	benchEnv = env
	if err := cleanS3Prefix(ctx, "s3ql/"); err != nil {
		return err
	}
	url := s3qlURL()
	authinfo := fmt.Sprintf("[bench]\nstorage-url: %s\nbackend-login: %s\nbackend-password: %s\n", url, id, secret)
	if err := os.WriteFile("/etc/bench-s3ql-authinfo2", []byte(authinfo), 0o600); err != nil {
		return err
	}
	if err := os.MkdirAll(s3qlCache, 0o755); err != nil {
		return err
	}
	// mkfs.s3ql refuses to overwrite an existing filesystem, and SCW's eventual
	// LIST-after-DELETE can still surface a prior run's fs to mkfs even after
	// cleanS3Prefix (the same object-store quirk zerofsSetup notes). Force-clear
	// first — best-effort, since a truly empty prefix has nothing to clear. Global
	// options (--authfile) must precede the `clear` action, and its "yes" prompt is
	// answered on stdin. (When SCW rate-limits the clear's object walk this can
	// still fail; s3ql then just produces no cells and the rest of the matrix runs.)
	_ = exec.Sh(ctx, "sh", "-c", fmt.Sprintf("printf 'yes\\n' | s3qladm --authfile /etc/bench-s3ql-authinfo2 clear %s 2>/dev/null; true", url))
	// Fresh prefix, so mkfs creates the filesystem. Surface its error instead of
	// swallowing it — a silent mkfs failure otherwise resurfaces as an opaque
	// `mount.s3ql` exit 31 ("not an s3ql filesystem") that hides the real cause.
	if err := exec.Sh(ctx, "mkfs.s3ql", "--authfile", "/etc/bench-s3ql-authinfo2", "--plain", url); err != nil {
		return fmt.Errorf("mkfs.s3ql: %w", err)
	}
	return s3qlMountFUSE(ctx)
}

// s3qlURL addresses generic S3 as s3c://<host>/<bucket>/<prefix>.
func s3qlURL() string {
	return fmt.Sprintf("s3c://%s/%s/s3ql", stripScheme(benchEnv.Endpoint), benchEnv.Bucket)
}

func s3qlMountFUSE(ctx context.Context) error {
	return exec.Sh(ctx, "mount.s3ql", "--authfile", "/etc/bench-s3ql-authinfo2",
		"--cachedir", s3qlCache, s3qlURL(), s3qlMnt)
}

// s3qlRemount flushes s3ql to S3 (umount.s3ql uploads its cache), clears the
// local cache, and remounts empty for a cold read.
func s3qlRemount(ctx context.Context) error {
	_ = exec.Sh(ctx, "umount.s3ql", s3qlMnt)
	if err := clearDir(ctx, s3qlCache); err != nil {
		return err
	}
	return s3qlMountFUSE(ctx)
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

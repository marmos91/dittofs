package backend

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/marmos91/dittofs/internal/dfsbench/exec"
)

// nfs-ganesha is the native userspace-NFS baseline: like DittoFS it serves NFS
// from its own process, but over a LOCAL ext4 dir (FSAL_VFS), no S3. So it
// isolates the userspace-NFS-server tax from any storage backend — the pure
// protocol-server ceiling, complementing local-disk (which measures the knfsd +
// FUSE-re-export path). Native NFSv3 + NFSv4.1 only (no SMB), so it's not a
// srcBackend; it registers directly, the zerofs.go shape.
//
// Bringup (apt package names, config schema, the launch invocation) is pinned
// against the installed nfs-ganesha on the VM — the first managed run is where
// it's tuned, same convention as zerofs.go.
const (
	ganeshaExportDir = "/var/lib/bench-ganesha"
	ganeshaConf      = "/etc/ganesha/ganesha.conf"
	ganeshaLog       = "/var/log/bench-ganesha.log"
	ganeshaNFSPort   = "2049"
)

func init() {
	register(&Backend{
		Name:     "ganesha",
		S3Backed: false,
		Tier:     "userspace NFS (nfs-ganesha FSAL_VFS) over local ext4; no S3",
		// Native NFSv3 + NFSv4.1 only; smb3 is NA (ganesha speaks no SMB) and skips.
		Support:  map[Protocol]Support{ProtoNFS3: Native, ProtoNFS4: Native},
		Setup:    ganeshaSetup,
		Mount:    ganeshaMount,
		Unmount:  func(ctx context.Context, _ Protocol) error { return exec.Sh(ctx, "umount", clientMntDir) },
		Teardown: ganeshaTeardown,
		// No S3 and no tool cache: an OS page-cache drop between passes suffices, so
		// no Evict/FlushFUSE (same as local-disk).
	})
}

func ganeshaSetup(ctx context.Context, env BackendEnv) error {
	if err := exec.Sh(ctx, "sh", "-c",
		"command -v ganesha.nfsd >/dev/null || { apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y nfs-ganesha nfs-ganesha-vfs; }"); err != nil {
		return err
	}
	if err := os.MkdirAll(ganeshaExportDir, 0o777); err != nil {
		return err
	}
	if err := os.MkdirAll("/etc/ganesha", 0o755); err != nil {
		return err
	}
	// Free :2049 for ganesha's own userspace server — the image ships
	// nfs-kernel-server bound there. Identical dance to zerofsSetup: stop the knfsd
	// units, tear down the in-kernel [nfsd] threads (systemctl stop leaves them
	// bound, so rpc.nfsd 0 is required), and wait until the port is genuinely free.
	// Also SIGKILL any ganesha left by a crashed prior run so the fresh one can bind.
	// Match ganesha by exact process name (-x ganesha.nfsd), never `-f`: this runs
	// in an sh -c whose argv could self-match (same self-kill pitfall as -x dfs).
	_ = exec.Sh(ctx, "sh", "-c", "pkill -9 -x ganesha.nfsd 2>/dev/null; systemctl stop nfs-server nfs-kernel-server nfs-mountd 2>/dev/null; exportfs -ua 2>/dev/null; for i in $(seq 1 20); do rpc.nfsd 0 2>/dev/null; ss -ltn 2>/dev/null | grep -q ':2049 ' || break; sleep 1; done; true")
	if err := os.WriteFile(ganeshaConf, []byte(ganeshaConfig()), 0o644); err != nil {
		return err
	}
	return ganeshaStart(ctx)
}

func ganeshaConfig() string {
	// Pseudo="/" makes the export the NFSv4 pseudo-root (v4 mounts "/"); v3 mounts
	// the Path. No_root_squash so the AUTH_SYS root client can write, matching the
	// dittofs/reexport exports.
	return fmt.Sprintf(`EXPORT {
	Export_Id = 1;
	Path = "%s";
	Pseudo = "/";
	Access_Type = RW;
	Squash = "No_root_squash";
	Transports = "TCP";
	Protocols = "3", "4";
	FSAL { Name = "VFS"; }
}
`, ganeshaExportDir)
}

func ganeshaStart(ctx context.Context) error {
	// ganesha.nfsd daemonizes by default; -N NIV_EVENT keeps the log terse.
	if err := exec.Sh(ctx, "sh", "-c",
		"ganesha.nfsd -f "+ganeshaConf+" -L "+ganeshaLog+" -N NIV_EVENT"); err != nil {
		return err
	}
	if err := waitPort(ctx, ganeshaNFSPort); err != nil {
		return fmt.Errorf("ganesha did not open NFS port %s (see %s): %w", ganeshaNFSPort, ganeshaLog, err)
	}
	return nil
}

func ganeshaMount(ctx context.Context, proto Protocol) (string, error) {
	if err := prepareMountpoint(ctx); err != nil {
		return "", err
	}
	var opts, src string
	switch proto {
	case ProtoNFS3:
		// Same attr-cache + parallelism as the dittofs/reexport nfs3 cells so the
		// comparison stays clean; nolock is v3-only (no NLM statd wired).
		opts, src = "nfsvers=3,tcp,actimeo=1,nconnect=4,nolock", "127.0.0.1:"+ganeshaExportDir
	case ProtoNFS4:
		opts, src = "nfsvers=4.1,tcp,actimeo=1,nconnect=4", "127.0.0.1:/"
	default:
		return "", fmt.Errorf("ganesha: unsupported protocol %s (native nfs3/nfs4 only)", proto)
	}
	if err := exec.Sh(ctx, "mount", "-t", "nfs", "-o", opts, src, clientMntDir); err != nil {
		return "", err
	}
	return clientMntDir, nil
}

// ganeshaStop signals the server and waits for it to actually exit before a
// following teardown/restart races a process still holding the export.
func ganeshaStop(ctx context.Context) error {
	_ = exec.Sh(ctx, "sh", "-c", "pkill -x ganesha.nfsd || true")
	for i := 0; i < 50; i++ {
		if exec.Sh(ctx, "sh", "-c", "! pgrep -x ganesha.nfsd >/dev/null 2>&1") == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil
}

func ganeshaTeardown(ctx context.Context) error {
	_ = ganeshaStop(ctx)
	// Restore the kernel NFS server that setup stopped, so a reexport backend
	// scheduled after ganesha still has a knfsd to export into.
	_ = exec.Sh(ctx, "sh", "-c", "systemctl start nfs-kernel-server nfs-server 2>/dev/null; true")
	return os.RemoveAll(ganeshaExportDir)
}

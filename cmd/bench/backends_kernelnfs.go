package main

import (
	"context"
	"fmt"
	"os"
)

// kernel-nfs is the local-disk control: a plain directory re-exported over the
// in-kernel NFS server (knfsd) and mounted back over loopback. No S3, no FUSE —
// so it doubles as the NFS protocol-overhead ceiling every S3-backed cell is
// read against. smb3 is N/A here (that's the Samba control, a separate backend).
const (
	kernelNFSExportDir = "/srv/bench-kernelnfs"
	kernelNFSMountDir  = "/mnt/bench-kernelnfs"
	kernelNFSExports   = "/etc/exports.d/bench-kernelnfs.exports"
)

func init() {
	register(&Backend{
		Name:     "kernel-nfs",
		S3Backed: false,
		Support:  map[Protocol]Support{ProtoNFS3: Reexport, ProtoNFS4: Reexport},
		Setup:    kernelNFSSetup,
		Mount:    kernelNFSMount,
		Unmount:  kernelNFSUnmount,
		Teardown: kernelNFSTeardown,
		// Evict is nil: local disk has no per-tool cache — the universal OS
		// page-cache drop (dropOSCache) is the whole of its cold pass.
	})
}

func kernelNFSSetup(ctx context.Context, _ BackendEnv) error {
	if err := os.MkdirAll(kernelNFSExportDir, 0o777); err != nil {
		return err
	}
	// Install knfsd if absent (idempotent), then (re)start it.
	if err := sh(ctx, "sh", "-c",
		"command -v exportfs >/dev/null || { apt-get update && apt-get install -y nfs-kernel-server; }"); err != nil {
		return err
	}
	if err := sh(ctx, "systemctl", "restart", "nfs-kernel-server"); err != nil {
		return err
	}
	// Export to loopback. fsid=0 makes this the NFSv4 pseudo-root so a v4 client
	// mounts "/"; v3 clients mount the real path.
	line := fmt.Sprintf("%s 127.0.0.1(rw,sync,no_subtree_check,no_root_squash,fsid=0)\n", kernelNFSExportDir)
	if err := os.MkdirAll("/etc/exports.d", 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(kernelNFSExports, []byte(line), 0o644); err != nil {
		return err
	}
	return sh(ctx, "exportfs", "-ra")
}

func kernelNFSMount(ctx context.Context, proto Protocol) (string, error) {
	var vers, src string
	switch proto {
	case ProtoNFS3:
		vers, src = "3", "127.0.0.1:"+kernelNFSExportDir
	case ProtoNFS4:
		vers, src = "4.1", "127.0.0.1:/" // fsid=0 pseudo-root
	default:
		return "", fmt.Errorf("kernel-nfs: unsupported protocol %s", proto)
	}
	if err := os.MkdirAll(kernelNFSMountDir, 0o755); err != nil {
		return "", err
	}
	if err := sh(ctx, "mount", "-t", "nfs", "-o", "vers="+vers, src, kernelNFSMountDir); err != nil {
		return "", err
	}
	return kernelNFSMountDir, nil
}

func kernelNFSUnmount(ctx context.Context, _ Protocol) error {
	return sh(ctx, "umount", kernelNFSMountDir)
}

func kernelNFSTeardown(ctx context.Context) error {
	_ = os.Remove(kernelNFSExports)
	_ = sh(ctx, "exportfs", "-ra")
	return os.RemoveAll(kernelNFSExportDir)
}

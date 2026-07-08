package main

import (
	"context"
	_ "embed"
	"fmt"
	"os"
)

// Re-export layer: re-serve a source directory (a FUSE mountpoint or a plain
// dir) over NFS (knfsd) or SMB (Samba) and mount it back over loopback. This is
// the shared plumbing that gives every FUSE competitor its nfs3/nfs4/smb3 cells
// for free — only Setup (install+FUSE-mount) and Evict differ per backend.
//
// One backend runs at a time (full teardown between competitors), so the layer
// uses fixed paths and a single NFS export / Samba share. The VM is disposable,
// so it overwrites /etc/samba/smb.conf outright rather than merging.
const (
	clientMntDir   = "/mnt/bench-client"
	nfsExportsFile = "/etc/exports.d/bench.exports"
	sambaConfFile  = "/etc/samba/smb.conf"
	sambaShare     = "bench"
)

//go:embed configs/smb.conf.tmpl
var smbConfTmpl string

// reexportMount re-serves srcDir over proto and returns the loopback client
// mountpoint. srcDir must already exist and hold the backend's data.
func reexportMount(ctx context.Context, srcDir string, proto Protocol) (string, error) {
	if err := os.MkdirAll(clientMntDir, 0o755); err != nil {
		return "", err
	}
	switch proto {
	case ProtoNFS3:
		return nfsReexport(ctx, srcDir, "3")
	case ProtoNFS4:
		return nfsReexport(ctx, srcDir, "4.1")
	case ProtoSMB3:
		return smbReexport(ctx, srcDir)
	default:
		return "", fmt.Errorf("re-export: unsupported protocol %s", proto)
	}
}

// reexportUnmount reverses reexportMount for proto.
func reexportUnmount(ctx context.Context, proto Protocol) error {
	_ = sh(ctx, "umount", clientMntDir)
	switch proto {
	case ProtoNFS3, ProtoNFS4:
		_ = os.Remove(nfsExportsFile)
		return sh(ctx, "exportfs", "-ra")
	case ProtoSMB3:
		return nil // smbd keeps running; the share dir is torn down by the backend
	}
	return nil
}

func nfsReexport(ctx context.Context, srcDir, vers string) (string, error) {
	if err := sh(ctx, "sh", "-c",
		"command -v exportfs >/dev/null || { apt-get update && apt-get install -y nfs-kernel-server; }"); err != nil {
		return "", err
	}
	if err := os.MkdirAll("/etc/exports.d", 0o755); err != nil {
		return "", err
	}
	// fsid=0 makes srcDir the NFSv4 pseudo-root (v4 mounts "/"); v3 mounts the path.
	line := fmt.Sprintf("%s 127.0.0.1(rw,sync,no_subtree_check,no_root_squash,fsid=0)\n", srcDir)
	if err := os.WriteFile(nfsExportsFile, []byte(line), 0o644); err != nil {
		return "", err
	}
	if err := sh(ctx, "systemctl", "restart", "nfs-kernel-server"); err != nil {
		return "", err
	}
	if err := sh(ctx, "exportfs", "-ra"); err != nil {
		return "", err
	}
	src := "127.0.0.1:" + srcDir
	if vers != "3" {
		src = "127.0.0.1:/"
	}
	if err := sh(ctx, "mount", "-t", "nfs", "-o", "vers="+vers, src, clientMntDir); err != nil {
		return "", err
	}
	return clientMntDir, nil
}

func smbReexport(ctx context.Context, srcDir string) (string, error) {
	if err := sh(ctx, "sh", "-c",
		"command -v smbd >/dev/null || { apt-get update && apt-get install -y samba cifs-utils; }"); err != nil {
		return "", err
	}
	conf := expandJob(smbConfTmpl, map[string]string{"SHARE": sambaShare, "SRC_PATH": srcDir})
	if err := os.WriteFile(sambaConfFile, []byte(conf), 0o644); err != nil {
		return "", err
	}
	if err := sh(ctx, "systemctl", "restart", "smbd"); err != nil {
		return "", err
	}
	// Guest share (map to guest = Bad User) — no auth machinery for a localhost
	// disposable VM.
	if err := sh(ctx, "mount", "-t", "cifs", "//127.0.0.1/"+sambaShare, clientMntDir,
		"-o", "guest,vers=3.0,uid=0,gid=0"); err != nil {
		return "", err
	}
	return clientMntDir, nil
}

// srcBackend describes a re-export-based backend: its bytes sit behind srcDir,
// which the shared layer re-serves over each protocol in protos. This is the
// "add a competitor = 1 registry entry + recipes" seam.
type srcBackend struct {
	name     string
	s3Backed bool
	protos   []Protocol
	srcDir   string
	setup    func(ctx context.Context, env BackendEnv) error // install + FUSE-mount at srcDir; nil = plain dir
	teardown func(ctx context.Context) error                 // FUSE-unmount; nil = none
	evict    func(ctx context.Context) error                 // clear tool cache; nil = OS-drop only
}

// newSrcBackend wires a srcBackend into a Backend, routing all protocols through
// the shared re-export layer.
func newSrcBackend(sb srcBackend) *Backend {
	support := make(map[Protocol]Support, len(sb.protos))
	for _, p := range sb.protos {
		support[p] = Reexport
	}
	return &Backend{
		Name:     sb.name,
		S3Backed: sb.s3Backed,
		Support:  support,
		Setup: func(ctx context.Context, env BackendEnv) error {
			if err := os.MkdirAll(sb.srcDir, 0o777); err != nil {
				return err
			}
			if sb.setup != nil {
				return sb.setup(ctx, env)
			}
			return nil
		},
		Mount:   func(ctx context.Context, proto Protocol) (string, error) { return reexportMount(ctx, sb.srcDir, proto) },
		Unmount: func(ctx context.Context, proto Protocol) error { return reexportUnmount(ctx, proto) },
		Evict:   sb.evict,
		Teardown: func(ctx context.Context) error {
			var err error
			if sb.teardown != nil {
				err = sb.teardown(ctx)
			}
			_ = os.RemoveAll(sb.srcDir)
			return err
		},
	}
}

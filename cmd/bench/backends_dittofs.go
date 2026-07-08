package main

import (
	"context"
	"fmt"
	"os"
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
)

func init() {
	register(&Backend{
		Name:     "dittofs-s3",
		S3Backed: true,
		Support:  map[Protocol]Support{ProtoNFS3: Native, ProtoNFS4: Native, ProtoSMB3: Native},
		Setup:    dittofsSetup,
		Mount:    dittofsMount,
		Evict:    dittofsEvict,
		Unmount:  func(ctx context.Context, _ Protocol) error { return sh(ctx, "umount", clientMntDir) },
		Teardown: dittofsTeardown,
	})
}

func dittofsSetup(ctx context.Context, env BackendEnv) error {
	id, secret, err := s3Creds()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dittofsDataDir, 0o755); err != nil {
		return err
	}
	// Start the server (config schema pinned on the VM), then wait for its NFS
	// port before driving dfsctl.
	if err := sh(ctx, "sh", "-c", "dfs start >/var/log/bench-dittofs.log 2>&1 &"); err != nil {
		return err
	}
	if err := waitPort(ctx, dittofsNFSPort); err != nil {
		return fmt.Errorf("dfs did not open NFS port %s: %w", dittofsNFSPort, err)
	}
	// Attach an S3 remote block store and create the share bound to it.
	storeCfg := fmt.Sprintf(`{"bucket":%q,"endpoint":%q,"access_key":%q,"secret_key":%q,"force_path_style":true}`,
		env.Bucket, env.Endpoint, id, secret)
	if err := sh(ctx, "dfsctl", "store", "block", "remote", "add",
		"--name", "bench-s3", "--type", "s3", "--config", storeCfg); err != nil {
		return err
	}
	return sh(ctx, "dfsctl", "share", "create", "--name", dittofsShare, "--block-store", "bench-s3")
}

func dittofsMount(ctx context.Context, proto Protocol) (string, error) {
	if err := os.MkdirAll(clientMntDir, 0o755); err != nil {
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
	if err := sh(ctx, "mount", "-t", typ, "-o", opts, src, clientMntDir); err != nil {
		return "", err
	}
	return clientMntDir, nil
}

// dittofsEvict drops locally-cached blocks so the next read is cold-from-S3.
// #1595's DrainLocalSynced is what makes `store block evict` actually force it.
func dittofsEvict(ctx context.Context) error {
	return sh(ctx, "dfsctl", "store", "block", "evict")
}

func dittofsTeardown(ctx context.Context) error {
	_ = sh(ctx, "sh", "-c", "pkill -f 'dfs start' || true")
	return os.RemoveAll(dittofsDataDir)
}

// waitPort blocks until 127.0.0.1:port accepts a connection or ~60s elapse.
func waitPort(ctx context.Context, port string) error {
	return sh(ctx, "sh", "-c",
		fmt.Sprintf("for i in $(seq 1 60); do nc -z 127.0.0.1 %s && exit 0; sleep 1; done; exit 1", port))
}

package backend

import (
	"context"
	"os"

	"github.com/marmos91/dittofs/internal/dfsbench/exec"
)

// goofys is a read-optimized S3 FUSE filesystem with NO local cache: it
// write-through-uploads to S3 on close and reads straight from S3. That makes it
// the no-cache FUSE comparator (against s3fs-nocache) — the re-export layer then
// serves it over nfs3/nfs4/smb3 like every other FUSE competitor.
//
// Bringup (release URL, flag names) is pinned against the installed goofys on the
// VM — the first managed run is where it gets tuned, same convention as the other
// FUSE recipes.
const goofysMnt = "/mnt/fuse-goofys"

func init() {
	register(newSrcBackend(srcBackend{
		name: "goofys", s3Backed: true, protos: []Protocol{ProtoNFS3, ProtoNFS4, ProtoSMB3},
		srcDir:   goofysMnt,
		tier:     "no local cache, write-through to S3 on close; knfsd sync export",
		setup:    goofysSetup,
		teardown: fuseUnmount(goofysMnt),
		remount:  goofysRemount,
	}))
}

func goofysSetup(ctx context.Context, env BackendEnv) error {
	// goofys isn't in apt — drop the prebuilt release binary onto PATH (idempotent).
	if err := exec.Sh(ctx, "sh", "-c",
		"command -v goofys >/dev/null || { curl -sSfL https://github.com/kahing/goofys/releases/latest/download/goofys -o /usr/local/bin/goofys && chmod +x /usr/local/bin/goofys; }"); err != nil {
		return err
	}
	id, secret, err := s3Creds()
	if err != nil {
		return err
	}
	// goofys reads creds from the AWS_* env vars (never argv), keeping the secret
	// out of the process list and run.log.
	_ = os.Setenv("AWS_ACCESS_KEY_ID", id)
	_ = os.Setenv("AWS_SECRET_ACCESS_KEY", secret)
	benchEnv = env
	// Own prefix, cleaned so a re-run isn't served stale objects.
	if err := cleanS3Prefix(ctx, "goofys/"); err != nil {
		return err
	}
	return goofysMountFUSE(ctx)
}

func goofysMountFUSE(ctx context.Context) error {
	// goofys mounts <bucket>:<prefix> at the mountpoint; --endpoint targets the
	// generic S3 endpoint. It daemonizes, so newSrcBackend's waitMounted gates the
	// re-export until it's serving.
	return exec.Sh(ctx, "goofys", "--endpoint", benchEnv.Endpoint,
		benchEnv.Bucket+":goofys", goofysMnt)
}

// goofysRemount forces the next read cold-from-S3. goofys has no local cache, so
// a non-lazy unmount + fresh mount is enough (nothing to wipe); it's uniform with
// the other FUSE backends' bounce so the runner drives it the same way.
func goofysRemount(ctx context.Context) error {
	flushUnmount(ctx, goofysMnt)
	return goofysMountFUSE(ctx)
}

//go:build e2e

// Package live holds e2e tests that run against a DittoFS deployment the test did
// not start: a real server, share, users and S3 bucket, named by the variables
// below. The rest of test/e2e starts and owns a server per test. These tests only
// use what they are given: they create files under a run directory of the given
// share, and remove them again.
//
// Every test skips unless DITTOFS_E2E_LIVE_API is set.
//
//	DITTOFS_E2E_LIVE_API             control-plane URL, e.g. http://127.0.0.1:8080
//	DITTOFS_E2E_LIVE_ADMIN_USER      an admin account (GC and evict are admin operations)
//	DITTOFS_E2E_LIVE_ADMIN_PASSWORD
//	DITTOFS_E2E_LIVE_SHARE           the share under test, e.g. /canary
//	DITTOFS_E2E_LIVE_SMB_HOST        default: the API URL's host
//	DITTOFS_E2E_LIVE_SMB_PORT        default: 12445
//	DITTOFS_E2E_LIVE_SMB_USER        an account with read-write on the share
//	DITTOFS_E2E_LIVE_SMB_PASSWORD
//	DITTOFS_E2E_LIVE_S3_ENDPOINT     the share's block store, to check its objects
//	DITTOFS_E2E_LIVE_S3_REGION       default: us-east-1
//	DITTOFS_E2E_LIVE_S3_BUCKET
//	DITTOFS_E2E_LIVE_S3_PREFIX       the block store's key prefix; it must hold this share's objects only
//	DITTOFS_E2E_LIVE_S3_ACCESS_KEY
//	DITTOFS_E2E_LIVE_S3_SECRET_KEY
//	DITTOFS_E2E_LIVE_S3_PROVIDER     rclone's name for the S3 provider, e.g. Cubbit (default: Other)
//	DITTOFS_E2E_LIVE_RCLONE          the operators' rclone, for `rclone size` (default: rclone on PATH)
//	DITTOFS_E2E_LIVE_DFSCTL          the server's own dfsctl (default: built from this checkout)
//	DITTOFS_E2E_LIVE_RESULT          optional: write a JSON summary of the run to this file
//
// The S3 prefix must be the share's own. Tests that check that objects go away
// after GC count everything under it.
package live

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsConfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
)

const envPrefix = "DITTOFS_E2E_LIVE_"

// BlocksDir is where the S3 block store keeps its objects under its prefix
// (blocks/<blockID>). It writes nothing else there.
const BlocksDir = "blocks/"

// Target is a live deployment under test, read from DITTOFS_E2E_LIVE_*.
type Target struct {
	APIURL        string
	AdminUser     string
	AdminPassword string
	Share         string
	SMBHost       string
	SMBPort       int
	SMBUser       string
	SMBPassword   string
	S3Endpoint    string
	S3Region      string
	S3Bucket      string
	S3Prefix      string
	S3AccessKey   string
	S3SecretKey   string
	S3Provider    string
	Rclone        string
	Dfsctl        string
	ResultFile    string
}

// FromEnv reads the live target. It skips the test when DITTOFS_E2E_LIVE_API is
// unset, and fails it when the target is only partly configured.
func FromEnv(t *testing.T) *Target {
	t.Helper()
	get := func(k string) string { return os.Getenv(envPrefix + k) }
	if get("API") == "" {
		t.Skip("no live target: set DITTOFS_E2E_LIVE_API and the other DITTOFS_E2E_LIVE_* variables (see test/e2e/live)")
	}
	tg := &Target{
		APIURL:        get("API"),
		AdminUser:     get("ADMIN_USER"),
		AdminPassword: get("ADMIN_PASSWORD"),
		Share:         get("SHARE"),
		SMBHost:       get("SMB_HOST"),
		SMBUser:       get("SMB_USER"),
		SMBPassword:   get("SMB_PASSWORD"),
		S3Endpoint:    get("S3_ENDPOINT"),
		S3Region:      get("S3_REGION"),
		S3Bucket:      get("S3_BUCKET"),
		S3Prefix:      get("S3_PREFIX"),
		S3AccessKey:   get("S3_ACCESS_KEY"),
		S3SecretKey:   get("S3_SECRET_KEY"),
		S3Provider:    get("S3_PROVIDER"),
		Rclone:        get("RCLONE"),
		Dfsctl:        get("DFSCTL"),
		ResultFile:    get("RESULT"),
		SMBPort:       12445,
	}
	if tg.SMBHost == "" {
		if u, err := url.Parse(tg.APIURL); err == nil {
			tg.SMBHost = u.Hostname()
		}
	}
	if p := get("SMB_PORT"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			t.Fatalf("DITTOFS_E2E_LIVE_SMB_PORT=%q: %v", p, err)
		}
		tg.SMBPort = n
	}
	if tg.S3Region == "" {
		tg.S3Region = "us-east-1"
	}
	if tg.S3Provider == "" {
		tg.S3Provider = "Other"
	}
	var missing []string
	for k, v := range map[string]string{
		"ADMIN_USER": tg.AdminUser, "ADMIN_PASSWORD": tg.AdminPassword, "SHARE": tg.Share,
		"SMB_USER": tg.SMBUser, "SMB_PASSWORD": tg.SMBPassword, "S3_ENDPOINT": tg.S3Endpoint,
		"S3_BUCKET": tg.S3Bucket, "S3_PREFIX": tg.S3Prefix, "S3_ACCESS_KEY": tg.S3AccessKey,
		"S3_SECRET_KEY": tg.S3SecretKey,
	} {
		if v == "" {
			missing = append(missing, envPrefix+k)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("live target incomplete, missing: %s", strings.Join(missing, ", "))
	}
	return tg
}

// Admin logs in as the target's admin account, with the server's own dfsctl when
// DITTOFS_E2E_LIVE_DFSCTL names one.
func (tg *Target) Admin(t *testing.T) *helpers.CLIRunner {
	t.Helper()
	var opts []helpers.RunnerOption
	if tg.Dfsctl != "" {
		opts = append(opts, helpers.WithDfsctlBinary(tg.Dfsctl))
	}
	return helpers.LoginWithCredentials(t, tg.APIURL, tg.AdminUser, tg.AdminPassword, opts...)
}

// Bucket returns the framework's S3 helper over the target's real bucket, so the
// listing code is the same the Localstack-backed tests use.
func (tg *Target) Bucket(t *testing.T) *framework.LocalstackHelper {
	t.Helper()
	cfg, err := awsConfig.LoadDefaultConfig(context.Background(),
		awsConfig.WithRegion(tg.S3Region),
		awsConfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(tg.S3AccessKey, tg.S3SecretKey, "")),
	)
	if err != nil {
		t.Fatalf("S3 config: %v", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(tg.S3Endpoint)
		o.UsePathStyle = true
	})
	return &framework.LocalstackHelper{T: t, Endpoint: tg.S3Endpoint, Client: client}
}

// Objects counts what is under the share's prefix: objects and bytes (the numbers
// `rclone size` reports), and, from an S3 listing, the keys.
type Objects struct {
	Count int
	Bytes int64
	Keys  map[string]int64
}

// Objects lists everything under the share's prefix through the S3 API.
func (tg *Target) Objects(t *testing.T, b *framework.LocalstackHelper) Objects {
	t.Helper()
	o := Objects{Keys: map[string]int64{}}
	for _, obj := range b.ListS3PrefixWithSizes(t, tg.S3Bucket, tg.S3Prefix) {
		o.Keys[obj.Key] = obj.Size
		o.Count++
		o.Bytes += obj.Size
	}
	return o
}

// rcloneRemote names the rclone remote for the target's bucket. It is defined in the
// environment of each rclone call (RCLONE_CONFIG_DITTOFSLIVE_*), like an rclone.conf
// section with type, provider, env_auth, endpoint and keys, so the credentials reach
// neither a command line nor a file.
const rcloneRemote = "dittofslive"

// rclone runs the target's rclone and returns its standard output.
func (tg *Target) rclone(t *testing.T, args ...string) []byte {
	t.Helper()
	bin := tg.Rclone
	if bin == "" {
		p, err := exec.LookPath("rclone")
		if err != nil {
			t.Fatalf("rclone not found: set DITTOFS_E2E_LIVE_RCLONE")
		}
		bin = p
	}
	cfg := "RCLONE_CONFIG_" + strings.ToUpper(rcloneRemote) + "_"
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(),
		cfg+"TYPE=s3", cfg+"PROVIDER="+tg.S3Provider, cfg+"ENV_AUTH=false", cfg+"ENDPOINT="+tg.S3Endpoint,
		cfg+"ACCESS_KEY_ID="+tg.S3AccessKey, cfg+"SECRET_ACCESS_KEY="+tg.S3SecretKey)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s: %v: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out
}

// RcloneVersion is the first line of `rclone version`, e.g. "rclone v1.75.1".
func (tg *Target) RcloneVersion(t *testing.T) string {
	t.Helper()
	line, _, _ := strings.Cut(string(tg.rclone(t, "version")), "\n")
	return strings.TrimSpace(line)
}

// RcloneSize is `rclone size` of sub under the share's prefix ("" for all of it,
// BlocksDir for the block objects), the operators' own check, run with their binary
// when DITTOFS_E2E_LIVE_RCLONE names it. Keys is nil.
func (tg *Target) RcloneSize(t *testing.T, sub string) Objects {
	t.Helper()
	var size struct {
		Count int   `json:"count"`
		Bytes int64 `json:"bytes"`
	}
	out := tg.rclone(t, "size", "--json", rcloneRemote+":"+tg.S3Bucket+"/"+tg.S3Prefix+sub)
	if err := json.Unmarshal(out, &size); err != nil {
		t.Fatalf("rclone size output %q: %v", out, err)
	}
	return Objects{Count: size.Count, Bytes: size.Bytes}
}

// MountSMB mounts the share with the Linux kernel SMB client and unmounts it when
// the test ends. cache=none sends every read to the server, so a read-back tests
// DittoFS, not the client's page cache. The password goes into a credentials file,
// not onto the mount command line.
func (tg *Target) MountSMB(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("live SMB tests use the Linux CIFS client")
	}
	if os.Geteuid() != 0 {
		t.Skip("mounting needs root")
	}
	dir := t.TempDir()
	mnt := filepath.Join(dir, "mnt")
	cred := filepath.Join(dir, "smb.cred")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cred, []byte(fmt.Sprintf("username=%s\npassword=%s\n", tg.SMBUser, tg.SMBPassword)), 0o600); err != nil {
		t.Fatal(err)
	}
	src := fmt.Sprintf("//%s/%s", tg.SMBHost, strings.TrimPrefix(tg.Share, "/"))
	opts := fmt.Sprintf("port=%d,credentials=%s,vers=3.1.1,cache=none,uid=0,gid=0", tg.SMBPort, cred)
	if out, err := exec.Command("mount", "-t", "cifs", src, mnt, "-o", opts).CombinedOutput(); err != nil {
		t.Fatalf("mount.cifs %s: %v: %s", src, err, strings.TrimSpace(string(out)))
	}
	// Registered after t.TempDir, so it runs first: unmount, then remove the dir.
	t.Cleanup(func() {
		if exec.Command("umount", mnt).Run() != nil {
			_ = exec.Command("umount", "-f", "-l", mnt).Run()
		}
	})
	return mnt
}

// WaitUploaded waits until the share has nothing left to upload to its block store
// and returns how long that took.
func WaitUploaded(t *testing.T, runner *helpers.CLIRunner, share string, timeout time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	for {
		st := helpers.GetBlockStats(t, runner, share)
		if st.Totals.UnsyncedBytes == 0 && st.Totals.PendingUploads == 0 {
			return time.Since(start)
		}
		if time.Since(start) > timeout {
			t.Fatalf("upload not finished after %s: unsynced_bytes=%d pending_uploads=%d",
				timeout, st.Totals.UnsyncedBytes, st.Totals.PendingUploads)
		}
		time.Sleep(2 * time.Second)
	}
}

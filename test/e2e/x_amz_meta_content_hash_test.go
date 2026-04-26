//go:build e2e

// TestExternalVerifier_ContentHashHeader confirms BSCAS-06 (D-33): every
// cas/.../ object stamped by Phase 11's syncer carries the
// x-amz-meta-content-hash user-metadata header so that EXTERNAL tooling
// (e.g., aws s3api head-object) can verify integrity without depending
// on DittoFS's own metadata store.
//
// The test:
//
//  1. Writes a small file (4 MiB — large enough to produce ≥1 chunk
//     under FastCDC's 1 MiB minimum, but small enough to keep the test
//     fast) through DittoFS via NFS.
//  2. Drains uploads.
//  3. Lists every cas/.../ object directly via the AWS SDK (NOT
//     through DittoFS) and, for each one:
//       - calls HEAD; asserts resp.Metadata["content-hash"] is present
//         and prefixed "blake3:";
//       - GETs the body; computes BLAKE3 ourselves; asserts the header
//         value equals "blake3:" + hex(BLAKE3(body)).
//
// This proves the wire format matches what `aws s3api head-object` would
// print — operators can audit DittoFS-written CAS objects with stock
// AWS tooling, no DittoFS-specific verifier required.
//
// Run command (requires sudo + Docker for Localstack):
//
//	cd test/e2e && sudo ./run-e2e.sh --s3 --test TestExternalVerifier_ContentHashHeader
package e2e

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"lukechampine.com/blake3"
)

// extVerifierPayloadSize is large enough to guarantee at least one
// chunk emit (FastCDC min is 1 MiB) without slowing down the test.
const extVerifierPayloadSize = 4 * 1024 * 1024 // 4 MiB

func TestExternalVerifier_ContentHashHeader(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping BSCAS-06 external-verifier sanity test in short mode")
	}

	if !framework.CheckLocalstackAvailable(t) {
		t.Skip("Skipping: Localstack (S3) not available — run via run-e2e.sh --s3")
	}

	lsHelper := framework.NewLocalstackHelper(t)
	require.NotNil(t, lsHelper, "Localstack helper must be available")

	// ---- Server + share + S3 backend setup ----
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)

	cli := helpers.LoginAsAdmin(t, sp.APIURL())

	shareName := "/export-bscas-06"

	setup := helpers.SetupStoreMatrix(t, cli, shareName, helpers.MatrixSetupConfig{
		MetadataType: "memory",
		LocalType:    "fs",
		RemoteType:   "s3",
	}, nil, lsHelper)
	require.NotNil(t, setup, "store-matrix setup")

	require.NotEmpty(t, lsHelper.Buckets, "Localstack helper should track at least one bucket")
	bucket := lsHelper.Buckets[len(lsHelper.Buckets)-1]
	t.Logf("CAS bucket = %q", bucket)

	// ---- Mount NFS ----
	nfsPort := helpers.FindFreePort(t)
	_, err := cli.EnableAdapter("nfs", helpers.WithAdapterPort(nfsPort))
	require.NoError(t, err, "Should enable NFS adapter")
	t.Cleanup(func() { _, _ = cli.DisableAdapter("nfs") })

	require.NoError(t, helpers.WaitForAdapterStatus(t, cli, "nfs", true, 5*time.Second),
		"NFS adapter should become enabled")
	framework.WaitForServer(t, nfsPort, 10*time.Second)

	mount := mountNFSExport(t, nfsPort, shareName)
	t.Cleanup(mount.Cleanup)

	// ---- Write a 4 MiB file through NFS ----
	payload := deterministicPayload(t, extVerifierPayloadSize, 42)
	filePath := mount.FilePath("hello.bin")
	framework.WriteFile(t, filePath, payload)
	t.Logf("wrote %d bytes via NFS to %s", len(payload), filePath)

	drainUploads(t, cli)

	// ---- Direct S3 verification (BSCAS-06) ----
	keys := helpers.ListCASKeys(t, lsHelper, bucket)
	require.NotEmpty(t, keys, "expected at least one cas/.../ object after write+drain")
	t.Logf("listed %d cas/ objects via direct S3 SDK", len(keys))

	for _, key := range keys {
		t.Run("verify_"+keyShortForm(key), func(t *testing.T) {
			// 1. HEAD: assert the metadata header is present and well-formed.
			meta, contentLen := helpers.HeadCASObject(t, lsHelper, bucket, key)
			t.Logf("HEAD %s: ContentLength=%d Metadata=%v", key, contentLen, meta)

			got, ok := meta["content-hash"]
			require.Truef(t, ok,
				"BSCAS-06 VIOLATION: cas/ key %q is missing x-amz-meta-content-hash; "+
					"present metadata keys: %v", key, metadataKeys(meta))
			require.Truef(t, strings.HasPrefix(got, "blake3:"),
				"BSCAS-06 VIOLATION: cas/ key %q metadata value %q lacks blake3: prefix",
				key, got)

			// 2. GET: fetch the body, compute BLAKE3 ourselves,
			// confirm the header matches.
			body := helpers.GetCASObject(t, lsHelper, bucket, key)
			require.NotEmpty(t, body, "cas/ object %q has empty body", key)

			sum := blake3.Sum256(body)
			want := "blake3:" + hex.EncodeToString(sum[:])
			assert.Equalf(t, want, got,
				"BSCAS-06 VIOLATION: x-amz-meta-content-hash mismatch for key %q. "+
					"want=%s (computed BLAKE3 of body) got=%s (header value). "+
					"This means the syncer wrote a header that does not match the body.",
				key, want, got)
		})
	}

	t.Logf("BSCAS-06: verified x-amz-meta-content-hash on %d cas/ objects", len(keys))
}

// metadataKeys returns the sorted key list of a metadata map for
// debug logging. Used only inside failure messages.
func metadataKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// keyShortForm returns the trailing 8 hex chars of a cas/ key for
// readable subtest names. cas/ab/cd/abcd...ef → "...abcd...ef" → "...cdef0123".
func keyShortForm(key string) string {
	if len(key) < 8 {
		return key
	}
	return key[len(key)-8:]
}

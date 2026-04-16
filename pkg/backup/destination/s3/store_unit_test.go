package s3

import (
	"context"
	"errors"
	"testing"

	smithy "github.com/aws/smithy-go"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/backup/destination"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestParseConfig_MissingBucket asserts that a repo config without a
// bucket is rejected with ErrIncompatibleConfig — operator error, not
// a runtime failure.
func TestParseConfig_MissingBucket(t *testing.T) {
	repo := &models.BackupRepo{ID: "r", Kind: models.BackupRepoKindS3}
	require.NoError(t, repo.SetConfig(map[string]any{"region": "eu-west-1"}))
	_, err := parseConfig(repo)
	require.ErrorIs(t, err, destination.ErrIncompatibleConfig)
}

// TestParseConfig_AllFields round-trips every D-12 field name. If any of
// these assertions ever fails, the driver silently drops operator config.
func TestParseConfig_AllFields(t *testing.T) {
	repo := &models.BackupRepo{ID: "r", Kind: models.BackupRepoKindS3}
	require.NoError(t, repo.SetConfig(map[string]any{
		"bucket":           "b",
		"region":           "eu-west-1",
		"endpoint":         "http://localhost:4566",
		"access_key":       "AK",
		"secret_key":       "SK",
		"prefix":           "metadata/prod/",
		"force_path_style": true,
		"max_retries":      7,
		"grace_window":     "48h",
	}))
	cfg, err := parseConfig(repo)
	require.NoError(t, err)
	require.Equal(t, "b", cfg.Bucket)
	require.Equal(t, "eu-west-1", cfg.Region)
	require.Equal(t, "http://localhost:4566", cfg.Endpoint)
	require.Equal(t, "AK", cfg.AccessKey)
	require.Equal(t, "SK", cfg.SecretKey)
	require.Equal(t, "metadata/prod/", cfg.Prefix)
	require.True(t, cfg.ForcePathStyle)
	require.Equal(t, 7, cfg.MaxRetries)
	require.Equal(t, "48h", cfg.GraceWindow)
}

// TestParseConfig_DefaultMaxRetries ensures parseConfig fills in a
// reasonable default when the operator omits max_retries.
func TestParseConfig_DefaultMaxRetries(t *testing.T) {
	repo := &models.BackupRepo{ID: "r", Kind: models.BackupRepoKindS3}
	require.NoError(t, repo.SetConfig(map[string]any{"bucket": "b"}))
	cfg, err := parseConfig(repo)
	require.NoError(t, err)
	require.Equal(t, defaultMaxRetries, cfg.MaxRetries)
}

// TestParseGrace covers the three parseGrace paths: default, valid, and
// malformed.
func TestParseGrace(t *testing.T) {
	d, err := parseGrace("")
	require.NoError(t, err)
	require.Equal(t, defaultGraceWindow, d)

	d, err = parseGrace("1h30m")
	require.NoError(t, err)
	require.NotZero(t, d)

	_, err = parseGrace("not-a-duration")
	require.ErrorIs(t, err, destination.ErrIncompatibleConfig)
}

// TestNormalizePrefix verifies leading-slash strip + trailing-slash add.
func TestNormalizePrefix(t *testing.T) {
	cases := map[string]string{
		"":          "",
		"foo":       "foo/",
		"foo/":      "foo/",
		"/foo":      "foo/",
		"/foo/bar":  "foo/bar/",
		"/foo/bar/": "foo/bar/",
	}
	for in, want := range cases {
		require.Equal(t, want, normalizePrefix(in), "input=%q", in)
	}
}

// TestNormalizeEndpoint ensures https:// is prepended only when a scheme
// is absent and that existing schemes are preserved.
func TestNormalizeEndpoint(t *testing.T) {
	cases := map[string]string{
		"":                      "",
		"s3.amazonaws.com":      "https://s3.amazonaws.com",
		"localhost:4566":        "https://localhost:4566",
		"http://localhost:4566": "http://localhost:4566",
		"https://example.com":   "https://example.com",
	}
	for in, want := range cases {
		require.Equal(t, want, normalizeEndpoint(in), "input=%q", in)
	}
}

// fakeLister implements blockStoreLister with a canned result set.
type fakeLister struct{ rows []*models.BlockStoreConfig }

func (f *fakeLister) ListBlockStores(ctx context.Context, kind models.BlockStoreKind) ([]*models.BlockStoreConfig, error) {
	return f.rows, nil
}

// remoteS3Real builds a BlockStoreConfig using the SAME JSON keys that
// runtime/shares/service.go:1011-1013 uses to persist S3 block-store
// configs: "bucket" and "prefix". Using the real key shape guarantees the
// collision check exercised here is reading what production actually
// persists — the exact guard against PITFALL #8. Any alternate name
// (keyPrefix, key_prefix, etc.) would silently approve every overlap.
func remoteS3Real(name, bucket, prefix string) *models.BlockStoreConfig {
	bs := &models.BlockStoreConfig{
		Name: name,
		Kind: models.BlockStoreKindRemote,
		Type: "s3",
	}
	_ = bs.SetConfig(map[string]any{"bucket": bucket, "prefix": prefix})
	return bs
}

// TestCheckPrefixCollision_TableDriven covers every D-13 overlap case plus
// a regression for the empty-backup-prefix catastrophic overlap.
func TestCheckPrefixCollision_TableDriven(t *testing.T) {
	cases := []struct {
		name         string
		bucket       string
		backupPrefix string
		blocks       []*models.BlockStoreConfig
		wantErr      bool
	}{
		{
			"different-bucket-ok",
			"A", "metadata/",
			[]*models.BlockStoreConfig{remoteS3Real("x", "B", "blocks/")},
			false,
		},
		{
			"different-prefix-ok",
			"A", "metadata/",
			[]*models.BlockStoreConfig{remoteS3Real("x", "A", "blocks/")},
			false,
		},
		{
			"block-root-backup-subdir-collide",
			"A", "metadata/",
			[]*models.BlockStoreConfig{remoteS3Real("x", "A", "")},
			true,
		},
		{
			"backup-is-prefix-of-block",
			"A", "data/",
			[]*models.BlockStoreConfig{remoteS3Real("x", "A", "data/meta/")},
			true,
		},
		{
			"block-is-prefix-of-backup",
			"A", "data/meta/",
			[]*models.BlockStoreConfig{remoteS3Real("x", "A", "data/")},
			true,
		},
		{
			"equal-prefix-collide",
			"A", "data/",
			[]*models.BlockStoreConfig{remoteS3Real("x", "A", "data/")},
			true,
		},
		{
			"non-s3-ignored",
			"A", "data/",
			[]*models.BlockStoreConfig{{
				Name: "x",
				Kind: models.BlockStoreKindRemote,
				Type: "minio-ish-nonstandard",
			}},
			false,
		},
		// REGRESSION: empty backup prefix (entire-bucket backup) with ANY
		// registered s3 block store on the same bucket is a collision —
		// the catastrophic D-13 PITFALL #8 case.
		{
			"empty-backup-prefix-same-bucket",
			"A", "",
			[]*models.BlockStoreConfig{remoteS3Real("x", "A", "blocks/")},
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeLister{rows: tc.blocks}
			err := checkPrefixCollision(context.Background(), f, tc.bucket, normalizePrefix(tc.backupPrefix))
			if tc.wantErr {
				require.Error(t, err)
				require.ErrorIs(t, err, destination.ErrIncompatibleConfig)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestClassifyS3Error_MapsCodes asserts the three documented D-07
// mappings using real smithy.GenericAPIError values — not ad-hoc fakes.
// This is the load-bearing regression test: a drift in SDK error codes or
// in the classifier's switch body will surface here rather than deep in
// orchestrator retry logic.
func TestClassifyS3Error_MapsCodes(t *testing.T) {
	// Nil input returns nil.
	require.NoError(t, classifyS3Error(nil))

	var throttle smithy.APIError = &smithy.GenericAPIError{Code: "SlowDown", Message: "slow down"}
	require.ErrorIs(t, classifyS3Error(throttle), destination.ErrDestinationThrottled)

	var denied smithy.APIError = &smithy.GenericAPIError{Code: "AccessDenied", Message: "denied"}
	require.ErrorIs(t, classifyS3Error(denied), destination.ErrPermissionDenied)

	var notFound smithy.APIError = &smithy.GenericAPIError{Code: "NoSuchBucket", Message: "no such bucket"}
	require.ErrorIs(t, classifyS3Error(notFound), destination.ErrIncompatibleConfig)

	// Plain (non-smithy, non-network) error passes through unchanged.
	plain := errors.New("unknown")
	require.Equal(t, plain, classifyS3Error(plain))
}

// TestClassifyS3Error_Throttling covers other throttling codes the
// mapping recognises, ensuring none regress to a pass-through.
func TestClassifyS3Error_Throttling(t *testing.T) {
	codes := []string{"SlowDown", "RequestLimitExceeded", "ThrottlingException"}
	for _, c := range codes {
		t.Run(c, func(t *testing.T) {
			var e smithy.APIError = &smithy.GenericAPIError{Code: c}
			require.ErrorIs(t, classifyS3Error(e), destination.ErrDestinationThrottled)
		})
	}
}

// TestIsNotFound_APIErrorCodes covers the four code-based not-found paths
// exercised by the driver's Head*-then-classify flow.
func TestIsNotFound_APIErrorCodes(t *testing.T) {
	for _, code := range []string{"NoSuchKey", "NoSuchBucket", "NotFound"} {
		t.Run(code, func(t *testing.T) {
			var e error = &smithy.GenericAPIError{Code: code}
			require.True(t, isNotFound(e), "code=%q", code)
		})
	}
	require.False(t, isNotFound(nil))
	require.False(t, isNotFound(errors.New("some other error")))
}

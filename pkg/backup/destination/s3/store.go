// Package s3 provides an S3-backed destination.Destination implementation.
//
// Layout (per Phase 3 CONTEXT.md D-01/D-02):
//
//	<bucket>/<prefix><id>/payload.bin     (uploaded via manager.Uploader — multipart)
//	<bucket>/<prefix><id>/manifest.yaml   (manifest-last — single PutObject, publish marker)
//
// Crash between payload upload and manifest put leaves payload.bin without
// manifest.yaml; List excludes it and the orphan sweep on New() deletes it
// once older than grace_window (D-06).
package s3

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/marmos91/dittofs/pkg/backup/destination"
	"github.com/marmos91/dittofs/pkg/backup/manifest"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// Compile-time interface satisfaction check.
var _ destination.Destination = (*Store)(nil)

const (
	payloadName        = "payload.bin"
	manifestName       = "manifest.yaml"
	defaultGraceWindow = 24 * time.Hour
	defaultMaxRetries  = 5
	// multipartPartSize is the manager.Uploader part size. 5 MiB matches the
	// SDK default; operators can override via future Config additions if
	// benchmarks justify (D-02 discretion).
	multipartPartSize = 5 * 1024 * 1024
	// multipartParallel bounds in-flight part uploads per PutBackup.
	multipartParallel = 5
	// orphanSweepTimeout bounds the async orphan sweep on New so a slow or
	// misbehaving bucket does not leak a goroutine indefinitely.
	orphanSweepTimeout = 2 * time.Minute
)

// Config mirrors the field names and JSON keys of
// pkg/blockstore/remote/s3.Config so operators can copy-paste between
// block-store and backup-repo configs (D-12). The field names here are the
// exact keys runtime/shares/service.go reads — see PITFALL #8 / D-13.
type Config struct {
	Bucket         string `json:"bucket"`
	Region         string `json:"region,omitempty"`
	Endpoint       string `json:"endpoint,omitempty"`
	AccessKey      string `json:"access_key,omitempty"`
	SecretKey      string `json:"secret_key,omitempty"`
	Prefix         string `json:"prefix,omitempty"`
	ForcePathStyle bool   `json:"force_path_style,omitempty"`
	MaxRetries     int    `json:"max_retries,omitempty"`
	GraceWindow    string `json:"grace_window,omitempty"` // e.g. "24h" (D-06)
}

// Store is the S3-backed destination. One Store instance per BackupRepo.
type Store struct {
	client        *s3.Client
	uploader      *manager.Uploader
	bucket        string
	prefix        string // always ends with "/" if non-empty
	graceWindow   time.Duration
	encryptionOn  bool
	encryptionRef string
	lister        blockStoreLister // nil => collision check skipped (tests)
	now           func() time.Time // testable clock
}

// Option customises Store construction.
type Option func(*Store)

// WithBlockStoreLister injects a narrow lister used by ValidateConfig to
// enforce the bucket/prefix collision hard-reject (D-13). Callers that do
// not have access to a control-plane store (e.g. unit tests) may omit this;
// ValidateConfig then skips the collision check.
func WithBlockStoreLister(l blockStoreLister) Option { return func(s *Store) { s.lister = l } }

// WithClock injects a deterministic clock. Tests use this to fast-forward
// past grace_window without sleeping.
func WithClock(fn func() time.Time) Option { return func(s *Store) { s.now = fn } }

// New constructs a Store for repo. The orphan sweep runs asynchronously in
// the background so a slow bucket cannot block server startup.
//
// Callers obtain the factory via destination.Lookup(models.BackupRepoKindS3)
// or register the factory at process startup via destination.Register.
func New(ctx context.Context, repo *models.BackupRepo, opts ...Option) (destination.Destination, error) {
	if repo == nil {
		return nil, fmt.Errorf("%w: nil backup repo", destination.ErrIncompatibleConfig)
	}
	cfg, err := parseConfig(repo)
	if err != nil {
		return nil, err
	}
	client, err := buildS3Client(ctx, cfg)
	if err != nil {
		return nil, err
	}
	uploader := manager.NewUploader(client, func(u *manager.Uploader) {
		u.PartSize = multipartPartSize
		u.Concurrency = multipartParallel
	})
	gw, err := parseGrace(cfg.GraceWindow)
	if err != nil {
		return nil, err
	}
	s := &Store{
		client:        client,
		uploader:      uploader,
		bucket:        cfg.Bucket,
		prefix:        normalizePrefix(cfg.Prefix),
		graceWindow:   gw,
		encryptionOn:  repo.EncryptionEnabled,
		encryptionRef: repo.EncryptionKeyRef,
		now:           time.Now,
	}
	for _, o := range opts {
		o(s)
	}
	// Best-effort orphan sweep on startup (D-06). Errors are logged only.
	go func() {
		sweepCtx, cancel := context.WithTimeout(context.Background(), orphanSweepTimeout)
		defer cancel()
		if err := s.sweepOrphans(sweepCtx); err != nil {
			slog.Warn("destination/s3: orphan sweep error",
				"bucket", s.bucket, "prefix", s.prefix, "err", err)
		}
	}()
	return s, nil
}

// parseConfig decodes repo.Config into a typed Config struct using the D-12
// JSON field names. Missing bucket is a permanent config error.
func parseConfig(repo *models.BackupRepo) (Config, error) {
	raw, err := repo.GetConfig()
	if err != nil {
		return Config{}, fmt.Errorf("%w: parse repo config: %v", destination.ErrIncompatibleConfig, err)
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return Config{}, fmt.Errorf("%w: remarshal config: %v", destination.ErrIncompatibleConfig, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("%w: unmarshal s3 config: %v", destination.ErrIncompatibleConfig, err)
	}
	if cfg.Bucket == "" {
		return Config{}, fmt.Errorf("%w: bucket is required", destination.ErrIncompatibleConfig)
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = defaultMaxRetries
	}
	return cfg, nil
}

// parseGrace parses a duration string into a time.Duration. Empty selects
// the D-06 default (24h); malformed returns ErrIncompatibleConfig.
func parseGrace(s string) (time.Duration, error) {
	if s == "" {
		return defaultGraceWindow, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%w: grace_window %q: %v", destination.ErrIncompatibleConfig, s, err)
	}
	return d, nil
}

// normalizePrefix trims a leading "/" and ensures a trailing "/" on a
// non-empty prefix. Empty stays empty (whole-bucket backup, permitted only
// when no block store shares the bucket — see D-13).
func normalizePrefix(p string) string {
	p = strings.TrimLeft(p, "/")
	if p != "" && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// buildS3Client constructs an s3.Client following the same shape as
// pkg/blockstore/remote/s3.NewFromConfig. Field-name substitutions (D-12):
//   - Prefix replaces KeyPrefix (applied outside this function by caller)
//
// Duplicating (rather than factoring into internal/awsclient/) matches the
// Phase 2 02-PATTERNS precedent "duplicate over premature refactor" — two
// users is not yet three.
func buildS3Client(ctx context.Context, cfg Config) (*s3.Client, error) {
	var opts []func(*awsconfig.LoadOptions) error

	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}

	if cfg.AccessKey != "" && cfg.SecretKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		))
	}

	// HTTP transport tuning matches pkg/blockstore/remote/s3/store.go:99-125.
	httpTransport := &http.Transport{
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 50,
		MaxConnsPerHost:     50,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   false,
		TLSNextProto:        make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			NextProtos: []string{"http/1.1"},
		},
		WriteBufferSize:       256 * 1024,
		ReadBufferSize:        256 * 1024,
		ExpectContinueTimeout: 0,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	httpClient := &http.Client{Transport: httpTransport, Timeout: 0}
	opts = append(opts, awsconfig.WithHTTPClient(httpClient))

	maxAttempts := cfg.MaxRetries
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxRetries
	}
	opts = append(opts, awsconfig.WithRetryer(func() aws.Retryer {
		return retry.NewStandard(func(o *retry.StandardOptions) {
			o.MaxAttempts = maxAttempts
			o.MaxBackoff = 30 * time.Second
			o.Retryables = append(o.Retryables, retry.RetryableHTTPStatusCode{
				Codes: map[int]struct{}{429: {}},
			})
		})
	}))

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("%w: load AWS config: %v", destination.ErrIncompatibleConfig, err)
	}

	var s3Opts []func(*s3.Options)
	if cfg.Endpoint != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(normalizeEndpoint(cfg.Endpoint))
		})
	}
	if cfg.ForcePathStyle {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.UsePathStyle = true
		})
	}

	return s3.NewFromConfig(awsCfg, s3Opts...), nil
}

// normalizeEndpoint mirrors pkg/blockstore/remote/s3.normalizeEndpoint:
// prepend https:// when the endpoint lacks a scheme.
func normalizeEndpoint(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	if i := strings.Index(endpoint, "://"); i > 0 {
		scheme := endpoint[:i]
		if isValidScheme(scheme) {
			return endpoint
		}
	}
	return "https://" + endpoint
}

// isValidScheme reports whether s is a valid URI scheme per RFC 3986.
func isValidScheme(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i, c := range s {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z':
			// always valid
		case '0' <= c && c <= '9', c == '+', c == '-', c == '.':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// payloadKey returns the full S3 key for <id>/payload.bin.
func (s *Store) payloadKey(id string) string { return s.prefix + id + "/" + payloadName }

// manifestKey returns the full S3 key for <id>/manifest.yaml.
func (s *Store) manifestKey(id string) string { return s.prefix + id + "/" + manifestName }

// writeNopCloser adapts an io.Writer into an io.WriteCloser whose Close is
// a no-op. Used to uniformly handle the encrypt-on/encrypt-off cases.
type writeNopCloser struct{ io.Writer }

func (writeNopCloser) Close() error { return nil }

// PutBackup publishes a new backup using the D-02 two-phase commit:
// payload first (streaming multipart), manifest last (single PutObject).
//
// Uses io.Pipe so manager.Uploader consumes the pipe reader while the
// encrypt+hash pipeline writes into the pipe writer. The producer goroutine
// drains the caller's payload reader; the consumer is the SDK uploader.
//
// Per Phase 3 D-11, this is the single enforcement point for manifest-last
// + SHA-256 tee + AES-GCM envelope. We intentionally do NOT invoke the
// manifest's full validator here — it requires SHA256, which is only
// known after the payload stream drains. Pre-write required-field checks
// run inline instead.
func (s *Store) PutBackup(ctx context.Context, m *manifest.Manifest, payload io.Reader) error {
	if m == nil {
		return fmt.Errorf("%w: manifest is nil", destination.ErrIncompatibleConfig)
	}
	if m.BackupID == "" {
		return fmt.Errorf("%w: manifest.BackupID required", destination.ErrIncompatibleConfig)
	}
	if m.StoreID == "" || m.StoreKind == "" || m.PayloadIDSet == nil {
		return fmt.Errorf("%w: manifest missing required pre-write fields (StoreID/StoreKind/PayloadIDSet)", destination.ErrIncompatibleConfig)
	}
	id := m.BackupID

	// Reject duplicate backup id — manifest key presence is the publish
	// marker, so its existence means a completed backup already occupies
	// this id (D-02).
	exists, err := s.objectExists(ctx, s.manifestKey(id))
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("%w: %s", destination.ErrDuplicateBackupID, id)
	}

	// Resolve the encryption key up front (BEFORE spawning the producer
	// goroutine) so any config/resolution failure short-circuits before we
	// open the pipe to the uploader. The raw key is zeroed as soon as
	// cipher.NewGCM has consumed it inside the goroutine (D-09).
	var key []byte
	if m.Encryption.Enabled {
		k, kerr := destination.ResolveKey(m.Encryption.KeyRef)
		if kerr != nil {
			return kerr
		}
		key = k
	}

	pr, pw := io.Pipe()

	// Producer goroutine: build the encrypt+hash pipeline INSIDE the
	// goroutine so any bytes the encrypt writer emits eagerly (the D-05
	// envelope header is 9 bytes written during NewEncryptWriter) go into
	// the pipe concurrently with the uploader starting to read. Building
	// it on the calling goroutine deadlocks on the unbuffered io.Pipe.
	//
	// Error handling: all failures close the pipe with the error so the
	// uploader unblocks with the same cause, then signal via errCh.
	errCh := make(chan error, 1)
	// Capture Sum / Size after the goroutine completes — the hashTeeWriter
	// is constructed inside, so we surface its final state via an
	// outer-scope variable set just before goroutine exit.
	var sha string
	var size int64
	go func() {
		var gerr error
		defer func() {
			if gerr != nil {
				_ = pw.CloseWithError(gerr)
			} else {
				_ = pw.Close()
			}
			errCh <- gerr
		}()

		tee := newHashTeeWriter(pw)
		var writer io.WriteCloser
		if m.Encryption.Enabled {
			enc, eerr := destination.NewEncryptWriter(tee, key, 0)
			// Zero the key bytes regardless of success — cipher.NewGCM
			// (called by NewEncryptWriter) has already consumed them.
			for i := range key {
				key[i] = 0
			}
			if eerr != nil {
				gerr = eerr
				return
			}
			writer = enc
		} else {
			writer = writeNopCloser{Writer: tee}
		}
		if _, err := io.Copy(writer, payload); err != nil {
			gerr = fmt.Errorf("stream payload: %w", err)
			return
		}
		if err := writer.Close(); err != nil {
			gerr = fmt.Errorf("close encrypt writer: %w", err)
			return
		}
		sha = tee.Sum()
		size = tee.Size()
	}()

	// 1. Multipart upload of payload.bin. Manager aborts the MPU on error.
	_, upErr := s.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.payloadKey(id)),
		Body:   pr,
	})
	// Always drain the producer channel to avoid goroutine leak.
	producerErr := <-errCh
	if upErr != nil {
		return classifyS3Error(fmt.Errorf("upload payload: %w", upErr))
	}
	if producerErr != nil {
		return classifyS3Error(producerErr)
	}

	// 2. Fill in SHA-256 + size from the hash-tee (the goroutine captured
	//    them into sha / size just before closing the pipe successfully),
	//    then PutObject the manifest. This is the publish marker
	//    (manifest-last, D-02).
	m.SHA256 = sha
	m.SizeBytes = size
	data, merr := m.Marshal()
	if merr != nil {
		return fmt.Errorf("%w: marshal manifest: %v", destination.ErrDestinationUnavailable, merr)
	}
	if _, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(s.manifestKey(id)),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/yaml"),
	}); err != nil {
		return classifyS3Error(fmt.Errorf("put manifest: %w", err))
	}
	return nil
}

// GetManifestOnly implements destination.Destination. Single GetObject
// call against the manifest key — no payload bandwidth spent. See Phase 5
// CONTEXT.md D-12: used by the restore pre-flight (validate store_id /
// store_kind before committing to a payload download) and the block-GC
// hold provider (union PayloadIDSet across all retained manifests).
func (s *Store) GetManifestOnly(ctx context.Context, id string) (*manifest.Manifest, error) {
	mOut, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.manifestKey(id)),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", destination.ErrManifestMissing, id)
		}
		return nil, classifyS3Error(fmt.Errorf("get manifest: %w", err))
	}
	defer func() { _ = mOut.Body.Close() }()
	m, perr := manifest.ReadFrom(mOut.Body)
	if perr != nil {
		return nil, fmt.Errorf("%w: parse manifest: %v", destination.ErrDestinationUnavailable, perr)
	}
	return m, nil
}

// GetBackup streams the manifest + payload (post-decrypt if encrypted). The
// returned reader verifies SHA-256 while it streams and returns
// ErrSHA256Mismatch from Close if the computed digest differs.
//
// The manifest prologue delegates to GetManifestOnly so both call sites
// share the single "fetch + parse manifest" path (Phase 5 D-12 dedup).
func (s *Store) GetBackup(ctx context.Context, id string) (*manifest.Manifest, io.ReadCloser, error) {
	m, err := s.GetManifestOnly(ctx, id)
	if err != nil {
		// GetManifestOnly already wraps ErrManifestMissing and
		// ErrDestinationUnavailable correctly — preserve error shape.
		return nil, nil, err
	}

	pOut, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.payloadKey(id)),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, nil, fmt.Errorf("%w: %s/payload.bin", destination.ErrIncompleteBackup, id)
		}
		return nil, nil, classifyS3Error(fmt.Errorf("get payload: %w", err))
	}

	// Verify SHA-256 over the ciphertext (what's actually stored, D-04),
	// then optionally decrypt.
	vr := newVerifyReader(pOut.Body, m.SHA256)
	var reader io.Reader = vr

	if m.Encryption.Enabled {
		key, kerr := destination.ResolveKey(m.Encryption.KeyRef)
		if kerr != nil {
			_ = pOut.Body.Close()
			return nil, nil, kerr
		}
		dec, derr := destination.NewDecryptReader(reader, key)
		for i := range key {
			key[i] = 0
		}
		if derr != nil {
			_ = pOut.Body.Close()
			return nil, nil, derr
		}
		reader = dec
	}

	return m, &verifyReadCloser{r: reader, vr: vr, body: pOut.Body}, nil
}

// List returns descriptors for every PUBLISHED backup (those with a
// manifest.yaml present), sorted lexicographically by ID.
func (s *Store) List(ctx context.Context) ([]destination.BackupDescriptor, error) {
	ids, err := s.listPublishedIDs(ctx)
	if err != nil {
		return nil, err
	}
	sort.Strings(ids)
	out := make([]destination.BackupDescriptor, 0, len(ids))
	for _, id := range ids {
		d, err := s.Stat(ctx, id)
		if err != nil {
			slog.Warn("destination/s3: skipping unreadable backup",
				"id", id, "bucket", s.bucket, "err", err)
			continue
		}
		if !d.HasManifest {
			continue
		}
		out = append(out, *d)
	}
	return out, nil
}

// listPublishedIDs returns every <id> whose manifest.yaml exists under the
// configured prefix. Paginates through ListObjectsV2.
func (s *Store) listPublishedIDs(ctx context.Context) ([]string, error) {
	return s.listIDsByFile(ctx, manifestName)
}

// listIDsByFile paginates ListObjectsV2 under s.prefix and returns every
// <id> whose direct child is exactly filename. Used by listPublishedIDs
// (filename=manifest.yaml) and listAllIDs (filename=payload.bin).
func (s *Store) listIDsByFile(ctx context.Context, filename string) ([]string, error) {
	seen := map[string]struct{}{}
	var token *string
	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(s.prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, classifyS3Error(fmt.Errorf("list: %w", err))
		}
		for _, obj := range out.Contents {
			rest := strings.TrimPrefix(aws.ToString(obj.Key), s.prefix)
			parts := strings.Split(rest, "/")
			if len(parts) == 2 && parts[1] == filename {
				seen[parts[0]] = struct{}{}
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		token = out.NextContinuationToken
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	return ids, nil
}

// Stat returns a descriptor for one backup without streaming the payload.
// Manifest-present is authoritative for HasManifest; if the manifest is
// readable we use its SHA-256 and CreatedAt.
func (s *Store) Stat(ctx context.Context, id string) (*destination.BackupDescriptor, error) {
	d := &destination.BackupDescriptor{ID: id}

	mHead, mHeadErr := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.manifestKey(id)),
	})
	if mHeadErr == nil {
		d.HasManifest = true
		// Best-effort: fetch manifest body for SHA + CreatedAt. Not
		// critical — a descriptor without SHA still round-trips List.
		if mOut, merr := s.client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(s.manifestKey(id)),
		}); merr == nil {
			if m, parseErr := manifest.ReadFrom(mOut.Body); parseErr == nil {
				d.CreatedAt = m.CreatedAt
				d.SHA256 = m.SHA256
				d.SizeBytes = m.SizeBytes
			} else if mHead.LastModified != nil {
				d.CreatedAt = *mHead.LastModified
			}
			_ = mOut.Body.Close()
		} else if mHead.LastModified != nil {
			d.CreatedAt = *mHead.LastModified
		}
	} else if !isNotFound(mHeadErr) {
		return nil, classifyS3Error(fmt.Errorf("head manifest: %w", mHeadErr))
	}

	// Payload presence fills in SizeBytes/CreatedAt when manifest was
	// absent or unreadable. Absence + no manifest = not a backup at all.
	pHead, pErr := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.payloadKey(id)),
	})
	if pErr == nil {
		if d.SizeBytes == 0 {
			d.SizeBytes = aws.ToInt64(pHead.ContentLength)
		}
		if d.CreatedAt.IsZero() && pHead.LastModified != nil {
			d.CreatedAt = *pHead.LastModified
		}
	} else if isNotFound(pErr) {
		if !d.HasManifest {
			return nil, fmt.Errorf("%w: %s", destination.ErrManifestMissing, id)
		}
	} else {
		return nil, classifyS3Error(fmt.Errorf("head payload: %w", pErr))
	}
	return d, nil
}

// Delete removes a published backup. Inverts publish order: manifest first
// (List excludes immediately), then payload. A crash mid-delete leaves an
// orphan payload — which the startup sweep cleans up (D-06).
func (s *Store) Delete(ctx context.Context, id string) error {
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.manifestKey(id)),
	}); err != nil && !isNotFound(err) {
		return classifyS3Error(fmt.Errorf("delete manifest: %w", err))
	}
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.payloadKey(id)),
	}); err != nil && !isNotFound(err) {
		return classifyS3Error(fmt.Errorf("delete payload: %w", err))
	}
	return nil
}

// ValidateConfig probes the destination at repo-create time. Checks:
//  1. HeadBucket — bucket exists and is reachable (D-12).
//  2. Prefix-collision against registered remote block stores (D-13).
//  3. Bucket lifecycle — warn (not error) if AbortIncompleteMultipartUpload
//     rule is missing (D-06). Operator-owned, so warning-only.
//  4. Encryption key reference validates (D-08) when encryption is on.
func (s *Store) ValidateConfig(ctx context.Context) error {
	if _, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(s.bucket),
	}); err != nil {
		if isNotFound(err) {
			return fmt.Errorf("%w: bucket %s not found", destination.ErrIncompatibleConfig, s.bucket)
		}
		return classifyS3Error(fmt.Errorf("head bucket: %w", err))
	}

	if s.lister != nil {
		if err := checkPrefixCollision(ctx, s.lister, s.bucket, s.prefix); err != nil {
			return err
		}
	}

	lc, err := s.client.GetBucketLifecycleConfiguration(ctx,
		&s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(s.bucket)})
	if err != nil || !hasAbortIncompleteMultipartRule(lc) {
		slog.Warn("destination/s3: bucket has no AbortIncompleteMultipartUpload lifecycle rule — stale multipart uploads can accumulate. Recommend adding one.",
			"bucket", s.bucket)
	}

	if s.encryptionOn {
		if err := destination.ValidateKeyRef(s.encryptionRef); err != nil {
			return err
		}
	}
	return nil
}

// hasAbortIncompleteMultipartRule returns true when the bucket lifecycle
// configuration contains any rule with an AbortIncompleteMultipartUpload
// action (D-06 warn-only check).
func hasAbortIncompleteMultipartRule(out *s3.GetBucketLifecycleConfigurationOutput) bool {
	if out == nil {
		return false
	}
	for _, r := range out.Rules {
		if r.AbortIncompleteMultipartUpload != nil {
			return true
		}
	}
	return false
}

// sweepOrphans (D-06 belt-and-suspenders layer 2) deletes:
//   - <id>/payload.bin without <id>/manifest.yaml, older than grace_window.
//   - Stale in-progress multipart uploads older than grace_window.
//
// Best-effort: errors are logged but do not propagate. Safe to run
// concurrently with PutBackup (only objects older than grace_window are
// touched).
func (s *Store) sweepOrphans(ctx context.Context) error {
	cutoff := s.now().Add(-s.graceWindow)

	// Layer A: orphan payloads.
	ids, err := s.listAllIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		hasManifest, _ := s.objectExists(ctx, s.manifestKey(id))
		if hasManifest {
			continue
		}
		h, hErr := s.client.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(s.payloadKey(id)),
		})
		if hErr != nil {
			continue
		}
		if h.LastModified != nil && h.LastModified.Before(cutoff) {
			size := aws.ToInt64(h.ContentLength)
			if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(s.bucket),
				Key:    aws.String(s.payloadKey(id)),
			}); err != nil {
				slog.Warn("destination/s3: orphan delete failed",
					"id", id, "bucket", s.bucket, "err", err)
				continue
			}
			slog.Warn("destination/s3: removed orphan payload",
				"id", id, "bucket", s.bucket, "size_bytes", size)
		}
	}

	// Layer B: stale multipart uploads.
	mpu, err := s.client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(s.prefix),
	})
	if err != nil {
		// Best-effort: log and return nil. The bucket lifecycle rule is
		// the belt; this is the suspenders — one failing is not fatal.
		slog.Warn("destination/s3: list multipart uploads failed",
			"bucket", s.bucket, "err", err)
		return nil
	}
	for _, u := range mpu.Uploads {
		if u.Initiated == nil || u.Initiated.After(cutoff) {
			continue
		}
		_, _ = s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket:   aws.String(s.bucket),
			Key:      u.Key,
			UploadId: u.UploadId,
		})
		slog.Warn("destination/s3: aborted stale multipart upload",
			"key", aws.ToString(u.Key),
			"upload_id", aws.ToString(u.UploadId),
			"age", s.now().Sub(*u.Initiated).String())
	}
	return nil
}

// listAllIDs returns every <id> directory under s.prefix that contains a
// payload.bin (regardless of whether a manifest.yaml exists).
func (s *Store) listAllIDs(ctx context.Context) ([]string, error) {
	return s.listIDsByFile(ctx, payloadName)
}

// objectExists reports whether key exists. isNotFound errors become false;
// other SDK errors bubble up classified.
func (s *Store) objectExists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, classifyS3Error(fmt.Errorf("head: %w", err))
}

// Close is a no-op — the s3.Client manages its own HTTP connection pool
// and is safe to leave to the garbage collector.
func (s *Store) Close() error { return nil }

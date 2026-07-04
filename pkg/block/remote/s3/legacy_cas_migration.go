package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/remote"
)

// Migration-only legacy standalone-CAS accessors (#1493 PR4). This file is
// the S3 backend's implementation of remote.LegacyCASStore and holds the only
// surviving hash-keyed CAS operations (Put/Get/GetRange/Has/Head/Delete/Walk +
// ReadBlockVerified). They are NOT part of the production RemoteStore surface —
// they exist solely to read and purge legacy per-chunk "cas/" objects during
// the one-shot cas→blocks startup migration. Delete this file (and the
// legacy_cas_verifier.go helpers) when the migration is retired.

var _ remote.LegacyCASStore = (*Store)(nil)

// casPrefix is the CAS object-key prefix walked by Walk. Mirrors the
// block.FormatCASKey output ("cas/{hh}/{hh}/{hex}").
const casPrefix = "cas/"

// WalkLegacyChunks implements remote.LegacyCASStore: a LIST of the cas/
// namespace. An empty namespace costs a single LIST page.
func (s *Store) WalkLegacyChunks(ctx context.Context, fn func(hash block.ContentHash, size int64) error) error {
	return s.Walk(ctx, func(hash block.ContentHash, meta block.Meta) error {
		return fn(hash, meta.Size)
	})
}

// ReadLegacyChunkVerified implements remote.LegacyCASStore. The hash is both
// the lookup key and the expected plaintext BLAKE3 (they coincide on the
// standalone layout).
func (s *Store) ReadLegacyChunkVerified(ctx context.Context, hash block.ContentHash) ([]byte, error) {
	return s.ReadBlockVerified(ctx, hash, hash)
}

// DeleteLegacyChunk implements remote.LegacyCASStore.
func (s *Store) DeleteLegacyChunk(ctx context.Context, hash block.ContentHash) error {
	return s.Delete(ctx, hash)
}

// hashKey returns the full S3 key for a CAS content hash.
func (s *Store) hashKey(hash block.ContentHash) string {
	return s.fullKey(block.FormatCASKey(hash))
}

// Put writes data under the CAS-shaped key derived from hash. The
// x-amz-meta-content-hash header is stamped atomically with the PUT
// the AWS SDK normalizes the metadata key to lowercase and
// prepends "x-amz-meta-" on the wire, so we pass the bare key
// "content-hash". The header value is the canonical "blake3:{hex}" form
// via ContentHash.CASKey().
func (s *Store) Put(ctx context.Context, hash block.ContentHash, data []byte) error {
	if err := s.checkClosed(); err != nil {
		return err
	}

	key := s.hashKey(hash)
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
		Metadata: map[string]string{
			"content-hash": hash.CASKey(),
		},
	})
	if err != nil {
		return fmt.Errorf("s3 put: %w", err)
	}

	return nil
}

// Get reads a complete object from S3 by content hash. Returns raw bytes
// WITHOUT BLAKE3 verification — the migration read path uses
// ReadBlockVerified.
func (s *Store) Get(ctx context.Context, hash block.ContentHash) ([]byte, error) {
	if err := s.checkClosed(); err != nil {
		return nil, err
	}

	key := s.hashKey(hash)
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFoundError(err) {
			return nil, block.ErrChunkNotFound
		}
		return nil, fmt.Errorf("s3 get: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	return readResponseBody(resp.Body, resp.ContentLength, maxBlockReadSize)
}

// ReadBlockVerified GETs the object at the CAS-derived key and verifies
// the body's BLAKE3 hash matches expected before returning bytes
//
// Two-stage verification (fail-closed twice)
//  1. Header pre-check: if the response carries x-amz-meta-content-hash
//     and it does not match expected, return ErrChunkContentMismatch
//     BEFORE reading any body bytes (saves a doomed body transfer).
//  2. Streaming recompute: every body byte feeds a BLAKE3 hasher; on
//     EOF the accumulated digest is compared to expected. Mismatch
//     returns ErrChunkContentMismatch and the buffer is discarded.
//
// Both hash arguments are intentional: hash derives the canonical CAS
// key, while expected is the body BLAKE3 the caller is asserting.
func (s *Store) ReadBlockVerified(ctx context.Context, hash block.ContentHash, expected block.ContentHash) ([]byte, error) {
	if err := s.checkClosed(); err != nil {
		return nil, err
	}

	key := s.hashKey(hash)
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFoundError(err) {
			return nil, block.ErrChunkNotFound
		}
		return nil, fmt.Errorf("s3 get: %w", err)
	}

	// header pre-check. AWS SDK lower-cases user metadata keys and
	// strips the x-amz-meta- prefix. Memory store mirror uses the same
	// "content-hash" key.
	if hdr, ok := resp.Metadata["content-hash"]; ok && hdr != expected.CASKey() {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: header %q != expected %q",
			block.ErrChunkContentMismatch, hdr, expected.CASKey())
	}

	// streaming recompute. The verifier owns Close on resp.Body —
	// it surfaces ErrChunkContentMismatch if the caller closes before EOF.
	reader := newVerifyingReader(resp.Body, expected)
	data, readErr := readAllVerified(reader, resp.ContentLength, maxBlockReadSize)
	closeErr := reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return data, nil
}

// GetRange reads a byte range from a CAS object using S3 range requests.
func (s *Store) GetRange(ctx context.Context, hash block.ContentHash, offset, length int64) ([]byte, error) {
	if err := s.checkClosed(); err != nil {
		return nil, err
	}

	// Validate bounds before issuing the range request so callers get a
	// stable sentinel instead of an opaque S3 protocol error from a
	// malformed Range header. A past-EOF offset cannot be detected here
	// without a HEAD, so S3 surfaces its native 416 for that case (the
	// conformance suite only requires "any error" for offset >= EOF).
	if offset < 0 {
		return nil, block.ErrInvalidOffset
	}
	if length <= 0 {
		return nil, block.ErrInvalidSize
	}

	// Guard against offset+length-1 overflowing int64 (which would emit a
	// negative, malformed Range header). offset >= 0 and length > 0 here.
	if length > math.MaxInt64-offset {
		return nil, block.ErrInvalidSize
	}

	key := s.hashKey(hash)
	rangeHeader := fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)

	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Range:  aws.String(rangeHeader),
	})
	if err != nil {
		if isNotFoundError(err) {
			return nil, block.ErrChunkNotFound
		}
		return nil, fmt.Errorf("s3 get range: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	return readResponseBody(resp.Body, resp.ContentLength, length)
}

// Has reports whether the CAS object addressed by hash exists in the
// bucket. Implemented via HEAD for cost and latency reasons (a Get with
// Range: bytes=0-0 would still transfer one byte body).
func (s *Store) Has(ctx context.Context, hash block.ContentHash) (bool, error) {
	if err := s.checkClosed(); err != nil {
		return false, err
	}
	key := s.hashKey(hash)
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFoundError(err) {
			return false, nil
		}
		return false, fmt.Errorf("s3 head: %w", err)
	}
	return true, nil
}

// Head returns block.Meta for the CAS object addressed by hash
// without transferring the body. Returns block.ErrChunkNotFound on
// missing keys (same convention as Get).
//
// The x-amz-meta-content-hash header is NOT echoed in the returned
// Meta — the lookup key (ContentHash) is the input, not output. The
// header is still consulted internally by ReadBlockVerified for
// defense-in-depth.
func (s *Store) Head(ctx context.Context, hash block.ContentHash) (block.Meta, error) {
	if err := s.checkClosed(); err != nil {
		return block.Meta{}, err
	}

	key := s.hashKey(hash)
	resp, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFoundError(err) {
			return block.Meta{}, block.ErrChunkNotFound
		}
		return block.Meta{}, fmt.Errorf("s3 head: %w", err)
	}

	out := block.Meta{}
	if resp.ContentLength != nil {
		out.Size = *resp.ContentLength
	}
	if resp.LastModified != nil {
		out.LastModified = *resp.LastModified
	}
	return out, nil
}

// Delete removes the CAS object addressed by hash. Delete is idempotent
// S3's DeleteObject succeeds with 204 even when the key is absent.
func (s *Store) Delete(ctx context.Context, hash block.ContentHash) error {
	if err := s.checkClosed(); err != nil {
		return err
	}

	key := s.hashKey(hash)
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("s3 delete: %w", err)
	}

	return nil
}

// Walk enumerates every CAS object in the store. Iterates S3 listing
// pages under the cas/ prefix, parses each key via block.ParseCASKey
// (skipping non-CAS keys), and dispatches the callback with the parsed
// ContentHash and the per-object block.Meta. Honors
// block.ErrStopWalk for clean early exit; any other callback error
// halts and is wrapped as "walk halted at %s: %w".
// Context cancellation aborts immediately.
func (s *Store) Walk(ctx context.Context, fn func(hash block.ContentHash, meta block.Meta) error) error {
	if err := s.checkClosed(); err != nil {
		return err
	}

	fullPrefix := s.fullKey(casPrefix)

	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(fullPrefix),
	})

	for paginator.HasMorePages() {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("s3 walk: %w", err)
		}
		for _, obj := range page.Contents {
			if err := ctx.Err(); err != nil {
				return err
			}
			rawKey := ""
			if obj.Key != nil {
				rawKey = *obj.Key
			}
			// Strip the configured keyPrefix so ParseCASKey sees the
			// canonical "cas/..." shape.
			parseKey := rawKey
			if s.keyPrefix != "" && strings.HasPrefix(parseKey, s.keyPrefix) {
				parseKey = parseKey[len(s.keyPrefix):]
			}
			hash, perr := block.ParseCASKey(parseKey)
			if perr != nil {
				continue // non-CAS keys (legacy / sentinel) are skipped
			}
			meta := block.Meta{}
			if obj.Size != nil {
				meta.Size = *obj.Size
			}
			if obj.LastModified != nil {
				meta.LastModified = *obj.LastModified
			}
			if cberr := fn(hash, meta); cberr != nil {
				if errors.Is(cberr, block.ErrStopWalk) {
					return nil
				}
				return fmt.Errorf("walk halted at %s: %w", hash, cberr)
			}
		}
	}

	return nil
}

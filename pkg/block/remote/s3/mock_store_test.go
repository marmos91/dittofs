package s3

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/health"
)

// mockS3 is an in-process, deterministic S3 wire emulator sufficient to
// exercise the Store wire path (Put/Get/GetRange/Head/Has/Delete/Walk)
// without a Localstack/MinIO container. It implements only the subset of
// the S3 REST API the Store uses and is intentionally minimal — it is a
// test fixture, not a general S3 implementation.
//
// It uses path-style addressing (/{bucket}/{key}); the test client is
// constructed with UsePathStyle=true to match.
type mockS3 struct {
	mu      sync.Mutex
	objects map[string]mockObject // key -> object (key excludes the bucket segment)
	bucket  string

	// listPageSize, when >0, caps ListObjectsV2 results per page so the
	// paginator multi-page path is exercised deterministically.
	listPageSize int

	// failNext, when set, causes the next matching request to return the
	// given HTTP status instead of servicing it. It is consumed (reset to
	// 0) after firing once so retries can succeed. method is matched
	// case-insensitively; an empty method matches any.
	failNextStatus int
	failNextMethod string

	// omitContentLength, when true, suppresses the Content-Length header on
	// GET responses (chunked transfer) so the Store's no-content-length
	// readResponseBody fallback is exercised.
	omitContentLength bool
}

type mockObject struct {
	data         []byte
	metadata     map[string]string
	lastModified time.Time
}

func newMockS3(bucket string) *mockS3 {
	return &mockS3{
		objects: make(map[string]mockObject),
		bucket:  bucket,
	}
}

// listObjectsV2Result mirrors the XML the AWS SDK unmarshals for a
// ListObjectsV2 response. Only the fields the Store reads are populated.
type listObjectsV2Result struct {
	XMLName               xml.Name       `xml:"ListBucketResult"`
	IsTruncated           bool           `xml:"IsTruncated"`
	NextContinuationToken string         `xml:"NextContinuationToken,omitempty"`
	Contents              []listObjEntry `xml:"Contents"`
}

type listObjEntry struct {
	Key          string    `xml:"Key"`
	Size         int64     `xml:"Size"`
	LastModified time.Time `xml:"LastModified"`
}

func (m *mockS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	if m.failNextStatus != 0 &&
		(m.failNextMethod == "" || strings.EqualFold(m.failNextMethod, r.Method)) {
		status := m.failNextStatus
		m.failNextStatus = 0
		m.failNextMethod = ""
		m.mu.Unlock()
		http.Error(w, "<Error><Code>InternalError</Code></Error>", status)
		return
	}
	m.mu.Unlock()

	// Path-style: /{bucket}/{key...}. The leading slash + bucket are
	// stripped to recover the object key.
	path := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.SplitN(path, "/", 2)
	key := ""
	if len(parts) == 2 {
		key = parts[1]
	}

	// ListObjectsV2 is a GET on the bucket root with ?list-type=2.
	if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
		m.handleList(w, r)
		return
	}

	switch r.Method {
	case http.MethodPut:
		m.handlePut(w, r, key)
	case http.MethodGet:
		m.handleGet(w, r, key)
	case http.MethodHead:
		// HeadBucket is a HEAD on the bucket root (empty key); the Store
		// uses it for HealthCheck. Always reachable in the mock.
		if key == "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		m.handleHead(w, key)
	case http.MethodDelete:
		m.handleDelete(w, key)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (m *mockS3) handlePut(w http.ResponseWriter, r *http.Request, key string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	meta := make(map[string]string)
	for h, vals := range r.Header {
		const prefix = "X-Amz-Meta-"
		if strings.HasPrefix(h, prefix) && len(vals) > 0 {
			meta[strings.ToLower(strings.TrimPrefix(h, prefix))] = vals[0]
		}
	}
	m.mu.Lock()
	m.objects[key] = mockObject{data: body, metadata: meta, lastModified: time.Now().UTC()}
	m.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (m *mockS3) handleGet(w http.ResponseWriter, r *http.Request, key string) {
	m.mu.Lock()
	obj, ok := m.objects[key]
	m.mu.Unlock()
	if !ok {
		writeNoSuchKey(w)
		return
	}

	for k, v := range obj.metadata {
		w.Header().Set("X-Amz-Meta-"+k, v)
	}
	w.Header().Set("Last-Modified", obj.lastModified.Format(http.TimeFormat))

	data := obj.data
	status := http.StatusOK
	if rangeHdr := r.Header.Get("Range"); rangeHdr != "" {
		start, end, ok := parseByteRange(rangeHdr, int64(len(data)))
		if !ok {
			// Unsatisfiable range -> 416, matching real S3 for offset>=EOF.
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		data = data[start : end+1]
		status = http.StatusPartialContent
	}

	m.mu.Lock()
	omit := m.omitContentLength
	m.mu.Unlock()
	if !omit {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	}
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func (m *mockS3) handleHead(w http.ResponseWriter, key string) {
	m.mu.Lock()
	obj, ok := m.objects[key]
	m.mu.Unlock()
	if !ok {
		// HEAD has no body; the SDK maps a 404 with no NoSuchKey body to a
		// NotFound, which isNotFoundError catches via the "404"/"NotFound"
		// string fallback.
		w.WriteHeader(http.StatusNotFound)
		return
	}
	for k, v := range obj.metadata {
		w.Header().Set("X-Amz-Meta-"+k, v)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(obj.data)))
	w.Header().Set("Last-Modified", obj.lastModified.Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
}

func (m *mockS3) handleDelete(w http.ResponseWriter, key string) {
	m.mu.Lock()
	delete(m.objects, key)
	m.mu.Unlock()
	// S3 DeleteObject returns 204 even when the key was absent.
	w.WriteHeader(http.StatusNoContent)
}

func (m *mockS3) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix := q.Get("prefix")
	token := q.Get("continuation-token")

	m.mu.Lock()
	keys := make([]string, 0, len(m.objects))
	for k := range m.objects {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	objs := make(map[string]mockObject, len(m.objects))
	for k, v := range m.objects {
		objs[k] = v
	}
	pageSize := m.listPageSize
	m.mu.Unlock()

	sort.Strings(keys)

	// Continuation token is simply the index to resume from (encoded as a
	// decimal string). Deterministic and sufficient for the paginator.
	startIdx := 0
	if token != "" {
		if i, err := strconv.Atoi(token); err == nil {
			startIdx = i
		}
	}

	res := listObjectsV2Result{}
	end := len(keys)
	if pageSize > 0 && startIdx+pageSize < end {
		end = startIdx + pageSize
		res.IsTruncated = true
		res.NextContinuationToken = strconv.Itoa(end)
	}
	for _, k := range keys[startIdx:end] {
		obj := objs[k]
		res.Contents = append(res.Contents, listObjEntry{
			Key:          k,
			Size:         int64(len(obj.data)),
			LastModified: obj.lastModified,
		})
	}

	out, err := xml.Marshal(res)
	if err != nil {
		http.Error(w, "marshal", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(out)
}

func writeNoSuchKey(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>` +
		`<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>`))
}

// parseByteRange parses a single "bytes=start-end" header against a known
// object size and returns the inclusive [start,end] indices. Returns
// ok=false for an unsatisfiable range (start beyond EOF), mirroring S3's
// 416 behavior. end is clamped to size-1 so a partial-past-EOF request
// returns the available tail.
func parseByteRange(hdr string, size int64) (start, end int64, ok bool) {
	const p = "bytes="
	if !strings.HasPrefix(hdr, p) {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(hdr, p)
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(spec[:dash], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	end, err = strconv.ParseInt(spec[dash+1:], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	if start < 0 || start >= size {
		return 0, 0, false // unsatisfiable
	}
	if end >= size {
		end = size - 1
	}
	if end < start {
		return 0, 0, false
	}
	return start, end, true
}

// newTestStore spins up a mockS3 behind an httptest.Server and returns a
// Store wired to it plus the mock for fault injection. The store and
// server are torn down via t.Cleanup.
//
// The client disables request/response checksums so PutObject sends a
// plain (non-aws-chunked) body the mock can read directly.
func newTestStore(t *testing.T) (*Store, *mockS3) {
	t.Helper()
	const bucket = "test-bucket"
	mock := newMockS3(bucket)
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)

	client := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(
			"test", "test", ""),
		HTTPClient:                 awshttp.NewBuildableClient(),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})

	store := New(client, Config{Bucket: bucket})
	t.Cleanup(func() { _ = store.Close() })
	return store, mock
}

// mustHash returns the BLAKE3-256 content hash of b. Unlike the
// *testing.T-bound hashOf in verifier_test.go, this is usable from
// helpers that do not hold a t.
func mustHash(b []byte) block.ContentHash {
	return block.ContentHash(blake3.Sum256(b))
}

// TestStore_Put_Get_RoundTrip drives the full PUT then GET wire path.
func TestStore_Put_Get_RoundTrip(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	data := []byte("mock-s3 round-trip payload — deterministic bytes")
	h := mustHash(data)

	if err := store.Put(ctx, h, data); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(ctx, h)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("Get returned %q, want %q", got, data)
	}
}

// TestStore_Get_NotFound pins the NoSuchKey -> ErrChunkNotFound mapping.
func TestStore_Get_NotFound(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	if _, err := store.Get(ctx, mustHash([]byte("absent"))); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("Get on missing key: want ErrChunkNotFound, got %v", err)
	}
}

// TestStore_Has covers both the present and absent HEAD outcomes.
func TestStore_Has(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	data := []byte("has-probe payload")
	h := mustHash(data)

	has, err := store.Has(ctx, h)
	if err != nil {
		t.Fatalf("Has (absent): %v", err)
	}
	if has {
		t.Fatal("Has on absent key: want false")
	}

	if err := store.Put(ctx, h, data); err != nil {
		t.Fatalf("Put: %v", err)
	}
	has, err = store.Has(ctx, h)
	if err != nil {
		t.Fatalf("Has (present): %v", err)
	}
	if !has {
		t.Fatal("Has on present key: want true")
	}
}

// TestStore_Head verifies Meta.Size and a non-zero LastModified, plus the
// NotFound mapping.
func TestStore_Head(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i)
	}
	h := mustHash(data)
	if err := store.Put(ctx, h, data); err != nil {
		t.Fatalf("Put: %v", err)
	}

	m, err := store.Head(ctx, h)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if m.Size != int64(len(data)) {
		t.Errorf("Head Size = %d, want %d", m.Size, len(data))
	}
	if m.LastModified.IsZero() {
		t.Error("Head LastModified is zero")
	}

	if _, err := store.Head(ctx, mustHash([]byte("nope"))); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("Head on missing key: want ErrChunkNotFound, got %v", err)
	}
}

// TestStore_GetRange exercises mid-block, tail, partial-past-EOF, and the
// argument-validation sentinels.
func TestStore_GetRange(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	data := []byte("0123456789abcdef") // 16 bytes
	h := mustHash(data)
	if err := store.Put(ctx, h, data); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Mid-block.
	got, err := store.GetRange(ctx, h, 4, 8)
	if err != nil {
		t.Fatalf("GetRange mid: %v", err)
	}
	if string(got) != "456789ab" {
		t.Fatalf("GetRange mid = %q, want %q", got, "456789ab")
	}

	// Partial past EOF: returns the available tail without error.
	got, err = store.GetRange(ctx, h, 8, 20)
	if err != nil {
		t.Fatalf("GetRange partial-past-EOF: %v", err)
	}
	if string(got) != "89abcdef" {
		t.Fatalf("GetRange partial-past-EOF = %q, want %q", got, "89abcdef")
	}

	// Offset strictly past EOF: the mock returns 416, surfaced as an error.
	if _, err := store.GetRange(ctx, h, 100, 4); err == nil {
		t.Fatal("GetRange offset past EOF: want error, got nil")
	}

	// Argument validation sentinels (checked before any wire call).
	if _, err := store.GetRange(ctx, h, -1, 4); !errors.Is(err, block.ErrInvalidOffset) {
		t.Fatalf("GetRange offset=-1: want ErrInvalidOffset, got %v", err)
	}
	if _, err := store.GetRange(ctx, h, 0, 0); !errors.Is(err, block.ErrInvalidSize) {
		t.Fatalf("GetRange length=0: want ErrInvalidSize, got %v", err)
	}
	if _, err := store.GetRange(ctx, h, 0, -4); !errors.Is(err, block.ErrInvalidSize) {
		t.Fatalf("GetRange length=-4: want ErrInvalidSize, got %v", err)
	}

	// Range on a missing key maps to ErrChunkNotFound.
	if _, err := store.GetRange(ctx, mustHash([]byte("missing")), 0, 4); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("GetRange on missing key: want ErrChunkNotFound, got %v", err)
	}
}

// TestStore_Delete_Idempotent confirms Delete succeeds whether or not the
// object exists and that a subsequent Get misses.
func TestStore_Delete_Idempotent(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	data := []byte("to be deleted")
	h := mustHash(data)
	if err := store.Put(ctx, h, data); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := store.Delete(ctx, h); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, h); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("Get after Delete: want ErrChunkNotFound, got %v", err)
	}
	// Idempotent: deleting an absent key still succeeds.
	if err := store.Delete(ctx, h); err != nil {
		t.Fatalf("Delete (idempotent): %v", err)
	}
}

// TestStore_ReadBlockVerified covers the happy path, the streaming-hash
// mismatch path, and the header pre-check fast-fail path.
func TestStore_ReadBlockVerified(t *testing.T) {
	store, mock := newTestStore(t)
	ctx := context.Background()

	data := []byte("verified read payload — must hash to the stored key")
	h := mustHash(data)
	if err := store.Put(ctx, h, data); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Happy path: body hashes to expected.
	got, err := store.ReadBlockVerified(ctx, h, h)
	if err != nil {
		t.Fatalf("ReadBlockVerified happy: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("ReadBlockVerified bytes mismatch")
	}

	// Header pre-check: the stored object carries content-hash == h, so
	// asking the verifier to expect a different hash trips the header
	// fast-fail before the body is read.
	wrongExpected := mustHash([]byte("different"))
	if _, err := store.ReadBlockVerified(ctx, h, wrongExpected); !errors.Is(err, block.ErrCASContentMismatch) {
		t.Fatalf("ReadBlockVerified header pre-check: want ErrCASContentMismatch, got %v", err)
	}

	// Streaming mismatch: store an object whose stamped header matches the
	// expected hash but whose body does NOT, so the header pre-check passes
	// and the streaming recompute catches the corruption.
	key := store.hashKey(h)
	mock.mu.Lock()
	mock.objects[key] = mockObject{
		data:         []byte("corrupted body that does not hash to h"),
		metadata:     map[string]string{"content-hash": h.CASKey()},
		lastModified: time.Now().UTC(),
	}
	mock.mu.Unlock()
	if _, err := store.ReadBlockVerified(ctx, h, h); !errors.Is(err, block.ErrCASContentMismatch) {
		t.Fatalf("ReadBlockVerified streaming mismatch: want ErrCASContentMismatch, got %v", err)
	}

	// Missing key maps to ErrChunkNotFound.
	if _, err := store.ReadBlockVerified(ctx, mustHash([]byte("gone")), mustHash([]byte("gone"))); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("ReadBlockVerified missing: want ErrChunkNotFound, got %v", err)
	}
}

// TestStore_Walk_Enumerate verifies Walk visits every CAS object exactly
// once with a non-zero LastModified, skips non-CAS keys, and spans
// multiple paginator pages.
func TestStore_Walk_Enumerate(t *testing.T) {
	store, mock := newTestStore(t)
	mock.mu.Lock()
	mock.listPageSize = 2 // force multi-page pagination
	mock.mu.Unlock()
	ctx := context.Background()

	want := make(map[block.ContentHash]int64)
	for i := 0; i < 5; i++ {
		p := []byte(fmt.Sprintf("walk-object-%d-payload", i))
		h := mustHash(p)
		want[h] = int64(len(p))
		if err := store.Put(ctx, h, p); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}

	// A non-CAS key under the bucket must be skipped by Walk.
	mock.mu.Lock()
	mock.objects["cas/zz/zz/not-a-valid-cas-key"] = mockObject{
		data: []byte("junk"), lastModified: time.Now().UTC(),
	}
	mock.mu.Unlock()

	seen := make(map[block.ContentHash]int)
	err := store.Walk(ctx, func(h block.ContentHash, m block.Meta) error {
		seen[h]++
		if m.LastModified.IsZero() {
			t.Errorf("Walk Meta.LastModified zero for %s", h)
		}
		if w, ok := want[h]; ok && m.Size != w {
			t.Errorf("Walk Size for %s = %d, want %d", h, m.Size, w)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(seen) != len(want) {
		t.Fatalf("Walk visited %d objects, want %d", len(seen), len(want))
	}
	for h, n := range seen {
		if n != 1 {
			t.Errorf("Walk visited %s %d times, want 1", h, n)
		}
	}
}

// TestStore_Walk_StopSentinel pins the ErrStopWalk clean-exit contract.
func TestStore_Walk_StopSentinel(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		p := []byte(fmt.Sprintf("stop-%d", i))
		if err := store.Put(ctx, mustHash(p), p); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	seen := 0
	err := store.Walk(ctx, func(block.ContentHash, block.Meta) error {
		seen++
		return block.ErrStopWalk
	})
	if err != nil {
		t.Fatalf("Walk ErrStopWalk: want nil, got %v", err)
	}
	if seen != 1 {
		t.Fatalf("Walk should stop after first ErrStopWalk; saw %d", seen)
	}
}

// TestStore_Walk_ErrorWrap pins the "walk halted at %s: %w" wrapping
// contract for a non-sentinel callback error.
func TestStore_Walk_ErrorWrap(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	p := []byte("wrap-target")
	if err := store.Put(ctx, mustHash(p), p); err != nil {
		t.Fatalf("Put: %v", err)
	}

	custom := errors.New("custom walk callback error")
	err := store.Walk(ctx, func(block.ContentHash, block.Meta) error {
		return custom
	})
	if !errors.Is(err, custom) {
		t.Fatalf("Walk error does not wrap custom: got %v", err)
	}
	if !strings.Contains(err.Error(), "walk halted at") {
		t.Errorf("Walk error missing 'walk halted at' prefix: %q", err.Error())
	}
}

// TestStore_Walk_ContextCancel verifies a cancelled context aborts Walk.
func TestStore_Walk_ContextCancel(t *testing.T) {
	store, mock := newTestStore(t)
	mock.mu.Lock()
	mock.listPageSize = 1
	mock.mu.Unlock()

	for i := 0; i < 4; i++ {
		p := []byte(fmt.Sprintf("cancel-%d", i))
		if err := store.Put(context.Background(), mustHash(p), p); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Walk starts
	err := store.Walk(ctx, func(block.ContentHash, block.Meta) error {
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Walk on cancelled ctx: want context.Canceled, got %v", err)
	}
}

// TestStore_ClosedGuards confirms every public method fails closed with
// ErrStoreClosed after Close.
func TestStore_ClosedGuards(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	h := mustHash([]byte("x"))

	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := store.Put(ctx, h, []byte("x")); !errors.Is(err, block.ErrStoreClosed) {
		t.Errorf("Put after Close: want ErrStoreClosed, got %v", err)
	}
	if _, err := store.Get(ctx, h); !errors.Is(err, block.ErrStoreClosed) {
		t.Errorf("Get after Close: want ErrStoreClosed, got %v", err)
	}
	if _, err := store.GetRange(ctx, h, 0, 4); !errors.Is(err, block.ErrStoreClosed) {
		t.Errorf("GetRange after Close: want ErrStoreClosed, got %v", err)
	}
	if _, err := store.Has(ctx, h); !errors.Is(err, block.ErrStoreClosed) {
		t.Errorf("Has after Close: want ErrStoreClosed, got %v", err)
	}
	if _, err := store.Head(ctx, h); !errors.Is(err, block.ErrStoreClosed) {
		t.Errorf("Head after Close: want ErrStoreClosed, got %v", err)
	}
	if err := store.Delete(ctx, h); !errors.Is(err, block.ErrStoreClosed) {
		t.Errorf("Delete after Close: want ErrStoreClosed, got %v", err)
	}
	if _, err := store.ReadBlockVerified(ctx, h, h); !errors.Is(err, block.ErrStoreClosed) {
		t.Errorf("ReadBlockVerified after Close: want ErrStoreClosed, got %v", err)
	}
	walkErr := store.Walk(ctx, func(block.ContentHash, block.Meta) error { return nil })
	if !errors.Is(walkErr, block.ErrStoreClosed) {
		t.Errorf("Walk after Close: want ErrStoreClosed, got %v", walkErr)
	}
	if err := store.HealthCheck(ctx); !errors.Is(err, block.ErrStoreClosed) {
		t.Errorf("HealthCheck after Close: want ErrStoreClosed, got %v", err)
	}
}

// TestStore_RetryOn5xx verifies the SDK retryer recovers from a single
// transient 5xx on Put (failNext fires once, then the retry succeeds).
func TestStore_RetryOn5xx(t *testing.T) {
	store, mock := newTestStore(t)
	ctx := context.Background()

	mock.mu.Lock()
	mock.failNextStatus = http.StatusServiceUnavailable
	mock.failNextMethod = http.MethodPut
	mock.mu.Unlock()

	data := []byte("retry payload")
	h := mustHash(data)
	if err := store.Put(ctx, h, data); err != nil {
		t.Fatalf("Put should recover after one 5xx: %v", err)
	}
	got, err := store.Get(ctx, h)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("Get bytes mismatch after retry")
	}
}

// TestStore_HealthCheck covers the HeadBucket success path and the
// structured Healthcheck wrapper.
func TestStore_HealthCheck(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	if err := store.HealthCheck(ctx); err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	rep := store.Healthcheck(ctx)
	if rep.Status != health.StatusHealthy {
		t.Errorf("Healthcheck reported non-healthy status: %+v", rep)
	}
}

// TestStore_PutContextCancel verifies a cancelled context propagates as an
// error from a wire call rather than hanging.
func TestStore_PutContextCancel(t *testing.T) {
	store, _ := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := store.Put(ctx, mustHash([]byte("x")), []byte("x"))
	if err == nil {
		t.Fatal("Put on cancelled ctx: want error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Logf("Put on cancelled ctx returned %v (non-context.Canceled is acceptable if it is a wrapped transport error)", err)
	}
}

// TestStore_Get_NoContentLength exercises the readResponseBody fallback
// path where the response omits Content-Length (chunked transfer).
func TestStore_Get_NoContentLength(t *testing.T) {
	store, mock := newTestStore(t)
	ctx := context.Background()

	data := []byte("chunked-transfer payload without content-length")
	h := mustHash(data)
	if err := store.Put(ctx, h, data); err != nil {
		t.Fatalf("Put: %v", err)
	}

	mock.mu.Lock()
	mock.omitContentLength = true
	mock.mu.Unlock()

	got, err := store.Get(ctx, h)
	if err != nil {
		t.Fatalf("Get (no content-length): %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("Get bytes mismatch on no-content-length path")
	}
}

// TestNewFromConfig_Validation pins the required-field validation and a
// successful construction (which does not perform any network I/O).
func TestNewFromConfig_Validation(t *testing.T) {
	ctx := context.Background()

	if _, err := NewFromConfig(ctx, Config{}); err == nil {
		t.Error("NewFromConfig with empty bucket: want error, got nil")
	}
	if _, err := NewFromConfig(ctx, Config{Bucket: "b"}); err == nil {
		t.Error("NewFromConfig without credentials: want error, got nil")
	}
	if _, err := NewFromConfig(ctx, Config{Bucket: "b", AccessKey: "a"}); err == nil {
		t.Error("NewFromConfig with access key but no secret: want error, got nil")
	}

	store, err := NewFromConfig(ctx, Config{
		Bucket:         "b",
		Region:         "us-east-1",
		Endpoint:       "localhost:4566",
		AccessKey:      "a",
		SecretKey:      "s",
		KeyPrefix:      "prefix/",
		MaxRetries:     3,
		ForcePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewFromConfig valid: %v", err)
	}
	if store == nil {
		t.Fatal("NewFromConfig valid: want non-nil store")
	}
	_ = store.Close()
}

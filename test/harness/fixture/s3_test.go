package fixture

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// fakeS3 is an S3 service holding its buckets and objects in memory, which
// answers path-style requests as the service does, and records each.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte // by bucket/key, and each bucket by its name
	create  int               // when set, what creating a bucket answers, creating nothing
	corrupt bool              // a read gives other bytes than were written
	calls   []string
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/")
	f.calls = append(f.calls, r.Method+" "+path)
	name, key, _ := strings.Cut(path, "/")
	_, exists := f.objects[name]
	switch {
	case key == "" && r.Method == http.MethodPut && f.create != 0:
		w.WriteHeader(f.create)
	case key == "" && r.Method == http.MethodPut && exists:
		w.WriteHeader(http.StatusConflict) // BucketAlreadyOwnedByYou
	case key == "" && r.Method == http.MethodPut:
		f.objects[name] = nil
	case !exists:
		http.Error(w, "NoSuchBucket", http.StatusNotFound)
	case r.Method == http.MethodPut:
		f.objects[path], _ = io.ReadAll(r.Body)
	case r.Method == http.MethodGet && f.objects[path] != nil:
		data := f.objects[path]
		if f.corrupt {
			data = append([]byte("x"), data...)
		}
		_, _ = w.Write(data)
	default:
		http.Error(w, "NoSuchKey", http.StatusNotFound)
	}
}

// The bucket is created at the endpoint the cell's services give, or found
// there, and must then take a write and read it back; anything else fails
// the setup, saying where.
func TestBucket(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fake    *fakeS3
		wantErr string
	}{
		{"created", &fakeS3{}, ""},
		{"there already", &fakeS3{objects: map[string][]byte{bucketName: nil}}, ""},
		{"refused", &fakeS3{create: http.StatusForbidden}, "creating the bucket dittofs-dt at {url}: answered 403"},
		{"missing", &fakeS3{create: http.StatusOK}, "the bucket dittofs-dt at {url} takes no write: PUT dt-"},
		{"reads back other bytes", &fakeS3{corrupt: true}, "the bucket dittofs-dt at {url} doesn't give back a write: GET dt-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := tc.fake
			if fake.objects == nil {
				fake.objects = map[string][]byte{}
			}
			srv := httptest.NewServer(fake)
			t.Cleanup(srv.Close)
			services := []string{"LOCALSTACK_ENDPOINT=" + srv.URL}
			b, err := newBucket(context.Background(), services)
			if want := strings.ReplaceAll(tc.wantErr, "{url}", srv.URL); want != "" {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("got %v, want %q", err, want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			probe := bucketName + "/" + b.prefix + "dt-probe"
			if b.endpoint != srv.URL || len(fake.calls) != 3 || fake.calls[1] != "PUT "+probe || fake.calls[2] != "GET "+probe {
				t.Errorf("bucket %+v, calls %q", b, fake.calls)
			}
		})
	}
}

// The block store configuration the fixture sends is one the server's own
// validator accepts, so a misspelt or dropped key shows here, where no S3
// service is needed, rather than as every S3 cell not running. The endpoint
// is on loopback, as the S3 service dt services starts is.
func TestStoreConfigValidates(t *testing.T) {
	b := &bucket{endpoint: "http://127.0.0.1:4566", prefix: "dt-0123456789abcdef/"}
	cfg := &models.BlockStoreConfig{Type: "s3"}
	if err := cfg.SetConfig(b.storeConfig()); err != nil {
		t.Fatal(err)
	}
	if err := runtime.ValidateBlockStoreConfig("s3", cfg); err != nil {
		t.Fatalf("the server refuses the fixture's S3 configuration: %v", err)
	}
}

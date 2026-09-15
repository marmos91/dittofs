package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestRESTSeam_DecodesTheShareNameInTheURLOnce pins the half of the share-name
// fold that does still decode. A router matches on the escaped path when the
// request carries one, so a handler reading {name} gets the segment as it was
// sent; without the decode a request for a share whose name contains a slash
// addresses no share at all.
//
// Decoding here is also the seam's ceiling: the last two cases are one name in
// two spellings, and only the one that keeps the path escaped reaches it.
func TestRESTSeam_DecodesTheShareNameInTheURLOnce(t *testing.T) {
	tests := []struct {
		name    string
		segment string
		want    string
	}{
		{"an escaped slash reaches the share it names", "%2Fexport", "/export"},
		{"an unescaped name is untouched", "export", "/export"},
		// The next two are one name, "/a%2Fb", in two spellings. Escaped whole
		// it reaches its share: the escaped leading slash keeps the path in its
		// escaped form, so the seam's decode is the only one applied.
		{"a literal percent survives when the name is escaped whole", "%2Fa%252Fb", "/a%2Fb"},
		// With that leading slash stripped, the remaining escapes are exactly
		// what the default path encoder would produce, so the segment arrives
		// already decoded and the seam's decode is the second one.
		{"the same name loses its percent once the leading slash is stripped", "a%252Fb", "/a/b"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var reached string
			h := NewSnapshotHandler(&fakeSnapshotRuntime{
				listFn: func(_ context.Context, share string) ([]*models.Snapshot, error) {
					reached = share
					return nil, nil
				},
			}, time.Second, nil)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/shares/"+tc.segment+"/snapshots", nil)
			newSnapshotRouter(h).ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("GET /api/v1/shares/%s/snapshots: status %d, want 200", tc.segment, rec.Code)
			}
			if reached != tc.want {
				t.Fatalf("GET /api/v1/shares/%s/snapshots addressed share %q, want %q", tc.segment, reached, tc.want)
			}
		})
	}
}

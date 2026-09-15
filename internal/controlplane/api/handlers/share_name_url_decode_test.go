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
// Decoding here is also the seam's ceiling: see the third case.
func TestRESTSeam_DecodesTheShareNameInTheURLOnce(t *testing.T) {
	tests := []struct {
		name    string
		segment string
		want    string
	}{
		{"an escaped slash reaches the share it names", "%2Fexport", "/export"},
		{"an unescaped name is untouched", "export", "/export"},
		// The router matches on the escaped path only when the raw segment
		// differs from the default encoding of its decoded form. "%25" decodes
		// to "%", which re-encodes to "%25", so the segment reaches the handler
		// already decoded once and the seam's own decode is the second. A share
		// whose name holds a percent that forms a valid escape therefore cannot
		// be addressed through a URL at all.
		{"a percent that forms a valid escape cannot be addressed", "a%252Fb", "/a/b"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := make(chan string, 1)
			h := NewSnapshotHandler(&fakeSnapshotRuntime{
				listFn: func(_ context.Context, share string) ([]*models.Snapshot, error) {
					got <- share
					return nil, nil
				},
			}, time.Second, nil)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/shares/"+tc.segment+"/snapshots", nil)
			newSnapshotRouter(h).ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("GET /api/v1/shares/%s/snapshots: status %d, want 200", tc.segment, rec.Code)
			}
			select {
			case share := <-got:
				if share != tc.want {
					t.Fatalf("GET /api/v1/shares/%s/snapshots addressed share %q, want %q", tc.segment, share, tc.want)
				}
			default:
				t.Fatalf("GET /api/v1/shares/%s/snapshots reached no share", tc.segment)
			}
		})
	}
}

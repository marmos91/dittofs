package health

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A failed fetch must not be indistinguishable from a cluster with no block
// stores. The old code appended both failures to a warning list and gated its
// output on a non-empty result, so total loss of visibility exited zero.
func TestBlockStoreFetchFailureIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	ents := FetchEntities(srv.Client(), srv.URL, "")
	joined := strings.Join(ents.Errors, " ")
	if joined == "" {
		t.Fatal("a failed block store fetch must be reported")
	}
	if !strings.Contains(joined, "block stores") {
		t.Errorf("error must name what failed; got %q", joined)
	}
}

// "Nothing configured" and "could not ask" must not both come back as a nil
// slice: a successful fetch leaves BlockStores non-nil even when it is empty.
func TestEmptyBlockStoreListIsNotAFetchFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	ents := FetchEntities(srv.Client(), srv.URL, "")
	if len(ents.Errors) != 0 {
		t.Fatalf("no fetch failed; got errors %v", ents.Errors)
	}
	if ents.BlockStores == nil {
		t.Error("an empty block store list must be distinguishable from a failed fetch")
	}
}

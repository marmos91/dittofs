package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestShareHandler_Create_ArmsDurableCommitOnTheServedShare checks the setting
// where COMMIT reads it: the running share's block store. A share created with
// commit_ack=block-store must acknowledge only durably from its first COMMIT,
// with no update in between to apply it.
func TestShareHandler_Create_ArmsDurableCommitOnTheServedShare(t *testing.T) {
	s := newShareServer(t)
	body := `{"name":"acked","metadata_store_id":"md","block_store":"mem","default_permission":"read-write","commit_ack":"block-store"}`
	w := httptest.NewRecorder()
	s.h.Create(w, httptest.NewRequest(http.MethodPost, "/api/v1/shares", strings.NewReader(body)))
	if w.Code != http.StatusCreated {
		t.Fatalf("Create = %d, body=%s", w.Code, w.Body.String())
	}

	bs, err := s.rt.GetBlockStoreForShare("/acked")
	if err != nil {
		t.Fatalf("GetBlockStoreForShare: %v", err)
	}
	if !bs.RequireDurableCommit() {
		t.Fatal("share created with commit_ack=block-store serves COMMIT with durable commit off")
	}
}

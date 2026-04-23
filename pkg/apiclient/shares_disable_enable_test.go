package apiclient

import (
	"net/http"
	"testing"
)

// TestClient_DisableShare_ReturnsEnabledFalse verifies the client parses the
// Enabled=false field from the response body (Plan 01 Task 4 landed the
// field; Plan 02 wires the handler).
func TestClient_DisableShare_ReturnsEnabledFalse(t *testing.T) {
	s := newStubServer(t)
	defer s.Close()
	s.reset()
	s.body = []byte(`{"id":"s1","name":"/alice","enabled":false}`)

	c := newTestClient(s)
	share, err := c.DisableShare("alice")
	if err != nil {
		t.Fatalf("DisableShare: %v", err)
	}
	if share.Enabled {
		t.Errorf("Enabled should be false, got true")
	}
	calls := s.observedCalls()
	if calls[0].Method != http.MethodPost {
		t.Errorf("method = %q, want POST", calls[0].Method)
	}
	if calls[0].Path != "/api/v1/shares/alice/disable" {
		t.Errorf("path = %q", calls[0].Path)
	}
}

func TestClient_EnableShare_ReturnsEnabledTrue(t *testing.T) {
	s := newStubServer(t)
	defer s.Close()
	s.reset()
	s.body = []byte(`{"id":"s1","name":"/alice","enabled":true}`)

	c := newTestClient(s)
	share, err := c.EnableShare("alice")
	if err != nil {
		t.Fatalf("EnableShare: %v", err)
	}
	if !share.Enabled {
		t.Errorf("Enabled should be true")
	}
	calls := s.observedCalls()
	if calls[0].Path != "/api/v1/shares/alice/enable" {
		t.Errorf("path = %q", calls[0].Path)
	}
}

package apiclient

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// stubCall records a single request observed by stubServer.
type stubCall struct {
	Method string
	Path   string
	Body   []byte
}

// stubServer is a test-only httptest.Server wrapper that records every
// request and echoes back a configurable status+body+content-type.
// Concurrent requests from parallel subtests are serialized by mu.
type stubServer struct {
	*httptest.Server

	mu          sync.Mutex
	calls       []stubCall
	status      int
	body        []byte
	contentType string
}

// reset clears recorded calls and restores default response controls.
func (s *stubServer) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = nil
	s.status = http.StatusOK
	s.body = nil
	s.contentType = "application/json"
}

// observedCalls returns a snapshot of recorded calls for assertions.
func (s *stubServer) observedCalls() []stubCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]stubCall, len(s.calls))
	copy(out, s.calls)
	return out
}

// newStubServer builds an httptest.Server that records request method+path
// and echoes back a canned response + status+contenttype.
func newStubServer(t *testing.T) *stubServer {
	t.Helper()
	s := &stubServer{status: http.StatusOK, contentType: "application/json"}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "stubserver: read body: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := r.Body.Close(); err != nil {
			http.Error(w, "stubserver: close body: "+err.Error(), http.StatusInternalServerError)
			return
		}

		s.mu.Lock()
		s.calls = append(s.calls, stubCall{Method: r.Method, Path: r.URL.RequestURI(), Body: b})
		status := s.status
		body := s.body
		contentType := s.contentType
		s.mu.Unlock()

		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		if body != nil {
			_, _ = w.Write(body)
		}
	}))
	return s
}

// newTestClient returns a Client pointed at the stub server.
func newTestClient(s *stubServer) *Client {
	return New(s.URL)
}

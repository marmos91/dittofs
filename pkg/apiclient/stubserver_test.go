package apiclient

import (
	"io"
	"net/http"
	"net/http/httptest"
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
type stubServer struct {
	*httptest.Server
	calls []stubCall

	// Response controls
	status      int
	body        []byte
	contentType string
}

// reset clears recorded calls and restores default response controls.
func (s *stubServer) reset() {
	s.calls = nil
	s.status = http.StatusOK
	s.body = nil
	s.contentType = "application/json"
}

// newStubServer builds an httptest.Server that records request method+path
// and echoes back a canned response + status+contenttype.
func newStubServer(t *testing.T) *stubServer {
	t.Helper()
	s := &stubServer{status: http.StatusOK, contentType: "application/json"}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		s.calls = append(s.calls, stubCall{Method: r.Method, Path: r.URL.RequestURI(), Body: b})
		if s.contentType != "" {
			w.Header().Set("Content-Type", s.contentType)
		}
		w.WriteHeader(s.status)
		if s.body != nil {
			_, _ = w.Write(s.body)
		}
	}))
	return s
}

// newTestClient returns a Client pointed at the stub server.
func newTestClient(s *stubServer) *Client {
	return New(s.URL)
}

package handlers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// slowDrainRuntime is a drainRuntime whose flush deliberately outlasts the
// server's WriteTimeout, modelling a multi-GiB backlog draining to a WAN remote.
type slowDrainRuntime struct {
	drainDelay time.Duration
	progress   atomic.Int64
}

func (s *slowDrainRuntime) DrainAllUploads(ctx context.Context) error {
	select {
	case <-time.After(s.drainDelay):
		s.progress.Add(1)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *slowDrainRuntime) UploadProgress() int64 { return s.progress.Load() }

// TestDrainUploads_SurvivesServerWriteTimeout guards the cold-read benchmark
// blocker: a drain legitimately runs longer than http.Server.WriteTimeout, so
// the handler must clear this connection's write deadline. Without the clear the
// transport tears the connection down mid-drain and the client sees a bare EOF
// (which is exactly what broke `dfsctl system drain-uploads` and, with it, every
// cold-from-S3 read benchmark). Here the drain (400ms) outlasts the server's
// 150ms WriteTimeout; the POST must still return 200, not error out.
func TestDrainUploads_SurvivesServerWriteTimeout(t *testing.T) {
	const writeTimeout = 150 * time.Millisecond
	h := &SystemHandler{
		runtime:           &slowDrainRuntime{drainDelay: 400 * time.Millisecond},
		drainStallTimeout: time.Minute,
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(h.DrainUploads))
	srv.Config.WriteTimeout = writeTimeout
	srv.Start()
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", nil)
	if err != nil {
		t.Fatalf("POST failed — connection torn down mid-drain (write deadline not cleared?): %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
}

//go:build e2e

package snapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"gopkg.in/yaml.v3"

	"github.com/marmos91/dittofs/internal/controlplane/api/handlers"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// CLI e2e suite — drives the built `dfsctl` binary against an
// in-process httptest server. The binary is built lazily once per
// test run, the auth middleware is stubbed out (handler-only routing,
// same approach as snapshot_http_test.go), and a tiny share endpoint
// is mounted alongside the snapshot routes so the restore-preflight
// `GetShare` call resolves.

var (
	cliBinPath     string
	cliBuildOnce   sync.Once
	cliBuildErr    error
	cliBuildOutput []byte
)

// buildDFSCtl compiles the dfsctl binary once into a temp directory.
func buildDFSCtl(t *testing.T) string {
	t.Helper()
	cliBuildOnce.Do(func() {
		dir, err := os.Getwd()
		if err != nil {
			cliBuildErr = fmt.Errorf("getwd: %w", err)
			return
		}
		// Walk up until we find go.mod — the package CWD is
		// test/e2e/snapshot, not the repo root.
		repoRoot := dir
		for {
			if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err == nil {
				break
			}
			parent := filepath.Dir(repoRoot)
			if parent == repoRoot {
				cliBuildErr = fmt.Errorf("could not locate go.mod above %s", dir)
				return
			}
			repoRoot = parent
		}

		tmpDir, err := os.MkdirTemp("", "dfsctl-e2e-")
		if err != nil {
			cliBuildErr = fmt.Errorf("mkdir tmp: %w", err)
			return
		}
		cliBinPath = filepath.Join(tmpDir, "dfsctl")

		cmd := exec.Command("go", "build", "-o", cliBinPath, "./cmd/dfsctl")
		cmd.Dir = repoRoot
		out, err := cmd.CombinedOutput()
		cliBuildOutput = out
		if err != nil {
			cliBuildErr = fmt.Errorf("go build: %w\n%s", err, out)
			return
		}
	})
	if cliBuildErr != nil {
		t.Fatalf("build dfsctl: %v\nbuild output: %s", cliBuildErr, cliBuildOutput)
	}
	return cliBinPath
}

// cliFake extends the snapshot test fake with a tiny share registry
// so the restore-preflight GetShare call has something to consult,
// and auto-promotes snapshots from creating -> ready on the polling
// path so the blocking create flow terminates.
type cliFake struct {
	*fakeRuntime
	shares  map[string]bool // share name (e.g. "/data") -> enabled
	shareMu sync.Mutex
}

func newCLIFake() *cliFake {
	return &cliFake{
		fakeRuntime: newFakeRuntime(),
		shares:      map[string]bool{},
	}
}

func (f *cliFake) setShareEnabled(name string, enabled bool) {
	f.shareMu.Lock()
	defer f.shareMu.Unlock()
	f.shares[name] = enabled
}

func (f *cliFake) shareEnabled(name string) (bool, bool) {
	f.shareMu.Lock()
	defer f.shareMu.Unlock()
	v, ok := f.shares[name]
	return v, ok
}

// GetSnapshot wraps the parent fake so the polling loop inside the
// real apiclient's WaitForSnapshot observes a terminal state on the
// first poll. CreateSnapshot stamps state=creating; the block-by-
// default create path otherwise spins forever. We promote the
// underlying record to ready+durable on the first probe so the
// promotion is visible to subsequent GetSnapshot polls.
func (f *cliFake) GetSnapshot(ctx context.Context, share, snapID string) (*models.Snapshot, error) {
	f.fakeRuntime.mu.Lock()
	if bucket, ok := f.fakeRuntime.store[share]; ok {
		if s, ok := bucket[snapID]; ok && s.State == models.StateCreating {
			s.State = models.StateReady
			s.RemoteDurable = true
		}
	}
	f.fakeRuntime.mu.Unlock()
	return f.fakeRuntime.GetSnapshot(ctx, share, snapID)
}

// shareGetHandler answers GET /api/v1/shares/{name} with a minimal
// payload that satisfies apiclient.Client.GetShare. Only the fields
// the restore-preflight reads (Name, Enabled) need real values.
func shareGetHandler(f *cliFake) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw := chi.URLParam(r, "name")
		// Re-add the leading slash the API client strips before
		// sending — server-side normalization is otherwise lossy.
		name := raw
		if !strings.HasPrefix(name, "/") {
			name = "/" + name
		}
		enabled, ok := f.shareEnabled(name)
		if !ok {
			http.Error(w, `{"error":"share not found"}`, http.StatusNotFound)
			return
		}
		body := apiclient.Share{
			ID:      "id-" + strings.TrimPrefix(name, "/"),
			Name:    name,
			Enabled: enabled,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
}

// mountCLIServer mounts both the snapshot subtree and the minimal
// share GET endpoint behind a StripSlashes middleware so the client's
// no-trailing-slash collection paths match the test routes.
func mountCLIServer(t *testing.T, f *cliFake) *httptest.Server {
	t.Helper()
	h := handlers.NewSnapshotHandler(f, 30*time.Second, nil)
	r := chi.NewRouter()
	r.Use(chimiddleware.StripSlashes)
	r.Route("/api/v1/shares", func(r chi.Router) {
		r.Get("/{name}", shareGetHandler(f))
		r.Route("/{name}/snapshots", func(r chi.Router) {
			r.Post("/", h.Create)
			r.Get("/", h.List)
			r.Get("/{id}", h.Get)
			r.Delete("/{id}", h.Delete)
			r.Post("/{id}/restore", h.Restore)
		})
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// cliResult is the captured outcome of a single dfsctl invocation.
type cliResult struct {
	stdout   string
	stderr   string
	exitCode int
}

// runCLI invokes the built binary with the global --server/--token
// flags pointed at srv, optionally piping stdin. The returned
// cliResult never fails the test on a non-zero exit; callers assert
// exit codes explicitly.
func runCLI(t *testing.T, srv *httptest.Server, stdin string, args ...string) cliResult {
	t.Helper()
	bin := buildDFSCtl(t)
	full := append([]string{"--server", srv.URL, "--token", "test-token"}, args...)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, full...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	// Prevent the binary from reaching the developer's HOME-bound
	// credential store; --server/--token is the only auth path used.
	cmd.Env = append(os.Environ(), "HOME=/nonexistent-e2e-snapshot")
	err := cmd.Run()
	res := cliResult{stdout: outBuf.String(), stderr: errBuf.String()}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("dfsctl run: %v (stderr=%s)", err, res.stderr)
		}
	}
	return res
}

// seedReadyDurable seeds a snapshot already in the ready+durable
// state — saves a CreateSnapshot round-trip in tests that only care
// about the downstream surface (list/show/delete/restore). The
// `name` parameter is accepted for call-site readability but not
// stored, because models.Snapshot does not carry a human-readable
// name today (the wire-side `name` field is currently always empty).
func seedReadyDurable(f *cliFake, share, id, _ string) {
	f.seedSnapshot(share, &models.Snapshot{
		ID:            id,
		ShareName:     share,
		State:         models.StateReady,
		RemoteDurable: true,
		CreatedAt:     time.Now().Add(-2 * time.Hour),
		UpdatedAt:     time.Now().Add(-2 * time.Hour),
	})
}

// seedReadyNonDurable seeds a snapshot in ready state but with
// RemoteDurable=false — exercises the --force path.
func seedReadyNonDurable(f *cliFake, share, id, _ string) {
	f.seedSnapshot(share, &models.Snapshot{
		ID:            id,
		ShareName:     share,
		State:         models.StateReady,
		RemoteDurable: false,
		CreatedAt:     time.Now().Add(-1 * time.Hour),
		UpdatedAt:     time.Now().Add(-1 * time.Hour),
	})
}

// ---------- Tests ----------

func TestCLI_CreateNoWait(t *testing.T) {
	f := newCLIFake()
	srv := mountCLIServer(t, f)

	res := runCLI(t, srv, "", "share", "snapshot", "create", "/data", "--no-wait")
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0\nstdout=%s\nstderr=%s", res.exitCode, res.stdout, res.stderr)
	}
	// The fake assigns IDs like "snap-1"; assert presence + queued banner.
	if !strings.Contains(res.stdout, "snap-1") {
		t.Fatalf("stdout missing snapshot ID: %q", res.stdout)
	}
	if !strings.Contains(res.stdout, "queued") {
		t.Fatalf("stdout missing 'queued' banner: %q", res.stdout)
	}
}

func TestCLI_CreateBlock(t *testing.T) {
	f := newCLIFake()
	srv := mountCLIServer(t, f)

	res := runCLI(t, srv, "", "share", "snapshot", "create", "/data")
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0\nstdout=%s\nstderr=%s", res.exitCode, res.stdout, res.stderr)
	}
	// The CLI prints "Snapshot {id} -> ready" once the polling loop
	// observes the terminal state. ASCII arrow per the implementation.
	if !strings.Contains(res.stdout, "-> ready") {
		t.Fatalf("stdout missing '-> ready' marker: %q", res.stdout)
	}
}

func TestCLI_List_TableMode(t *testing.T) {
	f := newCLIFake()
	seedReadyDurable(f, "/data", "snap-cli-list-1", "weekly")
	srv := mountCLIServer(t, f)

	res := runCLI(t, srv, "", "share", "snapshot", "list", "/data")
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0\nstdout=%s\nstderr=%s", res.exitCode, res.stdout, res.stderr)
	}

	// Header row should carry all 6 expected columns.
	wantHeaders := []string{"ID", "NAME", "STATE", "DURABLE", "CREATED", "SIZE"}
	for _, h := range wantHeaders {
		if !strings.Contains(res.stdout, h) {
			t.Fatalf("table header missing %q\nstdout=%s", h, res.stdout)
		}
	}

	// ID column is 8 chars wide — the seeded ID has length > 8 so
	// the truncated form must appear and the full form must NOT.
	if !strings.Contains(res.stdout, "snap-cli") {
		t.Fatalf("table missing 8-char truncated ID\nstdout=%s", res.stdout)
	}
	if strings.Contains(res.stdout, "snap-cli-list-1") {
		t.Fatalf("table should have truncated full ID to 8 chars\nstdout=%s", res.stdout)
	}
}

func TestCLI_List_JSON(t *testing.T) {
	f := newCLIFake()
	seedReadyDurable(f, "/data", "snap-json-1", "alpha")
	seedReadyDurable(f, "/data", "snap-json-2", "beta")
	srv := mountCLIServer(t, f)

	res := runCLI(t, srv, "", "share", "snapshot", "list", "/data", "-o", "json")
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0\nstdout=%s\nstderr=%s", res.exitCode, res.stdout, res.stderr)
	}

	var got []apiclient.Snapshot
	if err := json.Unmarshal([]byte(res.stdout), &got); err != nil {
		t.Fatalf("json unmarshal: %v\nstdout=%s", err, res.stdout)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(got), got)
	}
	for _, s := range got {
		if s.ID == "" || s.Share == "" || s.State == "" {
			t.Fatalf("snapshot missing required fields: %+v", s)
		}
	}
}

func TestCLI_List_YAML(t *testing.T) {
	f := newCLIFake()
	seedReadyDurable(f, "/data", "snap-yaml-1", "gamma")
	srv := mountCLIServer(t, f)

	res := runCLI(t, srv, "", "share", "snapshot", "list", "/data", "-o", "yaml")
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0\nstdout=%s\nstderr=%s", res.exitCode, res.stdout, res.stderr)
	}

	// yaml.v3 lowercases field names since dto.Snapshot carries
	// only json tags. Parse into a neutral shape and assert keys.
	var got []map[string]any
	if err := yaml.Unmarshal([]byte(res.stdout), &got); err != nil {
		t.Fatalf("yaml unmarshal: %v\nstdout=%s", err, res.stdout)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1: %+v", len(got), got)
	}
	if id, _ := got[0]["id"].(string); id == "" {
		t.Fatalf("yaml entry missing 'id' field: %+v", got[0])
	}
}

func TestCLI_Show(t *testing.T) {
	f := newCLIFake()
	seedReadyDurable(f, "/data", "snap-show-1", "weekly")
	srv := mountCLIServer(t, f)

	res := runCLI(t, srv, "", "share", "snapshot", "show", "/data", "snap-show-1")
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0\nstdout=%s\nstderr=%s", res.exitCode, res.stdout, res.stderr)
	}

	// SimpleTable detail view should list every DTO field by its UI
	// label (see show.go). Assert a representative subset.
	wantLabels := []string{
		"ID", "NAME", "SHARE", "STATE", "REMOTE DURABLE",
		"MANIFEST COUNT", "DUMP BYTES", "CREATED AT", "UPDATED AT",
	}
	for _, l := range wantLabels {
		if !strings.Contains(res.stdout, l) {
			t.Fatalf("show output missing label %q\nstdout=%s", l, res.stdout)
		}
	}
	if !strings.Contains(res.stdout, "snap-show-1") {
		t.Fatalf("show output missing snapshot ID\nstdout=%s", res.stdout)
	}
}

func TestCLI_Delete_RefusesWithoutYes(t *testing.T) {
	f := newCLIFake()
	seedReadyDurable(f, "/data", "snap-del-1", "weekly")
	srv := mountCLIServer(t, f)

	res := runCLI(t, srv, "n\n", "share", "snapshot", "delete", "/data", "snap-del-1")
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0\nstdout=%s\nstderr=%s", res.exitCode, res.stdout, res.stderr)
	}
	if !strings.Contains(strings.ToLower(res.stdout), "aborted") {
		t.Fatalf("stdout missing 'Aborted': %q", res.stdout)
	}
	// Snapshot must still exist on the fake.
	if _, err := f.GetSnapshot(context.Background(), "/data", "snap-del-1"); err != nil {
		t.Fatalf("snapshot disappeared after refused delete: %v", err)
	}
}

func TestCLI_Delete_YesFlag(t *testing.T) {
	f := newCLIFake()
	seedReadyDurable(f, "/data", "snap-del-2", "weekly")
	srv := mountCLIServer(t, f)

	res := runCLI(t, srv, "", "share", "snapshot", "delete", "/data", "snap-del-2", "--yes")
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0\nstdout=%s\nstderr=%s", res.exitCode, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stdout, "deleted") {
		t.Fatalf("stdout missing 'deleted' confirmation: %q", res.stdout)
	}
	// Subsequent list must not surface the snapshot.
	listRes := runCLI(t, srv, "", "share", "snapshot", "list", "/data", "-o", "json")
	if listRes.exitCode != 0 {
		t.Fatalf("list after delete: exit = %d", listRes.exitCode)
	}
	var list []apiclient.Snapshot
	if err := json.Unmarshal([]byte(listRes.stdout), &list); err != nil {
		t.Fatalf("list after delete unmarshal: %v\nstdout=%s", err, listRes.stdout)
	}
	for _, s := range list {
		if s.ID == "snap-del-2" {
			t.Fatalf("deleted snapshot still listed: %+v", s)
		}
	}
}

func TestCLI_Restore_RefusesEnabled(t *testing.T) {
	f := newCLIFake()
	f.setShareEnabled("/data", true)
	seedReadyDurable(f, "/data", "snap-restore-1", "weekly")
	srv := mountCLIServer(t, f)

	res := runCLI(t, srv, "", "share", "snapshot", "restore", "/data", "snap-restore-1", "--yes")
	if res.exitCode == 0 {
		t.Fatalf("exit = 0, want non-zero (enabled share)\nstdout=%s\nstderr=%s", res.stdout, res.stderr)
	}
	if !strings.Contains(res.stderr, "disable") {
		t.Fatalf("stderr missing 'disable' hint: %q", res.stderr)
	}
}

func TestCLI_Restore_Disabled_YesFlag_Success(t *testing.T) {
	f := newCLIFake()
	f.setShareEnabled("/data", false)
	seedReadyDurable(f, "/data", "snap-restore-2", "weekly")
	srv := mountCLIServer(t, f)

	res := runCLI(t, srv, "", "share", "snapshot", "restore", "/data", "snap-restore-2", "--yes")
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0\nstdout=%s\nstderr=%s", res.exitCode, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stdout, "Safety snap:") {
		t.Fatalf("stdout missing 'Safety snap:' line: %q", res.stdout)
	}
}

func TestCLI_Restore_NotDurable_NoForce(t *testing.T) {
	f := newCLIFake()
	f.setShareEnabled("/data", false)
	seedReadyNonDurable(f, "/data", "snap-restore-3", "no-verify-snap")
	srv := mountCLIServer(t, f)

	res := runCLI(t, srv, "", "share", "snapshot", "restore", "/data", "snap-restore-3", "--yes")
	if res.exitCode == 0 {
		t.Fatalf("exit = 0, want non-zero (not durable, no --force)\nstdout=%s\nstderr=%s", res.stdout, res.stderr)
	}
	if !strings.Contains(res.stderr, "--force") {
		t.Fatalf("stderr missing '--force' suggestion: %q", res.stderr)
	}
}

func TestCLI_Restore_NotDurable_Force(t *testing.T) {
	f := newCLIFake()
	f.setShareEnabled("/data", false)
	seedReadyNonDurable(f, "/data", "snap-restore-4", "no-verify-snap")
	srv := mountCLIServer(t, f)

	res := runCLI(t, srv, "", "share", "snapshot", "restore", "/data", "snap-restore-4", "--yes", "--force")
	if res.exitCode != 0 {
		t.Fatalf("exit = %d, want 0\nstdout=%s\nstderr=%s", res.exitCode, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stdout, "Restored") {
		t.Fatalf("stdout missing 'Restored' confirmation: %q", res.stdout)
	}
}

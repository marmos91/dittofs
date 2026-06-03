package controlplane

import (
	"context"
	"runtime"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/api"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)

// guardPprofGlobals snapshots the process-global pprof sampling knobs and
// restores them after the test. api.NewServer (invoked by New when an API
// config is present) calls runtime.SetMutexProfileFraction /
// runtime.SetBlockProfileRate unconditionally; without this guard, constructing
// the server here would leak that mutation into other packages' tests when
// `go test ./...` runs them in the same process.
func guardPprofGlobals(t *testing.T) {
	t.Helper()
	prevMutex := runtime.SetMutexProfileFraction(-1) // -1 reads without changing
	runtime.SetMutexProfileFraction(prevMutex)       // restore the read-back value
	t.Cleanup(func() {
		runtime.SetMutexProfileFraction(prevMutex)
		runtime.SetBlockProfileRate(0)
	})
}

func memConfig() *store.Config {
	return &store.Config{
		Type:   store.DatabaseTypeSQLite,
		SQLite: store.SQLiteConfig{Path: ":memory:"},
	}
}

func TestNew_ValidationErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("nil options", func(t *testing.T) {
		if _, err := New(ctx, nil); err == nil {
			t.Error("expected error for nil options")
		}
	})

	t.Run("nil database config", func(t *testing.T) {
		if _, err := New(ctx, &Options{}); err == nil {
			t.Error("expected error for nil database config")
		}
	})

	t.Run("invalid database type", func(t *testing.T) {
		_, err := New(ctx, &Options{Database: &store.Config{Type: "bogus"}})
		if err == nil {
			t.Error("expected error for invalid database type")
		}
	})
}

// Happy path without the API server: store + runtime wired, API nil.
func TestNew_NoAPIServer(t *testing.T) {
	cp, err := New(context.Background(), &Options{Database: memConfig()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := cp.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	if cp.Store() == nil {
		t.Error("Store() is nil")
	}
	if cp.Runtime() == nil {
		t.Error("Runtime() is nil")
	}
	if cp.APIServer() != nil {
		t.Error("APIServer() should be nil when API not configured")
	}
	if cp.IdentityStore() == nil {
		t.Error("IdentityStore() is nil")
	}
}

// With an API config present, New constructs the API server and applies the
// default restore timeout when RestoreHTTPTimeout is zero.
func TestNew_WithAPIServer(t *testing.T) {
	guardPprofGlobals(t)

	apiCfg := &api.APIConfig{}
	apiCfg.ApplyDefaults()
	apiCfg.Port = 0 // ephemeral / unbound — New does not Start the server
	apiCfg.JWT.Secret = "test-secret-at-least-32-characters-long"

	cp, err := New(context.Background(), &Options{
		Database: memConfig(),
		API:      apiCfg,
		// RestoreHTTPTimeout left zero -> DefaultRestoreHTTPTimeout applied
	})
	if err != nil {
		t.Fatalf("New with API: %v", err)
	}
	t.Cleanup(func() {
		if err := cp.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	if cp.APIServer() == nil {
		t.Error("APIServer() should be non-nil when API is configured")
	}
}

func TestEnsureAdminUser(t *testing.T) {
	cp, err := New(context.Background(), &Options{Database: memConfig()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := cp.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	ctx := context.Background()
	pw, err := cp.EnsureAdminUser(ctx)
	if err != nil {
		t.Fatalf("EnsureAdminUser: %v", err)
	}
	if pw == "" {
		t.Error("expected a generated password on first creation")
	}
	// Idempotent: second call returns empty password (user already exists).
	pw2, err := cp.EnsureAdminUser(ctx)
	if err != nil {
		t.Fatalf("EnsureAdminUser (2nd): %v", err)
	}
	if pw2 != "" {
		t.Errorf("second EnsureAdminUser returned password %q, want empty", pw2)
	}
}

func TestClose(t *testing.T) {
	cp, err := New(context.Background(), &Options{Database: memConfig()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := cp.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

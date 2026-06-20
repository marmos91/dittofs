//go:build integration

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// sidBackend names a control-plane store backend the conformance test runs
// against. SQLite always runs; postgres runs only when the DSN is present.
type sidBackend struct {
	name string
	// open returns a freshly-opened store pointing at the SAME durable
	// database each time it is called, so a re-open simulates a process
	// restart. Callers must Close the returned store.
	open func(t *testing.T) *GORMStore
}

// sidBackends returns the backends to exercise. SQLite uses a file (NOT
// :memory:, which is wiped on close) so reopen tests observe durable state.
func sidBackends(t *testing.T) []sidBackend {
	t.Helper()
	backends := []sidBackend{
		{
			name: "sqlite",
			open: func(t *testing.T) *GORMStore {
				return openSQLiteFileStore(t)
			},
		},
	}

	if dsn := os.Getenv("DITTOFS_TEST_POSTGRES_DSN"); dsn != "" {
		cfg := postgresConfigFromDSN(t, dsn)
		// Clean the SID/identity tables once so a prior run can't leak rows.
		// Fail fast on cleanup errors rather than letting leftover rows cause
		// confusing downstream flakes.
		cleanup := openPostgresStore(t, cfg)
		if err := cleanup.db.Exec("DELETE FROM sid_mappings").Error; err != nil {
			t.Fatalf("cleanup sid_mappings: %v", err)
		}
		if err := cleanup.db.Exec("DELETE FROM users").Error; err != nil {
			t.Fatalf("cleanup users: %v", err)
		}
		if err := cleanup.Close(); err != nil {
			t.Fatalf("close cleanup store: %v", err)
		}
		backends = append(backends, sidBackend{
			name: "postgres",
			open: func(t *testing.T) *GORMStore {
				return openPostgresStore(t, cfg)
			},
		})
	}

	return backends
}

// openSQLiteFileStore opens a file-backed SQLite store under t.TempDir() so the
// same path can be reopened to simulate a restart. The file path is stashed on
// the test so repeated open() calls in one subtest hit the same DB.
func openSQLiteFileStore(t *testing.T) *GORMStore {
	t.Helper()
	dir := sqliteDirForTest(t)
	st, err := New(&Config{
		Type:   DatabaseTypeSQLite,
		SQLite: SQLiteConfig{Path: filepath.Join(dir, "controlplane.db")},
	})
	if err != nil {
		t.Fatalf("open sqlite file store: %v", err)
	}
	return st
}

// sqliteDirForTest returns a per-subtest stable temp dir. t.TempDir() returns a
// fresh dir per call, so cache the first one on a closure-scoped map keyed by
// the test pointer via a sub-helper.
var sqliteDirs = map[*testing.T]string{}

func sqliteDirForTest(t *testing.T) string {
	t.Helper()
	if d, ok := sqliteDirs[t]; ok {
		return d
	}
	d := t.TempDir()
	sqliteDirs[t] = d
	t.Cleanup(func() { delete(sqliteDirs, t) })
	return d
}

func openPostgresStore(t *testing.T, cfg *Config) *GORMStore {
	t.Helper()
	st, err := New(cfg)
	if err != nil {
		t.Fatalf("open postgres store: %v", err)
	}
	return st
}

// postgresConfigFromDSN parses a libpq key=value DSN into a store Config.
func postgresConfigFromDSN(t *testing.T, dsn string) *Config {
	t.Helper()
	pc := PostgresConfig{Host: "localhost", Port: 5432, SSLMode: "disable"}
	for _, field := range strings.Fields(dsn) {
		kv := strings.SplitN(field, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "host":
			pc.Host = kv[1]
		case "port":
			p, err := strconv.Atoi(kv[1])
			if err != nil {
				t.Fatalf("parse DSN port %q: %v", kv[1], err)
			}
			pc.Port = p
		case "dbname", "database":
			pc.Database = kv[1]
		case "user":
			pc.User = kv[1]
		case "password":
			pc.Password = kv[1]
		case "sslmode", "ssl_mode":
			pc.SSLMode = kv[1]
		}
	}
	if pc.Database == "" {
		t.Fatalf("DITTOFS_TEST_POSTGRES_DSN %q has no dbname", dsn)
	}
	return &Config{Type: DatabaseTypePostgres, Postgres: pc}
}

// TestSIDMapping_RoundTripAndRestart covers the AD-3 acceptance: a foreign SID
// binds to a stable UID/GID, round-trips, and survives a store reopen.
func TestSIDMapping_RoundTripAndRestart(t *testing.T) {
	ctx := context.Background()
	for _, be := range sidBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			st := be.open(t)

			const userSID = "S-1-5-21-111-222-333-1107"
			const groupSID = "S-1-5-21-111-222-333-2050"

			uMap, err := st.AllocateSIDMapping(ctx, userSID, 7001, false, "alice")
			if err != nil {
				t.Fatalf("allocate user SID: %v", err)
			}
			if uMap.UnixID != 7001 || uMap.IsGroup {
				t.Fatalf("user mapping wrong: %+v", uMap)
			}

			gMap, err := st.AllocateSIDMapping(ctx, groupSID, 8001, true, "eng")
			if err != nil {
				t.Fatalf("allocate group SID: %v", err)
			}
			if gMap.UnixID != 8001 || !gMap.IsGroup {
				t.Fatalf("group mapping wrong: %+v", gMap)
			}

			// Reopen the SAME durable store: mappings must reload unchanged.
			if err := st.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			st2 := be.open(t)
			defer st2.Close()

			got, err := st2.GetSIDMapping(ctx, userSID)
			if err != nil {
				t.Fatalf("get user SID after restart: %v", err)
			}
			if got.UnixID != 7001 || got.IsGroup {
				t.Errorf("user mapping changed after restart: %+v", got)
			}

			gotG, err := st2.GetSIDMapping(ctx, groupSID)
			if err != nil {
				t.Fatalf("get group SID after restart: %v", err)
			}
			if gotG.UnixID != 8001 || !gotG.IsGroup {
				t.Errorf("group mapping changed after restart: %+v", gotG)
			}
		})
	}
}

// TestSIDMapping_NeverRemap is the data-exposure invariant: re-allocating an
// existing foreign SID with a DIFFERENT UID returns the original, never remaps.
func TestSIDMapping_NeverRemap(t *testing.T) {
	ctx := context.Background()
	for _, be := range sidBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			st := be.open(t)
			defer st.Close()

			const sid = "S-1-5-21-9-8-7-1500"
			first, err := st.AllocateSIDMapping(ctx, sid, 5000, false, "")
			if err != nil {
				t.Fatalf("first allocate: %v", err)
			}

			// Attempt a remap to a different UID and to group=true.
			second, err := st.AllocateSIDMapping(ctx, sid, 6000, true, "evil")
			if err != nil {
				t.Fatalf("second allocate: %v", err)
			}
			if second.UnixID != first.UnixID {
				t.Errorf("SID remapped: first UID=%d second UID=%d (must be equal)", first.UnixID, second.UnixID)
			}
			if second.IsGroup != first.IsGroup {
				t.Errorf("SID class flipped: first IsGroup=%v second=%v", first.IsGroup, second.IsGroup)
			}

			got, err := st.GetSIDMapping(ctx, sid)
			if err != nil {
				t.Fatalf("get after remap attempt: %v", err)
			}
			if got.UnixID != 5000 || got.IsGroup {
				t.Errorf("stored mapping mutated: %+v (want UID=5000, group=false)", got)
			}
		})
	}
}

// TestSIDMapping_NotFoundAndDelete covers the sentinel + admin delete path.
func TestSIDMapping_NotFoundAndDelete(t *testing.T) {
	ctx := context.Background()
	for _, be := range sidBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			st := be.open(t)
			defer st.Close()

			if _, err := st.GetSIDMapping(ctx, "S-1-5-21-0-0-0-9999"); !errors.Is(err, models.ErrSIDMappingNotFound) {
				t.Errorf("missing SID err = %v, want ErrSIDMappingNotFound", err)
			}
			if err := st.DeleteSIDMapping(ctx, "S-1-5-21-0-0-0-9999"); !errors.Is(err, models.ErrSIDMappingNotFound) {
				t.Errorf("delete missing SID err = %v, want ErrSIDMappingNotFound", err)
			}

			const sid = "S-1-5-21-1-2-3-2200"
			if _, err := st.AllocateSIDMapping(ctx, sid, 4242, true, ""); err != nil {
				t.Fatalf("allocate: %v", err)
			}
			if err := st.DeleteSIDMapping(ctx, sid); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if _, err := st.GetSIDMapping(ctx, sid); !errors.Is(err, models.ErrSIDMappingNotFound) {
				t.Errorf("after delete err = %v, want ErrSIDMappingNotFound", err)
			}
		})
	}
}

// TestUserSIDPersistence covers the User.SID / User.GroupSIDs persistence
// acceptance: both survive create, update, and a store reopen.
func TestUserSIDPersistence(t *testing.T) {
	ctx := context.Background()
	for _, be := range sidBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			st := be.open(t)

			uid := uint32(4000)
			gid := uint32(4000)
			user := &models.User{
				Username:     "winuser",
				PasswordHash: "x",
				UID:          &uid,
				GID:          &gid,
				SID:          "S-1-5-21-50-60-70-9001",
				GroupSIDs:    []string{"S-1-5-21-50-60-70-9100", "S-1-5-21-50-60-70-9101"},
			}
			if _, err := st.CreateUser(ctx, user); err != nil {
				t.Fatalf("create user: %v", err)
			}

			// Reload after a restart and confirm SID/GroupSIDs persisted.
			if err := st.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			st2 := be.open(t)
			defer st2.Close()

			got, err := st2.GetUser(ctx, "winuser")
			if err != nil {
				t.Fatalf("get user after restart: %v", err)
			}
			if got.SID != "S-1-5-21-50-60-70-9001" {
				t.Errorf("SID not persisted: got %q", got.SID)
			}
			if len(got.GroupSIDs) != 2 ||
				got.GroupSIDs[0] != "S-1-5-21-50-60-70-9100" ||
				got.GroupSIDs[1] != "S-1-5-21-50-60-70-9101" {
				t.Errorf("GroupSIDs not persisted: got %v", got.GroupSIDs)
			}

			// UpdateUserSIDInfo persists a changed SID/GroupSIDs without
			// reloading or touching the rest of the user.
			if err := st2.UpdateUserSIDInfo(ctx, "winuser",
				"S-1-5-21-50-60-70-9999", []string{"S-1-5-21-50-60-70-9200"}); err != nil {
				t.Fatalf("update user SID info: %v", err)
			}
			reloaded, err := st2.GetUser(ctx, "winuser")
			if err != nil {
				t.Fatalf("get user after update: %v", err)
			}
			if reloaded.SID != "S-1-5-21-50-60-70-9999" {
				t.Errorf("updated SID not persisted: got %q", reloaded.SID)
			}
			if len(reloaded.GroupSIDs) != 1 || reloaded.GroupSIDs[0] != "S-1-5-21-50-60-70-9200" {
				t.Errorf("updated GroupSIDs not persisted: got %v", reloaded.GroupSIDs)
			}

			// A general UpdateUser with a partial user (no GroupSIDs) must NOT
			// erase the persisted Windows identity (F4 regression guard).
			profile, err := st2.GetUser(ctx, "winuser")
			if err != nil {
				t.Fatalf("get user before profile update: %v", err)
			}
			profile.GroupSIDs = nil
			profile.SID = ""
			profile.DisplayName = "Win User"
			if err := st2.UpdateUser(ctx, profile); err != nil {
				t.Fatalf("update user profile: %v", err)
			}
			afterProfile, err := st2.GetUser(ctx, "winuser")
			if err != nil {
				t.Fatalf("get user after profile update: %v", err)
			}
			if afterProfile.SID != "S-1-5-21-50-60-70-9999" {
				t.Errorf("profile UpdateUser clobbered SID: got %q", afterProfile.SID)
			}
			if len(afterProfile.GroupSIDs) != 1 {
				t.Errorf("profile UpdateUser clobbered GroupSIDs: got %v", afterProfile.GroupSIDs)
			}
		})
	}
}

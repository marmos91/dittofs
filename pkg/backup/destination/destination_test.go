package destination

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// factoryStub is a no-op Factory used as a placeholder in registry tests.
// Its sole purpose is to satisfy the Factory signature so Register/Lookup
// can be exercised without bringing up a real driver.
func factoryStub(_ context.Context, _ *models.BackupRepo) (Destination, error) {
	return nil, nil
}

// Compile-time proof that factoryStub matches the Factory signature. If
// the interface or Factory type drifts, this line breaks the build rather
// than producing a misleading runtime failure.
var _ Factory = factoryStub

// assertPanics fails the test if fn does not panic.
func assertPanics(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic, got none")
		}
	}()
	fn()
}

func TestRegister_HappyPath(t *testing.T) {
	ResetRegistryForTest()
	kind := models.BackupRepoKind("test")
	Register(kind, factoryStub)

	got, ok := Lookup(kind)
	if !ok {
		t.Fatalf("Lookup(%q) ok = false; want true", kind)
	}
	if got == nil {
		t.Fatalf("Lookup(%q) returned nil factory", kind)
	}
}

func TestRegister_DuplicatePanics(t *testing.T) {
	ResetRegistryForTest()
	kind := models.BackupRepoKind("test")
	Register(kind, factoryStub)
	assertPanics(t, func() {
		Register(kind, factoryStub)
	})
}

func TestRegister_EmptyKindPanics(t *testing.T) {
	ResetRegistryForTest()
	assertPanics(t, func() {
		Register(models.BackupRepoKind(""), factoryStub)
	})
}

func TestRegister_NilFactoryPanics(t *testing.T) {
	ResetRegistryForTest()
	assertPanics(t, func() {
		Register(models.BackupRepoKind("test"), nil)
	})
}

func TestLookup_Missing(t *testing.T) {
	ResetRegistryForTest()
	f, ok := Lookup(models.BackupRepoKind("nope"))
	if ok {
		t.Fatalf("Lookup on empty registry ok = true; want false")
	}
	if f != nil {
		t.Fatalf("Lookup on empty registry returned non-nil factory")
	}
}

// TestFactorySignature_Compiles is handled by the package-level
// `var _ Factory = factoryStub` above. This test records the intent and
// provides a named entry in `go test -v` output so future readers see the
// compile-time check is load-bearing. We additionally invoke the stub to
// exercise it at runtime (a non-nil panic-free call proves the signature
// lines up with Factory at invocation, not just at assignment).
func TestFactorySignature_Compiles(t *testing.T) {
	var f Factory = factoryStub
	got, err := f(context.Background(), nil)
	if err != nil {
		t.Fatalf("factoryStub err = %v; want nil", err)
	}
	if got != nil {
		t.Fatal("factoryStub returned non-nil Destination; want nil")
	}
}

// TestRegister_TypedKindKey proves the registry is keyed on the typed
// models.BackupRepoKind enum (not a bare string) — callers must be able
// to pass repo.Kind directly. Go does not auto-convert typed strings on
// map lookup, so this test breaks loudly if someone retypes the registry
// to map[string]Factory.
func TestRegister_TypedKindKey(t *testing.T) {
	ResetRegistryForTest()
	Register(models.BackupRepoKindLocal, factoryStub)

	got, ok := Lookup(models.BackupRepoKindLocal)
	if !ok {
		t.Fatalf("Lookup(BackupRepoKindLocal) ok = false; want true")
	}
	if got == nil {
		t.Fatal("Lookup(BackupRepoKindLocal) returned nil factory")
	}

	// Also verify a simulated caller pattern: repo.Kind in hand, pass it verbatim.
	repo := &models.BackupRepo{Kind: models.BackupRepoKindLocal}
	_, ok = Lookup(repo.Kind)
	if !ok {
		t.Fatal("Lookup(repo.Kind) ok = false; want true (callers must pass Kind verbatim)")
	}
}

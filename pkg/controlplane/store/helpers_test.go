package store

import (
	"context"
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// widget is a throwaway model used to exercise the generic GORM helpers without
// coupling the test to the real schema / migrations.
type widget struct {
	ID   string `gorm:"primaryKey"`
	Name string `gorm:"uniqueIndex"`
	Kind string
}

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	if err := db.AutoMigrate(&widget{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestDedup(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty", []string{}, []string{}},
		{"no dups", []string{"a", "b", "c"}, []string{"a", "b", "c"}},
		{"dups preserve order", []string{"b", "a", "b", "c", "a"}, []string{"b", "a", "c"}},
		{"all same", []string{"x", "x", "x"}, []string{"x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := dedup(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("len = %d, want %d (%v)", len(got), len(c.want), got)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("at %d: %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestConvertNotFoundError(t *testing.T) {
	sentinel := errors.New("widget not found")

	if got := convertNotFoundError(gorm.ErrRecordNotFound, sentinel); !errors.Is(got, sentinel) {
		t.Errorf("gorm.ErrRecordNotFound not converted to sentinel: %v", got)
	}
	// Wrapped gorm.ErrRecordNotFound must still convert (errors.Is unwraps).
	wrapped := errors.Join(errors.New("ctx"), gorm.ErrRecordNotFound)
	if got := convertNotFoundError(wrapped, sentinel); !errors.Is(got, sentinel) {
		t.Errorf("wrapped ErrRecordNotFound not converted: %v", got)
	}
	// Any other error passes through unchanged.
	other := errors.New("disk error")
	if got := convertNotFoundError(other, sentinel); got != other {
		t.Errorf("non-notfound error mutated: %v", got)
	}
}

func TestIsUniqueConstraintError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"sqlite", errors.New("UNIQUE constraint failed: widgets.name"), true},
		{"postgres", errors.New(`duplicate key value violates unique constraint "widgets_name_key"`), true},
		{"unrelated", errors.New("connection refused"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isUniqueConstraintError(c.err); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestGetByField(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	db.Create(&widget{ID: "1", Name: "alpha", Kind: "k"})

	notFound := errors.New("nope")

	got, err := getByField[widget](db, ctx, "name", "alpha", notFound)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "1" {
		t.Errorf("ID = %q, want 1", got.ID)
	}

	// Missing row maps to the supplied sentinel.
	_, err = getByField[widget](db, ctx, "name", "missing", notFound)
	if !errors.Is(err, notFound) {
		t.Errorf("missing row err = %v, want sentinel", err)
	}
}

func TestListAll(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Empty table returns a non-nil empty slice.
	got, err := listAll[widget](db, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Error("listAll returned nil, want empty slice")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}

	db.Create(&widget{ID: "1", Name: "a"})
	db.Create(&widget{ID: "2", Name: "b"})
	got, err = listAll[widget](db, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
}

func TestCreateWithID(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	dupErr := errors.New("dup widget")
	setID := func(w *widget, id string) { w.ID = id }

	t.Run("generates id when empty", func(t *testing.T) {
		w := &widget{Name: "gen"}
		id, err := createWithID[widget](db, ctx, w, setID, "", dupErr)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id == "" {
			t.Error("expected generated id")
		}
		if w.ID != id {
			t.Errorf("idSetter not applied: w.ID=%q id=%q", w.ID, id)
		}
	})

	t.Run("honors provided id", func(t *testing.T) {
		w := &widget{Name: "fixed"}
		id, err := createWithID[widget](db, ctx, w, setID, "fixed-id", dupErr)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "fixed-id" {
			t.Errorf("id = %q, want fixed-id", id)
		}
	})

	t.Run("unique violation maps to dupErr", func(t *testing.T) {
		// Fresh DB so prior subtests' rows can't interfere with the
		// uniqueness assertion.
		db := newTestDB(t)
		_, err := createWithID[widget](db, ctx, &widget{Name: "clash"}, setID, "a", dupErr)
		if err != nil {
			t.Fatalf("first create failed: %v", err)
		}
		// Same unique Name -> constraint violation -> dupErr.
		_, err = createWithID[widget](db, ctx, &widget{Name: "clash"}, setID, "b", dupErr)
		if !errors.Is(err, dupErr) {
			t.Errorf("err = %v, want dupErr", err)
		}
	})
}

func TestDeleteByField(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	notFound := errors.New("nope")

	db.Create(&widget{ID: "1", Name: "a", Kind: "x"})
	db.Create(&widget{ID: "2", Name: "b", Kind: "x"})
	db.Create(&widget{ID: "3", Name: "c", Kind: "y"})

	// Deletes both kind=x rows.
	if err := deleteByField[widget](db, ctx, "kind", "x", notFound); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	var remaining []widget
	db.Find(&remaining)
	if len(remaining) != 1 || remaining[0].Kind != "y" {
		t.Errorf("after delete, remaining = %+v", remaining)
	}

	// No matching rows -> notFound.
	if err := deleteByField[widget](db, ctx, "kind", "z", notFound); !errors.Is(err, notFound) {
		t.Errorf("delete of absent rows = %v, want notFound", err)
	}
}

package kvstore

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

func memKV(t *testing.T) kv.KV {
	t.Helper()
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}

func set(t *testing.T, db kv.KV, key, value []byte) {
	t.Helper()
	if err := db.Update(context.Background(), func(tx kv.Txn) error { return tx.Set(key, value) }); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRefusesUninitialised(t *testing.T) {
	db := memKV(t)
	set(t, db, []byte("other"), []byte("x"))
	if _, err := Open(context.Background(), db); !errors.Is(err, ErrNotInitialised) {
		t.Fatalf("Open: %v", err)
	}
}

func TestInitThenOpen(t *testing.T) {
	ctx := context.Background()
	db := memKV(t)
	install := uuid.New()
	if err := Init(ctx, db, install); err != nil {
		t.Fatal(err)
	}
	if err := Init(ctx, db, uuid.New()); !errors.Is(err, ErrInitialised) {
		t.Fatalf("second Init: %v", err)
	}
	// Open reads the format record and nothing else.
	c := &kv.Counting{KV: db}
	if _, err := Open(ctx, c); err != nil {
		t.Fatal(err)
	}
	if n := c.Take(); n != (kv.Counts{Gets: 1}) {
		t.Errorf("Open read %+v", n)
	}
	var in, sl []byte
	if err := db.View(ctx, func(r kv.Reader) error {
		var err error
		if in, err = r.Get(installationKey); err != nil {
			return err
		}
		sl, err = r.Get(shareListKey)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := decodeInstallation(in); err != nil || got.ID != install || got.Instance == got.ID {
		t.Errorf("installation %+v, %v", got, err)
	}
	if v, err := decodeCounter(sl); err != nil || v != 0 {
		t.Errorf("share list %d, %v", v, err)
	}
}

// Two racing initialisations: exactly one commits.
func TestInitRace(t *testing.T) {
	db := memKV(t)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = Init(context.Background(), db, uuid.New())
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrInitialised):
			t.Errorf("Init: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d initialisations committed", ok)
	}
}

func TestOpenRefusesFormat(t *testing.T) {
	for name, f := range map[string]format{
		"too new":      {Version: maxFormat + 1, Folds: knownFolds},
		"too old":      {Version: minFormat - 1, Folds: knownFolds},
		"unknown fold": {Version: maxFormat, Folds: []uint32{uint32(entity.FoldIdentity), 99}},
	} {
		t.Run(name, func(t *testing.T) {
			db := memKV(t)
			set(t, db, keys.Format, encodeFormat(f))
			if _, err := Open(context.Background(), db); !errors.Is(err, ErrFormat) {
				t.Fatalf("Open: %v", err)
			}
		})
	}
}

func TestFormatGolden(t *testing.T) {
	if got := encodeFormat(format{Version: 1, Folds: []uint32{1}}); string(got) != "\x01"+"\x00\x00\x00\x00\x00\x00\x00\x01"+"\x01"+"\x00\x00\x00\x01" {
		t.Errorf("format record %x", got)
	}
}

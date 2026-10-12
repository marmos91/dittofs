package kvstore

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/codec"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

var (
	ErrNotInitialised = errors.New("kvstore: store not initialised")
	ErrInitialised    = errors.New("kvstore: store already initialised")
	ErrFormat         = errors.New("kvstore: store format not supported")
)

// The store formats this binary reads and writes. Format n names the codec
// versions a binary may write in it, so raising the active format is the only
// way a newer codec version reaches the store.
const (
	minFormat = 1
	maxFormat = 1
)

// knownFolds is every fold rule this binary implements, by the ID the
// format record names it by.
var knownFolds = func() []uint32 {
	var ids []uint32
	for _, r := range entity.FoldRules {
		ids = append(ids, uint32(r))
	}
	return ids
}()

// format is the store format record: the active store format and the fold
// rules the store's shares use.
type format struct {
	Version uint64
	Folds   []uint32
}

func encodeFormat(f format) []byte {
	w := codec.NewWriter(v1).Uint64(f.Version).Len(len(f.Folds))
	for _, id := range f.Folds {
		w.Uint32(id)
	}
	return w.Encode()
}

func decodeFormat(b []byte) (format, error) {
	r, err := open(b)
	if err != nil {
		return format{}, err
	}
	f := format{Version: r.Uint64()}
	if n := r.Len(); n > 0 {
		f.Folds = make([]uint32, n)
		for i := range f.Folds {
			f.Folds[i] = r.Uint32()
		}
	}
	return f, r.Done()
}

// Store is the metadata store over one KV.
type Store struct {
	kv      kv.KV
	format  uint64
	numbers numbers
	bounds  boundCache
}

// Init creates the store: the format record at the newest format, the
// Installation record with a fresh identity, and ShareList at zero, in one
// transaction. It refuses a store that already has a format record, so two
// racing initialisations cannot both commit. Opening never initialises.
func Init(ctx context.Context, db kv.KV) error {
	return db.Update(ctx, func(tx kv.Txn) error {
		if _, err := tx.Get(keys.Format); err == nil {
			return ErrInitialised
		} else if !errors.Is(err, kv.ErrNotFound) {
			return err
		}
		in := entity.Installation{ID: uuid.New(), Instance: uuid.New()}
		for _, kvs := range [][2][]byte{
			{keys.Format, encodeFormat(format{Version: maxFormat, Folds: knownFolds})},
			{installationKey, encodeInstallation(in)},
			{shareListKey, encodeCounter(0)},
		} {
			if err := tx.Set(kvs[0], kvs[1]); err != nil {
				return err
			}
		}
		return nil
	})
}

// Open reads the format record before any other key and refuses a store
// whose format or fold rules this binary does not implement. It never
// rewrites the record: a store's format is raised only by the control plane's
// gate, so an older binary keeps opening it until then.
func Open(ctx context.Context, db kv.KV) (*Store, error) {
	var f format
	err := db.View(ctx, func(r kv.Reader) error {
		b, err := r.Get(keys.Format)
		if errors.Is(err, kv.ErrNotFound) {
			return ErrNotInitialised
		}
		if err != nil {
			return err
		}
		f, err = decodeFormat(b)
		return err
	})
	if err != nil {
		return nil, err
	}
	if f.Version < minFormat || f.Version > maxFormat {
		return nil, fmt.Errorf("%w: format %d, this binary reads %d to %d", ErrFormat, f.Version, minFormat, maxFormat)
	}
	for _, id := range f.Folds {
		if !slices.Contains(knownFolds, id) {
			return nil, fmt.Errorf("%w: fold rule %d", ErrFormat, id)
		}
	}
	return &Store{kv: db, format: f.Version}, nil
}

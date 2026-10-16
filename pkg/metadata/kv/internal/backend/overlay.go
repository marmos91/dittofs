// Package backend holds what the kv backends share above their storage: a
// transaction's own writes, merged into its reads, and store time.
package backend

import (
	"bytes"
	"slices"

	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

// Overlay is a transaction's own writes, which its reads see before the
// snapshot's records. A View has none; reads of a nil Overlay see only the
// snapshot.
type Overlay map[string]Write

// Write is one key's last write in the transaction.
type Write struct {
	Value   []byte
	Deleted bool
}

// Put records a write of key, keeping the slices it is given.
func (o Overlay) Put(key, value []byte, deleted bool) {
	o[string(key)] = Write{Value: value, Deleted: deleted}
}

// Has reports whether the transaction wrote key.
func (o Overlay) Has(key []byte) bool {
	_, ok := o[string(key)]
	return ok
}

// Cursor walks a snapshot's records in one direction. Key is valid until
// Next.
type Cursor interface {
	Valid() bool
	Key() []byte
	Value() ([]byte, error)
	Next()
}

// Scan yields the records under prefix after after, in key order: c's, which
// starts at the first stored key past after, merged with the transaction's
// own writes, an own write shadowing the stored key it names and an own
// deletion hiding it.
//
// ponytail: each call sorts the transaction's own writes under prefix,
// O(w log w) for w writes. Keep them in an ordered structure when a
// transaction that writes and scans many keys shows it in a profile.
func (o Overlay) Scan(prefix, after []byte, c Cursor, yield func(kv.KeyValue, error) bool) {
	var mine []string
	for k := range o {
		if bytes.HasPrefix([]byte(k), prefix) && (after == nil || k > string(after)) {
			mine = append(mine, k)
		}
	}
	slices.Sort(mine)

	for c.Valid() || len(mine) > 0 {
		var stored []byte
		if c.Valid() {
			stored = c.Key()
		}
		if len(mine) > 0 && (stored == nil || mine[0] <= string(stored)) {
			k := mine[0]
			mine = mine[1:]
			if stored != nil && k == string(stored) {
				c.Next()
			}
			if w := o[k]; !w.Deleted && !yield(kv.KeyValue{Key: []byte(k), Value: w.Value}, nil) {
				return
			}
			continue
		}
		v, err := c.Value()
		if err != nil {
			yield(kv.KeyValue{}, err)
			return
		}
		if !yield(kv.KeyValue{Key: stored, Value: v}, nil) {
			return
		}
		c.Next()
	}
}

// Before returns the last record under prefix below key: c walks the stored
// keys below key in reverse, and the transaction's own writes below key are
// merged in, the larger key winning and an own deletion hiding the stored key
// it names.
//
// ponytail: each call sorts the transaction's own writes under prefix, as
// Scan does. Keep them ordered when a transaction that writes and seeks many
// keys shows it in a profile.
func (o Overlay) Before(prefix, key []byte, c Cursor) (kv.KeyValue, bool, error) {
	var mine []string
	for k := range o {
		if bytes.HasPrefix([]byte(k), prefix) && k < string(key) {
			mine = append(mine, k)
		}
	}
	slices.Sort(mine)

	for c.Valid() || len(mine) > 0 {
		var stored []byte
		if c.Valid() {
			stored = c.Key()
		}
		if n := len(mine); n > 0 && (stored == nil || mine[n-1] >= string(stored)) {
			k := mine[n-1]
			mine = mine[:n-1]
			if stored != nil && k == string(stored) {
				c.Next()
			}
			if w := o[k]; !w.Deleted {
				return kv.KeyValue{Key: []byte(k), Value: w.Value}, true, nil
			}
			continue
		}
		v, err := c.Value()
		if err != nil {
			return kv.KeyValue{}, false, err
		}
		return kv.KeyValue{Key: bytes.Clone(stored), Value: v}, true, nil
	}
	return kv.KeyValue{}, false, nil
}

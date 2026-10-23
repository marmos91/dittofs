package openstate

import (
	"errors"
	"io"
	"sync"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// Deps is what the table is built over. Each further dependency of RFC 14's
// arrives with the piece that first reads it.
type Deps struct {
	Rand io.Reader // every random bit of a token; crypto/rand in production
}

// Table is the open state of every file this node is the primary of. Each
// file's state lives in its own entry, created with the file's first piece of
// state and freed with its last, so a file with none costs nothing.
type Table struct {
	d Deps

	// files is split 256 ways by a FileID's first byte, which is random, so
	// steps on different files rarely share a shard's mutex. A shard's mutex
	// is held only to find, add or free an entry, never while a step runs.
	files [256]fileShard

	// idx guards the state references. It is taken after a file's step,
	// never before one, and only to add, remove or look up a reference.
	idx  sync.Mutex
	refs map[refKey]holder
}

type fileShard struct {
	mu      sync.Mutex
	entries map[entity.FileID]*entry
}

// entry is one file's state. Its mutex is the file's open-state step: every
// call that reads or changes the file's state runs under it.
type entry struct {
	file   entity.FileID
	mu     sync.Mutex
	users  int // steps running or waiting on this entry, guarded by its file shard's mutex
	states int // pieces of state held, guarded by mu
}

// New builds an empty table. It starts nothing.
func New(d Deps) (*Table, error) {
	if d.Rand == nil {
		return nil, errors.New("openstate: a table needs a source of random bits")
	}
	t := &Table{d: d, refs: map[refKey]holder{}}
	for i := range t.files {
		t.files[i].entries = map[entity.FileID]*entry{}
	}
	return t, nil
}

// step runs fn under file f's step, creating the file's entry if it has none
// and freeing it afterwards if fn left it holding no state.
func (t *Table) step(f entity.FileID, fn func(e *entry) error) error {
	fs := &t.files[f[0]]
	fs.mu.Lock()
	e := fs.entries[f]
	if e == nil {
		e = &entry{file: f}
		fs.entries[f] = e
	}
	e.users++
	fs.mu.Unlock()

	e.mu.Lock()
	err := fn(e)
	e.mu.Unlock()

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if e.users--; e.users > 0 {
		return err
	}
	// No step holds or waits on the entry, so its mutex is free and nothing
	// can change its state while it is read here.
	e.mu.Lock()
	if e.states == 0 {
		delete(fs.entries, f)
	}
	e.mu.Unlock()
	return err
}

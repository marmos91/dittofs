package openstate

import (
	"sync"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// Table is the open state of every file this node is the primary of. Each
// file's state lives in its own entry, created with the file's first piece of
// state and freed with its last, so a file with none costs nothing.
type Table struct {
	// ponytail: one mutex guards the file map, held only to find, add or
	// free an entry, never while a step runs. Shard the map by FileID when a
	// profile shows contention on it under many files' steps at once.
	mu    sync.Mutex
	files map[entity.FileID]*entry

	// idx guards the state references and the shards' grace state. It is
	// taken after a file's step, never before one, and only to add, remove
	// or look up a reference.
	idx    sync.Mutex
	refs   map[refKey]holder
	shards map[ShardID]shardGrace
}

// entry is one file's state. Its mutex is the file's open-state step: every
// call that reads or changes the file's state runs under it.
type entry struct {
	mu     sync.Mutex
	users  int // steps running or waiting on this entry, guarded by Table.mu
	states int // pieces of state held, guarded by mu
}

func New() *Table {
	return &Table{
		files:  map[entity.FileID]*entry{},
		refs:   map[refKey]holder{},
		shards: map[ShardID]shardGrace{},
	}
}

// step runs fn under file f's step, creating the file's entry if it has none
// and freeing it afterwards if fn left it holding no state.
func (t *Table) step(f entity.FileID, fn func(e *entry) error) error {
	t.mu.Lock()
	e := t.files[f]
	if e == nil {
		e = &entry{}
		t.files[f] = e
	}
	e.users++
	t.mu.Unlock()

	e.mu.Lock()
	err := fn(e)
	e.mu.Unlock()

	t.mu.Lock()
	defer t.mu.Unlock()
	if e.users--; e.users > 0 {
		return err
	}
	// No step holds or waits on the entry, so its mutex is free and nothing
	// can change its state while it is read here.
	e.mu.Lock()
	if e.states == 0 {
		delete(t.files, f)
	}
	e.mu.Unlock()
	return err
}

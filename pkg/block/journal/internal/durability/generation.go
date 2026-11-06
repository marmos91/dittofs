package durability

import (
	"path/filepath"
	"sync"
)

// registry holds the per-directory state for the life of the process.
//
// decision: the loss generation lives here, keyed by directory, and not in
// a Durability: a reopen in one process must not hand a client a verifier it
// has seen, and a new Durability on a directory a process already opened
// continues that directory's count rather than starting at zero. The count
// is not persisted, so a restart of the process starts it over; a client
// that sees the verifier change across a restart already treats it as a
// reboot.
var registry = struct {
	mu   sync.Mutex
	dirs map[string]*dirState
}{dirs: map[string]*dirState{}}

// dirState is one directory's generation. Its mutex is a leaf: nothing is
// called while it is held.
//
// ponytail: an entry is one small struct per directory ever opened, and none
// is ever freed. Upgrade to freeing it when a directory is forgotten if a
// process ever opens thousands.
type dirState struct {
	mu  sync.Mutex
	gen uint64
}

// stateFor returns the state of dir, made on first use, keyed by its cleaned
// absolute path so two spellings of one directory share it.
func stateFor(dir string) (*dirState, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	key := filepath.Clean(abs)

	registry.mu.Lock()
	defer registry.mu.Unlock()
	st, ok := registry.dirs[key]
	if !ok {
		st = &dirState{}
		registry.dirs[key] = st
	}
	return st, nil
}

// raise raises the generation by one and returns it.
func (s *dirState) raise() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen++
	return s.gen
}

// load returns the generation.
func (s *dirState) load() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen
}

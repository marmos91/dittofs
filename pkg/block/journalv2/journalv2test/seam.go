package journalv2test

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/storage"
	"github.com/marmos91/dittofs/pkg/sim"
)

// The seam's operation names, as Calls and CallsFor count them.
const (
	OpCreate     = "create"
	OpOpen       = "open"
	OpAllocate   = "allocate"
	OpWrite      = "write"
	OpRead       = "read"
	OpReadDirect = "read_direct"
	OpSync       = "sync"
	OpTruncate   = "truncate"
	OpClose      = "close"
	OpRename     = "rename"
	OpRemove     = "remove"
	OpList       = "list"
	OpSyncDir    = "sync_dir"
)

// Seam is a storage.FS over a real directory that can simulate a crash.
//
// Every call reaches the real files. Alongside, the seam remembers what is not
// yet durable: per file, every WriteAt, Allocate and Truncate since that file's
// last Sync, and every Create, Rename and Remove since the last SyncDir. A
// change a failed Sync covered is lost: the file keeps serving it, but no
// later Sync makes it durable, so from then on the seam keeps that file's
// durable image and replays onto it what each later Sync covers. A failed
// SyncDir does the same for the directory: its changes are lost, and the seam
// keeps the durable directory and replays onto it what each later SyncDir
// covers. Crash puts every file back to its durable image and the directory
// back to its durable names, closes every open file, and leaves the directory
// as a power loss would have, so a journal can be opened on it again.
//
// A removed or replaced file is not unlinked while a crash could bring it
// back: it is moved to a stash directory beside dir, and unlinked by the
// first SyncDir after which the durable directory no longer holds it. The
// stash lives in dir's parent, which a test's temporary directory already
// cleans up.
//
// decision: Sync and SyncDir do not call fsync. Durability is the seam's
// record, not the device's, and nothing here survives a real power loss, so an
// fsync would only slow every test down.
type Seam struct {
	dir    string
	stash  string
	fault  sim.Fault
	marked string // a sim.Match pattern of the files whose writes MarkReorder marks; "" marks none
	seq    uint64
	names  map[string]*node // the directory as it is now
	dirLog []dirOp          // Create, Rename and Remove since the last SyncDir

	// dirSnap is the durable directory, kept from the first failed SyncDir
	// until the next Crash: the directory then holds changes no SyncDir will
	// make durable, so undoing dirLog no longer gives the durable names. Nil
	// otherwise.
	dirSnap map[string]*node

	open     map[*seamFile]struct{}
	calls    map[string]int
	callsFor map[[2]string]int
	stashed  int
	holds    []*Hold
	crashes  uint64 // Crash calls so far; a call that waited across one fails

	mu sync.Mutex
}

// node is one file, followed across renames: its undo log stays with it.
type node struct {
	path  string   // where the file is now, in dir or in the stash
	undo  []change // what its last Sync did not cover, oldest first
	stale bool     // ReadAt serves the durable image

	// snap is the file's durable image, kept once a change was lost: the
	// file then holds bytes no Sync will make durable, so undoing the
	// unsynced changes no longer gives the durable image. Nil otherwise.
	snap []byte
}

type changeKind int

const (
	changeWrite changeKind = iota + 1
	changeResize
)

// change is one unsynced operation on a file, with what it replaced.
type change struct {
	kind    changeKind
	seq     uint64
	off     int64  // write: where it landed
	data    []byte // write: the bytes written
	old     []byte // the bytes it overwrote or cut off, at off for a write and at the new size for a shrink
	oldSize int64  // the file's size before it
	newSize int64  // resize: the file's size after it
	lost    bool   // a failed Sync covered it, or it was dropped: no Sync ever makes it durable
	dropped bool   // write: it never reached the device, so no crash keeps it either
	reorder bool   // write: MarkReorder marked it for Reordered's draw
}

type dirOpKind int

const (
	dirCreate dirOpKind = iota + 1
	dirRename
	dirRemove
)

type dirOp struct {
	kind     dirOpKind
	node     *node
	name     string // create and remove: the name; rename: the new name
	oldName  string // rename: the old name
	replaced *node  // rename: the file newName held before, now in the stash
}

// NewSeam returns a seam over the existing directory dir. The files already
// in dir are taken as durable.
func NewSeam(dir string) (*Seam, error) {
	dir = filepath.Clean(dir) // a file's key compares its directory with dir
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	stash, err := os.MkdirTemp(filepath.Dir(dir), filepath.Base(dir)+".stash-")
	if err != nil {
		return nil, err
	}
	s := &Seam{
		dir:      dir,
		stash:    stash,
		names:    map[string]*node{},
		open:     map[*seamFile]struct{}{},
		calls:    map[string]int{},
		callsFor: map[[2]string]int{},
	}

	for _, e := range entries {
		s.names[e.Name()] = &node{path: filepath.Join(dir, e.Name())}
	}

	return s, nil
}

var _ storage.FS = (*Seam)(nil)

// FS returns a view of the seam for one process: a storage.FS that serves the
// directory as the seam does until the next Crash, and from then on fails
// every call with an error matching fs.ErrClosed and does nothing. A call it
// has in flight when the Crash runs fails the same way. Files opened through
// it are the seam's, which a Crash already closes.
//
// decision: a crash ends the process that made it, so nothing that process
// still runs may act on the directory the crash left. Without a view, a
// journal closed after a simulated crash could create, rename or sync files
// after the power cut. A test gives each journal it opens a view of its own,
// and the Seam itself always serves the current one.
func (s *Seam) FS() storage.FS {
	s.mu.Lock()
	defer s.mu.Unlock()
	return view{s: s, epoch: s.crashes}
}

// view is the storage.FS FS returns: the seam, bound to the crashes it had
// seen when the view was made.
type view struct {
	s     *Seam
	epoch uint64
}

var _ storage.FS = view{}

func (v view) Create(name string) (storage.File, error) { return v.s.createFrom(v.epoch, name) }
func (v view) Open(name string) (storage.File, error)   { return v.s.openFrom(v.epoch, name) }
func (v view) Rename(oldName, newName string) error     { return v.s.renameFrom(v.epoch, oldName, newName) }
func (v view) Remove(name string) error                 { return v.s.removeFrom(v.epoch, name) }
func (v view) List() ([]string, error)                  { return v.s.listFrom(v.epoch) }
func (v view) SyncDir() error                           { return v.s.syncDirFrom(v.epoch) }

// SetFault makes the seam consult f before every operation, and apply the
// results it returns. A nil f, the default, passes every call.
//
// Each operation is named by one of the Op constants, and its key is the name
// of the file it acts on, as the directory names it at the call: the old name
// for a rename, and empty for list and sync_dir. Size and Name are not
// operations and consult nothing. A call on a closed file fails with
// fs.ErrClosed before f is consulted, so it never counts in a rule. f is
// called without the seam's lock held, so a delay stalls only its own call.
// The delay is one f has already waited out, so the seam applies it as passed.
//
// How the seam applies the results:
//   - Zero, or a delay alone: performs the operation.
//   - A loss (tearAfter < 0) on write: reports the whole write done, and the
//     bytes reach the file, so ReadAt sees them, but they are lost: no Sync
//     makes them durable, and Torn and Reordered never keep them.
//   - A loss on sync or sync_dir: reports success and does nothing, so what
//     was pending stays pending and a later sync can make it durable.
//   - A tear (tearAfter > 0) on write: with err nil, a silent tear, performs
//     the write whole and reports it done, but only its first tearAfter bytes
//     can become durable: the rest is lost, as a lost write is. With err set
//     it is a short write: only the first tearAfter bytes land, and it returns
//     them and err. At or past the write's length nothing is torn.
//   - err alone: performs nothing and returns err. A failed sync also marks
//     every pending change of the file lost: ReadAt still serves it, and no
//     later Sync makes it durable, but a crash may still find it, as Torn and
//     Reordered may keep it. A failed sync_dir loses its directory changes
//     the same way: they stay in the directory, and no later SyncDir makes
//     them durable.
//
// A tear on any other op, or a loss on any other op or with err set, panics,
// naming it and the op: it is a mistake in the test, not a storage fault.
func (s *Seam) SetFault(f sim.Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fault = f
}

// MarkReorder marks every later write of a file whose name matches key, a
// pattern as sim.Match takes it, replacing any pattern set before; "" marks
// none. While any pending write is marked, Reordered draws only among the
// marked ones; a plain Crash and Torn ignore marks. A Sync that covers a
// write clears its mark.
func (s *Seam) MarkReorder(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marked = key
}

// isMarked reports whether a write of the file named key is marked; the
// caller holds mu.
func (s *Seam) isMarked(key string) bool {
	return s.marked != "" && sim.Match(s.marked, key)
}

// action is what the seam's fault decided for one call.
type action struct {
	err       error
	tearAfter int // > 0: tear a write after this many bytes; < 0: lose a write or a sync
}

func (a action) lost() bool { return a.tearAfter < 0 }

// torn reports whether a tears a write of n bytes.
func (a action) torn(n int) bool { return a.tearAfter > 0 && a.tearAfter < n }

// consult asks the seam's fault what to do with op on key, and panics on
// results the seam cannot apply to op. The caller must not hold mu, because a
// delay waits inside At.
func (s *Seam) consult(op, key string) action {
	s.mu.Lock()
	f := s.fault
	s.mu.Unlock()

	if f == nil {
		return action{}
	}

	_, err, tearAfter := f.At(op, []byte(key))
	switch {
	case tearAfter > 0 && op != OpWrite:
		panic(fmt.Sprintf("journalv2test: a tear on %s is not supported", op))
	case tearAfter < 0 && op != OpWrite && op != OpSync && op != OpSyncDir:
		panic(fmt.Sprintf("journalv2test: a loss on %s is not supported", op))
	case tearAfter < 0 && err != nil:
		panic(fmt.Sprintf("journalv2test: a loss on %s with an error is not supported", op))
	}

	return action{err: err, tearAfter: tearAfter}
}

// enter counts op on name, waits at any hold that matches it, then consults
// the fault. It returns the fault's action holding mu, unless the fault fails
// the call: then it returns the fault's error, without mu. A call from a
// process a Crash ended, one made after the crashes epoch counts or during
// which one ran while it was held or delayed, fails with an error matching
// fs.ErrClosed and does nothing: it must not act on the directory as the crash
// left it.
//
// A call refused on entry is not counted: its process no longer exists, so it
// is no call of the journal under test. A call on a file a Crash closed is
// counted, as file.go's enter does, because it is one the live process made.
func (s *Seam) enter(op, name string, epoch uint64) (action, error) {
	s.mu.Lock()
	if s.crashes != epoch {
		s.mu.Unlock()
		return action{}, &fs.PathError{Op: op, Path: name, Err: fs.ErrClosed}
	}
	s.count(op, name)
	s.mu.Unlock()

	s.hold(op, name)
	a := s.consult(op, name)

	s.mu.Lock()
	if s.crashes != epoch {
		s.mu.Unlock()
		return action{}, &fs.PathError{Op: op, Path: name, Err: fs.ErrClosed}
	}

	if a.err != nil && a.tearAfter == 0 {
		if op == OpSyncDir {
			s.loseDirLog()
		}

		s.mu.Unlock()
		return a, &fs.PathError{Op: op, Path: name, Err: a.err}
	}

	return a, nil
}

// count records one call of op on name; the caller holds mu.
func (s *Seam) count(op, name string) {
	s.calls[op]++
	s.callsFor[[2]string{op, name}]++
}

// Calls returns how many times op was called, on any file.
func (s *Seam) Calls(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[op]
}

// CallsFor returns how many times op was called on the file name.
func (s *Seam) CallsFor(op, name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.callsFor[[2]string{op, name}]
}

func (s *Seam) Create(name string) (storage.File, error) { return s.FS().Create(name) }

func (s *Seam) createFrom(epoch uint64, name string) (storage.File, error) {
	if _, err := s.enter(OpCreate, name, epoch); err != nil {
		return nil, err
	}
	defer s.mu.Unlock()

	path := filepath.Join(s.dir, name)
	f, err := openFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}

	n := &node{path: path}
	s.names[name] = n

	s.dirLog = append(s.dirLog, dirOp{kind: dirCreate, node: n, name: name})
	return s.track(f, n, name), nil
}

func (s *Seam) Open(name string) (storage.File, error) { return s.FS().Open(name) }

func (s *Seam) openFrom(epoch uint64, name string) (storage.File, error) {
	if _, err := s.enter(OpOpen, name, epoch); err != nil {
		return nil, err
	}
	defer s.mu.Unlock()

	n, ok := s.names[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}

	f, err := openFile(n.path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}

	return s.track(f, n, name), nil
}

func (s *Seam) track(f *os.File, n *node, name string) *seamFile {
	sf := &seamFile{seam: s, f: f, node: n, name: name}
	s.open[sf] = struct{}{}
	return sf
}

func (s *Seam) Rename(oldName, newName string) error { return s.FS().Rename(oldName, newName) }

func (s *Seam) renameFrom(epoch uint64, oldName, newName string) error {
	if _, err := s.enter(OpRename, oldName, epoch); err != nil {
		return err
	}
	defer s.mu.Unlock()

	n, ok := s.names[oldName]
	if !ok {
		return &fs.PathError{Op: "rename", Path: oldName, Err: fs.ErrNotExist}
	}

	if oldName == newName {
		return nil
	}

	var replaced *node
	if r, ok := s.names[newName]; ok {
		if err := s.toStash(r); err != nil {
			return err
		}
		replaced = r
	}

	path := filepath.Join(s.dir, newName)
	if err := os.Rename(n.path, path); err != nil {
		if replaced != nil {
			back := filepath.Join(s.dir, newName)
			if rerr := os.Rename(replaced.path, back); rerr != nil {
				return errors.Join(err, rerr)
			}

			replaced.path = back
		}

		return err
	}
	n.path = path
	delete(s.names, oldName)

	s.names[newName] = n
	s.dirLog = append(s.dirLog, dirOp{
		kind:     dirRename,
		node:     n,
		name:     newName,
		oldName:  oldName,
		replaced: replaced},
	)

	return nil
}

func (s *Seam) Remove(name string) error { return s.FS().Remove(name) }

func (s *Seam) removeFrom(epoch uint64, name string) error {
	if _, err := s.enter(OpRemove, name, epoch); err != nil {
		return err
	}
	defer s.mu.Unlock()

	n, ok := s.names[name]
	if !ok {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrNotExist}
	}
	if err := s.toStash(n); err != nil {
		return err
	}
	delete(s.names, name)
	s.dirLog = append(s.dirLog, dirOp{kind: dirRemove, node: n, name: name})
	return nil
}

// toStash moves n out of the directory without unlinking it; the caller
// holds mu.
func (s *Seam) toStash(n *node) error {
	s.stashed++
	path := filepath.Join(s.stash, fmt.Sprint(s.stashed))
	if err := os.Rename(n.path, path); err != nil {
		return err
	}
	n.path = path
	return nil
}

func (s *Seam) List() ([]string, error) { return s.FS().List() }

func (s *Seam) listFrom(epoch uint64) ([]string, error) {
	if _, err := s.enter(OpList, "", epoch); err != nil {
		return nil, err
	}
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

func (s *Seam) SyncDir() error { return s.FS().SyncDir() }

func (s *Seam) syncDirFrom(epoch uint64) error {
	a, err := s.enter(OpSyncDir, "", epoch)
	if err != nil {
		return err
	}
	defer s.mu.Unlock()

	if a.lost() {
		return nil
	}

	if s.dirSnap != nil {
		s.replayDirLog()
	}
	s.dirLog = nil

	// decision: SyncDir's result is the commit of the directory log, which cannot
	// fail here. Unlinking the stashed files the durable directory no longer
	// holds is cleanup, and what it leaves is removed by a later SyncDir or Crash.
	_ = s.emptyStash()
	return nil
}

// loseDirLog drops every pending directory change, after a SyncDir that
// failed. The first failure keeps the durable directory in dirSnap; a later
// one leaves dirSnap as it is. The caller holds mu.
//
// decision: a later success does not stand in for a SyncDir that failed, as
// for a file's Sync. The changes stay in the directory, so List and Open
// still see them, but only what is logged after the failure can become
// durable, so code that retries a failed SyncDir and trusts the retry is
// caught. The one exception is a lost rename that a later change depends on
// (see replayDirLog).
func (s *Seam) loseDirLog() {
	if s.dirSnap == nil {
		s.dirSnap = s.durableNames()
	}
	s.dirLog = nil
}

// replayDirLog applies the pending directory changes to dirSnap, oldest
// first, after a SyncDir that succeeded; dirSnap stays set. The caller holds
// mu.
//
// decision: a change is replayed by name, whatever the durable directory
// holds under that name: a create or a rename sets the name, a remove deletes
// it, and the file it removed under any name the durable directory gives it.
// Replaying only changes whose node the durable directory still holds would
// dead-end the re-append path: once the removal of an old file is lost, a new
// file under its name could never become durable, and removing the name again
// would act on the new file, which the durable directory never held. A node
// keeps one name, so a rename also takes it away from any name it held.
//
// decision: a node a replayed change takes a name from keeps a durable name
// while the directory still holds it: the one the directory holds it under
// now, given it by a rename the failure lost. A filesystem commits directory
// changes in order, so it cannot commit the change that reuses the name
// without the rename that freed it, and a renamed file survives a crash
// under one of its names, never under none. By the same order, a filesystem
// would commit every lost change before a later success; the seam keeps the
// others lost on purpose, as the worst case, so code that trusts a retried
// SyncDir is caught. That is a harness choice, not a filesystem state.
func (s *Seam) replayDirLog() {
	now := make(map[*node]string, len(s.names))
	for name, n := range s.names {
		now[n] = name
	}

	// keep gives the node under name, and any node it displaces in turn, the
	// name the directory holds it under now.
	keep := func(name string) {
		for n := s.dirSnap[name]; n != nil; {
			to, ok := now[n]
			if !ok || to == name {
				return
			}
			n, s.dirSnap[to] = s.dirSnap[to], n
		}
	}

	for _, op := range s.dirLog {
		switch op.kind {
		case dirCreate:
			keep(op.name)
			s.dirSnap[op.name] = op.node
		case dirRemove:
			maps.DeleteFunc(s.dirSnap, func(_ string, n *node) bool { return n == op.node })
			keep(op.name)
			delete(s.dirSnap, op.name)
		case dirRename:
			maps.DeleteFunc(s.dirSnap, func(_ string, n *node) bool { return n == op.node })
			keep(op.oldName)
			delete(s.dirSnap, op.oldName)
			keep(op.name)
			s.dirSnap[op.name] = op.node
		}
	}
}

// emptyStash unlinks every stashed file the durable directory does not hold,
// now that nothing can bring one back; the caller holds mu.
func (s *Seam) emptyStash() error {
	held := map[string]bool{}
	for _, n := range s.dirSnap {
		held[n.path] = true
	}

	entries, err := os.ReadDir(s.stash)
	if err != nil {
		return err
	}

	for _, e := range entries {
		path := filepath.Join(s.stash, e.Name())
		if held[path] {
			continue
		}

		if err := os.Remove(path); err != nil {
			return err
		}
	}

	return nil
}

// StaleReads makes every ReadAt of the file name, through a handle open now
// or later, serve the file's durable image instead of what was written since
// its last Sync: what a page cache that dropped the unsynced pages would
// return.
func (s *Seam) StaleReads(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if n, ok := s.names[name]; ok {
		n.stale = true
	}
}

// Durable returns the bytes a crash now would leave in the file name, and an
// error matching fs.ErrNotExist when a crash now would leave no such file.
func (s *Seam) Durable(name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n, ok := s.durableNames()[name]
	if !ok {
		return nil, &fs.PathError{Op: "durable", Path: name, Err: fs.ErrNotExist}
	}
	return n.durableImage()
}

// durableNames returns the directory as the last SyncDir left it: dirSnap
// when it is set, otherwise the current names with every pending change
// undone. The caller holds mu.
func (s *Seam) durableNames() map[string]*node {
	if s.dirSnap != nil {
		return maps.Clone(s.dirSnap)
	}

	names := make(map[string]*node, len(s.names))
	maps.Copy(names, s.names)

	for i := range slices.Backward(s.dirLog) {
		op := s.dirLog[i]

		switch op.kind {
		case dirCreate:
			delete(names, op.name)
		case dirRename:
			delete(names, op.name)
			names[op.oldName] = op.node

			if op.replaced != nil {
				names[op.name] = op.replaced
			}

		case dirRemove:
			names[op.name] = op.node
		}
	}

	return names
}

package journalv2test

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math/rand/v2"
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
	lost    bool   // a failed Sync covered it, so no Sync ever makes it durable
	reorder bool   // write: a Reorder fault marked it for Reordered's draw
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

// SetFault makes the seam consult f before every operation, and apply the
// action it returns. A nil f, the default, passes every call.
//
// Each operation is named by one of the Op constants, and its key is the name
// of the file it acts on: the old name for a rename, and empty for list and
// sync_dir. Size and Name are not operations and consult nothing. A call on a
// closed file fails with fs.ErrClosed before f is consulted, so it never
// counts in a rule. f is called without the seam's lock held, so a Delay
// stalls only its own call.
//
// How the seam applies each action:
//   - Pass, Delay: performs the operation.
//   - Drop on write: reports the whole write done, and the bytes reach the
//     file, so ReadAt sees them, but they are lost: no Sync makes them
//     durable, and Torn and Reordered never keep them.
//   - Drop on sync or sync_dir: reports success and does nothing, so what
//     was pending stays pending and a later sync can make it durable.
//   - Tear on write: writes only the first N bytes. With the action's Err
//     nil it reports the whole write done; with Err set it is a short write,
//     returning N and Err. With N at or past the write's length nothing is
//     torn, and the write is reported whole.
//   - Reorder on write: performs it and marks it. While any pending write
//     is marked, Reordered draws only among the marked ones; a plain Crash
//     and Torn ignore marks. A Sync that covers the write clears its mark.
//   - Fail: performs nothing and returns the action's Err. A failed sync
//     also marks every pending change of the file lost: ReadAt still
//     serves it, and no later Sync makes it durable. A failed sync_dir
//     loses its directory changes the same way: they stay in the directory,
//     and no later SyncDir makes them durable.
//
// Any other kind, or Drop, Tear or Reorder on any other op, panics, naming the
// kind and the op: it is a mistake in the test, not a storage fault.
func (s *Seam) SetFault(f sim.Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fault = f
}

// consult asks the seam's fault what to do with op on key, and panics on an
// action the seam cannot apply to op. The caller must not hold mu, because a
// Delay waits inside At.
func (s *Seam) consult(op, key string) sim.FaultAction {
	s.mu.Lock()
	f := s.fault
	s.mu.Unlock()

	if f == nil {
		return sim.FaultAction{}
	}

	a := f.At(op, []byte(key))
	if !applies(a.Kind, op) {
		panic(fmt.Sprintf("journalv2test: a %v fault on %s is not supported", a.Kind, op))
	}

	return a
}

// applies reports whether the seam can apply a fault of kind k to op.
func applies(k sim.FaultKind, op string) bool {
	switch k {
	case sim.Pass, sim.Delay, sim.Fail:
		return true
	case sim.Drop:
		return op == OpWrite || op == OpSync || op == OpSyncDir
	case sim.Tear, sim.Reorder:
		return op == OpWrite
	default:
		return false
	}
}

// enter counts op on name and consults the fault. It returns the fault's
// action holding mu, unless the fault fails the call: then it returns the
// fault's error, without mu.
func (s *Seam) enter(op, name string) (sim.FaultAction, error) {
	s.mu.Lock()
	s.count(op, name)
	s.mu.Unlock()

	a := s.consult(op, name)

	s.mu.Lock()
	if a.Kind == sim.Fail {
		if op == OpSyncDir {
			s.loseDirLog()
		}

		s.mu.Unlock()
		return a, &fs.PathError{Op: op, Path: name, Err: a.Err}
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

func (s *Seam) Create(name string) (storage.File, error) {
	if _, err := s.enter(OpCreate, name); err != nil {
		return nil, err
	}
	defer s.mu.Unlock()

	path := filepath.Join(s.dir, name)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}

	n := &node{path: path}
	s.names[name] = n

	s.dirLog = append(s.dirLog, dirOp{kind: dirCreate, node: n, name: name})
	return s.track(f, n, name), nil
}

func (s *Seam) Open(name string) (storage.File, error) {
	if _, err := s.enter(OpOpen, name); err != nil {
		return nil, err
	}
	defer s.mu.Unlock()

	n, ok := s.names[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}

	f, err := os.OpenFile(n.path, os.O_RDWR, 0)
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

func (s *Seam) Rename(oldName, newName string) error {
	if _, err := s.enter(OpRename, oldName); err != nil {
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

func (s *Seam) Remove(name string) error {
	if _, err := s.enter(OpRemove, name); err != nil {
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

func (s *Seam) List() ([]string, error) {
	if _, err := s.enter(OpList, ""); err != nil {
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

func (s *Seam) SyncDir() error {
	a, err := s.enter(OpSyncDir, "")
	if err != nil {
		return err
	}
	defer s.mu.Unlock()

	if a.Kind == sim.Drop {
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
// caught.
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
// it. Replaying only changes whose node the durable directory still holds
// would dead-end the re-append path: once the removal of an old file is lost,
// a new file under its name could never become durable, and removing the name
// again would act on the new file, which the durable directory never held. A
// node keeps one name, so a rename also takes it away from any name it held.
func (s *Seam) replayDirLog() {
	for _, op := range s.dirLog {
		switch op.kind {
		case dirCreate:
			s.dirSnap[op.name] = op.node
		case dirRemove:
			delete(s.dirSnap, op.name)
		case dirRename:
			maps.DeleteFunc(s.dirSnap, func(_ string, n *node) bool { return n == op.node })
			delete(s.dirSnap, op.oldName)
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

// CrashOption selects how Crash treats the writes it would otherwise drop.
type CrashOption func(*crashConfig)

type crashConfig struct {
	torn      bool
	tornBytes int
	reordered bool
	seed      uint64
}

// Torn keeps the last unsynced write, across every file, cut to its first n
// bytes. A write a failed Sync lost never counts. Every other unsynced
// operation is lost as usual.
func Torn(n int) CrashOption {
	return func(c *crashConfig) {
		c.torn, c.tornBytes = true, n
	}
}

// Reordered keeps a subset of the unsynced writes, chosen from seed, as a
// device that wrote them in another order and lost power part-way would. A
// write a failed Sync lost is never among them, and while any of them was
// marked by a Reorder fault, only the marked ones are. The kept writes land
// in the order they were issued, so a write never ends up under an older one
// it overlapped. Unsynced allocations, truncations and directory changes are
// lost as usual.
//
// decision: only data writes are reordered, as the format's crash model lists
// reordering for writes only. Directory changes since the last SyncDir are
// undone whole.
//
// ponytail: a filesystem may keep a prefix of those directory changes, for
// example a create without the rename after it. The seam keeps none. Add a
// crash option that keeps a seeded prefix if a test needs that state.
func Reordered(seed uint64) CrashOption {
	return func(c *crashConfig) {
		c.reordered, c.seed = true, seed
	}
}

// Crash simulates a power loss: it closes every open file, puts every file
// back to its durable image, and undoes every Create, Rename and Remove made
// since the last SyncDir or lost by a failed one. The files it keeps, and the
// only ones Torn and Reordered pick writes from, are those of the durable
// directory. With no option, every unsynced write is lost. After
// it returns, every call on a file opened before it fails with an error
// matching fs.ErrClosed, and the seam serves the directory as the crash left
// it. A Crash refused for its options returns an error and changes nothing.
func (s *Seam) Crash(opts ...CrashOption) error {
	var cfg crashConfig
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.torn && cfg.reordered {
		return errors.New("journalv2test: Torn and Reordered cannot be combined")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	survivors := s.durableNames()
	alive := make(map[*node]bool, len(survivors))
	for _, n := range survivors {
		alive[n] = true
	}

	var torn *change
	var tornNode *node
	if cfg.torn {
		for n := range alive {
			for i := range n.undo {
				c := &n.undo[i]
				if c.kind == changeWrite && !c.lost && (torn == nil || c.seq > torn.seq) {
					torn, tornNode = c, n
				}
			}
		}

		if torn == nil {
			return errors.New("journalv2test: Torn needs an unsynced write")
		}

		if cfg.tornBytes < 0 || cfg.tornBytes > len(torn.data) {
			return fmt.Errorf("journalv2test: Torn(%d) of a %d-byte write", cfg.tornBytes, len(torn.data))
		}
	}

	// Every refusal is above, so a refused Crash leaves every file open.
	for sf := range s.open {
		sf.closeLocked()
	}

	// The draws follow the order the writes were issued, never the map's, so
	// a seed keeps the same writes on every run.
	keep := map[*change]bool{}
	if cfg.reordered {
		var writes []*change

		for n := range alive {
			for i := range n.undo {
				if n.undo[i].kind == changeWrite && !n.undo[i].lost {
					writes = append(writes, &n.undo[i])
				}
			}
		}

		// ponytail: a mark narrows the draw to the marked writes and loses
		// every other one, which any device may do. It cannot express a device
		// that writes in order except for the marked writes: a seeded prefix of
		// the unmarked writes kept alongside a seeded subset of the marked
		// ones. Add that second draw when a check needs an earlier unmarked
		// write kept while a later marked one is lost.
		if slices.ContainsFunc(writes, func(c *change) bool { return c.reorder }) {
			writes = slices.DeleteFunc(writes, func(c *change) bool { return !c.reorder })
		}

		slices.SortFunc(writes, func(a, b *change) int {
			return cmp.Compare(a.seq, b.seq)
		})

		rng := rand.New(rand.NewPCG(cfg.seed, cfg.seed^0x9e3779b97f4a7c15))
		for _, c := range writes {
			keep[c] = rng.IntN(2) == 0
		}
	}

	for n := range alive {
		if len(n.undo) == 0 && n.snap == nil {
			continue
		}

		img, err := n.durableImage()
		if err != nil {
			return err
		}

		switch {
		case n == tornNode:
			img = writeInto(img, torn.off, torn.data[:cfg.tornBytes])

		case cfg.reordered:
			for i := range n.undo {
				if c := &n.undo[i]; keep[c] {
					img = writeInto(img, c.off, c.data)
				}
			}
		}

		if err := os.WriteFile(n.path, img, 0o644); err != nil {
			return err
		}

		n.undo, n.snap = nil, nil
	}

	restore := s.undoDir
	if s.dirSnap != nil {
		restore = func() error { return s.restoreDir(survivors) }
	}
	if err := restore(); err != nil {
		return err
	}

	s.names, s.dirSnap = survivors, nil
	for _, n := range survivors {
		n.undo, n.snap = nil, nil
	}

	return s.emptyStash()
}

// restoreDir makes the directory hold exactly names, the durable directory
// dirSnap keeps: every file now in it moves to the stash, and every durable
// one back under its name. Undoing dirLog cannot do it once a SyncDir failed,
// since the directory then holds changes the log no longer records. The
// caller holds mu.
func (s *Seam) restoreDir(names map[string]*node) error {
	for _, n := range s.names {
		if err := s.toStash(n); err != nil {
			return err
		}
	}

	for name, n := range names {
		path := filepath.Join(s.dir, name)
		if err := os.Rename(n.path, path); err != nil {
			return err
		}
		n.path = path
	}

	s.dirLog = nil
	return nil
}

// undoDir puts the directory back as the last SyncDir left it, newest change
// first; the caller holds mu.
func (s *Seam) undoDir() error {
	for i := range slices.Backward(s.dirLog) {
		op := s.dirLog[i]

		switch op.kind {
		case dirCreate:
			if err := os.Remove(op.node.path); err != nil {
				return err
			}
		case dirRename:
			old := filepath.Join(s.dir, op.oldName)
			if err := os.Rename(op.node.path, old); err != nil {
				return err
			}

			op.node.path = old
			if op.replaced != nil {
				back := filepath.Join(s.dir, op.name)

				if err := os.Rename(op.replaced.path, back); err != nil {
					return err
				}

				op.replaced.path = back
			}
		case dirRemove:
			back := filepath.Join(s.dir, op.name)
			if err := os.Rename(op.node.path, back); err != nil {
				return err
			}
			op.node.path = back
		}
	}

	s.dirLog = nil
	return nil
}

// durableImage returns the file's durable image: a copy of snap when it is
// set, otherwise the file's bytes, read by path, with every unsynced change
// undone.
func (n *node) durableImage() ([]byte, error) {
	if n.snap != nil {
		return slices.Clone(n.snap), nil
	}

	img, err := os.ReadFile(n.path)
	if err != nil {
		return nil, err
	}
	return n.undone(img), nil
}

// loseUndo marks every pending change lost, after a Sync that failed.
//
// decision: a later success does not stand in for a Sync that failed. A
// filesystem may drop the dirty pages a failed sync could not write and mark
// them clean, so a retried sync succeeds with the data gone; the seam does
// the same, so code that retries a failed sync and trusts the retry is caught.
// The pages stay readable, as the page cache keeps serving them.
func (n *node) loseUndo() {
	for i := range n.undo {
		n.undo[i].lost = true
	}
}

// sync makes every pending change of the file that is not lost durable.
//
// While no change was ever lost, the durable image is the file with its
// pending changes undone, so clearing them is enough. Once one is lost, the
// file holds bytes that must never become durable, so the durable image is
// kept in snap and each Sync replays onto it the changes it covers, in the
// order they were made.
func (sf *seamFile) sync() error {
	n := sf.node
	if n.snap == nil && !slices.ContainsFunc(n.undo, func(c change) bool { return c.lost }) {
		n.undo = nil
		return nil
	}

	img, err := sf.durableImage()
	if err != nil {
		return err
	}

	for _, c := range n.undo {
		if c.lost {
			continue
		}

		switch c.kind {
		case changeWrite:
			img = writeInto(img, c.off, c.data)
		case changeResize:
			img = resize(img, c.newSize)
		}
	}

	n.snap, n.undo = img, nil
	return nil
}

// undone returns img, the file's current bytes, with every unsynced change
// undone, newest first.
//
// ponytail: it works on the whole file on every call, which is fine for the
// small segments tests run with. Upgrade to undoing only the range asked for
// if a test reads large files through ReadAtDirect in a loop.
func (n *node) undone(img []byte) []byte {
	for i := range slices.Backward(n.undo) {
		c := n.undo[i]

		switch c.kind {
		case changeWrite:
			copy(img[c.off:], c.old)
		case changeResize:
			if int64(len(img)) < c.oldSize {
				img = resize(img, c.oldSize)
				copy(img[c.oldSize-int64(len(c.old)):], c.old)
			}
		}

		img = resize(img, c.oldSize)
	}
	return img
}

// resize returns b cut or zero-extended to size.
func resize(b []byte, size int64) []byte {
	if int64(len(b)) >= size {
		return b[:size]
	}

	return append(b, make([]byte, size-int64(len(b)))...)
}

// writeInto returns img with p written at off, extending it as a write past
// the end of a file does. An empty p leaves img as it is, as an empty write
// leaves a file's size alone.
func writeInto(img []byte, off int64, p []byte) []byte {
	if len(p) == 0 {
		return img
	}

	if end := off + int64(len(p)); end > int64(len(img)) {
		img = resize(img, end)
	}

	copy(img[off:], p)
	return img
}

// seamFile is one open file of a Seam.
type seamFile struct {
	seam   *Seam
	f      *os.File
	node   *node
	name   string
	closed bool
}

var _ storage.File = (*seamFile)(nil)

// enter is Seam.enter for a call on the file. A call on a closed file fails
// with ErrClosed before the fault is consulted, and again after it: a Crash
// may have closed the file while the fault delayed the call, and the call
// must not then act on the node a new handle is using.
func (sf *seamFile) enter(op string) (sim.FaultAction, error) {
	s := sf.seam

	s.mu.Lock()
	s.count(op, sf.name)
	closed := sf.closed
	s.mu.Unlock()

	if closed {
		return sim.FaultAction{}, sf.closedErr(op)
	}

	a := s.consult(op, sf.name)

	s.mu.Lock()
	if sf.closed {
		s.mu.Unlock()
		return sim.FaultAction{}, sf.closedErr(op)
	}

	if a.Kind == sim.Fail {
		if op == OpSync {
			sf.node.loseUndo()
		}

		s.mu.Unlock()
		return a, &fs.PathError{Op: op, Path: sf.name, Err: a.Err}
	}

	return a, nil
}

func (sf *seamFile) closedErr(op string) error {
	return &fs.PathError{Op: op, Path: sf.name, Err: fs.ErrClosed}
}

func (sf *seamFile) Name() string { return sf.name }

func (sf *seamFile) ReadAt(p []byte, off int64) (int, error) {
	if _, err := sf.enter(OpRead); err != nil {
		return 0, err
	}
	defer sf.seam.mu.Unlock()
	if sf.node.stale {
		return sf.readDurable(p, off)
	}
	return sf.f.ReadAt(p, off)
}

// ReadAtDirect serves the file's durable image.
//
// decision: reading past the page cache observes what reached the device, and
// what is known to have reached it is what a Sync covered. A device may hold
// more, but a test cannot rely on that, so the seam returns the durable image.
func (sf *seamFile) ReadAtDirect(p []byte, off int64) (int, error) {
	if _, err := sf.enter(OpReadDirect); err != nil {
		return 0, err
	}
	defer sf.seam.mu.Unlock()
	return sf.readDurable(p, off)
}

// durableImage is node.durableImage read through the open file rather than
// by path, so a file removed or replaced while open, whose path may no longer
// exist, still has one.
func (sf *seamFile) durableImage() ([]byte, error) {
	if sf.node.snap != nil {
		return slices.Clone(sf.node.snap), nil
	}

	size, err := sf.size()
	if err != nil {
		return nil, err
	}

	img := make([]byte, size)
	if _, err := sf.f.ReadAt(img, 0); err != nil {
		return nil, err
	}
	return sf.node.undone(img), nil
}

// readDurable reads through the open file rather than by path, so a file
// removed or replaced while open still serves its durable image.
func (sf *seamFile) readDurable(p []byte, off int64) (int, error) {
	// The error os.File.ReadAt returns for a negative offset.
	if off < 0 {
		return 0, &fs.PathError{Op: "readat", Path: sf.name, Err: errors.New("negative offset")}
	}

	img, err := sf.durableImage()
	if err != nil {
		return 0, err
	}

	if off >= int64(len(img)) {
		return 0, io.EOF
	}

	n := copy(p, img[off:])
	if n < len(p) {
		return n, io.EOF
	}

	return n, nil
}

func (sf *seamFile) WriteAt(p []byte, off int64) (int, error) {
	a, err := sf.enter(OpWrite)
	if err != nil {
		return 0, err
	}
	defer sf.seam.mu.Unlock()

	// A torn write lands, and is recorded, only up to its tear.
	data := p
	if a.Kind == sim.Tear && a.N < int64(len(p)) {
		data = p[:a.N]
	}

	size, err := sf.size()
	if err != nil {
		return 0, err
	}

	var old []byte
	if off < size {
		old = make([]byte, min(int64(len(data)), size-off))
		if _, err := sf.f.ReadAt(old, off); err != nil {
			return 0, err
		}
	}

	// decision: a failed write records the part of data the file's size after
	// the call allows, not data[:n]. os.File.WriteAt reports n == 0 when a write
	// fails part-way, even if some of it reached the file, and a byte left
	// unrecorded would survive a crash. Recording a byte that never landed is
	// harmless: its undo restores what old holds, which was there anyway.
	//
	// A dropped write is recorded lost: it reaches the file, as a write the
	// device acknowledged and never stored, so ReadAt sees it and no Sync
	// makes it durable.
	n, err := sf.f.WriteAt(data, off)
	landed := int64(len(data))
	if err != nil {
		after, serr := sf.size()
		if serr != nil {
			after = size
		}
		landed = min(landed, max(max(size, after)-off, 0))
	}

	if landed > 0 {
		sf.record(change{
			kind:    changeWrite,
			off:     off,
			data:    append([]byte(nil), data[:landed]...),
			old:     old[:min(int64(len(old)), landed)],
			oldSize: size,
			lost:    a.Kind == sim.Drop,
			reorder: a.Kind == sim.Reorder,
		})
	}

	if err != nil {
		return n, err
	}

	// A torn write reports itself whole, as a device that acknowledged it
	// and kept only part would, unless it carries an error: then it is a
	// write that failed part-way.
	if a.Kind == sim.Tear && a.Err != nil && a.N < int64(len(p)) {
		return n, &fs.PathError{Op: OpWrite, Path: sf.name, Err: a.Err}
	}

	return len(p), nil
}

// Allocate extends the file to cover the n bytes at off, as sparse space that
// reads as zeros. It never shrinks the file.
//
// decision: allocation is a sparse extension, not fallocate, because the
// test components may use the standard library only. A full device is a
// fault the test injects, not something this call reaches.
func (sf *seamFile) Allocate(off, n int64) error {
	if _, err := sf.enter(OpAllocate); err != nil {
		return err
	}
	defer sf.seam.mu.Unlock()

	size, err := sf.size()
	if err != nil {
		return err
	}

	if off+n <= size {
		return nil
	}

	sf.record(change{kind: changeResize, oldSize: size, newSize: off + n})
	return sf.f.Truncate(off + n)
}

func (sf *seamFile) Truncate(size int64) error {
	if _, err := sf.enter(OpTruncate); err != nil {
		return err
	}
	defer sf.seam.mu.Unlock()

	cur, err := sf.size()
	if err != nil {
		return err
	}

	var old []byte
	if size < cur {
		old = make([]byte, cur-size)
		if _, err := sf.f.ReadAt(old, size); err != nil {
			return err
		}
	}

	if err := sf.f.Truncate(size); err != nil {
		return err
	}

	sf.record(change{
		kind:    changeResize,
		old:     old,
		oldSize: cur,
		newSize: size,
	})

	return nil
}

func (sf *seamFile) Sync() error {
	a, err := sf.enter(OpSync)
	if err != nil {
		return err
	}
	defer sf.seam.mu.Unlock()

	if a.Kind == sim.Drop {
		return nil
	}

	return sf.sync()
}

func (sf *seamFile) Size() (int64, error) {
	sf.seam.mu.Lock()
	defer sf.seam.mu.Unlock()

	if sf.closed {
		return 0, sf.closedErr("size")
	}

	return sf.size()
}

// Close releases the file, even when the fault fails it.
//
// decision: a failed close still releases the handle, as close(2) on Linux
// frees the descriptor whatever it returns. Keeping the handle would show
// up as a descriptor leak in the code under test that the seam caused.
func (sf *seamFile) Close() error {
	_, err := sf.enter(OpClose)
	if err == nil {
		defer sf.seam.mu.Unlock()
		return sf.closeLocked()
	}

	sf.seam.mu.Lock()
	defer sf.seam.mu.Unlock()

	if !sf.closed {
		sf.closeLocked()
	}

	return err
}

// closeLocked closes the file. The caller holds the seam's mu.
func (sf *seamFile) closeLocked() error {
	sf.closed = true
	delete(sf.seam.open, sf)
	return sf.f.Close()
}

func (sf *seamFile) size() (int64, error) {
	st, err := sf.f.Stat()

	if err != nil {
		return 0, err
	}

	return st.Size(), nil
}

func (sf *seamFile) record(c change) {
	sf.seam.seq++
	c.seq = sf.seam.seq
	sf.node.undo = append(sf.node.undo, c)
}

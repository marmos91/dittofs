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
// last Sync, and every Create, Rename and Remove since the last SyncDir. Crash
// discards exactly those, closes every open file, and leaves the directory as
// a power loss would have, so a journal can be opened on it again.
//
// A removed or replaced file is not unlinked until the next SyncDir: it is
// moved to a stash directory beside dir, so a crash can bring it back. The
// stash lives in dir's parent, which a test's temporary directory already
// cleans up.
//
// decision: Sync and SyncDir do not call fsync. Durability is the seam's
// record, not the device's, and nothing here survives a real power loss, so an
// fsync would only slow every test down.
type Seam struct {
	dir   string
	stash string

	mu       sync.Mutex
	seq      uint64
	names    map[string]*node // the directory as it is now
	dirLog   []dirOp          // Create, Rename and Remove since the last SyncDir
	open     map[*seamFile]struct{}
	calls    map[string]int
	callsFor map[[2]string]int
	stashed  int
}

// node is one file, followed across renames: its undo log stays with it.
type node struct {
	path  string   // where the file is now, in dir or in the stash
	undo  []change // what its last Sync did not cover, oldest first
	stale bool     // ReadAt serves the durable image
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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count(OpCreate, name)

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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count(OpOpen, name)

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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count(OpRename, oldName)

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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count(OpRemove, name)

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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count(OpList, "")

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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count(OpSyncDir, "")

	s.dirLog = nil
	return s.emptyStash()
}

// emptyStash unlinks every stashed file, now that nothing can bring one
// back; the caller holds mu.
func (s *Seam) emptyStash() error {
	entries, err := os.ReadDir(s.stash)
	if err != nil {
		return err
	}

	for _, e := range entries {
		if err := os.Remove(filepath.Join(s.stash, e.Name())); err != nil {
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

// durableNames returns the directory as the last SyncDir left it; the caller
// holds mu.
func (s *Seam) durableNames() map[string]*node {
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
// bytes. Every other unsynced operation is lost as usual.
func Torn(n int) CrashOption {
	return func(c *crashConfig) {
		c.torn, c.tornBytes = true, n
	}
}

// Reordered keeps a subset of the unsynced writes, chosen from seed, as a
// device that wrote them in another order and lost power part-way would. The
// kept writes land in the order they were issued, so a write never ends up
// under an older one it overlapped. Unsynced allocations, truncations and
// directory changes are lost as usual.
//
// decision: only data writes are reordered. Directory changes are undone
// whole, because a filesystem's metadata journal does not apply them out of
// order.
func Reordered(seed uint64) CrashOption {
	return func(c *crashConfig) {
		c.reordered, c.seed = true, seed
	}
}

// Crash simulates a power loss: it closes every open file, puts every file
// back to its durable image, and undoes every Create, Rename and Remove made
// since the last SyncDir. With no option, every unsynced write is lost. After
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
				if c.kind == changeWrite && (torn == nil || c.seq > torn.seq) {
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
				if n.undo[i].kind == changeWrite {
					writes = append(writes, &n.undo[i])
				}
			}
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
		if len(n.undo) == 0 {
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

		n.undo = nil
	}

	if err := s.undoDir(); err != nil {
		return err
	}

	s.names = survivors
	for _, n := range survivors {
		n.undo = nil
	}

	return s.emptyStash()
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

// durableImage returns the file's bytes, read by path, with every unsynced
// change undone.
func (n *node) durableImage() ([]byte, error) {
	img, err := os.ReadFile(n.path)
	if err != nil {
		return nil, err
	}
	return n.undone(img), nil
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

// begin counts op and returns ErrClosed for a closed file. The caller holds
// the seam's mu.
func (sf *seamFile) begin(op string) error {
	sf.seam.count(op, sf.name)

	if sf.closed {
		return &fs.PathError{
			Op:   op,
			Path: sf.name,
			Err:  fs.ErrClosed,
		}
	}

	return nil
}

func (sf *seamFile) Name() string { return sf.name }

func (sf *seamFile) ReadAt(p []byte, off int64) (int, error) {
	sf.seam.mu.Lock()
	defer sf.seam.mu.Unlock()
	if err := sf.begin(OpRead); err != nil {
		return 0, err
	}
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
	sf.seam.mu.Lock()
	defer sf.seam.mu.Unlock()
	if err := sf.begin(OpReadDirect); err != nil {
		return 0, err
	}
	return sf.readDurable(p, off)
}

// readDurable reads through the open file rather than by path, so a file
// removed or replaced while open still serves its durable image.
func (sf *seamFile) readDurable(p []byte, off int64) (int, error) {
	size, err := sf.size()
	if err != nil {
		return 0, err
	}

	img := make([]byte, size)
	if _, err := sf.f.ReadAt(img, 0); err != nil {
		return 0, err
	}
	img = sf.node.undone(img)

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
	sf.seam.mu.Lock()
	defer sf.seam.mu.Unlock()
	if err := sf.begin(OpWrite); err != nil {
		return 0, err
	}

	size, err := sf.size()
	if err != nil {
		return 0, err
	}

	var old []byte
	if off < size {
		old = make([]byte, min(int64(len(p)), size-off))
		if _, err := sf.f.ReadAt(old, off); err != nil {
			return 0, err
		}
	}

	// decision: an empty write is not recorded: it changes nothing, and past the
	// end of the file its undo would have nothing to restore. Any other write is
	// recorded whole, even if it fails part-way, because os.File.WriteAt reports
	// n == 0 when a partial write then fails. The undo stays exact (old holds only
	// bytes that existed before); Torn and Reordered may replay bytes a failed
	// write never landed. Narrow it if tests come to rely on failed writes.
	if len(p) > 0 {
		sf.record(change{kind: changeWrite, off: off, data: append([]byte(nil), p...), old: old, oldSize: size})
	}

	return sf.f.WriteAt(p, off)
}

// Allocate extends the file to cover the n bytes at off, as sparse space that
// reads as zeros. It never shrinks the file.
//
// decision: allocation is a sparse extension, not fallocate, because the
// test components may use the standard library only. A full device is a
// fault the test injects, not something this call reaches.
func (sf *seamFile) Allocate(off, n int64) error {
	sf.seam.mu.Lock()
	defer sf.seam.mu.Unlock()

	if err := sf.begin(OpAllocate); err != nil {
		return err
	}

	size, err := sf.size()
	if err != nil {
		return err
	}

	if off+n <= size {
		return nil
	}

	sf.record(change{kind: changeResize, oldSize: size})
	return sf.f.Truncate(off + n)
}

func (sf *seamFile) Truncate(size int64) error {
	sf.seam.mu.Lock()
	defer sf.seam.mu.Unlock()

	if err := sf.begin(OpTruncate); err != nil {
		return err
	}

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

	sf.record(change{kind: changeResize, old: old, oldSize: cur})
	return sf.f.Truncate(size)
}

func (sf *seamFile) Sync() error {
	sf.seam.mu.Lock()
	defer sf.seam.mu.Unlock()

	if err := sf.begin(OpSync); err != nil {
		return err
	}

	sf.node.undo = nil
	return nil
}

func (sf *seamFile) Size() (int64, error) {
	sf.seam.mu.Lock()
	defer sf.seam.mu.Unlock()

	if sf.closed {
		return 0, &fs.PathError{Op: "size", Path: sf.name, Err: fs.ErrClosed}
	}

	return sf.size()
}

func (sf *seamFile) Close() error {
	sf.seam.mu.Lock()
	defer sf.seam.mu.Unlock()

	if err := sf.begin(OpClose); err != nil {
		return err
	}
	return sf.closeLocked()
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

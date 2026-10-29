package journalv2test

import (
	"cmp"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
)

// CrashOption selects how Crash treats the writes it would otherwise drop.
type CrashOption func(*crashConfig)

type crashConfig struct {
	torn      bool
	tornBytes int
	tornName  string // TornIn: the only file Torn picks from
	tornEnd   bool   // TornEnd: tornBytes counts from the end of the write
	reordered bool
	seed      uint64
	keeping   bool
	mask      uint64
}

// ErrCrashRefused is wrapped by the error of a Crash refused for its options:
// a bit of Keeping past the last unsynced write, a cut past the write Torn
// picked, or options that cannot be combined. A refused crash changes
// nothing.
var ErrCrashRefused = errors.New("journalv2test: the crash is refused")

// Torn keeps the last unsynced write, across every file, cut to its first n
// bytes. A dropped write never counts; one a failed Sync covered does. Every
// other unsynced operation is lost as usual.
func Torn(n int) CrashOption {
	return func(c *crashConfig) {
		c.torn, c.tornBytes = true, n
	}
}

// TornIn is Torn restricted to one file: the file called name in the
// directory as the last SyncDir left it. Its last unsynced write is cut to n
// bytes, however much newer a write to another file is, and every other
// unsynced write is lost as usual. It is for a check that tears one file
// while the code under test may write others in the background.
//
// It names the file by its durable name, unlike holds, faults and call
// counts, which use the name the directory gives it now. The two differ only
// for a rename no SyncDir has made durable.
func TornIn(name string, n int) CrashOption {
	return func(c *crashConfig) {
		c.torn, c.tornBytes, c.tornName = true, n, name
	}
}

// TornEnd is TornIn with the cut counted from the end: the last unsynced
// write of the file called name survives but its last n bytes. It is for a
// check that cuts inside the tail of a write whose length it does not know,
// such as the trailer a seal writes last.
func TornEnd(name string, n int) CrashOption {
	return func(c *crashConfig) {
		c.torn, c.tornBytes, c.tornName, c.tornEnd = true, n, name, true
	}
}

// Keeping keeps exactly the unsynced writes whose bit is set in mask, bit i
// for the i-th unsynced write in the order they were issued across every file
// of the durable directory, as Unsynced counts them, and loses the others. A
// dropped write is never among them. The kept writes land in the order they
// were issued. Marks set by a Reorder fault are ignored: Keeping names the
// subset itself. A bit set past the last unsynced write refuses the crash.
//
// decision: Reordered draws a subset from a seed, which cannot name the
// subset it drew, so a check that must visit every subset of the unsynced
// writes needs this option to enumerate them.
func Keeping(mask uint64) CrashOption {
	return func(c *crashConfig) {
		c.keeping, c.mask = true, mask
	}
}

// Reordered keeps a subset of the unsynced writes, chosen from seed, as a
// device that wrote them in another order and lost power part-way would. A
// dropped write is never among them, one a failed Sync covered may be, and
// while any of them was marked by a Reorder fault, only the marked ones are.
// The kept writes land in the order they were issued, so a write never ends
// up under an older one it overlapped. Unsynced allocations, truncations and
// directory changes are lost as usual.
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
		return fmt.Errorf("%w: Torn and Reordered cannot be combined", ErrCrashRefused)
	}

	if cfg.keeping && (cfg.torn || cfg.reordered) {
		return fmt.Errorf("%w: Keeping cannot be combined with Torn or Reordered", ErrCrashRefused)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	survivors := s.durableNames()
	alive := make(map[*node]bool, len(survivors))
	for _, n := range survivors {
		alive[n] = true
	}

	keep := map[*change]bool{}
	if cfg.keeping {
		writes := unsyncedWrites(alive)
		if len(writes) < 64 && cfg.mask>>uint(len(writes)) != 0 {
			return fmt.Errorf("%w: Keeping(%#x) names a write past the %d unsynced writes", ErrCrashRefused, cfg.mask, len(writes))
		}

		for i, c := range writes {
			keep[c] = i < 64 && cfg.mask&(1<<i) != 0
		}
	}

	var torn *change
	var tornNode *node
	if cfg.torn {
		for name, n := range survivors {
			if cfg.tornName != "" && name != cfg.tornName {
				continue
			}
			for i := range n.undo {
				c := &n.undo[i]
				if c.kind == changeWrite && !c.dropped && (torn == nil || c.seq > torn.seq) {
					torn, tornNode = c, n
				}
			}
		}

		if torn == nil {
			return fmt.Errorf("%w: Torn needs an unsynced write in a file it may pick from", ErrCrashRefused)
		}

		if cfg.tornBytes < 0 || cfg.tornBytes > len(torn.data) {
			option := "Torn"
			if cfg.tornEnd {
				option = "TornEnd"
			}
			return fmt.Errorf("%w: %s(%d) of a %d-byte write", ErrCrashRefused, option, cfg.tornBytes, len(torn.data))
		}

		if cfg.tornEnd {
			cfg.tornBytes = len(torn.data) - cfg.tornBytes
		}
	}

	// Every refusal is above, so a refused Crash leaves every file open.
	//
	// decision: a close error is dropped. A crashed process never sees what
	// close returns, and returning here would leave the crash half done, with
	// some files closed and no undo applied.
	//
	// ponytail: dropping it assumes a failed close still releases the file.
	// Report it if a close can ever fail without releasing the file, which
	// would leave it open across a crash.
	for sf := range s.open {
		_ = sf.closeLocked()
	}
	s.crashes++

	// The draws follow the order the writes were issued, never the map's, so
	// a seed keeps the same writes on every run.
	if cfg.reordered {
		writes := unsyncedWrites(alive)

		// ponytail: a mark narrows the draw to the marked writes and loses
		// every other one, which any device may do. It cannot express a device
		// that writes in order except for the marked writes: a seeded prefix of
		// the unmarked writes kept alongside a seeded subset of the marked
		// ones. Add that second draw when a check needs an earlier unmarked
		// write kept while a later marked one is lost.
		if slices.ContainsFunc(writes, func(c *change) bool { return c.reorder }) {
			writes = slices.DeleteFunc(writes, func(c *change) bool { return !c.reorder })
		}

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

		case cfg.reordered || cfg.keeping:
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

// unsyncedWrites returns the unsynced writes of the files in alive that a
// crash may keep, dropped writes left out, in the order they were issued.
func unsyncedWrites(alive map[*node]bool) []*change {
	var writes []*change

	for n := range alive {
		for i := range n.undo {
			if n.undo[i].kind == changeWrite && !n.undo[i].dropped {
				writes = append(writes, &n.undo[i])
			}
		}
	}

	slices.SortFunc(writes, func(a, b *change) int {
		return cmp.Compare(a.seq, b.seq)
	})

	return writes
}

// Unsynced returns how many unsynced writes a crash may keep now, across
// every file of the durable directory: the writes Keeping names by bit.
func (s *Seam) Unsynced() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	alive := map[*node]bool{}
	for _, n := range s.durableNames() {
		alive[n] = true
	}

	return len(unsyncedWrites(alive))
}

// Abandon ends the process using the seam without a power loss, as a kill
// does: it closes every open file and stops serving every view made before,
// as Crash does, and changes nothing else. Every file and the directory stay
// as they are, unsynced changes included, as the operating system keeps a
// dead process's writes in its cache. The durable images do not change, so
// what no sync covered is still lost by a later Crash, and a read past the
// cache still serves only what was synced.
func (s *Seam) Abandon() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for sf := range s.open {
		_ = sf.closeLocked() // a dead process never sees what close returns
	}
	s.crashes++
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

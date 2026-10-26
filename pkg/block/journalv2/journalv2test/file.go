package journalv2test

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"slices"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/storage"
	"github.com/marmos91/dittofs/pkg/sim"
)

// loseUndo marks every pending change lost, after a Sync that failed.
//
// decision: a later success does not stand in for a Sync that failed. A
// filesystem may drop the dirty pages a failed sync could not write and mark
// them clean, so a retried sync succeeds with the data gone; the seam does
// the same, so code that retries a failed sync and trusts the retry is caught.
// The pages stay readable, as the page cache keeps serving them.
//
// decision: a lost change is still unsynced, so a crash may find it: the
// failed sync may have written some pages before it failed. A plain Crash
// loses it, and Torn and Reordered may keep it, as any other unsynced write.
// A dropped write never reached the device, so no crash keeps it.
//
// ponytail: a later successful Sync of the file clears the undo log, so from
// then on no crash finds the lost change, though a device might still hold
// it. The journal takes no further append on a failed segment, not even a
// seal, so it has no reason to sync it again, and the check that a bad copy
// does not mask a good one can be built without it. Keep lost changes past
// that Sync, landing each under any newer synced bytes, if a check needs a
// crash to find them after one.
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
// with ErrClosed before it is held or the fault is consulted, and again after
// both: a Crash may have closed the file while the call was held or delayed,
// and the call must not then act on the node a new handle is using.
func (sf *seamFile) enter(op string) (sim.FaultAction, error) {
	s := sf.seam

	s.mu.Lock()
	s.count(op, sf.name)
	closed := sf.closed
	s.mu.Unlock()

	if closed {
		return sim.FaultAction{}, sf.closedErr(op)
	}

	s.hold(op, sf.name)
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

	// A short write, a tear with an error, lands only up to its tear.
	data := p
	if a.Kind == sim.Tear && a.Err != nil && a.N < int64(len(p)) {
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
	//
	// decision: a silent tear tears the write's durability, not the write. The
	// acknowledged write lands whole in the page cache, so Size and ReadAt see
	// all of it; its first N bytes are recorded as a write and the rest as a
	// dropped one, so a Sync makes only the head durable and a crash leaves
	// the file at its old size or the head's end, whichever is larger. Like
	// Drop, it is a device that acknowledges what it never stores, a fault
	// beyond the crash-time losses of the format's crash model.
	n, err := sf.f.WriteAt(data, off)
	landed := int64(len(data))
	if err != nil {
		after, serr := sf.size()
		if serr != nil {
			after = size
		}
		landed = min(landed, max(max(size, after)-off, 0))
	}

	tear := landed
	if a.Kind == sim.Tear && a.Err == nil {
		tear = min(a.N, landed)
	}

	if tear > 0 {
		sf.record(change{
			kind:    changeWrite,
			off:     off,
			data:    append([]byte(nil), data[:tear]...),
			old:     old[:min(int64(len(old)), tear)],
			oldSize: size,
			lost:    a.Kind == sim.Drop,
			dropped: a.Kind == sim.Drop,
			reorder: a.Kind == sim.Reorder,
		})
	}

	if tear < landed {
		tailSize := size // the file's size before the tail landed
		if tear > 0 {
			tailSize = max(size, off+tear)
		}

		sf.record(change{
			kind:    changeWrite,
			off:     off + tear,
			data:    append([]byte(nil), data[tear:landed]...),
			old:     old[min(int64(len(old)), tear):min(int64(len(old)), landed)],
			oldSize: tailSize,
			lost:    true,
			dropped: true,
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

	// The injected error is the one the caller sees; the real close only
	// releases the handle, as the decision above requires.
	if !sf.closed {
		_ = sf.closeLocked()
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

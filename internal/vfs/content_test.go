package vfs

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// fakeContent is Content over bytes in memory: every write is stable at once,
// nothing is ever lost, and a copy or clone is a copy of bytes. Nothing it
// stages ever commits, so a file it wrote or truncated stays pending in its
// overlay. It stands in for the engine until the engine is built, so the
// filesystem's tests prove what the filesystem orders, never what the engine
// keeps.
type fakeContent struct {
	mu       sync.Mutex
	files    map[entity.FileID][]byte
	shares   map[entity.FileID]entity.ShareID // the share each file was first named with
	overlays map[entity.FileID]Overlay
	versions uint64 // the last version drawn: store time, or one past the last, as the store draws its records'
	verifier vfs.Verifier
	// truncating, when set, runs as Truncate is called, before it stages
	// anything.
	truncating func(entity.FileID)
	// truncErr, when set, is every Truncate's answer, staging nothing.
	truncErr error
	// overlayErr, when set, is every Overlay's answer.
	overlayErr error
	// overlaying, when set, runs as Overlay is called, before it answers.
	overlaying func(entity.FileID)
	// deleteErr, when set, is every Delete's answer, dropping nothing.
	deleteErr error
	// writing and dealloc, when set, run as Write and Deallocate are
	// called, before either stages anything.
	writing, dealloc func(entity.FileID)
	// deallocErr, when set, is every Deallocate's answer, staging nothing.
	deallocErr error
	// writeErr and commitErr, when set, are every Write's and every
	// Commit's answer, staging and syncing nothing.
	writeErr, commitErr error
	// synced is the verifier Commit answers, sampled after its sync; loss is
	// every file's loss sequence.
	synced vfs.Verifier
	loss   uint64
	// commits is every Commit's extent, in order.
	commits []entity.Extent
	// committing, when set, runs as Commit is called, before it syncs.
	committing func(entity.FileID)
	// existence, when set, runs as CommitExistence is called, and is its
	// answer; otherwise nothing commits.
	existence func(entity.FileID) error
	// readErr, when set, is answered by every Read after the bytes it read.
	readErr error
	// reading, when set, runs as Read is called, before it reads anything.
	reading func(entity.FileID)
	// hold, when set, keeps each accepted write's Done in dones, uncalled,
	// as for a write whose commit has not landed; otherwise Write calls it.
	hold  bool
	dones []func()
	// pending, when set, is called by each Delete: a non-nil channel it
	// answers leaves that release pending on it, dropping nothing.
	pending func() <-chan error
	// stall, when set, makes each Delete wait for its context's end or for
	// stall to close, then answer the context's error.
	stall   chan struct{}
	deletes int // how many Deletes were called
}

var _ Content = (*fakeContent)(nil)

// errNegative is the fake's answer to a negative offset or length, which
// the filesystem never passes it.
var errNegative = errors.New("fake content: a negative offset or length")

func newFakeContent() *fakeContent {
	return &fakeContent{files: map[entity.FileID][]byte{}, shares: map[entity.FileID]entity.ShareID{}, overlays: map[entity.FileID]Overlay{}, verifier: vfs.Verifier{1}, synced: vfs.Verifier{1}}
}

// placed answers a Delay with DelayFrozen, as the engine does for a share
// with no binding, for a call naming no share or a share other than the one
// f was first named with, so a caller passing the wrong share fails its test.
// The caller holds mu.
func (c *fakeContent) placed(share entity.ShareID, f entity.FileID) error {
	if share == (entity.ShareID{}) {
		return &vfs.Delay{Cause: vfs.DelayFrozen}
	}
	if was, ok := c.shares[f]; ok && was != share {
		return &vfs.Delay{Cause: vfs.DelayFrozen}
	}
	c.shares[f] = share
	return nil
}

func (c *fakeContent) Draw(ctx context.Context, share entity.ShareID, f entity.FileID) (entity.JournalVersion, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.placed(share, f); err != nil {
		return entity.JournalVersion{}, err
	}
	c.versions = max(c.versions+1, uint64(time.Now().UnixNano()))
	return entity.VersionOf(c.versions), nil
}

func (c *fakeContent) Write(ctx context.Context, share entity.ShareID, f entity.FileID, off int64, p []byte, o WriteOpts) (vfs.Verifier, error) {
	if c.writing != nil {
		c.writing(f)
	}
	if c.writeErr != nil {
		return vfs.Verifier{}, c.writeErr
	}
	if off < 0 {
		return vfs.Verifier{}, errNegative
	}
	c.mu.Lock()
	if err := c.placed(share, f); err != nil {
		c.mu.Unlock()
		return vfs.Verifier{}, err
	}
	b := grow(c.files[f], off+int64(len(p)))
	copy(b[off:], p)
	c.files[f] = b
	ov := c.staged(f)
	ov.Size = max(ov.Size, off+int64(len(p)))
	c.overlays[f] = ov
	if c.hold {
		c.dones = append(c.dones, o.Done)
		o.Done = nil
	}
	c.mu.Unlock()
	if o.Done != nil {
		o.Done()
	}
	return c.verifier, nil
}

func (c *fakeContent) Commit(ctx context.Context, share entity.ShareID, f entity.FileID, off, n int64) (vfs.Verifier, uint64, error) {
	if c.committing != nil {
		c.committing(f)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.placed(share, f); err != nil {
		return vfs.Verifier{}, 0, err
	}
	if c.commitErr != nil {
		return vfs.Verifier{}, 0, c.commitErr
	}
	c.commits = append(c.commits, entity.Extent{Offset: off, Length: n})
	return c.synced, c.loss, nil
}

// CommitExistence runs existence, and once it succeeds calls the Done of
// every write held before it ran, as the commit charging them would,
// leaving held those of writes staged while it ran.
func (c *fakeContent) CommitExistence(ctx context.Context, share entity.ShareID, f entity.FileID) error {
	if c.existence == nil {
		return nil
	}
	c.mu.Lock()
	if err := c.placed(share, f); err != nil {
		c.mu.Unlock()
		return err
	}
	n := len(c.dones)
	c.mu.Unlock()
	if err := c.existence(f); err != nil {
		return err
	}
	c.mu.Lock()
	done := c.dones[:n:n]
	c.dones = c.dones[n:]
	c.mu.Unlock()
	for _, d := range done {
		d()
	}
	return nil
}

func (c *fakeContent) LossSeq(share entity.ShareID, f entity.FileID) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loss
}

func (c *fakeContent) Read(ctx context.Context, share entity.ShareID, f entity.FileID, off int64, dst []byte) (int, error) {
	if c.reading != nil {
		c.reading(f)
	}
	if off < 0 {
		return 0, errNegative
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.placed(share, f); err != nil {
		return 0, err
	}
	b := c.files[f]
	if off >= int64(len(b)) {
		return 0, c.readErr
	}
	return copy(dst, b[off:]), c.readErr
}

func (c *fakeContent) Truncate(ctx context.Context, share entity.ShareID, f entity.FileID, size int64) error {
	if c.truncating != nil {
		c.truncating(f)
	}
	if c.truncErr != nil {
		return c.truncErr
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.placed(share, f); err != nil {
		return err
	}
	c.files[f] = grow(c.files[f], size)[:size]
	o := c.staged(f)
	o.Size, o.SizeSet = size, true
	c.overlays[f] = o
	return nil
}

// staged is f's overlay with one more operation staged: a version drawn, and
// the times now. The caller holds mu.
func (c *fakeContent) staged(f entity.FileID) Overlay {
	c.versions = max(c.versions+1, uint64(time.Now().UnixNano()))
	o := c.overlays[f]
	o.Mtime, o.Ctime = time.Now(), time.Now()
	o.Version = entity.VersionOf(c.versions)
	return o
}

func (c *fakeContent) Deallocate(ctx context.Context, share entity.ShareID, f entity.FileID, off, n int64) error {
	if c.dealloc != nil {
		c.dealloc(f)
	}
	if c.deallocErr != nil {
		return c.deallocErr
	}
	if off < 0 || n < 0 {
		return errNegative
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.placed(share, f); err != nil {
		return err
	}
	b := c.files[f]
	clear(b[min(off, int64(len(b))):min(off+n, int64(len(b)))])
	return nil
}

func (c *fakeContent) Delete(ctx context.Context, share entity.ShareID, f entity.FileID) (<-chan error, error) {
	c.mu.Lock()
	stall := c.stall
	c.mu.Unlock()
	if stall != nil {
		select {
		case <-ctx.Done():
		case <-stall:
		}
		return nil, ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletes++
	if err := c.placed(share, f); err != nil {
		return nil, err
	}
	if c.deleteErr != nil {
		return nil, c.deleteErr
	}
	if c.pending != nil {
		if retry := c.pending(); retry != nil {
			return retry, nil
		}
	}
	delete(c.files, f)
	return nil, nil
}

// deleted is how many Deletes were called.
func (c *fakeContent) deleted() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deletes
}

func (c *fakeContent) Clone(ctx context.Context, srcShare entity.ShareID, src entity.FileID, dstShare entity.ShareID, dst entity.FileID, x vfs.Extent) error {
	_, err := c.Copy(ctx, srcShare, src, dstShare, dst, []vfs.Extent{x})
	return err
}

func (c *fakeContent) Copy(ctx context.Context, srcShare entity.ShareID, src entity.FileID, dstShare entity.ShareID, dst entity.FileID, xs []vfs.Extent) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.placed(srcShare, src); err != nil {
		return 0, err
	}
	if err := c.placed(dstShare, dst); err != nil {
		return 0, err
	}
	var n int64
	for _, x := range xs {
		from := grow(c.files[src], x.SrcOff+x.Len)
		to := grow(c.files[dst], x.DstOff+x.Len)
		copy(to[x.DstOff:x.DstOff+x.Len], from[x.SrcOff:])
		c.files[dst] = to
		n += x.Len
	}
	return n, nil
}

func (c *fakeContent) Overlay(ctx context.Context, share entity.ShareID, f entity.FileID) (Overlay, bool, error) {
	if c.overlaying != nil {
		c.overlaying(f)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.placed(share, f); err != nil {
		return Overlay{}, false, err
	}
	if c.overlayErr != nil {
		return Overlay{}, false, c.overlayErr
	}
	o, ok := c.overlays[f]
	return o, ok, nil
}

// Allocation is the run of f's bytes from off that are all zero, a hole, or
// all not, data; past the bytes held, a hole to the end.
func (c *fakeContent) Allocation(ctx context.Context, share entity.ShareID, f entity.FileID, off int64) (entity.Span, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.placed(share, f); err != nil {
		return entity.Span{}, err
	}
	b := c.files[f]
	if off >= int64(len(b)) {
		return entity.Span{Extent: entity.Extent{Offset: off, Length: 1<<62 - off}, Class: entity.Hole}, nil
	}
	end := off
	for end < int64(len(b)) && (b[end] == 0) == (b[off] == 0) {
		end++
	}
	class := entity.Carved
	if b[off] == 0 {
		class = entity.Hole
	}
	return entity.Span{Extent: entity.Extent{Offset: off, Length: end - off}, Class: class}, nil
}

// grow is b, extended with zeros to at least n bytes.
func grow(b []byte, n int64) []byte {
	if n > int64(len(b)) {
		b = append(b, make([]byte, n-int64(len(b)))...)
	}
	return b
}

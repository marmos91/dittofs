package vfs

import (
	"context"
	"sync"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/vfs"
)

// fakeContent is Content over bytes in memory: every write is stable at once,
// nothing is ever lost, and a copy or clone is a copy of bytes. It stages no
// overlay: no test reads one yet. It stands in for the engine until the
// engine is built, so the filesystem's tests prove what the filesystem
// orders, never what the engine keeps.
type fakeContent struct {
	mu       sync.Mutex
	files    map[entity.FileID][]byte
	versions uint64
	verifier vfs.Verifier
}

var _ Content = (*fakeContent)(nil)

func newFakeContent() *fakeContent {
	return &fakeContent{files: map[entity.FileID][]byte{}, verifier: vfs.Verifier{1}}
}

func (c *fakeContent) Draw(ctx context.Context, f entity.FileID) (entity.JournalVersion, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.versions++
	return entity.VersionOf(c.versions), nil
}

func (c *fakeContent) Write(ctx context.Context, f entity.FileID, off int64, p []byte, o WriteOpts) (vfs.Verifier, error) {
	c.mu.Lock()
	b := grow(c.files[f], off+int64(len(p)))
	copy(b[off:], p)
	c.files[f] = b
	c.mu.Unlock()
	if o.Done != nil {
		o.Done()
	}
	return c.verifier, nil
}

func (c *fakeContent) Commit(ctx context.Context, f entity.FileID, off, n int64) (vfs.Verifier, uint64, error) {
	return c.verifier, 0, nil
}

func (c *fakeContent) LossSeq(f entity.FileID) uint64 { return 0 }

func (c *fakeContent) Read(ctx context.Context, f entity.FileID, off int64, dst []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.files[f]
	if off >= int64(len(b)) {
		return 0, nil
	}
	return copy(dst, b[off:]), nil
}

func (c *fakeContent) Truncate(ctx context.Context, f entity.FileID, size int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.files[f] = grow(c.files[f], size)[:size]
	return nil
}

func (c *fakeContent) Deallocate(ctx context.Context, f entity.FileID, off, n int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.files[f]
	clear(b[min(off, int64(len(b))):min(off+n, int64(len(b)))])
	return nil
}

func (c *fakeContent) Delete(ctx context.Context, f entity.FileID) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.files, f)
	return nil
}

func (c *fakeContent) Clone(ctx context.Context, src, dst entity.FileID, x vfs.Extent) error {
	_, err := c.Copy(ctx, src, dst, []vfs.Extent{x})
	return err
}

func (c *fakeContent) Copy(ctx context.Context, src, dst entity.FileID, xs []vfs.Extent) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
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

func (c *fakeContent) Overlay(ctx context.Context, f entity.FileID) (Overlay, bool, error) {
	return Overlay{}, false, nil
}

func (c *fakeContent) Allocation(ctx context.Context, f entity.FileID, off int64) (entity.Span, error) {
	return entity.Span{}, nil
}

// grow is b, extended with zeros to at least n bytes.
func grow(b []byte, n int64) []byte {
	if n > int64(len(b)) {
		b = append(b, make([]byte, n-int64(len(b)))...)
	}
	return b
}

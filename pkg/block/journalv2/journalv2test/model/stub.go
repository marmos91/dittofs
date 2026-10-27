package model

import (
	"context"
	"errors"
	"iter"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

// StubError is a journal call answered with ErrNotImplemented: a call not
// written yet, which says nothing about whether the journal agrees with the
// model, so it is never shrunk.
type StubError struct{ Call string }

func (e StubError) Error() string { return "stub: " + e.Call }

// stubs records the first journal call of a step that was a stub. Every call
// the runner makes, Compare's reads included, goes through it, so the step's
// failure is reported as that stub whatever the model made of the answer.
type stubs struct{ call string }

func (s *stubs) seen(call string, err error) {
	if s.call == "" && errors.Is(err, journalv2.ErrNotImplemented) {
		s.call = call
	}
}

type stubHandle struct {
	Handle
	s *stubs
}

func (h stubHandle) WriteAt(ctx context.Context, id journalv2.FileID, off int64, p []byte, mtime int64) (journalv2.Version, error) {
	v, err := h.Handle.WriteAt(ctx, id, off, p, mtime)
	h.s.seen("WriteAt", err)
	return v, err
}

func (h stubHandle) Sync(ctx context.Context, ids ...journalv2.FileID) error {
	err := h.Handle.Sync(ctx, ids...)
	h.s.seen("Sync", err)
	return err
}

func (h stubHandle) ReadAt(ctx context.Context, id journalv2.FileID, off int64, p []byte) (int, []journalv2.Extent, []journalv2.Held, journalv2.Version, error) {
	n, missing, held, asOf, err := h.Handle.ReadAt(ctx, id, off, p)
	h.s.seen("ReadAt", err)
	return n, missing, held, asOf, err
}

func (h stubHandle) Fill(ctx context.Context, id journalv2.FileID, off int64, p []byte, asOf, v journalv2.Version) error {
	err := h.Handle.Fill(ctx, id, off, p, asOf, v)
	h.s.seen("Fill", err)
	return err
}

func (h stubHandle) Offload(ctx context.Context, id journalv2.FileID, through journalv2.Version, limit, widen int64, fn func(context.Context, journalv2.Offer, func([]journalv2.Extent) error) error) error {
	err := h.Handle.Offload(ctx, id, through, limit, widen, func(ctx context.Context, o journalv2.Offer, report func([]journalv2.Extent) error) error {
		return fn(ctx, o, func(es []journalv2.Extent) error {
			err := report(es)
			h.s.seen("Offload report", err)
			return err
		})
	})
	h.s.seen("Offload", err)
	return err
}

func (h stubHandle) Release(ctx context.Context, id journalv2.FileID, es []journalv2.Extent) error {
	err := h.Handle.Release(ctx, id, es)
	h.s.seen("Release", err)
	return err
}

func (h stubHandle) Truncate(ctx context.Context, id journalv2.FileID, size int64) (journalv2.Version, error) {
	v, err := h.Handle.Truncate(ctx, id, size)
	h.s.seen("Truncate", err)
	return v, err
}

func (h stubHandle) Deallocate(ctx context.Context, id journalv2.FileID, e journalv2.Extent) (journalv2.Version, error) {
	v, err := h.Handle.Deallocate(ctx, id, e)
	h.s.seen("Deallocate", err)
	return v, err
}

func (h stubHandle) Delete(ctx context.Context, id journalv2.FileID) (journalv2.Version, error) {
	v, err := h.Handle.Delete(ctx, id)
	h.s.seen("Delete", err)
	return v, err
}

type stubView struct {
	View
	s *stubs
}

func (v stubView) ReadAt(ctx context.Context, id journalv2.FileID, off int64, p []byte) (int, []journalv2.Extent, []journalv2.Held, journalv2.Version, error) {
	n, missing, held, asOf, err := v.View.ReadAt(ctx, id, off, p)
	v.s.seen("ReadAt", err)
	return n, missing, held, asOf, err
}

func (v stubView) Extents(id journalv2.FileID) ([]journalv2.Extent, error) {
	es, err := v.View.Extents(id)
	v.s.seen("Extents", err)
	return es, err
}

func (v stubView) Since(ctx context.Context, id journalv2.FileID, from journalv2.Version) iter.Seq2[journalv2.Change, error] {
	return func(yield func(journalv2.Change, error) bool) {
		for c, err := range v.View.Since(ctx, id, from) {
			v.s.seen("Since", err)
			if !yield(c, err) {
				return
			}
		}
	}
}

func (v stubView) Index(id journalv2.FileID) ([]journalv2.IndexEntry, error) {
	es, err := v.View.Index(id)
	v.s.seen("Inspect.File", err)
	return es, err
}

// Files and DirtyFiles return no error, so a stub of either shows as an
// empty answer, which Compare reports as it finds it.
func (v stubView) Files() iter.Seq[journalv2.FileID] { return v.View.Files() }

func (v stubView) DirtyFiles() iter.Seq2[journalv2.FileID, time.Time] { return v.View.DirtyFiles() }

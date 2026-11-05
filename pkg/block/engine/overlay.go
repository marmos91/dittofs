package engine

import (
	"context"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// Overlay is the size, times and version file f of share s has as its
// uncommitted operations leave them, read from its entry alone: no metadata
// read, and no lock but the file's own. pending false: none is held, the zero
// Overlay answers, and the File record answers alone.
func (e *Engine) Overlay(ctx context.Context, s entity.ShareID, f entity.FileID) (o Overlay, pending bool, err error) {
	if _, err := e.bindingOf(s); err != nil {
		return Overlay{}, false, err
	}
	fe := e.files.lockOf(f)
	if fe == nil {
		return Overlay{}, false, nil
	}
	o, pending = replay(fe.ops)
	e.files.unlock(f, fe)
	return o, pending, nil
}

// replay is the overlay ops leave, replayed in their order, which is version
// order: a truncate sets Size and SizeSet, each later write or clone raises Size
// to its end, and Version is the last operation's. pending false: ops is empty.
//
// decision: the journal records one time per write, its mtime, zero when
// suspended, so the overlay's times are the entry's clock times: Ctime the
// newest operation's, Mtime the newest of an operation that set it, zero when
// none did. A suspended write's change time is therefore memory only, and a
// restart cannot rebuild it. Overturned once the journal records a change
// time beside the modification time.
func replay(ops []pendingOp) (o Overlay, pending bool) {
	live := 0
	for _, op := range ops {
		if op.dropped {
			continue
		}
		live++
		switch op.kind {
		case opTruncate:
			o.Size, o.SizeSet = op.extent.Off, true
		case opWrite, opClone:
			o.Size = max(o.Size, opEnd(op.extent))
		}
		o.Ctime = op.at
		if op.setsMtime {
			o.Mtime = op.at
		}
		o.Version = entity.JournalVersion(op.version.Bytes())
	}
	return o, live > 0
}

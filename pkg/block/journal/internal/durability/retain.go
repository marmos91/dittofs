package durability

import (
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// isContent reports whether w is a record of file content: a write or a fill.
// It is the records that hold extents in the index, and so the ones that
// supersede retained content; every other kind carries its whole encoding in
// w.Bytes and holds nothing.
func isContent(w segment.Written) bool {
	return w.Kind == format.KindWrite || w.Kind == format.KindFill
}

// lift stops retaining what the record of version v superseded in file, now
// that the record is synced, and accounts the change. A failure is logged and
// stands: the extent stays retained, which wastes space and loses nothing.
//
// decision: the caller holds file's append domain. A writer publishes the
// content it superseded as retained while it holds that domain, from its
// append to its publish, so a sync that covers the record in that gap lifts
// only after the publish and finds the extent to drop. Lifting outside the
// domain would find nothing to drop, and the extent would stay retained for
// good. Overturned by a publish that no longer follows the append under one
// domain hold.
func (d *Durability) lift(file core.FileID, v core.Version) {
	hd, err := d.index.LiftSuperseded(file, v)
	if err != nil {
		d.log.Error("journal could not lift superseded content", "file", file, "version", v, "err", err)
		return
	}
	d.accountDelta(hd)
}

// accountDelta hands hd to Config.Account when one is set and hd changes
// something.
func (d *Durability) accountDelta(hd index.HeldDelta) {
	if d.account != nil && hd != (index.HeldDelta{}) {
		d.account(hd)
	}
}

package segment

import (
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
)

// Written is one record Append wrote, as the durability sink registers it.
type Written struct {
	// Loc is where the record landed: its segment, its own offset and its
	// padded length, Loc.Len.
	Loc Location
	// Seq, Version, File and Kind are the record's header fields.
	Seq     core.Seq
	Version core.Version
	File    core.FileID
	Kind    format.Kind
	// Extent and Tag are the record's header fields too: the file range the
	// record names and the share it counts against.
	Extent core.Extent
	Tag    core.ShareTag
	// PayloadCRC is the seeded payload checksum as written on disk.
	PayloadCRC uint32
	// Bytes is the whole encoded record, header, body and padding, for a
	// record with no payload — every kind but KindWrite and KindFill — and
	// nil otherwise. It is not a copy: the sink may keep it.
	Bytes []byte
}

// Sink is the durability sink a stream reports to: the two functions the
// layer above registers when it builds the stream. Track is called once per
// record Append wrote, after its bytes are written and before Append returns
// its location. Synced is called with the offset each completed sync of a
// segment covered, whoever issued the sync.
//
// Track is always called with the stream's segment append domain held, and
// possibly with the file append domain the caller of Append holds. Synced
// arrives with the segment append domain held when it comes from a stream's
// Sync, and with no stream lock held when it comes from a seal's own sync
// (WriteSeal). So neither may take a lock of this package or block, whichever
// way it arrives: the consumer hands each event to its own per-stream
// worker. Synced values of one segment may arrive out of order, for two syncs
// can run at once, so a consumer takes the largest.
//
// decision: the sink is called synchronously rather than through a queue of
// the stream's own, because Track must precede Append's return and precede any
// Synced that covers the record, and a queue would need an order of its own to
// keep both. The cost lands on the sink, which must meet the non-blocking
// contract; withdraw the synchronous call if a sink that cannot meet it
// appears. The stream cannot enforce that contract: it calls the sink under
// the segment append domain, so a sink that blocks blocks the append.
type Sink struct {
	Track  func(Written)
	Synced func(seg core.SegmentID, through int64)
}

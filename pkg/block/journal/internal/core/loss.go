package core

// LossRingSize is how many loss events the journal keeps for a poller, the
// newest. An older one is overwritten, and the gap shows in the events'
// SeqNum.
//
// ponytail: the ring's size is fixed. Upgrade to a configured size when a
// poller is seen to miss events in practice.
const LossRingSize = 256

// LossEvent is an extent the journal stopped holding without being asked.
type LossEvent struct {
	// SeqNum is the event's sequence number in the loss ring, from 1, not
	// the sequence number of the record it dropped.
	SeqNum uint64

	Share   ShareTag
	File    FileID
	Extent  Extent
	Version Version
	Reason  LossReason

	// Offloaded is whether the extent's offloaded bit was set.
	Offloaded bool

	// AtOpen reports that the event was found at open, from a loss record
	// still on disk, or is an open's own loss.
	AtOpen bool

	// Deferred reports that no loss record exists yet: the copy that
	// retires the dropped record's segment writes it.
	Deferred bool
}

// LossReason is why the journal stopped holding an extent. The zero
// LossReason names none.
type LossReason uint8

// The loss reasons.
const (
	// LossCorrupt is an extent dropped as corrupt.
	LossCorrupt LossReason = iota + 1

	// LossStale is an extent dropped as older than offloaded content.
	LossStale

	// LossFailedWindow is an extent dropped because its sync window failed.
	LossFailedWindow

	// LossSuperseded is an extent dropped because metadata holds newer
	// content under it. It never raises the generation.
	LossSuperseded

	// LossAcknowledged is an extent an operator acknowledged as unreadable.
	LossAcknowledged
)

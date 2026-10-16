package core

// Config is the journal's configuration as its internal packages read it. The
// public package fills it from the options it is given; a zero field means
// that field's default.
type Config struct {
	MaxBytes        int64
	Streams         int
	ExtentLimit     int64
	HeadroomRecords int
	IdleSealBytes   int64
	SpareTarget     int

	// SegmentCap is the size a segment is allocated at, in bytes.
	//
	// decision: the segment cap is a constant of the implementation, not a
	// setting, and the public API offers no way to change it. It is a field
	// here only so a test can shrink it: a check that needs a journal of a
	// thousand segments must fit in a temporary directory. Only code inside
	// the journal's tree can build an option that sets it.
	SegmentCap int64
}

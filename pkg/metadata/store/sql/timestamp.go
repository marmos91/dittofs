package sql

import "time"

// TimestampResolution is the finest timestamp step the SQL backends can store.
//
// Both encode file times as a Windows FILETIME — 100ns ticks since 1601 — so a
// caller setting a finer time has it rounded down on the way in. This is what
// the backends advertise in FilesystemCapabilities, and NFSv3 passes it on to
// clients verbatim as FSINFO time_delta, so it has to describe the row rather
// than the precision of a Go time.Time.
const TimestampResolution = 100 * time.Nanosecond

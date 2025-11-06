package metadata

// FSInfo contains static filesystem information and capabilities.
// This structure is returned by the repository to inform NFS clients
// about server limits, preferences, and supported features.
type FSInfo struct {
	// RtMax is the maximum size in bytes of a READ request
	// Clients should not exceed this value in READ operations
	RtMax uint32

	// RtPref is the preferred size in bytes of a READ request
	// Optimal performance is achieved using this size
	RtPref uint32

	// RtMult is the suggested multiple for READ request sizes
	// READ sizes should ideally be multiples of this value
	RtMult uint32

	// WtMax is the maximum size in bytes of a WRITE request
	// Clients should not exceed this value in WRITE operations
	WtMax uint32

	// WtPref is the preferred size in bytes of a WRITE request
	// Optimal performance is achieved using this size
	WtPref uint32

	// WtMult is the suggested multiple for WRITE request sizes
	// WRITE sizes should ideally be multiples of this value
	WtMult uint32

	// DtPref is the preferred size in bytes of a READDIR request
	// Optimal performance is achieved using this size
	DtPref uint32

	// MaxFileSize is the maximum file size in bytes supported by the server
	MaxFileSize uint64

	// TimeDelta represents the server's time resolution
	// Indicates the granularity of timestamps
	TimeDelta TimeDelta

	// Properties is a bitmask of filesystem properties
	// Indicates supported features (hard links, symlinks, etc.)
	Properties uint32
}

package payload

import "github.com/marmos91/dittofs/pkg/payload/offloader"

// StorageStats contains statistics about block storage capacity and usage.
type StorageStats struct {
	TotalSize     uint64 // Total storage capacity in bytes
	UsedSize      uint64 // Space consumed by content in bytes
	AvailableSize uint64 // Remaining available space in bytes
	ContentCount  uint64 // Total number of content items
	AverageSize   uint64 // Average size of content items in bytes
}

// FlushResult is an alias to offloader.FlushResult.
type FlushResult = offloader.FlushResult

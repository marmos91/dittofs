package metrics

import (
	"time"
)

// NFSMetrics provides observability for NFS adapter operations.
//
// Implementations can collect metrics about NFS requests, connection lifecycle,
// throughput, and errors. This interface is optional - pass nil to disable
// metrics collection with zero overhead.
//
// Example usage:
//
//	// With metrics enabled
//	metrics := prometheus.NewNFSMetrics()
//	adapter := nfs.New(config, metrics)
//
//	// Without metrics (pass nil for zero overhead)
//	adapter := nfs.New(config, nil)
type NFSMetrics interface {
	// RecordRequest records a completed NFS request with its procedure name,
	// share, duration, and outcome.
	//
	// Parameters:
	//   - procedure: NFS procedure name (e.g., "LOOKUP", "READ", "WRITE")
	//   - share: Share name (e.g., "/export", "/archive")
	//   - duration: Time taken to process the request
	//   - errorCode: NFS error code if request failed (e.g., "NFS3ERR_NOENT"), empty if successful
	RecordRequest(procedure string, share string, duration time.Duration, errorCode string)

	// RecordRequestStart increments the in-flight request counter.
	// Should be called when starting to process a request.
	//
	// Parameters:
	//   - procedure: NFS procedure name
	//   - share: Share name
	RecordRequestStart(procedure string, share string)

	// RecordRequestEnd decrements the in-flight request counter.
	// Should be called when request processing completes.
	//
	// Parameters:
	//   - procedure: NFS procedure name
	//   - share: Share name
	RecordRequestEnd(procedure string, share string)

	// RecordBytesTransferred records bytes read or written.
	//
	// Parameters:
	//   - procedure: NFS procedure name (e.g., "READ", "WRITE")
	//   - share: Share name
	//   - direction: "read" or "write"
	//   - bytes: Number of bytes transferred
	RecordBytesTransferred(procedure string, share string, direction string, bytes uint64)

	// RecordOperationSize records the size of a READ or WRITE operation.
	//
	// Parameters:
	//   - operation: "read" or "write"
	//   - share: Share name
	//   - bytes: Size of the operation in bytes
	RecordOperationSize(operation string, share string, bytes uint64)

	// SetActiveConnections updates the current connection count.
	//
	// Parameters:
	//   - count: Current number of active connections
	SetActiveConnections(count int32)

	// RecordConnectionAccepted increments the total accepted connections counter.
	RecordConnectionAccepted()

	// RecordConnectionClosed increments the total closed connections counter.
	RecordConnectionClosed()

	// RecordConnectionForceClosed increments the force-closed connections counter.
	// Called when connections are forcibly closed after shutdown timeout.
	RecordConnectionForceClosed()

	// RecordCacheHit records a cache hit during a READ operation.
	//
	// Parameters:
	//   - share: Share name
	//   - cacheType: Type of cache ("read" or "write")
	//   - bytes: Number of bytes served from cache
	RecordCacheHit(share string, cacheType string, bytes uint64)

	// RecordCacheMiss records a cache miss during a READ operation.
	//
	// Parameters:
	//   - share: Share name
	//   - bytes: Number of bytes that will be read from content store
	RecordCacheMiss(share string, bytes uint64)
}

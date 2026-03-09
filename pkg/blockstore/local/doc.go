// Package local defines the LocalStore interface for on-node block caching.
//
// LocalStore manages the two-tier (memory + disk) cache that sits between
// protocol adapters and the remote block store. It handles buffering NFS writes,
// flushing to disk, memory backpressure, and block state transitions.
//
// The interface is decomposed into four focused sub-interfaces:
//   - LocalReader: cache-aware reads (memory then disk)
//   - LocalWriter: write buffering and remote-data caching
//   - LocalFlusher: memory-to-disk flush and dirty block retrieval
//   - LocalManager: lifecycle, eviction, deletion, and observability
package local

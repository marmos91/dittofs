// Package local defines the LocalStore interface for on-node block caching.
//
// LocalStore manages the two-tier (memory + disk) cache that sits between
// protocol adapters and the remote block store. It handles buffering NFS writes,
// flushing to disk, memory backpressure, and block state transitions.
package local

// Package remote defines the RemoteStore interface for durable block storage
// backends (S3, filesystem, memory).
//
// RemoteStore provides low-level block operations (read, write, delete, list)
// against a persistent storage backend. Blocks are immutable chunks of data
// (up to BlockSize) stored with a string key.
//
// Key format: "{payloadID}/block-{blockIdx}"
package remote

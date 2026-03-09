// Package io provides cache-aware read and write operations for the block store.
//
// ReadAt and WriteAt coordinate between the local cache (LocalStore) and the
// syncer (for remote downloads) to provide transparent I/O. Reads check the
// local cache first, falling back to remote download on cache miss. Writes go
// directly to the local cache; background syncing handles remote persistence.
//
// Import alias: Use bsio "github.com/marmos91/dittofs/pkg/blockstore/io"
// to avoid collision with Go's standard io package.
package io

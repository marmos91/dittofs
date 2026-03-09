package fs

import (
	"errors"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// Errors returned by FSStore.
var (
	ErrCacheClosed    = errors.New("cache: closed")
	ErrDiskFull       = errors.New("cache: disk full after eviction")
	ErrFileNotInCache = errors.New("file not in cache")

	// ErrBlockNotFound is an alias for blockstore.ErrBlockNotFound.
	ErrBlockNotFound = blockstore.ErrBlockNotFound
)

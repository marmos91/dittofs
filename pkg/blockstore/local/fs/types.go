package fs

import "errors"

// Errors returned by FSStore.
var (
	ErrCacheClosed    = errors.New("cache: closed")
	ErrDiskFull       = errors.New("cache: disk full after eviction")
	ErrFileNotInCache = errors.New("file not in cache")
	ErrBlockNotFound  = errors.New("block not found")
)

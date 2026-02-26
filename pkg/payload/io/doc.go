// Package io provides read and write operations for the payload service,
// coordinating between the cache layer and the offloader for persistent storage.
//
// This package defines local interfaces (CacheReader, CacheWriter,
// BlockDownloader, BlockUploader) to avoid circular imports with the cache
// and offloader packages. Callers construct a ServiceImpl by passing concrete
// cache and offloader implementations that satisfy these interfaces.
//
// Architecture:
//
//	io.ServiceImpl
//	    |-- CacheReader/CacheWriter (cache layer)
//	    |-- BlockDownloader/BlockUploader (offloader layer)
//
// Usage:
//
//	import payloadio "github.com/marmos91/dittofs/pkg/payload/io"
//
//	svc := payloadio.New(cache, offloader)
//	n, err := svc.ReadAt(ctx, payloadID, buf, offset)
//	err = svc.WriteAt(ctx, payloadID, data, offset)
package io

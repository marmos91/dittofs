// Package gc implements garbage collection for orphan blocks in the block store.
//
// Orphan blocks are blocks that exist in the block store but have no corresponding
// metadata. This can happen when file deletion fails after metadata is removed but
// before blocks are deleted, or when the server crashes during file deletion.
//
// The CollectGarbage function scans the block store, groups blocks by payloadID,
// and checks each payloadID against the metadata store. Blocks without metadata
// are deleted.
//
// Usage:
//
//	// Dry run first
//	dryStats := gc.CollectGarbage(ctx, blockStore, registry, &gc.Options{DryRun: true})
//	logger.Info("Would delete", "orphanBlocks", dryStats.OrphanBlocks)
//
//	// Then actually delete
//	stats := gc.CollectGarbage(ctx, blockStore, registry, nil)
//
// This package has zero coupling to the Offloader - it only needs a BlockStore
// and a MetadataReconciler to check metadata existence.
package gc

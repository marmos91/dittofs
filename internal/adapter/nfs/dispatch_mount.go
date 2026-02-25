package nfs

import (
	mount "github.com/marmos91/dittofs/internal/adapter/nfs/mount/handlers"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// ============================================================================
// Mount Dispatch Table Initialization
// ============================================================================

func initMountDispatchTable() {
	MountDispatchTable = map[uint32]*mountProcedure{
		mount.MountProcNull: {
			Name:      "NULL",
			Handler:   handleMountNull,
			NeedsAuth: true,
		},
		mount.MountProcMnt: {
			Name:      "MNT",
			Handler:   handleMountMnt,
			NeedsAuth: true,
		},
		mount.MountProcDump: {
			Name:      "DUMP",
			Handler:   handleMountDump,
			NeedsAuth: false,
		},
		mount.MountProcUmnt: {
			Name:      "UMNT",
			Handler:   handleMountUmnt,
			NeedsAuth: false,
		},
		mount.MountProcUmntAll: {
			Name:      "UMNTALL",
			Handler:   handleMountUmntAll,
			NeedsAuth: false,
		},
		mount.MountProcExport: {
			Name:      "EXPORT",
			Handler:   handleMountExport,
			NeedsAuth: false,
		},
	}
}

// ============================================================================
// Mount Procedure Handlers
// ============================================================================
//
// Each Mount handler follows the same pattern as NFS handlers:
// context propagation for cancellation support.

func handleMountNull(
	ctx *mount.MountHandlerContext,
	handler *mount.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		mount.DecodeNullRequest,
		func(req *mount.NullRequest) (*mount.NullResponse, error) {
			return handler.MountNull(ctx, req)
		},
		mount.MountErrIO,
		func(status uint32) *mount.NullResponse {
			return &mount.NullResponse{MountResponseBase: mount.MountResponseBase{Status: status}}
		},
	)
}

func handleMountMnt(
	ctx *mount.MountHandlerContext,
	handler *mount.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		mount.DecodeMountRequest,
		func(req *mount.MountRequest) (*mount.MountResponse, error) {
			return handler.Mount(ctx, req)
		},
		mount.MountErrIO,
		func(status uint32) *mount.MountResponse {
			return &mount.MountResponse{MountResponseBase: mount.MountResponseBase{Status: status}}
		},
	)
}

func handleMountDump(
	ctx *mount.MountHandlerContext,
	handler *mount.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		mount.DecodeDumpRequest,
		func(req *mount.DumpRequest) (*mount.DumpResponse, error) {
			return handler.Dump(ctx, req)
		},
		mount.MountErrIO,
		func(status uint32) *mount.DumpResponse {
			return &mount.DumpResponse{MountResponseBase: mount.MountResponseBase{Status: status}, Entries: []mount.DumpEntry{}}
		},
	)
}

func handleMountUmnt(
	ctx *mount.MountHandlerContext,
	handler *mount.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		mount.DecodeUmountRequest,
		func(req *mount.UmountRequest) (*mount.UmountResponse, error) {
			return handler.Umnt(ctx, req)
		},
		mount.MountErrIO,
		func(status uint32) *mount.UmountResponse {
			return &mount.UmountResponse{MountResponseBase: mount.MountResponseBase{Status: status}}
		},
	)
}

func handleMountUmntAll(
	ctx *mount.MountHandlerContext,
	handler *mount.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		mount.DecodeUmountAllRequest,
		func(req *mount.UmountAllRequest) (*mount.UmountAllResponse, error) {
			return handler.UmntAll(ctx, req)
		},
		mount.MountErrIO,
		func(status uint32) *mount.UmountAllResponse {
			return &mount.UmountAllResponse{MountResponseBase: mount.MountResponseBase{Status: status}}
		},
	)
}

func handleMountExport(
	ctx *mount.MountHandlerContext,
	handler *mount.Handler,
	reg *runtime.Runtime,
	data []byte,
) (*HandlerResult, error) {
	return handleRequest(
		data,
		mount.DecodeExportRequest,
		func(req *mount.ExportRequest) (*mount.ExportResponse, error) {
			return handler.Export(ctx, req)
		},
		mount.MountErrIO,
		func(status uint32) *mount.ExportResponse {
			return &mount.ExportResponse{MountResponseBase: mount.MountResponseBase{Status: status}, Entries: []mount.ExportEntry{}}
		},
	)
}

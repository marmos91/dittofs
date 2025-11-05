package mount

// Mount Protocol Procedure Numbers
// These identify the different Mount operations as defined in RFC 1813 Appendix I.
//
// The Mount protocol is RPC program 100005, version 3.
// All procedures use XDR encoding for requests and responses.
const (
	// MountProcNull - Do nothing (connectivity test)
	// Request: void
	// Response: void
	MountProcNull = 0

	// MountProcMnt - Add mount entry and return file handle
	// Request: dirpath (string)
	// Response: fhstatus3 (status + file handle + auth flavors)
	MountProcMnt = 1

	// MountProcDump - Return list of active mounts
	// Request: void
	// Response: mountlist (linked list of hostname + directory pairs)
	MountProcDump = 2

	// MountProcUmnt - Remove a specific mount entry
	// Request: dirpath (string)
	// Response: void
	MountProcUmnt = 3

	// MountProcUmntAll - Remove all mount entries for the calling client
	// Request: void
	// Response: void
	MountProcUmntAll = 4

	// MountProcExport - Return list of available exports
	// Request: void
	// Response: exports (linked list of directory + groups pairs)
	MountProcExport = 5
)

// Mount Status Codes
// These are the error codes that can be returned by Mount protocol procedures.
// Only the MOUNT (MNT) procedure returns status codes; other procedures return void.
//
// Status codes follow the Unix errno convention where applicable.
const (
	// MountOK - Success
	MountOK = 0

	// MountErrPerm - Not owner (operation not permitted)
	MountErrPerm = 1

	// MountErrNoEnt - No such file or directory (export not found)
	MountErrNoEnt = 2

	// MountErrIO - I/O error (internal server error reading export)
	MountErrIO = 5

	// MountErrAccess - Permission denied (client not allowed to mount)
	MountErrAccess = 13

	// MountErrNotDir - Not a directory (export path is not a directory)
	MountErrNotDir = 20

	// MountErrInval - Invalid argument (malformed request)
	MountErrInval = 22

	// MountErrNameTooLong - Filename too long (export path exceeds limits)
	MountErrNameTooLong = 63

	// MountErrNotSupp - Operation not supported
	MountErrNotSupp = 10004

	// MountErrServerFault - Server fault (internal error)
	MountErrServerFault = 10006
)

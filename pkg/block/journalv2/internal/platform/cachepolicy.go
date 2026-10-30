package platform

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// The answer to IOCTL_STORAGE_QUERY_PROPERTY for StorageDeviceWriteCacheProperty
// is a STORAGE_WRITE_CACHE_PROPERTY
// (https://learn.microsoft.com/windows/win32/api/winioctl/ns-winioctl-storage_write_cache_property):
// Version and Size as two little-endian 32-bit DWORDs, four enums, which
// Microsoft C stores as an int ("A variable declared as enum is an int.",
// https://learn.microsoft.com/cpp/c-language/enum-type), then the BOOLEANs
// FlushCacheSupported, UserDefinedPowerProtection and NVCacheEnabled, one byte
// each ("typedef BYTE BOOLEAN;",
// https://learn.microsoft.com/windows/win32/winprog/windows-data-types).
const (
	// writeCacheVersionOff is the offset of Version, which "Contains the
	// size of this structure" and "will change as members are added to the
	// structure" (the Learn page above).
	writeCacheVersionOff = 0
	// writeCacheSizeOff is the offset of Size, which "Specifies the total
	// size of the data returned" (the Learn page above).
	writeCacheSizeOff = 4
	// writeCacheHeaderLen is Version and Size, the bytes every answer has.
	writeCacheHeaderLen = 8
	// writeCachePolicyOff is the offset of UserDefinedPowerProtection.
	writeCachePolicyOff = 25
)

// The Win32 errors a query fails with when the device does not report its
// write cache at all (System Error Codes (0-499),
// https://learn.microsoft.com/windows/win32/debug/system-error-codes--0-499-).
// Any other error is a failure to ask, not an answer. Microsoft documents that
// the write cache property is "not supported for RAID devices ... or for flash
// memory devices" (Querying for the Write Cache Property,
// https://learn.microsoft.com/windows-hardware/drivers/storage/querying-for-the-write-cache-property),
// but not which error the query returns there; these two are the ones that say
// the request itself is not handled. Any other error fails the check, an I/O
// error included: Microsoft's class driver sample fails this query with
// STATUS_IO_DEVICE_ERROR when the disk cannot read its caching mode page
// (ClasspWriteCacheProperty, storage/class/classpnp/src/utils.c in
// Windows-driver-samples), so such a disk cannot hold a journal.
const (
	// errorInvalidFunction is ERROR_INVALID_FUNCTION, "Incorrect function."
	errorInvalidFunction uint32 = 1
	// errorNotSupported is ERROR_NOT_SUPPORTED, "The request is not
	// supported."
	errorNotSupported uint32 = 50
)

// cacheProbe is what the Windows probe gathers about a journal directory's
// volume: the inputs of the verdict, recorded so the verdict itself makes no
// system call and is tested on every platform.
type cacheProbe struct {
	// dir is the journal directory.
	dir string
	// mountPoint is GetVolumePathName's answer for dir: the root of the
	// volume dir is on, junctions and mounted folders followed.
	mountPoint string
	// volume is GetVolumeNameForVolumeMountPoint's answer for mountPoint, a
	// volume GUID path.
	volume string
	// queryErr is the Win32 error IOCTL_STORAGE_QUERY_PROPERTY failed with,
	// or 0 when it succeeded.
	queryErr uint32
	// answer is the bytes the query returned.
	answer []byte
}

// The errors the verdict returns when the query failed or its answer was cut
// short. None is a refusal: each fails the check without naming a rule.
var (
	// errCacheQueryFailed: the query failed with an error that does not say
	// the device leaves its write cache unreported.
	errCacheQueryFailed = errors.New("journal: the volume's write cache query failed")
	// errCacheAnswerShort: the answer's Version holds the field, but its Size
	// or the bytes that came back do not reach it, or fewer bytes came back
	// than its Size declares.
	errCacheAnswerShort = errors.New("journal: the volume's write cache answer is cut short")
)

// checkCacheProbe decides whether the volume p describes may hold a journal
// under RFC 28 Appendix A: "Open MUST refuse a volume whose write-cache buffer
// flushing is turned off by policy, where the platform reports it". It returns
// a *refusal when the answer reports the policy on, errCacheQueryFailed or
// errCacheAnswerShort when the query did not give a whole answer, and nil when
// the policy is off or not reported.
//
// The policy is UserDefinedPowerProtection. Microsoft's Learn page reads it as
// whether a user "can configure" power protection, but the SDK header
// describes it as the "User selected power protection option through
// registry" (ntddstor.h), and Microsoft's driver samples set it from the
// option itself (Windows-driver-samples): the disk driver turns the
// CacheIsPowerProtected registry value into the device flag
// DEV_POWER_PROTECTED (DiskStartFdo, storage/class/disk/src/pnp.c), then
// completes every flush without sending it to the device while the flag is
// set (DiskShutdownFlush, storage/class/disk/src/disk.c), and the class driver
// reports that flag in this field (ClasspWriteCacheProperty,
// storage/class/classpnp/src/utils.c). With it set, a sync does not reach the
// device, so the volume is refused whatever the rest of the answer says,
// a disabled or battery-backed cache included: the RFC names no exception.
func checkCacheProbe(p cacheProbe) error {
	switch p.queryErr {
	case 0:
	case errorInvalidFunction, errorNotSupported:
		return nil
	default:
		return fmt.Errorf("%w: volume %s: Win32 error %d", errCacheQueryFailed, p.mountPoint, p.queryErr)
	}

	if len(p.answer) < writeCacheHeaderLen {
		return fmt.Errorf("%w: volume %s: %d bytes", errCacheAnswerShort, p.mountPoint, len(p.answer))
	}

	// An older structure, whose Version ends before the field, does not
	// report the policy.
	version := binary.LittleEndian.Uint32(p.answer[writeCacheVersionOff:])

	if version <= writeCachePolicyOff {
		return nil
	}

	// The structure holds the field, so the data returned must reach it:
	// Size, the total the driver says it returned, and then the bytes that
	// actually came back. A Size of 0 is cut short too.
	size := binary.LittleEndian.Uint32(p.answer[writeCacheSizeOff:])

	if size <= writeCachePolicyOff {
		return fmt.Errorf("%w: volume %s: version %d, size %d", errCacheAnswerShort, p.mountPoint, version, size)
	}

	if uint64(len(p.answer)) < uint64(size) {
		return fmt.Errorf("%w: volume %s: %d bytes, size %d", errCacheAnswerShort, p.mountPoint, len(p.answer), size)
	}

	// The field is a C BOOLEAN, true when it is not zero.
	if p.answer[writeCachePolicyOff] != 0 {
		return &refusal{
			rule:  ruleCachePolicy,
			dir:   p.dir,
			mount: p.mountPoint,
			path:  p.volume,
		}
	}

	return nil
}

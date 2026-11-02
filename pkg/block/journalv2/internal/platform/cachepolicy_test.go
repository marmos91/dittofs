package platform

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
)

// flushingOn returns a STORAGE_WRITE_CACHE_PROPERTY answer for a disk whose
// write-cache buffer flushing is on. It is built from the source of the
// answer Microsoft's class driver sample writes, not captured on Windows
// (ClasspWriteCacheProperty in Windows-driver-samples,
// storage/class/classpnp/src/utils.c): Version and Size are both
// sizeof(STORAGE_WRITE_CACHE_PROPERTY), and UserDefinedPowerProtection is
// the disk's DEV_POWER_PROTECTED flag. The layout is the structure of
// https://learn.microsoft.com/windows/win32/api/winioctl/ns-winioctl-storage_write_cache_property:
// two 32-bit DWORDs, four enums, which Microsoft C stores as an int ("A
// variable declared as enum is an int.",
// https://learn.microsoft.com/cpp/c-language/enum-type), and three
// BOOLEANs of one byte ("typedef BYTE BOOLEAN;",
// https://learn.microsoft.com/windows/win32/winprog/windows-data-types),
// padded to 28 bytes, little-endian. Each call returns new bytes, so a
// verdict that writes into one answer cannot hide in the next test's.
func flushingOn() []byte {
	return []byte{
		0x1c, 0x00, 0x00, 0x00, // Version: 28
		0x1c, 0x00, 0x00, 0x00, // Size: 28
		0x02, 0x00, 0x00, 0x00, // WriteCacheType: WriteCacheTypeWriteBack
		0x02, 0x00, 0x00, 0x00, // WriteCacheEnabled: WriteCacheEnabled
		0x01, 0x00, 0x00, 0x00, // WriteCacheChangeable: WriteCacheNotChangeable
		0x02, 0x00, 0x00, 0x00, // WriteThroughSupported: WriteThroughSupported
		0x01, // FlushCacheSupported: TRUE
		0x00, // UserDefinedPowerProtection: FALSE
		0x00, // NVCacheEnabled: FALSE
		0x00, // padding
	}
}

// with returns a copy of answer with the byte at off set to v.
func with(answer []byte, off int, v byte) []byte {
	b := make([]byte, len(answer))
	copy(b, answer)
	b[off] = v

	return b
}

// flushingOff returns the answer of a disk whose flushing is turned off by
// policy: the class driver sample sets UserDefinedPowerProtection, offset 25,
// from the same flag that makes the disk driver sample complete every flush
// without sending it to the device (DiskShutdownFlush in
// storage/class/disk/src/disk.c). The flag is read from the
// CacheIsPowerProtected registry value (DiskStartFdo in
// storage/class/disk/src/pnp.c).
func flushingOff() []byte {
	return with(flushingOn(), 25, 0x01)
}

// The volume of the probes below: the volume GUID path Naming a Volume
// gives as its example
// (https://learn.microsoft.com/windows/win32/fileio/naming-a-volume),
// mounted at D:\.
const (
	testDir        = `D:\journal`
	testMountPoint = `D:\`
	testVolume     = `\\?\Volume{26a21bda-a627-11d7-9931-806e6f6e6963}\`
)

// cacheProbeOf returns a probe of testDir whose query succeeded with answer.
func cacheProbeOf(answer []byte) cacheProbe {
	return cacheProbe{
		dir:        testDir,
		mountPoint: testMountPoint,
		volume:     testVolume,
		answer:     answer,
	}
}

// checkNoPanic runs the verdict on p and fails t if it panics, so a verdict
// that reads past the bytes of an answer fails on an assertion that names
// the answer's length.
func checkNoPanic(t *testing.T, p cacheProbe) (err error) {
	t.Helper()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("verdict panicked on %d bytes: %v", len(p.answer), r)
		}
	}()

	return checkCacheProbe(p)
}

// TestCheckWriteCachePolicy covers RFC 28 Appendix A: "Open MUST refuse a
// volume whose write-cache buffer flushing is turned off by policy, where the
// platform reports it". An answer with the policy on refuses, whatever else it
// says; an answer with the policy off, and every form of "not reported", pass;
// a query that failed for another reason, or an answer cut short, is an error
// and not a refusal.
func TestCheckWriteCachePolicy(t *testing.T) {
	prof.Watch(t, prof.Options{})

	refused := []struct {
		name   string
		answer []byte
	}{
		{
			// Fails if the policy is not checked, or if NVCacheEnabled,
			// offset 26 and clear here, is read for it.
			name:   "flushing off refuses",
			answer: flushingOff(),
		},
		{
			// A C BOOLEAN is true when it is not zero. Fails if only 0x01 is
			// read as true, which would pass a driver writing another true
			// value.
			name:   "flushing off refuses on any true value",
			answer: with(flushingOn(), 25, 0xff),
		},
		{
			// WriteCacheEnabled, offset 12, is WriteCacheDisabled (1). RFC 28
			// Appendix A names no exception, so the refusal stands. Fails if
			// the verdict excuses the policy when the cache reports itself
			// off.
			name:   "flushing off refuses with the device cache off",
			answer: with(flushingOff(), 12, 0x01),
		},
		{
			// NVCacheEnabled, offset 26, says the cache has a battery. RFC 28
			// Appendix A names no exception, so the refusal stands. Fails if
			// the verdict excuses the policy when the cache has a battery.
			name:   "flushing off refuses with a battery-backed cache",
			answer: with(flushingOff(), 26, 0x01),
		},
		{
			// Size "may include data that follows this structure" (the Learn
			// page above): 32 bytes, Size 32. Fails if only a Size of exactly
			// 28 is read as a whole answer.
			name:   "flushing off refuses past a longer answer",
			answer: append(with(flushingOff(), 4, 0x20), 0x00, 0x00, 0x00, 0x00),
		},
		{
			// The smallest Version that holds the field, 26 bytes. Fails if
			// a Version of 26 is read as ending before offset 25.
			name:   "flushing off refuses at version 26",
			answer: with(flushingOff(), 0, 0x1a),
		},
		{
			// The smallest Size and the fewest bytes that hold the field:
			// Size 26 and 26 bytes returned. Fails if a Size of 26, or 26
			// bytes, are read as cut short.
			name:   "flushing off refuses at size 26",
			answer: with(flushingOff(), 4, 0x1a)[:26],
		},
	}

	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			err := checkNoPanic(t, cacheProbeOf(tc.answer))

			wantRefusal(t, err, ruleCachePolicy, testDir, testMountPoint, "", testVolume)
		})
	}

	passed := []struct {
		name   string
		answer []byte
	}{
		{
			// Fails if FlushCacheSupported, offset 24 and set here, is read
			// as the policy.
			name:   "flushing on passes",
			answer: flushingOn(),
		},
		{
			// Fails if NVCacheEnabled, offset 26 and set here, is read as the
			// policy.
			name:   "flushing on with a battery-backed cache passes",
			answer: with(flushingOn(), 26, 0x01),
		},
	}

	for _, tc := range passed {
		t.Run(tc.name, func(t *testing.T) {
			wantPass(t, checkNoPanic(t, cacheProbeOf(tc.answer)))
		})
	}

	notReported := []struct {
		name string
		p    cacheProbe
	}{
		{
			// ERROR_NOT_SUPPORTED, 50 (0x32), "The request is not supported."
			// (https://learn.microsoft.com/windows/win32/debug/system-error-codes--0-499-).
			name: "query not supported",
			p: cacheProbe{
				dir:        testDir,
				mountPoint: testMountPoint,
				volume:     testVolume,
				queryErr:   50,
			},
		},
		{
			// ERROR_INVALID_FUNCTION, 1 (0x1), "Incorrect function." (same
			// page), what a device that does not handle the request returns.
			name: "query invalid function",
			p: cacheProbe{
				dir:        testDir,
				mountPoint: testMountPoint,
				volume:     testVolume,
				queryErr:   1,
			},
		},
		{
			// An older structure: Version "Contains the size of this
			// structure" and "will change as members are added to the
			// structure" (the Learn page above). Version 25 ends before
			// UserDefinedPowerProtection at offset 25, so it does not report
			// the policy.
			name: "version 25 ends before the field",
			p: cacheProbeOf([]byte{
				0x19, 0x00, 0x00, 0x00, // Version: 25
				0x19, 0x00, 0x00, 0x00, // Size: 25
				0x02, 0x00, 0x00, 0x00, // WriteCacheType
				0x02, 0x00, 0x00, 0x00, // WriteCacheEnabled
				0x01, 0x00, 0x00, 0x00, // WriteCacheChangeable
				0x02, 0x00, 0x00, 0x00, // WriteThroughSupported
				0x01, // FlushCacheSupported
			}),
		},
		{
			// Version 25 with 28 bytes returned whose byte 25 is set: the
			// byte lies past the structure Version describes, so it is not
			// the policy. Fails if a Version of 25 is read as holding the
			// field.
			name: "version 25 with a set byte past it",
			p:    cacheProbeOf(with(flushingOff(), 0, 0x19)),
		},
	}

	for _, tc := range notReported {
		t.Run(tc.name+" passes", func(t *testing.T) {
			// Fails if an answer that does not report the policy is read as
			// the policy being on.
			wantPass(t, checkNoPanic(t, tc.p))
		})
	}

	t.Run("every other query error is an error", func(t *testing.T) {
		// Every number from 2 to 499 but 50, among them ERROR_ACCESS_DENIED,
		// 5, and ERROR_INSUFFICIENT_BUFFER, 122 (System Error Codes (0-499),
		// above). Fails if a failed query is read as "not reported", which
		// would pass a volume the probe could not ask, or if any other code
		// is added to the two.
		for code := uint32(2); code <= 499; code++ {
			if code == 50 {
				continue
			}

			p := cacheProbeOf(nil)
			p.queryErr = code

			err := checkNoPanic(t, p)
			if !errors.Is(err, errCacheQueryFailed) {
				t.Fatalf("Win32 error %d: got %v, want %v", code, err, errCacheQueryFailed)
			}
		}
	})

	t.Run("an I/O device error is an error", func(t *testing.T) {
		// ERROR_IO_DEVICE, 1117 (0x45D), "The request could not be performed
		// because of an I/O device error."
		// (https://learn.microsoft.com/windows/win32/debug/system-error-codes--1000-1299-).
		// Microsoft's class driver sample fails this query with
		// STATUS_IO_DEVICE_ERROR when the disk cannot read its caching mode
		// page (ClasspWriteCacheProperty, storage/class/classpnp/src/utils.c),
		// so such a disk cannot hold a journal. Fails if a code above 499 is
		// read as "not reported".
		p := cacheProbeOf(nil)
		p.queryErr = 1117

		wantError(t, checkNoPanic(t, p), errCacheQueryFailed)
	})

	short := []struct {
		name   string
		answer []byte
	}{
		{
			name:   "no answer",
			answer: nil,
		},
		{
			name:   "half a header",
			answer: flushingOn()[:4],
		},
		{
			// The class driver sample answers with the 8-byte header alone
			// when the caller's buffer is too small for the structure.
			name:   "header alone",
			answer: flushingOn()[:8],
		},
		{
			// The bytes stop after the field, before Size's 28: the policy
			// byte is there, but the answer is not whole.
			name:   "cut after the field",
			answer: flushingOff()[:26],
		},
		{
			// The bytes stop just before the field Size promises.
			name:   "cut at 25 bytes",
			answer: flushingOff()[:25],
		},
		{
			// Size 26 with 25 bytes returned: one byte short of Size, and
			// short of the field.
			name:   "one byte short of size 26",
			answer: with(flushingOff(), 4, 0x1a)[:25],
		},
		{
			// Version 28 holds the field, but Size 25, the total of the data
			// returned, does not reach it. Fails if Size is read as the
			// structure's version.
			name:   "size 25 under version 28",
			answer: with(flushingOff(), 4, 0x19),
		},
		{
			// Size 0 under Version 28: no data is said to be returned.
			name:   "size 0",
			answer: with(flushingOff(), 4, 0x00),
		},
		{
			// Size 0x0001001c, 65564, with 28 bytes returned. Fails if Size
			// is read as 16 bits, which gives 28.
			name:   "size past 16 bits",
			answer: with(flushingOff(), 6, 0x01),
		},
	}

	for _, tc := range short {
		t.Run(tc.name+" is an error", func(t *testing.T) {
			// Fails if a cut answer is read as "not reported" or as the
			// policy, or if the verdict reads past the bytes returned.
			wantError(t, checkNoPanic(t, cacheProbeOf(tc.answer)), errCacheAnswerShort)
		})
	}
}

// TestCheckWriteCachePolicyConcurrent runs the verdict from several
// goroutines on shared answers: Init and Open of two journals may probe
// together. A verdict that writes into an answer fails the byte comparison
// after the goroutines return, with or without the race detector, and a
// write of the value already there fails under -race.
func TestCheckWriteCachePolicyConcurrent(t *testing.T) {
	prof.Watch(t, prof.Options{})

	off := flushingOff()
	on := flushingOn()
	offBefore := bytes.Clone(off)
	onBefore := bytes.Clone(on)

	var wg sync.WaitGroup
	errs := make([]error, 16)

	for i := range errs {
		wg.Go(func() {
			if i%2 == 0 {
				errs[i] = checkCacheProbe(cacheProbeOf(off))
			} else {
				errs[i] = checkCacheProbe(cacheProbeOf(on))
			}
		})
	}

	wg.Wait()

	for i, err := range errs {
		var r *refusal

		if (i%2 == 0 && !errors.As(err, &r)) || (i%2 == 1 && err != nil) {
			t.Errorf("caller %d: got %v, want refused %t", i, err, i%2 == 0)
		}
	}

	if !bytes.Equal(off, offBefore) || !bytes.Equal(on, onBefore) {
		t.Fatalf("the verdict wrote into the answers: got % x and % x, want % x and % x", off, on, offBefore, onBefore)
	}
}

// TestCheckWriteCachePolicyNamesWhat checks that the message an operator
// reads names the directory, the volume, the policy and what to change. The
// words are asserted, not the message's final form.
func TestCheckWriteCachePolicyNamesWhat(t *testing.T) {
	prof.Watch(t, prof.Options{})

	err := checkNoPanic(t, cacheProbeOf(flushingOff()))
	if err == nil {
		t.Fatal("got nil, want a refusal")
	}

	for _, want := range []string{
		testDir,
		"volume " + testMountPoint,
		testVolume,
		"write-cache buffer flushing",
		"Device Manager",
		"Policies",
		"clear",
		"Turn off Windows write-cache buffer flushing on the device",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not name %q", err.Error(), want)
		}
	}
}

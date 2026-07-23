//go:build windows

package journal

import "golang.org/x/sys/windows"

// diskFreeBytes reports the bytes available to the caller on the volume backing
// dir. It sizes Open's default local-store cap so an unconfigured store still
// bounds its on-disk growth.
func diskFreeBytes(dir string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var freeToCaller, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeToCaller, &total, &totalFree); err != nil {
		return 0, err
	}
	return freeToCaller, nil
}

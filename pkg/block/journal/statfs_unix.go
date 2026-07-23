//go:build unix

package journal

import "golang.org/x/sys/unix"

// diskFreeBytes reports the bytes available to an unprivileged writer on the
// filesystem backing dir. It sizes Open's default local-store cap so an
// unconfigured store still bounds its on-disk growth.
func diskFreeBytes(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

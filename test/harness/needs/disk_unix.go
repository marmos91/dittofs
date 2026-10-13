//go:build !windows

package needs

import "syscall"

// freeGB is the free space, in whole GB, of the file system holding dir.
func freeGB(dir string) (int, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int(uint64(st.Bavail) * uint64(st.Bsize) >> 30), nil
}

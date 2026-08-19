//go:build linux || darwin

package facts

import "syscall"

// diskUsage returns total and free megabytes for the filesystem holding path.
// Free space is reported from Bavail (space available to unprivileged users)
// rather than Bfree, so the reserved-blocks margin is not counted as usable.
func diskUsage(path string) (totalMB, freeMB int, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	total := uint64(st.Blocks) * bs / (1 << 20)
	free := uint64(st.Bavail) * bs / (1 << 20)
	return int(total), int(free), nil
}

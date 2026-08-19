//go:build !linux && !darwin

package facts

import "errors"

func diskUsage(path string) (totalMB, freeMB int, err error) {
	return 0, 0, errors.New("disk usage unsupported on this platform")
}

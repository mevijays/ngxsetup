//go:build !linux && !darwin

package state

// lockFile is a no-op where flock is unavailable; ngxsetup only provisions
// Linux, so this exists purely to keep the package building elsewhere.
func lockFile(string) (func(), error) { return func() {}, nil }

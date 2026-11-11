//go:build !unix

package scaletest

import "time"

// cpuTime falls back to the monotonic clock where the process's own processor
// time is not at hand.
func cpuTime() time.Duration {
	return time.Since(clockStart)
}

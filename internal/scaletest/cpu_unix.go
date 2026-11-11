//go:build unix

package scaletest

import (
	"syscall"
	"time"
)

// cpuTime returns the processor time this process has used, in user and
// system mode. Unlike a wall clock it does not advance while the process
// waits for a processor another job holds.
func cpuTime() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return time.Since(clockStart)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

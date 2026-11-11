// Package scaletest is the measuring half of the module's complexity tests
// (ROB-060-013, ROB-120-002): it runs a piece of work on an input and on a
// larger one built the same way, and fails the test when what the work cost
// grew faster than the input did.
//
// Three costs are compared: the bytes allocated, the number of allocations,
// and the processor time of the test process. The first two are counted, and
// so do not depend on what else the host is doing. Processor time is the only
// measure there is of work that allocates nothing — a scanner searching the
// same buffer again — so it is read from the process's own accounting rather
// than from a wall clock, with the garbage collector held off so that a
// collection falling in one run and not in the other is not read as growth,
// taken as the best of several attempts, and compared as a ratio with a wide
// margin. It is not asserted under the race detector or with -short.
//
// It is imported by tests only.
package scaletest

import (
	"io"
	"runtime"
	"runtime/debug"
	"testing"
	"time"
)

// MaxGrowth is how many times faster than the input a cost may grow before
// Check fails. Work that grows with the square of the size grows four times
// as fast as an input made four times larger.
const MaxGrowth = 2.0

// MaxInput bounds the inputs Check accepts, so that a case cannot be made to
// build something the host has no room for.
const MaxInput = 8 << 20

const (
	// What a larger run may allocate beyond its share whatever the size:
	// allocations of the runtime itself, and the steps of a growing buffer.
	slackBytes  = 64 << 10
	slackAllocs = 64

	// attempts is how often the larger run is timed before its best time is
	// taken for what the work costs, and minRun what the smaller run is
	// repeated up to, so that it is well above the resolution of the clock.
	attempts = 6
	minRun   = 400 * time.Microsecond
	maxReps  = 256

	// A first pair of single runs settles a case when the smaller took at
	// least quickRun and the larger at most quickShare of what it may.
	quickRun   = 12 * time.Microsecond
	quickShare = 0.75

	// collectEvery is how much the timed runs may allocate before the
	// collector, held off while they run, is run between two of them.
	collectEvery = 96 << 20
)

// A Cost is what one measured run cost.
type Cost struct {
	CPU    time.Duration
	Allocs uint64
	Bytes  uint64
}

// Measure runs f reps times and returns what that cost in all.
func Measure(reps int, f func()) Cost {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := cpuTime()
	for i := 0; i < reps; i++ {
		f()
	}
	cpu := cpuTime() - start
	runtime.ReadMemStats(&after)
	return Cost{CPU: cpu, Allocs: after.Mallocs - before.Mallocs, Bytes: after.TotalAlloc - before.TotalAlloc}
}

// Timed reports whether Check compares processor time in this run.
func Timed() bool {
	return !RaceEnabled && !testing.Short()
}

// Check runs work on small and on large, the same input grown at least
// twofold, and fails t when a cost grew more than MaxGrowth times as much as
// the input did. work must not keep what it is given.
func Check[T ~string | ~[]byte](t *testing.T, small, large T, work func(input T)) {
	t.Helper()
	if len(large) > MaxInput {
		t.Fatalf("input of %d bytes is over the cap of %d", len(large), MaxInput)
	}
	if len(small) == 0 || len(large) < 2*len(small) {
		t.Fatalf("the input grew from %d to %d bytes: the case scales nothing", len(small), len(large))
	}
	limit := MaxGrowth * float64(len(large)) / float64(len(small))

	// Once unmeasured: lazily built tables and the first growth of the heap
	// belong to neither size.
	work(small)

	c1 := Measure(1, func() { work(small) })
	c4 := Measure(1, func() { work(large) })
	if float64(c4.Bytes) > limit*float64(c1.Bytes)+slackBytes {
		t.Errorf("allocated %d bytes for %d bytes of input and %d for %d: more than %.1f times as much",
			c1.Bytes, len(small), c4.Bytes, len(large), limit)
	}
	if float64(c4.Allocs) > limit*float64(c1.Allocs)+slackAllocs {
		t.Errorf("%d allocations for %d bytes of input and %d for %d: more than %.1f times as many",
			c1.Allocs, len(small), c4.Allocs, len(large), limit)
	}
	if !Timed() {
		return
	}

	// The collector is held off while a run is timed, and run between runs
	// once enough has been allocated. A memory limit of the process still
	// applies.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	var pending uint64
	timed := func(reps int, input T) time.Duration {
		if pending > collectEvery {
			runtime.GC()
			pending = 0
		}
		c := Measure(reps, func() { work(input) })
		pending += c.Bytes
		return c.CPU
	}

	report := func(reps int, t1, t4 time.Duration) {
		t.Logf("%d -> %d bytes: cpu %v -> %v (x%.1f), allocated %d -> %d bytes in %d -> %d allocations",
			len(small), len(large), t1/time.Duration(reps), t4/time.Duration(reps),
			float64(t4)/float64(t1), c1.Bytes, c4.Bytes, c1.Allocs, c4.Allocs)
	}

	// One run of each first: a pair long enough to read and well within the
	// limit needs no second look.
	reps, best1 := 1, timed(1, small)
	if t4 := timed(1, large); best1 >= quickRun && float64(t4) <= quickShare*limit*float64(best1) {
		report(1, best1, t4)
		return
	}
	for best1 < minRun && reps < maxReps {
		reps *= 4
		best1 = timed(reps, small)
	}
	// A second look times each run on a heap just collected. With the
	// collector held off, a run that allocates more than any before it is
	// the first to touch that memory, and pays the system for it: that is a
	// cost of the measuring, and the larger run meets it first.
	clean := func(reps int, input T) time.Duration {
		var total time.Duration
		for i := 0; i < reps; i++ {
			runtime.GC()
			total += Measure(1, func() { work(input) }).CPU
		}
		pending = 0
		return total
	}
	best4 := timed(reps, large)
	for attempt := 1; ; attempt++ {
		if best1 > 0 && float64(best4) <= limit*float64(best1) {
			report(reps, best1, best4)
			return
		}
		if attempt == attempts {
			break
		}
		// Only a slow small run can hide growth, so the fastest one seen is
		// what the large run is held against.
		if t1 := clean(reps, small); t1 > 0 && (best1 == 0 || t1 < best1) {
			best1 = t1
		}
		if t4 := clean(reps, large); t4 < best4 {
			best4 = t4
		}
	}
	t.Errorf("processor time grew from %v for %d bytes to %v for %d bytes: more than %.1f times as much",
		best1/time.Duration(reps), len(small), best4/time.Duration(reps), len(large), limit)
}

// ChunkReader returns a reader that delivers r at most n bytes a read.
func ChunkReader(r io.Reader, n int) io.Reader {
	return &chunkReader{r: r, n: n}
}

type chunkReader struct {
	r io.Reader
	n int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(p) > c.n {
		p = p[:c.n]
	}
	return c.r.Read(p)
}

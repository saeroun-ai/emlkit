//go:build race

package scaletest

// RaceEnabled reports that the race detector is on: timings mean nothing
// under it, and Check then compares only what it can count.
const RaceEnabled = true

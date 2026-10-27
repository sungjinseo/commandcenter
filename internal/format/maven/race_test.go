//go:build race

package maven

// raceEnabled reports a race-detector build, whose instrumentation inflates
// allocation counts far beyond the real ones.
const raceEnabled = true

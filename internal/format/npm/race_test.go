//go:build race

package npm

// raceEnabled reports a race-detector build, whose instrumentation inflates
// allocations: the tests that bound them do not apply.
const raceEnabled = true

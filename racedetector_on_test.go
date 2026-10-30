//go:build race

package quickjs_test

// raceDetector is whether the race detector is on, under which a stop is
// noticed at the engine's counted checks only (stopwatch.go).
const raceDetector = true

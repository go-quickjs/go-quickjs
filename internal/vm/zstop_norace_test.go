//go:build !race

package vm

// raceDetector is whether the race detector is on (zstop_race_test.go).
const raceDetector = false

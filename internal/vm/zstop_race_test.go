//go:build race

package vm

// raceDetector is whether the race detector is on, which would report the
// store watchStopForTest makes on purpose: such a test notices a stop at
// the counted checks only, as the public Runtime does.
const raceDetector = true

// Package spanclock is a monotonic clock that resolves spans of
// microseconds on every system: what a cost of tens of microseconds, or a
// benchmark's run of a few milliseconds, is measured by. Go's own clock on
// Windows is the interrupt time, which moves only at the system timer's
// tick, every 0.5 ms or, while no program asks for more, every 15.6 ms: a
// span shorter than that reads as none in most of its measures, and the
// next as a tick. There the clock reads the performance counter, as
// QuickJS's os.now and Node's performance.now do, which moves every 100
// ns; elsewhere it is Go's own, which resolves such spans.
package spanclock

package vm

import "testing"

// newTestRuntime is New for a test or benchmark tb: every runtime the
// package's tests make is made by it.
func newTestRuntime(tb testing.TB, cfg Config) *Runtime {
	return New(cfg)
}

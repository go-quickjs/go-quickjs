// Package sys holds the types the wasm package and host functions share for
// system concerns: a module's exit, and the clocks a module is configured
// with.
package sys

import (
	"context"
	"fmt"
)

// The exit codes a module is closed with when a call ends because its
// context was done, under the runtime's WithCloseOnContextDone. A program is
// assumed not to exit with either code of its own.
const (
	// ExitCodeContextCanceled is the exit code for context.Canceled.
	ExitCodeContextCanceled uint32 = 0xffffffff
	// ExitCodeDeadlineExceeded is the exit code for
	// context.DeadlineExceeded.
	ExitCodeDeadlineExceeded uint32 = 0xefffffff
)

// ExitError is the error a call returns when its module was closed: by
// CloseWithExitCode, possibly from a host function such as an exit
// implementation, or because the call's context was done under the
// runtime's WithCloseOnContextDone.
//
// An exit code of zero means success, any other a failure.
type ExitError struct {
	exitCode uint32
}

var exitZero = &ExitError{}

// NewExitError returns the error of exiting with exitCode.
func NewExitError(exitCode uint32) *ExitError {
	if exitCode == 0 {
		return exitZero
	}
	return &ExitError{exitCode: exitCode}
}

// ExitCode returns the code the module exited with: zero on success.
func (e *ExitError) ExitCode() uint32 { return e.exitCode }

// Error returns "module closed with exit_code(N)", or names the context's
// error for the two context exit codes.
func (e *ExitError) Error() string {
	switch e.exitCode {
	case ExitCodeContextCanceled:
		return fmt.Sprintf("module closed with %s", context.Canceled)
	case ExitCodeDeadlineExceeded:
		return fmt.Sprintf("module closed with %s", context.DeadlineExceeded)
	}
	return fmt.Sprintf("module closed with exit_code(%d)", e.exitCode)
}

// Is reports whether target is an ExitError of the same code, or the context
// error a context exit code stands for, for errors.Is.
func (e *ExitError) Is(target error) bool {
	if t, ok := target.(*ExitError); ok {
		return e.exitCode == t.exitCode
	}
	switch e.exitCode {
	case ExitCodeContextCanceled:
		return target == context.Canceled
	case ExitCodeDeadlineExceeded:
		return target == context.DeadlineExceeded
	}
	return false
}

// ClockResolution is a clock's granularity in nanoseconds: 1000 for a clock
// that counts microseconds.
type ClockResolution uint32

// Walltime returns the current time as seconds since the Unix epoch, with a
// fraction in nanoseconds.
type Walltime func() (sec int64, nsec int32)

// Nanotime returns nanoseconds since an arbitrary point, to measure elapsed
// time with. It never decreases.
type Nanotime func() int64

// Nanosleep sleeps for at least ns nanoseconds.
type Nanosleep func(ns int64)

// Osyield yields the processor, as a spin-wait loop does.
type Osyield func()

// Package jit contains the optional native executor infrastructure: the
// executable memory the new pipeline's code (internal/jit/ssa, mir) runs
// in, and the calls into it and back to Go. It is experimental; the VM
// selects it only with a tagged build and runtime opt-in.
package jit

import "errors"

var (
	// ErrUnavailable means this build or host cannot execute native code.
	ErrUnavailable = errors.New("native execution is unavailable")
	// ErrClosed means the executable allocation has been released.
	ErrClosed = errors.New("native code is closed")
	// ErrCodeBudget means code cannot fit its owner's budget.
	ErrCodeBudget = errors.New("native code memory budget exceeded")
	// ErrCodeSize means a function's code is past MaxCodeBytes: that
	// function's refusal, not the host's (ErrUnavailable).
	ErrCodeSize = errors.New("native code past the size one placement may have")
)

// MaxCodeBytes bounds a single placement of code before page rounding.
const MaxCodeBytes = 1 << 20

// Supported reports whether this build includes a backend. It does not probe
// executable-memory permissions or allocate memory.
func Supported() bool { return supported }

// Package vmhook is what stdlib's node:vm needs of a runtime beyond the public
// API: making a realm a context of a sandbox object, and running a script in a
// realm from inside running code -- without running the job queue, and under
// a timeout of its own.
//
// The quickjs package fills the functions in; the values they take and return
// are *quickjs.Runtime, *quickjs.Realm and quickjs.Value, passed as any so
// that this package need not import it.
package vmhook

import "time"

// Options says how a script is compiled and run.
type Options struct {
	// Filename is what stack traces call the script.
	Filename string
	// LineOffset and ColumnOffset place it within a larger file.
	LineOffset, ColumnOffset int
	// Timeout bounds the run, when it is not zero; running out is an Error
	// with the code ERR_SCRIPT_EXECUTION_TIMEOUT that the caller can catch.
	Timeout time.Duration
}

var (
	// Contextify makes realm a context of sandbox: its global names are the
	// sandbox's properties first, and then its own.
	Contextify func(realm, sandbox any) error
	// Run compiles src and runs it as a script of realm, or of the realm
	// running now when realm is nil, returning its completion value.
	Run func(rt, realm any, src string, opts Options) (any, error)
	// Check compiles src, reporting a SyntaxError if it does not.
	Check func(rt any, src string, opts Options) error
)

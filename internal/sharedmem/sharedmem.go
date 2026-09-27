// Package sharedmem hands a SharedArrayBuffer's memory from one runtime to
// another, which is what agents -- workers, and test262's $262.agent -- share
// memory through.
//
// The quickjs package fills the functions in; the values they take and return
// are *quickjs.Runtime and quickjs.Value, passed as any so that this package
// need not import it. The memory itself is opaque: it is handed from the
// runtime that shared it to the one that attaches it, and to nothing else.
package sharedmem

var (
	// Share returns the memory behind a SharedArrayBuffer, which from then
	// on stays where it is, and whether v is one. A growable buffer whose
	// maximum is too large to reserve cannot be shared.
	Share func(v any) (mem any, ok bool, err error)
	// Attach makes a SharedArrayBuffer of rt, in the realm running now, over
	// memory Share returned.
	Attach func(rt any, mem any) (any, error)
)

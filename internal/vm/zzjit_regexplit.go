//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// A RegExp literal's call of the built-in exec or test, /x/.test(s), given
// a primitive, runs nothing of the script's, and the RegExp it was called
// on -- only ever the call's receiver -- is reachable from nothing once it
// returns: the interpreter gives it back to the literal
// (regexpLiteralDone), and native code, whose literal takes its RegExps
// from a pool, has the call of Go give it back to the pool, lastIndex 0
// again, as the pool's RegExps are made. A literal in a loop then made a
// RegExp each time round in native code alone.
//
// The method native code read is the built-in only where reading it ran
// nothing: where RegExp.prototype is as the realm built it (its layout the
// one the realm was built with, which a getter put in the method's place,
// and anything else a script did to it, leaves for good), its property the
// built-in's.

// jitRegExpLitDone gives l's pool v back, the RegExp of the literal that a
// call jitRegExpLitPool found was done with, lastIndex 0 again, as the
// pool's RegExps are made (jitFillLiteral), where it has room.
func (r *Runtime) jitRegExpLitDone(l *jitLiteral, v Value) {
	pool := l.pool
	if !v.IsObject() || pool.Count >= pool.Size || pool.Count >= abi.PoolCapacity {
		return
	}
	o := v.Object()
	if o.proto() != (*Object)(pool.Proto) || o.class != ClassRegExp || int(o.valn) != 1 {
		return
	}
	ro := (*regexpObject)(unsafe.Pointer(o))
	if ro.rd.re != &ro.re || ro.rd.realm != r.Realm || ro.side.data != any(&ro.rd) || ro.re.Source() != l.lit.Str {
		return
	}
	o.vals()[0] = Int(0)
	pool.Objects[pool.Count] = unsafe.Pointer(o)
	pool.Count++
}

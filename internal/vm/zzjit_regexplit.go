//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// A RegExp literal's call of the built-in exec or test, /x/.test(s), given
// a primitive, runs nothing of the script's, and the RegExp it was called
// on -- only ever the call's receiver -- is reachable from nothing once it
// returns: the tree tier gives it back to the literal (see
// ztree_zzzzzzregexp.go), and native code, whose literal takes its RegExps
// from a pool, has the call of Go give it back to the pool, lastIndex 0
// again, as the pool's RegExps are made. A literal in a loop then made a
// RegExp each time round in native code alone.
//
// The method native code read is the built-in only where reading it ran
// nothing: where RegExp.prototype is as the realm built it (its layout the
// one the realm was built with, which a getter put in the method's place,
// and anything else a script did to it, leaves for good), its property the
// built-in's.

// jitRegExpLitPool is the pool of the RegExp literal whose RegExp the call
// of Go at pc, in, on the VM's stack below sp, of f's code run by e, is a
// call of the built-in exec or test of, given a primitive: the pool the
// call may give the RegExp back to once it returns (jitRegExpLitDone), or
// nil for any other call.
func (r *Runtime) jitRegExpLitPool(e *jitEntry, f *frame, pc int, in bytecode.Instr, sp int) *jitLiteral {
	if in.Op != bytecode.OpCallMethod || e.ssaProg == nil || f == nil || f.cl == nil {
		return nil
	}
	argc := int(in.A)
	callee, recv := r.stack[sp-argc-1], r.stack[sp-argc-2]
	if !callee.IsObject() || !recv.IsObject() || recv.Object().class != ClassRegExp ||
		argc > 0 && r.stack[sp-argc].IsObject() {
		return nil
	}
	switch callee.Object() {
	case r.regexpExecFn:
		if r.regexpLitMethod(false) == nil {
			return nil
		}
	case r.regexpTestFn:
		if r.regexpLitMethod(true) == nil || r.regexpLitMethod(false) == nil {
			return nil
		}
	default:
		return nil
	}
	if r.regexpProtoShape == nil || r.proto.regexp.shape != r.regexpProtoShape {
		return nil
	}
	fn := f.cl.fn
	lpc, ok := e.regexpLitCalls[int32(pc)]
	if !ok {
		lpc = int32(jitRegExpLitPC(e, fn, pc, argc))
		if e.regexpLitCalls == nil {
			e.regexpLitCalls = make(map[int32]int32)
		}
		e.regexpLitCalls[int32(pc)] = lpc
	}
	if lpc < 0 {
		return nil
	}
	for i := range e.literals {
		if l := &e.literals[i]; l.pc == lpc && l.fn == fn && l.lit != nil && l.pool != nil {
			return l
		}
	}
	return nil
}

// jitRegExpLitPC is the pc of the new_regexp whose RegExp the call_method
// at pc, of argc arguments, of fn's code is called on, read off it by the
// get_prop_this of exec or test right after it, or -1: what e's program's
// depths say, the RegExp beneath the method in its slot and nothing
// between them and the call reaching below the arguments.
func jitRegExpLitPC(e *jitEntry, fn *bytecode.Function, pc, argc int) int {
	maps := e.ssaProg.Maps
	if len(maps) != len(fn.Code) || pc >= len(maps) {
		return -1
	}
	recv := maps[pc].Depth - argc - 2
	if recv < 0 {
		return -1
	}
	for q := pc - 1; q >= 1; q-- {
		d := maps[q].Depth
		if d >= recv+2 {
			continue
		}
		in, prev := fn.Code[q], fn.Code[q-1]
		if d != recv+1 || in.Op != bytecode.OpGetPropThis || prev.Op != bytecode.OpNewRegExp || maps[q-1].Depth != recv ||
			int(in.A) >= len(fn.Names) || fn.Names[in.A] != "exec" && fn.Names[in.A] != "test" {
			return -1
		}
		return q - 1
	}
	return -1
}

// jitRegExpLitDone gives l's pool v back, the RegExp of the literal that a
// call jitRegExpLitPool found was done with, lastIndex 0 again, as the
// pool's RegExps are made (jitFillLiteral), where it has room.
func (r *Runtime) jitRegExpLitDone(l *jitLiteral, v Value) {
	pool := l.pool
	if !v.IsObject() || pool.Count >= pool.Size || pool.Count >= abi.PoolCapacity {
		return
	}
	o := v.Object()
	if o.proto != (*Object)(pool.Proto) || o.class != ClassRegExp || !o.extEmbedded() || len(o.vals) != 1 {
		return
	}
	ro := (*regexpObject)(unsafe.Pointer(o))
	if ro.rd.re != &ro.re || ro.rd.realm != r.Realm || ro.side.data != any(&ro.rd) || ro.re.Source() != l.lit.Str {
		return
	}
	o.vals[0] = Int(0)
	pool.Objects[pool.Count] = unsafe.Pointer(o)
	pool.Count++
}

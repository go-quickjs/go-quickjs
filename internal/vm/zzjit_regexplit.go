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

// jitRegExpLitPool is the literal whose RegExp the call of Go at pc, in, on
// the VM's stack below sp, of f's code run by e, is given -- as the
// receiver of the built-in exec or test, given a primitive, or as the first
// argument of a string's replace, split or match that keeps nothing of it
// (regexpArgCall) -- and whose pool the call may give the RegExp back to
// once it returns (jitRegExpLitDone); nil for any other call. The second
// result is the RegExp.
func (r *Runtime) jitRegExpLitPool(e *jitEntry, f *frame, pc int, in bytecode.Instr, sp int) (*jitLiteral, Value) {
	if in.Op != bytecode.OpCallMethod || e.ssaProg == nil || f == nil || f.cl == nil {
		return nil, Undefined
	}
	argc := int(in.A)
	callee, recv := r.stack[sp-argc-1], r.stack[sp-argc-2]
	if !callee.IsObject() {
		return nil, Undefined
	}
	// The RegExp is the receiver (0) or the first argument (1).
	var rx Value
	which := int32(0)
	switch fo := callee.Object(); fo {
	case r.regexpExecFn, r.regexpTestFn:
		if !recv.IsObject() || recv.Object().class != ClassRegExp || argc > 0 && r.stack[sp-argc].IsObject() ||
			r.regexpLitMethod(false) == nil || fo == r.regexpTestFn && r.regexpLitMethod(true) == nil ||
			r.regexpProtoShape == nil || r.proto.regexp.shape != r.regexpProtoShape {
			return nil, Undefined
		}
		rx = recv
	case r.strReplaceFn, r.strSplitFn, r.strMatchFn:
		// Nothing read the RegExp before the call, which made it after
		// the method was read.
		if argc == 0 || !r.stack[sp-argc].IsObject() || r.stack[sp-argc].Object().class != ClassRegExp ||
			!r.regexpArgCall(fo, recv, r.stack[sp-argc:sp]) {
			return nil, Undefined
		}
		rx, which = r.stack[sp-argc], 1
	default:
		return nil, Undefined
	}
	fn := f.cl.fn
	key := int32(pc)<<1 | which
	lpc, ok := e.regexpLitCalls[key]
	if !ok {
		lpc = int32(jitRegExpLitPC(e, fn, pc, argc, which == 1))
		if e.regexpLitCalls == nil {
			e.regexpLitCalls = make(map[int32]int32)
		}
		e.regexpLitCalls[key] = lpc
	}
	if lpc < 0 {
		return nil, Undefined
	}
	for i := range e.literals {
		if l := &e.literals[i]; l.pc == lpc && l.fn == fn && l.lit != nil && l.pool != nil {
			return l, rx
		}
	}
	return nil, Undefined
}

// jitRegExpLitCallee reports, in a few instructions that inline, whether the
// call of Go in, on the VM's stack below sp, is a method call of one of the
// built-ins jitRegExpLitPool asks about: most calls of Go are of none.
func (r *Runtime) jitRegExpLitCallee(in bytecode.Instr, sp int) bool {
	if in.Op != bytecode.OpCallMethod {
		return false
	}
	p := r.stack[sp-int(in.A)-1].ref
	re := r.Realm
	return p == unsafe.Pointer(re.regexpExecFn) || p == unsafe.Pointer(re.regexpTestFn) ||
		p == unsafe.Pointer(re.strReplaceFn) || p == unsafe.Pointer(re.strSplitFn) || p == unsafe.Pointer(re.strMatchFn)
}

// jitRegExpLitPC is the pc of the new_regexp whose RegExp the call_method
// at pc, of argc arguments, of fn's code is given -- as its first argument
// where arg is set, or else as its receiver, read off it by the
// get_prop_this of exec or test right after it -- or -1: what e's
// program's depths say, the RegExp in its slot, and nothing between it and
// the call reaching below what comes after it.
func jitRegExpLitPC(e *jitEntry, fn *bytecode.Function, pc, argc int, arg bool) int {
	maps := e.ssaProg.Maps
	if len(maps) != len(fn.Code) || pc >= len(maps) {
		return -1
	}
	if arg {
		at := maps[pc].Depth - argc
		if at < 0 {
			return -1
		}
		for q := pc - 1; q >= 0; q-- {
			d := maps[q].Depth
			if d > at+1 || d == at+1 && jitPushesOnly(fn.Code[q].Op) {
				continue
			}
			if d != at || fn.Code[q].Op != bytecode.OpNewRegExp {
				return -1
			}
			return q
		}
		return -1
	}
	recv := maps[pc].Depth - argc - 2
	if recv < 0 {
		return -1
	}
	for q := pc - 1; q >= 1; q-- {
		d := maps[q].Depth
		if d > recv+2 || d == recv+2 && jitPushesOnly(fn.Code[q].Op) {
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

// jitPushesOnly reports whether op pushes a value and reads none on the
// stack: the first instruction of an argument, after the RegExp, which
// leaves the RegExp where it is -- where one that reads it, a copy of it
// stored, would let it be reached.
func jitPushesOnly(op bytecode.Op) bool {
	switch op {
	case bytecode.OpPushConst, bytecode.OpPushUndef, bytecode.OpPushNull, bytecode.OpPushTrue, bytecode.OpPushFalse,
		bytecode.OpPushThis, bytecode.OpPushInt, bytecode.OpPushEmptyString, bytecode.OpGetLocal, bytecode.OpGetLocalCheck,
		bytecode.OpGetLocal2, bytecode.OpGetLocalIndex, bytecode.OpLocalBinImm,
		bytecode.OpGetUpvalue, bytecode.OpGetUpvalueCheck, bytecode.OpGetGlobal, bytecode.OpGetGlobalOpt,
		bytecode.OpNewRegExp, bytecode.OpNewObject, bytecode.OpNewArray, bytecode.OpClosure:
		return true
	}
	return false
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

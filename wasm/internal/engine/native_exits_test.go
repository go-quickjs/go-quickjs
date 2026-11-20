package engine

import (
	"fmt"
	"strings"
	"unsafe"
)

// nativeExit is an exit of native code to a Go handler a test saw: the
// instruction's operation, its X where it escapes to a table, its pc, and
// why it left.
type nativeExit struct {
	op, x uint8
	pc    int32
	why   string
}

func (e nativeExit) String() string {
	return fmt.Sprintf("op %d x %d at pc %d: %s", e.op, e.x, e.pc, e.why)
}

// Why an exit left native code (nativeExit.why): for the exec's room, a
// call at a depth its frames, call records or chunks do not reach yet --
// an exec fresh from the pool makes them as Go's calls first need them,
// which the pool decides (under the race detector it drops a quarter of
// what is put back) -- and otherwise for the instruction itself.
const (
	exitRoom = "a call the exec has no frame, record or chunk room for yet"
	exitCold = "a call of a function not compiled yet"
	exitOp   = "the instruction is Go's"
)

// logNativeExits runs f with native code's exits to Go handlers recorded,
// each with why it left, and returns them.
func logNativeExits(inst *Instance, f func()) (exits []nativeExit) {
	table := nativeExitTable
	t := testOf(inst)
	saved := t.exitTable
	t.exitTable = &table
	defer func() { t.exitTable = saved }()
	for i := range table {
		table[i] = func(l unsafe.Pointer, w uint64, pc int32, fr *frame) int32 {
			e := nativeExit{op: uint8(w), pc: pc, why: exitOp}
			if e.op == uint8(rExt) || e.op == uint8(rSIMD) {
				e.x = uint8(w >> 8)
			}
			if e.op == uint8(rCall) || e.op == uint8(rExt) && (e.x == uint8(xCallIndirect) || e.x == uint8(xCallRef)) {
				e.why = nativeCallExit(w, fr)
			}
			exits = append(exits, e)
			return nativeExitRun(l, w, pc, fr)
		}
	}
	f()
	return exits
}

// nativeCallExit says why the call w of frame fr left native code, as far
// as the exec's state tells.
func nativeCallExit(w uint64, fr *frame) string {
	x := fr.x
	if x.depth+1 >= len(x.frames) || len(x.calls) >= cap(x.calls) || x.nc >= len(x.nchunks) || x.rc >= len(x.rchunks) {
		return exitRoom
	}
	if uint8(w) == uint8(rCall) {
		s := &fr.rc.sites[uint32(w>>32)]
		fn := fr.inst.Funcs[s.fn]
		if fn.Inst == fr.inst {
			cd := fn.Inst.Code.fns[fn.Index].Load()
			if cd == nil {
				return exitCold
			}
			if x.nsp+cd.nslots > len(x.nchunks[x.nc]) || cd.refs && x.rsp+cd.rslots > len(x.rchunks[x.rc]) {
				return exitRoom
			}
		}
	}
	return exitOp
}

// exitsBut returns the exits of exits for none of the reasons in skip, and
// their description.
func exitsBut(exits []nativeExit, skip ...string) (int, string) {
	var b strings.Builder
	n := 0
	for _, e := range exits {
		counted := true
		for _, s := range skip {
			if e.why == s {
				counted = false
			}
		}
		if counted {
			n++
		}
		fmt.Fprintf(&b, "\n\t%v", e)
	}
	return n, b.String()
}

// callExits returns the exits of exits at calls: call, call_indirect and
// call_ref.
func callExits(exits []nativeExit) []nativeExit {
	var calls []nativeExit
	for _, e := range exits {
		if e.op == uint8(rCall) || e.op == uint8(rExt) && (e.x == uint8(xCallIndirect) || e.x == uint8(xCallRef)) {
			calls = append(calls, e)
		}
	}
	return calls
}

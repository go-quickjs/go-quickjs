//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// A destructuring's or a loop's close of a generator it left suspended,
// which native code calls Go for (ssa.GoIterClose). Its file sorts past
// the layout lock's anchor J, jitLeafGo, so that it moves none of the
// JIT's leaf calls.
// jitIterClose is a close of a cursor native code found open
// (ssa.GoIterClose), the context's first operand, where closing it runs no
// script, as iterCloseNormal closes it: a done cursor's, nothing; a
// generator's that jitGenCloseable finds the close of plain, the generator
// finished as its return would finish it -- the return path's one return,
// nothing captured to close -- and the cursor done. Any other it refuses,
// which leaves for Go to close after an exit: a destructuring that left
// its generator suspended had left native code at every turn.
func (r *Runtime) jitIterClose(ctx *abi.Context) bool {
	cur := cursorOf(jitGoArg(ctx, 0))
	if cur == nil || cur.forIn || cur.arr != nil || cur.delegation || cur.asyncIter {
		return false
	}
	if cur.done {
		return true
	}
	g := r.jitGenCloseable(cur.iter)
	if g == nil {
		return false
	}
	cur.done = true
	g.state, g.saved = genCompleted, nil
	return true
}

// jitGenCloseable is iter's generator where a close of it runs no script
// and throws nothing, or nil: a synchronous generator of register code a
// step may resume natively (jitRegGenCallable), of this realm, suspended
// at a yield -- not running, not in a yield* -- nothing captured open on
// its registers, the return path past the yield its one return of the
// value sent in, which no finally clause, loop's close or disposal comes
// before, and the iterator's return method, found up a chain of ordinary
// objects' data properties, the realm's %GeneratorPrototype%.return; and
// the frames and the stack have room for the return's run, which would
// otherwise throw.
func (r *Runtime) jitGenCloseable(iter Value) *generator {
	if !iter.IsObject() || r.genReturnFn == nil {
		return nil
	}
	it := iter.Object()
	if it.class != ClassGenerator {
		return nil
	}
	g := genOf(it)
	if g == nil || !g.reg || g.async || g.cl == nil || g.cl.reg == nil || g.state != genSuspendedYield || !g.started ||
		g.delegating || g.saved != nil || g.cl.realm != nil && g.cl.realm != r.Realm || !jitRegGenCallable(g.cl) {
		return nil
	}
	c := g.cl.reg
	pc := int(g.pc)
	if pc < 1 || pc+1 >= len(c.rc.Code) || r.frameDepth+1 >= r.maxFrames || r.stackTop+int(c.numRegs) > len(r.stack) {
		return nil
	}
	y, ret := regAt(c.code, pc-1), regAt(c.code, pc+1)
	if y.Op() != bytecode.RYield || ret.Op() != bytecode.RRet || ret.A() != y.A() || c.rc.HandlerAt(pc+1) != nil {
		return nil
	}
	o := it
	for range 8 {
		if o.class != ClassGenerator && o.class != ClassObject {
			return nil
		}
		if p := o.getOwn(atomReturn); p.ok() {
			if p.isAccessor() || p.value().ref != unsafe.Pointer(r.genReturnFn) {
				return nil
			}
			return g
		}
		if o = o.proto(); o == nil {
			return nil
		}
	}
	return nil
}

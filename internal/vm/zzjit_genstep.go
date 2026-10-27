//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// A loop's step over a generator, natively.
//
// A for...of's or a destructuring's step over a generator is, in native
// code, a native call of the generator's code (ssa.CallSite's Gens), as
// V8's optimized code calls a generator's resumption: the step checks the
// cursor's iterator is a generator of a function it has met, suspended,
// whose next is the realm's, and jumps to its code's generator entry
// (mir's genEntry), which makes the generator's frame -- its locals the
// generator's own, its operands past the VM's stack's top, what its
// suspension saved on them and the value sent in -- in the next context,
// as a native callee's, and goes on at the entry for the generator's place.
// A yield there writes the frame, saves the generator's state as Go's
// suspension would -- its place, the operands beneath the value, the
// handlers in force -- and returns the value to the step; a return
// finishes the generator and returns the step's done. So a
// loop over a generator steps it with neither an exit nor a call of Go,
// where each step had been a call of Go from native code, which resumed
// the generator's tree, or its code from Go.
//
// Anything else leaves native code as a native callee does, and Go makes
// the generator's frame from its context, as the step's resumption would
// have (jitGenFrame), finishes it -- a yield it suspends at is Go's
// suspension -- and saves what its suspension, or its end, leaves
// (jitGenDone), the step's caller going on with the result. A generator a
// step resumes natively is one of these (jitGenCallable): synchronous, no
// yield*, whose captured locals none -- its frame's locals the generator's
// alone -- and no call in tail position.

// jitGenStep is a loop's step at pc that has met generators of the
// closures cls (jitGenStepSeen).
type jitGenStep struct {
	pc  int32
	cls []*closure
}

// jitGenTargets is how many generators' functions a step resumes
// natively, as a call calls jitCallTargets functions natively.
const jitGenTargets = 2

// jitGenResumesMax bounds the places a generator's code resumes it at
// natively (ssa.Func's GenResumes): its first, past its parameters, and
// after its yields, each a check at every resumption.
const jitGenResumesMax = 16

// jitGenCallable reports whether a loop's step may resume a generator of
// fn natively (ssa.Func's GenEntry, jitcompile.GeneratorResumable): a
// synchronous generator whose body has no yield*, whose locals no closure
// captures -- its frame's locals are then the generator's alone, which no
// cell holds -- and no call in tail position, which a generator's frame
// does not give up.
func jitGenCallable(fn *bytecode.Function) bool { return jitcompile.GeneratorResumable(fn) }

// jitGenResumes are the places a step resumes a generator of fn natively at
// (ssa.Func's GenResumes): past its parameters, where it starts, and after
// each yield, where code compiled from p, built as f, has an entry.
func jitGenResumes(fn *bytecode.Function, p *ir.Program, f *ssa.Func) []ssa.GenResume {
	has := func(pc int) bool {
		for _, e := range f.Entries {
			if e.PC == pc {
				return true
			}
		}
		return false
	}
	var out []ssa.GenResume
	for pc, in := range fn.Code {
		if pc+1 >= len(fn.Code) || pc+1 >= len(p.Maps) || p.Maps[pc+1].Depth < 0 || !has(pc+1) {
			continue
		}
		switch d := p.Maps[pc+1].Depth; in.Op {
		case bytecode.OpEndParams:
			out = append(out, ssa.GenResume{PC: pc + 1, Saved: d})
		case bytecode.OpYield:
			if d >= 1 {
				out = append(out, ssa.GenResume{PC: pc + 1, Saved: d - 1, Started: true})
			}
		}
		if len(out) == jitGenResumesMax {
			break
		}
	}
	return out
}

// GenSteps is ssa.GenStepFeedback's: the generators the step at pc has met
// (jitGenStepSeen), each of the code's environment and realm, called
// through its function's link, which the code holds.
func (fb *jitFeedback) GenSteps(pc int) []ssa.GenStep {
	e := fb.ownEntry()
	if e == nil || fb.root != nil || fb.cl == nil {
		return nil
	}
	for _, st := range e.genSteps {
		if int(st.pc) != pc {
			continue
		}
		var out []ssa.GenStep
		for _, cl := range st.cls {
			if cl.env != fb.cl.env || cl.realm != fb.cl.realm {
				continue
			}
			link := fb.r.jit.linkOf(cl.fn)
			fb.links = append(fb.links, link)
			out = append(out, ssa.GenStep{
				Fn: uintptr(unsafe.Pointer(cl.fn)), Env: uintptr(unsafe.Pointer(cl.env)), Realm: uintptr(unsafe.Pointer(cl.realm)),
				Entry: uintptr(unsafe.Pointer(&link.gen)), Count: uintptr(unsafe.Pointer(&link.in)),
			})
		}
		return out
	}
	return nil
}

// GenNext is ssa.GenStepFeedback's: the realm's %GeneratorPrototype%.next,
// an object as long-lived as the realm.
func (fb *jitFeedback) GenNext() uintptr { return uintptr(unsafe.Pointer(fb.r.genNextFn)) }

// jitGenStepSeen notes a loop's step at pc of e's code, cl's, which Go made
// from native code, of cursor: one over a generator native code may resume
// (jitGenCallable), of cl's environment and realm, stepped by the realm's
// next, which the code resumes natively once compiled again (ssa.CallSite's
// Gens), as a call learns its target.
func (r *Runtime) jitGenStepSeen(e *jitEntry, cl *closure, pc int, cursor Value) {
	st := cursorOf(cursor)
	if st == nil || st.done || st.arr != nil || st.forIn || !st.iter.IsObject() || r.genNextFn == nil ||
		st.next.ref != unsafe.Pointer(r.genNextFn) || cl == nil {
		return
	}
	g, ok := st.iter.Object().dataOf().(*generator)
	if !ok || g.cl == nil || g.cl.env != cl.env || g.cl.realm != cl.realm || g.cl.realm != nil && g.cl.realm != r.Realm ||
		!jitGenCallable(g.cl.fn) {
		return
	}
	for i := range e.genSteps {
		gs := &e.genSteps[i]
		if int(gs.pc) != pc {
			continue
		}
		for _, c := range gs.cls {
			if c.fn == g.cl.fn {
				return
			}
		}
		if len(gs.cls) >= jitGenTargets {
			return
		}
		gs.cls = append(gs.cls, g.cl)
		e.genLearned, e.reopt = true, true
		return
	}
	e.genSteps = append(e.genSteps, jitGenStep{pc: int32(pc), cls: []*closure{g.cl}})
	e.genLearned, e.reopt = true, true
}

// jitGenLevel is the generator a native call's context c, a level of
// closure cl that left as kind says, runs, where it is a loop's step's
// resumption of it (mir's genEntry), or nil: no other native call runs a
// generator's code -- a call of a generator function makes its object,
// which Go does (abi.ExitEnter).
func jitGenLevel(c *abi.Context, cl *closure, kind uint64) *generator {
	if c.Live != abi.LiveCall || kind == abi.ExitEnter || cl == nil || !cl.fn.Generator || c.GenObject == 0 || c.GenCursor == 0 {
		return nil
	}
	// Addresses native code wrote, which the step's frame keeps alive.
	if g := *(**generator)(unsafe.Pointer(&c.GenObject)); g.cl == cl {
		return g
	}
	return nil
}

// jitGenCursor is the cursor of the step whose resumption of a generator
// context c runs (jitGenLevel).
func jitGenCursor(c *abi.Context) *iterState {
	return *(**iterState)(unsafe.Pointer(&c.GenCursor))
}

// jitPushGenNext pushes the frame of the generator's next a step's
// resumption of a generator calls, st's, as stepIter makes it, below the
// generator's own, for a stack trace to show.
func (r *Runtime) jitPushGenNext(st *iterState) {
	f := r.pushFrame()
	gn := st.next.Object()
	f.cl = nil
	f.native = gn.fn().name
	f.this, f.newTarget, f.callee, f.args = st.iter, Undefined, gn, nil
	f.handlers, f.openUpvalues = f.handlers[:0], f.openUpvalues[:0]
	f.thisRef, f.withScopes, f.evalVars, f.savedSP = nil, nil, nil, 0
}

// jitGenFrame makes the frame of l, a level of a generator a step resumed
// natively (jitGenLevel), which left native code, as the step's resumption
// would have made it (resumeTree), its next's frame below it: its locals
// the generator's, its operands at the level's base, its handlers those in
// force at its exit, or, past, after its call, which a stack trace shows.
// The records its exit left, the collector marking, are written to it.
func (r *Runtime) jitGenFrame(l *jitNativeLevel, c *abi.Context, past bool) *frame {
	s := r.jit
	g, fn := l.gen, l.cl.fn
	l.top = l.base
	r.jitPushGenNext(l.cursor)
	f := r.pushFrame()
	f.cl = g.cl
	f.locals = g.vals[g.nargs:]
	f.base = l.base
	f.pc = uint32(l.pc)
	if past {
		f.pc++
	}
	f.this, f.callee, f.args = g.this, g.callee, g.vals[:g.nargs:g.nargs]
	f.handlers, f.openUpvalues = g.handlers, g.openUpvalues
	g.frameRare(f)
	f.native, f.savedSP, f.paramsOnly = "", 0, false
	tc := &f.tc
	tc.r, tc.f, tc.cl, tc.locals = r, f, g.cl, f.locals
	tc.closureCode = g.cl.closureCode
	tc.stack = r.stack[l.base : l.base+fn.MaxStack]
	e := r.jitEntryOf(l.cl)
	if e != nil && c.Records != 0 {
		saved := s.ssaCtx
		s.ssaCtx = c
		r.jitApplyRecords(f, e.this, c)
		s.ssaCtx = saved
	}
	r.jitMaterialize(f, e)
	if e != nil {
		jitHandlers(f, e.ssaProg, int(l.pc))
	}
	return f
}

// jitGenDone ends the frame of l, a generator's level (jitGenFrame), which
// returned v or ended in err, as finishResume ends a resumption: suspended,
// what the next resumption needs saved, and its value the step's result;
// finished, by a return or a throw, the generator done and the step's
// cursor too -- the step's result its done, the uninitialized value, or for
// a destructuring's step undefined, or the throw. The frame is then popped,
// with its next's below it.
func (r *Runtime) jitGenDone(f *frame, l *jitNativeLevel, v Value, err error) (Value, error) {
	g := l.gen
	defer func() {
		f.detachSuspended()
		r.frameDepth -= 2
		r.stackTop = l.top
	}()
	if s, ok := err.(*suspendSignal); ok {
		g.pc = f.pc
		if g.stack = g.stack[:0]; f.savedSP > f.base {
			g.stack = append(g.stack, r.stack[f.base:f.savedSP]...)
		}
		g.handlers, g.openUpvalues = f.handlers, f.openUpvalues
		g.saveWith(f.withScopes)
		g.delegating, g.state, g.started = s.delegate, genSuspendedYield, true
		// A yield's: the generator has no yield* (jitGenCallable), whose
		// suspension hands out a result object.
		v = s.value
		s.value = Undefined
		return v, nil
	}
	for _, u := range f.openUpvalues {
		u.close()
	}
	g.state, g.stack = genCompleted, g.stack[:0]
	l.cursor.done = true
	switch {
	case err != nil:
		return Undefined, err
	case l.fill:
		return Undefined, nil
	}
	return uninitialized, nil
}

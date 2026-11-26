package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// The register interpreter's calls and slow path, after its loop
// (reg_run.go): rhCallW first, which the layout lock's pad W
// (reg_run_a_layoutpad.go) keeps where it was against runReg whatever
// the loop's size does (docs/optimizing.md, "The layout lock").

// rhCallW is call_w's and call_method_w's handler: a call whose callee's
// frame starts at the caller's window, R[A], its parameters the arguments
// where they lie. It writes the call's value to R[B] itself, and is the
// call path's first function, called from the loop with nothing between.
//
// A callee the site called last, of the form's own layout and with nothing
// of its surroundings to establish but a this its cache holds, is entered
// here: its frame is the window, the frame record the few fields a stack
// trace and a closure read, and runReg is the only call between the two
// loops. What the frame needs is read from the callee's register code,
// whose facts are the function's, precomputed. Anything else is
// regCallMiss's.
func rhCallW(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
	r, f, c, wb := st.r, st.f, st.c, st.wb
	st.kept.pc = pc + 1
	base := wb + int(w.A())
	win := regWindow(r.stack, base)
	if calls := f.cl.regCalls; uint(w.C()) < uint(len(calls)) {
		e := &calls[w.C()]
		// The callee's tag and pointer read as words: read as a Value, the
		// tag goes through a float register to be compared.
		if regIsObject(&win[0]) && (*Object)(win[0].ref) == e.callee {
			// A leaf is answered first, before the frame's pc is written:
			// its body reads nothing of the frame and throws nothing.
			if e.kind&regCallLeaf != 0 {
				if leaf := e.rc.leaf; leaf >= regLeafArg {
					v := e.rc.leafVal
					if p := int(e.rc.leafArg); leaf == regLeafArg {
						v = Undefined
						if p < int(w.X()) {
							v = win[2+p]
						}
					}
					regWindow(r.stack, wb)[w.B()] = v
					return pc + 1
				}
				if e.rc.leaf == regLeafCode {
					if v, ok := r.regLeafRun(e.cl, e.rc, base, int(w.X())); ok {
						regWindow(r.stack, wb)[w.B()] = v
						return pc + 1
					}
					if e.cl.pureMiss >= pureMissLimit {
						// A body that keeps meeting what it cannot run
						// without a frame is entered with one from now on.
						e.kind &^= regCallOut | regCallLeaf
					}
					f.pc = uint32(pc) + 1
					return st.done(r.regCallMiss(f, c, w, wb, pc))
				}
				// The this is given to the body, not written to the
				// window: a miss writes it (regCallSlow).
				this, ok := win[1], true
				switch e.kind &^ (regCallOut | regCallLeaf) {
				case regCallThis:
					this = e.this
				case regCallObjThis:
					ok = this.IsObject()
				}
				if ok {
					var v Value
					argc := int(w.X())
					if e.rc.leaf == regLeafPure {
						// pureCallAt checks the closure's misses.
						r.pureRefused = false
						v, ok = r.pureCallAt(e.callee.fn(), this, win[2:2+argc], 0, true)
					} else if ok = this.IsObject(); ok {
						v, ok = r.leafCall(e.cl, this.Object(), win[2:2+argc])
					}
					if ok {
						regWindow(r.stack, wb)[w.B()] = v
						return pc + 1
					}
				}
				f.pc = uint32(pc) + 1
				return st.done(r.regCallMiss(f, c, w, wb, pc))
			}
			f.pc = uint32(pc) + 1
			if e.kind == regCallNative {
				return st.done(r.regNative(e.callee, w, wb))
			}
			if e.kind == regCallMarked {
				v, err := r.regMarkedCall(f, c, base, int(w.X()), pc, c.rc.Marks[pc], regAuxAt(c, pc))
				if err == nil {
					regWindow(r.stack, wb)[w.B()] = v
				}
				return st.done(err)
			}
			ok := e.kind == regCallEnter
			if e.kind == regCallThis {
				win[1] = e.this
				ok = true
			} else if e.kind == regCallObjThis {
				ok = win[1].IsObject()
			}
			if ok {
				o, cl, rc := e.callee, e.cl, e.rc
				argc := int(w.X())
				top := base + int(rc.numRegs)
				if top > len(r.stack) || r.frameDepth >= r.maxFrames {
					return st.done(r.throwRangeError("maximum call stack size exceeded"))
				}
				if r.interruptCounter--; r.interruptCounter <= 0 {
					if err := r.checkInterruptNow(); err != nil {
						return st.done(err)
					}
				}
				saved := r.stackTop
				if top > saved {
					r.stackTop = top
					if top > r.stackHigh {
						r.stackHigh = top
					}
				}
				if argc > int(rc.keep) {
					// The arguments past the parameters, which the
					// bindings overwrite: f.arguments reads them.
					r.regKeepWindow(base, int(rc.keep), argc)
				}
				// Parameters no argument was given for, and the bindings
				// past them.
				for i := min(argc, int(rc.params)); i < int(rc.params+rc.inits); i++ {
					win[2+i] = Undefined
				}
				off := r.frameDepth - r.frameBase
				if uint(off) >= uint(len(r.cur)) {
					r.seekFrameBlock()
					off = r.frameDepth - r.frameBase
				}
				r.frameDepth++
				if r.frameDepth > r.frameHigh {
					r.frameHigh = r.frameDepth
				}
				nf := &r.cur[off]
				nf.cl = cl
				nf.callee = o
				nf.base = base
				nf.pc = 0
				if len(nf.openUpvalues) != 0 {
					nf.openUpvalues = nf.openUpvalues[:0]
				}
				if nf.thisRef != nil || nf.withScopes != nil || nf.evalVars != nil || nf.native != "" || nf.args != nil {
					regClearFrame(nf)
				}
				if rc.newTarget {
					nf.newTarget = Undefined
				}
				nf.regKind = regFrameCall
				var v Value
				var err error
				if jitBuilt && cl.jitEnters {
					// Native code made from its register code, which the
					// site may have been filled before (jitRegEnter).
					v, err = r.jitRegEnter(nf, cl, rc, base, 0)
				} else {
					v, err = r.runReg(nf, rc, 0)
				}
				nf.regKind = 0
				if rc.ncap != 0 && len(nf.openUpvalues) != 0 {
					regCloseUpvalues(nf)
				}
				r.frameDepth--
				r.stackTop = saved
				if err != nil {
					return st.done(r.regCallWEnd(nf, regFrameCall, err, saved, wb+int(w.B())))
				}
				regWindow(r.stack, wb)[w.B()] = v
				return pc + 1
			}
		} else if regIsObject(&win[0]) && regSameTemplate(e, (*Object)(win[0].ref)) {
			// Another closure of the callee's template, which the cache is
			// now made: the call made again, which the cache answers.
			return pc
		}
	}
	f.pc = uint32(pc) + 1
	return st.done(r.regCallMiss(f, c, w, wb, pc))
}

package engine

// frameAt returns the frame object for call depth d, reused from one call
// to the next.
func (x *exec) frameAt(d int) *frame {
	for len(x.frames) <= d {
		x.frames = append(x.frames, &frame{x: x})
	}
	return x.frames[d]
}

// codeOf returns wasm function fn's compiled body.
func codeOf(fn *Function) *compiled { return fn.Inst.Code.get(fn.Index) }

// run runs wasm function fn's body in a frame of slots l and r, holding its
// arguments, and leaves its results there. The frame was taken at stack
// position (nc, nsp, rc, rsp), where a tail call takes its callee's.
func (x *exec) run(fn *Function, code *compiled, l []uint64, r []Ref, nc, nsp, rc, rsp int) {
	x.depth++
	if x.depth > MaxCallDepth {
		x.depth--
		trap(TrapStackOverflow)
	}
	if x.poll--; x.poll <= 0 {
		x.pollContext()
	}
	x.calls = append(x.calls, callSite{fn: fn})
	if jitBuilt {
		if c, ok := x.runNative(fn, code, l); ok {
			if c == sigTail {
				f := x.frameAt(x.depth)
				f.l, f.r, f.inst = l, r, fn.Inst
				x.tailChain(f, code, l, r, nc, nsp, rc, rsp)
			}
			x.calls = x.calls[:len(x.calls)-1]
			x.depth--
			return
		}
	}
	f := x.frameAt(x.depth)
	f.l, f.r, f.inst = l, r, fn.Inst
	c := runSeq(f, code.body)
	if c >= 0 {
		runSeq(f, code.epi)
	} else if c == sigTail {
		x.tailChain(f, code, l, r, nc, nsp, rc, rsp)
	}
	x.calls = x.calls[:len(x.calls)-1]
	x.depth--
}

// tailChain runs the calls a return_call asked for, each in place of the
// last, and leaves the last one's results in the slots l and r of the frame
// the chain began in.
func (x *exec) tailChain(f *frame, code *compiled, l []uint64, r []Ref, nc, nsp, rc, rsp int) {
	results := code.results
	for {
		fn := x.tail
		x.tail = nil
		x.calls[len(x.calls)-1] = callSite{fn: fn}
		if fn.Host != nil {
			res := x.hostCall(fn, x.tailArgs)
			putVals(l, r, results, res)
			return
		}
		callee := codeOf(fn)
		// The callee's frame replaces the last one.
		x.nc, x.nsp, x.rc, x.rsp = nc, nsp, rc, rsp
		nl, _, _ := x.takeNum(callee.nslots)
		nr, _, _ := x.takeRef(callee.rslots)
		putVals(nl, nr, callee.params, x.tailArgs)
		f.l, f.r, f.inst = nl, nr, fn.Inst
		if jitBuilt {
			if c, ok := x.runNative(fn, callee, nl); ok {
				if c == sigTail {
					continue
				}
				copyResults(l, r, nl, nr, results)
				return
			}
		}
		c := runSeq(f, callee.body)
		if c == sigTail {
			continue
		}
		runSeq(f, callee.epi)
		copyResults(l, r, nl, nr, results)
		return
	}
}

// putVals stores vals into the slots at refs.
func putVals(l []uint64, r []Ref, refs []slotRef, vals []Val) {
	for i, s := range refs {
		v := vals[i]
		if s.ref {
			r[s.i] = v.R
			continue
		}
		l[s.i] = v.N
		if s.v128 {
			l[s.i+1] = v.Hi
		}
	}
}

// getVals loads vals from the slots at refs.
func getVals(l []uint64, r []Ref, refs []slotRef, vals []Val) {
	for i, s := range refs {
		if s.ref {
			vals[i] = Val{R: r[s.i]}
			continue
		}
		vals[i] = Val{N: l[s.i]}
		if s.v128 {
			vals[i].Hi = l[s.i+1]
		}
	}
}

// copyResults copies results at refs from one frame's slots to another's.
func copyResults(dl []uint64, dr []Ref, sl []uint64, sr []Ref, refs []slotRef) {
	if len(dl) > 0 && len(sl) > 0 && &dl[0] == &sl[0] {
		return
	}
	for _, s := range refs {
		if s.ref {
			dr[s.i] = sr[s.i]
			continue
		}
		dl[s.i] = sl[s.i]
		if s.v128 {
			dl[s.i+1] = sl[s.i+1]
		}
	}
}

// callGeneric calls fn with args, storing its results in results. The
// calling frame, if any, is recorded as calling from offset.
func (x *exec) callGeneric(fn *Function, args, results []Val) {
	if fn.Host != nil {
		res := x.hostCall(fn, args)
		copy(results, res)
		return
	}
	code := codeOf(fn)
	nc, nsp, rc, rsp := x.nc, x.nsp, x.rc, x.rsp
	l, _, _ := x.takeNum(code.nslots)
	r, _, _ := x.takeRef(code.rslots)
	putVals(l, r, code.params, args)
	x.run(fn, code, l, r, nc, nsp, rc, rsp)
	getVals(l, r, code.results, results)
	x.nc, x.nsp, x.rc, x.rsp = nc, nsp, rc, rsp
}

// hostCall calls host function fn with args, returning its results, valid
// until the next host call.
func (x *exec) hostCall(fn *Function, args []Val) []Val {
	var caller *Instance
	if n := len(x.calls); n > 0 && x.calls[n-1].fn != nil {
		caller = x.calls[n-1].fn.Inst
	}
	x.depth++
	if x.depth > MaxCallDepth {
		x.depth--
		trap(TrapStackOverflow)
	}
	x.calls = append(x.calls, callSite{fn: fn})
	n := len(fn.Sig.Results)
	if cap(x.hostResults) < n {
		x.hostResults = make([]Val, n)
	}
	res := x.hostResults[:n]
	clear(res)
	// The host may call back into wasm, which uses the scratch buffers.
	x.hostResults = nil
	args = append([]Val(nil), args...)
	err := fn.Host(x.ctx, caller, args, res)
	x.hostResults = res
	if err != nil {
		switch e := err.(type) {
		case *Trap:
			panic(e)
		case *Exception:
			panic(e)
		}
		panic(hostError{err})
	}
	x.calls = x.calls[:len(x.calls)-1]
	x.depth--
	return res
}

// callArgs evaluates arguments into a fresh Val slice.
func evalArgs(f *frame, args []func(*frame) Val) []Val {
	vals := make([]Val, len(args))
	for i, a := range args {
		vals[i] = a(f)
	}
	return vals
}

package engine

// delegation is an exception a delegate passes to the construct at index
// target of frame f's control stack, and the handlers of the constructs
// around it.
type delegation struct {
	exn    *Exception
	f      *frame
	target int
}

// tryState is the execution state a handler restores, as it was when its
// construct was entered.
type tryState struct {
	depth, calls, caught, vals int
	nc, nsp, rc, rsp           int
	// jit is the number of native code's levels running.
	jit int
}

func saveTry(x *exec) tryState {
	return tryState{x.depth, len(x.calls), len(x.caught), len(x.vals), x.nc, x.nsp, x.rc, x.rsp, x.jitLevel()}
}

func (s tryState) restore(x *exec) {
	x.setJITLevel(s.jit)
	x.depth = s.depth
	x.calls = x.calls[:s.calls]
	x.caught = x.caught[:s.caught]
	x.vals = x.vals[:s.vals]
	x.nc, x.nsp, x.rc, x.rsp = s.nc, s.nsp, s.rc, s.rsp
	x.tail = nil
}

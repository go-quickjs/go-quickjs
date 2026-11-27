//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
)

// A compile in the background's own function lowered on its worker: the
// runtime's goroutine hands the compile over with the function's hints as
// they are, copied (jitRegHintsCopy), and its snapshot gathered with no
// program (jitRegSnapshotOf's p nil); the worker's first step lowers the
// function (lowerRoot), for each kind that shares its program at once
// (jitRegLowerKinds), or takes the programs the runtime's lowering of it
// had made with those hints, and gives its snapshot the program
// (jitSnapshot's program), which binds and derives the answers at its
// PCs; and the install keeps the programs in the function's lowering
// (regKeepLoweredAt), as one lowered on the goroutine is kept. A lowering
// is of the register code, which is never rewritten, the function, the
// kind and the hints, whose lists only grow, so the worker makes the
// program the goroutine would.

// jitRegRoot is what a compile in the background's worker lowers its
// function's program of: its register code, its function, its hints as
// they were when it was handed over, the programs the runtime's lowering
// had made of them (nil for none), and what decides the program's kind
// where the function's is refused as host operations alone (jitRegCompile):
// whether it is compiled for its native callers (callee), and whether a
// call it makes is native (callsNatively).
type jitRegRoot struct {
	rc            *bytecode.RegCode
	h             jitcompile.Hints
	cached        *jitLowered
	callee        bool
	callsNatively bool
	// got is what the worker lowered, or took of cached; lowered marks
	// it done (lowerRoot), err the lowering's refusal, callee and hostOnly
	// the entry's marks of the kind chosen (jitRegEntry's).
	got                  jitLowered
	lowered              bool
	err                  error
	asCallee, asHostOnly bool
}

// lowerRoot lowers j's function's program, once, for its compile on the
// worker -- or on the runtime's goroutine for one no worker ran -- as
// jitRegCompile's lowering does on the spot: its own entry's, or, refused
// as host operations alone, its native callers' (LowerRegInline) where it
// is compiled for them, or a hot one's (LowerRegHot) where a call it makes
// is native; the program given to its feedback and its snapshot, which
// binds and derives the answers at its PCs.
func (j *jitRegJob) lowerRoot() {
	rt := j.root
	if rt == nil || rt.lowered {
		return
	}
	rt.lowered = true
	if rt.cached != nil {
		rt.got = *rt.cached
	}
	if !rt.got.done[jitLowerSSA] {
		got := jitRegLowerKinds(rt.rc, j.cl.fn, jitLowerSSA, rt.h)
		for k := range got.done {
			if got.done[k] && !rt.got.done[k] {
				rt.got.done[k], rt.got.progs[k], rt.got.errs[k] = true, got.progs[k], got.errs[k]
			}
		}
	}
	kind := jitLowerSSA
	if err := rt.got.errs[jitLowerSSA]; err != nil && jitcompile.HostOnly(err) {
		switch {
		case rt.callee:
			kind, rt.asCallee = jitLowerInline, true
		case rt.callsNatively:
			kind = jitLowerHot
		default:
			rt.asHostOnly = true
		}
	}
	if !rt.got.done[kind] {
		got := jitRegLowerKinds(rt.rc, j.cl.fn, kind, rt.h)
		rt.got.done[kind], rt.got.progs[kind], rt.got.errs[kind] = true, got.progs[kind], got.errs[kind]
	}
	p, err := rt.got.progs[kind], rt.got.errs[kind]
	if err != nil {
		rt.err = err
		return
	}
	j.p = p
	j.rf.p = p
	j.snap.s.program(p)
}

// jitRegHandOver hands cl's function's compile in the background, its
// register code c, its entry e, to the workers with its program not
// lowered (jitRegRoot), its snapshot gathered with none: true where it
// did, false where the compile cannot be gathered so (jitRegSnapshotOf's
// nil, its snapshot bad), which its caller lowers on the runtime's
// goroutine and compiles as before.
func (r *Runtime) jitRegHandOver(cl *closure, c *regCode, key jitKey, e *jitRegEntry, callee bool, frameAt, osrAt int) bool {
	s := r.jit
	_, h, cached := r.jitRegHintsCopy(cl, c.rc)
	// Whether a call it makes is native: the kind of a program of host
	// operations alone (lowerRoot), asked where the compile on the spot
	// asks it, before its feedback is.
	callsNatively := r.jitRegCallsNatively(cl, c)
	if cached != nil && cached.done[jitLowerSSA] {
		if err := cached.errs[jitLowerSSA]; err != nil && (!jitcompile.HostOnly(err) || !callee && !callsNatively) {
			// Refused, as lowered before -- for host operations alone
			// too, neither compiled for its native callers nor making a
			// native call: refused on the spot, nothing gathered.
			return false
		}
	}
	r.jitRegSeenFeedback(cl, c)
	rf := r.jitRegFeedbackOf(cl)
	if rf == nil {
		return false
	}
	// The links its feedback asks for, which no code holds until it is
	// placed, are let go of by none meanwhile (regUnlink).
	defer s.regBuilding(&rf)()
	snap := jitRegSnapshotOf(rf, nil)
	if snap == nil || snap.s.bad {
		return false
	}
	s.regSerial++
	e.serial = s.regSerial
	if n := r.jitTest.compileNote; n != nil {
		n(r, cl)
	}
	root := &jitRegRoot{rc: c.rc, h: h, cached: cached, callee: callee, callsNatively: callsNatively}
	j := r.jitRegJobOf(cl, c, key, e, rf, snap, callee, frameAt, osrAt)
	j.root = root
	r.jitRegSubmit(j)
	return true
}

// finishSnap finishes j's snapshot (jitSnapshot's finish) where it has a
// program: of a root whose lowering was refused, it has none, and its
// compile is refused as one lowered on the spot is, before any question
// was asked of it, whose answers nothing then derives or writes.
func (j *jitRegJob) finishSnap() {
	if j.root != nil && j.p == nil {
		return
	}
	j.snap.s.finish()
}

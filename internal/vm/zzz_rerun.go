package vm

import (
	"maps"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A program run again in a realm.
//
// Each run of compiled code has that code prepared anew (prepare): new caches,
// and so, as each object literal first runs, a new root and new shapes grown
// from it, of which nothing the run before it learned knows. V8's literals
// reach the same map run after run, along the transitions out of their
// function's initial map. Here a literal's objects start from a root of its
// site's own, which keeps their room and their transitions apart from every
// other site's; that it is the same root from run to run is had by keeping it.
// A realm keeps a root for each object literal of each Program it runs, and
// each run's caches start with them, as a literal's first run would leave
// them (siteRoot): a run after the first makes none of the shapes its literals
// need, keeps the room they learned, and has objects of the layouts what was
// compiled in an earlier run was compiled for.
//
// The roots are made as the program first runs, before anything of it is
// compiled, so that native code never holds one of a run's own, which it would
// go on making its objects of while each run's interpreter made another's. So
// every literal has one, run or not: a program pays a shape for each it never
// runs, and saves the allocation of each it does.
//
// Code compiled for one run -- Eval, a direct eval, new Function -- has nothing
// to keep. Its shapes go with it (chargeChild).
//
// What keeps the roots is the layout of the realm's Object.prototype, which
// keeps the roots of the objects made on it, as any prototype's does
// (protoRoots), and so they go with the realm. Their table's keys are weak, so
// that a program a host has let go goes too. A function of a program is reached
// through its constants, as a run's closures are, so that nothing here holds
// one.

// programShapes is the most shapes a program's literals may have grown for
// their roots to go on being kept. Objects given keys the program makes up
// grow them at each run, and what is grown from a kept root is the tree's for
// as long as the program is, where a run's own went with the run: past this
// the roots are let go, with what they grew, which goes once the code that
// ran with them has, and the program starts again.
const programShapes = maxShapes / 4

// programTable is the Programs a realm runs, and at how many of them there
// are to be before it looks for those collected, each time twice those left.
type programTable struct {
	byCode map[weak.Pointer[bytecode.Function]]*programSites
	at     int
}

// programSites is what a realm keeps of a program it has run: the roots of
// its literals, in one allocation, as a program of many functions of a literal
// or two would have an allocation or three for each.
type programSites struct {
	// at is the literals' caches, each function's after those of the one
	// before it in fns, and roots their roots.
	at    []uint32
	roots []shape
	// fns is the functions with literals, or making functions that do, each
	// before the functions it makes: the program's, then, the first.
	fns []programFn
	// budget is the slot of a budget of its own, which all the roots hold
	// (chargeChild).
	budget *budgetSlot
}

// programFn is a function of a program in its programSites.
type programFn struct {
	// constant is where the function that makes it has it, sites how many
	// literals it has, and inner how many functions of fns it makes.
	constant, sites, inner int32
}

// RunProgram is Run for fn, the script of a Program, which a host may run any
// number of times: its literals start from the roots the current realm keeps
// for it. It is generic, as this file's functions are, so that it is laid out
// after every function and closure of the package, and with what the package
// that instantiates it is, which is the one that exports Program.
func RunProgram[_ any](r *Runtime, fn *bytecode.Function) (Value, error) {
	cl := r.prepare(fn)
	seedLiterals[struct{}](r, cl)
	v, err := r.run(cl, r.globalThis, nil, Undefined, nil)
	r.endTurn()
	return v, err
}

// seedLiterals starts the caches of cl, a program just prepared, with the
// roots the realm keeps for it.
func seedLiterals[_ any](r *Runtime, cl *closure) {
	side := sideOf[struct{}](r.proto.object.ownLayout())
	ps := side.programs
	if ps == nil {
		ps = &programTable{byCode: map[weak.Pointer[bytecode.Function]]*programSites{}, at: 32}
		side.programs = ps
	}
	key := weak.Make(cl.fn)
	p := ps.byCode[key]
	if p == nil && len(ps.byCode) >= ps.at {
		maps.DeleteFunc(ps.byCode, func(k weak.Pointer[bytecode.Function], _ *programSites) bool { return k.Value() == nil })
		ps.at = max(32, 2*len(ps.byCode))
	}
	if p == nil || p.budget.b.n.Load() >= programShapes {
		p = newProgramSites[struct{}](r, cl.fn)
		ps.byCode[key] = p
	}
	if len(p.fns) != 0 {
		var at, fn int
		seedSites[struct{}](p, cl, &at, &fn)
	}
}

// newProgramSites is the roots of fn's literals in the current realm.
func newProgramSites[_ any](r *Runtime, fn *bytecode.Function) *programSites {
	t := r.shapes
	r.proto.object.flags |= objProtoMet
	t.chunk = nil
	p := &programSites{budget: budgetSlotOf[struct{}](t)}
	t.chunk = nil // The chunk is the program's alone.
	listSites[struct{}](p, fn, 0)
	p.roots = make([]shape, len(p.at))
	for i := range p.roots {
		p.roots[i] = shape{tree: t, proto: r.proto.object, budget: p.budget}
	}
	return p
}

// listSites adds fn, the constant of that name of the function that makes it,
// to p, and the functions it makes, reporting whether it did: not for a
// function with no literal in it or in them.
func listSites[_ any](p *programSites, fn *bytecode.Function, constant int) bool {
	at, self := len(p.at), len(p.fns)
	for i := range fn.Code {
		if in := &fn.Code[i]; in.Op == bytecode.OpNewObject {
			p.at = append(p.at, in.B)
		}
	}
	p.fns = append(p.fns, programFn{constant: int32(constant), sites: int32(len(p.at) - at)})
	inner := int32(0)
	for i := range fn.Constants {
		if c := &fn.Constants[i]; c.Kind == bytecode.ConstFunction && listSites[struct{}](p, c.Fn, i) {
			inner++
		}
	}
	if inner == 0 && len(p.at) == at {
		p.fns = p.fns[:self]
		return false
	}
	p.fns[self].inner = inner
	return true
}

// seedSites starts the caches of cl, the closure of the function at *fn in
// p's, whose literals' roots are at *at, and of the functions it makes.
func seedSites[_ any](p *programSites, cl *closure, at, fn *int) {
	f := p.fns[*fn]
	*fn++
	for range f.sites {
		cl.ic[p.at[*at]].shape = &p.roots[*at]
		*at++
	}
	for range f.inner {
		seedSites[struct{}](p, cl.consts[p.fns[*fn].constant].template(), at, fn)
	}
}

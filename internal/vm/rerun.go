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
// function's initial map. Here a literal whose keys the compiler says, and
// {}, starts from the realm's root for them (literalkeys.go), which is the
// same from run to run; any other's objects -- a literal with a spread or a
// computed key -- start from a root of its site's own, which keeps their
// room and their transitions apart from every other site's, and that it is
// the same root from run to run is had by keeping it.
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
// to keep. The shapes of its literals that say no list of keys -- a spread, a
// computed key -- go with it (chargeChild).
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
	// roots is the roots of the realm's literals, by their keys
	// (literalkeys.go), which a realm that runs no Program has too.
	roots *literalRoots
}

// programSites is what a realm keeps of a program it has run: the roots of
// its literals, in one allocation, as a program of many functions of a literal
// or two would have an allocation or three for each.
type programSites struct {
	// at is the literals' caches, each function's after those of the one
	// before it in fns, and roots their roots: the realm's for a literal's
	// keys (literalkeys.go), or one of the program's.
	at    []uint32
	roots []*shape
	// keys is the hash of each literal's keys, 0 where it says none
	// (keysOf).
	keys []uint64
	// rooms is, for each {} (keys noKeysHash), its site's record of the
	// room its objects are made with (literalkeys.go) as the last run left
	// it, which each run's caches start with, so that a run after the
	// first goes on from what the runs before learned rather than learning
	// it anew; nil where the program has no {}.
	rooms []siteRoom
	// from is the realm's roots they are of, nil for none: those the realm
	// has let go, which would otherwise be kept, are not asked for again,
	// nor those grown past literalShapes, which asking for again lets go.
	// The shapes its {} and its literals of keys grow are theirs, not the
	// program's (budget): a program giving its {} objects keys it makes up
	// had kept them for good, its own budget never spent, until the tree
	// was full.
	from *literalRoots
	// fns is the functions with literals, or making functions that do, each
	// before the functions it makes: the program's, then, the first.
	fns []programFn
	// budget is the slot of a budget of its own, which all the roots hold
	// (chargeChild).
	budget *budgetSlot
	// listed says the sites are listed of the code the program's first run
	// compiled (relistSites): a function's register code is compiled at its
	// first call, so a program listed before its first run lists the sites
	// of its own code alone.
	listed bool
}

// siteRoom is a {} site's record, its cache entry's idx and fills
// (literalkeys.go), a run's to the next; zero before a run has started
// it.
type siteRoom struct {
	room, fills uint8
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
	p := seedLiterals[struct{}](r, cl)
	v, err := r.run(cl, r.globalThis, nil, Undefined, nil)
	r.endTurn()
	if !p.listed {
		p = relistSites[struct{}](r, p, cl)
	}
	if p.rooms != nil {
		// What the run's {} sites learned, the turn's end having read
		// their samples, for the next run.
		var at, fn int
		keepRooms[struct{}](p, cl, &at, &fn)
	}
	return v, err
}

// relistSites is p, the sites of cl's program as its first run in the realm
// began, listed again as the run left its code: the functions it called,
// each compiled at its first call, with their sites. A site listed before
// keeps its root; one of a function compiled since, a literal of keys or a
// {}, takes the realm's root for its keys, as its first execution did
// (regLiteralKeyedRoot), and any other is seeded with nothing, its runs'
// objects starting from a root of the run's own, as before.
func relistSites[_ any](r *Runtime, p *programSites, cl *closure) *programSites {
	p.listed = true
	n := &programSites{budget: p.budget, listed: true}
	listSites[struct{}](n, cl.fn, 0)
	if len(n.at) == len(p.at) {
		return p
	}
	type site struct {
		fn *bytecode.Function
		at uint32
	}
	kept := make(map[site]*shape, len(p.at))
	if len(p.fns) != 0 {
		var at, fn int
		eachSite[struct{}](p, cl.fn, &at, &fn, func(f *bytecode.Function, i int) {
			kept[site{f, p.at[i]}] = p.roots[i]
		})
	}
	n.roots = make([]*shape, len(n.at))
	keyed := false
	var at, fn int
	eachSite[struct{}](n, cl.fn, &at, &fn, func(f *bytecode.Function, i int) {
		if root, ok := kept[site{f, n.at[i]}]; ok {
			n.roots[i] = root
		} else if keys := n.keys[i]; keys != 0 {
			n.roots[i] = rootOfKeys[struct{}](r, keys)
		}
		if n.roots[i] != nil && n.keys[i] != 0 {
			keyed = true
		}
		if n.keys[i] == noKeysHash && n.rooms == nil {
			n.rooms = make([]siteRoom, len(n.at))
		}
	})
	if keyed {
		n.from = sideOf[struct{}](r.proto.object.ownLayout()).programs.roots
	}
	ps := sideOf[struct{}](r.proto.object.ownLayout()).programs
	ps.byCode[weak.Make(cl.fn)] = n
	return n
}

// eachSite calls visit with each site of p, by its index, of fn, the
// function at *fn in p's, whose sites are at *at, and of the functions it
// makes, with the function the site is of.
func eachSite[_ any](p *programSites, fn *bytecode.Function, at, idx *int, visit func(*bytecode.Function, int)) {
	f := p.fns[*idx]
	*idx++
	for range f.sites {
		visit(fn, *at)
		*at++
	}
	for range f.inner {
		eachSite[struct{}](p, fn.Constants[p.fns[*idx].constant].Fn, at, idx, visit)
	}
}

// seedLiterals starts the caches of cl, a program just prepared, with the
// roots the realm keeps for it, and is what it keeps.
func seedLiterals[_ any](r *Runtime, cl *closure) *programSites {
	side := sideOf[struct{}](r.proto.object.ownLayout())
	ps := side.programs
	if ps == nil {
		ps = &programTable{at: 32}
		side.programs = ps
	}
	if ps.byCode == nil {
		ps.byCode = map[weak.Pointer[bytecode.Function]]*programSites{}
	}
	key := weak.Make(cl.fn)
	p := ps.byCode[key]
	if p == nil && len(ps.byCode) >= ps.at {
		maps.DeleteFunc(ps.byCode, func(k weak.Pointer[bytecode.Function], _ *programSites) bool { return k.Value() == nil })
		ps.at = max(32, 2*len(ps.byCode))
	}
	if p == nil || p.budget.b.n.Load() >= programShapes || p.from != nil && (p.from != ps.roots || p.from.budget.b.n.Load() >= literalShapes) {
		p = newProgramSites[struct{}](r, cl)
		ps.byCode[key] = p
	}
	if len(p.fns) != 0 {
		var at, fn int
		seedSites[struct{}](p, cl, &at, &fn)
	}
	return p
}

// newProgramSites is the roots of the literals of cl, a program just prepared,
// in the current realm.
func newProgramSites[_ any](r *Runtime, cl *closure) *programSites {
	t := r.shapes
	r.proto.object.flags |= objProtoMet
	t.chunk = nil
	p := &programSites{budget: budgetSlotOf[struct{}](t)}
	t.chunk = nil // The chunk is the program's alone.
	listSites[struct{}](p, cl.fn, 0)
	p.roots = make([]*shape, len(p.at))
	for _, keys := range p.keys {
		if keys == noKeysHash {
			p.rooms = make([]siteRoom, len(p.at))
			break
		}
	}
	var at, fn int
	if len(p.fns) != 0 {
		keyProgramSites[struct{}](r, p, cl, &at, &fn)
	}
	own := 0
	for _, s := range p.roots {
		if s == nil {
			own++
		}
	}
	if own != len(p.roots) {
		p.from = sideOf[struct{}](r.proto.object.ownLayout()).programs.roots
	}
	roots := make([]shape, own)
	for i, s := range p.roots {
		if s == nil {
			own--
			roots[own] = shape{tree: t, proto: r.proto.object, budget: p.budget}
			p.roots[i] = &roots[own]
		}
	}
	return p
}

// keyProgramSites finds the realm's roots for the keys of the literals of cl,
// the closure of the function at *fn in p's, whose literals are at *at, and of
// the functions it makes.
func keyProgramSites[_ any](r *Runtime, p *programSites, cl *closure, at, fn *int) {
	f := p.fns[*fn]
	*fn++
	for range f.sites {
		if keys := p.keys[*at]; keys != 0 {
			p.roots[*at] = rootOfKeys[struct{}](r, keys)
		}
		*at++
	}
	for range f.inner {
		keyProgramSites[struct{}](r, p, cl.consts[p.fns[*fn].constant].template(), at, fn)
	}
}

// listSites adds fn, the constant of that name of the function that makes it,
// to p, and the functions it makes, reporting whether it did: not for a
// function with no literal in it or in them.
func listSites[_ any](p *programSites, fn *bytecode.Function, constant int) bool {
	at, self := len(p.at), len(p.fns)
	if rc := fn.RegCompiled(); rc != nil {
		for pc, w := range rc.Code {
			if op := w.Op(); op == bytecode.RNewObject || op == bytecode.RNewObjectLit {
				p.at = append(p.at, uint32(w.C()))
				p.keys = append(p.keys, keysOf[struct{}](rc, pc))
			}
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
//
// A site of the function's register code is past its own caches where the
// closure, just prepared, has not been given the code's yet (regExtend),
// as in a realm the program has not run in: they are made here, as
// regExtend makes them, which keeps what is in them.
func seedSites[_ any](p *programSites, cl *closure, at, fn *int) {
	f := p.fns[*fn]
	*fn++
	for range f.sites {
		site := int(p.at[*at])
		if site >= len(cl.ic) {
			ic := newPropCaches(site)
			copy(ic, cl.ic)
			cl.ic = ic
		}
		if p.roots[*at] == nil {
			// One of a function compiled since the program was first
			// listed that says no keys (relistSites).
			*at++
			continue
		}
		cl.ic[site].shape = p.roots[*at]
		if p.rooms != nil && p.keys[*at] == noKeysHash {
			if room := p.rooms[*at]; room.room != 0 {
				cl.ic[site].idx, cl.ic[site].fills = int32(room.room), room.fills
			} else {
				emptySiteStart[struct{}](&cl.ic[site])
			}
		}
		*at++
	}
	for range f.inner {
		seedSites[struct{}](p, cl.consts[p.fns[*fn].constant].template(), at, fn)
	}
}

// keepRooms keeps in p the records of the {} sites of cl, the closure of
// the function at *fn in p's, a run's, whose literals are at *at, and of
// the functions it makes: what they learned, for the next run
// (programSites' rooms).
func keepRooms[_ any](p *programSites, cl *closure, at, fn *int) {
	f := p.fns[*fn]
	*fn++
	for range f.sites {
		if site := int(p.at[*at]); p.keys[*at] == noKeysHash && site < len(cl.ic) {
			if c := &cl.ic[site]; c.shape == p.roots[*at] && c.idx != 0 {
				p.rooms[*at] = siteRoom{room: uint8(c.idx), fills: c.fills}
			}
		}
		*at++
	}
	for range f.inner {
		keepRooms[struct{}](p, cl.consts[p.fns[*fn].constant].template(), at, fn)
	}
}

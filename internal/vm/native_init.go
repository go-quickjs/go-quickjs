package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// regInitPlan is rc's Stores, the plan of an initializer's body, as the
// native handlers read it (regCode's initPlan), or nil. The native
// interpreter makes the stores itself where a construction or an
// apply_args calls the body: every store's cache checked against the
// object, as set_prop's handler checks it, before any is made. The Go
// table runs the body.
func regInitPlan(rc *bytecode.RegCode) []uint32 {
	if len(rc.Stores) == 0 {
		return nil
	}
	plan := make([]uint32, len(rc.Stores))
	for i, s := range rc.Stores {
		plan[i] = uint32(s.Site)<<16 | uint32(s.Param)<<1
		if s.Zero {
			plan[i] |= 1
		}
	}
	return plan
}

// initCache is what the native initializer (nativegen's initRun) keeps of
// the last plan of its template's code it validated in full, as V8 keeps
// a constructor's initial map and the map its stores lead to: the
// object's layout before the stores (from) and after (to), each store's
// slot, and the prototype chain as the additions' caches checked it --
// p1 and s1, p2 and s2, or the chain's cell (propCache). The layouts are
// remembered ones, which never change in place, so an object of layout
// from, its chain as it was, takes the same stores to the same slots
// and to: initRun makes them with no check of each, deciding only the
// parameters made 0 where falsy. It is the runtime's, on its template's
// closureCode, and holds what the template's own caches hold; initRun
// fills it, each pointer through the write barrier's routine while the
// collector marks, and stops refilling past maxCacheFills, as a site's
// cache gives up. Go only makes it, empty (regExtend).
type initCache struct {
	from, to *shape
	p1, p2   *Object
	s1, s2   *shape
	cell     *protoCell
	// slots is each store's slot, in the plan's order: as many as a plan
	// has stores, bytecode.MaxStores at most. A slot past 16 bits is not
	// filled: the plan is then validated in full each time.
	slots [bytecode.MaxStores]uint16
	// fills counts the fills, as propCache.fills does.
	fills uint8
}

// The kinds of a forwarder's plan (regCode's fwdKind).
const (
	fwdApply = 1 + iota
	fwdCall
)

// fwdArg is an argument a forwarder's call hands on (regCode's fwdArgs):
// its parameter param, or, where param is -1, the constant k.
type fwdArg struct {
	param int64
	k     Value
}

// regForwardPlan is rc's Forward as the native construction reads it:
// its kind, its sites, and a call's arguments, constants made Values.
func regForwardPlan(fn *bytecode.Function, rc *bytecode.RegCode) (kind, sites uint32, args []fwdArg) {
	f := rc.Forward
	if f == nil {
		return 0, 0, nil
	}
	kind = fwdApply
	if f.Call {
		kind = fwdCall
		args = make([]fwdArg, len(f.Args))
		for i, x := range f.Args {
			args[i].param = int64(x.Param)
			if x.Param < 0 {
				v, ok := regConstOf(regConsts(fn, rc), x.Load)
				if !ok {
					return 0, 0, nil
				}
				args[i].k = v
			}
		}
	}
	return kind, uint32(f.Method) | uint32(f.Fn)<<16, args
}

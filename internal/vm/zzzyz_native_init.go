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

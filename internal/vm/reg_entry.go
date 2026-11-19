package vm

import (
	"fmt"
	"sync/atomic"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The register interpreter's set-up: what runs once a function or a
// closure, out of the hot block (reg_run.go).

// RegsBuilt is how many functions have been given register code, for tests
// that check it ran.
func RegsBuilt() int64 { return regsBuilt.Load() }

// firstReg is fn's register code, built at its first call
// (bytecode.RegLater) and kept in fn.VMCode for every runtime running it.
// The backend compiles every function, so nil -- no register code -- is
// an internal error, which the caller reports (errNoRegCode).
func firstReg(fn *bytecode.Function) *regCode {
	if c := (*regCode)(atomic.LoadPointer(&fn.VMCode)); c != nil {
		return c
	}
	rc := fn.RegCode()
	if rc == nil || !rc.Wide {
		if regTrace {
			println("reg: not", fn.Name, "(no register code)")
		}
		return nil
	}
	c := regBuild(fn, rc)
	if !atomic.CompareAndSwapPointer(&fn.VMCode, nil, unsafe.Pointer(c)) {
		return (*regCode)(atomic.LoadPointer(&fn.VMCode))
	}
	return c
}

// errNoRegCode is the error of a call of a function the register backend
// gave no code: none is meant to have any.
func errNoRegCode(fn *bytecode.Function) error {
	return fmt.Errorf("internal error: %s has no register code", fn.Name)
}

// regExtend adds to cl's tables -- its names, constants and caches, which
// every closure of its template in this runtime shares -- those of c's code
// past its function's own, where the code was compiled at the function's
// first call (RegCode.Names): a runtime makes a function's tables as it
// loads its program, before the code exists, and the code adds nothing to
// the Function, which the runtimes running it share. It runs where a
// closure is first given its register code (cl.reg), which every entry
// into the code reads but those a plan makes (regBindPlan).
func (r *Runtime) regExtend(cl *closure, c *regCode) {
	if c.initPlan != nil && cl.ctorInit == nil {
		// The native initializer's cache, which it fills.
		cl.ctorInit = newInitCache(c.initPlan)
	}
	rc := c.rc
	if rc.Names == nil {
		return
	}
	if n := len(rc.Names); len(cl.names) < n {
		names := make([]Atom, n)
		copy(names, cl.names)
		for i := len(cl.names); i < n; i++ {
			// The compiler's names and strings are slices of the source,
			// which under a limit a value made of them must not hold
			// (ownEvaluator).
			names[i] = r.atoms.intern(r.ownText(rc.Names[i]))
		}
		cl.names = names
	}
	if n := len(rc.Constants); len(cl.consts) < n {
		consts := make([]Value, n)
		copy(consts, cl.consts)
		for i := len(cl.consts); i < n; i++ {
			k := rc.Constants[i]
			k.Str, k.Flags = r.ownText(k.Str), r.ownText(k.Flags)
			consts[i] = r.materialize(k)
		}
		cl.consts = consts
	}
	if len(cl.ic) <= int(rc.PropSites) {
		ic := newPropCaches(int(rc.PropSites))
		copy(ic, cl.ic)
		cl.ic = ic
	}
}

// regBindPlan gives fd's closure its register code where a plan lets a
// call enter it without asking (fd.regCall): a built-in's callback
// (regDirect) reads the code from the plan, not from cl.reg.
func regBindPlan(fd *funcData, c *regCode) {
	if cl := fd.closure; cl.reg == nil {
		cl.realm.agent.regExtend(cl, c)
		cl.reg = c
	}
}

// regConsts is the constants of rc's code, whose function is fn.
func regConsts(fn *bytecode.Function, rc *bytecode.RegCode) []bytecode.Constant {
	if rc.Constants != nil {
		return rc.Constants
	}
	return fn.Constants
}

// regConst is constant k of c's code, whose function is fn.
func regConst(fn *bytecode.Function, c *regCode, k uint32) *bytecode.Constant {
	return &regConsts(fn, c.rc)[k]
}

// regBuild is what runs fn's register code rc, which the register backend
// compiled from its AST.
func regBuild(fn *bytecode.Function, rc *bytecode.RegCode) *regCode {
	regsBuilt.Add(1)
	if regDump {
		println("reg:", fn.Name)
		println(rc.String())
	}
	ncap := 0
	for _, l := range fn.Locals {
		if l.Captured {
			ncap++
		}
	}
	c := &regCode{
		code: unsafe.Pointer(unsafe.SliceData(rc.Code)), siteName: rc.SiteName,
		numRegs: int32(rc.NumRegs), params: int32(rc.Params), inits: int32(rc.Inits - int(rc.SkipInit)),
		coerceThis: fn.CoerceThis, rc: rc, fn: fn, wide: true, ncap: int32(ncap), newTarget: fn.UsesNewTarget, args: rc.UsesArguments || rc.ForwardsArguments || rc.KeepsArgs,
		suspends: fn.Generator || fn.Async,
	}
	if len(rc.Aux) > 0 {
		c.aux = unsafe.Pointer(&rc.Aux[0])
	}
	c.tailTo = fn.Strict && !c.args && !c.suspends && !isClassConstructorKind(fn.Kind)
	c.keep = uint16(rc.Params)
	if !fn.Strict {
		if !fn.HasSimpleParams {
			c.keep = 0
		} else {
			for k := 0; k < rc.Params && k < len(fn.Locals); k++ {
				if fn.Locals[k].Captured {
					c.keep = uint16(k)
					break
				}
			}
		}
	}
	if rc.LocalReg != nil {
		c.reuse = regReuseSites(rc)
	}
	if c.suspends && rc.LocalReg == nil && !regCapsFrame(rc) && !regGenNative(fn, c) {
		c.genFrom = 2
	}
	if fn.Kind == bytecode.KindDerivedConstructor {
		c.cellThis = true
		for _, w := range rc.Code {
			switch w.Op() {
			case bytecode.RClosureW, bytecode.RDirectEval, bytecode.RDebug:
				c.cellThis = false
			}
		}
	}
	switch {
	case c.suspends:
	case rc.Leaf == bytecode.LeafPure && !rc.PureCalls && regLeafRunnable(rc) && regLeafOps(fn, rc) != nil:
		// A short straight body of numbers and upvalues is run as its
		// register code without a frame (regLeafRun).
		c.leaf, c.leafOps = regLeafCode, regLeafOps(fn, rc)
		c.leafNative = regBinaryLeaf(c)
	case rc.Leaf == bytecode.LeafPure && !rc.PureCalls && !pureByTree(rc):
		// A longer body runs faster as code than in the frameless
		// evaluator (pureByTree).
		c.leaf = regLeafPure
	case rc.Leaf != bytecode.LeafNone && rc.Leaf != bytecode.LeafPure:
		c.leaf = regLeafThis
		c.leafNative = regGetterLeaf(rc)
	}
	if !c.suspends && c.leafNative == 0 {
		if d := regTypeofLeaf(fn, rc); d != 0 {
			c.leaf, c.leafNative = regLeafPure, d
		}
	}
	c.initPlan = regInitPlan(rc)
	if c.initPlan != nil && fn.Kind == bytecode.KindConstructor {
		c.initNew = 1
	}
	c.fwdKind, c.fwdSites, c.fwdArgs = regForwardPlan(fn, rc)
	if len(rc.Code) > 0 && !c.suspends {
		// A body that returns a parameter, a constant or nothing first
		// thing -- what follows is unreachable: a call's answer is read
		// from its window or the code, with no frame and no evaluator,
		// whatever its this, which the body never reads.
		switch w := rc.Code[0]; w.Op() {
		case bytecode.RRet:
			if a := int(w.A()); a >= 2 && a < 2+int(rc.Params) {
				c.leaf, c.leafArg = regLeafArg, int32(a-2)
			}
		case bytecode.RRetUndef:
			c.leaf, c.leafVal = regLeafConst, Undefined
		case bytecode.RGetUpval:
			// A getter of a binding it captures, a regLeafCode body
			// regLeafRun runs: the native call handler reads the
			// callee's upvalue (leafUp).
			if len(rc.Code) > 1 && c.leaf == regLeafCode {
				if ret := rc.Code[1]; ret.Op() == bytecode.RRet && ret.A() == w.A() {
					c.leafUp = int32(w.BC()) + 1
				}
			}
		default:
			if v, ok := regConstOf(regConsts(fn, rc), w); ok && len(rc.Code) > 1 {
				if ret := rc.Code[1]; ret.Op() == bytecode.RRet && ret.A() == w.A() {
					c.leaf, c.leafVal = regLeafConst, v
				}
			}
		}
	}
	for _, cases := range rc.Switches {
		c.switches = append(c.switches, newRegSwitchTable(cases))
	}
	return c
}

// regConstOf is the value instruction w loads, where it loads a constant
// that is the same in every realm and runtime: a number, a boolean, null or
// undefined.
func regConstOf(consts []bytecode.Constant, w bytecode.RInstr) (Value, bool) {
	switch w.Op() {
	case bytecode.RLoadInt:
		return Int32(int32(w.BC())), true
	case bytecode.RLoadUndef:
		return Undefined, true
	case bytecode.RLoadNull:
		return Null, true
	case bytecode.RLoadTrue:
		return True, true
	case bytecode.RLoadFalse:
		return False, true
	case bytecode.RLoadConst:
		if k := int(w.BC()); k < len(consts) && consts[k].Kind == bytecode.ConstNumber {
			return Float(consts[k].Num), true
		}
	}
	return Undefined, false
}

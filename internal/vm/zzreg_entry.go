package vm

import (
	"os"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The register interpreter's set-up and its entries from the tree: what
// runs once a function or a frame, out of the hot block (zzreg_run.go).

func init() { regTier.Store(os.Getenv("QJS_REG") != "") }

// SetRegTier turns the register interpreter on or off for functions not yet
// run, for tests that compare it with the other tiers.
func SetRegTier(on bool) { regTier.Store(on) }

// RegsBuilt is how many functions have been given register code, for tests
// that check it ran.
func RegsBuilt() int64 { return regsBuilt.Load() }

// firstForm is what firstTree builds for a function: its register code,
// run by a tree of one node, where the register interpreter is on and the
// form covers it, and its tree otherwise. With the tree tier off there is
// neither: the stack interpreter runs everything, as it does without the
// register interpreter.
func firstForm(fn *bytecode.Function) *tree {
	if regTier.Load() && treeTier.Load() {
		if t := regTreeOf(fn); t != nil {
			return t
		}
	}
	if regTier.Load() {
		regFallback(regFallNoCode, fn)
	}
	return buildTree(fn)
}

// regTreeOf translates fn into register code and returns the tree that runs
// it, or nil. A function's register code is compiled here, at its first
// call (bytecode.RegLater).
func regTreeOf(fn *bytecode.Function) *tree {
	if rc := fn.RegCode(); rc != nil && rc.Wide {
		return regTreeWide(fn, rc)
	}
	if regTrace {
		println("reg: not", fn.Name, "(no register code)")
	}
	return nil
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
	rc := c.rc
	if rc.Names == nil {
		return
	}
	if n := len(rc.Names); len(cl.names) < n {
		names := make([]Atom, n)
		copy(names, cl.names)
		for i := len(cl.names); i < n; i++ {
			names[i] = r.atoms.intern(rc.Names[i])
		}
		cl.names = names
	}
	if n := len(rc.Constants); len(cl.consts) < n {
		consts := make([]Value, n)
		copy(consts, cl.consts)
		for i := len(cl.consts); i < n; i++ {
			consts[i] = r.materialize(rc.Constants[i])
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
// call enter it without asking (fd.treeCall): a built-in's callback
// (regDirect) reads the code from the plan, not from cl.reg.
func regBindPlan(fd *funcData, t *tree) {
	if cl := fd.closure; t.reg != nil && cl.reg == nil {
		cl.realm.agent.regExtend(cl, t.reg)
		cl.reg = t.reg
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

// regTreeWide is the tree that runs fn's register code rc, which the
// register backend compiled from its AST.
func regTreeWide(fn *bytecode.Function, rc *bytecode.RegCode) *tree {
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
		code: unsafe.Pointer(unsafe.SliceData(rc.Code)), src: rc.SrcPC, siteName: rc.SiteName,
		numRegs: int32(rc.NumRegs), params: int32(rc.Params), inits: int32(rc.Inits),
		coerceThis: fn.CoerceThis, rc: rc, fn: fn, wide: true, ncap: int32(ncap), newTarget: fn.UsesNewTarget, args: rc.UsesArguments || rc.ForwardsArguments || rc.KeepsArgs,
		suspends: fn.Generator || fn.Async,
	}
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
		c.reuse = regReuseSites(fn, rc)
	}
	if c.suspends && rc.LocalReg == nil && !regCapsFrame(rc) {
		c.genFrom = 2
	}
	if fn.Kind == bytecode.KindDerivedConstructor {
		c.cellThis = true
		for _, in := range fn.Code {
			switch in.Op {
			case bytecode.OpClosure, bytecode.OpDirectEval, bytecode.OpTailDirectEval, bytecode.OpDebugStmt:
				c.cellThis = false
			}
		}
	}
	switch {
	case c.suspends:
	case fn.Leaf == bytecode.LeafPure && !fn.PureCalls && regLeafRunnable(rc) && regLeafOps(fn, rc) != nil:
		// A short straight body of numbers and upvalues is run as its
		// register code without a frame (regLeafRun).
		c.leaf, c.leafOps = regLeafCode, regLeafOps(fn, rc)
	case fn.Leaf == bytecode.LeafPure && !fn.PureCalls && !pureByTree(fn):
		// A longer body runs faster as code than in the frameless
		// evaluator, as the tree tier finds (pureByTree).
		c.leaf = regLeafPure
	case fn.Leaf != bytecode.LeafNone && fn.Leaf != bytecode.LeafPure:
		c.leaf = regLeafThis
	}
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
	t := &tree{reg: c}
	t.blocks = []tblock{{body: []tstmt{regStmt(c)}, next: regNext}}
	return t
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

// regStmt is the one node of a register function's tree: it runs the frame
// a call through runFD or callTree has made as register code, the with
// objects and the eval's variables runFD gave it among what it keeps. A
// derived constructor's frame, and an arrow's sharing its this, is run
// where its code reads this from the binding (RegCode.ThisRef).
func regStmt(c *regCode) tstmt {
	return func(t *tctx) {
		r, f := t.r, t.f
		regFallback(regFallTree, c.fn)
		if c.wide && (f.thisRef == nil || c.rc.ThisRef) {
			if f.cl.reg == nil {
				r.regExtend(f.cl, c)
				f.cl.reg = c
			}
			if f.callee != nil {
				if fd := f.callee.fn(); fd != nil {
					regPlan(fd)
				}
			}
			v, err := r.regEnterFrame(f, c)
			if err == errTailCall {
				// The frame ended in a tail call: runTree leaves with
				// errTailCall, as after tailCallNode's.
				r.treeTail = true
				return
			}
			if err != nil {
				t.throw(err)
			}
			t.ret = v
			return
		}
		if c.wide || f.thisRef != nil || f.withScopes != nil || f.evalVars != nil {
			regFallback(regFallStack, c.fn)
			v, err := r.execute(f)
			if err != nil {
				t.throw(err)
			}
			t.ret = v
			return
		}
		// The frame was made for the stack form's slots; the register form
		// has a few more of its own.
		base := f.base - int(c.locals)
		if top := base + int(c.numRegs); top > r.stackTop {
			if top > len(r.stack) {
				t.throw(r.throwRangeError("maximum call stack size exceeded"))
			}
			r.stackTop = top
			if top > r.stackHigh {
				r.stackHigh = top
			}
		}
		v, err := r.runReg(f, c, 0)
		if err == errTailCall {
			r.treeTail = true
			return
		}
		if err != nil {
			t.throw(err)
		}
		t.ret = v
	}
}

func regNext(*tctx) int { return -1 }

// regEnterFrame runs the frame f, which runFD or callTree made for the stack
// form, as register code of the form's own layout: its window is made at the
// top of the stack, the function, this and the arguments copied in.
func (r *Runtime) regEnterFrame(f *frame, c *regCode) (Value, error) {
	if regReportOn {
		regNoteCross(regCrossEnter, c.fn, "")
	}
	base := r.stackTop
	top := base + int(c.numRegs)
	if top > len(r.stack) {
		return Undefined, r.throwRangeError("maximum call stack size exceeded")
	}
	r.stackTop = top
	if top > r.stackHigh {
		r.stackHigh = top
	}
	w := r.stack[base:top:top]
	w[0] = Undefined
	w[1] = f.this
	if f.callee != nil {
		w[0] = Obj(f.callee)
		// An arrow's this is the one it captured, whichever way it is
		// called (regPlan).
		if fd := f.callee.fn(); fd != nil && fd.arrow {
			w[1] = fd.extra.lexThis
		}
	}
	p := int(c.params)
	m := min(p, len(f.args))
	for i, a := range f.args[:m] {
		w[2+i] = a
	}
	for i := 2 + m; i < 2+p+int(c.inits); i++ {
		w[i] = Undefined
	}
	saved, savedLocals := f.base, f.locals
	f.base = base
	if c.args {
		f.locals = w[2 : 2+p : 2+p]
	}
	v, err := r.runReg(f, c, 0)
	f.base, f.locals = saved, savedLocals
	return v, err
}

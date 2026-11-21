package regcomp

import (
	"fmt"
	"math"
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler/asmjs"
)

// The typed backend (docs/register-interpreter.md, "The typed table"):
// a function of a validated asm.js module compiled into typed code, each
// register of one kind for the whole function, from the types the
// validator proved (asmjs.Func.Types).
//
// The frame: register 0 the function and 1 its this, the parameters from
// 2 as the call passed them -- Values, which the prologue converts into
// raw locals and nothing writes, so that f.arguments reads them as passed
// -- then a raw register for each of the function's locals,
// then the raw temporaries, each of one kind, and last the window of
// every call, Values: a call's arguments are computed into raw
// temporaries first, so that every call's window is at the same place,
// past everything raw, and nothing raw lives where a callee's frame is.

// LaterTyped is f's typed code as CompileTyped compiles it, compiled when
// first asked for (bytecode.Function.RegTypedCode): f is function fi of
// the validated module m, its literal lit, its stack form fn and its
// identifiers' resolution rec.
func LaterTyped(lit *ast.FuncLit, fn *bytecode.Function, rec *Record, m *asmjs.Module, fi int) *bytecode.RegLater {
	return bytecode.NewRegLater(func() *bytecode.RegCode {
		g := fn.RegCode()
		if g == nil {
			return nil
		}
		rc, err := CompileTyped(lit, fn, rec, m, m.Funcs[fi], g)
		if err != nil {
			if trace || asmTrace {
				println("regcomp: no typed code for", fn.Name+":", err.Error())
			}
			return nil
		}
		return rc
	})
}

// asmTrace prints why a function of a module has no typed code
// (QJS_ASMTRACE).
var asmTrace = false

// SetAsmTrace turns asmTrace on or off.
func SetAsmTrace(on bool) { asmTrace = on }

// CompileTyped compiles f, a function of the validated module m whose
// literal is lit, stack form fn and ordinary register code g, into typed
// code, verified (VerifyKinds), or reports why it cannot.
func CompileTyped(lit *ast.FuncLit, fn *bytecode.Function, rec *Record, m *asmjs.Module, f *asmjs.Func, g *bytecode.RegCode) (rc *bytecode.RegCode, err error) {
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(typedFail)
			if !ok {
				panic(r)
			}
			rc, err = nil, e
		}
	}()
	// Two passes: the first finds how many raw registers the code takes,
	// past which every call's window is.
	t := &tgen{m: m, f: f, fn: fn, rec: rec, lit: lit, sites: g.CallSites, win: 1 << 15}
	t.compile()
	win := t.nregs
	// Then as many as it takes for every branch to reach: one whose
	// offset is past 16 bits is made far in the next (tgen.far), as the
	// ordinary code's jumps are (generate).
	var far []bool
	for {
		t = &tgen{m: m, f: f, fn: fn, rec: rec, lit: lit, sites: g.CallSites, win: win, far: far}
		t.compile()
		if t.win+2+t.maxArgs >= 1<<16 {
			return nil, typedFail("more than 65,536 registers")
		}
		if out := t.unreached(); len(out) > 0 {
			for _, k := range out {
				for len(far) <= k {
					far = append(far, false)
				}
				far[k] = true
			}
			continue
		}
		break
	}
	rc = t.finish(g)
	if err := VerifyKinds(rc, fn); err != nil {
		return nil, err
	}
	return rc, nil
}

type typedFail string

func (e typedFail) Error() string { return string(e) }

func (t *tgen) fail(format string, args ...any) {
	panic(typedFail(fmt.Sprintf(format, args...)))
}

// tgen is the typed backend's state for one function.
type tgen struct {
	m   *asmjs.Module
	f   *asmjs.Func
	fn  *bytecode.Function
	rec *Record
	lit *ast.FuncLit

	code  []bytecode.RInstr
	pos   []int32
	at    int32
	kinds []bytecode.RegKind
	// temp says a register is a temporary, and free is each kind's
	// temporaries free.
	temp  []bool
	free  [3][]int
	nregs int
	// local is each of the function's locals' raw register.
	local []int
	// win is where every call's window begins; maxArgs the most
	// arguments a call passes.
	win, maxArgs int
	// sites is the ordinary code's call sites, past which the typed
	// code's are numbered; calls the typed code's.
	sites, calls int
	floats       []float64
	asm          []bytecode.AsmUp
	// jumps are the branches to patch, labels each label's pc (-1 until
	// bound). far is, by the order of the branches of a 16-bit offset
	// (branches), those made far: a branch over a jmp over a jmp of 32
	// bits, each one that did not reach in a compile before.
	labels   []int
	jumps    []tjump
	far      []bool
	branches int
	// loops are the statements break and continue leave or go on in,
	// innermost last.
	loops []tloop
}

type tjump struct {
	at, label int
	bc        bool // the offset is BC's, else SC's
	ord       int  // a branch's order among those of SC (tgen.far)
}

type tloop struct {
	names     []string
	brk, cont int // labels, cont -1 for a switch or a labelled block
}

// kindOf is the kind of register a value of type ty takes.
func kindOf(ty asmjs.Type) bytecode.RegKind {
	switch ty {
	case asmjs.Double, asmjs.DoubleQ, asmjs.Float, asmjs.FloatQ, asmjs.Floatish:
		return bytecode.RegFloat
	case asmjs.Void:
		return bytecode.RegValue
	}
	return bytecode.RegInt
}

func (t *tgen) typeOf(e ast.Expr) asmjs.Type {
	ty, ok := t.f.Types[e]
	if !ok {
		t.fail("an expression the validator did not type at %d", e.Pos())
	}
	return ty
}

// reg makes register n, of kind k.
func (t *tgen) reg(n int, k bytecode.RegKind) {
	for len(t.kinds) <= n {
		t.kinds = append(t.kinds, bytecode.RegValue)
		t.temp = append(t.temp, false)
	}
	t.kinds[n] = k
	if n+1 > t.nregs {
		t.nregs = n + 1
	}
}

// newTemp is a free temporary of kind k.
func (t *tgen) newTemp(k bytecode.RegKind) int {
	if f := t.free[k]; len(f) > 0 {
		r := f[len(f)-1]
		t.free[k] = f[:len(f)-1]
		return r
	}
	r := t.nregs
	t.reg(r, k)
	t.temp[r] = true
	return r
}

// release frees r where it is a temporary.
func (t *tgen) release(r int) {
	if r >= 0 && r < len(t.temp) && t.temp[r] {
		if slices.Contains(t.free[t.kinds[r]], r) {
			t.fail("a temporary freed twice")
		}
		t.free[t.kinds[r]] = append(t.free[t.kinds[r]], r)
	}
}

func (t *tgen) emit(w bytecode.RInstr) int {
	t.code = append(t.code, w)
	t.pos = append(t.pos, t.at)
	return len(t.code) - 1
}

// abc is an instruction of op with operands A, B and C.
func abc(op bytecode.ROp, a, b, c int) bytecode.RInstr {
	return bytecode.RInstr(uint64(op) | uint64(uint16(a))<<16 | uint64(uint16(b))<<32 | uint64(uint16(c))<<48)
}

// abx is an instruction of op with operand A, B, C and X.
func abcx(op bytecode.ROp, x uint8, a, b, c int) bytecode.RInstr {
	return abc(op, a, b, c) | bytecode.RInstr(uint64(x)<<8)
}

// aBC is an instruction of op with A and the 32-bit BC.
func aBC(op bytecode.ROp, a int, bc uint32) bytecode.RInstr {
	return bytecode.RInstr(uint64(op) | uint64(uint16(a))<<16 | uint64(bc)<<32)
}

func (t *tgen) newLabel() int {
	t.labels = append(t.labels, -1)
	return len(t.labels) - 1
}

func (t *tgen) bind(l int) { t.labels[l] = len(t.code) }

// jmp emits a jump to label l.
func (t *tgen) jmp(l int) {
	at := t.emit(aBC(bytecode.RJmp, 0, 0))
	t.jumps = append(t.jumps, tjump{at: at, label: l, bc: true})
}

// branch emits w, a branch whose SC goes to label l.
func (t *tgen) branchTo(w bytecode.RInstr, l int) {
	ord := t.branches
	t.branches++
	if ord < len(t.far) && t.far[ord] {
		t.emit(w.WithSC(1))
		t.emit(aBC(bytecode.RJmp, 0, 1))
		t.jmp(l)
		return
	}
	at := t.emit(w)
	t.jumps = append(t.jumps, tjump{at: at, label: l, ord: ord})
}

// unreached is the order of each branch of SC whose label is past 16 bits
// from it (tgen.far).
func (t *tgen) unreached() []int {
	var out []int
	for _, j := range t.jumps {
		if to := t.labels[j.label]; !j.bc && to >= 0 {
			if off := to - (j.at + 1); off < math.MinInt16 || off > math.MaxInt16 {
				out = append(out, j.ord)
			}
		}
	}
	return out
}

// float is the index of x in the code's float constants.
func (t *tgen) float(x float64) uint32 {
	for i, f := range t.floats {
		if math.Float64bits(f) == math.Float64bits(x) {
			return uint32(i)
		}
	}
	t.floats = append(t.floats, x)
	return uint32(len(t.floats) - 1)
}

// upval is the upvalue that the module-level name id reads (write: is
// written), noted in the code's table of them as g.
func (t *tgen) upval(id *ast.Ident, g *asmjs.Global, write bool) int {
	var res Res
	var ok bool
	if write {
		res, ok = t.rec.WriteOf(id)
	} else {
		res, ok = t.rec.ReadOf(id)
	}
	if !ok || res.Kind != ResUpval || int(res.Index) >= len(t.fn.Upvalues) {
		t.fail("%s is not one of the function's upvalues", id.Name)
	}
	k := int(res.Index)
	up := asmUpOf(t.m, g)
	for len(t.asm) <= k {
		t.asm = append(t.asm, bytecode.AsmUp{})
	}
	if old := t.asm[k]; old.Kind != bytecode.AsmUpNone && old != up {
		t.fail("upvalue %d is two of the module's bindings", k)
	}
	t.asm[k] = up
	return k
}

// asmUpOf is what the link checks of an upvalue holding module binding g.
func asmUpOf(m *asmjs.Module, g *asmjs.Global) bytecode.AsmUp {
	switch g.Kind {
	case asmjs.GlobalView:
		return bytecode.AsmUp{Kind: bytecode.AsmUpView, View: uint8(g.View)}
	case asmjs.GlobalMath:
		return bytecode.AsmUp{Kind: bytecode.AsmUpMath, Name: g.Math}
	case asmjs.GlobalConst:
		return bytecode.AsmUp{Kind: bytecode.AsmUpConst}
	case asmjs.GlobalVar, asmjs.GlobalImport:
		if g.Type == asmjs.Int {
			return bytecode.AsmUp{Kind: bytecode.AsmUpVarI}
		}
		return bytecode.AsmUp{Kind: bytecode.AsmUpVarF}
	case asmjs.GlobalFFI:
		return bytecode.AsmUp{Kind: bytecode.AsmUpFFI}
	case asmjs.GlobalFunc:
		return bytecode.AsmUp{Kind: bytecode.AsmUpFunc}
	case asmjs.GlobalTable:
		return bytecode.AsmUp{Kind: bytecode.AsmUpTable, Len: len(m.Tables[g.Table].Funcs)}
	}
	return bytecode.AsmUp{}
}

// global is the module-level binding the name id names in f, or nil
// where a local of f's does.
func (t *tgen) global(id *ast.Ident) *asmjs.Global {
	if t.f.Local(id.Name) >= 0 {
		return nil
	}
	g := t.m.Global(id.Name)
	if g == nil {
		t.fail("%s is not declared", id.Name)
	}
	return g
}

func (t *tgen) compile() {
	f := t.f
	nparams := len(f.Sig.Params)
	for i := 0; i < 2+nparams; i++ {
		t.reg(i, bytecode.RegValue)
	}
	t.local = make([]int, len(f.Locals))
	for i, l := range f.Locals {
		r := 2 + nparams + i
		t.local[i] = r
		t.reg(r, kindOf(l.Type))
	}
	t.at = int32(t.lit.Start)
	// The prologue: each parameter's annotation, into its raw local --
	// the parameters stay as the call passed them, where f.arguments reads
	// them -- and the locals' initial values.
	for i, p := range f.Sig.Params {
		r := t.local[i]
		switch p {
		case asmjs.Int:
			t.emit(abc(bytecode.TConvI, r, 2+i, 0))
		default:
			t.emit(abc(bytecode.TConvF, r, 2+i, 0))
			if p == asmjs.Float {
				t.emit(abc(bytecode.TFRound, r, r, 0))
			}
		}
	}
	for i := nparams; i < len(f.Locals); i++ {
		l := f.Locals[i]
		t.constInto(t.local[i], l.Type, l.Init)
	}
	for _, s := range t.lit.Body[f.Header:] {
		t.stmt(s)
	}
	t.emit(aBC(bytecode.RRetUndef, 0, 0))
}

// constInto loads the constant x, of type ty, into r.
func (t *tgen) constInto(r int, ty asmjs.Type, x float64) {
	if kindOf(ty) == bytecode.RegInt {
		t.emit(aBC(bytecode.TIConst, r, uint32(int32(uint32(int64(x))))))
		return
	}
	t.emit(aBC(bytecode.TFConst, r, t.float(x)))
}

func (t *tgen) stmt(s ast.Stmt) {
	t.at = int32(s.Pos())
	switch s := s.(type) {
	case *ast.ExprStmt:
		if c, ok := s.X.(*ast.Call); ok {
			if call := t.f.Calls[c]; call != nil && call.Kind != asmjs.CallMath {
				t.release(t.call(c, call, -1))
				return
			}
		}
		t.release(t.expr(s.X, -1))
	case *ast.BlockStmt:
		for _, b := range s.Body {
			t.stmt(b)
		}
	case *ast.EmptyStmt:
	case *ast.IfStmt:
		els, end := t.newLabel(), t.newLabel()
		t.branch(s.Test, els, false)
		t.stmt(s.Cons)
		if s.Alt != nil {
			t.jmp(end)
			t.bind(els)
			t.stmt(s.Alt)
		} else {
			t.bind(els)
		}
		t.bind(end)
	case *ast.WhileStmt:
		t.loop(nil, nil, s.Test, nil, s.Body, false)
	case *ast.DoWhileStmt:
		t.loop(nil, nil, s.Test, nil, s.Body, true)
	case *ast.ForStmt:
		if s.Init != nil {
			t.stmt(s.Init)
		}
		t.loop(nil, nil, s.Test, s.Update, s.Body, false)
	case *ast.LabeledStmt:
		names := []string{s.Label}
		body := s.Body
		for {
			l, ok := body.(*ast.LabeledStmt)
			if !ok {
				break
			}
			names = append(names, l.Label)
			body = l.Body
		}
		switch b := body.(type) {
		case *ast.WhileStmt:
			t.loop(names, nil, b.Test, nil, b.Body, false)
		case *ast.DoWhileStmt:
			t.loop(names, nil, b.Test, nil, b.Body, true)
		case *ast.ForStmt:
			if b.Init != nil {
				t.stmt(b.Init)
			}
			t.loop(names, nil, b.Test, b.Update, b.Body, false)
		default:
			end := t.newLabel()
			t.loops = append(t.loops, tloop{names: names, brk: end, cont: -1})
			t.stmt(body)
			t.loops = t.loops[:len(t.loops)-1]
			t.bind(end)
		}
	case *ast.BreakStmt:
		t.jmp(t.target(s.Label, false))
	case *ast.ContinueStmt:
		t.jmp(t.target(s.Label, true))
	case *ast.ReturnStmt:
		t.ret(s)
	case *ast.SwitchStmt:
		t.switchStmt(s)
	default:
		t.fail("a statement the typed backend has none of")
	}
}

// target is the label a break (or continue) of label name goes to.
func (t *tgen) target(name string, cont bool) int {
	for i := len(t.loops) - 1; i >= 0; i-- {
		l := t.loops[i]
		if name == "" {
			if cont && l.cont < 0 || !cont && l.names != nil && l.cont < 0 && !l.isSwitch() {
				continue
			}
		} else if !slices.Contains(l.names, name) {
			continue
		}
		if cont {
			if l.cont < 0 {
				t.fail("a continue of what is no loop")
			}
			return l.cont
		}
		return l.brk
	}
	t.fail("a break or continue of no statement")
	return 0
}

// isSwitch reports whether l is a switch's, which an unlabelled break
// leaves.
func (l tloop) isSwitch() bool { return l.cont == -2 }

// loop compiles a loop: its test, which a do-while's body runs before,
// and update, both possibly nil, at its bottom, where its body's turns
// go on by a branch back.
func (t *tgen) loop(names []string, _ ast.Stmt, test, update ast.Expr, body ast.Stmt, do bool) {
	if _, val, ok := t.literal(test); test != nil && ok && val != 0 {
		// while (1): a loop with no test.
		test = nil
	}
	top, cont, end, check := t.newLabel(), t.newLabel(), t.newLabel(), t.newLabel()
	if !do && test != nil {
		t.jmp(check)
	}
	t.bind(top)
	t.loops = append(t.loops, tloop{names: names, brk: end, cont: cont})
	t.stmt(body)
	t.loops = t.loops[:len(t.loops)-1]
	t.bind(cont)
	if update != nil {
		t.at = int32(update.Pos())
		t.release(t.expr(update, -1))
	}
	t.bind(check)
	if test != nil {
		t.at = int32(test.Pos())
		t.branch(test, top, true)
	} else {
		t.jmp(top)
	}
	t.bind(end)
}

func (t *tgen) switchStmt(s *ast.SwitchStmt) {
	d := t.expr(s.Disc, -1)
	if !t.temp[d] {
		// The discriminant read once, as JavaScript reads it.
		c := t.newTemp(bytecode.RegInt)
		t.emit(abc(bytecode.TMov, c, d, 0))
		d = c
	}
	end := t.newLabel()
	bodies := make([]int, len(s.Cases))
	def := end
	for i, c := range s.Cases {
		bodies[i] = t.newLabel()
		if c.Test == nil {
			def = bodies[i]
			continue
		}
		_, val, ok := t.literal(c.Test)
		if !ok {
			t.fail("a case that is not a literal")
		}
		k := int32(int64(val))
		if k >= math.MinInt16 && k <= math.MaxInt16 {
			t.branchTo(abc(bytecode.TJEqI, d, int(uint16(int16(k))), 0), bodies[i])
		} else {
			kr := t.newTemp(bytecode.RegInt)
			t.emit(aBC(bytecode.TIConst, kr, uint32(k)))
			t.branchTo(abc(bytecode.TJEq, d, kr, 0), bodies[i])
			t.release(kr)
		}
	}
	t.jmp(def)
	t.release(d)
	t.loops = append(t.loops, tloop{brk: end, cont: -2})
	for i, c := range s.Cases {
		t.bind(bodies[i])
		for _, b := range c.Body {
			t.stmt(b)
		}
	}
	t.loops = t.loops[:len(t.loops)-1]
	t.bind(end)
}

func (t *tgen) ret(s *ast.ReturnStmt) {
	if s.Arg == nil {
		t.emit(aBC(bytecode.RRetUndef, 0, 0))
		return
	}
	r := t.expr(s.Arg, -1)
	switch t.f.Sig.Ret {
	case asmjs.Signed:
		t.emit(abc(bytecode.TIBox, t.win, r, 0))
	default:
		t.emit(abc(bytecode.TFBox, t.win, r, 0))
	}
	t.release(r)
	t.emit(aBC(bytecode.RRet, t.win, 0))
}

// literal is e's literal type and value, where it is a literal.
func (t *tgen) literal(e ast.Expr) (asmjs.Type, float64, bool) {
	switch x := e.(type) {
	case *ast.NumberLit:
		ty := t.typeOfLit(e)
		return ty, x.Value, ty != asmjs.Void
	case *ast.BoolLit:
		if x.Value {
			return asmjs.Fixnum, 1, true
		}
		return asmjs.Fixnum, 0, true
	case *ast.Unary:
		if n, ok := x.Operand.(*ast.NumberLit); ok && x.Op == "-" {
			ty, ok := t.f.Types[e]
			if ok && (ty == asmjs.Double || ty == asmjs.Signed || ty == asmjs.Fixnum || ty == asmjs.Unsigned) {
				if _, isLit := t.f.Types[x.Operand]; !isLit {
					return ty, -n.Value, true
				}
			}
		}
	}
	return asmjs.Void, 0, false
}

// typeOfLit is a literal's type as the validator gave it, Void where it
// gave it none.
func (t *tgen) typeOfLit(e ast.Expr) asmjs.Type {
	ty, ok := t.f.Types[e]
	if !ok {
		return asmjs.Void
	}
	return ty
}

// hasAssign reports whether e assigns a local of the function.
func (t *tgen) hasAssign(e ast.Expr) bool {
	found := false
	var walk func(x ast.Expr)
	walk = func(x ast.Expr) {
		if found || x == nil {
			return
		}
		switch x := x.(type) {
		case *ast.Assign:
			if id, ok := x.Target.(*ast.Ident); ok && t.f.Local(id.Name) >= 0 {
				found = true
				return
			}
			walk(x.Target)
			walk(x.Value)
		case *ast.Binary:
			walk(x.Left)
			walk(x.Right)
		case *ast.Unary:
			walk(x.Operand)
		case *ast.Conditional:
			walk(x.Test)
			walk(x.Cons)
			walk(x.Alt)
		case *ast.Sequence:
			for _, y := range x.Exprs {
				walk(y)
			}
		case *ast.Call:
			walk(x.Callee)
			for _, y := range x.Args {
				walk(y)
			}
		case *ast.Member:
			walk(x.Object)
			walk(x.Property)
		}
	}
	walk(e)
	return found
}

// operands compiles es in order into registers, each a local's own only
// where no later one may write it.
func (t *tgen) operands(es ...ast.Expr) []int {
	rs := make([]int, len(es))
	for i, e := range es {
		rs[i] = t.expr(e, -1)
		if !t.temp[rs[i]] && rs[i] >= 2 {
			for _, later := range es[i+1:] {
				if t.hasAssign(later) {
					c := t.newTemp(t.kinds[rs[i]])
					t.emit(abc(bytecode.TMov, c, rs[i], 0))
					rs[i] = c
					break
				}
			}
		}
	}
	return rs
}

// dest is the register a result of kind k goes to: dst, or a temporary.
func (t *tgen) dest(dst int, k bytecode.RegKind) int {
	if dst >= 0 {
		if t.kinds[dst] != k {
			t.fail("a result of kind %d into a register of kind %d", k, t.kinds[dst])
		}
		return dst
	}
	return t.newTemp(k)
}

// into compiles e into dst.
func (t *tgen) into(e ast.Expr, dst int) {
	r := t.expr(e, dst)
	if r != dst {
		t.emit(abc(bytecode.TMov, dst, r, 0))
		t.release(r)
	}
}

// expr compiles e and gives the register its value is in: dst where it is
// not negative, a local's own or a temporary otherwise.
func (t *tgen) expr(e ast.Expr, dst int) int {
	ty := t.typeOf(e)
	if lt, val, ok := t.literal(e); ok {
		r := t.dest(dst, kindOf(lt))
		t.constInto(r, lt, val)
		return r
	}
	switch e := e.(type) {
	case *ast.Ident:
		if i := t.f.Local(e.Name); i >= 0 {
			r := t.local[i]
			if dst >= 0 && dst != r {
				t.emit(abc(bytecode.TMov, dst, r, 0))
				return dst
			}
			return r
		}
		g := t.global(e)
		k := t.upval(e, g, false)
		if g.Type == asmjs.Int {
			r := t.dest(dst, bytecode.RegInt)
			t.emit(aBC(bytecode.TGGetI, r, uint32(k)))
			return r
		}
		r := t.dest(dst, bytecode.RegFloat)
		t.emit(aBC(bytecode.TGGetF, r, uint32(k)))
		return r
	case *ast.Member:
		return t.load(e, dst)
	case *ast.Assign:
		return t.assign(e, dst)
	case *ast.Unary:
		return t.unary(e, ty, dst)
	case *ast.Binary:
		return t.binary(e, ty, dst)
	case *ast.Conditional:
		k := kindOf(ty)
		r := t.dest(dst, k)
		els, end := t.newLabel(), t.newLabel()
		t.branch(e.Test, els, false)
		t.into(e.Cons, r)
		t.jmp(end)
		t.bind(els)
		t.into(e.Alt, r)
		t.bind(end)
		return r
	case *ast.Sequence:
		for _, x := range e.Exprs[:len(e.Exprs)-1] {
			if c, ok := x.(*ast.Call); ok {
				if call := t.f.Calls[c]; call != nil && call.Kind != asmjs.CallMath {
					t.release(t.call(c, call, -1))
					continue
				}
			}
			t.release(t.expr(x, -1))
		}
		return t.expr(e.Exprs[len(e.Exprs)-1], dst)
	case *ast.Call:
		call := t.f.Calls[e]
		if call == nil {
			t.fail("a call the validator did not see")
		}
		if call.Kind == asmjs.CallMath {
			return t.mathCall(e, call, ty, dst)
		}
		return t.call(e, call, dst)
	}
	t.fail("an expression the typed backend has none of")
	return 0
}

// heapAddr is a view's element's address: the register of its byte
// address and the offset added to it, and its view's shift.
func (t *tgen) heapAddr(e *ast.Member) (base, off int, op asmjs.View) {
	id := e.Object.(*ast.Ident)
	g := t.global(id)
	if g == nil || g.Kind != asmjs.GlobalView {
		t.fail("%s is not a view", id.Name)
	}
	view := g.View
	shift := view.Shift()
	// The view's upvalue, which the link checks.
	t.upval(id, g, false)
	idx := e.Property
	if lt, val, ok := t.literal(idx); ok && lt.IsInt() {
		r := t.newTemp(bytecode.RegInt)
		t.emit(aBC(bytecode.TIConst, r, uint32(int32(int64(val)<<shift))))
		return r, 0, view
	}
	if b, ok := idx.(*ast.Binary); ok && b.Op == ">>" {
		if _, val, ok := t.literal(b.Right); ok && uint8(val) == shift {
			idx = b.Left
		}
	} else if shift != 0 {
		t.fail("an index not shifted by its view's")
	}
	// x + k, x - k and (x + k) | 0: the offset k added in the access.
	if b, ok := idx.(*ast.Binary); ok && b.Op == "|" {
		if _, val, ok := t.literal(b.Right); ok && val == 0 {
			if inner, ok := b.Left.(*ast.Binary); ok && (inner.Op == "+" || inner.Op == "-") {
				idx = inner
			}
		}
	}
	if b, ok := idx.(*ast.Binary); ok && (b.Op == "+" || b.Op == "-") {
		if lt, val, ok := t.literal(b.Right); ok && lt.IsInt() {
			k := int64(val)
			if b.Op == "-" {
				k = -k
			}
			if k >= math.MinInt16 && k <= math.MaxInt16 {
				return t.expr(b.Left, -1), int(k), view
			}
		}
	}
	return t.expr(idx, -1), 0, view
}

var loadOps = [...]bytecode.ROp{asmjs.Int8: bytecode.TLd8S, asmjs.Uint8: bytecode.TLd8U, asmjs.Int16: bytecode.TLd16S,
	asmjs.Uint16: bytecode.TLd16U, asmjs.Int32: bytecode.TLd32, asmjs.Uint32: bytecode.TLd32,
	asmjs.Float32: bytecode.TLdF32, asmjs.Float64: bytecode.TLdF64}

var storeOps = [...]bytecode.ROp{asmjs.Int8: bytecode.TSt8, asmjs.Uint8: bytecode.TSt8, asmjs.Int16: bytecode.TSt16,
	asmjs.Uint16: bytecode.TSt16, asmjs.Int32: bytecode.TSt32, asmjs.Uint32: bytecode.TSt32,
	asmjs.Float32: bytecode.TStF32, asmjs.Float64: bytecode.TStF64}

func (t *tgen) load(e *ast.Member, dst int) int {
	base, off, view := t.heapAddr(e)
	k := bytecode.RegInt
	if view == asmjs.Float32 || view == asmjs.Float64 {
		k = bytecode.RegFloat
	}
	t.release(base)
	r := t.dest(dst, k)
	t.emit(abc(loadOps[view], r, base, int(uint16(int16(off)))))
	return r
}

func (t *tgen) assign(e *ast.Assign, dst int) int {
	switch target := e.Target.(type) {
	case *ast.Ident:
		if i := t.f.Local(target.Name); i >= 0 {
			r := t.local[i]
			t.into(e.Value, r)
			if dst >= 0 && dst != r {
				t.emit(abc(bytecode.TMov, dst, r, 0))
				return dst
			}
			return r
		}
		g := t.global(target)
		k := t.upval(target, g, true)
		v := t.expr(e.Value, dst)
		if g.Type == asmjs.Int {
			t.emit(aBC(bytecode.TGSetI, v, uint32(k)))
		} else {
			if g.Type == asmjs.Float {
				// A float global holds a float32's value.
				t.emit(abc(bytecode.TFRound, v, v, 0))
			}
			t.emit(aBC(bytecode.TGSetF, v, uint32(k)))
		}
		return v
	case *ast.Member:
		base, off, view := t.heapAddr(target)
		if !t.temp[base] && t.hasAssign(e.Value) {
			c := t.newTemp(bytecode.RegInt)
			t.emit(abc(bytecode.TMov, c, base, 0))
			base = c
		}
		v := t.expr(e.Value, dst)
		vk := t.kinds[v]
		wantK := bytecode.RegInt
		if view == asmjs.Float32 || view == asmjs.Float64 {
			wantK = bytecode.RegFloat
		}
		if vk != wantK {
			t.fail("a store of a value of another kind")
		}
		t.emit(abc(storeOps[view], v, base, int(uint16(int16(off)))))
		t.release(base)
		return v
	}
	t.fail("an assignment to what is no variable or element")
	return 0
}

func (t *tgen) unary(e *ast.Unary, ty asmjs.Type, dst int) int {
	switch e.Op {
	case "+":
		if c, ok := e.Operand.(*ast.Call); ok {
			if call := t.f.Calls[c]; call != nil && call.Kind != asmjs.CallMath {
				return t.call(c, call, dst)
			}
		}
		ot := t.typeOf(e.Operand)
		if kindOf(ot) == bytecode.RegFloat {
			return t.expr(e.Operand, dst)
		}
		r := t.expr(e.Operand, -1)
		if ot.Sub(asmjs.Signed) {
			return t.op1(bytecode.TI2FS, r, dst, bytecode.RegFloat)
		}
		return t.op1(bytecode.TI2FU, r, dst, bytecode.RegFloat)
	case "-":
		ot := t.typeOf(e.Operand)
		r := t.expr(e.Operand, -1)
		if kindOf(ot) == bytecode.RegInt {
			return t.op1(bytecode.TNeg, r, dst, bytecode.RegInt)
		}
		return t.op1(bytecode.TFNeg, r, dst, bytecode.RegFloat)
	case "~":
		if in, ok := e.Operand.(*ast.Unary); ok && in.Op == "~" {
			it := t.typeOf(in)
			r := t.expr(in.Operand, -1)
			if kindOf(it) == bytecode.RegFloat || kindOf(t.typeOf(in.Operand)) == bytecode.RegFloat {
				return t.op1(bytecode.TF2I, r, dst, bytecode.RegInt)
			}
			// ~~x of an intish is its bits.
			return t.move(r, dst)
		}
		return t.op1(bytecode.TNot, t.expr(e.Operand, -1), dst, bytecode.RegInt)
	case "!":
		return t.op1(bytecode.TLNot, t.expr(e.Operand, -1), dst, bytecode.RegInt)
	}
	t.fail("the operator %s", e.Op)
	return 0
}

// move gives r as the result, moved to dst where dst is not negative.
func (t *tgen) move(r, dst int) int {
	if dst < 0 || dst == r {
		return r
	}
	t.emit(abc(bytecode.TMov, dst, r, 0))
	t.release(r)
	return dst
}

// op1 emits op of r into dst, a register of kind k.
func (t *tgen) op1(op bytecode.ROp, r, dst int, k bytecode.RegKind) int {
	t.release(r)
	d := t.dest(dst, k)
	t.emit(abc(op, d, r, 0))
	return d
}

// op2 emits op of a and b into dst, a register of kind k.
func (t *tgen) op2(op bytecode.ROp, a, b, dst int, k bytecode.RegKind) int {
	t.release(a)
	t.release(b)
	d := t.dest(dst, k)
	t.emit(abc(op, d, a, b))
	return d
}

// imm16 is e's value where it is an int literal that fits 16 bits.
func (t *tgen) imm16(e ast.Expr) (int32, bool) {
	lt, val, ok := t.literal(e)
	if !ok || !lt.IsInt() {
		return 0, false
	}
	k := int32(int64(val))
	if int64(k) != int64(val) || k < math.MinInt16 || k > math.MaxInt16 {
		return 0, false
	}
	return k, true
}

func (t *tgen) binary(e *ast.Binary, ty asmjs.Type, dst int) int {
	lt := t.typeOf(e.Left)
	switch e.Op {
	case "|":
		if c, ok := e.Left.(*ast.Call); ok {
			if call := t.f.Calls[c]; call != nil && call.Kind != asmjs.CallMath {
				return t.call(c, call, dst)
			}
		}
		if k, ok := t.imm16(e.Right); ok && k == 0 {
			// x | 0 is x's bits.
			return t.expr(e.Left, dst)
		}
		return t.intOp(e, bytecode.TOr, bytecode.TOrI, true, dst)
	case "&":
		return t.intOp(e, bytecode.TAnd, bytecode.TAndI, true, dst)
	case "^":
		return t.intOp(e, bytecode.TXor, bytecode.TXorI, true, dst)
	case "<<":
		return t.intOp(e, bytecode.TShl, bytecode.TShlI, false, dst)
	case ">>":
		return t.intOp(e, bytecode.TShr, bytecode.TShrI, false, dst)
	case ">>>":
		return t.intOp(e, bytecode.TUShr, bytecode.TUShrI, false, dst)
	case "+":
		if kindOf(ty) == bytecode.RegInt {
			return t.intOp(e, bytecode.TAdd, bytecode.TAddI, true, dst)
		}
		return t.floatOp(e, bytecode.TFAdd, dst)
	case "-":
		if kindOf(ty) == bytecode.RegInt {
			if k, ok := t.imm16(e.Right); ok && k != math.MinInt16 {
				a := t.expr(e.Left, -1)
				t.release(a)
				d := t.dest(dst, bytecode.RegInt)
				t.emit(abc(bytecode.TAddI, d, a, int(uint16(int16(-k)))))
				return d
			}
			rs := t.operands(e.Left, e.Right)
			return t.op2(bytecode.TSub, rs[0], rs[1], dst, bytecode.RegInt)
		}
		return t.floatOp(e, bytecode.TFSub, dst)
	case "*":
		if kindOf(ty) == bytecode.RegInt {
			rs := t.operands(e.Left, e.Right)
			return t.op2(bytecode.TMul, rs[0], rs[1], dst, bytecode.RegInt)
		}
		return t.floatOp(e, bytecode.TFMul, dst)
	case "/", "%":
		if kindOf(ty) == bytecode.RegInt {
			op := map[string]bytecode.ROp{"/": bytecode.TDivS, "%": bytecode.TRemS}[e.Op]
			if !lt.Sub(asmjs.Signed) || !t.typeOf(e.Right).Sub(asmjs.Signed) {
				op = map[string]bytecode.ROp{"/": bytecode.TDivU, "%": bytecode.TRemU}[e.Op]
			}
			rs := t.operands(e.Left, e.Right)
			return t.op2(op, rs[0], rs[1], dst, bytecode.RegInt)
		}
		if e.Op == "%" {
			return t.floatOp(e, bytecode.TFMod, dst)
		}
		return t.floatOp(e, bytecode.TFDiv, dst)
	case "<", "<=", ">", ">=", "==", "!=":
		return t.compareValue(e, dst)
	}
	t.fail("the operator %s", e.Op)
	return 0
}

// intOp compiles an int operator of two operands, by its immediate form
// where the right operand is a literal of 16 bits, or the left one is and
// the operator commutes.
func (t *tgen) intOp(e *ast.Binary, op, imm bytecode.ROp, commutes bool, dst int) int {
	left, right := e.Left, e.Right
	k, ok := t.imm16(right)
	if !ok && commutes {
		if k, ok = t.imm16(left); ok {
			left, right = right, left
		}
	}
	if ok {
		if imm == bytecode.TShlI || imm == bytecode.TShrI || imm == bytecode.TUShrI {
			k &= 31
		}
		if k == 0 && imm != bytecode.TAndI {
			// x >>> 0, x >> 0, x << 0, x ^ 0, x + 0: x's bits.
			return t.expr(left, dst)
		}
		a := t.expr(left, -1)
		t.release(a)
		d := t.dest(dst, bytecode.RegInt)
		t.emit(abc(imm, d, a, int(uint16(int16(k)))))
		return d
	}
	rs := t.operands(e.Left, e.Right)
	return t.op2(op, rs[0], rs[1], dst, bytecode.RegInt)
}

func (t *tgen) floatOp(e *ast.Binary, op bytecode.ROp, dst int) int {
	rs := t.operands(e.Left, e.Right)
	if t.kinds[rs[0]] != bytecode.RegFloat || t.kinds[rs[1]] != bytecode.RegFloat {
		t.fail("a float operator of an int")
	}
	return t.op2(op, rs[0], rs[1], dst, bytecode.RegFloat)
}

// cmpKind is how a compare of e's operands compares: signed, unsigned or
// float.
func (t *tgen) cmpKind(e *ast.Binary) int {
	a, b := t.typeOf(e.Left), t.typeOf(e.Right)
	switch {
	case kindOf(a) == bytecode.RegFloat:
		return 2
	case a.Sub(asmjs.Signed) && b.Sub(asmjs.Signed):
		return 0
	}
	return 1
}

// compareValue compiles a compare as a value, 0 or 1.
func (t *tgen) compareValue(e *ast.Binary, dst int) int {
	kind := t.cmpKind(e)
	rs := t.operands(e.Left, e.Right)
	a, b := rs[0], rs[1]
	var op bytecode.ROp
	swap := false
	switch e.Op {
	case "<":
		op = [3]bytecode.ROp{bytecode.TLtS, bytecode.TLtU, bytecode.TFLt}[kind]
	case "<=":
		op = [3]bytecode.ROp{bytecode.TLeS, bytecode.TLeU, bytecode.TFLe}[kind]
	case ">":
		op, swap = [3]bytecode.ROp{bytecode.TLtS, bytecode.TLtU, bytecode.TFLt}[kind], true
	case ">=":
		op, swap = [3]bytecode.ROp{bytecode.TLeS, bytecode.TLeU, bytecode.TFLe}[kind], true
	case "==":
		op = [3]bytecode.ROp{bytecode.TEq, bytecode.TEq, bytecode.TFEq}[kind]
	case "!=":
		op = [3]bytecode.ROp{bytecode.TNe, bytecode.TNe, bytecode.TFNe}[kind]
	}
	if swap {
		a, b = b, a
	}
	return t.op2(op, a, b, dst, bytecode.RegInt)
}

// branch goes to label l where e, an int condition, is true (onTrue) or
// false.
func (t *tgen) branch(e ast.Expr, l int, onTrue bool) {
	switch x := e.(type) {
	case *ast.Unary:
		if x.Op == "!" {
			t.branch(x.Operand, l, !onTrue)
			return
		}
	case *ast.Binary:
		switch x.Op {
		case "<", "<=", ">", ">=", "==", "!=":
			t.compareBranch(x, l, onTrue)
			return
		}
	}
	if _, val, ok := t.literal(e); ok {
		if (val != 0) == onTrue {
			t.jmp(l)
		}
		return
	}
	r := t.expr(e, -1)
	t.release(r)
	op := bytecode.TJZ
	if onTrue {
		op = bytecode.TJNZ
	}
	t.branchTo(abc(op, r, 0, 0), l)
}

// The compares' negations and mirrors, for a branch where one is false or
// whose operands are swapped.
var (
	cmpNot    = map[string]string{"<": ">=", "<=": ">", ">": "<=", ">=": "<", "==": "!=", "!=": "=="}
	cmpMirror = map[string]string{"<": ">", "<=": ">=", ">": "<", ">=": "<=", "==": "==", "!=": "!="}
)

func (t *tgen) compareBranch(e *ast.Binary, l int, onTrue bool) {
	kind := t.cmpKind(e)
	op := e.Op
	if kind == 2 {
		// A float compare's negation is not its opposite's: a NaN makes
		// both false.
		rs := t.operands(e.Left, e.Right)
		a, b := rs[0], rs[1]
		t.release(a)
		t.release(b)
		if op == ">" || op == ">=" {
			a, b, op = b, a, cmpMirror[op]
		}
		var w bytecode.ROp
		switch {
		case op == "<" && onTrue:
			w = bytecode.TJFLt
		case op == "<":
			w = bytecode.TJFNLt
		case op == "<=" && onTrue:
			w = bytecode.TJFLe
		case op == "<=":
			w = bytecode.TJFNLe
		case op == "==" && onTrue, op == "!=" && !onTrue:
			w = bytecode.TJFEq
		default:
			w = bytecode.TJFNe
		}
		t.branchTo(abc(w, a, b, 0), l)
		return
	}
	if !onTrue {
		op = cmpNot[op]
	}
	if kind == 0 {
		// A signed compare with a literal of 16 bits.
		left, right := e.Left, e.Right
		k, ok := t.imm16(right)
		if !ok {
			if k, ok = t.imm16(left); ok {
				left, right, op = right, left, cmpMirror[op]
			}
		}
		if ok {
			a := t.expr(left, -1)
			t.release(a)
			w := map[string]bytecode.ROp{"<": bytecode.TJLtSI, "<=": bytecode.TJLeSI, ">": bytecode.TJGtSI,
				">=": bytecode.TJGeSI, "==": bytecode.TJEqI, "!=": bytecode.TJNeI}[op]
			if k == 0 && (op == "==" || op == "!=") {
				w = map[string]bytecode.ROp{"==": bytecode.TJZ, "!=": bytecode.TJNZ}[op]
			}
			t.branchTo(abc(w, a, int(uint16(int16(k))), 0), l)
			return
		}
	}
	rs := t.operands(e.Left, e.Right)
	a, b := rs[0], rs[1]
	t.release(a)
	t.release(b)
	if op == ">" || op == ">=" {
		a, b, op = b, a, cmpMirror[op]
	}
	var w bytecode.ROp
	switch op {
	case "<":
		w = [2]bytecode.ROp{bytecode.TJLtS, bytecode.TJLtU}[kind]
	case "<=":
		w = [2]bytecode.ROp{bytecode.TJLeS, bytecode.TJLeU}[kind]
	case "==":
		w = bytecode.TJEq
	case "!=":
		w = bytecode.TJNe
	}
	t.branchTo(abc(w, a, b, 0), l)
}

// mathCall compiles a call of a stdlib Math function, inline or by the
// engine's own.
func (t *tgen) mathCall(e *ast.Call, call *asmjs.Call, ty asmjs.Type, dst int) int {
	g := call.Global
	id := e.Callee.(*ast.Ident)
	// The binding the link checks holds the realm's own function.
	t.upval(id, g, false)
	argF := func(i int) int {
		r := t.expr(e.Args[i], -1)
		at := t.typeOf(e.Args[i])
		if kindOf(at) == bytecode.RegInt {
			op := bytecode.TI2FS
			if at.Sub(asmjs.Unsigned) && !at.Sub(asmjs.Signed) {
				op = bytecode.TI2FU
			}
			return t.op1(op, r, -1, bytecode.RegFloat)
		}
		return r
	}
	switch g.Math {
	case "imul":
		rs := t.operands(e.Args[0], e.Args[1])
		return t.op2(bytecode.TMul, rs[0], rs[1], dst, bytecode.RegInt)
	case "clz32":
		return t.op1(bytecode.TClz, t.expr(e.Args[0], -1), dst, bytecode.RegInt)
	case "abs":
		if kindOf(ty) == bytecode.RegInt {
			return t.op1(bytecode.TAbs, t.expr(e.Args[0], -1), dst, bytecode.RegInt)
		}
		return t.op1(bytecode.TFAbs, t.expr(e.Args[0], -1), dst, bytecode.RegFloat)
	case "sqrt":
		return t.op1(bytecode.TFSqrt, t.expr(e.Args[0], -1), dst, bytecode.RegFloat)
	case "fround":
		if c, ok := e.Args[0].(*ast.Call); ok {
			if in := t.f.Calls[c]; in != nil && in.Kind != asmjs.CallMath {
				r := t.call(c, in, -1)
				return t.op1(bytecode.TFRound, r, dst, bytecode.RegFloat)
			}
		}
		return t.op1(bytecode.TFRound, argF(0), dst, bytecode.RegFloat)
	case "min", "max":
		var op bytecode.ROp
		k := kindOf(ty)
		switch {
		case k == bytecode.RegInt && g.Math == "min":
			op = bytecode.TMinS
		case k == bytecode.RegInt:
			op = bytecode.TMaxS
		case g.Math == "min":
			op = bytecode.TFMin
		default:
			op = bytecode.TFMax
		}
		rs := t.operands(e.Args...)
		acc := rs[0]
		for i, r := range rs[1:] {
			d := -1
			if i == len(rs)-2 {
				d = dst
			}
			acc = t.op2(op, acc, r, d, k)
		}
		return acc
	case "pow", "atan2":
		x := map[string]uint8{"pow": bytecode.TMathPow, "atan2": bytecode.TMathAtan2}[g.Math]
		a := argF(0)
		if !t.temp[a] && t.hasAssign(e.Args[1]) {
			c := t.newTemp(bytecode.RegFloat)
			t.emit(abc(bytecode.TMov, c, a, 0))
			a = c
		}
		b := argF(1)
		t.release(a)
		t.release(b)
		d := t.dest(dst, bytecode.RegFloat)
		t.emit(abcx(bytecode.TFMath2, x, d, a, b))
		return d
	}
	x, ok := map[string]uint8{"floor": bytecode.TMathFloor, "ceil": bytecode.TMathCeil, "sin": bytecode.TMathSin,
		"cos": bytecode.TMathCos, "tan": bytecode.TMathTan, "asin": bytecode.TMathAsin, "acos": bytecode.TMathAcos,
		"atan": bytecode.TMathAtan, "exp": bytecode.TMathExp, "log": bytecode.TMathLog}[g.Math]
	if !ok {
		t.fail("Math.%s", g.Math)
	}
	a := argF(0)
	t.release(a)
	d := t.dest(dst, bytecode.RegFloat)
	t.emit(abcx(bytecode.TFMath1, x, d, a, 0))
	return d
}

// call compiles a call of a function of the module, of a table's or of an
// import: its index, then its arguments, into raw registers, then its
// window -- the callee, an undefined this, the arguments as Values --
// past everything raw, then the call, its result coerced as call.Ret
// says into dst.
func (t *tgen) call(e *ast.Call, call *asmjs.Call, dst int) int {
	t.at = int32(e.Start)
	idx := -1
	if call.Kind == asmjs.CallTable {
		idx = t.expr(call.Index, -1)
		if !t.temp[idx] && slices.ContainsFunc(e.Args, t.hasAssign) {
			c := t.newTemp(bytecode.RegInt)
			t.emit(abc(bytecode.TMov, c, idx, 0))
			idx = c
		}
	}
	args := t.operands(e.Args...)
	w := t.win
	if len(args) > t.maxArgs {
		t.maxArgs = len(args)
	}
	switch call.Kind {
	case asmjs.CallTable:
		mb := e.Callee.(*ast.Member)
		k := t.upval(mb.Object.(*ast.Ident), call.Global, false)
		t.emit(abc(bytecode.TTabGet, w, idx, k))
		t.release(idx)
	default:
		k := t.upval(e.Callee.(*ast.Ident), call.Global, false)
		t.emit(aBC(bytecode.RGetUpval, w, uint32(k)))
	}
	t.emit(aBC(bytecode.RLoadUndef, w+1, 0))
	for i, r := range args {
		at := t.typeOf(e.Args[i])
		op := bytecode.TFBox
		if t.kinds[r] == bytecode.RegInt {
			op = bytecode.TIBox
			if at.Sub(asmjs.Unsigned) && !at.Sub(asmjs.Signed) {
				op = bytecode.TUBox
			}
		}
		t.emit(abc(op, w+2+i, r, 0))
		t.release(r)
	}
	t.at = int32(e.Start)
	site := t.sites + t.calls
	t.calls++
	if site >= 1<<16 {
		t.fail("more call sites than C numbers")
	}
	t.emit(abcx(bytecode.RCallW, uint8(len(args)), w, w, site))
	switch call.Ret {
	case asmjs.Signed:
		d := t.dest(dst, bytecode.RegInt)
		t.emit(abc(bytecode.TConvI, d, w, 0))
		return d
	case asmjs.Double, asmjs.Float:
		d := t.dest(dst, bytecode.RegFloat)
		t.emit(abc(bytecode.TConvF, d, w, 0))
		if call.Ret == asmjs.Float {
			t.emit(abc(bytecode.TFRound, d, d, 0))
		}
		return d
	}
	if dst >= 0 {
		t.fail("a void call's value")
	}
	return -1
}

// finish makes the code: its jumps patched, its line table, its tables.
func (t *tgen) finish(g *bytecode.RegCode) *bytecode.RegCode {
	for _, j := range t.jumps {
		to := t.labels[j.label]
		if to < 0 {
			t.fail("a label not bound")
		}
		off := to - (j.at + 1)
		if j.bc {
			t.code[j.at] = t.code[j.at].WithBC(uint32(int32(off)))
			continue
		}
		if off < math.MinInt16 || off > math.MaxInt16 {
			t.fail("a branch past 16 bits")
		}
		t.code[j.at] = t.code[j.at].WithSC(int32(off))
	}
	// Each instruction's position, an entry where it changes, as the
	// ordinary code's line table is made (bytecode.RegCode.Lines).
	var lines []bytecode.SourceLoc
	for i, p := range t.pos {
		if i == 0 || p != lines[len(lines)-1].Pos {
			lines = append(lines, bytecode.SourceLoc{PC: uint32(i), Pos: p})
		}
	}
	nregs := t.win + 2 + t.maxArgs
	for i := 0; i < 2+t.maxArgs; i++ {
		t.reg(t.win+i, bytecode.RegValue)
	}
	rc := &bytecode.RegCode{
		Code: t.code, NumRegs: nregs, Lines: lines, SiteName: g.SiteName, This: -1,
		Wide: true, Params: len(t.f.Sig.Params), CallSites: t.sites + t.calls,
		Names: g.Names, Constants: g.Constants, PropSites: g.PropSites,
		Typed: true, Kinds: t.kinds[:nregs], Floats: t.floats, Asm: t.asm,
	}
	return rc
}

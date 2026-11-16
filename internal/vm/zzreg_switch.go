package vm

import (
	"math"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// regTypeOf is the index in typeofNames of typeof a value of kind k but
// an object, whose is its own: undefined for the rest. A switch, not a
// table: package data moves the runtime's data lines.
func regTypeOf(k Kind) uint8 {
	switch k {
	case KindNumber:
		return typeNumber
	case KindNull, KindObject:
		return typeObject
	case KindBool:
		return typeBoolean
	case KindString:
		return typeString
	case KindSymbol:
		return typeSymbol
	case KindBigInt:
		return typeBigInt
	}
	return typeUndefined
}

// regSwitchTable is a switch instruction's table, as the tree tier's
// switchLookup makes one of the same cases: a number by an array's index
// where the cases are integers of a small range, by a short list or a map
// otherwise; a string by its code units, past the longest case's length
// not even flattened; true, false and null by their kind. Each selector's
// first case is the one it goes to, as the chain of === would.
type regSwitchTable struct {
	dense  []int32
	lo, hi float64
	few    []float64
	fewTo  []int32
	nums   map[float64]int32
	// byLen is where flat's strings of each length start, as the tree's
	// switchLookup groups them.
	byLen []int32
	flat  []bytecode.SwitchCase
	other [3]int32
	// typeTo is the body of each type's name in typeofNames, for
	// switch_typeof.
	typeTo [8]int32
}

// newRegSwitchTable is the table of cases.
func newRegSwitchTable(cases []bytecode.SwitchCase) *regSwitchTable {
	t := &regSwitchTable{other: [3]int32{-1, -1, -1}, lo: math.Inf(1), hi: math.Inf(-1)}
	for i := range t.typeTo {
		t.typeTo[i] = -1
	}
	nums := map[float64]int32{}
	var order []float64
	seen := map[string]bool{}
	var strs []bytecode.SwitchCase
	ints := true
	for _, c := range cases {
		switch c.Kind {
		case bytecode.SwitchNum:
			f := c.Num
			if f != f {
				continue // NaN equals nothing.
			}
			if _, ok := nums[f]; !ok {
				nums[f] = int32(c.To)
				order = append(order, f)
				t.lo, t.hi = min(t.lo, f), max(t.hi, f)
				ints = ints && f == math.Trunc(f) && !math.IsInf(f, 0)
			}
		case bytecode.SwitchStr:
			if !seen[c.Str] {
				seen[c.Str] = true
				strs = append(strs, c)
				for i, name := range typeofNames {
					if name == c.Str {
						t.typeTo[i] = int32(c.To)
					}
				}
			}
		default:
			if i := c.Kind - bytecode.SwitchTrue; t.other[i] < 0 {
				t.other[i] = int32(c.To)
			}
		}
	}
	if len(strs) > 0 {
		most := 0
		lens := make([]int, len(strs))
		for i, s := range strs {
			lens[i] = NewString(s.Str).length
			most = max(most, lens[i])
		}
		t.byLen = make([]int32, most+2)
		for n := 0; n <= most; n++ {
			t.byLen[n] = int32(len(t.flat))
			for i, s := range strs {
				if lens[i] == n {
					t.flat = append(t.flat, s)
				}
			}
		}
		t.byLen[most+1] = int32(len(t.flat))
	}
	switch {
	case len(nums) == 0:
	case ints && t.hi-t.lo < switchDense:
		t.dense = make([]int32, int(t.hi-t.lo)+1)
		for i := range t.dense {
			t.dense[i] = -1
		}
		for f, to := range nums {
			t.dense[int(f-t.lo)] = to
		}
	case len(nums) <= 8:
		for _, f := range order {
			t.few, t.fewTo = append(t.few, f), append(t.fewTo, nums[f])
		}
	default:
		t.nums = nums
	}
	return t
}

// lookup is the pc of the body v goes to, or -1: a string the constant a
// case was made from first by its pointer, as the tree's lookup asks.
func (t *regSwitchTable) lookup(v Value, consts []Value) int32 {
	switch {
	case v.IsNumber():
		f := v.num
		if t.dense != nil {
			if f >= t.lo && f <= t.hi {
				if i := int(f - t.lo); float64(i)+t.lo == f {
					return t.dense[i]
				}
			}
			return -1
		}
		for i, g := range t.few {
			if f == g {
				return t.fewTo[i]
			}
		}
		if t.nums != nil {
			if to, ok := t.nums[f]; ok {
				return to
			}
		}
	case v.IsString():
		s := (*String)(v.ref)
		if s.length >= len(t.byLen)-1 {
			return -1
		}
		for _, e := range t.flat[t.byLen[s.length]:t.byLen[s.length+1]] {
			if int(e.K) < len(consts) && consts[e.K].ref == unsafe.Pointer(s) {
				return int32(e.To)
			}
			if s.left != nil {
				s.Go()
			}
			if s.s == e.Str {
				return int32(e.To)
			}
		}
	case v.IsBool():
		if v.Truthy() {
			return t.other[0]
		}
		return t.other[1]
	case v.IsNull():
		return t.other[2]
	}
	return -1
}

// regCold is the operations runReg leaves to a case of one call: each
// case added to the loop moves the code of every other, which no placement
// varies, so only what runs per turn of a hot loop is a case of its own.
// It switches on the operation again. None of them throws.
func (r *Runtime) regCold(f *frame, c *regCode, k *regKept, regs *[1 << 16]Value) {
	w := regAt(c.code, k.pc-1)
	switch w.Op() {
	case bytecode.RNewArrayE:
		r.regNewArrayE(f, c, k, regs)
	case bytecode.RSwitch:
		regSwitch(c, k, regs[w.A()], f.cl.consts)
	case bytecode.RSwitchTypeof:
		regSwitchTypeof(c, k, regs[w.A()])
	case bytecode.RTypeof:
		r.regTypeof(c, k, regs)
	case bytecode.RNewObjectLit:
		r.regNewObjectLit(f, c, k, regs)
	}
}

// regColdSub is cold's operations, by the sub-operation in X: one opcode
// value for every operation that does not run per turn of a hot loop, and
// may throw. The loop runs cold through the slow path's case, regSlow,
// whose error it returns: an error check in regCold's case made empty_loop
// 7.5% and string_build1 6.4% slower at every placement, and a case of
// cold's own, before the free values', empty_loop 7.6% and loop_sum 4.4%,
// the loop's blocks laid out otherwise.
// Every one of them may throw: the frame's pc is the instruction's first.
func (r *Runtime) regColdSub(f *frame, c *regCode, k *regKept, regs *[1 << 16]Value) error {
	pc := k.pc - 1
	w := regAt(c.code, pc)
	f.pc = c.src[pc] + 1
	switch bytecode.RColdOp(w.X()) {
	case bytecode.RColdNewRegExp:
		v, err := r.newRegExpLiteral(&f.cl.fn.Constants[w.BC()])
		if err != nil {
			return err
		}
		regs[w.A()] = v
		return nil
	case bytecode.RColdForOfStart, bytecode.RColdForInStart:
		start := (*Runtime).startForOf
		if bytecode.RColdOp(w.X()) == bytecode.RColdForInStart {
			start = (*Runtime).startForIn
		}
		v, err := start(r, regs[w.B()])
		if err != nil {
			return err
		}
		regs[w.A()] = v
		return nil
	case bytecode.RColdIterClose:
		return r.iterCloseNormal(regs[w.B()])
	case bytecode.RColdIterCloseThrow:
		// As closeIteratorsIn closes a cursor an exception leaves: what
		// its return method throws is dropped.
		st := cursorOf(regs[w.B()])
		if st == nil || st.forIn || st.done || st.delegation || st.arr != nil && !r.unwalkToClose(st) {
			return nil
		}
		st.done = true
		r.closeIterator(st.iter)
		return nil
	}
	panic("register code: no cold operation " + bytecode.RColdOp(w.X()).String())
}

// regDeadCall is a call of o on recv with args whose value nothing reads
// but its truth: of the realm's exec or replace, the function told so, as
// deadExec's node tells it (deadCallRest); of anything else, the call.
func (r *Runtime) regDeadCall(o *Object, recv Value, args []Value) (Value, error) {
	switch o {
	case r.regexpExecFn:
		var mark *Object
		if recv.IsObject() {
			mark = recv.Object()
		}
		return deadCallRest[struct{}](r, r.regexpExecDead, mark, o, o.fn(), recv, args)
	case r.strReplaceFn:
		var mark *Object
		if len(args) > 0 && args[0].IsObject() {
			mark = args[0].Object()
		}
		return deadCallRest[struct{}](r, r.strReplaceDead, mark, o, o.fn(), recv, args)
	}
	return r.callRest(o, o.fn(), recv, args)
}

// regReuseSites is the sites of rc's calls of exec marked RMarkReuse, by
// call site, whose arrays execReuseOf says may be made again: its answer,
// of the stack form's call and locals, put in the frame's registers.
func regReuseSites(fn *bytecode.Function, rc *bytecode.RegCode) []*execReuseSite {
	var out []*execReuseSite
	for pc, w := range rc.Code {
		if !w.Op().MethodCall() || pc >= len(rc.Marks) || rc.Marks[pc]&bytecode.RMarkReuse == 0 {
			continue
		}
		spc := int(rc.Aux[pc])
		site := execReuseOf[struct{}](fn, spc+1, int(fn.Code[spc+1].A))
		if site == nil || rc.LocalReg[site.l] < 0 || site.s >= 0 && rc.LocalReg[site.s] < 0 {
			continue
		}
		rs := &execReuseSite{l: int(rc.LocalReg[site.l]), s: -1, strs: site.strs}
		if site.s >= 0 {
			rs.s = int(rc.LocalReg[site.s])
		}
		if out == nil {
			out = make([]*execReuseSite, rc.CallSites)
		}
		out[w.C()] = rs
	}
	return out
}

// regNewObjectLit is new_object_lit: the literal made with the room its
// site's objects came to need, as new_object makes it, and each property
// defined as define_field defines it, the values read from their
// registers. A fresh ordinary object takes each new key by a transition,
// and defining one can neither throw nor run code.
func (r *Runtime) regNewObjectLit(f *frame, c *regCode, k *regKept, regs *[1 << 16]Value) {
	pc := k.pc - 1
	w := regAt(c.code, pc)
	cl := f.cl
	n := int(w.X())
	ic := &cl.ic[w.C()]
	root := r.shapes.siteRoot(ic, r.proto.object)
	m := n
	if root != nil && int(root.slack) > m {
		m = int(root.slack)
	}
	o := newShapedLiteral(root.orRoot(ClassObject, r.proto.object), ClassObject, m)
	vals := regs[w.B() : int(w.B())+n]
	if final := ic.next; final != nil && root != nil && ic.s1 == root && n <= int(o.valc) {
		// The shape the keys led to from this root before, which the
		// site keeps as the tree's literal node does: transitions are
		// never undone, so the values go in place.
		dst := o.vals()[:n]
		copy(dst, vals)
		o.setVals(dst)
		o.shape = final
		if verifyShapes {
			checkShape(o)
		}
		regs[w.A()] = Obj(o)
		return
	}
	along := root != nil
	keys := c.rc.ObjKeys[c.rc.Aux[pc]]
	for i, key := range keys {
		name := cl.names[key]
		if next := o.transition(name, propDefault); next != nil {
			o.appendTransition(vals[i], next)
		} else {
			// No transition where the shape tree is full: the long way,
			// which for a fresh extensible object of distinct keys
			// cannot fail.
			along = false
			_ = r.defineOwnProp(o, name, vals[i], propDefault)
		}
	}
	if s := o.shape; along && s != nil && !s.unique && !s.loose && int(s.n) == n && int(o.valn) == n &&
		(s.index != nil || n <= linearScanLimit) {
		ic.next, ic.s1 = s, root
	}
	regs[w.A()] = Obj(o)
}

// regSwitch is switch: k's pc, past the instruction, made the body's
// where v equals a case.
func regSwitch(c *regCode, k *regKept, v Value, consts []Value) {
	w := regAt(c.code, k.pc-1)
	if to := c.switches[w.BC()].lookup(v, consts); to >= 0 {
		k.pc = int(to)
	}
}

// regSwitchTypeof is switch_typeof: k's pc made the body of v's type's
// name, where a case has it.
func regSwitchTypeof(c *regCode, k *regKept, v Value) {
	w := regAt(c.code, k.pc-1)
	if to := c.switches[w.BC()].typeTo[typeofKind[struct{}](v)]; to >= 0 {
		k.pc = int(to)
	}
}

// regTypeof is typeof: the type's name the runtime keeps, as
// typeofString answers it.
func (r *Runtime) regTypeof(c *regCode, k *regKept, regs *[1 << 16]Value) {
	w := regAt(c.code, k.pc-1)
	regs[w.A()] = Str(r.typeofString(regs[w.B()]))
}

// regNewArrayE is new_array_empty: the tree's node for the literal, made
// here, its site in the closure's arrays, named to native code by the
// stack form's pc. Making an array throws nothing, so the frame's pc is
// left as it is.
func (r *Runtime) regNewArrayE(f *frame, c *regCode, k *regKept, regs *[1 << 16]Value) {
	pc := k.pc - 1
	w := regAt(c.code, pc)
	n := arrayEmptyNode[struct{}]{site: int(w.B()), pc: int32(regSrc(c, pc))}
	regs[w.A()] = n.make(regTctx(r, f, f.cl))
}

// regCharCodeAtSlow is char_code_at as get_method_w and call_method_w make
// it: the method read, with the read's position (Aux), then the argument
// read, and the call, with the call's.
func (r *Runtime) regCharCodeAtSlow(f *frame, c *regCode, pc int, regs *[1 << 16]Value) error {
	w := regAt(c.code, pc)
	o := regs[w.B()]
	f.pc = c.rc.Aux[pc] + 1
	m, err := r.regGetMethod(f.cl, c, w, o)
	if err != nil {
		return err
	}
	a := regs[w.X()]
	f.pc = c.src[pc] + 1
	v, err := r.regCall(m, o, []Value{a})
	if err != nil {
		return err
	}
	regs[w.A()] = v
	return nil
}

// regCatch is where a register frame goes on after the instruction at
// k.pc-1 threw err: the pc of its handler, the innermost whose ranges hold
// the instruction, with the exception's value in the handler's register;
// or -1 where none does, or err is no JavaScript exception -- an interrupt,
// a memory limit -- which no handler catches, as in the interpreter
// (unwindToHandler). A call that threw has left the stack and the frames
// as they were before it.
//
//go:noinline
func (r *Runtime) regCatch(f *frame, c *regCode, k *regKept, regs *[1 << 16]Value, err error) int {
	if len(c.rc.Handlers) == 0 {
		return -1
	}
	th, ok := err.(*Thrown)
	if !ok {
		return -1
	}
	h := c.rc.HandlerAt(k.pc - 1)
	if h == nil {
		return -1
	}
	regs[h.Reg] = th.Value
	return int(h.To)
}

// regIterNextTree is iter_next of a generator's cursor, stepped as the
// tree steps one (iterNextTree): a generator of tree code resumed in
// place (stepGenTree), not through its next's call. Its body throws as the
// tree throws, with a panic, which a function of the tree with a for-of
// loop catches under its own recover; register code has none, so the
// step has one, and leaves the frames and the stack as runTreeHandlers
// does. The frame's tctx is the step's: the tree reads only its runtime.
func (r *Runtime) regIterNextTree(f *frame, cur Value) (v Value, more bool, err error) {
	depth, top := r.frameDepth, r.stackTop
	defer func() {
		if p := recover(); p != nil {
			th, ok := p.(treeThrow)
			if !ok {
				panic(p)
			}
			r.unwindTreeFrames(depth)
			r.stackTop = top
			v, more, err = Undefined, false, th.err
		}
	}()
	tc := &f.tc
	tc.r, tc.f = r, f
	return iterNextTree(tc, cur)
}

// regStepNext is iter_next of a cursor neither of an array, a for-in nor
// a generator, as iterNext steps it (stepIter): a next of script called as
// register code calls (regCall), its register code entered directly rather
// than through the frames a call from Go makes, and its result's done and
// value read; a Map's or a Set's iterator of this realm stepped as the
// tree's iterNextTree steps it; anything else by stepIter. The cursor is
// done once it is, or once a step fails.
func (r *Runtime) regStepNext(st *iterState) (Value, bool, error) {
	var fd *funcData
	if st.next.IsObject() {
		fd = st.next.Object().fn()
	}
	if fd == nil || fd.closure == nil || fd.native != nil {
		if fd != nil && fd.iterNext == iterNextMap && (fd.realm == nil || fd.realm == r.Realm) && st.iter.IsObject() {
			if d, ok := st.iter.Object().dataOf().(*mapIterData); ok {
				if err := r.tick(); err != nil {
					st.done = true
					return Undefined, false, err
				}
				v, ok := r.mapIterStep(d)
				if !ok {
					st.done = true
				}
				return v, ok, nil
			}
		}
		v, ok, err := r.stepIter(st.iter, st.next)
		if !ok {
			st.done = true
		}
		return v, ok, err
	}
	res, err := r.regCall(st.next, st.iter, nil)
	if err == nil && !res.IsObject() {
		err = r.throwTypeError("an iterator result must be an object")
	}
	if err != nil {
		st.done = true
		return Undefined, false, err
	}
	done, err := r.getValueProp(res, atomDone)
	if err != nil {
		st.done = true
		return Undefined, false, err
	}
	if done.Truthy() {
		st.done = true
		return Undefined, false, nil
	}
	v, err := r.getValueProp(res, atomValue)
	if err != nil {
		st.done = true
		return Undefined, false, err
	}
	return v, true, nil
}

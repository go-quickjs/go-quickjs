package vm

import (
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jsnum"
)

// tentry is a value on the operand stack, as the builder follows it: a tree
// to evaluate, or the frame's stack slot it is in.
type tentry struct {
	v tval
	// slot is the stack slot the entry is a read of, which is always its
	// own position, or -1 for a tree still to be evaluated.
	slot int
}

// tbuilder builds one block of a function's tree.
type tbuilder struct {
	fn    *bytecode.Function
	stack []tentry
	body  []tstmt
}

// buildTree builds a function's tree, or reports nil where the function has
// an instruction the tier does not build, or code it cannot follow.
func buildTree(fn *bytecode.Function) *tree {
	if !treeTier.Load() || fn.Generator || fn.Async || fn.TopLevel || fn.HasDirectEval || fn.MaxStack > 64 {
		return nil
	}
	code := fn.Code
	// Running a tree costs a little more to start than the interpreter does,
	// which only a loop wins back: a function without one runs as bytecode.
	// A call costs a tree more than it costs the interpreter, so a function
	// whose code is mostly calls does too.
	loops, calls := false, 0
	for pc, in := range code {
		switch in.Op {
		case bytecode.OpJump, bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue, bytecode.OpJumpIfCmpFalse:
			loops = loops || int(in.A) <= pc
		case bytecode.OpCall, bytecode.OpCallMethod, bytecode.OpNew:
			calls++
		}
	}
	if !treeEverything.Load() && (!loops || calls*treeCallDensity > len(code)) {
		return nil
	}
	leader := make([]bool, len(code)+1)
	leader[0] = true
	for pc, in := range code {
		if !treeBuilds(in.Op) {
			return nil
		}
		switch in.Op {
		case bytecode.OpJump, bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue, bytecode.OpJumpIfCmpFalse,
			bytecode.OpJumpIfFalseKeep, bytecode.OpJumpIfTrueKeep:
			if int(in.A) > len(code) {
				return nil
			}
			leader[in.A] = true
			leader[pc+1] = true
		case bytecode.OpReturn, bytecode.OpReturnUndef, bytecode.OpThrow:
			leader[pc+1] = true
		}
	}
	// Blocks are numbered in code order, from their first instruction.
	index := make([]int, len(code)+1)
	var starts []int
	for pc := 0; pc < len(code); pc++ {
		if leader[pc] {
			index[pc] = len(starts)
			starts = append(starts, pc)
		}
	}
	t := &tree{blocks: make([]tblock, len(starts))}
	depth := make([]int, len(starts))
	for i := range depth {
		depth[i] = -1
	}
	depth[0] = 0
	// A block is built once its entry depth is known, from a predecessor
	// built before it. A block nothing reaches is never run, and is given an
	// end that says so.
	work := []int{0}
	done := make([]bool, len(starts))
	for len(work) > 0 {
		bi := work[len(work)-1]
		work = work[:len(work)-1]
		if done[bi] {
			continue
		}
		done[bi] = true
		end := len(code)
		if bi+1 < len(starts) {
			end = starts[bi+1]
		}
		succ, ok := buildBlock(t, fn, bi, starts[bi], end, depth[bi], index, depth)
		if !ok {
			return nil
		}
		for _, s := range succ {
			if !done[s] {
				work = append(work, s)
			}
		}
	}
	for i := range t.blocks {
		if !done[i] {
			t.blocks[i].next = func(*tctx) int { panic("unreachable block of a tree") }
		}
	}
	return t
}

// treeCallDensity is the fewest instructions a function has for each call
// in it, for the tier to build it.
const treeCallDensity = 12

// treeBuilds reports whether the tier builds an instruction.
func treeBuilds(op bytecode.Op) bool {
	switch op {
	case bytecode.OpPushConst, bytecode.OpPushUndef, bytecode.OpPushNull, bytecode.OpPushTrue,
		bytecode.OpPushFalse, bytecode.OpPushInt, bytecode.OpPushThis,
		bytecode.OpDup, bytecode.OpDrop, bytecode.OpInsert2, bytecode.OpInsert3,
		bytecode.OpGetLocal, bytecode.OpGetLocal2, bytecode.OpSetLocal, bytecode.OpSetLocalGet,
		bytecode.OpClearLocal, bytecode.OpInitLocal, bytecode.OpIncLocal, bytecode.OpDecLocal,
		bytecode.OpUpdateLocal, bytecode.OpGetUpvalue, bytecode.OpSetUpvalue,
		bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv, bytecode.OpMod,
		bytecode.OpBitAnd, bytecode.OpBitOr, bytecode.OpBitXor, bytecode.OpShl, bytecode.OpShr, bytecode.OpUShr,
		bytecode.OpNeg, bytecode.OpInc, bytecode.OpDec, bytecode.OpToNumeric, bytecode.OpNot,
		bytecode.OpLt, bytecode.OpLe, bytecode.OpGt, bytecode.OpGe,
		bytecode.OpEq, bytecode.OpNe, bytecode.OpStrictEq, bytecode.OpStrictNe,
		bytecode.OpBinImm, bytecode.OpBinLocal, bytecode.OpLocalBinImm,
		bytecode.OpGetIndex, bytecode.OpGetLocalIndex, bytecode.OpGetLocalIndexUpdate, bytecode.OpSetIndex,
		bytecode.OpGetProp, bytecode.OpGetPropThis, bytecode.OpSetProp,
		bytecode.OpCall, bytecode.OpCallMethod, bytecode.OpNew, bytecode.OpClosure,
		bytecode.OpGetGlobal, bytecode.OpSetGlobal,
		bytecode.OpJump, bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue, bytecode.OpJumpIfCmpFalse,
		bytecode.OpReturn, bytecode.OpReturnUndef, bytecode.OpThrow,
		bytecode.OpNewArray, bytecode.OpNewObject, bytecode.OpDefineField, bytecode.OpGetLength, bytecode.OpTypeOf,
		bytecode.OpPushEmptyString, bytecode.OpPutLocal, bytecode.OpDup2, bytecode.OpSwap, bytecode.OpRot3, bytecode.OpRot4,
		bytecode.OpToPropertyKey, bytecode.OpToPropertyKeyOfBase, bytecode.OpSetHomeObject, bytecode.OpBitNot,
		bytecode.OpJumpIfFalseKeep, bytecode.OpJumpIfTrueKeep, bytecode.OpCheckGlobalRef, bytecode.OpAssertResolved:
		return true
	}
	return false
}

// buildBlock builds the block at bi, which runs code[start:end] from an
// operand stack depth of entry, and reports the blocks it can go on to.
func buildBlock(t *tree, fn *bytecode.Function, bi, start, end, entry int, index, depth []int) ([]int, bool) {
	b := &tbuilder{fn: fn}
	for d := 0; d < entry; d++ {
		b.pushSlot(d)
	}
	// target records a successor's entry depth, which every way into it
	// must agree on.
	var succ []int
	target := func(pc, d int) (int, bool) {
		if pc >= len(fn.Code) {
			return 0, false
		}
		s := index[pc]
		if depth[s] >= 0 && depth[s] != d {
			return 0, false
		}
		depth[s] = d
		succ = append(succ, s)
		return s, true
	}
	code := fn.Code
	for pc := start; pc < end; pc++ {
		in := code[pc]
		switch in.Op {
		case bytecode.OpJump:
			if !b.spill() {
				return nil, false
			}
			s, ok := target(int(in.A), len(b.stack))
			if !ok {
				return nil, false
			}
			d := len(b.stack)
			if int(in.A) <= pc {
				t.blocks[bi] = tblock{body: b.body, next: func(c *tctx) int { c.backEdge(pc, d); return s }}
			} else {
				t.blocks[bi] = tblock{body: b.body, next: func(*tctx) int { return s }}
			}
			return succ, true
		case bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue, bytecode.OpJumpIfCmpFalse:
			var cond func(c *tctx) bool
			switch in.Op {
			case bytecode.OpJumpIfCmpFalse:
				y, x := b.pop(), b.pop()
				cmp := bytecode.Op(in.B)
				test := compareNode(cmp, x.v, y.v, pc)
				cond = func(c *tctx) bool { return !test(c) }
			case bytecode.OpJumpIfFalse:
				v := b.pop().v
				cond = func(c *tctx) bool { return !truthy(v(c)) }
			default:
				v := b.pop().v
				cond = func(c *tctx) bool { return truthy(v(c)) }
			}
			if !b.spill() {
				return nil, false
			}
			d := len(b.stack)
			taken, ok := target(int(in.A), d)
			if !ok {
				return nil, false
			}
			fall, ok := target(pc+1, d)
			if !ok {
				return nil, false
			}
			if int(in.A) <= pc {
				t.blocks[bi] = tblock{body: b.body, next: func(c *tctx) int {
					if cond(c) {
						c.backEdge(pc, d)
						return taken
					}
					return fall
				}}
			} else {
				t.blocks[bi] = tblock{body: b.body, next: func(c *tctx) int {
					if cond(c) {
						return taken
					}
					return fall
				}}
			}
			return succ, true
		case bytecode.OpJumpIfFalseKeep, bytecode.OpJumpIfTrueKeep:
			// x && y and x || y as values: x stays on the stack where the
			// jump is taken, and goes where it is not, so it is in its slot
			// either way.
			if !b.spill() {
				return nil, false
			}
			d := b.depth()
			taken, ok := target(int(in.A), d)
			if !ok {
				return nil, false
			}
			fall, ok := target(pc+1, d-1)
			if !ok {
				return nil, false
			}
			want := in.Op == bytecode.OpJumpIfTrueKeep
			back := int(in.A) <= pc
			t.blocks[bi] = tblock{body: b.body, next: func(c *tctx) int {
				if truthy(c.stack[d-1]) == want {
					if back {
						c.backEdge(pc, d)
					}
					return taken
				}
				return fall
			}}
			return succ, true
		case bytecode.OpThrow:
			v := b.pop().v
			if !b.spill() {
				return nil, false
			}
			t.blocks[bi] = tblock{body: b.body, next: func(c *tctx) int {
				e := v(c)
				c.at(pc)
				c.throw(c.r.throw(e))
				return -1
			}}
			return succ, true
		case bytecode.OpReturn, bytecode.OpReturnUndef:
			v := tval(func(*tctx) Value { return Undefined })
			if in.Op == bytecode.OpReturn {
				v = b.pop().v
			}
			if !b.spill() {
				return nil, false
			}
			d := len(b.stack)
			t.blocks[bi] = tblock{body: b.body, next: func(c *tctx) int {
				res := v(c)
				if d > 0 {
					c.at(pc)
					if err := c.r.closeIteratorsReturning(c.f.base, c.f.base+d); err != nil {
						c.throw(err)
					}
				}
				if c.f.thisRef != nil {
					c.at(pc)
					var err error
					if res, err = c.r.derivedResult(c.f, c.cl, res); err != nil {
						c.throw(err)
					}
				}
				c.ret = res
				return -1
			}}
			return succ, true
		}
		skip, ok := b.op(pc, in, code, pc+1 < end)
		if !ok {
			return nil, false
		}
		pc += skip
	}
	// The block runs on into the next.
	if !b.spill() {
		return nil, false
	}
	s, ok := target(end, len(b.stack))
	if !ok {
		return nil, false
	}
	t.blocks[bi] = tblock{body: b.body, next: func(*tctx) int { return s }}
	return succ, true
}

func (b *tbuilder) push(v tval) { b.stack = append(b.stack, tentry{v: v, slot: -1}) }
func (b *tbuilder) pop() tentry {
	e := b.stack[len(b.stack)-1]
	b.stack = b.stack[:len(b.stack)-1]
	return e
}
func (b *tbuilder) depth() int { return len(b.stack) }
func (b *tbuilder) pushSlot(d int) {
	b.stack = append(b.stack, tentry{v: func(c *tctx) Value { return c.stack[d] }, slot: d})
}

// spill evaluates every entry still a tree into its stack slot, in order,
// so that what comes next can neither run before them nor change what they
// read. Every entry is then a read of its slot. It reports false where it
// cannot, which it always can.
func (b *tbuilder) spill() bool {
	var pos []int
	var vals []tval
	for i, e := range b.stack {
		if e.slot != i {
			pos = append(pos, i)
			vals = append(vals, e.v)
		}
	}
	// Each is written as soon as it is evaluated. A tree at a position is
	// made of what was pushed above it, so it reads only the slots from its
	// own up, and none of them is written before it is evaluated.
	switch len(pos) {
	case 0:
		return true
	case 1:
		p, v := pos[0], vals[0]
		b.body = append(b.body, func(c *tctx) { c.stack[p] = v(c) })
	case 2:
		p, v, q, w := pos[0], vals[0], pos[1], vals[1]
		b.body = append(b.body, func(c *tctx) { c.stack[p] = v(c); c.stack[q] = w(c) })
	case 3:
		p, v, q, w, o, u := pos[0], vals[0], pos[1], vals[1], pos[2], vals[2]
		b.body = append(b.body, func(c *tctx) { c.stack[p] = v(c); c.stack[q] = w(c); c.stack[o] = u(c) })
	default:
		b.body = append(b.body, func(c *tctx) {
			for i, v := range vals {
				c.stack[pos[i]] = v(c)
			}
		})
	}
	for _, p := range pos {
		b.stack[p] = tentry{}
		d := p
		b.stack[p] = tentry{v: func(c *tctx) Value { return c.stack[d] }, slot: d}
	}
	return true
}

// stmt adds a statement, after evaluating what is on the stack beneath it.
func (b *tbuilder) stmt(s tstmt) bool {
	if !b.spill() {
		return false
	}
	b.body = append(b.body, s)
	return true
}

// op builds one instruction other than a block's end, reporting how many
// instructions after it it took as well.
func (b *tbuilder) op(pc int, in bytecode.Instr, code []bytecode.Instr, more bool) (int, bool) {
	fn := b.fn
	switch in.Op {
	case bytecode.OpPushConst:
		k := in.A
		b.push(func(c *tctx) Value { return c.cl.consts[k] })
	case bytecode.OpPushUndef:
		b.push(func(*tctx) Value { return Undefined })
	case bytecode.OpPushNull:
		b.push(func(*tctx) Value { return Null })
	case bytecode.OpPushTrue:
		b.push(func(*tctx) Value { return True })
	case bytecode.OpPushFalse:
		b.push(func(*tctx) Value { return False })
	case bytecode.OpPushInt:
		v := Int32(int32(in.A))
		b.push(func(*tctx) Value { return v })
	case bytecode.OpPushThis:
		b.push(func(c *tctx) Value {
			v, bound := c.f.thisValue()
			if !bound {
				c.at(pc)
				c.throw(c.r.throwError(errReference, "\"this\" is not bound until super() has been called"))
			}
			return v
		})
	case bytecode.OpDup:
		if !b.spill() {
			return 0, false
		}
		d := b.depth()
		b.body = append(b.body, func(c *tctx) { c.stack[d] = c.stack[d-1] })
		b.pushSlot(d)
	case bytecode.OpDrop:
		e := b.pop()
		if e.slot < 0 {
			v := e.v
			return 0, b.stmt(func(c *tctx) { v(c) })
		}
	case bytecode.OpInsert2, bytecode.OpInsert3:
		if in.Op == bytecode.OpInsert3 && more && code[pc+1].Op == bytecode.OpSetIndex {
			// a[k] = v as an expression: the store, and its value.
			val, key, obj := b.pop(), b.pop(), b.pop()
			b.push(setIndexNode(obj.v, key.v, val.v, pc+1, fn.Strict))
			return 1, true
		}
		if in.Op == bytecode.OpInsert2 && more && code[pc+1].Op == bytecode.OpSetProp {
			val, obj := b.pop(), b.pop()
			b.push(setPropNode(obj.v, val.v, code[pc+1], pc+1, fn.Strict))
			return 1, true
		}
		if !b.spill() {
			return 0, false
		}
		d := b.depth()
		if in.Op == bytecode.OpInsert2 {
			// a b -> b a b
			b.body = append(b.body, func(c *tctx) {
				s := c.stack
				v := s[d-1]
				s[d] = v
				s[d-1] = s[d-2]
				s[d-2] = v
			})
		} else {
			// a b c -> c a b c
			b.body = append(b.body, func(c *tctx) {
				s := c.stack
				v := s[d-1]
				s[d] = v
				s[d-1] = s[d-2]
				s[d-2] = s[d-3]
				s[d-3] = v
			})
		}
		b.pushSlot(d)
	case bytecode.OpGetLocal:
		k := in.A
		b.push(func(c *tctx) Value { return c.locals[k] })
	case bytecode.OpGetLocal2:
		k, j := in.A, in.B
		b.push(func(c *tctx) Value { return c.locals[k] })
		b.push(func(c *tctx) Value { return c.locals[j] })
	case bytecode.OpSetLocal, bytecode.OpInitLocal:
		k, v := in.A, b.pop().v
		return 0, b.stmt(func(c *tctx) { c.locals[k] = v(c) })
	case bytecode.OpSetLocalGet:
		k, j, v := in.A, in.B, b.pop().v
		if !b.stmt(func(c *tctx) { c.locals[k] = v(c) }) {
			return 0, false
		}
		b.push(func(c *tctx) Value { return c.locals[j] })
	case bytecode.OpClearLocal:
		k := in.A
		return 0, b.stmt(func(c *tctx) { c.locals[k] = Undefined })
	case bytecode.OpIncLocal, bytecode.OpDecLocal:
		k := in.A
		delta := 1.0
		if in.Op == bytecode.OpDecLocal {
			delta = -1
		}
		return 0, b.stmt(func(c *tctx) {
			if v := c.locals[k]; v.IsNumber() {
				c.locals[k] = Float(v.num + delta)
				return
			}
			c.at(pc)
			c.locals[k] = c.step(c.locals[k], delta)
		})
	case bytecode.OpUpdateLocal:
		k, postfix := in.A, in.B&bytecode.UpdatePostfix != 0
		delta := 1.0
		if in.B&bytecode.UpdateDec != 0 {
			delta = -1
		}
		b.push(func(c *tctx) Value {
			old := c.locals[k]
			if old.IsNumber() {
				next := Float(old.num + delta)
				c.locals[k] = next
				if postfix {
					return old
				}
				return next
			}
			c.at(pc)
			n, err := c.r.toNumeric(old)
			if err != nil {
				c.throw(err)
			}
			next := c.step(n, delta)
			c.locals[k] = next
			if postfix {
				return n
			}
			return next
		})
	case bytecode.OpGetUpvalue:
		k := in.A
		b.push(func(c *tctx) Value { return c.cl.upvalues[k].get() })
	case bytecode.OpSetUpvalue:
		k, v := in.A, b.pop().v
		return 0, b.stmt(func(c *tctx) { c.cl.upvalues[k].set(v(c)) })
	case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv, bytecode.OpMod,
		bytecode.OpBitAnd, bytecode.OpBitOr, bytecode.OpBitXor, bytecode.OpShl, bytecode.OpShr, bytecode.OpUShr:
		y, x := b.pop(), b.pop()
		b.push(binaryNode(in.Op, x.v, y.v, pc))
	case bytecode.OpLt, bytecode.OpLe, bytecode.OpGt, bytecode.OpGe,
		bytecode.OpEq, bytecode.OpNe, bytecode.OpStrictEq, bytecode.OpStrictNe:
		y, x := b.pop(), b.pop()
		test := compareNode(in.Op, x.v, y.v, pc)
		b.push(func(c *tctx) Value { return Bool(test(c)) })
	case bytecode.OpNeg:
		x := b.pop().v
		b.push(func(c *tctx) Value {
			a := x(c)
			if a.IsNumber() {
				return Float(-a.num)
			}
			c.at(pc)
			v, err := c.r.negate(a)
			if err != nil {
				c.throw(err)
			}
			return v
		})
	case bytecode.OpInc, bytecode.OpDec:
		x := b.pop().v
		delta := 1.0
		if in.Op == bytecode.OpDec {
			delta = -1
		}
		b.push(func(c *tctx) Value {
			a := x(c)
			if a.IsNumber() {
				return Float(a.num + delta)
			}
			c.at(pc)
			return c.step(a, delta)
		})
	case bytecode.OpToNumeric:
		x := b.pop().v
		b.push(func(c *tctx) Value {
			a := x(c)
			if a.IsNumber() {
				return a
			}
			c.at(pc)
			n, err := c.r.toNumeric(a)
			if err != nil {
				c.throw(err)
			}
			return n
		})
	case bytecode.OpNot:
		x := b.pop().v
		b.push(func(c *tctx) Value { return Bool(!truthy(x(c))) })
	case bytecode.OpBinImm:
		x := b.pop().v
		imm := Int32(int32(in.A))
		b.push(binaryNode(bytecode.Op(in.B), x, func(*tctx) Value { return imm }, pc))
	case bytecode.OpBinLocal:
		x, k := b.pop().v, in.A
		b.push(binaryNode(bytecode.Op(in.B), x, func(c *tctx) Value { return c.locals[k] }, pc))
	case bytecode.OpLocalBinImm:
		k := in.A & (1<<24 - 1)
		imm := Int32(int32(in.B))
		b.push(binaryNode(bytecode.Op(in.A>>24), func(c *tctx) Value { return c.locals[k] },
			func(*tctx) Value { return imm }, pc))
	case bytecode.OpGetIndex:
		key, obj := b.pop().v, b.pop().v
		b.push(getIndexNode(obj, key, pc))
	case bytecode.OpGetLocalIndex:
		k, j := in.A, in.B
		b.push(getIndexNode(func(c *tctx) Value { return c.locals[k] },
			func(c *tctx) Value { return c.locals[j] }, pc))
	case bytecode.OpGetLocalIndexUpdate:
		b.push(getLocalIndexUpdateNode(in, pc))
	case bytecode.OpSetIndex:
		val, key, obj := b.pop(), b.pop(), b.pop()
		set := setIndexNode(obj.v, key.v, val.v, pc, fn.Strict)
		return 0, b.stmt(func(c *tctx) { set(c) })
	case bytecode.OpGetProp:
		obj := b.pop().v
		b.push(getPropNode(obj, in, pc))
	case bytecode.OpGetPropThis:
		// The receiver stays beneath the method: it is read from its slot
		// twice rather than evaluated twice.
		if !b.spill() {
			return 0, false
		}
		d := b.depth() - 1
		b.push(getPropThisNode(d, in, pc))
	case bytecode.OpSetProp:
		val, obj := b.pop(), b.pop()
		set := setPropNode(obj.v, val.v, in, pc, fn.Strict)
		return 0, b.stmt(func(c *tctx) { set(c) })
	case bytecode.OpCall, bytecode.OpCallMethod, bytecode.OpNew:
		// The callee and the arguments go to their slots, which is where a
		// call takes its arguments from.
		if !b.spill() {
			return 0, false
		}
		n := int(in.A)
		p := b.depth() - n - 1
		this := -1
		if in.Op == bytecode.OpCallMethod {
			this = p - 1
		}
		for i := 0; i <= n; i++ {
			b.pop()
		}
		if this >= 0 {
			b.pop()
		}
		b.push(callNode(in.Op, p, n, this, pc))
	case bytecode.OpCheckGlobalRef:
		name := in.A
		b.push(func(c *tctx) Value {
			r, f, cl := c.r, c.f, c.cl
			name := cl.names[name]
			env := cl.scope()
			switch {
			case f.evalVars != nil && evalVarProp(f.evalVars, name) != nil:
			case r.globalLexProp(env, name) != nil:
			case !r.isGlobalScope(env) && r.moduleLexProp(env, name) != nil:
			case r.nodeQuirks && chainHasProxy(env):
			default:
				c.at(pc)
				found, err := r.hasPropErr(env, name)
				if err != nil {
					c.throw(err)
				}
				return Bool(found)
			}
			return True
		})
	case bytecode.OpAssertResolved:
		v, found, name := b.pop().v, b.pop().v, in.A
		b.push(func(c *tctx) Value {
			ok := found(c)
			x := v(c)
			if !ok.Truthy() {
				c.at(pc)
				c.throw(c.r.throwReferenceError("%s is not defined", c.r.atoms.name(c.cl.names[name])))
			}
			return x
		})
	case bytecode.OpPushEmptyString:
		b.push(func(*tctx) Value { return Str(emptyString) })
	case bytecode.OpPutLocal:
		// The value is stored and stays: it is stored when it is evaluated,
		// which is in its turn.
		k, v := in.A, b.pop().v
		b.push(func(c *tctx) Value { x := v(c); c.locals[k] = x; return x })
	case bytecode.OpDup2, bytecode.OpSwap, bytecode.OpRot3, bytecode.OpRot4:
		if !b.spill() {
			return 0, false
		}
		d := b.depth()
		switch in.Op {
		case bytecode.OpDup2:
			b.body = append(b.body, func(c *tctx) { c.stack[d], c.stack[d+1] = c.stack[d-2], c.stack[d-1] })
			b.pushSlot(d)
			b.pushSlot(d + 1)
		case bytecode.OpSwap:
			b.body = append(b.body, func(c *tctx) { c.stack[d-1], c.stack[d-2] = c.stack[d-2], c.stack[d-1] })
		case bytecode.OpRot3:
			b.body = append(b.body, func(c *tctx) {
				s := c.stack
				s[d-3], s[d-2], s[d-1] = s[d-2], s[d-1], s[d-3]
			})
		default:
			b.body = append(b.body, func(c *tctx) {
				s := c.stack
				s[d-4], s[d-3], s[d-2], s[d-1] = s[d-3], s[d-2], s[d-1], s[d-4]
			})
		}
	case bytecode.OpToPropertyKey, bytecode.OpToPropertyKeyOfBase:
		// The object beneath the key is looked at first, and stays: it is
		// read from its slot.
		key := b.pop().v
		if !b.spill() {
			return 0, false
		}
		d, ofBase := b.depth(), in.Op == bytecode.OpToPropertyKeyOfBase
		b.push(func(c *tctx) Value {
			v := key(c)
			if ofBase {
				if base := c.stack[d-1]; base.IsNullish() {
					c.at(pc)
					c.throw(c.r.throwTypeError("cannot read property of %s", c.r.describe(base)))
				}
			}
			if v.IsString() || v.IsSymbol() || ofBase && v.IsNumber() {
				return v
			}
			c.at(pc)
			k, err := c.r.toPropertyKey(v)
			if err != nil {
				c.throw(err)
			}
			return c.r.keyToValue(k)
		})
	case bytecode.OpSetHomeObject:
		if !b.spill() {
			return 0, false
		}
		d, below := b.depth(), int(in.A)
		b.body = append(b.body, func(c *tctx) {
			if fnVal := c.stack[d-1]; fnVal.IsObject() {
				if home := c.stack[d-1-below]; fnVal.Object().fn() != nil && home.IsObject() {
					fnVal.Object().fn().homeObject = home.Object()
				}
			}
		})
	case bytecode.OpBitNot:
		x := b.pop().v
		b.push(func(c *tctx) Value {
			v := x(c)
			if !v.IsNumber() {
				c.at(pc)
				n, err := c.r.toNumeric(v)
				if err != nil {
					c.throw(err)
				}
				if n.IsBigInt() {
					out := &BigInt{}
					out.V.Not(&n.BigInt().V)
					return Big(out)
				}
				v = n
			}
			i, err := c.r.toInt32(v)
			if err != nil {
				c.throw(err)
			}
			return Int32(^i)
		})
	case bytecode.OpNewArray:
		// The elements go to their slots, from which the array is made.
		if !b.spill() {
			return 0, false
		}
		n := int(in.A)
		p := b.depth() - n
		for i := 0; i < n; i++ {
			b.pop()
		}
		b.push(func(c *tctx) Value { return Obj(c.r.newArrayFrom(c.stack[p : p+n])) })
	case bytecode.OpNewObject:
		n, site := int(in.A), in.B
		b.push(func(c *tctx) Value {
			o := newLiteralObject(c.r.proto.object, ClassObject, n)
			o.shape = c.r.shapes.siteRoot(&c.cl.ic[site])
			return Obj(o)
		})
	case bytecode.OpDefineField:
		// The object stays where it is, beneath the value: it is defined on
		// in its slot.
		val := b.pop().v
		if !b.spill() {
			return 0, false
		}
		d, name := b.depth()-1, in.A
		b.body = append(b.body, func(c *tctx) {
			v := val(c)
			obj := c.stack[d]
			if !obj.IsObject() {
				return
			}
			key := c.cl.names[name]
			if o := obj.Object(); o.class == ClassObject && o.flags&objExtensible != 0 {
				if next := o.transition(key, propDefault); next != nil {
					o.appendTransition(Property{key: key, flags: propDefault, value: v}, next)
					return
				}
			}
			c.at(pc)
			if err := c.r.defineOwnProp(obj.Object(), key, v, propDefault); err != nil {
				c.throw(err)
			}
		})
	case bytecode.OpGetLength:
		x := b.pop().v
		b.push(func(c *tctx) Value {
			o := x(c)
			c.at(pc)
			v, err := c.r.getValueProp(o, atomLength)
			if err != nil {
				c.throw(err)
			}
			return v
		})
	case bytecode.OpTypeOf:
		x := b.pop().v
		b.push(func(c *tctx) Value { return Str(c.r.typeofString(x(c))) })
	case bytecode.OpClosure:
		k := in.A
		b.push(func(c *tctx) Value { return Obj(c.r.makeClosure(c.f, c.cl.consts[k])) })
	case bytecode.OpGetGlobal:
		b.push(func(c *tctx) Value {
			v, err := c.r.getGlobalAt(c, in, pc)
			if err != nil {
				c.throw(err)
			}
			return v
		})
	case bytecode.OpSetGlobal:
		v := b.pop().v
		return 0, b.stmt(func(c *tctx) {
			if err := c.r.setGlobalAt(c, in, v(c), pc); err != nil {
				c.throw(err)
			}
		})
	default:
		return 0, false
	}
	return 0, true
}

// step is ++ or -- by delta on a value that is not a number: converted, and
// stepped as a BigInt or a number. The caller has set the pc.
func (c *tctx) step(v Value, delta float64) Value {
	n, err := c.r.toNumeric(v)
	if err != nil {
		c.throw(err)
	}
	if n.IsBigInt() {
		res, err := c.r.arith(bytecode.OpAdd, n, Big(NewBigInt(int64(delta))))
		if err != nil {
			c.throw(err)
		}
		return res
	}
	return Float(n.Number() + delta)
}

// binaryNode is a binary operator over two trees, with the interpreter's
// fast path for two numbers.
func binaryNode(op bytecode.Op, x, y tval, pc int) tval {
	slow := func(c *tctx, a, b Value) Value {
		c.at(pc)
		var v Value
		var err error
		switch op {
		case bytecode.OpAdd:
			v, err = c.r.add(a, b)
		case bytecode.OpBitAnd, bytecode.OpBitOr, bytecode.OpBitXor, bytecode.OpShl, bytecode.OpShr, bytecode.OpUShr:
			v, err = c.r.bitwise(op, a, b)
		default:
			v, err = c.r.arith(op, a, b)
		}
		if err != nil {
			c.throw(err)
		}
		return v
	}
	switch op {
	case bytecode.OpAdd:
		return func(c *tctx) Value {
			a, b := x(c), y(c)
			if a.IsNumber() && b.IsNumber() {
				return Float(a.num + b.num)
			}
			return slow(c, a, b)
		}
	case bytecode.OpSub:
		return func(c *tctx) Value {
			a, b := x(c), y(c)
			if a.IsNumber() && b.IsNumber() {
				return Float(a.num - b.num)
			}
			return slow(c, a, b)
		}
	case bytecode.OpMul:
		return func(c *tctx) Value {
			a, b := x(c), y(c)
			if a.IsNumber() && b.IsNumber() {
				return Float(a.num * b.num)
			}
			return slow(c, a, b)
		}
	case bytecode.OpDiv:
		return func(c *tctx) Value {
			a, b := x(c), y(c)
			if a.IsNumber() && b.IsNumber() {
				return Float(a.num / b.num)
			}
			return slow(c, a, b)
		}
	case bytecode.OpMod:
		return func(c *tctx) Value {
			a, b := x(c), y(c)
			if a.IsNumber() && b.IsNumber() {
				return Float(numericOp(op, a.num, b.num))
			}
			return slow(c, a, b)
		}
	}
	return func(c *tctx) Value {
		a, b := x(c), y(c)
		if a.IsNumber() && b.IsNumber() {
			fa, fb := a.num, b.num
			p, q := int32(fa), int32(fb)
			if float64(p) != fa {
				p = jsnum.ToInt32(fa)
			}
			if float64(q) != fb {
				q = jsnum.ToInt32(fb)
			}
			switch op {
			case bytecode.OpBitAnd:
				return Int32(p & q)
			case bytecode.OpBitOr:
				return Int32(p | q)
			case bytecode.OpBitXor:
				return Int32(p ^ q)
			case bytecode.OpShl:
				return Int32(p << (uint32(q) & 31))
			case bytecode.OpShr:
				return Int32(p >> (uint32(q) & 31))
			default:
				return Uint32(uint32(p) >> (uint32(q) & 31))
			}
		}
		return slow(c, a, b)
	}
}

// compareNode is a comparison over two trees, as a condition.
func compareNode(op bytecode.Op, x, y tval, pc int) func(c *tctx) bool {
	switch op {
	case bytecode.OpStrictEq:
		return func(c *tctx) bool { a := x(c); return a.StrictEquals(y(c)) }
	case bytecode.OpStrictNe:
		return func(c *tctx) bool { a := x(c); return !a.StrictEquals(y(c)) }
	case bytecode.OpEq, bytecode.OpNe:
		want := op == bytecode.OpEq
		return func(c *tctx) bool {
			a, b := x(c), y(c)
			c.at(pc)
			eq, err := c.r.looseEquals(a, b)
			if err != nil {
				c.throw(err)
			}
			return eq == want
		}
	case bytecode.OpLt:
		return func(c *tctx) bool {
			a, b := x(c), y(c)
			if a.IsNumber() && b.IsNumber() {
				return a.num < b.num
			}
			return c.compareSlow(op, a, b, pc)
		}
	case bytecode.OpLe:
		return func(c *tctx) bool {
			a, b := x(c), y(c)
			if a.IsNumber() && b.IsNumber() {
				return a.num <= b.num
			}
			return c.compareSlow(op, a, b, pc)
		}
	case bytecode.OpGt:
		return func(c *tctx) bool {
			a, b := x(c), y(c)
			if a.IsNumber() && b.IsNumber() {
				return a.num > b.num
			}
			return c.compareSlow(op, a, b, pc)
		}
	}
	return func(c *tctx) bool {
		a, b := x(c), y(c)
		if a.IsNumber() && b.IsNumber() {
			return a.num >= b.num
		}
		return c.compareSlow(op, a, b, pc)
	}
}

func (c *tctx) compareSlow(op bytecode.Op, a, b Value, pc int) bool {
	c.at(pc)
	res, err := c.r.compare(a, b)
	if err != nil {
		c.throw(err)
	}
	return relationalResult(op, res)
}

// elemAt is an element in an array's dense storage, as get_index reads one
// first thing, or false.
func elemAt(obj, key Value) (Value, bool) {
	if obj.IsObject() && key.IsNumber() {
		o := obj.Object()
		if i := uint32(key.num); float64(i) == key.num && uint(i) < uint(len(o.elems)) && o.flags&objMappedArguments == 0 {
			if v := o.elems[i]; !isHole(v) {
				return v, true
			}
		}
	}
	return Undefined, false
}

func getIndexNode(obj, key tval, pc int) tval {
	return func(c *tctx) Value {
		o, k := obj(c), key(c)
		if v, ok := elemAt(o, k); ok {
			return v
		}
		c.at(pc)
		v, err := c.r.getIndexed(o, k)
		if err != nil {
			c.throw(err)
		}
		return v
	}
}

// getLocalIndexUpdateNode is a[i++] and its kind over locals: the object is
// read before the update, which may run a valueOf that assigns to it.
func getLocalIndexUpdateNode(in bytecode.Instr, pc int) tval {
	k, slot := in.A, in.B>>2
	dec, postfix := in.B&bytecode.UpdateDec != 0, in.B&bytecode.UpdatePostfix != 0
	delta := 1.0
	if dec {
		delta = -1
	}
	return func(c *tctx) Value {
		obj := c.locals[k]
		var key Value
		if old := c.locals[slot]; old.IsNumber() {
			next := Float(old.num + delta)
			c.locals[slot] = next
			key = next
			if postfix {
				key = old
			}
		} else {
			c.at(pc)
			var err error
			if key, err = c.r.updateLocal(&c.locals[slot], in.B&3); err != nil {
				c.throw(err)
			}
		}
		if v, ok := elemAt(obj, key); ok {
			return v
		}
		c.at(pc)
		v, err := c.r.getIndexed(obj, key)
		if err != nil {
			c.throw(err)
		}
		return v
	}
}

// setIndexNode is obj[key] = val, and gives val, which is the value of the
// assignment where it is used as one.
func setIndexNode(obj, key, val tval, pc int, strict bool) tval {
	return func(c *tctx) Value {
		o, k := obj(c), key(c)
		v := val(c)
		if o.IsObject() && k.IsNumber() {
			a := o.Object()
			if i := uint32(k.num); float64(i) == k.num &&
				uint(i) < uint(len(a.elems)) && a.flags&objMappedArguments == 0 && !isHole(a.elems[i]) {
				a.elems[i] = v
				return v
			}
		}
		c.at(pc)
		if o.IsNullish() {
			c.throw(c.r.throwTypeError("cannot set property of %s", c.r.describe(o)))
		}
		pk, err := c.r.toPropertyKey(k)
		if err != nil {
			c.throw(err)
		}
		if err := c.r.setValueProp(o, pk, v, strict); err != nil {
			c.throw(err)
		}
		return v
	}
}

func getPropNode(obj tval, in bytecode.Instr, pc int) tval {
	name, site := in.A, in.B
	return func(c *tctx) Value {
		o := obj(c)
		if o.IsObject() {
			ob := o.Object()
			if v, ok := plainOwn(ob, c.cl.names[name]); ok {
				return v
			}
			c.at(pc)
			if v, ok, err := c.r.cachedGet(&c.cl.ic[site], ob, c.cl.names[name]); ok {
				if err != nil {
					c.throw(err)
				}
				return v
			}
		}
		c.at(pc)
		v, err := c.r.getValueProp(o, c.cl.names[name])
		if err != nil {
			c.throw(err)
		}
		return v
	}
}

func getPropThisNode(d int, in bytecode.Instr, pc int) tval {
	name, site := in.A, in.B
	return func(c *tctx) Value {
		recv := c.stack[d]
		if recv.IsObject() {
			if v, ok := c.r.cachedProp(&c.cl.ic[site], recv.Object(), c.cl.names[name]); ok {
				return v
			}
		}
		c.at(pc)
		v, err := c.r.getValueProp(recv, c.cl.names[name])
		if err != nil {
			c.throw(err)
		}
		return v
	}
}

// setPropNode is obj.name = val, and gives val.
func setPropNode(obj, val tval, in bytecode.Instr, pc int, strict bool) tval {
	name, site := in.A, in.B
	return func(c *tctx) Value {
		o, v := obj(c), val(c)
		c.at(pc)
		if o.IsObject() {
			if err := c.r.setPropCached(&c.cl.ic[site], o.Object(), c.cl.names[name], v, strict); err != nil {
				c.throw(err)
			}
			return v
		}
		if err := c.r.setValueProp(o, c.cl.names[name], v, strict); err != nil {
			c.throw(err)
		}
		return v
	}
}

// callNode is a call, a method call or a construction whose callee is in
// slot p and arguments in the n slots after it, and the receiver of a
// method call in slot this.
func callNode(op bytecode.Op, p, n, this, pc int) tval {
	switch op {
	case bytecode.OpNew:
		return func(c *tctx) Value {
			c.at(pc)
			v, err := c.r.construct(c.stack[p], c.stack[p+1:p+1+n])
			if err != nil {
				c.throw(err)
			}
			return v
		}
	case bytecode.OpCallMethod:
		return func(c *tctx) Value {
			c.at(pc)
			v, err := c.r.callFromLoop(c.stack[p], c.stack[this], c.stack[p+1:p+1+n])
			if err != nil {
				c.throw(err)
			}
			return v
		}
	}
	return func(c *tctx) Value {
		c.at(pc)
		v, err := c.r.callFromLoop(c.stack[p], Undefined, c.stack[p+1:p+1+n])
		if err != nil {
			c.throw(err)
		}
		return v
	}
}

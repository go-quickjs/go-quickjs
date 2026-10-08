package vm

import (
	"math"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Updates in the tree tier: x op= y, x++ and x-- over a place -- a property,
// an element, a global or an upvalue -- as one node.
//
// The compiler emits an update as the place's read, the operand, the
// operator and the place's store, with the copies that carry the result out
// where it is used:
//
//	READ [to_numeric KEEP] (inc | dec | bin_imm | bin_local | OPERAND op) [KEEP] STORE [drop]
//
// KEEP is insert2 for a property, insert3 for an element and dup for a
// global or an upvalue: before the operator, it keeps the old value, as a
// postfix update's result; after it, the new one. A local's update is an
// instruction of its own, or a store the tree already builds as one
// statement (storeArith). Built one instruction at a time, the update is a
// read node, an arithmetic node and a store, three closures, with the
// place's object and key checked by both the read and the store.
//
// The builder recognizes the whole of it at the read (tbuilder.update) and
// builds one node of an rmw, the place and what is done to it. Where the
// operand is a local or a number, a statement updates a number in the
// place's storage where it is, behind the read's own checks: nothing runs
// between the read and the write that could change the place. Anything
// else -- a value that is not a number, a getter, a setter, a cache that
// does not answer, an operand that is a tree -- takes rmwRun, which does
// what the instructions do, one step at a time: the object and key
// evaluated once, the key converted once, the read, ToNumeric of a postfix
// update's old value, the operand, the operator and the store, each where
// it may throw or run code. An operand that would need a statement of its
// own -- a call, anything spilled -- would run before the read, and the
// update is then built as before.

// placeKind is what an update's place is.
type placeKind uint8

const (
	placeProp placeKind = iota
	placeElem
	placeGlobal
	placeUpvalue
)

// refKind says where a node finds a value it reads without a node of its
// own.
type refKind uint8

const (
	refTree refKind = iota
	refLocal
	refSlot
	refThis
	refUpvalue
	refConst
)

// tref is a value an update reads: a tree to call, or what it reads in
// place -- a local, a stack slot, this, an upvalue or a constant.
type tref struct {
	kind refKind
	k    uint32
	n    Value
	v    tval
}

// refOf is the entry as a tref.
func refOf(e tentry) tref {
	switch {
	case e.v != nil:
		return tref{kind: refTree, v: e.v}
	case e.slot >= 0:
		return tref{kind: refSlot, k: uint32(e.slot)}
	case e.local:
		return tref{kind: refLocal, k: e.k}
	case e.upvalue:
		return tref{kind: refUpvalue, k: e.k}
	case e.this:
		return tref{kind: refThis}
	case e.number, e.literal:
		return tref{kind: refConst, n: e.n}
	}
	return tref{kind: refTree, v: e.tree()}
}

// ref reads the value r is.
func (c *tctx) ref(r *tref) Value {
	switch r.kind {
	case refLocal:
		return c.locals[r.k]
	case refConst:
		return r.n
	case refSlot:
		return c.stack[r.k]
	case refThis:
		return c.f.this
	case refUpvalue:
		return *c.cl.upvalues[r.k].slot
	}
	return r.v(c)
}

// sameRef reports whether two entries read the same thing: the same local,
// upvalue or number, this, or the same stack slot -- one a dup before a read
// left where it was (see tbuilder.op's dup).
func sameRef(x, y tentry) bool {
	switch {
	case x.v != nil || y.v != nil:
		return false
	case x.slot >= 0 || y.slot >= 0:
		return x.slot >= 0 && x.slot == y.slot
	case x.local && y.local, x.upvalue && y.upvalue:
		return x.k == y.k
	case x.this && y.this:
		return true
	case x.number && y.number:
		return math.Float64bits(x.n.num) == math.Float64bits(y.n.num)
	}
	return false
}

// rmw is an update: its place, what is done to it, and the instructions'
// positions, for the steps that may throw.
type rmw struct {
	kind placeKind
	// obj and key are a property's object, an element's object and key.
	obj, key tref
	// name is a property's or a global's name; site is the read's cache,
	// and set the store's. getIn and setIn are a global's read and store.
	name, site, set uint32
	getIn, setIn    bytecode.Instr
	// upv is an upvalue's index.
	upv uint32
	// ofBase is to_property_key_of_base's position, for an element whose key
	// is converted once before the read, or -1.
	ofBase int
	// getPC, opPC and setPC are the read's, the operator's and the store's
	// positions; numPC is to_numeric's, before a postfix update whose value
	// is used, or -1.
	getPC, numPC, opPC, setPC int
	// op is the operator; ++ and -- are step, by delta.
	op    bytecode.Op
	step  bool
	delta float64
	// y is the operand; yLocal is its local, or -1, for a node that reads a
	// local operand in place.
	y      tref
	yLocal int
	// value says the update's value is used: the old value, ToNumeric'd,
	// for postfix, and the new one otherwise.
	value, postfix bool
	strict         bool
}

// updateOperandMax bounds how far past the read the builder follows an
// operand looking for the update's operator and store.
const updateOperandMax = 64

// isUpdateOp reports whether an operator is one an update over a place is
// built with.
func isUpdateOp(op bytecode.Op) bool {
	switch op {
	case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpDiv, bytecode.OpMod,
		bytecode.OpBitAnd, bytecode.OpBitOr, bytecode.OpBitXor, bytecode.OpShl, bytecode.OpShr, bytecode.OpUShr:
		return true
	}
	return false
}

// keepOp is the copy that keeps an update's value beneath a place's object
// and key, for its store.
func (u *rmw) keepOp() bytecode.Op {
	switch u.kind {
	case placeProp:
		return bytecode.OpInsert2
	case placeElem:
		return bytecode.OpInsert3
	}
	return bytecode.OpDup
}

// isStore reports whether in is the store of the update's place.
func (u *rmw) isStore(in bytecode.Instr) bool {
	switch u.kind {
	case placeProp:
		return in.Op == bytecode.OpSetProp && in.A == u.name
	case placeElem:
		return in.Op == bytecode.OpSetIndex
	case placeGlobal:
		return in.Op == bytecode.OpSetGlobal && in.A == u.name
	}
	return in.Op == bytecode.OpSetUpvalue && in.A == u.upv
}

// updatePlaceholder stands for an update's old value on the builder's stack
// while its operand is built; nothing evaluates it.
var updatePlaceholder = tentry{slot: -2, v: func(*tctx) Value { panic("an update's read evaluated apart from it") }}

// update builds the read of u's place at pc, whose entries are the top n of
// the stack, and the update that follows it from code[next], as one node,
// reporting how many instructions after pc it took. It reports false, and
// leaves the builder as it found it, where what follows is not an update of
// the place it can build as one.
//
//go:noinline
func (b *tbuilder) update(u *rmw, n, pc, next int) (int, bool) {
	code := b.fn.Code
	end := min(b.end, next+updateOperandMax)
	// The store is looked for first: most reads are not updates.
	found := false
	for i := next; i < end; i++ {
		if u.isStore(code[i]) {
			found = true
			break
		}
	}
	if !found {
		return 0, false
	}
	saved := append([]tentry(nil), b.stack...)
	body := len(b.body)
	fail := func() (int, bool) {
		b.stack = append(b.stack[:0], saved...)
		b.body = b.body[:body]
		return 0, false
	}
	b.stack = b.stack[:len(b.stack)-n]
	d := len(b.stack)
	u.numPC, u.yLocal, u.strict = -1, -1, b.fn.Strict
	i := next
	if i+2 < end && code[i].Op == bytecode.OpToNumeric && code[i+1].Op == u.keepOp() &&
		(code[i+2].Op == bytecode.OpInc || code[i+2].Op == bytecode.OpDec) {
		u.numPC, u.value, u.postfix = i, true, true
		i += 2
	}
	if i >= end {
		return fail()
	}
	switch in := code[i]; in.Op {
	case bytecode.OpInc, bytecode.OpDec:
		u.op, u.step, u.delta, u.y = bytecode.OpAdd, true, 1, tref{kind: refConst, n: Int(1)}
		if in.Op == bytecode.OpDec {
			u.delta, u.y.n = -1, Int(-1)
		}
		u.opPC = i
		i++
	case bytecode.OpBinImm:
		if u.postfix || !isUpdateOp(bytecode.Op(in.B)) {
			return fail()
		}
		u.op, u.opPC, u.y = bytecode.Op(in.B), i, tref{kind: refConst, n: Int32(int32(in.A))}
		i++
	case bytecode.OpBinLocal:
		if u.postfix {
			return fail()
		}
		u.op, u.opPC, u.y = bytecode.Op(in.B), i, tref{kind: refLocal, k: in.A}
		i++
	default:
		if u.postfix {
			return fail()
		}
		// The operand, built as any other tree over the old value, which
		// is the first instruction to take that value.
		b.pushEntry(updatePlaceholder)
		outer := b.noUpdate
		b.noUpdate = true
		for {
			if i >= end {
				b.noUpdate = outer
				return fail()
			}
			in := code[i]
			if len(b.stack) == d+2 && isUpdateOp(in.Op) {
				break
			}
			if !treeBuilds(in.Op) || endsBlock(in.Op) {
				b.noUpdate = outer
				return fail()
			}
			skip, ok := b.op(i, in, code, i+1 < b.end)
			if !ok || len(b.body) != body || len(b.stack) <= d || b.stack[d].slot != -2 {
				b.noUpdate = outer
				return fail()
			}
			i += 1 + skip
		}
		b.noUpdate = outer
		u.y = refOf(b.pop())
		b.pop()
		u.op, u.opPC = code[i].Op, i
		i++
	}
	if u.y.kind == refLocal {
		u.yLocal = int(u.y.k)
	}
	// A global's or an upvalue's store takes its value, so a postfix
	// update's new value is copied for it, beside the old one, and the copy
	// dropped after the store: to_numeric dup inc dup STORE drop.
	copied := u.postfix && u.keepOp() == bytecode.OpDup
	if (!u.postfix || copied) && i < end && code[i].Op == u.keepOp() {
		u.value = true
		i++
	} else if copied {
		return fail()
	}
	if i >= end || !u.isStore(code[i]) {
		return fail()
	}
	u.setPC, u.set, u.setIn = i, code[i].B, code[i]
	i++
	if copied {
		if i >= b.end || code[i].Op != bytecode.OpDrop {
			return fail()
		}
		i++
	}
	if u.value && i < b.end && code[i].Op == bytecode.OpDrop {
		u.value = false
		i++
	}
	if u.value {
		b.push(rmwNode(u))
	} else if !b.stmt(rmwStmt(u)) {
		return fail()
	}
	return i - pc - 1, true
}

// endsBlock reports whether an instruction ends a block, or is built apart
// from tbuilder.op.
func endsBlock(op bytecode.Op) bool {
	switch op {
	case bytecode.OpJump, bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue, bytecode.OpJumpIfCmpFalse,
		bytecode.OpJumpIfFalseKeep, bytecode.OpJumpIfTrueKeep, bytecode.OpJumpIfNullish, bytecode.OpJumpIfNotNullish,
		bytecode.OpReturn, bytecode.OpReturnUndef, bytecode.OpThrow, bytecode.OpTailCall, bytecode.OpTailCallMethod:
		return true
	}
	return false
}

// placeUpdate is the read at pc as the read of an update, where it is one
// tbuilder.update builds: get_prop over an object read twice (or copied, or
// this pushed twice), get_index over an object and key read twice,
// get_local_index over the locals beneath it, to_property_key_of_base's
// dup2 and get_index, get_global and get_upvalue.
func (b *tbuilder) placeUpdate(pc int, in bytecode.Instr) (int, bool) {
	if b.noUpdate {
		return 0, false
	}
	s, code := b.stack, b.fn.Code
	n := len(s)
	switch in.Op {
	case bytecode.OpGetProp:
		// An arrow's or a derived constructor's this is a tree, which the
		// second push_this reads again with nothing between.
		if n < 2 || !sameRef(s[n-2], s[n-1]) &&
			!(pc >= 2 && code[pc-1].Op == bytecode.OpPushThis && code[pc-2].Op == bytecode.OpPushThis && s[n-2].v != nil && s[n-1].v != nil) {
			return 0, false
		}
		return b.update(&rmw{kind: placeProp, obj: refOf(s[n-2]), name: in.A, site: in.B, getPC: pc}, 2, pc, pc+1)
	case bytecode.OpGetIndex:
		if n < 4 || !sameRef(s[n-4], s[n-2]) || !sameRef(s[n-3], s[n-1]) {
			return 0, false
		}
		return b.update(&rmw{kind: placeElem, obj: refOf(s[n-4]), key: refOf(s[n-3]), ofBase: -1, getPC: pc}, 4, pc, pc+1)
	case bytecode.OpGetLocalIndex:
		if n < 2 || !sameRef(s[n-2], localEntry(in.A)) || !sameRef(s[n-1], localEntry(in.B)) {
			return 0, false
		}
		return b.update(&rmw{kind: placeElem, obj: refOf(s[n-2]), key: refOf(s[n-1]), ofBase: -1, getPC: pc}, 2, pc, pc+1)
	case bytecode.OpToPropertyKeyOfBase:
		// Followed by dup2 and get_index, which the caller has seen.
		return b.update(&rmw{kind: placeElem, obj: refOf(s[n-2]), key: refOf(s[n-1]), ofBase: pc, getPC: pc + 2}, 2, pc, pc+3)
	case bytecode.OpGetGlobal:
		return b.update(&rmw{kind: placeGlobal, name: in.A, site: in.B, getIn: in, getPC: pc}, 0, pc, pc+1)
	case bytecode.OpGetUpvalue:
		return b.update(&rmw{kind: placeUpvalue, upv: in.A, getPC: pc}, 0, pc, pc+1)
	}
	return 0, false
}

// rmwNode is an update whose value is used: a node that updates a number
// where it is, for a property of a local or this, an element of a local
// array over a local key, or an upvalue, with a local or constant operand,
// and otherwise rmwRun.
//
//go:noinline
func rmwNode(u *rmw) tval {
	if u.y.kind == refLocal || u.y.kind == refConst && u.y.n.IsNumber() {
		switch {
		case u.kind == placeProp && (u.obj.kind == refLocal || u.obj.kind == refThis):
			return propValueUpdate(u)
		case u.kind == placeElem && u.obj.kind == refLocal && u.key.kind == refLocal:
			return elemValueUpdate(u)
		case u.kind == placeUpvalue:
			return upvalueValueUpdate(u)
		}
	}
	return func(c *tctx) Value { return c.rmwRun(u) }
}

// updated is an update's value, given the old value and the new: the old
// one for postfix, a number here.
func updated(old, r Value, postfix bool) Value {
	if postfix {
		return old
	}
	return r
}

// propValueUpdate is o.name op= y as a value, o a local or this.
//
//go:noinline
func propValueUpdate(u *rmw) tval {
	o, this, site, op, y, yl, post := u.obj.k, u.obj.kind == refThis, u.set, u.op, u.y.n.num, u.yLocal, u.postfix
	return func(c *tctx) Value {
		ov := c.f.this
		if !this {
			ov = c.locals[o]
		}
		if ob := objectAt(&ov); ob != nil {
			if p := storeSlot(&c.cl.ic[site], ob); p != nil && numberAt(&p.value) {
				b := y
				if yl >= 0 {
					v := &c.locals[yl]
					if !numberAt(v) {
						return c.rmwAt(u, ov, Undefined)
					}
					b = v.num
				}
				old := p.value
				r := Float(updateNum(op, old.num, b))
				p.value.num = r.num
				return updated(old, r, post)
			}
		}
		return c.rmwAt(u, ov, Undefined)
	}
}

// elemValueUpdate is a[k] op= y as a value, a and k locals.
//
//go:noinline
func elemValueUpdate(u *rmw) tval {
	o, k, op, y, yl, post := u.obj.k, u.key.k, u.op, u.y.n.num, u.yLocal, u.postfix
	return func(c *tctx) Value {
		ov, kv := c.locals[o], c.locals[k]
		if a := objectAt(&ov); a != nil {
			if i := uint32(kv.num); float64(i) == kv.num && uint(i) < uint(len(a.elems)) && a.flags&objMappedArguments == 0 {
				if p := &a.elems[i]; numberAt(p) {
					b := y
					if yl >= 0 {
						v := &c.locals[yl]
						if !numberAt(v) {
							return c.rmwAt(u, ov, kv)
						}
						b = v.num
					}
					old := *p
					r := Float(updateNum(op, old.num, b))
					p.num = r.num
					return updated(old, r, post)
				}
			}
		}
		return c.rmwAt(u, ov, kv)
	}
}

// upvalueValueUpdate is v op= y as a value, v an upvalue.
//
//go:noinline
func upvalueValueUpdate(u *rmw) tval {
	k, op, y, yl, post := u.upv, u.op, u.y.n.num, u.yLocal, u.postfix
	return func(c *tctx) Value {
		if p := c.cl.upvalues[k].slot; numberAt(p) {
			b := y
			if yl >= 0 {
				v := &c.locals[yl]
				if !numberAt(v) {
					return c.rmwRun(u)
				}
				b = v.num
			}
			old := *p
			r := Float(updateNum(op, old.num, b))
			p.num = r.num
			return updated(old, r, post)
		}
		return c.rmwRun(u)
	}
}

// rmwStmt is an update as a statement: one that updates a number where it
// is, where the place is one with a node of its own, and otherwise rmwRun.
// The nodes over a local or constant operand read, update and write the
// number in one step; those over an operand that is a tree read the place,
// evaluate the operand and then look again before they write, as the
// operand may have changed the place.
//
//go:noinline
func rmwStmt(u *rmw) tstmt {
	leaf := u.y.kind == refLocal || u.y.kind == refConst && u.y.n.IsNumber()
	tree := u.y.kind == refTree
	switch u.kind {
	case placeProp:
		switch {
		case u.obj.kind == refLocal && leaf:
			return propLocalUpdate(u)
		case u.obj.kind == refThis && leaf:
			return propThisUpdate(u)
		case (u.obj.kind == refLocal || u.obj.kind == refThis) && tree:
			return propTreeUpdate(u)
		}
	case placeElem:
		if u.obj.kind != refLocal {
			break
		}
		switch k := u.key.n.num; {
		case u.key.kind == refLocal && leaf:
			return elemLocalUpdate(u)
		case u.key.kind == refConst && leaf && float64(uint32(k)) == k && uint32(k) < 1<<31:
			return elemIndexUpdate(u)
		case u.key.kind == refLocal && tree, u.key.kind == refTree && (leaf || tree):
			return elemTreeUpdate(u)
		}
	case placeGlobal:
		if leaf {
			return globalUpdateStmt(u)
		}
	case placeUpvalue:
		switch {
		case leaf:
			return upvalueUpdateStmt(u)
		case tree:
			return upvalueTreeUpdate(u)
		}
	}
	return func(c *tctx) { c.rmwRun(u) }
}

// updateNum is a op b on two numbers, + asked first.
func updateNum(op bytecode.Op, a, b float64) float64 {
	if op == bytecode.OpAdd {
		return a + b
	}
	return updateNumOp(op, a, b)
}

// updateNumOp is a op b on two numbers, as the arithmetic and bitwise nodes
// compute it, for an op other than +, which updateNum makes itself.
func updateNumOp(op bytecode.Op, a, b float64) float64 {
	switch op {
	case bytecode.OpSub:
		return a - b
	case bytecode.OpMul:
		return a * b
	case bytecode.OpDiv:
		return a / b
	case bytecode.OpMod:
		return jsMod(a, b)
	case bytecode.OpBitAnd:
		return float64(toInt32(a) & toInt32(b))
	case bytecode.OpBitOr:
		return float64(toInt32(a) | toInt32(b))
	case bytecode.OpBitXor:
		return float64(toInt32(a) ^ toInt32(b))
	case bytecode.OpShl:
		return float64(toInt32(a) << (uint32(toInt32(b)) & 31))
	case bytecode.OpShr:
		return float64(toInt32(a) >> (uint32(toInt32(b)) & 31))
	}
	return float64(uint32(toInt32(a)) >> (uint32(toInt32(b)) & 31))
}

// storeSlot is the property the write cache s says o has itself, holding
// data, writable: o is of the shape s remembers, as for storeOwn. The value
// in it is what a read of the property gives.
func storeSlot(s *propCache, o *Object) *Property {
	if sh := o.shape; sh == s.shape && sh != nil && s.next == nil {
		if verifyShapes {
			checkShape(o)
		}
		return &o.props[s.idx]
	}
	return nil
}

// Each statement below comes in two closures, for an operand that is a
// number, which needs no test, and for one that is a local.

// propLocalUpdate is o.name op= y as a statement, o a local.
//
//go:noinline
func propLocalUpdate(u *rmw) tstmt {
	o, site, op, y, yl := u.obj.k, u.set, u.op, u.y.n.num, u.yLocal
	if yl >= 0 {
		return func(c *tctx) {
			if ob := objectAt(&c.locals[o]); ob != nil {
				if p := storeSlot(&c.cl.ic[site], ob); p != nil && numberAt(&p.value) {
					if b := &c.locals[yl]; numberAt(b) {
						p.value.num = Float(updateNum(op, p.value.num, b.num)).num
						return
					}
				}
			}
			c.rmwRun(u)
		}
	}
	return func(c *tctx) {
		if ob := objectAt(&c.locals[o]); ob != nil {
			if p := storeSlot(&c.cl.ic[site], ob); p != nil && numberAt(&p.value) {
				p.value.num = Float(updateNum(op, p.value.num, y)).num
				return
			}
		}
		c.rmwRun(u)
	}
}

// propThisUpdate is this.name op= y as a statement.
//
//go:noinline
func propThisUpdate(u *rmw) tstmt {
	site, op, y, yl := u.set, u.op, u.y.n.num, u.yLocal
	if yl >= 0 {
		return func(c *tctx) {
			if ob := objectAt(&c.f.this); ob != nil {
				if p := storeSlot(&c.cl.ic[site], ob); p != nil && numberAt(&p.value) {
					if b := &c.locals[yl]; numberAt(b) {
						p.value.num = Float(updateNum(op, p.value.num, b.num)).num
						return
					}
				}
			}
			c.rmwRun(u)
		}
	}
	return func(c *tctx) {
		if ob := objectAt(&c.f.this); ob != nil {
			if p := storeSlot(&c.cl.ic[site], ob); p != nil && numberAt(&p.value) {
				p.value.num = Float(updateNum(op, p.value.num, y)).num
				return
			}
		}
		c.rmwRun(u)
	}
}

// elemLocalUpdate is a[k] op= y as a statement, a and k locals: a number in
// an array's dense storage is updated where it is.
//
//go:noinline
func elemLocalUpdate(u *rmw) tstmt {
	o, k, op, y, yl := u.obj.k, u.key.k, u.op, u.y.n.num, u.yLocal
	if yl >= 0 {
		return func(c *tctx) {
			if a, k := objectAt(&c.locals[o]), c.locals[k]; a != nil {
				if i := uint32(k.num); float64(i) == k.num && uint(i) < uint(len(a.elems)) && a.flags&objMappedArguments == 0 {
					if p, b := &a.elems[i], &c.locals[yl]; numberAt(p) {
						if numberAt(b) {
							p.num = Float(updateNum(op, p.num, b.num)).num
							return
						}
					}
				}
			}
			c.rmwRun(u)
		}
	}
	return func(c *tctx) {
		if a, k := objectAt(&c.locals[o]), c.locals[k]; a != nil {
			if i := uint32(k.num); float64(i) == k.num && uint(i) < uint(len(a.elems)) && a.flags&objMappedArguments == 0 {
				if p := &a.elems[i]; numberAt(p) {
					p.num = Float(updateNum(op, p.num, y)).num
					return
				}
			}
		}
		c.rmwRun(u)
	}
}

// elemIndexUpdate is a[i] op= y as a statement, a a local and i a constant
// index.
//
//go:noinline
func elemIndexUpdate(u *rmw) tstmt {
	o, i, op, y, yl := u.obj.k, uint32(u.key.n.num), u.op, u.y.n.num, u.yLocal
	if yl >= 0 {
		return func(c *tctx) {
			if a := objectAt(&c.locals[o]); a != nil && uint(i) < uint(len(a.elems)) && a.flags&objMappedArguments == 0 {
				if p, b := &a.elems[i], &c.locals[yl]; numberAt(p) {
					if numberAt(b) {
						p.num = Float(updateNum(op, p.num, b.num)).num
						return
					}
				}
			}
			c.rmwRun(u)
		}
	}
	return func(c *tctx) {
		if a := objectAt(&c.locals[o]); a != nil && uint(i) < uint(len(a.elems)) && a.flags&objMappedArguments == 0 {
			if p := &a.elems[i]; numberAt(p) {
				p.num = Float(updateNum(op, p.num, y)).num
				return
			}
		}
		c.rmwRun(u)
	}
}

// globalUpdateStmt is g op= y as a statement: a plain writable property of
// the global object, in the slot the store's site remembers, where nothing
// shadows it, is updated where it is.
//
//go:noinline
func globalUpdateStmt(u *rmw) tstmt {
	name, site, op, y, yl := u.name, u.set, u.op, u.y.n.num, u.yLocal
	return func(c *tctx) {
		s := &c.cl.ic[site]
		if env := s.p1; env != nil && c.f.evalVars == nil {
			if r := c.r; len(r.globalLex.props) == 0 || !r.lexShadows(c.cl.names[name]) {
				if i := uint(s.idx); i < uint(len(env.props)) {
					if p := &env.props[i]; p.flags&(propAccessor|propPrivate|propDeleted|propUninit|propWritable) == propWritable && numberAt(&p.value) {
						b := y
						if yl >= 0 {
							if v := &c.locals[yl]; numberAt(v) {
								b = v.num
							} else {
								c.rmwRun(u)
								return
							}
						}
						p.value.num = Float(updateNum(op, p.value.num, b)).num
						return
					}
				}
			}
		}
		c.rmwRun(u)
	}
}

// upvalueUpdateStmt is v op= y as a statement, v an upvalue.
//
//go:noinline
func upvalueUpdateStmt(u *rmw) tstmt {
	k, op, y, yl := u.upv, u.op, u.y.n.num, u.yLocal
	if yl >= 0 {
		return func(c *tctx) {
			if p, b := c.cl.upvalues[k].slot, &c.locals[yl]; numberAt(p) && numberAt(b) {
				p.num = Float(updateNum(op, p.num, b.num)).num
				return
			}
			c.rmwRun(u)
		}
	}
	return func(c *tctx) {
		if p := c.cl.upvalues[k].slot; numberAt(p) {
			p.num = Float(updateNum(op, p.num, y)).num
			return
		}
		c.rmwRun(u)
	}
}

// propTreeUpdate is o.name op= y as a statement, o a local or this and y a
// tree: the property is read where the store's cache says it is, y is
// evaluated, and the property is written where the cache still says it is.
//
//go:noinline
func propTreeUpdate(u *rmw) tstmt {
	o, this, site, op, yv := u.obj.k, u.obj.kind == refThis, u.set, u.op, u.y.v
	return func(c *tctx) {
		ov := c.f.this
		if !this {
			ov = c.locals[o]
		}
		if ob := objectAt(&ov); ob != nil {
			s := &c.cl.ic[site]
			if p := storeSlot(s, ob); p != nil && numberAt(&p.value) {
				old := p.value
				y := yv(c)
				if y.IsNumber() {
					if p := storeSlot(s, ob); p != nil && numberAt(&p.value) {
						p.value.num = Float(updateNum(op, old.num, y.num)).num
						return
					}
				}
				c.rmwSet(u, ov, Undefined, c.rmwOp(u, old, y))
				return
			}
		}
		c.rmwAt(u, ov, Undefined)
	}
}

// elemTreeUpdate is a[k] op= y as a statement, a a local, where k or y is a
// tree: the element is read in the array's dense storage, y is evaluated,
// and the element written where it still is, the array's storage looked at
// again.
//
//go:noinline
func elemTreeUpdate(u *rmw) tstmt {
	o, kk, kv, op := u.obj.k, u.key.k, u.key.v, u.op
	yk, yl, yn, yv := u.y.kind, u.y.k, u.y.n, u.y.v
	return func(c *tctx) {
		ov := c.locals[o]
		var k Value
		if kv != nil {
			k = kv(c)
		} else {
			k = c.locals[kk]
		}
		if a := objectAt(&ov); a != nil {
			if i := uint32(k.num); float64(i) == k.num && uint(i) < uint(len(a.elems)) && a.flags&objMappedArguments == 0 {
				if p := &a.elems[i]; numberAt(p) {
					old := *p
					var y Value
					switch yk {
					case refConst:
						y = yn
					case refLocal:
						y = c.locals[yl]
					default:
						y = yv(c)
					}
					if y.IsNumber() && uint(i) < uint(len(a.elems)) {
						if p := &a.elems[i]; numberAt(p) {
							p.num = Float(updateNum(op, old.num, y.num)).num
							return
						}
					}
					c.rmwSet(u, ov, k, c.rmwOp(u, old, y))
					return
				}
			}
		}
		c.rmwAt(u, ov, k)
	}
}

// upvalueTreeUpdate is v op= y as a statement, v an upvalue and y a tree.
//
//go:noinline
func upvalueTreeUpdate(u *rmw) tstmt {
	k, op, yv := u.upv, u.op, u.y.v
	return func(c *tctx) {
		if p := c.cl.upvalues[k].slot; numberAt(p) {
			old := *p
			y := yv(c)
			if p := c.cl.upvalues[k].slot; y.IsNumber() && numberAt(p) {
				p.num = Float(updateNum(op, old.num, y.num)).num
				return
			}
			c.rmwSet(u, Undefined, Undefined, c.rmwOp(u, old, y))
			return
		}
		c.rmwRun(u)
	}
}

// rmwRun is an update as its instructions do it, one step at a time, and
// its value: the object and the key evaluated, the key converted, the read,
// a postfix update's ToNumeric, the operand, the operator and the store,
// each where it may throw or run code, with each node's own fast path.
func (c *tctx) rmwRun(u *rmw) Value {
	var o, k Value
	switch u.kind {
	case placeProp:
		o = c.ref(&u.obj)
	case placeElem:
		o = c.ref(&u.obj)
		k = c.ref(&u.key)
	}
	return c.rmwAt(u, o, k)
}

// rmwAt is rmwRun once the object and the key are evaluated.
func (c *tctx) rmwAt(u *rmw, o, k Value) Value {
	var old Value
	switch u.kind {
	case placeProp:
		if o.IsObject() {
			ob := o.object()
			if v, ok := c.cl.ic[u.site].own(ob); ok {
				old = v
				break
			}
			if v, ok := ownScan(&c.cl.ic[u.site], ob, c.cl.names[u.name]); ok {
				old = v
				break
			}
		}
		old = c.getPropSlow(o, u.name, u.site, u.getPC)
	case placeElem:
		if u.ofBase >= 0 && (o.IsNullish() || !k.IsNumber() && !k.IsString() && !k.IsSymbol()) {
			k = c.keyOfBase(o, k, u.ofBase)
		}
		v, ok := elemAt(o, k)
		if !ok {
			v = c.getIndexSlow(o, k, u.getPC)
		}
		old = v
	case placeGlobal:
		if p := c.globalSite(u.site, u.name); p != nil && p.flags&(propAccessor|propPrivate|propDeleted|propUninit) == 0 {
			old = p.value
			break
		}
		v, err := c.r.getGlobalAt(c, u.getIn, u.getPC)
		if err != nil {
			c.throw(err)
		}
		old = v
	default:
		old = *c.cl.upvalues[u.upv].slot
	}
	if u.numPC >= 0 && !old.IsNumber() {
		c.at(u.numPC)
		n, err := c.r.toNumeric(old)
		if err != nil {
			c.throw(err)
		}
		old = n
	}
	var r Value
	if u.step {
		if old.IsNumber() {
			r = Float(old.num + u.delta)
		} else {
			c.at(u.opPC)
			r = c.step(old, u.delta)
		}
	} else {
		r = c.rmwOp(u, old, c.ref(&u.y))
	}
	c.rmwSet(u, o, k, r)
	if u.postfix {
		return old
	}
	return r
}

// rmwOp is the update's operator on the old value and the operand, a step
// for ++ and --.
func (c *tctx) rmwOp(u *rmw, old, y Value) Value {
	switch {
	case old.IsNumber() && y.IsNumber():
		return Float(updateNum(u.op, old.num, y.num))
	case u.step:
		c.at(u.opPC)
		return c.step(old, u.delta)
	case isBitwise(u.op):
		return c.bitwiseSlow(u.op, old, y, u.opPC)
	}
	return c.arithSlow(u.op, old, y, u.opPC)
}

// rmwSet is the update's store of r, to the object and key it read.
func (c *tctx) rmwSet(u *rmw, o, k, r Value) {
	switch u.kind {
	case placeProp:
		if !o.IsObject() || !c.cl.ic[u.set].storeOwn(o.object(), r) {
			c.setProp(o, r, u.name, u.set, u.setPC, u.strict)
		}
	case placeElem:
		if !setElem(o, k, r) {
			c.setIndexSlow(o, k, r, u.setPC, u.strict)
		}
	case placeGlobal:
		if p := c.globalSite(u.set, u.name); p != nil && p.flags&(propAccessor|propPrivate|propDeleted|propUninit|propWritable) == propWritable {
			p.value = r
			return
		}
		if err := c.r.setGlobalAt(c, u.setIn, r, u.setPC); err != nil {
			c.throw(err)
		}
	default:
		*c.cl.upvalues[u.upv].slot = r
	}
}

// globalSite is the global object's property in the slot a global read's or
// write's site remembers for name, where nothing can shadow it -- a direct
// eval's variables or a script's lexical binding of the name -- or nil.
func (c *tctx) globalSite(site, name uint32) *Property {
	s := &c.cl.ic[site]
	if env := s.p1; env != nil && c.f.evalVars == nil {
		if r := c.r; len(r.globalLex.props) == 0 || !r.lexShadows(c.cl.names[name]) {
			if i := uint(s.idx); i < uint(len(env.props)) {
				return &env.props[i]
			}
		}
	}
	return nil
}

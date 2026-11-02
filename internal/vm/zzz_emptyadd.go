package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// "" + x of a local x -- the compiler's push_empty_string and bin_local
// add, how scripts convert a value to a string, an HTML escaper its input
// -- as a node of the tree tier that answers a string x itself, as V8's
// StringAdd of the empty string answers the other operand, and asks the
// addition's slow path (arithSlow, arithOther, add, concat) only for
// anything else. x = "" + x, a set_local, an init_local or a
// set_local_get of the sum right after it, is one statement of the same:
// storeArith's statement had made the addition its own way, through the
// slow path, 18 ns of a call that does nothing else.
//
// The addition's entry is the builder's own (OpBinLocal's), its arith
// kept, which a template's or a + chain's join reads (OpConcat); only its
// tree is another.
//
// It is generic, instantiated with struct{} alone, and asked by opHook
// (see zzz_typeof.go), so that every function and closure there was stays
// at its address.

// emptyAddOp builds the bin_local at pc where it adds local in.A to the
// empty string beneath it -- and the store of the sum that may come next
// -- reporting how many instructions after it it took, whether it built
// them, and whether it was such an addition at all (handled), which the
// builder builds otherwise.
func emptyAddOp[_ any](b *tbuilder, pc int, code []bytecode.Instr, more bool) (skip int, ok, handled bool) {
	in := code[pc]
	n := len(b.stack)
	if bytecode.Op(in.B) != bytecode.OpAdd || n == 0 || !b.stack[n-1].emptyStr || b.stack[n-1].slot >= 0 {
		return 0, false, false
	}
	if skip, ok = b.op(pc, in, code, more); !ok || skip != 0 || len(b.stack) != n {
		return skip, ok, true
	}
	e := &b.stack[n-1]
	slow, k := e.v, in.A
	if slow == nil || e.arith == nil || e.slot >= 0 {
		return 0, true, true
	}
	e.v = func(c *tctx) Value {
		if v := c.locals[k]; v.IsString() {
			return v
		}
		return slow(c)
	}
	if !more {
		return 0, true, true
	}
	st := code[pc+1]
	switch st.Op {
	case bytecode.OpSetLocal, bytecode.OpInitLocal, bytecode.OpSetLocalGet:
	default:
		return 0, true, true
	}
	// The sum is a string, which no BigInt's owner keeps (ownedStore).
	b.pop()
	d := st.A
	if !b.stmt(func(c *tctx) {
		if v := c.locals[k]; v.IsString() {
			c.locals[d] = v
			return
		}
		c.locals[d] = slow(c)
	}) {
		return 1, false, true
	}
	if st.Op == bytecode.OpSetLocalGet {
		// And then local st.B, as set_local_get pushes it.
		b.pushEntry(localEntry(st.B))
	}
	return 1, true, true
}

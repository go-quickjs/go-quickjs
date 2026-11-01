package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// typeof's tests in the tree tier -- typeof f === 'function', typeof x !==
// 'undefined', typeof v == 'object' -- as one node that asks the value's
// kind and makes no string, as V8's TestTypeOf does. A typeof had been a
// node making its name, typeofString's switch over the value's kind and
// then over the name, and the equality a node comparing that string with
// the constant: three calls, two switches and a string comparison for a
// test of the value's tag.
//
// The test is the one Value.TypeOf answers: an object with
// [[IsHTMLDDA]] (Annex B's document.all) is "undefined", any other
// callable one "function", and null and every other object "object". Its
// name is the constant's: a constant that is no type's name -- typeof x
// === 'strnig' -- is a test that never holds, the value still evaluated.
// ==, != and their strict forms answer alike, the two sides strings.
//
// A typeof that no such test takes is still marked (tentryRare's typeOf,
// its operand), so that a switch on it can test the kind as well (see
// zzz_switch.go).
//
// Its functions are generic, each instantiated with struct{} alone: Go
// lays a generic function's instantiations out after every function and
// closure of the package (see zzz_direct.go), where code written anywhere
// else moves the closures, or the functions after it, against the hot
// ones (AGENTS.md). The tree builder reaches it through opHook, called
// where buildBlock's call of op was, and eqJumpAtHook, which eqJumpAt is,
// the function eqJump asks first: each asks this file first and then
// makes the call it stands for. buildBlock's code grows within the
// padding that rounds it to 32 bytes, op, eqJump and the closure eqJumpAt
// had been are as they were, and every function and closure of the
// package stays at its address (go tool nm).

// opHook is buildBlock's call of op, for the instruction at pc of code: a
// typeof is typeofOp's, a dup that begins a switch's literal cases
// switchStart's, a new_object literalNew's and a define_field its literal
// takes literalField's, anything else op's.
//
//go:noinline
func opHook[_ any](b *tbuilder, pc int, code []bytecode.Instr, more bool) (int, bool) {
	in := code[pc]
	switch in.Op {
	case bytecode.OpTypeOf:
		return typeofOp[struct{}](b, pc, code), true
	case bytecode.OpDup:
		// A switch's literal cases, as one lookup (zzz_switch.go).
		if skip, ok, handled := switchStart[struct{}](b, pc); handled {
			return skip, ok
		}
	case bytecode.OpNewObject:
		// An object literal, as one node with its fields
		// (zzz_literal.go).
		literalNew[struct{}](b, in)
		return 0, true
	case bytecode.OpDefineField:
		if literalField[struct{}](b, pc, in) {
			return 0, true
		}
	}
	return b.op(pc, in, code, more)
}

// eqJumpAtHook is eqJumpAt, eqJump's first try: typeofJump's end, where
// it makes one, or else eqJumpInt32's, the closure eqJumpAt had been
// (ztree_a_int32.go).
func eqJumpAtHook[_ any](op bytecode.Op, x, y tentry, pc, taken, fall int) tnext {
	if next := typeofJump[struct{}](op, x, y, taken, fall); next != nil {
		return next
	}
	return eqJumpInt32(op, x, y, pc, taken, fall)
}

// typeofOf is what a typeof's entry is of: its operand, x, and where a
// string constant is next to it -- pushed after it, or beneath its
// operand -- the constant, k, and the index in typeofNames of the type it
// names, i, -1 for none, which a jump on their equality tests for.
type typeofOf struct {
	x   tentry
	k   uint32
	i   int
	has bool
}

// The types' indices in typeofNames.
const (
	typeUndefined = iota
	typeObject
	typeBoolean
	typeNumber
	typeString
	typeSymbol
	typeBigInt
	typeFunction
)

// typeofIndex is the index of a type's name in typeofNames, or -1 where s
// names none.
func typeofIndex[_ any](s string) int {
	for i, n := range typeofNames {
		if n == s {
			return i
		}
	}
	return -1
}

// typeofKind is the index in typeofNames of typeof v, as Value.TypeOf
// answers it.
func typeofKind[_ any](v Value) int {
	switch v.Kind() {
	case KindNumber:
		return typeNumber
	case KindNull:
		return typeObject
	case KindBool:
		return typeBoolean
	case KindString:
		return typeString
	case KindSymbol:
		return typeSymbol
	case KindBigInt:
		return typeBigInt
	case KindObject:
		o := v.object()
		if o.flags&objHTMLDDA != 0 {
			return typeUndefined
		}
		if o.IsCallable() {
			return typeFunction
		}
		return typeObject
	}
	return typeUndefined
}

// typeofOfStr is r's string of the type of index i in typeofNames, as
// typeofString makes it.
func typeofOfStr[_ any](r *Runtime, i int) *String {
	if s := r.typeofStrs[i]; s != nil {
		return s
	}
	s := NewString(typeofNames[i])
	r.typeofStrs[i] = s
	return s
}

// typeofTest is the test of whether typeof of e is the type of index i in
// typeofNames, -1 for none: true where it is.
func typeofTest[_ any](e tentry, i int) func(c *tctx) bool {
	if i < 0 {
		// No type's name: never, the value evaluated all the same.
		if e.leaf() {
			return func(*tctx) bool { return false }
		}
		v := e.tree()
		return func(c *tctx) bool { v(c); return false }
	}
	if e.local && e.v == nil && e.slot < 0 {
		k := e.k
		switch i {
		case typeNumber:
			return func(c *tctx) bool { return c.locals[k].IsNumber() }
		case typeString:
			return func(c *tctx) bool { return c.locals[k].IsString() }
		case typeUndefined:
			return func(c *tctx) bool {
				l := c.locals[k]
				return l.IsUndefined() || l.IsObject() && l.object().flags&objHTMLDDA != 0 || l.IsUninitialized()
			}
		case typeFunction:
			return func(c *tctx) bool {
				l := c.locals[k]
				return l.IsObject() && l.object().flags&objHTMLDDA == 0 && l.object().IsCallable()
			}
		}
		return func(c *tctx) bool { return typeofKind[struct{}](c.locals[k]) == i }
	}
	v := e.tree()
	switch i {
	case typeNumber:
		return func(c *tctx) bool { return v(c).IsNumber() }
	case typeString:
		return func(c *tctx) bool { return v(c).IsString() }
	case typeUndefined:
		return func(c *tctx) bool {
			l := v(c)
			return l.IsUndefined() || l.IsObject() && l.object().flags&objHTMLDDA != 0 || l.IsUninitialized()
		}
	case typeFunction:
		return func(c *tctx) bool {
			l := v(c)
			return l.IsObject() && l.object().flags&objHTMLDDA == 0 && l.object().IsCallable()
		}
	}
	return func(c *tctx) bool { return typeofKind[struct{}](v(c)) == i }
}

// typeofJump is eqJump's end for op, an equality, of x and y, where one is
// a typeof typeofOp marked and the other a string constant, for the jump to
// taken, or falling to fall; strEqJump's for any other; nil for anything
// else, which eqJump builds.
func typeofJump[_ any](op bytecode.Op, x, y tentry, taken, fall int) tnext {
	if x.rare == strConstRare && x.slot < 0 {
		x, y = y, x
	}
	if x.rare == nil || x.rare.typeOf == nil || x.v == nil || x.slot >= 0 || y.rare != strConstRare || y.slot >= 0 {
		// Any other equality with a string constant (see zzz_streq.go).
		return strEqJump[struct{}](op, x, y, taken, fall)
	}
	t := x.rare.typeOf
	if !t.has || t.k != y.k {
		return strEqJump[struct{}](op, x, y, taken, fall)
	}
	// yes is where a comparison that holds goes, no where one that
	// does not.
	yes, no := fall, taken
	if op == bytecode.OpNe || op == bytecode.OpStrictNe {
		yes, no = taken, fall
	}
	e, i := t.x, t.i
	if e.local && e.v == nil && e.slot < 0 {
		// The commonest, a parameter's or a variable's type, read in
		// place.
		k := e.k
		switch i {
		case typeNumber:
			return func(c *tctx) int {
				if c.locals[k].IsNumber() {
					return yes
				}
				return no
			}
		case typeString:
			return func(c *tctx) int {
				if c.locals[k].IsString() {
					return yes
				}
				return no
			}
		case typeFunction:
			return func(c *tctx) int {
				if l := c.locals[k]; l.IsObject() && l.object().flags&objHTMLDDA == 0 && l.object().IsCallable() {
					return yes
				}
				return no
			}
		}
	}
	if i >= 0 {
		// Any other value, its tree called in the end itself.
		v := e.tree()
		return func(c *tctx) int {
			l := v(c)
			var is bool
			switch i {
			case typeNumber:
				is = l.IsNumber()
			case typeString:
				is = l.IsString()
			case typeFunction:
				is = l.IsObject() && l.object().flags&objHTMLDDA == 0 && l.object().IsCallable()
			case typeUndefined:
				is = l.IsUndefined() || l.IsObject() && l.object().flags&objHTMLDDA != 0 || l.IsUninitialized()
			default:
				is = typeofKind[struct{}](l) == i
			}
			if is {
				return yes
			}
			return no
		}
	}
	test := typeofTest[struct{}](e, i)
	return func(c *tctx) int {
		if test(c) {
			return yes
		}
		return no
	}
}

// typeofOp builds the typeof at pc, reporting how many instructions after
// it it took too: an equality with a string constant after it -- or
// before its operand -- as one node of the test, or else the typeof's
// node, marked with its operand.
func typeofOp[_ any](b *tbuilder, pc int, code []bytecode.Instr) int {
	x := b.pop()
	consts := b.fn.Constants
	// at is the equality's, k the constant's, after the typeof --
	// typeof x === 'number' -- or, where the constant is beneath its
	// operand, pushed first, with nothing to evaluate, at the
	// constant's entry -- 'number' === typeof x.
	at, k := -1, uint32(0)
	if pc+2 < b.end && code[pc+1].Op == bytecode.OpPushConst && uint64(code[pc+1].A) < uint64(len(consts)) &&
		consts[code[pc+1].A].Kind == bytecode.ConstString {
		at, k = pc+2, code[pc+1].A
	} else if n := len(b.stack); pc+1 < b.end && n > 0 && b.stack[n-1].rare == strConstRare && b.stack[n-1].slot < 0 {
		at, k = pc+1, b.stack[n-1].k
	}
	want, ok := false, false
	if at >= 0 {
		switch code[at].Op {
		case bytecode.OpEq, bytecode.OpStrictEq:
			want, ok = true, true
		case bytecode.OpNe, bytecode.OpStrictNe:
			ok = true
		}
	}
	if ok {
		if at == pc+1 {
			b.pop()
		}
		test := typeofTest[struct{}](x, typeofIndex[struct{}](consts[k].Str))
		if want {
			b.push(func(c *tctx) Value { return Bool(test(c)) })
		} else {
			b.push(func(c *tctx) Value { return Bool(!test(c)) })
		}
		return at - pc
	}
	t := &typeofOf{x: x}
	if at >= 0 {
		// The constant a jump compares its answer with, as eqJump
		// builds it (typeofJump).
		t.k, t.i, t.has = k, typeofIndex[struct{}](consts[k].Str), true
	}
	v := x.tree()
	b.pushEntry(tentry{v: func(c *tctx) Value { return Str(c.r.typeofString(v(c))) }, slot: -1,
		rare: &tentryRare{typeOf: t}})
	return 0
}

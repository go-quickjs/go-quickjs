package vm

import (
	"encoding/binary"
	"math"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Element reads and writes at int32 keys, and element reads under a
// bitwise operator, in the int32 trees (see ztree_a_int32.go): HEAP32[p >>
// 2], HEAPU8[q] & 255, F = c[g >> 2] | 0, as code compiled from C reads and
// writes its heap, a typed array. The key had been a Value whose float64
// elemAt made an index again, and an element read that an operator took a
// Value the operator made an int32 again, a typed array's integer element
// made a float64 for it.
//
// A key i32Sure says is an int32 indexes as the int32 it is: an index
// where it is a uint32 or not negative, a property key otherwise, which
// getIndexSlow and setIndexMiss take as the number it is (getIndexAt,
// setIndexAt). An element read a bitwise operator with a constant takes
// straight after it, in the same block and expression, is one entry
// (getIndexAt), an int32 tree (i32Elem) of ToInt32 of the element under
// the operator: a typed array's integer element is the int32 its bytes
// say, a float element ToInt32 of it, and a dense number ToInt32 of that
// (i32Read). Its arithOf says the operator over the read's own tree, as
// the operator would have been built over it, for what makes its own node
// of the two; it keeps the read's object and key in its y, which an
// operator with a constant has no use for. A store of it to a local
// straight after is one statement.
//
// A hole, a getter, an element that is no number, a key that is no index
// and an object that is no array are read as getIndexNode's slow path
// reads them, getIndexSlow, and the operator then made by i32Slow; a write
// that is no number into a typed array's buffer is setIndexStmt's, setElem
// and setIndexMiss. A read an update makes is built as before.
//
// The code is closures assigned in this file's inits, laid out with the
// rest of the int32 trees'.

// i32Site is what an element read under a bitwise operator needs beyond
// the object and the key: the operator, its constant k and kv, k as a
// Value, and the read's and the operator's pcs.
type i32Site struct {
	op       bytecode.Op
	k        int32
	kv       Value
	gpc, bpc int
}

var (
	// getIndexAt is get_index at pc, after the updates placeUpdate makes:
	// a read whose key i32Sure says is an int32, and one a bitwise
	// operator with a constant takes, with a store of that to a local; it
	// reports false for anything else, which tbuilder.op builds.
	getIndexAt func(b *tbuilder, pc int, in bytecode.Instr) (int, bool)
	// setIndexAt is setIndexStmt, with an int32 key's node.
	setIndexAt func(obj, key tentry, val tval, pc int, strict bool) tstmt

	// i32Elem is obj[key] op s.k as an int32 tree.
	i32Elem func(obj, key tentry, s *i32Site) ti32
	// i32GetNode is obj[key] for an int32 key.
	i32GetNode func(obj, key tentry, pc int) tval
	// i32Read is ToInt32(o[i]) op s.k, where idx says i is the key, an
	// index; kv is the key otherwise.
	i32Read func(c *tctx, s *i32Site, o Value, i uint32, idx bool, kv Value) int32
	// i32Get is o[i] at pc, where idx says i is the key, an index; kv is
	// the key otherwise.
	i32Get func(c *tctx, o Value, i uint32, idx bool, kv Value, pc int) Value
	// i32Put is o[i] = v at pc, i being an int32 key, a uint32 where uns is
	// set.
	i32Put func(c *tctx, o Value, i int32, uns bool, v Value, pc int, strict bool)
)

// i32Index is an int32 key as an index, and whether it is one: a uint32's
// always, an int32's below 0 never.
func i32Index(i int32, uns bool) (uint32, bool) {
	return uint32(i), uns || i >= 0
}

// valueIndex is a key as an index, and whether it is one, as elemAt asks:
// what is no number has a NaN's bits, which equal no float64.
func valueIndex(k Value) (uint32, bool) {
	i := uint32(k.num)
	return i, float64(i) == k.num
}

func init() {
	// i32Elem is obj[key] as an int32 tree, ToInt32 of the element under
	// s's operator: the object read in place where it is a local or an
	// upvalue, then the key.
	i32Elem = func(obj, key tentry, s *i32Site) ti32 {
		switch {
		case i32Sure(key):
			p, uns := i32Of(key)
			switch {
			case obj.local:
				o := obj.k
				return func(c *tctx) int32 {
					o := c.locals[o]
					k := p(c)
					i, idx := i32Index(k, uns)
					return i32Read(c, s, o, i, idx, Int32(k))
				}
			case obj.upvalue:
				o := obj.k
				return func(c *tctx) int32 {
					o := c.cl.upvalues[o].get()
					k := p(c)
					i, idx := i32Index(k, uns)
					return i32Read(c, s, o, i, idx, Int32(k))
				}
			}
			t := obj.tree()
			return func(c *tctx) int32 {
				o := t(c)
				k := p(c)
				i, idx := i32Index(k, uns)
				return i32Read(c, s, o, i, idx, Int32(k))
			}
		case key.local:
			k := key.k
			switch {
			case obj.local:
				o := obj.k
				return func(c *tctx) int32 {
					o, k := c.locals[o], c.locals[k]
					i, idx := valueIndex(k)
					return i32Read(c, s, o, i, idx, k)
				}
			case obj.upvalue:
				o := obj.k
				return func(c *tctx) int32 {
					o, k := c.cl.upvalues[o].get(), c.locals[k]
					i, idx := valueIndex(k)
					return i32Read(c, s, o, i, idx, k)
				}
			}
			t := obj.tree()
			return func(c *tctx) int32 {
				o := t(c)
				k := c.locals[k]
				i, idx := valueIndex(k)
				return i32Read(c, s, o, i, idx, k)
			}
		}
		u := key.tree()
		switch {
		case obj.local:
			o := obj.k
			return func(c *tctx) int32 {
				o := c.locals[o]
				k := u(c)
				i, idx := valueIndex(k)
				return i32Read(c, s, o, i, idx, k)
			}
		case obj.upvalue:
			o := obj.k
			return func(c *tctx) int32 {
				o := c.cl.upvalues[o].get()
				k := u(c)
				i, idx := valueIndex(k)
				return i32Read(c, s, o, i, idx, k)
			}
		}
		t := obj.tree()
		return func(c *tctx) int32 {
			o := t(c)
			k := u(c)
			i, idx := valueIndex(k)
			return i32Read(c, s, o, i, idx, k)
		}
	}

}

func init() {
	i32Read = func(c *tctx, s *i32Site, o Value, i uint32, idx bool, kv Value) int32 {
		if idx && o.IsObject() {
			// elemAt's element, an index's: a typed array's first, from the
			// storage its view keeps -- whose indices are its buffer's
			// alone -- then an array's dense one.
			a := o.object()
			if a.class == ClassTypedArray {
				if t, ok := a.dataOf().(*typedArrayData); ok && uint(i) < uint(t.fixedLength) {
					if st := t.st; st != nil && t.end <= len(st.bytes) {
						b, j := st.bytes[t.byteOffset:t.end], int(i)
						var n int32
						switch t.kind {
						case elemInt8:
							n = int32(int8(b[j]))
						case elemUint8, elemUint8Clamped:
							n = int32(b[j])
						case elemInt16:
							n = int32(int16(binary.LittleEndian.Uint16(b[j<<1:])))
						case elemUint16:
							n = int32(binary.LittleEndian.Uint16(b[j<<1:]))
						case elemInt32, elemUint32:
							n = int32(binary.LittleEndian.Uint32(b[j<<2:]))
						case elemFloat32:
							n = toInt32(float64(math.Float32frombits(binary.LittleEndian.Uint32(b[j<<2:]))))
						case elemFloat64:
							n = toInt32(math.Float64frombits(binary.LittleEndian.Uint64(b[j<<3:])))
						default:
							goto slow
						}
						return i32Do(s.op, n, s.k)
					}
				}
			} else if e := a.elemsOf(); uint(i) < uint(len(e)) && a.flags&objMappedArguments == 0 {
				if p := &e[i]; numberAt(p) {
					return i32Do(s.op, toInt32(p.num), s.k)
				}
			}
		}
	slow:
		// A hole, a getter, anything not a number, a key that is no index:
		// the read as getIndexNode's slow path makes it, then the
		// operator.
		if idx {
			kv = Uint32(i)
		}
		v := c.getIndexSlow(o, kv, s.gpc)
		if v.IsNumber() {
			return i32Do(s.op, toInt32(v.num), s.k)
		}
		return i32Slow(c, s.op, v, s.kv, s.bpc)
	}

}

func init() {
	i32Get = func(c *tctx, o Value, i uint32, idx bool, kv Value, pc int) Value {
		if idx && o.IsObject() {
			a := o.object()
			if a.class == ClassTypedArray {
				if t, ok := a.dataOf().(*typedArrayData); ok && uint(i) < uint(t.fixedLength) {
					if st := t.st; st != nil && t.end <= len(st.bytes) {
						b, j := st.bytes[t.byteOffset:t.end], int(i)
						switch t.kind {
						case elemInt8:
							return Int32(int32(int8(b[j])))
						case elemUint8, elemUint8Clamped:
							return Int32(int32(b[j]))
						case elemInt16:
							return Int32(int32(int16(binary.LittleEndian.Uint16(b[j<<1:]))))
						case elemUint16:
							return Int32(int32(binary.LittleEndian.Uint16(b[j<<1:])))
						case elemInt32:
							return Int32(int32(binary.LittleEndian.Uint32(b[j<<2:])))
						case elemUint32:
							return Uint32(binary.LittleEndian.Uint32(b[j<<2:]))
						case elemFloat32:
							return Float(float64(math.Float32frombits(binary.LittleEndian.Uint32(b[j<<2:]))))
						case elemFloat64:
							return Float(math.Float64frombits(binary.LittleEndian.Uint64(b[j<<3:])))
						}
					}
				}
			} else if e := a.elemsOf(); uint(i) < uint(len(e)) && a.flags&objMappedArguments == 0 {
				if p := &e[i]; !holeAt(p) {
					return *p
				}
			}
		}
		if idx {
			kv = Uint32(i)
		}
		return c.getIndexSlow(o, kv, pc)
	}

}

func init() {
	i32Put = func(c *tctx, o Value, k int32, uns bool, v Value, pc int, strict bool) {
		i, idx := i32Index(k, uns)
		if idx && o.IsObject() && v.IsNumber() {
			// A number in a typed array's buffer, as setElem stores it,
			// its integer kinds taking ToInt32's bits.
			if a := o.object(); a.class == ClassTypedArray && len(a.elemsOf()) == 0 {
				if t, ok := a.dataOf().(*typedArrayData); ok && uint(i) < uint(t.fixedLength) {
					if st := t.st; st != nil && t.end <= len(st.bytes) && !st.immutable {
						b, j := st.bytes[t.byteOffset:t.end], int(i)
						switch t.kind {
						case elemInt8, elemUint8:
							b[j] = byte(toInt32(v.num))
							return
						case elemInt16, elemUint16:
							binary.LittleEndian.PutUint16(b[j<<1:], uint16(toInt32(v.num)))
							return
						case elemInt32, elemUint32:
							binary.LittleEndian.PutUint32(b[j<<2:], uint32(toInt32(v.num)))
							return
						case elemFloat32:
							binary.LittleEndian.PutUint32(b[j<<2:], math.Float32bits(float32(v.num)))
							return
						case elemFloat64:
							binary.LittleEndian.PutUint64(b[j<<3:], math.Float64bits(v.num))
							return
						}
					}
				}
			}
		}
		kv := i32Box(k, uns)
		if !setElem(o, kv, v) {
			c.setIndexMiss(o, kv, v, pc, strict)
		}
	}

}

func init() {
	// i32GetNode is obj[key] for an int32 key, as getIndexOperands reads it.
	i32GetNode = func(obj, key tentry, pc int) tval {
		p, uns := i32Of(key)
		switch {
		case obj.local:
			o := obj.k
			return func(c *tctx) Value {
				o := c.locals[o]
				k := p(c)
				i, idx := i32Index(k, uns)
				return i32Get(c, o, i, idx, Int32(k), pc)
			}
		case obj.upvalue:
			o := obj.k
			return func(c *tctx) Value {
				o := c.cl.upvalues[o].get()
				k := p(c)
				i, idx := i32Index(k, uns)
				return i32Get(c, o, i, idx, Int32(k), pc)
			}
		}
		t := obj.tree()
		return func(c *tctx) Value {
			o := t(c)
			k := p(c)
			i, idx := i32Index(k, uns)
			return i32Get(c, o, i, idx, Int32(k), pc)
		}
	}

}

func init() {
	getIndexAt = func(b *tbuilder, pc int, in bytecode.Instr) (int, bool) {
		if skip, ok := b.placeUpdate(pc, in); ok {
			return skip, true
		}
		n, code := len(b.stack), b.fn.Code
		if n < 2 || b.noUpdate {
			// An update's operand is built as before: its loop looks at
			// every instruction.
			return 0, false
		}
		obj, key := b.stack[n-2], b.stack[n-1]
		// within reports whether the instruction at i may be taken in with
		// this one: in its block, and not past where an && or a || whose
		// y this is ends (see buildBlock).
		within := func(i int) bool {
			l := len(b.logic)
			return i < b.end && (l == 0 || b.logic[l-1].at > i)
		}
		next := code[min(pc+1, len(code)-1)]
		fuse := within(pc+1) && next.Op == bytecode.OpBinImm && isBitwise(bytecode.Op(next.B))
		sure := i32Sure(key)
		if !fuse && !sure {
			return 0, false
		}
		b.stack = b.stack[:n-2]
		var plain tval
		if sure {
			plain = i32GetNode(obj, key, pc)
		} else {
			plain = getIndexOperands(obj, key, pc)
		}
		if !fuse {
			b.push(plain)
			return 0, true
		}
		op, k := bytecode.Op(next.B), int32(next.A)
		uns := op == bytecode.OpUShr
		// The read and the operator as one entry: what it is of says the
		// operator over the read's tree, as the operator would have been
		// built over it, and keeps the read's object and key in y.
		e := tentry{slot: -1, arith: &arithOf{op: op, x: tentry{v: plain, slot: -1}, imm: true, k: k, pc: pc + 1,
			y: tentry{slot: -1, arith: &arithOf{op: bytecode.OpGetIndex, x: obj, y: key, pc: pc}}}}
		p, _ := i32Of(e)
		if within(pc + 2) {
			// A store of it to a local, as one statement.
			switch st := code[pc+2]; st.Op {
			case bytecode.OpSetLocal, bytecode.OpSetLocalGet:
				j := st.A
				s := func(c *tctx) { l := &c.locals[j]; *l = Int32(p(c)) }
				if uns {
					s = func(c *tctx) { l := &c.locals[j]; *l = Uint32(uint32(p(c))) }
				}
				if !b.stmt(s) {
					return 0, false
				}
				if st.Op == bytecode.OpSetLocalGet {
					b.pushEntry(localEntry(st.B))
				}
				return 2, true
			}
		}
		e.v = i32Value(p, uns)
		b.pushEntry(e)
		return 1, true
	}

}

func init() {
	setIndexAt = func(obj, key tentry, val tval, pc int, strict bool) tstmt {
		if !i32Sure(key) {
			return setIndexStmt(obj, key, val, pc, strict)
		}
		p, uns := i32Of(key)
		switch {
		case obj.local:
			o := obj.k
			return func(c *tctx) {
				o := c.locals[o]
				k := p(c)
				i32Put(c, o, k, uns, val(c), pc, strict)
			}
		case obj.upvalue:
			o := obj.k
			return func(c *tctx) {
				o := c.cl.upvalues[o].get()
				k := p(c)
				i32Put(c, o, k, uns, val(c), pc, strict)
			}
		}
		t := obj.tree()
		return func(c *tctx) {
			o := t(c)
			k := p(c)
			i32Put(c, o, k, uns, val(c), pc, strict)
		}
	}

}

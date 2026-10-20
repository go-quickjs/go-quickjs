package ssa

import "github.com/go-quickjs/go-quickjs/internal/jit/ir"

// unboxBigPhis gives a phi that only ever carries BigInts held in their
// values an Int64 or Uint64 phi, as unboxPhis gives one of numbers a
// Float64 phi, so that a loop's BigInts stay unboxed, as V8 keeps its
// BigInt64 values in registers: a candidate's arguments are boxed BigInts
// (OpBigBox), BigInt constants held in their values, other candidates, or
// slots loaded at an entry, unboxed there by a guard (a speculation). The
// phis that merge into one another, a group, are of one type: Int64 where
// an argument is an Int64 or a constant below 0, or a use unboxes them as
// one (OpBigUnbox); Uint64 where one is a Uint64 or a constant past the
// int64 range, or a use unboxes them so (OpBigUnboxU64); a group that needs
// both stays as it is. A constant from 0 to 2^63-1, boxed, is either. A group needs a reason, a use that unboxes one of
// its phis. Each old phi becomes the box of the new one, in a keep cell of
// its own, which other passes then cancel where they unbox it.
func unboxBigPhis(f *Func) bool {
	// What unboxes each value, by value ID: 1 as an Int64, 2 as a Uint64,
	// 4 its low bits, either.
	var phis []*Value
	var unboxes map[*Value]uint8
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			switch v.Op {
			case OpPhi:
				if v.Type == Tagged && !trivialPhi(v) {
					phis = append(phis, v)
				}
			case OpBigUnbox, OpBigUnboxU64, OpBigLow:
				if a := v.Args[0]; a.Op == OpPhi {
					if unboxes == nil {
						unboxes = map[*Value]uint8{}
					}
					unboxes[a] |= map[Op]uint8{OpBigUnbox: 1, OpBigUnboxU64: 2, OpBigLow: 4}[v.Op]
				}
			}
		}
	}
	if len(unboxes) == 0 {
		return false
	}
	cand := map[*Value]bool{}
	for _, v := range phis {
		cand[v] = true
	}
	loaded := func(a *Value) bool {
		upvalue := a.Op == OpLoadCell && (a.Args[0].Op == OpUpvalueCell || a.Args[0].Op == OpLocalCell)
		return upvalue && a.State != nil || (a.Op == OpLoadSlot || upvalue) && a.Block.PC < 0 && !a.Block.Generic
	}
	acceptable := func(a *Value) bool {
		if boxedBig(a) != nil || cand[a] || loaded(a) {
			return true
		}
		_, _, ok := bigConst(a)
		return ok
	}
	for changed := true; changed; {
		changed = false
		for _, v := range phis {
			if !cand[v] {
				continue
			}
			for _, a := range v.Args {
				if !acceptable(a) {
					delete(cand, v)
					changed = true
					break
				}
			}
		}
	}
	if len(cand) == 0 {
		return false
	}
	// Groups, by union-find over the candidates' arguments.
	parent := map[*Value]*Value{}
	var find func(v *Value) *Value
	find = func(v *Value) *Value {
		if p, ok := parent[v]; ok && p != v {
			r := find(p)
			parent[v] = r
			return r
		}
		return v
	}
	for _, v := range phis {
		if !cand[v] {
			continue
		}
		for _, a := range v.Args {
			if cand[a] {
				if x, y := find(v), find(a); x != y {
					parent[x] = y
				}
			}
		}
	}
	// Each group's needs: 1 an Int64's, 2 a Uint64's, 4 a reason.
	need := map[*Value]uint8{}
	for _, v := range phis {
		if !cand[v] {
			continue
		}
		g := find(v)
		u := unboxes[v]
		n := u & 3
		if u != 0 {
			n |= 4
		}
		for _, a := range v.Args {
			if x := boxedBig(a); x != nil {
				if x.Op != OpConstI64 || int64(x.Const.Bits) < 0 {
					// A constant from 0 to 2^63-1 is either.
					n |= map[Type]uint8{Int64: 1, Uint64: 2}[x.Type]
				}
			} else if bits, kind, ok := bigConst(a); ok {
				switch {
				case kind == BigConstUint64:
					n |= 2
				case int64(bits) < 0:
					n |= 1
				}
			}
		}
		need[g] |= n
	}
	typeOf := func(v *Value) Type {
		switch n := need[find(v)]; {
		case n&4 == 0, n&3 == 3:
			return None
		case n&2 != 0:
			return Uint64
		}
		return Int64
	}
	// In block order: value numbers, and so register allocation and code,
	// must not vary from one compilation to the next.
	var ordered []*Value
	for _, v := range phis {
		if cand[v] && typeOf(v) != None {
			ordered = append(ordered, v)
		}
	}
	if len(ordered) == 0 || f.Keeps+len(ordered) > f.keepLimit() {
		return false
	}
	newPhi := map[*Value]*Value{}
	for _, v := range ordered {
		newPhi[v] = f.alloc(Value{Op: OpPhi, Type: typeOf(v), Block: v.Block})
	}
	type unboxedKey struct {
		v *Value
		t Type
	}
	unboxed := map[unboxedKey]*Value{}
	for _, v := range ordered {
		p := newPhi[v]
		t := p.Type
		p.Args = f.refsOf(len(v.Args))[:0]
		for _, a := range v.Args {
			var x *Value
			switch {
			case boxedBig(a) != nil:
				if x = boxedBig(a); x.Type != t {
					// A constant from 0 to 2^63-1 of the other type.
					c := f.alloc(Value{Op: OpConstI64, Type: t, Const: x.Const, Block: x.Block})
					insertAfter(f, x, c)
					x = c
				}
			case newPhi[a] != nil:
				x = newPhi[a]
			default:
				if x = unboxed[unboxedKey{a, t}]; x != nil {
					break
				}
				if bits, _, ok := bigConst(a); ok {
					// A constant: its bits, defined beside it.
					x = f.alloc(Value{Op: OpConstI64, Type: t, Const: ir.Value{Bits: bits}, Block: a.Block})
					insertAfter(f, a, x)
				} else {
					op := OpBigUnbox
					if t == Uint64 {
						op = OpBigUnboxU64
					}
					x = f.alloc(Value{Op: op, Type: t, Args: f.refsOf(1), Aux: int(ir.GuardExit), Block: a.Block})
					x.Args[0] = a
					if a.Block.PC >= 0 {
						// A captured binding read after a call: unboxed there.
						x.State = a.State
						insertAfter(f, a, x)
					} else {
						x.State = a.Block.Header
						a.Block.Values = f.appendValue(a.Block.Values, x)
					}
				}
				unboxed[unboxedKey{a, t}] = x
			}
			p.Args = append(p.Args, x)
		}
	}
	// Each block's values become its phis, the new ones in place of those
	// they replace, then the boxes of those -- each old phi the value kept
	// in its box's cell -- then the rest, in order.
	var last *Block
	for _, o := range ordered {
		b := o.Block
		if b == last {
			continue
		}
		last = b
		var front, boxes, rest []*Value
		for _, v := range b.Values {
			p := newPhi[v]
			switch {
			case v.Op != OpPhi:
				rest = append(rest, v)
			case p == nil:
				front = append(front, v)
			default:
				front = append(front, p)
				box := f.alloc(Value{Op: OpBigBox, Type: Tagged, Args: f.refsOf(1), Index: f.Keeps, Block: b})
				box.Args[0] = p
				f.Keeps++
				cell := f.alloc(Value{Op: OpBigCell, Type: Source, Args: f.refsOf(1), Block: b})
				cell.Args[0] = box
				*v = Value{ID: v.ID, Op: OpKept, Type: Tagged, Args: f.refsOf(2), Block: b, Uses: v.Uses, Shadow: cell}
				v.Args[0], v.Args[1] = cell, box
				boxes = append(boxes, box, cell, v)
			}
		}
		b.Values = append(append(append(b.Values[:0:0], front...), boxes...), rest...)
	}
	return true
}

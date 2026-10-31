package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// Element reads and writes of a global's in the tree tier, as code compiled
// from C reads and writes its heap, a typed array held in a global
// (Mandreel's): a read stored to a local, r1 = heap32[(fp+1)], f0 =
// heapFloat[(r1)], r3 = heapU16[(r0+6)>>1], r2 = heap32[r1>>2]; a read
// whose value something else takes, f0 = f1 * heapFloat[(r1+2)]; and a
// write of a local, a constant or such a read, heapFloat[(fp+3)] = f0,
// heap32[(r1+4)>>2] = 0, heap32[(g0)] = heap32[(fp+1)]. A read stored had
// been four nodes -- the store, the read, the global's and the key's -- a
// read three and a write four, the value's too; a read, stored or not, is
// one node here, and a write one and its value's, built where get_global
// begins it (placeUpdate, where it is no update), the global read in place
// where its site's slot holds it, as OpGetGlobal's node reads it, and the
// key and a local value read where they are.
//
// The key is a local, a local and a number constant added, or either under
// a bitwise operator with a constant, which makes it an int32. A sum of a
// local that is no number is the addition's tree's, an int32 key's
// operator's i32Slow and i32SumSlow, as the int32 trees make them. The
// element is read and written as an int32 key's is (i32Get, i32Put) where
// the key is an index below 1<<31, and as getIndexNode and setIndexStmt's
// tree case otherwise. A read a bitwise operator with a constant takes is
// left to getIndexAt, which makes the two one int32 tree.
//
// Its functions are variables, each assigned a closure in init, none of
// which captures a variable of the init's: such a closure is laid out
// among the named functions' closures, ahead of every init's, and would
// move them. The file sorts before every other file of the tree tier with
// an init, so that its closures, laid out in the inits' reverse order, come
// after every closure already there, and placeUpdate, a method, rather
// than getIndexAt or setIndexAt calls it, so that no closure there was
// grows.

// heapKey is the key of a read or write heapAt builds: local l, plus n
// where add is set, under op with constant k where op is not 0.
type heapKey struct {
	l    uint32
	add  bool
	n    float64
	op   bytecode.Op
	k    int32
	apc  int // the addition's pc
	bpc  int // the operator's pc
	next int // the pc after the key
}

var (
	// heapAt is get_global at pc, in, as the start of a read of its
	// element or a write to it, which it builds, reporting how many
	// instructions after pc it took; it reports false for anything else.
	heapAt func(b *tbuilder, pc int, in bytecode.Instr) (int, bool)
	// heapKeyAt is the key that begins at pc, where it is one heapAt
	// takes.
	heapKeyAt func(code []bytecode.Instr, pc int) (heapKey, bool)

	// heapGlobal is get_global in at pc, as OpGetGlobal's node reads it.
	heapGlobal func(in bytecode.Instr, pc int) tval
	// heapRead is locals[j] = global[key], the global read by read at
	// site, and get_index at gpc.
	heapRead func(read tval, name, site uint32, key heapKey, j uint32, gpc int) tstmt
	// heapValue is global[key], get_index at gpc.
	heapValue func(read tval, name, site uint32, key heapKey, gpc int) tval
	// heapWrite is global[key] = val, or locals[v] where val is nil,
	// set_index at spc.
	heapWrite func(read tval, name, site uint32, key heapKey, v uint32, val tval, spc int, strict bool) tstmt
)

func init() {
	heapKeyAt = func(code []bytecode.Instr, pc int) (heapKey, bool) {
		key := heapKey{next: pc + 1, apc: pc, bpc: pc}
		switch k := code[pc]; k.Op {
		case bytecode.OpGetLocal:
			key.l = k.A
		case bytecode.OpLocalBinImm:
			key.l = k.A & (1<<24 - 1)
			switch op := bytecode.Op(k.A >> 24); {
			case isBitwise(op):
				key.op, key.k = op, int32(k.B)
			case op == bytecode.OpAdd:
				key.add, key.n = true, float64(int32(k.B))
				if s := code[pc+1]; s.Op == bytecode.OpBinImm && isBitwise(bytecode.Op(s.B)) {
					key.op, key.k, key.bpc, key.next = bytecode.Op(s.B), int32(s.A), pc+1, pc+2
				}
			default:
				return key, false
			}
		default:
			return key, false
		}
		return key, true
	}

	heapAt = func(b *tbuilder, pc int, in bytecode.Instr) (int, bool) {
		code := b.fn.Code
		// within reports whether the instruction at i may be taken in with
		// this one, as getIndexAt's within does.
		within := func(i int) bool {
			l := len(b.logic)
			return i < b.end && (l == 0 || b.logic[l-1].at > i)
		}
		if !within(pc + 2) {
			return 0, false
		}
		var key heapKey
		v, at := -1, 0
		if k := code[pc+1]; k.Op == bytecode.OpGetLocal2 {
			// The key and the value, which set_index takes next.
			key, v, at = heapKey{l: k.A}, int(k.B), pc+2
		} else {
			var ok bool
			if key, ok = heapKeyAt(code, pc+1); !ok || !within(key.next) {
				return 0, false
			}
			at = key.next
		}
		read := heapGlobal(in, pc)
		var val tval
		if v < 0 {
			switch x := code[at]; x.Op {
			case bytecode.OpGetIndex:
				next := code[min(at+1, len(code)-1)]
				switch {
				case within(at+1) && (next.Op == bytecode.OpSetLocal || next.Op == bytecode.OpSetLocalGet):
					// A read stored to a local.
					if !b.stmt(heapRead(read, in.A, in.B, key, next.A, at)) {
						return 0, false
					}
					if next.Op == bytecode.OpSetLocalGet {
						b.pushEntry(localEntry(next.B))
					}
					return at + 1 - pc, true
				case within(at+1) && next.Op == bytecode.OpBinImm && isBitwise(bytecode.Op(next.B)):
					return 0, false
				}
				b.push(heapValue(read, in.A, in.B, key, at))
				return at - pc, true
			case bytecode.OpGetLocal:
				v, at = int(x.A), at+1
			case bytecode.OpPushInt:
				n := Int32(int32(x.A))
				val, at = func(c *tctx) Value { return n }, at+1
			case bytecode.OpPushConst:
				k := x.A
				val, at = func(c *tctx) Value { return c.consts[k] }, at+1
			case bytecode.OpGetGlobal:
				// A read of a global's element as the value.
				g := at
				if !within(g + 2) {
					return 0, false
				}
				k, ok := heapKeyAt(code, g+1)
				if !ok || !within(k.next) || code[k.next].Op != bytecode.OpGetIndex {
					return 0, false
				}
				val, at = heapValue(heapGlobal(x, g), x.A, x.B, k, k.next), k.next+1
			default:
				return 0, false
			}
		}
		if !within(at) || code[at].Op != bytecode.OpSetIndex {
			return 0, false
		}
		if v < 0 {
			v = 0
		}
		return at - pc, b.stmt(heapWrite(read, in.A, in.B, key, uint32(v), val, at, b.fn.Strict))
	}

	heapGlobal = func(in bytecode.Instr, pc int) tval {
		name, site := in.A, in.B
		return func(c *tctx) Value {
			s := &c.ic[site]
			if env := s.p1; env != nil && c.f.evalVars == nil {
				if r := c.r; len(r.globalLex.vals) == 0 || !r.lexShadows(c.names[name]) {
					if i := uint(s.idx); i < uint(len(env.vals)) {
						sh := env.shape
						if sh == s.shape {
							return env.vals[i]
						}
						if p := sh.keys[i]; p.flags&(propAccessor|propPrivate|propDeleted|propUninit) == 0 {
							noteGlobalShape(s, sh)
							return env.vals[i]
						}
					}
				}
			}
			v, err := c.r.getGlobalAt(c, in, pc)
			if err != nil {
				c.throw(err)
			}
			return v
		}
	}

	heapRead = func(read tval, name, site uint32, key heapKey, j uint32, gpc int) tstmt {
		l, n, op, k := key.l, key.n, key.op, key.k
		uns := op == bytecode.OpUShr
		switch {
		case op == 0 && !key.add:
			return func(c *tctx) {
				var o Value
				if s := &c.ic[site]; s.p1 != nil && c.f.evalVars == nil && uint(s.idx) < uint(len(s.p1.vals)) && s.p1.shape == s.shape &&
					(len(c.r.globalLex.vals) == 0 || !c.r.lexShadows(c.names[name])) {
					o = s.p1.vals[s.idx]
				} else {
					o = read(c)
				}
				kv := c.locals[l]
				i, idx := valueIndex(kv)
				c.locals[j] = i32Get(c, o, i, idx, kv, gpc)
			}
		case op == 0:
			add := arithEntries(bytecode.OpAdd, localEntry(l), numberEntry(Int32(int32(n))), key.apc)
			return func(c *tctx) {
				var o Value
				if s := &c.ic[site]; s.p1 != nil && c.f.evalVars == nil && uint(s.idx) < uint(len(s.p1.vals)) && s.p1.shape == s.shape &&
					(len(c.r.globalLex.vals) == 0 || !c.r.lexShadows(c.names[name])) {
					o = s.p1.vals[s.idx]
				} else {
					o = read(c)
				}
				var kv Value
				if p := &c.locals[l]; numberAt(p) {
					kv = Float(p.num + n)
				} else {
					kv = add(c)
				}
				i, idx := valueIndex(kv)
				c.locals[j] = i32Get(c, o, i, idx, kv, gpc)
			}
		case !key.add:
			kv, bpc := Int32(k), key.bpc
			return func(c *tctx) {
				var o Value
				if s := &c.ic[site]; s.p1 != nil && c.f.evalVars == nil && uint(s.idx) < uint(len(s.p1.vals)) && s.p1.shape == s.shape &&
					(len(c.r.globalLex.vals) == 0 || !c.r.lexShadows(c.names[name])) {
					o = s.p1.vals[s.idx]
				} else {
					o = read(c)
				}
				var m int32
				if a := c.locals[l]; float64(int32(a.num)) == a.num {
					m = i32Do(op, int32(a.num), k)
				} else {
					m = i32Slow(c, op, a, kv, bpc)
				}
				i, idx := i32Index(m, uns)
				c.locals[j] = i32Get(c, o, i, idx, Int32(m), gpc)
			}
		}
		nv := Int32(int32(n))
		ss := &i32SumSite{aop: bytecode.OpAdd, apc: key.apc, op: op, k: k, kv: Int32(k), bpc: key.bpc}
		return func(c *tctx) {
			var o Value
			if s := &c.ic[site]; s.p1 != nil && c.f.evalVars == nil && uint(s.idx) < uint(len(s.p1.vals)) && s.p1.shape == s.shape &&
				(len(c.r.globalLex.vals) == 0 || !c.r.lexShadows(c.names[name])) {
				o = s.p1.vals[s.idx]
			} else {
				o = read(c)
			}
			a, m := c.locals[l], int32(0)
			if e, ok := i32Exact(a.num + n); ok {
				m = i32Do(op, e, k)
			} else {
				m = i32SumSlow(c, ss, a, nv)
			}
			i, idx := i32Index(m, uns)
			c.locals[j] = i32Get(c, o, i, idx, Int32(m), gpc)
		}
	}

	heapValue = func(read tval, name, site uint32, key heapKey, gpc int) tval {
		l, n, op, k := key.l, key.n, key.op, key.k
		uns := op == bytecode.OpUShr
		switch {
		case op == 0 && !key.add:
			return func(c *tctx) Value {
				var o Value
				if s := &c.ic[site]; s.p1 != nil && c.f.evalVars == nil && uint(s.idx) < uint(len(s.p1.vals)) && s.p1.shape == s.shape &&
					(len(c.r.globalLex.vals) == 0 || !c.r.lexShadows(c.names[name])) {
					o = s.p1.vals[s.idx]
				} else {
					o = read(c)
				}
				kv := c.locals[l]
				i, idx := valueIndex(kv)
				return i32Get(c, o, i, idx, kv, gpc)
			}
		case op == 0:
			add := arithEntries(bytecode.OpAdd, localEntry(l), numberEntry(Int32(int32(n))), key.apc)
			return func(c *tctx) Value {
				var o Value
				if s := &c.ic[site]; s.p1 != nil && c.f.evalVars == nil && uint(s.idx) < uint(len(s.p1.vals)) && s.p1.shape == s.shape &&
					(len(c.r.globalLex.vals) == 0 || !c.r.lexShadows(c.names[name])) {
					o = s.p1.vals[s.idx]
				} else {
					o = read(c)
				}
				var kv Value
				if p := &c.locals[l]; numberAt(p) {
					kv = Float(p.num + n)
				} else {
					kv = add(c)
				}
				i, idx := valueIndex(kv)
				return i32Get(c, o, i, idx, kv, gpc)
			}
		case !key.add:
			kv, bpc := Int32(k), key.bpc
			return func(c *tctx) Value {
				var o Value
				if s := &c.ic[site]; s.p1 != nil && c.f.evalVars == nil && uint(s.idx) < uint(len(s.p1.vals)) && s.p1.shape == s.shape &&
					(len(c.r.globalLex.vals) == 0 || !c.r.lexShadows(c.names[name])) {
					o = s.p1.vals[s.idx]
				} else {
					o = read(c)
				}
				var m int32
				if a := c.locals[l]; float64(int32(a.num)) == a.num {
					m = i32Do(op, int32(a.num), k)
				} else {
					m = i32Slow(c, op, a, kv, bpc)
				}
				i, idx := i32Index(m, uns)
				return i32Get(c, o, i, idx, Int32(m), gpc)
			}
		}
		nv := Int32(int32(n))
		ss := &i32SumSite{aop: bytecode.OpAdd, apc: key.apc, op: op, k: k, kv: Int32(k), bpc: key.bpc}
		return func(c *tctx) Value {
			var o Value
			if s := &c.ic[site]; s.p1 != nil && c.f.evalVars == nil && uint(s.idx) < uint(len(s.p1.vals)) && s.p1.shape == s.shape &&
				(len(c.r.globalLex.vals) == 0 || !c.r.lexShadows(c.names[name])) {
				o = s.p1.vals[s.idx]
			} else {
				o = read(c)
			}
			a, m := c.locals[l], int32(0)
			if e, ok := i32Exact(a.num + n); ok {
				m = i32Do(op, e, k)
			} else {
				m = i32SumSlow(c, ss, a, nv)
			}
			i, idx := i32Index(m, uns)
			return i32Get(c, o, i, idx, Int32(m), gpc)
		}
	}

	heapWrite = func(read tval, name, site uint32, key heapKey, v uint32, val tval, spc int, strict bool) tstmt {
		l, n, op, k := key.l, key.n, key.op, key.k
		uns := op == bytecode.OpUShr
		switch {
		case op == 0 && !key.add:
			return func(c *tctx) {
				var o Value
				if s := &c.ic[site]; s.p1 != nil && c.f.evalVars == nil && uint(s.idx) < uint(len(s.p1.vals)) && s.p1.shape == s.shape &&
					(len(c.r.globalLex.vals) == 0 || !c.r.lexShadows(c.names[name])) {
					o = s.p1.vals[s.idx]
				} else {
					o = read(c)
				}
				kv := c.locals[l]
				var x Value
				if val == nil {
					x = c.locals[v]
				} else {
					x = val(c)
				}
				if i, idx := valueIndex(kv); idx && i < 1<<31 {
					i32Put(c, o, int32(i), false, x, spc, strict)
				} else if !setElem(o, kv, x) && !typedStore(o, kv.num, x) {
					c.setIndexSlow(o, kv, x, spc, strict)
				}
			}
		case op == 0:
			add := arithEntries(bytecode.OpAdd, localEntry(l), numberEntry(Int32(int32(n))), key.apc)
			return func(c *tctx) {
				var o Value
				if s := &c.ic[site]; s.p1 != nil && c.f.evalVars == nil && uint(s.idx) < uint(len(s.p1.vals)) && s.p1.shape == s.shape &&
					(len(c.r.globalLex.vals) == 0 || !c.r.lexShadows(c.names[name])) {
					o = s.p1.vals[s.idx]
				} else {
					o = read(c)
				}
				var kv Value
				if p := &c.locals[l]; numberAt(p) {
					kv = Float(p.num + n)
				} else {
					kv = add(c)
				}
				var x Value
				if val == nil {
					x = c.locals[v]
				} else {
					x = val(c)
				}
				if i, idx := valueIndex(kv); idx && i < 1<<31 {
					i32Put(c, o, int32(i), false, x, spc, strict)
				} else if !setElem(o, kv, x) && !typedStore(o, kv.num, x) {
					c.setIndexSlow(o, kv, x, spc, strict)
				}
			}
		case !key.add:
			kv, bpc := Int32(k), key.bpc
			return func(c *tctx) {
				var o Value
				if s := &c.ic[site]; s.p1 != nil && c.f.evalVars == nil && uint(s.idx) < uint(len(s.p1.vals)) && s.p1.shape == s.shape &&
					(len(c.r.globalLex.vals) == 0 || !c.r.lexShadows(c.names[name])) {
					o = s.p1.vals[s.idx]
				} else {
					o = read(c)
				}
				var m int32
				if a := c.locals[l]; float64(int32(a.num)) == a.num {
					m = i32Do(op, int32(a.num), k)
				} else {
					m = i32Slow(c, op, a, kv, bpc)
				}
				var x Value
				if val == nil {
					x = c.locals[v]
				} else {
					x = val(c)
				}
				i32Put(c, o, m, uns, x, spc, strict)
			}
		}
		nv := Int32(int32(n))
		ss := &i32SumSite{aop: bytecode.OpAdd, apc: key.apc, op: op, k: k, kv: Int32(k), bpc: key.bpc}
		return func(c *tctx) {
			var o Value
			if s := &c.ic[site]; s.p1 != nil && c.f.evalVars == nil && uint(s.idx) < uint(len(s.p1.vals)) && s.p1.shape == s.shape &&
				(len(c.r.globalLex.vals) == 0 || !c.r.lexShadows(c.names[name])) {
				o = s.p1.vals[s.idx]
			} else {
				o = read(c)
			}
			a, m := c.locals[l], int32(0)
			if e, ok := i32Exact(a.num + n); ok {
				m = i32Do(op, e, k)
			} else {
				m = i32SumSlow(c, ss, a, nv)
			}
			var x Value
			if val == nil {
				x = c.locals[v]
			} else {
				x = val(c)
			}
			i32Put(c, o, m, uns, x, spc, strict)
		}
	}
}

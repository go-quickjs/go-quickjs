package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// A call s.charCodeAt(i) as a node of its own: where the receiver is a
// string, the method String.prototype.charCodeAt itself (its elemOp) and
// the argument a number or undefined, the code unit is read here; anything
// else is the call methodCallN's node would make. The scanner of
// Typescript's compiler reads each character so (peekChar), and the
// checksum Octane's harness keeps of its output reads each character it
// writes: a call through methodRest, callRest's tests and leafChecks to the
// built-in, which then converts its argument and its this again.
//
// Package variables assigned closures, as ztree_a_aheap.go's are.

var (
	// charCodeAtCall builds recv.charCodeAt(arg), the method read by get
	// at gpc and the call at pc, its receiver's slot p.
	charCodeAtCall func(recv tentry, get bytecode.Instr, gpc int, arg tval, p, pc int) tval
	// charCodeAtRest is charCodeAtCall's call of m, the method read, on o
	// with a, which is not one it reads itself.
	charCodeAtRest func(c *tctx, m, o, a Value, p, pc int) Value
)

func init() {
	charCodeAtCall = func(recv tentry, get bytecode.Instr, gpc int, arg tval, p, pc int) tval {
		x := recv.tree()
		name, site := get.A, get.B
		return func(c *tctx) Value {
			o := x(c)
			var m Value
			ok := false
			key := c.names[name]
			if o.IsObject() {
				m, ok = c.r.cachedProp(&c.ic[site], o.object(), key)
			} else {
				m, ok = primMethod(c.r, o, key)
			}
			if !ok {
				m = methodMiss(c, o, key, gpc)
			}
			a := arg(c)
			if o.IsString() && m.IsObject() {
				if fd := m.object().fn(); fd != nil && fd.elemOp == elemStrCharCodeAt {
					f := 0.0
					if a.IsNumber() {
						if f = a.Number(); f != f {
							f = 0
						}
					} else if !a.IsUndefined() {
						return charCodeAtRest(c, m, o, a, p, pc)
					}
					// ToIntegerOrInfinity truncates toward zero, so any
					// position above -1 and below the length is a code
					// unit's.
					if s := o.String(); f > -1 && f < float64(s.Len()) {
						return Int(s.CharCodeAt(int(f)))
					}
					return Float(nan())
				}
			}
			return charCodeAtRest(c, m, o, a, p, pc)
		}
	}

	charCodeAtRest = func(c *tctx, m, o, a Value, p, pc int) Value {
		c.stack[p+1] = a
		as := c.stack[p+1 : p+2]
		c.at(pc)
		var f *Object
		var fd *funcData
		if m.IsObject() {
			f = m.object()
			fd = f.fn()
			if fd != nil && fd.treeCall != nil && fd.closure.realm == c.r.Realm {
				r := c.r
				if err := r.tick(); err != nil {
					c.throw(err)
				}
				v, err := r.callTree(f, fd, o, as, true)
				if err != nil {
					c.throw(err)
				}
				return v
			}
		}
		return methodRest(c, m, f, fd, o, as)
	}
}

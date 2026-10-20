package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// BigInt.asUintN(64, x op y) and BigInt.asIntN's kind, made of x and y: a
// 64-bit hash's or generator's arithmetic, wrapped to its width at every
// step. Where the method is asUintN or asIntN, the width one of 64 bits or
// fewer, and x and y BigInts held in their Values, the result is the low
// bits of x's and y's two's complement's low 64 bits under op -- which the
// sum, the difference, the product, the bitwise operators and a left shift
// of fewer than 64 bits keep, and a right shift of fewer than 64 bits takes
// from the sign as well -- without x op y, which past 64 bits had been a
// BigInt on the heap at every step, made for the call to cut down again.
// V8 makes BigInt.asUintN(64, ...) of its operators so, on machine words.
//
// Anything else is the operator's and the call's as callOperands makes
// them, in the same order: the method, the width, x, y, the operator, the
// call.
//
// The node is a closure assigned in this file's init, whose closures are
// laid out after every other closure of the package's (see
// ztree_zkself.go): this file's init comes first.

// wrapCall is m(w, x op y) for a call_method at pc of the method f, over
// the receiver in slot p-1, with the operator at opc: BigInt.asUintN's and
// asIntN's of x op y made of x and y where they may be (wrapShort).
var wrapCall func(f, w, x, y tval, op bytecode.Op, opc, p, pc int) tval

func init() {
	wrapCall = func(f, w, x, y tval, op bytecode.Op, opc, p, pc int) tval {
		return func(c *tctx) Value {
			fv := f(c)
			wv := w(c)
			c.stack[p+1] = wv
			a := x(c)
			b := y(c)
			if a.isShortBig() && b.isShortBig() && fv.IsObject() {
				if fd := fv.object().fn(); fd != nil && (fd.mathOp == mathAsUintN || fd.mathOp == mathAsIntN) {
					if v, ok := wrapShort(fd.mathOp == mathAsIntN, wv, op, a, b); ok {
						if jitBuilt {
							wrapSeen(c, op, a, b, opc)
						}
						return v
					}
				}
			}
			c.stack[p+2] = c.wrapOperator(op, a, b, opc)
			c.at(pc)
			recv, as := c.stack[p-1], c.stack[p+1:p+3]
			if fv.IsObject() {
				o := fv.object()
				fd := o.fn()
				if fd != nil && fd.treeCall != nil && fd.closure.realm == c.r.Realm {
					r := c.r
					if err := r.tick(); err != nil {
						c.throw(err)
					}
					v, err := r.callTree(o, fd, recv, as, true)
					if err != nil {
						c.throw(err)
					}
					return v
				}
				v, err := c.r.callRest(o, fd, recv, as)
				if err != nil {
					c.throw(err)
				}
				return v
			}
			v, err := c.r.call(fv, recv, as)
			if err != nil {
				c.throw(err)
			}
			return v
		}
	}
}

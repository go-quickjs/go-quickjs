package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// s += x on a captured binding, in the tree tier, where s is a string --
// a closure appending to a string its maker holds, a sink's push, a
// template's emit -- as one node that joins the two where both are
// strings. The update had been the number node's (upvalueValueUpdate,
// upvalueUpdateStmt) and, for anything but numbers, rmwRun's: rmwAt read
// the binding, rmwOp, arithSlow, arithOther and add each asked again what
// the operands were, and add joined them (Runtime.concat), five calls
// before the string was made -- a fifth of a call of
// { push(x) { return x !== null && (s += x), true } }.
//
// The node reads the binding, then the operand -- a local or a constant in
// place, or its tree -- as rmwAt does, and where both are numbers adds
// them as the number node did; where both are strings it joins them as
// add does, after the JIT's note that the addition met a string
// (treeStringAdd), at the operator's position (a string past the longest
// throws); anything else is rmwOp's, with the values it read. The result
// is stored to the binding as rmwSet stores it. Only an addition that is
// neither ++ nor a postfix's is taken.
//
// Its code is generic, instantiated with struct{}, as zzz_zliteral.go's;
// the tree builder's update reaches it through rmwNodeHook and
// rmwStmtHook, which stand where its calls of rmwNode and rmwStmt were.

// rmwNodeHook is rmwNode, but for an addition to an upvalue.
func rmwNodeHook[_ any](u *rmw) tval {
	if upvalueAddOf[struct{}](u) {
		return (*upvalueAdd[struct{}])(u).node
	}
	return rmwNode(u)
}

// rmwStmtHook is rmwStmt, but for an addition to an upvalue.
func rmwStmtHook[_ any](u *rmw) tstmt {
	if upvalueAddOf[struct{}](u) {
		return (*upvalueAdd[struct{}])(u).stmt
	}
	return rmwStmt(u)
}

// upvalueAddOf reports whether u is v += y, v an upvalue, which
// upvalueAdd makes.
func upvalueAddOf[_ any](u *rmw) bool {
	return u.kind == placeUpvalue && u.op == bytecode.OpAdd && !u.step && !u.postfix
}

// upvalueAdd is an rmw that upvalueAddOf takes, with the node's methods.
type upvalueAdd[_ any] rmw

// node is the node as a value: the binding's new value.
func (a *upvalueAdd[_]) node(c *tctx) Value {
	u := (*rmw)(a)
	old := *c.cl.upvalues[u.upv].slot
	var y Value
	switch {
	case u.yLocal >= 0:
		y = c.locals[u.yLocal]
	default:
		y = c.ref(&u.y)
	}
	var r Value
	switch {
	case old.IsNumber() && y.IsNumber():
		r = Float(old.num + y.num)
	case old.IsString() && y.IsString():
		c.at(u.opPC)
		if jitBuilt {
			treeStringAdd(c, u.opPC)
		}
		var s *String
		var err error
		if u.setIn.B != 0 {
			s, err = strAppend[struct{}](c.r, old.String(), y.String())
		} else {
			s, err = c.r.concat(old.String(), y.String())
		}
		if err != nil {
			c.throw(err)
		}
		r = Str(s)
	default:
		r = c.rmwOp(u, old, y)
	}
	*c.cl.upvalues[u.upv].slot = r
	return r
}

// stmt is the node as a statement.
func (a *upvalueAdd[_]) stmt(c *tctx) { a.node(c) }

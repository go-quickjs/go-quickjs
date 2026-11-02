package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// A call of RegExp.prototype.exec whose result nothing reads -- a
// statement of its own, re.exec(s);, or a test of whether it matched,
// if (re.exec(s)), while (re.exec(s)), !re.exec(s), re.exec(s) && x --
// makes the match, the legacy statics (RegExp.$1, lastMatch...) and
// lastIndex as any other, but no array and no captures' strings, as V8's
// optimized code drops a call's result it finds unused: an object stands
// for the array, the same each time (regexpLeaf's dummy), which is
// truthy as the array is, and of the same kind for whatever native code
// has guessed of the call's result. The V8 suite's RegExp makes 382 of
// its 466 calls of exec so, an array and its strings each time.
//
// Whether a call's result is read the bytecode says: the instruction
// after the call takes it (deadResultAt) -- a drop, a jump on its truth,
// && 's jump_if_false_keep, which keeps only a falsy value, null, or a
// not. The call is made as it was, through callRest, by the same path:
// the leaf's check, the leaf's call without a frame, the frame and the
// tick where it is refused or errs, errors and all. It only tells exec's
// leaf, just before, that the call it is about to make is of a result
// nothing reads, of that receiver (regexpLeaf's dead), which the leaf
// takes as it begins, before anything it runs could call exec again.
// Where a memory meter counts what is made, or a debugger may run the
// script's code at the call's tick, the call is told nothing.
//
// The tree tier builds such a call into a node of its own (deadExecCall)
// wherever the method read is exec's, on a receiver of any kind: a local
// or this read in place (a method entry), a property of either, a
// RegExp literal (regexpLitCall's), and anything in its slot -- an
// upvalue, a global -- whose get_prop_this deadExecGet marks
// (execDeadRare). Any other call is the builder's as before. Native code
// tells the leaf through its call's description (ssa.CallSite's Unused,
// zzjit_regexpdead.go).
//
// The functions are generic, each instantiated with struct{} alone: Go
// lays a generic function's instantiations out after every function and
// closure of the package (see zzz_direct.go). The tree builder reaches
// them through opHook (zzz_typeof.go).

// execDeadRare is the rare of a get_prop_this of exec whose receiver is in
// its slot, beneath it (deadExecGet), which the call of it builds into
// deadExecNode's node where nothing reads its result.
var execDeadRare = &tentryRare{}

// deadResultAt reports whether nothing reads the value the instruction at
// pc of code leaves but, at most, whether it is truthy: the next one drops
// it, jumps on its truth -- && 's keeps it only where it is falsy -- or is
// a not.
func deadResultAt[_ any](code []bytecode.Instr, pc int) bool {
	if pc+1 >= len(code) {
		return false
	}
	switch code[pc+1].Op {
	case bytecode.OpDrop, bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue, bytecode.OpJumpIfFalseKeep, bytecode.OpNot:
		return true
	}
	return false
}

// deadExecGet builds the get_prop_this at pc where it reads exec, as op
// builds it, and marks the method's entry (execDeadRare) where its
// receiver is in its slot: handled reports whether it was exec's.
func deadExecGet[_ any](b *tbuilder, pc int, code []bytecode.Instr, more bool) (skip int, ok, handled bool) {
	in, fn := code[pc], b.fn
	if fn.Generator || fn.Async || int(in.A) >= len(fn.Names) || fn.Names[in.A] != "exec" {
		return 0, false, false
	}
	skip, ok = b.op(pc, in, code, more)
	if n := len(b.stack); ok && skip == 0 && n > 1 {
		if e := &b.stack[n-1]; e.method == nil && e.rare == nil && e.v != nil && e.slot < 0 && int(b.stack[n-2].slot) == n-2 {
			e.rare = execDeadRare
		}
	}
	return skip, ok, true
}

// deadExecCall builds the call_method at pc, of exec's read, into
// deadExecNode's node, or a RegExp literal's into deadLitExecNode's,
// where nothing reads its result (deadResultAt), and reports whether it
// did.
func deadExecCall[_ any](b *tbuilder, pc int, code []bytecode.Instr) bool {
	fn := b.fn
	if fn.Generator || fn.Async || !deadResultAt[struct{}](code, pc) {
		return false
	}
	n := int(code[pc].A)
	k := len(b.stack) - n - 1
	if k < 1 {
		return false
	}
	m, recv := b.stack[k], b.stack[k-1]
	// The receiver's tree, or nil for one in its slot.
	var rv tval
	lit := false
	switch {
	case m.method != nil:
		if int(m.method.in.A) >= len(fn.Names) || fn.Names[m.method.in.A] != "exec" || m.v == nil {
			return false
		}
		if (recv.local || recv.this) && recv.v == nil && recv.slot < 0 {
			rv = recv.tree()
		} else if propIn(recv) != nil {
			// Its tree stores it in its slot too, which the method's
			// read reads (getPropThisNode).
			rv = recv.v
		} else {
			return false
		}
	case m.rare == execDeadRare && m.v != nil && m.slot < 0 && int(recv.slot) == k-1:
	case m.rare != nil && m.rare.regexpCall != nil && !m.rare.regexpCall.test && int(m.slot) != k && int(recv.slot) == k-1:
		lit = true
	default:
		return false
	}
	args := make([]tval, n)
	for i := n - 1; i >= 0; i-- {
		args[i] = b.pop().tree()
	}
	f := b.pop().tree()
	b.pop()
	if lit {
		b.push(deadLitExecNode[struct{}](f, m.rare.regexpCall.k, args, k, pc))
	} else {
		b.push(deadExecNode[struct{}](rv, f, args, k, pc))
	}
	return true
}

// deadExec is a node of a call of exec whose result nothing reads, of the
// method f reads, with the arguments to evaluate into slots p+1..., at pc.
// Its methods are what would be closures: a generic function's closures
// are laid out ahead of every generic instantiation there was, the tree
// tier's among them. node is deadExecNode's, of the receiver rv evaluates
// or, rv nil, the one in slot p-1; lit deadLitExecNode's, of a RegExp
// literal's, of constant k, in slot p-1.
type deadExec[_ any] struct {
	rv, f tval
	args  []tval
	p, pc int
	k     uint32
}

// deadExecNode is the call at pc of the method f reads, of the receiver
// rv evaluates -- or, rv nil, the one in slot p-1 -- with the arguments to
// evaluate into slots p+1..., whose result nothing reads: the realm's exec
// is told so (deadExecRest), and anything else called as callOperands's
// node calls a method.
func deadExecNode[_ any](rv, f tval, args []tval, p, pc int) tval {
	return (&deadExec[struct{}]{rv: rv, f: f, args: args, p: p, pc: pc}).node
}

// deadLitExecNode is regexpLitCallNode's node of a RegExp literal's exec,
// of constant k, whose result nothing reads (deadExecRest).
func deadLitExecNode[_ any](f tval, k uint32, args []tval, p, pc int) tval {
	return (&deadExec[struct{}]{f: f, args: args, p: p, pc: pc, k: k}).lit
}

// node is deadExecNode's node.
func (d *deadExec[_]) node(c *tctx) Value {
	var recv Value
	if d.rv != nil {
		recv = d.rv(c)
	}
	fv := d.f(c)
	p := d.p
	for i, a := range d.args {
		c.stack[p+1+i] = a(c)
	}
	c.at(d.pc)
	if d.rv == nil {
		recv = c.stack[p-1]
	}
	as := c.stack[p+1 : p+1+len(d.args)]
	if fv.IsObject() {
		if o := fv.Object(); o == c.r.regexpExecFn {
			v, err := deadExecRest[struct{}](c.r, o, o.fn(), recv, as)
			if err != nil {
				c.throw(err)
			}
			return v
		}
	}
	return regexpCallMethod(c, fv, recv, as)
}

// lit is deadLitExecNode's node.
func (d *deadExec[_]) lit(c *tctx) Value {
	r := c.r
	builtin := r.regexpLitMethod(false)
	var fv Value
	if builtin != nil {
		fv = Obj(builtin)
	} else {
		fv = d.f(c)
	}
	p, n := d.p, len(d.args)
	for i, a := range d.args {
		c.stack[p+1+i] = a(c)
	}
	c.at(d.pc)
	recv, as := c.stack[p-1], c.stack[p+1:p+1+n]
	done := builtin != nil && (n == 0 || !as[0].IsObject())
	if fv.IsObject() {
		o := fv.Object()
		if fd := o.fn(); fd == nil || fd.treeCall == nil || fd.closure.realm != r.Realm {
			v, err := deadExecRest[struct{}](r, o, fd, recv, as)
			if err != nil {
				c.throw(err)
			}
			if done && recv.IsObject() {
				r.regexpLiteralDone(&c.cl.fn.Constants[d.k], recv.Object())
			}
			return v
		}
	}
	// A function of the tree tier's, or no object: as regexpLitCallNode
	// calls it.
	return regexpCallMethod(c, fv, recv, as)
}

// deadExecRest is callRest's call of o, fd its function's data, on recv
// with args, where nothing reads the result: where o is the realm's exec,
// its leaf is told so first, of recv (regexpLeaf's dead), and untold after.
func deadExecRest[_ any](r *Runtime, o *Object, fd *funcData, recv Value, args []Value) (Value, error) {
	dead := r.regexpExecDead
	if o != r.regexpExecFn || dead == nil || !recv.IsObject() || r.meter != nil || r.debug != nil {
		return r.callRest(o, fd, recv, args)
	}
	*dead = recv.Object()
	v, err := r.callRest(o, fd, recv, args)
	*dead = nil
	return v, err
}

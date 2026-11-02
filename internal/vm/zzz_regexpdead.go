package vm

import (
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/regexp"
)

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
// So does a string's replace of a RegExp by a string, s.replace(re, ''),
// whose result nothing reads -- 614 of the suite's 712 replaces: where it
// is the built-in's direct replace (replaceDirect), which runs nothing of
// the script's, it makes the matches, the statics and lastIndex as it
// would, and not the string they make (regexpReplaceDead), the subject
// standing for it. A replacement that could make a string too long to be
// one, which throws, is replaced as before; so is any other replace -- a
// function's, which is called as it was.
//
// Whether a call's result is read the bytecode says: the instruction
// after the call takes it (deadResultAt) -- a drop, a jump on its truth,
// && 's jump_if_false_keep, which keeps only a falsy value, null, or a
// not. The call is made as it was, through callRest, by the same path:
// the leaf's check, the leaf's call without a frame, the frame and the
// tick where it is refused or errs, errors and all. It only tells the
// function, just before, that the call it is about to make is of a
// result nothing reads, of that RegExp -- exec's receiver, replace's
// first argument (regexpLeaf's dead, replaceDead's) -- which the function
// takes as it begins, before anything it runs could call it again. Where
// a memory meter counts what is made, or a debugger may run the script's
// code at the call's tick, the call is told nothing.
//
// The tree tier builds such a call into a node of its own (deadExecCall)
// wherever the method read is exec's or replace's, on a receiver of any
// kind: a local or this read in place (a method entry), a property of
// either, a RegExp literal (regexpLitCall's), and anything in its slot --
// an upvalue, a global, an element -- whose get_prop_this deadExecGet
// marks (execDeadRare, replaceDeadRare); replace given a RegExp literal
// gives the literal its RegExp back as regexpArgCall's node does, but for
// a method entry's, which stays regexpArgMethodCall's. Any other call is
// the builder's as before. Native code tells the functions through its
// leaf's call's description (ssa.CallSite's Unused) or the bytecode after
// its call of Go (zzjit_regexpdead.go).
//
// The functions are generic, each instantiated with struct{} alone: Go
// lays a generic function's instantiations out after every function and
// closure of the package (see zzz_direct.go). The tree builder reaches
// them through opHook (zzz_typeof.go).

// execDeadRare and replaceDeadRare are the rare of a get_prop_this of exec
// or of replace whose receiver is in its slot, beneath it (deadExecGet),
// which the call of it builds into deadExecNode's node where nothing reads
// its result.
var (
	execDeadRare    = &tentryRare{}
	replaceDeadRare = &tentryRare{}
)

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

// deadExecGet builds the get_prop_this at pc where it reads exec or
// replace, as op builds it, and marks the method's entry (execDeadRare,
// replaceDeadRare) where its receiver is in its slot: handled reports
// whether it was either's.
func deadExecGet[_ any](b *tbuilder, pc int, code []bytecode.Instr, more bool) (skip int, ok, handled bool) {
	in, fn := code[pc], b.fn
	if fn.Generator || fn.Async || int(in.A) >= len(fn.Names) {
		return 0, false, false
	}
	rare := execDeadRare
	switch fn.Names[in.A] {
	case "exec":
	case "replace":
		rare = replaceDeadRare
	default:
		return 0, false, false
	}
	skip, ok = b.op(pc, in, code, more)
	if n := len(b.stack); ok && skip == 0 && n > 1 {
		if e := &b.stack[n-1]; e.method == nil && e.rare == nil && e.v != nil && e.slot < 0 && int(b.stack[n-2].slot) == n-2 {
			e.rare = rare
		}
	}
	return skip, ok, true
}

// deadExecCall builds the call_method at pc, of exec's or replace's read,
// into deadExecNode's node, or a RegExp literal's exec into
// deadLitExecNode's, where nothing reads its result (deadResultAt), and
// reports whether it did.
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
	// replace's first argument a RegExp literal's, whose constant it is.
	argLit := n > 0 && b.stack[k+1].regexp && b.stack[k+1].slot < 0
	d := &deadExec[struct{}]{p: k, pc: pc}
	lit := false
	switch {
	case m.method != nil:
		if int(m.method.in.A) >= len(fn.Names) || m.v == nil {
			return false
		}
		switch fn.Names[m.method.in.A] {
		case "exec":
		case "replace":
			// Given a literal, regexpArgMethodCall's, which gives it back.
			if n < 2 || argLit {
				return false
			}
			d.replace = true
		default:
			return false
		}
		if (recv.local || recv.this) && recv.v == nil && recv.slot < 0 {
			d.rv = recv.tree()
		} else if propIn(recv) != nil {
			// Its tree stores it in its slot too, which the method's
			// read reads (getPropThisNode).
			d.rv = recv.v
		} else {
			return false
		}
	case m.rare == execDeadRare && m.v != nil && m.slot < 0 && int(recv.slot) == k-1:
	case m.rare == replaceDeadRare && m.v != nil && m.slot < 0 && int(recv.slot) == k-1 && n >= 2:
		d.replace = true
		if argLit {
			d.argLit, d.k = true, b.stack[k+1].k
		}
	case m.rare != nil && m.rare.regexpCall != nil && !m.rare.regexpCall.test && int(m.slot) != k && int(recv.slot) == k-1:
		lit, d.k = true, m.rare.regexpCall.k
	default:
		return false
	}
	d.args = make([]tval, n)
	for i := n - 1; i >= 0; i-- {
		d.args[i] = b.pop().tree()
	}
	d.f = b.pop().tree()
	b.pop()
	if lit {
		b.push(d.lit)
	} else {
		b.push(d.node)
	}
	return true
}

// deadExec is a node of a call of exec or replace whose result nothing
// reads, of the method f reads, with the arguments to evaluate into slots
// p+1..., at pc. Its methods are what would be closures: a generic
// function's closures are laid out ahead of every generic instantiation
// there was, the tree tier's among them. node is the call of the receiver
// rv evaluates or, rv nil, the one in slot p-1; lit a RegExp literal's
// exec, of constant k, in slot p-1. replace says the method read is
// replace's, argLit that its first argument is a RegExp literal's, of
// constant k.
type deadExec[_ any] struct {
	rv, f           tval
	args            []tval
	p, pc           int
	k               uint32
	replace, argLit bool
}

// node is the call of the method f reads, on the receiver rv evaluates --
// or, rv nil, the one in slot p-1: the realm's exec, or its replace, is
// told that nothing reads its result (deadCallRest), and anything else is
// called as callOperands's node calls a method. A RegExp literal given to
// replace is given back as regexpArgCall's node gives it.
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
	r := c.r
	as := c.stack[p+1 : p+1+len(d.args)]
	if !fv.IsObject() {
		return regexpCallMethod(c, fv, recv, as)
	}
	o := fv.Object()
	if d.replace {
		if o != r.strReplaceFn {
			return regexpCallMethod(c, fv, recv, as)
		}
		rx := as[0]
		done := d.argLit && rx.IsObject() && r.regexpArgCall(o, recv, as)
		var mark *Object
		if rx.IsObject() {
			mark = rx.Object()
		}
		v, err := deadCallRest[struct{}](r, r.strReplaceDead, mark, o, o.fn(), recv, as)
		if err != nil {
			c.throw(err)
		}
		if done {
			r.regexpLiteralDone(&c.cl.fn.Constants[d.k], rx.Object())
		}
		return v
	}
	if o != r.regexpExecFn {
		return regexpCallMethod(c, fv, recv, as)
	}
	var mark *Object
	if recv.IsObject() {
		mark = recv.Object()
	}
	v, err := deadCallRest[struct{}](r, r.regexpExecDead, mark, o, o.fn(), recv, as)
	if err != nil {
		c.throw(err)
	}
	return v
}

// lit is regexpLitCallNode's node of a RegExp literal's exec, of constant
// k, whose result nothing reads (deadCallRest).
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
			var mark *Object
			if o == r.regexpExecFn && recv.IsObject() {
				mark = recv.Object()
			}
			v, err := deadCallRest[struct{}](r, r.regexpExecDead, mark, o, fd, recv, as)
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

// deadCallRest is callRest's call of o, fd its function's data, on recv
// with args, where nothing reads the result: the function told so first,
// at dead, of mark -- exec's receiver, replace's RegExp -- and untold
// after; told nothing where mark or dead is nil.
func deadCallRest[_ any](r *Runtime, dead **Object, mark, o *Object, fd *funcData, recv Value, args []Value) (Value, error) {
	if dead == nil || mark == nil || r.meter != nil || r.debug != nil {
		return r.callRest(o, fd, recv, args)
	}
	*dead = mark
	v, err := r.callRest(o, fd, recv, args)
	*dead = nil
	return v, err
}

// replaceDead is String.prototype.replace's function wrapped, slow its
// own, and dead the RegExp the call about to be made -- its next, and only
// that one -- is given where nothing reads its result (see the top of this
// file). call takes it as it begins.
type replaceDead[_ any] struct {
	slow NativeFunc
	dead *Object
}

// replaceDeadOf wraps the realm's String.prototype.replace (replaceDead),
// for regexpLeaves.
func replaceDeadOf[_ any](r *Runtime) {
	if r.strReplaceFn == nil {
		return
	}
	if fd := r.strReplaceFn.fn(); fd != nil && fd.native != nil {
		l := &replaceDead[struct{}]{slow: fd.native}
		fd.native = l.call
		r.strReplaceDead = &l.dead
	}
}

// call is replace's function: where the call's result is one nothing
// reads, of a string by the RegExp dead with a string, which stringReplace
// would replace directly (replaceDirect), the replace without its string
// (regexpReplaceDead); anything else replace's own.
func (l *replaceDead[_]) call(r *Runtime, this Value, args []Value) (Value, error) {
	dead := l.dead
	l.dead = nil
	if dead != nil && len(args) >= 2 && this.IsString() && args[1].IsString() && args[0].IsObject() && args[0].Object() == dead && r.meter == nil {
		if fl, ok := r.replaceDirect(args[0]); ok {
			if v, ok, err := regexpReplaceDead[struct{}](r, args[0], this.String(), args[1].String(), fl); ok {
				return v, err
			}
		}
	}
	// A string's replace by a template without a $, both ASCII, made in
	// one pass (replaceFlat).
	if len(args) >= 2 && this.IsString() && args[1].IsString() && args[0].IsObject() && r.meter == nil {
		if s, tv := this.String(), args[1].String(); s.ascii && tv.ascii && strings.IndexByte(tv.Go(), '$') < 0 {
			if fl, ok := r.replaceDirect(args[0]); ok {
				return replaceFlat[struct{}](r, args[0], s, tv, fl)
			}
		}
	}
	return l.slow(r, this, args)
}

// regexpReplaceDead is regExpReplace's replace of s by rx, whose flags are
// fl, with the template tv, directly (replaceDirect), where nothing reads
// the result: lastIndex, the matches and the legacy statics as it makes
// them, the result the subject. It reports false, having done nothing,
// where the replacement could be too long to be a string -- each match's
// at most the template, each $ in it the subject -- which regExpReplace
// throws for.
func regexpReplaceDead[_ any](r *Runtime, rx Value, s, tv *String, fl regexp.Flags) (Value, bool, error) {
	n := int64(s.length)
	if per := int64(tv.length) + int64(strings.Count(tv.Go(), "$"))*n; per > maxStringLength || n+(n+1)*per > maxStringLength {
		return Undefined, false, nil
	}
	if fl&regexp.FlagGlobal == 0 {
		// One match, from lastIndex where the RegExp is sticky.
		if err := r.tick(); err != nil {
			return Undefined, true, err
		}
		_, err := r.execMatch(rx, s)
		return Str(s), true, err
	}
	if err := r.setLastIndex(rx.Object(), Int(0)); err != nil {
		return Undefined, true, err
	}
	room := r.takeSpans()
	room, err := r.execMatchAll(rx, s, fl&(regexp.FlagUnicode|regexp.FlagUnicodeSets) != 0, room)
	r.giveSpans(room)
	return Str(s), true, err
}

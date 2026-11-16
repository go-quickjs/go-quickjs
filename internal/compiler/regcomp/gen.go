package regcomp

import (
	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Generators and async functions (docs/register-interpreter.md,
// "Generators and async functions"). A frame that suspends is its
// window: a suspension hands the VM a value and the pc to resume at, and
// the VM keeps the window until the resumption puts it back. The code's
// first instruction, gen_entry, goes on at that pc; a generator's
// creation runs its parameters' binding up to end_params, which
// suspends it until its first next.
//
// What each suspension's resumption delivers is in a register of the
// suspension's own, never a binding's: a throw injected there is
// written to it before it is thrown.

// suspends reports whether fn's frame suspends: a generator's or an
// async function's.
func suspends(fn *bytecode.Function) bool { return fn.Generator || fn.Async }

// yieldExpr compiles yield: the frame suspended with the value, the
// value sent in its result. A return injected at it is a return
// statement there, compiled after the jump a resumption by next takes
// past it; a throw is thrown at the yield itself.
func (g *gen) yieldExpr(n *ast.Yield, dst int) int {
	if n.Delegate {
		return g.yieldStar(n, dst)
	}
	var v int
	if n.Arg != nil {
		v = g.expr(n.Arg, -1)
	} else {
		v = g.alloc()
		g.emit(bytecode.MakeR(bytecode.RLoadUndef, 0, r16(v), 0, 0))
	}
	t := g.alloc()
	g.at(n.Start)
	g.emit(bytecode.MakeR(bytecode.RYield, 0, r16(t), r16(v), 0))
	next := g.newMark()
	g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), next, false)
	g.retFrom(t)
	g.bind(next)
	return g.place(t, dst)
}

// retFrom returns the value in register v from where the code is, as a
// return statement there does: through the finally clauses and the
// iterators' closing it leaves.
func (g *gen) retFrom(v int) {
	if !g.finallyAround() {
		g.retReg(v)
		return
	}
	release := g.hold()
	g.leave(0, func() { g.retReg(v) })
	release()
}

// awaitExpr compiles await: the frame suspended until the value
// settles, what it settles to the result, a rejection thrown at the
// await.
func (g *gen) awaitExpr(n *ast.Await, dst int) int {
	v := g.expr(n.Arg, -1)
	return g.place(g.awaitReg(v, n.Start), dst)
}

// awaitReg awaits the value in register v at source position pos, into a
// register of its own, which it returns.
func (g *gen) awaitReg(v, pos int) int {
	t := g.alloc()
	g.at(pos)
	g.cold(bytecode.RColdAwait, t, v, 0)
	return t
}

// yieldStar compiles yield*, as the stack form does: a loop that sends
// what the generator is resumed with to the delegate's next, throw or
// return, by how it is resumed, and suspends the generator with each
// result the delegate gives (an async generator awaits each first), until
// the delegate is done. Its value is then the delegate's last; where the
// generator was resumed by a return, the generator returns it (an async
// generator awaits it first).
func (g *gen) yieldStar(n *ast.Yield, dst int) int {
	async := g.fn.Async
	src := g.expr(n.Arg, -1)
	// The delegation's steps are placed where the stack form's are, at
	// its operand's last position (a stack trace through the delegate
	// names it); its awaits at the yield.
	at := int(g.cur)
	cur := g.alloc()
	g.at(at)
	start := bytecode.RColdForOfStart
	if async {
		start = bytecode.RColdForAwaitOfStart
	}
	g.cold(start, cur, src, 0)
	// The value sent in and how, a pair yield_star's resumption writes.
	sent := g.allocN(2)
	g.emit(bytecode.MakeR(bytecode.RLoadUndef, 0, r16(sent), 0, 0))
	g.emit(bytecode.MakeR(bytecode.RLoadInt, 0, r16(sent+1), 0, 0))
	v := g.alloc()
	// The loop's back jump keeps what is live around it: these and what
	// the expression it is in holds.
	release := g.hold()
	end := g.newMark()
	loop := g.newMark()
	g.bind(loop)
	g.at(at)
	g.cold(bytecode.RColdIterResume, v, cur, sent)
	g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), end, false)
	if async {
		g.at(n.Start)
		g.cold(bytecode.RColdAwait, v, v, 0)
	}
	g.at(at)
	g.cold(bytecode.RColdIterUnpackDelegate, v, v, cur)
	g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), end, false)
	g.at(at)
	g.cold(bytecode.RColdYieldStar, sent, v, 0)
	g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), loop, false)
	g.bind(end)
	release()
	// The delegation is over: a return it carried out returns from here.
	notReturn := g.newMark()
	g.cond(&ast.Binary{Op: "===", Left: &regExpr{Ident: &ast.Ident{}, reg: sent + 1},
		Right: &ast.NumberLit{Value: float64(bytecode.ResumeReturn)}}, false, notReturn)
	r := v
	if async {
		r = g.awaitReg(v, n.Start)
	}
	g.retFrom(r)
	g.bind(notReturn)
	return g.place(v, dst)
}

package regcomp

import (
	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A try statement is compiled to handler tables (docs/register-interpreter.md,
// "Exceptions"): the code of its block is in the ranges of a handler, and an
// exception an instruction there throws goes on at the handler's pc, its
// value in the handler's register. Nothing is run as the block is entered
// or left. A finally clause is compiled once for each way out of what it
// protects: in place after the block's normal completion, before each
// break, continue or return that leaves it, and as a handler for a throw,
// which throws the value again at its end. A clause's copy before an exit
// is outside the ranges of the handlers the exit leaves, and inside those
// of the handlers around them.

// prot is a handler while its code is compiled.
type prot struct {
	// ranges are the [start, end) pairs of pcs it covers so far, and open
	// the start of the one being compiled, or -1.
	ranges []uint32
	open   int
	to     *mark
	reg    int
	// finally is the finally clause's statements, nil for a catch clause.
	finally []ast.Stmt
	// cursor is, for a for-of loop's handler, the register of its cursor,
	// which an exit closes, and iterOf the loop, which a continue of it
	// does not leave; -1 and nil otherwise.
	cursor int
	iterOf *loop
	// async marks a for await's cursor, closed as AsyncIteratorClose
	// closes it: its return method's result awaited (asyncClose).
	async bool
	// loops is how many loops were around the statement: a break or a
	// continue to one of them leaves the handler.
	loops int
	// with marks a with statement's body, whose object an exit takes off
	// the frame's with objects (with_pop).
	with bool
	// shared is a finally clause's one copy, where it is compiled once.
	shared *finShared
}

// protect puts p in force from here.
func (g *gen) protect(p *prot) {
	p.open = len(g.code)
	p.loops = len(g.loops)
	g.prots = append(g.prots, p)
}

// unprotect ends p, the innermost handler in force, and adds it to the
// table.
func (g *gen) unprotect(p *prot) {
	g.suspend(p)
	g.prots = g.prots[:len(g.prots)-1]
	if len(p.ranges) > 0 {
		g.handlers = append(g.handlers, p)
	}
}

// suspend ends p's open range, and resume opens another.
func (g *gen) suspend(p *prot) {
	if p.open >= 0 && p.open < len(g.code) {
		p.ranges = append(p.ranges, uint32(p.open), uint32(len(g.code)))
	}
	p.open = -1
}

func (g *gen) resume(p *prot) { p.open = len(g.code) }

// finallyDepth is how deep finally clauses' copies nest: each clause is
// compiled once for each way out of it, so that a clause inside one is
// compiled for each of the outer's copies. A clause deeper is compiled
// once, with a completion dispatch (finShared), and so is every clause
// inside it.
const finallyDepth = 4

// leave compiles an exit that leaves the handlers from g.prots[from:]: the
// finally clauses among them, innermost first, each outside the ranges of
// the handlers it is past and inside those of the rest, and then the exit
// itself, by last.
func (g *gen) leave(from int, last func()) {
	crossed := g.prots[from:]
	fin := false
	for _, p := range crossed {
		fin = fin || p.finally != nil || p.cursor >= 0 || p.with
	}
	if !fin {
		last()
		return
	}
	saved, savedLoops := g.prots, g.loops
	for i := len(crossed) - 1; i >= 0; i-- {
		p := crossed[i]
		g.suspend(p)
		if p.with {
			g.cold(bytecode.RWithPop, 0, 0, 0)
			continue
		}
		if p.cursor >= 0 {
			// A loop left closes its iterator, which may throw.
			if p.async {
				g.asyncClose(p.cursor)
			} else {
				g.emit(bytecode.MakeR(bytecode.RIterClose, 0, 0, r16(p.cursor), 0))
			}
			continue
		}
		if p.finally == nil {
			continue
		}
		if s := p.shared; s != nil {
			// The clause's one copy, by a way in of the exit's own, after
			// which the rest of the exit goes on (finShared).
			outer := saved[: from+i : from+i]
			loops := savedLoops
			s.conts = append(s.conts, func() {
				g.prots, g.loops = outer, loops
				g.leave(from, last)
			})
			g.number(float64(len(s.conts)+1), p.shared.kind)
			s.top = max(s.top, g.top)
			g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), s.entry, false)
			for _, q := range crossed[i:] {
				g.resume(q)
			}
			return
		}
		g.prots = saved[: from+i : from+i]
		g.loops = savedLoops[:p.loops:p.loops]
		g.finallyCopy(p.finally)
		g.prots, g.loops = saved, savedLoops
		if g.err != "" {
			return
		}
	}
	last()
	for _, p := range crossed {
		g.resume(p)
	}
}

// finallyCopy compiles a copy of a finally clause's statements.
func (g *gen) finallyCopy(list []ast.Stmt) {
	g.copies++
	g.finallyBody(list)
	g.copies--
}

// finallyBody compiles a finally clause's statements, the statement's
// completion value kept around them.
func (g *gen) finallyBody(list []ast.Stmt) {
	mark := g.top
	if g.completion > 0 {
		// What the clause produces is not the statement's value: the try
		// block's is, kept meanwhile; a break or a continue inside the
		// clause carries the clause's own, undefined where it produced
		// none, as the stack form's compileFinallyClause has it.
		save := g.alloc()
		release := g.hold()
		g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(save), r16(g.completion), 0))
		g.resetCompletion()
		g.stmts(list)
		g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(g.completion), r16(save), 0))
		release()
	} else {
		g.stmts(list)
	}
	g.top = mark
}

// protsOutside is how many handlers in force are around loop l: the ones
// past it a break or a continue to l leaves. A continue does not leave
// the loop's own iterator.
func (g *gen) protsOutside(l *loop, cont bool) int {
	i := 0
	for i < len(g.prots) && g.prots[i].loops <= g.loopIndex(l) {
		i++
	}
	if cont && i < len(g.prots) && g.prots[i].iterOf == l {
		i++
	}
	return i
}

func (g *gen) loopIndex(l *loop) int {
	for i, x := range g.loops {
		if x == l {
			return i
		}
	}
	return len(g.loops)
}

// tryStmt compiles a try statement.
func (g *gen) tryStmt(n *ast.TryStmt) {
	if n.Inline {
		// Nothing makes one: a using scope is the statements' own
		// (usingScope).
		panic("regcomp: an inline try statement")
	}
	var fin, cat *prot
	if n.Finally != nil {
		fin = &prot{to: g.newMark(), reg: g.alloc(), finally: n.Finally, cursor: -1}
		if g.copies >= finallyDepth {
			fin.shared = &finShared{entry: g.newMark(), kind: g.alloc()}
		}
		defer g.hold()()
		g.protect(fin)
	}
	after := g.newMark()
	if n.Catch == nil {
		g.stmts(n.Block)
	} else {
		reg, ok := g.catchReg(n.Catch)
		if !ok {
			return
		}
		cat = &prot{to: g.newMark(), reg: reg, cursor: -1}
		defer g.hold()()
		g.protect(cat)
		g.stmts(n.Block)
		g.unprotect(cat)
		g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), after, false)
		g.bind(cat.to)
		g.freshCatch(n.Catch, cat.reg)
		if pat := n.Catch.Param; pat != nil {
			if _, ok := pat.(*ast.Ident); !ok {
				g.patternBindings(pat)
				g.pattern(pat, cat.reg, true)
			}
		}
		g.stmts(n.Catch.Body)
	}
	g.bind(after)
	if fin == nil {
		return
	}
	g.unprotect(fin)
	if fin.shared != nil {
		g.finallyShared(fin, n.Finally, n.Start)
		return
	}
	end := g.newMark()
	g.finallyCopy(n.Finally)
	g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), end, false)
	g.bind(fin.to)
	g.finallyCopy(n.Finally)
	g.at(n.Start)
	g.emit(bytecode.MakeR(bytecode.RThrow, 0, r16(fin.reg), 0, 0))
	g.bind(end)
}

// finShared is a finally clause compiled once, where copies of it would
// nest deeper than finallyDepth: each way in sets kind -- 0 the try
// block's normal end, 1 a throw, k > 1 the exit of conts[k-2] -- and
// jumps to entry, and after the clause, the completion dispatch goes on
// as kind says. top is the most registers a way in has live, which the
// clause and the exits' rests leave alone.
type finShared struct {
	entry *mark
	kind  int
	conts []func()
	top   int
}

// finallyShared compiles fin's clause, list, once, with its completion
// dispatch: the try block's normal end falls in, a throw comes in from
// the handler and is thrown again after it, and each exit that leaves
// it goes on past it with the rest of the exit -- the clauses outside,
// the jump or the return -- compiled there.
func (g *gen) finallyShared(fin *prot, list []ast.Stmt, pos int) {
	s := fin.shared
	g.number(0, s.kind)
	s.top = max(s.top, g.top)
	g.bind(s.entry)
	saved := g.top
	g.top = s.top
	g.finallyBody(list)
	end, rethrow := g.newMark(), g.newMark()
	g.jumpIfKind(s.kind, 0, end)
	g.jumpIfKind(s.kind, 1, rethrow)
	conts := make([]*mark, len(s.conts))
	for i := range conts {
		conts[i] = g.newMark()
		g.jumpIfKind(s.kind, i+2, conts[i])
	}
	g.bind(rethrow)
	g.at(pos)
	g.emit(bytecode.MakeR(bytecode.RThrow, 0, r16(fin.reg), 0, 0))
	prots, loops := g.prots, g.loops
	// An exit's rest may make more of the clause's exits, of the clauses
	// outside it: not of this one, which is behind it.
	for i := 0; i < len(conts); i++ {
		g.bind(conts[i])
		g.top = s.top
		s.conts[i]()
		g.prots, g.loops = prots, loops
	}
	g.bind(fin.to)
	g.number(1, s.kind)
	g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), s.entry, false)
	g.bind(end)
	g.top = saved
}

// jumpIfKind jumps to m where register kind holds the integer k.
func (g *gen) jumpIfKind(kind, k int, m *mark) {
	if k <= 32767 {
		g.jump(bytecode.MakeR(bytecode.RJSEqI, 0, r16(kind), uint16(int16(k)), 0), m, true)
		return
	}
	t := g.alloc()
	g.number(float64(k), t)
	g.jump(bytecode.MakeR(bytecode.RJSEq, 0, r16(kind), r16(t), 0), m, true)
	g.top = t
}

// usingKind reports whether a statement list declares anything with
// using, and whether any of it is await using (the stack form's).
func usingKind(body []ast.Stmt) (has, async bool) {
	for _, s := range body {
		if vd, ok := s.(*ast.VarDecl); ok && vd.Kind.IsUsing() {
			has = true
			async = async || vd.Kind == ast.DeclAwaitUsing
		}
	}
	return has, async
}

// usingScope compiles body as a scope with using declarations, as the
// stack form's compileDisposeScope: its resources in a register of their
// own (new_dispose_capability), and a finally clause that disposes of
// them, compiled for each way out -- its normal end and each exit with
// no exception to fold in, a throw with the one it folds in.
func (g *gen) usingScope(pos int, async bool, body func()) {
	c := g.alloc()
	g.at(pos)
	g.cold(bytecode.RNewDispose, c, 0, 0)
	ds := &ast.DisposeStmt{Async: async, Start: pos}
	if g.disposeCaps == nil {
		g.disposeCaps = map[*ast.DisposeStmt]int{}
	}
	g.disposeCaps[ds] = c
	fin := &prot{to: g.newMark(), reg: g.alloc(), finally: []ast.Stmt{ds}, cursor: -1}
	defer g.hold()()
	g.protect(fin)
	g.usingCaps = append(g.usingCaps, c)
	body()
	g.usingCaps = g.usingCaps[:len(g.usingCaps)-1]
	g.unprotect(fin)
	if g.err != "" {
		return
	}
	end := g.newMark()
	g.finallyCopy(fin.finally)
	g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), end, false)
	g.bind(fin.to)
	g.disposeOut(ds, fin.reg)
	g.at(pos)
	g.emit(bytecode.MakeR(bytecode.RThrow, 0, r16(fin.reg), 0, 0))
	g.bind(end)
}

// usingDecl compiles a using or await using declaration: each value added
// to the innermost scope's resources (add_disposable, which throws for a
// value with no disposer) before its binding is initialized.
func (g *gen) usingDecl(n *ast.VarDecl) {
	if len(g.usingCaps) == 0 {
		panic("regcomp: a using declaration outside a scope that disposes")
	}
	c := g.usingCaps[len(g.usingCaps)-1]
	async := 0
	if n.Kind == ast.DeclAwaitUsing {
		async = 1
	}
	for _, d := range n.Decls {
		id, ok := d.Target.(*ast.Ident)
		if !ok || d.Init == nil {
			panic("regcomp: a using declaration of other than a name and a value")
		}
		v := g.expr(d.Init, -1)
		g.at(id.Pos())
		g.cold(bytecode.RAddDisposable, c, v, async)
		g.storeIdent(id, &regExpr{Ident: id, reg: v}, true)
		g.top = g.stmtTop()
	}
}

// disposeOut compiles a using scope's disposal, the scope's finally
// clause, where thrown is the register of a throw leaving the scope, or -1.
// An await using scope's is a loop of steps, each running the disposers
// until one leaves a value to await, which the loop awaits, an await that
// throws folded in by the next step (the stack form's compileDispose).
func (g *gen) disposeOut(ds *ast.DisposeStmt, thrown int) {
	c, ok := g.disposeCaps[ds]
	if !ok {
		panic("regcomp: a disposal of no scope")
	}
	if !ds.Async {
		g.at(ds.Start)
		if thrown >= 0 {
			g.cold(bytecode.RDispose, c, thrown, 1)
		} else {
			g.cold(bytecode.RDispose, c, c, 0)
		}
		return
	}
	// The loop's registers are live across its backward jumps, whose
	// sweep of stale registers spares only what is held.
	t := g.allocN(2)
	defer g.hold()()
	mode := bytecode.DisposeStart
	if thrown >= 0 {
		g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(t), r16(thrown), 0))
		mode |= bytecode.DisposeRecord
	}
	check, done := g.newMark(), g.newMark()
	g.at(ds.Start)
	g.cold(bytecode.RDisposeStep, t, c, mode)
	g.bind(check)
	g.jump(bytecode.MakeR(bytecode.RJmpF, 0, r16(t+1), 0, 0), done, false)
	p := &prot{to: g.newMark(), reg: t, cursor: -1}
	g.protect(p)
	g.awaitReg(t, ds.Start)
	g.unprotect(p)
	g.at(ds.Start)
	g.cold(bytecode.RDisposeStep, t, c, 0)
	g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), check, false)
	g.bind(p.to)
	g.at(ds.Start)
	g.cold(bytecode.RDisposeStep, t, c, bytecode.DisposeFail)
	g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), check, false)
	g.bind(done)
	g.at(ds.Start)
	g.cold(bytecode.RDisposeStep, t, c, bytecode.DisposeFinish)
}

// catchReg is the register a catch clause's value goes to: its binding's,
// or a temporary where it has none or nothing reads it.
func (g *gen) catchReg(c *ast.CatchClause) (int, bool) {
	if c.Param == nil {
		return g.alloc(), true
	}
	if _, ok := c.Param.(*ast.Ident); !ok {
		// A pattern, which the clause's body unpacks first.
		return g.alloc(), true
	}
	slot, ok := g.rec.Catches[c]
	if !ok {
		// bindCatchParam records every identifier's: none is the stack
		// compiler's bug, not a form to refuse.
		panic("regcomp: a catch clause's binding the record does not name")
	}
	if int(slot) >= len(g.slotReg) || g.slotReg[slot] < 0 {
		return g.alloc(), true
	}
	if g.fn.Locals[slot].Captured && g.inLoop() {
		// Each turn's binding is a new one a closure keeps: the value
		// waits in a temporary until the clause closes the binding of the
		// turn before (freshCatch).
		return g.alloc(), true
	}
	return int(g.slotReg[slot]), true
}

// handlerTable is the code's handlers, innermost first.
func (g *gen) handlerTable() []bytecode.RHandler {
	var hs []bytecode.RHandler
	for _, p := range g.handlers {
		hs = append(hs, bytecode.RHandler{Ranges: p.ranges, To: uint32(p.to.at), Reg: r16(p.reg),
			Catches: p.finally == nil && p.cursor < 0 && !p.with})
	}
	return hs
}

// finallyAround reports whether a finally clause, or a for-of loop's
// iterator, is around the code: what a return runs on its way out.
func (g *gen) finallyAround() bool {
	for _, p := range g.prots {
		if p.finally != nil || p.cursor >= 0 || p.with {
			return true
		}
	}
	return false
}

// forOfStmt compiles a for-of loop, or with in a for-in loop: a cursor in
// a register of its own, made by for_of_start or for_in_start (cold) and
// stepped by iter_next, a direct operation, at the head of each turn, which jumps out of the loop where
// it has no value. A for-of loop's body is in the ranges of a handler of
// its own, which closes the iterator for a throw and throws again; a
// break, continue or return that leaves the loop closes it on its way out
// (leave). A for-in loop's cursor needs no closing.
func (g *gen) forOfStmt(left ast.Node, right ast.Expr, body ast.Stmt, await, in bool, label labelSet, pos int) {
	var target ast.Expr
	lexical := false
	fresh := -1
	var using ast.DeclKind
	switch l := left.(type) {
	case *ast.VarDecl:
		if len(l.Decls) != 1 || l.Decls[0].Init != nil && (!in || l.Kind != ast.DeclVar) {
			// The parser allows one declaration, with an initializer
			// only as Annex B's for (var a = 0 in o).
			panic("regcomp: a for-of or for-in head's declaration")
		}
		if l.Decls[0].Init != nil {
			// Assigned once, before the object is evaluated, as the
			// stack form's compileForIn.
			g.varDecl(l)
		}
		target, lexical = l.Decls[0].Target, l.Kind != ast.DeclVar
		if l.Kind.IsUsing() {
			using = l.Kind
		}
	case ast.Expr:
		target = l
	}
	id, ok := target.(*ast.Ident)
	if !ok {
		g.forOfPattern(target, right, body, await, in, label, pos, lexical, left)
		return
	}
	res, ok := g.rec.WriteOf(id)
	if !ok {
		g.fail("a store the record does not resolve")
		return
	}
	dst := -1
	switch res.Kind {
	case ResLocal, ResLocalInit:
		slot := int(res.Index)
		if g.farOf(res.Index) >= 0 {
			// Stored at the head of each turn (storeValue), as a far
			// binding is: never captured, never in its dead zone.
			break
		}
		dst = int(g.slotReg[slot])
		if lexical && g.fn.Locals[slot].Captured {
			// Each turn's binding is a new one a closure keeps: closed at
			// the head of each turn, the closures of the turns before keep
			// theirs.
			fresh = dst
		}
		if lexical {
			// The head's expression has a binding of the name of its own,
			// in its dead zone for good, where a closure there reads it.
			g.headTDZ(left)
		}
		if lexical && g.tdz[slot] {
			// The head's expression sees the binding in its dead zone.
			g.emit(bytecode.MakeR(bytecode.RLoadUninit, 0, r16(dst), 0, 0))
		}
	case ResUpval, ResUpvalCheck, ResGlobal, ResGlobalVar, ResGlobalLexInit, ResModuleLexInit, ResLocalCheck, ResConstAssign, ResIgnore, ResWith:
		// Stored at the head of each turn (storeValue).
	default:
		g.fail("a for-of or for-in loop's store the backend does not compile")
		return
	}
	src := g.expr(right, -1)
	cursor := g.alloc()
	// The cursor is made, and stepped, at the position the stack form's
	// for_of_start and iter_next_or_jump have, which no line of their own
	// starts: the last of the expression's, or before it (forHead).
	head := g.forHead(pos)
	g.emit(bytecode.MakeR(cursorStart(await, in), 0, r16(cursor), r16(src), 0))
	l := g.pushLoop(label)
	g.bind(l.cont)
	if fresh >= 0 {
		g.closeRegs([]int{fresh})
	}
	val := dst
	if val < 0 || using != 0 {
		// A using head's value is added to the turn's resources before
		// its binding is initialized.
		val = g.alloc()
	}
	g.at(head)
	g.debugMark(int(left.Pos()))
	g.iterStep(val, cursor, l.brk, await, int(right.Pos()))
	var p *prot
	if !in {
		p = &prot{to: g.newMark(), reg: g.alloc(), cursor: cursor, iterOf: l, async: await}
		g.protect(p)
	}
	defer g.hold()()
	if using != 0 {
		// Each turn's value is disposed of at the turn's end, before the
		// loop goes on or, leaving, closes its iterator.
		g.usingScope(int(id.Pos()), using == ast.DeclAwaitUsing, func() {
			async := 0
			if using == ast.DeclAwaitUsing {
				async = 1
			}
			g.at(int(id.Pos()))
			g.cold(bytecode.RAddDisposable, g.usingCaps[len(g.usingCaps)-1], val, async)
			if dst >= 0 {
				g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(dst), r16(val), 0))
			} else {
				g.storeValue(id, val)
			}
			g.stmt(body)
		})
	} else {
		if dst < 0 {
			g.storeValue(id, val)
		}
		g.stmt(body)
	}
	g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), l.cont, false)
	if p != nil {
		g.unprotect(p)
		g.bind(p.to)
		g.closeThrow(p)
	}
	g.bind(l.brk)
	g.popLoop()
}

// forOfPattern is forOfStmt with a pattern, or an assignment's property,
// for its target: each turn's value unpacked into it in the loop's body,
// where a throw closes the iterator.
func (g *gen) forOfPattern(target ast.Expr, right ast.Expr, body ast.Stmt, await, in bool, label labelSet, pos int, lexical bool, left ast.Node) {
	_, declaring := left.(*ast.VarDecl)
	if lexical {
		// The head's expression sees the bindings in their dead zone; a
		// closure's binding is each turn's own.
		g.headTDZ(left)
		g.patternBindings(target)
	}
	src := g.expr(right, -1)
	cursor := g.alloc()
	head := g.forHead(pos)
	g.emit(bytecode.MakeR(cursorStart(await, in), 0, r16(cursor), r16(src), 0))
	l := g.pushLoop(label)
	g.bind(l.cont)
	if lexical {
		// Each turn's bindings are new ones, in their dead zone until the
		// pattern initializes them.
		g.patternBindings(target)
	}
	val := g.alloc()
	g.at(head)
	g.debugMark(int(left.Pos()))
	g.iterStep(val, cursor, l.brk, await, int(right.Pos()))
	var p *prot
	if !in {
		p = &prot{to: g.newMark(), reg: g.alloc(), cursor: cursor, iterOf: l, async: await}
		g.protect(p)
	}
	defer g.hold()()
	mark := g.top
	g.pattern(target, val, declaring)
	g.top = mark
	g.stmt(body)
	g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), l.cont, false)
	if p != nil {
		g.unprotect(p)
		g.bind(p.to)
		g.closeThrow(p)
	}
	g.bind(l.brk)
	g.popLoop()
}

// forHead is the position a for-of or a for-in loop at pos makes and steps
// its cursor at, once its expression is compiled: the position the
// expression's code left, as the stack form's cursor instructions, which
// record none, have the line of the expression's last instruction that
// records one. The loop's own position where the expression left none
// before it. A stack trace through a step -- a generator's body, an
// iterator's next -- names the line of that instruction, through SrcPC,
// which maps a position to the stack form's line at or before it: the
// loop's own position comes before any where the function's code before
// the loop records none, and named no line at all.
func (g *gen) forHead(pos int) int {
	if g.cur < int32(pos) {
		g.at(pos)
	}
	return int(g.cur)
}

// cursorStart is the operation that makes a loop's cursor.
func cursorStart(await, in bool) bytecode.ROp {
	switch {
	case await:
		return bytecode.RForAwaitOfStart
	case in:
		return bytecode.RForInStart
	}
	return bytecode.RForOfStart
}

// iterStep steps a loop's cursor into val, jumping to brk where it has no
// value: iter_next, or for a for await, the next method's result awaited
// and unpacked, in a register of its own, which code that runs meanwhile
// could otherwise see in the binding. The await is at awaitPos, the loop's
// expression's, where the stack form's is.
func (g *gen) iterStep(val, cursor int, brk *mark, await bool, awaitPos int) {
	if !await {
		g.jump(bytecode.MakeR(bytecode.RIterNext, 0, r16(val), r16(cursor), 0), brk, true)
		return
	}
	t := g.alloc()
	g.cold(bytecode.RAsyncIterNext, t, cursor, 0)
	g.at(awaitPos)
	g.cold(bytecode.RAwait, t, t, 0)
	g.cold(bytecode.RIterResult, val, t, 0)
	g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), brk, false)
	g.top = t
}

// closeThrow is the handler of a for-of loop's body, p, whose exception,
// in p's register, leaves the loop: the iterator closed, what closing it
// throws dropped, and the exception thrown again. A for await's return
// method's result is awaited, as AsyncIteratorClose awaits it.
func (g *gen) closeThrow(p *prot) {
	if !p.async {
		g.emit(bytecode.MakeR(bytecode.RIterCloseThrow, 0, 0, r16(p.cursor), 0))
		g.emit(bytecode.MakeR(bytecode.RThrow, 0, r16(p.reg), 0, 0))
		return
	}
	q := &prot{to: g.newMark(), reg: g.alloc(), cursor: -1}
	g.protect(q)
	v := g.alloc()
	g.cold(bytecode.RAsyncIterClose, v, p.cursor, 0)
	g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), q.to, false)
	g.cold(bytecode.RAwait, v, v, 0)
	g.unprotect(q)
	g.bind(q.to)
	g.emit(bytecode.MakeR(bytecode.RThrow, 0, r16(p.reg), 0, 0))
}

// asyncClose closes a for await's cursor as a break, a continue or a
// return that leaves the loop does: its return method called, where it has
// one, and the result awaited and checked to be an object.
func (g *gen) asyncClose(cursor int) {
	v := g.alloc()
	skip := g.newMark()
	g.cold(bytecode.RAsyncIterClose, v, cursor, 0)
	g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), skip, false)
	g.cold(bytecode.RAwait, v, v, 0)
	g.cold(bytecode.RAsyncIterCheck, v, 0, 0)
	g.bind(skip)
}

// freshCatch gives a catch clause in a loop whose binding a closure keeps
// its value, from reg, the clause's register, once the captures of the
// turn before's binding are closed.
func (g *gen) freshCatch(c *ast.CatchClause, reg int) {
	if _, ok := c.Param.(*ast.Ident); !ok {
		return
	}
	slot, ok := g.rec.Catches[c]
	if !ok || int(slot) >= len(g.slotReg) || g.slotReg[slot] < 0 || !g.fn.Locals[slot].Captured || !g.inLoop() {
		return
	}
	b := int(g.slotReg[slot])
	g.closeRegs([]int{b})
	g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(b), r16(reg), 0))
}

// headTDZ puts the bindings a for-of or for-in loop's head's expression
// sees in their dead zone, an environment of their own
// (Record.Heads), in it, where a closure there may read them.
func (g *gen) headTDZ(left ast.Node) {
	d, ok := left.(*ast.VarDecl)
	if !ok {
		return
	}
	for _, slot := range g.rec.Heads[d] {
		if int(slot) < len(g.slotReg) && g.slotReg[slot] >= 0 && g.fn.Locals[slot].Captured {
			g.emit(bytecode.MakeR(bytecode.RLoadUninit, 0, r16(int(g.slotReg[slot])), 0, 0))
		}
	}
}

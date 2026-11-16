package regcomp

import (
	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A name inside a with statement's body, or in a function with a direct
// eval that may declare it, is compiled as the stack form compiles it: a
// probe of the with objects and the eval's variables in scope -- the
// innermost Limit of them (Record.Withs) -- that sets a found flag, and a
// branch around the static access the record resolved. The probes are cold
// operations, each with the name and the limit, the flag's register in
// Aux: with_get, with_get_this, with_delete and with_set read, call, delete
// and write through the object that has the name; with_resolve settles
// which object a reference names before its value is made, and with_read
// and with_write read and write through it after.

// withName is a name the record says a probe precedes (ResWith): the
// probe, and the name's static resolution, read and write, kept aside.
type withName struct {
	probe           WithProbe
	read, write     Res
	hasRead, hasRwr bool
}

// withRef is the registers of a reference's object, undefined where the
// probe found none, and of the found flag (with_resolve).
type withRef struct{ base, flag int }

// probedNames sets aside the static resolution of each name the record
// says a probe precedes, its resolution ResWith from here (gen.withs).
func (g *gen) probedNames() {
	for id, p := range g.rec.Withs {
		w := &withName{probe: p}
		if res, ok := g.rec.ReadOf(id); ok {
			w.read, w.hasRead = res, true
			g.rec.setRead(id, Res{Kind: ResWith}, true)
		}
		if res, ok := g.rec.WriteOf(id); ok {
			w.write, w.hasRwr = res, true
			g.rec.setWrite(id, Res{Kind: ResWith}, true)
		}
		if g.withs == nil {
			g.withs = map[*ast.Ident]*withName{}
		}
		g.withs[id] = w
	}
}

// withStatic compiles f with id's static resolution in the record, as the
// stack form compiled its access past the probe, and no probe of it.
func (g *gen) withStatic(id *ast.Ident, f func()) {
	w := g.withs[id]
	if w == nil {
		panic("regcomp: a probe of a name the record does not probe")
	}
	g.rec.setRead(id, w.read, w.hasRead)
	g.rec.setWrite(id, w.write, w.hasRwr)
	delete(g.withs, id)
	f()
	g.withs[id] = w
	g.rec.setRead(id, Res{Kind: ResWith}, w.hasRead)
	g.rec.setWrite(id, Res{Kind: ResWith}, w.hasRwr)
}

// probeOp is id's probe's instruction, which must be one of ops: what the
// stack form compiled for the name where the backend compiles it.
func (g *gen) probeOp(id *ast.Ident, ops ...bytecode.Op) bytecode.Op {
	op := g.withs[id].probe.Op
	for _, o := range ops {
		if o == op {
			return op
		}
	}
	panic("regcomp: the probe " + op.String() + " of " + id.Name + " where the backend compiles another")
}

// probe emits id's probe sub, of operand a and B and C the name and the
// limit, its found flag in a new register, which it returns.
func (g *gen) probe(id *ast.Ident, sub bytecode.ROp, a int) int {
	flag := g.alloc()
	g.at(int(id.Start))
	at := g.far(g.cold(sub, a, int(g.name16(id.Name)), int(g.withs[id].probe.Limit)), id.Name)
	g.aux(at, uint32(flag))
	return flag
}

// probed compiles id's probe sub of operand a and, where it finds
// nothing, static with the name's static resolution.
func (g *gen) probed(id *ast.Ident, sub bytecode.ROp, a int, static func()) {
	flag := g.probe(id, sub, a)
	end := g.newMark()
	g.jump(bytecode.MakeR(bytecode.RJmpT, 0, r16(flag), 0, 0), end, false)
	g.withStatic(id, static)
	g.bind(end)
}

// withIdent is ident's read of a probed name into d.
func (g *gen) withIdent(id *ast.Ident, dst int) int {
	if ref, ok := g.withRefs[id]; ok {
		return g.refRead(id, ref, dst)
	}
	g.probeOp(id, bytecode.OpWithGet)
	d := g.dest(dst)
	g.probed(id, bytecode.RWithGet, d, func() { g.ident(id, d) })
	return d
}

// withResolve settles the object id names before its value is made
// (with_resolve), for the read and the write that follow (withRefs).
func (g *gen) withResolve(id *ast.Ident) withRef {
	base := g.alloc()
	ref := withRef{base: base, flag: g.probe(id, bytecode.RWithResolve, base)}
	if g.withRefs == nil {
		g.withRefs = map[*ast.Ident]withRef{}
	}
	g.withRefs[id] = ref
	return ref
}

// refRead reads id through ref into dst: from its object, or with the
// static resolution where the probe found none.
func (g *gen) refRead(id *ast.Ident, ref withRef, dst int) int {
	d := g.dest(dst)
	static, end := g.newMark(), g.newMark()
	g.jump(bytecode.MakeR(bytecode.RJmpF, 0, r16(ref.flag), 0, 0), static, false)
	g.at(int(id.Start))
	g.far(g.cold(bytecode.RWithRead, d, ref.base, int(g.name16(id.Name))), id.Name)
	g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), end, false)
	g.bind(static)
	g.withStatic(id, func() { g.readForWrite(id, d) })
	g.bind(end)
	return d
}

// withStore stores v into the probed name id: through the object its
// reference settled, or after a probe (with_set), with the static
// resolution where it finds none.
func (g *gen) withStore(id *ast.Ident, v int) {
	ref, ok := g.withRefs[id]
	if !ok {
		g.probeOp(id, bytecode.OpWithSet)
		g.probed(id, bytecode.RWithSet, v, func() { g.storeValue(id, v) })
		return
	}
	static, end := g.newMark(), g.newMark()
	g.jump(bytecode.MakeR(bytecode.RJmpF, 0, r16(ref.flag), 0, 0), static, false)
	g.at(int(id.Start))
	g.far(g.cold(bytecode.RWithWrite, v, ref.base, int(g.name16(id.Name))), id.Name)
	g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), end, false)
	g.bind(static)
	g.withStatic(id, func() { g.storeValue(id, v) })
	g.bind(end)
}

// withAssign is storeIdent's assignment of value to the probed name id:
// resolved before the value is made (with_resolve) or probed after it
// (with_set), as the stack form compiled it.
func (g *gen) withAssign(id *ast.Ident, value ast.Expr) int {
	if g.probeOp(id, bytecode.OpWithResolve, bytecode.OpWithSet) == bytecode.OpWithResolve {
		g.withResolve(id)
	}
	v := g.expr(value, -1)
	g.withStore(id, v)
	return v
}

// withRefFor settles the reference of a compound assignment's or an
// update's probed name before its read (with_resolve), as the stack
// form's with_get_under.
func (g *gen) withRefFor(id *ast.Ident) {
	g.probeOp(id, bytecode.OpWithGetUnder)
	g.withResolve(id)
}

// withStmt compiles a with statement, as the stack form's compileWith:
// its object put on the frame's with objects (with_push, which converts
// it with ToObject), and its body in the range of a handler that takes it
// off again (with_pop) for a throw, as each other way out does (leave).
func (g *gen) withStmt(n *ast.WithStmt) {
	o := g.expr(n.Object, -1)
	g.at(n.Start)
	g.cold(bytecode.RWithPush, o, 0, 0)
	// A body that produces no value leaves the statement's undefined.
	g.resetCompletion()
	p := &prot{to: g.newMark(), reg: g.alloc(), cursor: -1, with: true}
	defer g.hold()()
	g.protect(p)
	g.stmt(n.Body)
	g.unprotect(p)
	if g.err != "" {
		return
	}
	g.cold(bytecode.RWithPop, 0, 0, 0)
	if len(p.ranges) == 0 {
		return
	}
	end := g.newMark()
	g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), end, false)
	g.bind(p.to)
	g.cold(bytecode.RWithPop, 0, 0, 0)
	g.emit(bytecode.MakeR(bytecode.RThrow, 0, r16(p.reg), 0, 0))
	g.bind(end)
}

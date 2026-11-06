package compiler

import (
	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A closure appending to a string its maker holds.
//
//	function render() {
//		var out = ""
//		var sink = { push(chunk) { chunk !== null && (out += chunk); return true } }
//		...
//		return out
//	}
//
// is how React's renderToString collects a page, a chunk of three characters
// at a time, and how a template's emit or an accumulator works. Each out +=
// chunk makes a rope's node, and those were half the objects a render
// allocates. They need not be: nothing but the binding holds the last string,
// so an append may grow it in place (the VM's strAppend), if whatever else
// reads the binding is told that what it read is no longer the binding's
// alone, by a str_freeze after the read.
//
// The compiler can see every read of a binding, but not all at once: it
// compiles in one pass, a closure before what follows it. So it records, on
// the binding, that a read has been compiled (localVar.read), and an addition
// is an owned append only if none has. From then on every read of the binding
// is followed by a str_freeze (appended). So a read is compiled before the
// append, and there is no owned append, or after it, and it freezes what it
// read, in whatever order they run. A binding whose reader is compiled first,
// a function declaration above its appender, stays as it was.
//
// An append is one that nothing else could see the middle of:
//
//   - x += y whose value nothing uses, as the value would be another reference
//     to the string. compileExprForEffect asks, for x += y and a short circuit
//     around it.
//   - x is a binding of a function around the closure, a var or a let past its
//     dead zone, not a parameter, which arguments may alias; not a const; not
//     a loop head's let, whose copy for the next iteration holds the same
//     string; and not at the top level, whose bindings a module exports.
//   - y is a local, an upvalue or a string literal, which runs no code: x's
//     old value is read first, and code run by y could append to it, which
//     the old value must not show.
//   - nothing in the function that declares x mentions eval, whose code
//     reads x and is compiled apart. Nor is the code a debugger's, which
//     reads any binding.
//
// An owned append is x's read, y, the addition and the store as ever, the
// read with no str_freeze and the store marked (B of set_upvalue). A tier that
// ignores the mark makes a rope, which is always right: the string in the
// binding is an ordinary one at every moment.
//
// A local is simpler, and its right side may be anything:
//
//	function row(x) {
//		var out = "<td>"
//		out += esc(x)
//		out += "</td>"
//		return out
//	}
//
// esc runs between out's read and the addition, and what it does to out is
// what would make growing it wrong: the old value is on the stack, and must
// not change. But esc cannot reach a local no function mentions. So a local
// append (localAppend) asks that of the name, and that the right side's own
// text does not mention it, and it is safe whatever the right side runs. It
// is an add marked bytecode.AddOwned, which fuses with nothing before it, for
// any right side to come before: the tree tier's updates and fused additions
// know nothing of one. The interpreter reads no mark, and makes a rope.
//
// What a sum must not pay for: every read of a binding appended to has a
// str_freeze, which would stand between its read and what fuses with it, and
// sum += f(x) is as common as anything in JavaScript. So a local must have
// been given a string the source can see (stringly), as var out = "" does.
//
// An x.length is no read at all (compiledRead).

// enclosingBinding is the binding a name reaches in a function around c, and
// the function: the one resolveUpvalue finds.
func (c *compiler) enclosingBinding(name string) (*localVar, *compiler) {
	for p := c.parent; p != nil; p = p.parent {
		if l, ok := p.resolveLocal(name); ok {
			return l, p
		}
	}
	return nil, nil
}

// compiledRead notes a read of l, just compiled, and freezes what it read if l
// is appended to. x.length is no read of a string: it keeps nothing, and a
// string runs no code to answer it, so the object of one (lengthRead) is
// neither noted nor frozen. while (s.length < n) s += x stays an append.
func (c *compiler) compiledRead(l *localVar) {
	if c.lengthRead {
		return
	}
	l.read = true
	if l.appended {
		c.emit(bytecode.OpArgumentsIndex, bytecode.StrFreezeA, 0)
	}
}

// evalInside reports whether c's function, or one in it, mentions eval.
func (c *compiler) evalInside() bool {
	if c.mentionsEval == 0 {
		c.mentionsEval = 1
		if referencesName(c.lit, "eval") {
			c.mentionsEval = 2
		}
	}
	return c.mentionsEval == 2
}

// plainOperand reports whether evaluating e runs no code but the read: a string
// literal, or a name bound in a function, which no getter answers.
func (c *compiler) plainOperand(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.StringLit:
		return true
	case *ast.Ident:
		if c.withLimit(v.Name) > 0 {
			return false
		}
		if _, ok := c.resolveLocal(v.Name); ok {
			return true
		}
		_, ok := c.resolveUpvalue(v.Name)
		return ok
	}
	return false
}

// ownedAppend reports whether e is an addition to a binding that may grow
// what the binding holds in place, and notes that it does.
func (c *compiler) ownedAppend(e ast.Expr) bool {
	a, ok := e.(*ast.Assign)
	if !ok || a.Op != "+=" || c.opts.Debug {
		return false
	}
	id, ok := a.Target.(*ast.Ident)
	if !ok || c.withLimit(id.Name) > 0 {
		return false
	}
	if _, local := c.resolveLocal(id.Name); local || !c.plainOperand(a.Value) {
		return false
	}
	idx, ok := c.resolveUpvalue(id.Name)
	if !ok {
		return false
	}
	if up := c.fn.Upvalues[idx]; up.TDZ || up.FuncSelf || !up.Mutable {
		return false
	}
	l, owner := c.enclosingBinding(id.Name)
	if l == nil || owner.lit == nil || owner.evalInside() {
		return false
	}
	if l.kind != bindVar && l.kind != bindLet || l.copied || l.paramScoped || l.read && !l.appended {
		return false
	}
	l.appended = true
	return true
}

// stringly reports whether e is a string by what the source shows: a string
// literal, a template, or an addition with one of those on a side.
func stringly(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.StringLit:
		return true
	case *ast.TemplateLit:
		return true
	case *ast.Binary:
		return v.Op == "+" && (stringly(v.Left) || stringly(v.Right))
	}
	return false
}

// noteString notes on the local an assignment gives a string that it has.
func (c *compiler) noteString(n *ast.Assign) {
	if n.Op != "=" || !stringly(n.Value) {
		return
	}
	if id, ok := n.Target.(*ast.Ident); ok {
		if l, ok := c.resolveLocal(id.Name); ok {
			l.stringy = true
		}
	}
}

// mentionedInside reports whether a function or a class inside c's function
// mentions name, or an eval could.
func (c *compiler) mentionedInside(name string) bool {
	if seen, ok := c.mentioned[name]; ok {
		return seen
	}
	w := &argumentsScanner{seekName: name, nestedOnly: true}
	for _, p := range c.lit.Params {
		w.expr(p)
	}
	w.stmts(c.lit.Body)
	if c.mentioned == nil {
		c.mentioned = map[string]bool{}
	}
	c.mentioned[name] = w.found
	return w.found
}

// localAppend reports whether e is an addition to a local that may grow what
// the local holds in place, and notes that it does.
func (c *compiler) localAppend(e ast.Expr) bool {
	a, ok := e.(*ast.Assign)
	if !ok || a.Op != "+=" || c.opts.Debug || c.lit == nil {
		return false
	}
	id, ok := a.Target.(*ast.Ident)
	if !ok || c.withLimit(id.Name) > 0 {
		return false
	}
	l, ok := c.resolveLocal(id.Name)
	if !ok || !l.stringy || !l.initialized || l.kind != bindVar && l.kind != bindLet || l.copied || l.paramScoped ||
		l.read && !l.appended || c.evalInside() || c.mentionedInside(id.Name) {
		return false
	}
	w := &argumentsScanner{seekName: id.Name}
	if w.expr(a.Value); w.found {
		return false
	}
	l.appended = true
	return true
}

// compileLocalAppend compiles an addition localAppend took.
func (c *compiler) compileLocalAppend(n *ast.Assign) {
	l, _ := c.resolveLocal(n.Target.(*ast.Ident).Name)
	c.emit(bytecode.OpGetLocal, l.slot, 0)
	c.compileExpr(n.Value)
	// Not by emit, which would fuse it with a local or a constant before it,
	// into an instruction with no room for the mark.
	c.recordLine(n.Start)
	c.fn.Code = append(c.fn.Code, bytecode.Instr{Op: bytecode.OpAdd, B: bytecode.AddOwned})
	c.adjustStack(bytecode.OpAdd, 0, bytecode.AddOwned)
	c.emit(bytecode.OpSetLocal, l.slot, 0)
}

// compileOwnedAppend compiles an addition ownedAppend took.
func (c *compiler) compileOwnedAppend(n *ast.Assign) {
	idx, _ := c.resolveUpvalue(n.Target.(*ast.Ident).Name)
	c.emit(bytecode.OpGetUpvalue, idx, 0)
	c.compileExpr(n.Value)
	c.emitAt(n.Start, compoundOpcode(n.Op), 0, 0)
	c.emit(bytecode.OpSetUpvalue, idx, 1)
}

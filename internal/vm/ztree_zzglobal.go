package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// The top level of a script, a module or eval code in the tree tier.
//
// Top-level code runs once, so the tier builds it only where it has a loop
// (see buildTree), which is where a tree is faster than the interpreter:
// the same loop over the same globals ran two and a half times as fast in a
// function, as a tree, as at the top level. What top-level code has that a
// function does not is its prologue, the declarations of the names it binds
// in the global object, the global lexical environment or the module's,
// and their initializations. Each is a node that does what executeAt's
// case does, eval code's vars going where the code that called eval keeps
// them.
//
// Its functions are variables, each assigned a closure in init, rather
// than functions or methods: Go lays out all of a package's functions
// before any of its closures, so code added here as functions would sit
// between the interpreter's hot functions and the tree nodes' hot
// closures, and move one against the other, where a closure is laid out
// after every closure already there (see AGENTS.md). The zz sorts it after
// the tier's other files but ztree_zzimport.go and ztree_zzusing.go, whose
// closures it moves.

var (
	// treeBuildsGlobal reports whether the tier builds op, one of top-level
	// code's declarations and initializations.
	treeBuildsGlobal func(op bytecode.Op) bool

	// globalOp builds the instructions treeBuildsGlobal names.
	globalOp func(b *tbuilder, pc int, in bytecode.Instr) (int, bool)
)

func init() {
	treeBuildsGlobal = func(op bytecode.Op) bool {
		switch op {
		case bytecode.OpCheckGlobalVar, bytecode.OpCheckGlobalLex,
			bytecode.OpDefineGlobalVar, bytecode.OpDefineGlobalFunc,
			bytecode.OpDeclareGlobalLex, bytecode.OpInitGlobalLex,
			bytecode.OpDeclareModuleLex, bytecode.OpInitModuleLex:
			return true
		}
		return false
	}

	globalOp = func(b *tbuilder, pc int, in bytecode.Instr) (int, bool) {
		a, k := in.A, in.B
		switch in.Op {
		case bytecode.OpCheckGlobalVar:
			return 0, b.stmt(func(c *tctx) {
				r, name := c.r, c.names[a]
				if c.cl.scope() != r.global {
					return
				}
				c.at(pc)
				if err := r.checkGlobalVarName(name); err != nil {
					c.throw(err)
				}
				if k != 0 {
					if err := r.canDeclareGlobalFunc(name); err != nil {
						c.throw(err)
					}
				} else if !r.hasOwnProp(r.global, name) && !r.global.IsExtensible() {
					c.throw(r.throwTypeError("cannot declare %q on a non-extensible global", r.atoms.name(name)))
				}
			})
		case bytecode.OpCheckGlobalLex:
			return 0, b.stmt(func(c *tctx) {
				r, name := c.r, c.names[a]
				if r.globalLex.getOwn(name).ok() {
					c.at(pc)
					c.throw(r.throwError(errSyntax, "%q has already been declared", r.atoms.name(name)))
				}
				// A lexical binding shadows the global object's property of
				// the same name for good, so one that cannot be deleted may
				// not be shadowed.
				if p := r.global.getOwn(name); p.ok() && p.flags()&propConfigurable == 0 {
					c.at(pc)
					c.throw(r.throwError(errSyntax,
						"%q is already a property of the global object that cannot be removed", r.atoms.name(name)))
				}
			})
		case bytecode.OpDefineGlobalVar:
			return 0, b.stmt(func(c *tctx) {
				r, name := c.r, c.names[a]
				if ev := c.f.evalVars; ev != nil {
					// The evaluated code is inside a function, so what it
					// declares belongs to that function rather than to the
					// global object.
					if !evalVarProp(ev, name).ok() {
						ev.setOwnRaw(name, Undefined, propWritable|propConfigurable)
					}
					return
				}
				c.at(pc)
				env := c.cl.scope()
				if env == r.global {
					if err := r.checkGlobalVarName(name); err != nil {
						c.throw(err)
					}
				}
				// A binding eval creates is configurable, where a script's
				// is not.
				flags := propWritable | moduleBindingFlags(r.atoms.name(name))
				if k != 0 {
					flags |= propConfigurable
				}
				if proxyOf(env) != nil {
					// A context's global: the var is its sandbox's.
					if err := r.declareOnScope(env, name, Undefined, flags, false); err != nil {
						c.throw(err)
					}
					return
				}
				if !r.hasOwnProp(env, name) {
					if env == r.global && !env.IsExtensible() {
						c.throw(r.throwTypeError("cannot declare %q on a non-extensible global", r.atoms.name(name)))
					}
					env.setOwnRaw(name, Undefined, flags)
				}
			})
		case bytecode.OpDefineGlobalFunc:
			v := b.pop().tree()
			return 0, b.stmt(func(c *tctx) {
				fn := v(c)
				r, name := c.r, c.names[a]
				if ev := c.f.evalVars; ev != nil {
					ev.setOwnRaw(name, fn, propWritable|propConfigurable)
					return
				}
				c.at(pc)
				flags := propWritable | moduleBindingFlags(r.atoms.name(name))
				if k != 0 {
					flags |= propConfigurable
				}
				env := c.cl.scope()
				if proxyOf(env) != nil {
					if err := r.declareOnScope(env, name, fn, flags, true); err != nil {
						c.throw(err)
					}
					return
				}
				if env == r.global {
					if err := r.checkGlobalVarName(name); err != nil {
						c.throw(err)
					}
					if err := r.canDeclareGlobalFunc(name); err != nil {
						c.throw(err)
					}
					if p := env.getOwn(name); p.ok() && p.flags()&(propConfigurable|propAccessor) == 0 {
						// A property that cannot be redefined is updated in
						// place, keeping the attributes it was given.
						p.setValue(fn)
						return
					}
				}
				env.setOwnRaw(name, fn, flags)
			})
		case bytecode.OpDeclareGlobalLex:
			flags := propFlags(propConfigurable)
			if k != 0 {
				flags |= propWritable
			}
			return 0, b.stmt(func(c *tctx) { c.r.declareGlobalLex(c.names[a], flags) })
		case bytecode.OpInitGlobalLex:
			v := b.pop().tree()
			return 0, b.stmt(func(c *tctx) {
				x := v(c)
				r, name := c.r, c.names[a]
				r.globalLex.setOwnRaw(name, x, r.globalLex.getOwn(name).flags())
			})
		case bytecode.OpDeclareModuleLex:
			return 0, b.stmt(func(c *tctx) {
				name := c.names[a]
				flags := propUninit | moduleBindingFlags(c.r.atoms.name(name))
				if k != 0 {
					flags |= propWritable
				}
				c.cl.scope().setOwnRaw(name, uninitialized, flags)
			})
		case bytecode.OpInitModuleLex:
			v := b.pop().tree()
			return 0, b.stmt(func(c *tctx) {
				x := v(c)
				name := c.names[a]
				if p := c.cl.scope().getOwn(name); p.ok() {
					// Its attributes change, so its layout does.
					p.setValue(x)
					p.setFlags(p.flags() &^ propUninit)
				}
			})
		default:
			return 0, false
		}
	}
}

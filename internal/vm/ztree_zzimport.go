package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// import() and import.meta in the tree tier.
//
// A function that loads a module with import(), or reads its module's
// import.meta, ran in the interpreter, loops and all. Each is now a node
// that does what executeAt's case does: import() hands importCall its
// specifier and options and pushes the promise it returns, the module to
// resolve against found from the frames, a tree's among them; import.meta
// is the module environment's.
//
// Its functions are variables, each assigned a closure in init, rather
// than functions or methods: Go lays out all of a package's functions
// before any of its closures, so code added here as functions would sit
// between the interpreter's hot functions and the tree nodes' hot
// closures, and move one against the other, where a closure is laid out
// after every closure already there (see AGENTS.md). The zz sorts it after
// the tier's other files but ztree_zzusing.go, whose closures it moves.

var (
	// treeBuildsImport reports whether the tier builds op, import or
	// import_meta, or one treeBuildsGlobal names.
	treeBuildsImport func(op bytecode.Op) bool

	// importOp builds import and import_meta, and hands globalOp what
	// treeBuildsGlobal names.
	importOp func(b *tbuilder, pc int, in bytecode.Instr) (int, bool)
)

func init() {
	treeBuildsImport = func(op bytecode.Op) bool {
		return op == bytecode.OpImport || op == bytecode.OpImportMeta || treeBuildsGlobal(op)
	}

	importOp = func(b *tbuilder, pc int, in bytecode.Instr) (int, bool) {
		switch in.Op {
		case bytecode.OpImportMeta:
			b.push(func(c *tctx) Value { return c.r.importMeta(c.cl.env) })
		case bytecode.OpImport:
			// The specifier and, with B 2, the options; what it does runs
			// code -- the specifier's toString, the options' getters -- so
			// it is a statement, and the promise is written to its slot.
			n, phase := int(in.B), in.A
			if n < 1 || n > 2 || !b.spill() || b.depth() < n {
				return 0, false
			}
			d := b.depth()
			for range n {
				b.pop()
			}
			at := d - n
			b.body = append(b.body, func(c *tctx) {
				spec, options := c.stack[at], Undefined
				if n > 1 {
					options = c.stack[at+1]
				}
				c.at(pc)
				c.stack[at] = c.r.importCall(phase, spec, options)
			})
			b.pushSlot(at)
		default:
			return globalOp(b, pc, in)
		}
		return 0, true
	}
}

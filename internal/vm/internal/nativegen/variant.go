package nativegen

import (
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// variant is a native dispatcher of an architecture: a table over the
// handlers, which a runtime picks through the native context's table
// pointer (ctx.table, R13 in the code). plain is the base: every handler,
// the entry and the exit stub, in the handlers' block, and its table. Any
// other variant overrides the handlers that differ, in a block of its own,
// and its table points at those and at plain's for every other operation,
// so that a handler is generated once and the binary holds no copy of a
// shared one.
type variant struct {
	// name says what the variant is for.
	name string
	// sym is the variant's part of its symbols' names, "" for plain's:
	// regNative<sym>Handlers, its own handlers' block; regNative<sym>Table,
	// its table; native<sym>TableAddr, the table's address for Go.
	sym string
	// build is the build constraint of the variant's file; plain's table
	// is in the handlers' file, which every build has. A variant's file is
	// never in the build that reads the layout (layoutTag), which must not
	// need what it generates.
	build string
	// tags are the build tags of the build the variant is made from the
	// layout of: its constraint's.
	tags []string
	// overrides are the operations whose handlers the variant has of its
	// own; an emitter's body for one tells the variant's (nativeGen.v).
	overrides []bytecode.ROp
}

// jitBuild is the JIT's build constraint (internal/vm's zzjit_*.go).
const jitBuild = "quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))"

// variants are the dispatchers, plain first.
//
//   - plain is the only one in builds without the JIT, and the one a
//     runtime that does not run the JIT uses in builds with it.
//   - feedback is the dispatcher of a runtime that runs the JIT, in builds
//     with it alone: its handlers that record the JIT's feedback OR the
//     kinds of value they met into the function's feedback slot (ctx.fv),
//     every other handler plain's. None records yet.
//
// A mode's table -- the debugger's -- would be one more.
var variants = []*variant{
	{name: "plain"},
	{name: "feedback", sym: "Feedback", build: jitBuild + " && !" + layoutTag, tags: []string{"quickjs_jit"}},
}

// plain is the base variant.
var plain = variants[0]

// has reports whether v's own block has op's handler: plain's has every
// handler, another variant's its overrides.
func (v *variant) has(op bytecode.ROp) bool {
	return v == plain || slices.Contains(v.overrides, op)
}

// A variant's table has, after the operations' entries, slots for what
// its own handlers jump to in plain's block, which a jump from one block
// to another cannot reach by a displacement the block's bytes hold: the
// exit stub and the exit to nativeRun (exitRet). A jump to another of
// plain's handlers goes through that operation's own entry.
const (
	slotExit = 256 + iota
	slotExitRet
	variantSlots
)

// tableSize is the size in bytes of v's table.
func (v *variant) tableSize() int {
	if v == plain {
		return 8 * 256
	}
	return 8 * variantSlots
}

// Slot is a table entry: an offset in the variant's own block, or in
// plain's.
type Slot struct {
	Own bool
	Off int
}

// table is v's table, plain's block p and v's own b.
func (v *variant) table(p, b *block) []Slot {
	if v == plain {
		t := make([]Slot, 256)
		for op := range t {
			t[op] = Slot{Off: p.entry(bytecode.ROp(op))}
		}
		return t
	}
	t := make([]Slot, variantSlots)
	for op := range 256 {
		if o, ok := b.at[bytecode.ROp(op)]; ok {
			t[op] = Slot{Own: true, Off: o}
		} else {
			t[op] = Slot{Off: p.entry(bytecode.ROp(op))}
		}
	}
	t[slotExit] = Slot{Off: p.exit}
	t[slotExitRet] = Slot{Off: p.exitRet}
	return t
}

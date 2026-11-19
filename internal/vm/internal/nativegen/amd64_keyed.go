package nativegen

import (
	"fmt"

	. "github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
)

// keyedStore is set_index's store of a key string, and define_index's,
// by the site's keyed store cache (the VM's reg_table_keyed.go, its
// keyedStore): the object in RSI, the key's window offset in R8 and its
// number word in R9. The key is a string whose entry in the runtime's
// keyAtoms names an atom, no index's (keyFind), as Go's stringKey finds
// it; the site is Aux[pc]'s, as instanceof's; its entry and then its
// poly's are compared with the object's shape and the key (keyedEntry):
// an own slot's is written as set_prop writes one (storeValue), a
// transition's added as set_prop adds one (addTransition), which checks
// what the addition asks of the object and its prototypes and exits
// where the table has no room. Anything else exits, before anything is
// written, to the operation's Go handler, which stores the long way and
// fills the cache. The C operand is the value.
func (g *nativeGen) keyedStore() {
	a := g.a
	poly, loop, next, own, trans := a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()
	// The key, its atom in EAX.
	a.MovRR(RCX, R9)
	a.Op(Xor, RCX, R10, true)
	a.ShiftImm(Shr, RCX, 16, true)
	a.Jcc(CondNE, g.exit)
	a.MovZX8(RCX, R9)
	a.OpImm(Cmp, RCX, int32(KindString), false)
	a.Jcc(CondNE, g.exit)
	a.LoadIdx(R9, R12, R8, 1, 8)
	g.keySet(RCX, R9, RDX)
	g.keyFind(RCX, R9, g.exit)
	a.LoadU8(RDX, RCX, offKANone)
	a.Op(Test, RDX, RDX, false)
	a.Jcc(CondNE, g.exit)
	a.LoadU32(RAX, RCX, offKAAtom)
	a.Op(Test, RAX, RAX, false)
	a.Jcc(CondS, g.exit)
	// The site, Aux[pc]: &ic[Aux[pc]], in RCX.
	a.Load(R8, RDI, ncRC)
	a.Load(R8, R8, offRCAux)
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, g.exit)
	a.MovRR(RCX, RBX)
	a.Load(R9, RDI, ncCodeBase)
	a.Op(Sub, RCX, R9, true)
	a.ShiftImm(Shr, RCX, 1, true)
	a.Op(Add, R8, RCX, true)
	a.LoadU32(RCX, R8, 0)
	a.Op(Test, RCX, RCX, false)
	a.Jcc(CondE, g.exit)
	a.MovImm(R8, uint64(size("propCache")))
	a.Imul(RCX, R8)
	a.Op(Add, RCX, R15, true)
	// The object's shape, in RDX.
	a.Load(RDX, RSI, off("Object.shape"))
	a.Op(Test, RDX, RDX, true)
	a.Jcc(CondE, g.exit)
	g.keyedEntry(own, trans, poly)
	// The poly's entries, their count left in X1.
	a.Bind(poly)
	a.Load(R8, RCX, off("propCache.poly"))
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, g.exit)
	a.LoadU8(R9, R8, off("propPoly.n"))
	a.Op(Test, R9, R9, false)
	a.Jcc(CondE, g.exit)
	a.MovQToX(1, R9)
	a.AddMem(R8, RDI, ncPolyEnt)
	a.MovRR(RCX, R8)
	a.Bind(loop)
	g.keyedEntry(own, trans, next)
	a.Bind(next)
	a.OpImm(Add, RCX, size("propCache"), true)
	a.MovQFromX(R9, 1)
	a.OpImm(Sub, R9, 1, false)
	a.MovQToX(1, R9)
	a.Jcc(CondNE, loop)
	a.Jmp(g.exit)
	// An own slot: the value at idx.
	// The instruction word, which the atom took the place of, read again.
	a.Bind(own)
	a.Load(RAX, RBX, 0)
	g.decC(R8)
	a.LoadIdx(R9, R12, R8, 1, 0)
	a.LoadIdx(R8, R12, R8, 1, 8)
	a.LoadS32(RAX, RCX, off("propCache.idx"))
	a.ShiftImm(Shl, RAX, 4, true)
	a.AddMem(RAX, RSI, off("Object.valp"))
	g.storeValue()
	// A transition: the value at the address in RDX added.
	a.Bind(trans)
	a.Load(RAX, RBX, 0)
	g.decC(RDX)
	a.LeaIdx(RDX, R12, RDX, 1, 0)
	g.addTransition(g.exit)
}

// keyedEntry goes to own where the keyed site's entry at RCX is of the
// object's shape, in RDX, and is an own slot whose key, the shape's at
// its slot, is the atom in EAX; to trans where it is of the shape and is
// a transition whose next shape's key is that atom; and to miss
// otherwise. R8 and R9 are its scratch.
func (g *nativeGen) keyedEntry(own, trans, miss Label) {
	a := g.a
	ownKey := a.NewLabel()
	if s := size("propKey"); s != 1 && s != 2 && s != 4 && s != 8 {
		panic(fmt.Sprintf("nativegen: a propKey of %d bytes, no index's scale", s))
	}
	a.CmpMem(RDX, RCX, off("propCache.shape"), true)
	a.Jcc(CondNE, miss)
	a.Load(R8, RCX, off("propCache.next"))
	a.Op(Test, R8, R8, true)
	a.Jcc(CondE, ownKey)
	a.CmpMem(R8, RDI, ncSetterNx, true)
	a.Jcc(CondE, miss)
	a.CmpMem(RAX, R8, off("shape.key"), false)
	a.Jcc(CondE, trans)
	a.Jmp(miss)
	a.Bind(ownKey)
	a.LoadS32(R9, RCX, off("propCache.idx"))
	a.Load(R8, RDX, off("shape.keys"))
	a.LeaIdx(R8, R8, R9, uint8(size("propKey")), 0)
	a.CmpMem(RAX, R8, off("propKey.key"), false)
	a.Jcc(CondE, own)
	a.Jmp(miss)
}

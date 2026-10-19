//go:build quickjs_jit && !android && !ios && (linux || windows) && amd64

package jit

import (
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
)

// callGoProgram is native code that calls Go n times through callGo, adding
// each call's GoStatus to Ret, and returns.
func callGoProgram(n int) ([]byte, error) {
	a := &amd64.Asm{}
	a.MovImm(amd64.RAX, 0)
	a.Store(amd64.RDI, abi.OffRet, amd64.RAX)
	for range n {
		back := a.NewLabel()
		a.LeaLabel(amd64.RAX, back)
		a.Store(amd64.RDI, abi.OffGoResume, amd64.RAX)
		a.MovImm(amd64.RAX, CallGo())
		a.JmpReg(amd64.RAX)
		a.Bind(back)
		a.Load(amd64.RAX, amd64.RDI, abi.OffRet)
		a.Load(amd64.RCX, amd64.RDI, abi.OffGoStatus)
		a.Op(amd64.Add, amd64.RAX, amd64.RCX, true)
		a.Store(amd64.RDI, abi.OffRet, amd64.RAX)
	}
	a.Ret()
	return a.Finish()
}

// storeRefProgram is native code that moves storeRefPairs pointers, each
// from the word whose address is GoArgs[2k+1].Num to the one at
// GoArgs[2k].Num, through storeRef, clearing the first, and returns; raw,
// it stores them itself, as no code may while the collector marks.
func storeRefProgram(raw bool) ([]byte, error) {
	a := &amd64.Asm{}
	store := func() {
		if raw {
			a.Store(amd64.RAX, 0, amd64.RCX)
			return
		}
		back := a.NewLabel()
		a.LeaLabel(amd64.R11, back)
		a.MovImm(amd64.R10, StoreRef())
		a.JmpReg(amd64.R10)
		a.Bind(back)
	}
	for k := range int32(storeRefPairs) {
		dst, src := abi.OffGoArgs+32*k, abi.OffGoArgs+32*k+16
		a.Load(amd64.R8, amd64.RDI, src)
		a.Load(amd64.RCX, amd64.R8, 0)
		a.Load(amd64.RAX, amd64.RDI, dst)
		store()
		a.Load(amd64.RAX, amd64.RDI, src)
		a.MovImm(amd64.RCX, 0)
		store()
	}
	a.Ret()
	return a.Finish()
}

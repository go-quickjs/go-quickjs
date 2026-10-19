//go:build quickjs_jit && !android && !ios && darwin && arm64

package jit

import (
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
)

// callGoProgram is native code that calls Go n times through callGo, adding
// each call's GoStatus to Ret, and returns.
func callGoProgram(n int) ([]byte, error) {
	a := &arm64.Asm{}
	ctx, x, y := arm64.Reg(0), arm64.Reg(15), arm64.Reg(16)
	a.MovImm(x, 0)
	a.Store(ctx, abi.OffRet, x)
	for range n {
		back := a.NewLabel()
		a.Adr(x, back)
		a.Store(ctx, abi.OffGoResume, x)
		a.MovImm(x, CallGo())
		a.Br(x)
		a.Bind(back)
		a.Load(x, ctx, abi.OffRet)
		a.Load(y, ctx, abi.OffGoStatus)
		a.Op(arm64.Add, x, x, y, true)
		a.Store(ctx, abi.OffRet, x)
	}
	a.Ret()
	return a.Finish()
}

// storeRefProgram is amd64's.
func storeRefProgram(raw bool) ([]byte, error) {
	a := &arm64.Asm{}
	ctx, slot, p, back, x := arm64.Reg(0), arm64.Reg(15), arm64.Reg(17), arm64.Reg(16), arm64.Reg(9)
	store := func() {
		if raw {
			a.Store(slot, 0, p)
			return
		}
		l := a.NewLabel()
		a.Adr(back, l)
		a.MovImm(x, StoreRef())
		a.Br(x)
		a.Bind(l)
	}
	for k := range int32(storeRefPairs) {
		dst, src := abi.OffGoArgs+32*k, abi.OffGoArgs+32*k+16
		a.Load(x, ctx, src)
		a.Load(p, x, 0)
		a.Load(slot, ctx, dst)
		store()
		a.Load(slot, ctx, src)
		a.MovImm(p, 0)
		store()
	}
	a.Ret()
	return a.Finish()
}

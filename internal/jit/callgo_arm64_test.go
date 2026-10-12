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

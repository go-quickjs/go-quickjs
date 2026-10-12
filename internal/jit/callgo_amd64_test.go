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

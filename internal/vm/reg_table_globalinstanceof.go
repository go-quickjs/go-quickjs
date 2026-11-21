package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

func init() {
	regTable[bytecode.RGetGlobalInstanceOf] = rhGetGlobal
	regSlowTable[bytecode.RGetGlobalInstanceOf] = rsGetGlobal
}

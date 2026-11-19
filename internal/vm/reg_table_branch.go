package vm

import "github.com/go-quickjs/go-quickjs/internal/bytecode"

// The branches fused with their test. This file sorts before
// reg_table_cold.go, so that the layout lock's K pad takes up what it adds.
//
// instanceof_next's Go handler is instanceof's: it stores the boolean the
// native handler branches on without, and goes on at the partner, which
// branches on it (bytecode.RInstanceOfNext).
func init() {
	regTable[bytecode.RInstanceOfNext] = rsInstanceOf
}

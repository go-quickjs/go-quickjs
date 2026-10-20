//go:build quickjs_jit && !android && !ios && (linux || windows)

#include "funcdata.h"
#include "textflag.h"

// jitBigFast is where native code jumps for a BigInt's operator Go makes
// (abi.Encoding's BigFast), as it jumps to jit's callGo: its context in
// DI, where the call goes on (GoResume) in the context. To the Go runtime
// it is, as callGo is, a function the Go function that entered the code
// called. It calls jitBigLeaf, and goes on where the leaf says, or, where
// the leaf made nothing, to callGo, as native code would have, the stack
// and DI as it found them.
TEXT ·jitBigFast(SB), NOSPLIT|NOFRAME, $0-0
	ADJSP $24
	NO_LOCAL_POINTERS
	MOVQ DI, 16(SP)
	MOVQ DI, 0(SP)
	CALL ·jitBigLeaf(SB)
	MOVQ 8(SP), AX
	MOVQ 16(SP), DI
	ADJSP $-24
	TESTQ AX, AX
	JZ slow
	JMP AX
slow:
	MOVQ ·jitCallGoAddr(SB), AX
	JMP AX

// jitBigFastAddr is jitBigFast's address.
TEXT ·jitBigFastAddr(SB), NOSPLIT, $0-8
	LEAQ ·jitBigFast(SB), AX
	MOVQ AX, ret+0(FP)
	RET

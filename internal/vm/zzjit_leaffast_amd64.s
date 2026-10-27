//go:build quickjs_jit && !android && !ios && (linux || windows)

#include "funcdata.h"
#include "textflag.h"

// jitLeafFast is where native code jumps for a leaf's call (abi.Encoding's
// LeafFast), as it jumps to jit's callGo: its context in DI, the call's
// description in CX, where the call goes on in AX. To the Go runtime it
// is, as callGo is, a function the Go function that entered the code
// called. It calls jitLeafGo and goes where it says, the result's number
// word in R11 and DI as it found it.
TEXT ·jitLeafFast(SB), NOSPLIT|NOFRAME, $0-0
	ADJSP $48
	NO_LOCAL_POINTERS
	MOVQ DI, 40(SP)
	MOVQ DI, 0(SP)
	MOVQ CX, 8(SP)
	MOVQ AX, 16(SP)
	CALL ·jitLeafGo(SB)
	MOVQ 24(SP), AX
	MOVQ 32(SP), R11
	MOVQ 40(SP), DI
	ADJSP $-48
	JMP AX

// jitLeafFastAddr is jitLeafFast's address.
TEXT ·jitLeafFastAddr(SB), NOSPLIT, $0-8
	LEAQ ·jitLeafFast(SB), AX
	MOVQ AX, ret+0(FP)
	RET

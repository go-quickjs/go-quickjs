//go:build quickjs_jit && !android && !ios && darwin

#include "funcdata.h"
#include "textflag.h"

// jitBigFast is amd64's: native code's context in R0, SP and the link
// register as the Go function that entered the code left them, as jit's
// callGo takes them.
TEXT ·jitBigFast(SB), NOSPLIT|NOFRAME, $0-0
	SUB $32, RSP
	NO_LOCAL_POINTERS
	MOVD R30, 0(RSP)
	MOVD R0, 24(RSP)
	MOVD R0, 8(RSP)
	BL ·jitBigLeaf(SB)
	MOVD 16(RSP), R16
	MOVD 24(RSP), R0
	MOVD 0(RSP), R30
	ADD $32, RSP
	CBZ R16, slow
	JMP (R16)
slow:
	MOVD ·jitCallGoAddr(SB), R16
	JMP (R16)

// jitBigFastAddr is jitBigFast's address.
TEXT ·jitBigFastAddr(SB), NOSPLIT, $0-8
	MOVD $·jitBigFast(SB), R0
	MOVD R0, ret+0(FP)
	RET

//go:build quickjs_jit && !android && !ios && darwin

#include "funcdata.h"
#include "textflag.h"

// jitLeafFast is amd64's: native code's context in R0, the call's
// description in R17, where the call goes on in R15, SP and the link
// register as the Go function that entered the code left them, as jit's
// callGo takes them; the result's number word comes back in R16.
TEXT ·jitLeafFast(SB), NOSPLIT|NOFRAME, $0-0
	SUB $64, RSP
	NO_LOCAL_POINTERS
	MOVD R30, 0(RSP)
	MOVD R0, 48(RSP)
	MOVD R0, 8(RSP)
	MOVD R17, 16(RSP)
	MOVD R15, 24(RSP)
	BL ·jitLeafGo(SB)
	MOVD 32(RSP), R15
	MOVD 40(RSP), R16
	MOVD 48(RSP), R0
	MOVD 0(RSP), R30
	ADD $64, RSP
	JMP (R15)

// jitLeafFastAddr is jitLeafFast's address.
TEXT ·jitLeafFastAddr(SB), NOSPLIT, $0-8
	MOVD $·jitLeafFast(SB), R0
	MOVD R0, ret+0(FP)
	RET

// jitGoFast is amd64's: the context in R0, the description in R17, where
// the call goes on in R15.
TEXT ·jitGoFast(SB), NOSPLIT|NOFRAME, $0-0
	SUB $48, RSP
	NO_LOCAL_POINTERS
	MOVD R30, 0(RSP)
	MOVD R0, 40(RSP)
	MOVD R0, 8(RSP)
	MOVD R17, 16(RSP)
	MOVD R15, 24(RSP)
	BL ·jitGoDirect(SB)
	MOVD 32(RSP), R15
	MOVD 40(RSP), R0
	MOVD 0(RSP), R30
	ADD $48, RSP
	JMP (R15)

// jitGoFastAddr is jitGoFast's address.
TEXT ·jitGoFastAddr(SB), NOSPLIT, $0-8
	MOVD $·jitGoFast(SB), R0
	MOVD R0, ret+0(FP)
	RET

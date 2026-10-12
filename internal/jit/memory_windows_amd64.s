//go:build quickjs_jit

#include "textflag.h"

TEXT ·foreignBytes(SB), NOSPLIT|NOFRAME, $0-40
	MOVQ address+0(FP), AX
	MOVQ size+8(FP), CX
	MOVQ AX, ret+16(FP)
	MOVQ CX, ret_len+24(FP)
	MOVQ CX, ret_cap+32(FP)
	RET

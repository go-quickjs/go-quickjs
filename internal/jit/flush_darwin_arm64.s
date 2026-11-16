//go:build quickjs_jit && !android && !ios

#include "textflag.h"

// Darwin's data caches are coherent and its instruction maintenance granule
// is 64 bytes. Reading CTR_EL0 traps on Apple Silicon. A barrier after each
// invalidation also covers CPUs requiring periodic barriers during a sweep.
TEXT ·flushInstructionCache(SB), NOSPLIT|NOFRAME, $0-16
	MOVD start+0(FP), R0
	MOVD size+8(FP), R1
	CBZ R1, done
	ADD R0, R1, R1
	BIC $63, R0, R0
	DSB $11
instruction:
	WORD $0xd50b7520 // ic ivau, x0
	DSB $11
	ADD $64, R0, R0
	CMP R1, R0
	BLO instruction
	ISB $15
done:
	RET

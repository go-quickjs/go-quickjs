//go:build quickjs_jit && !android && !ios

#include "textflag.h"

// flushInstructionCache publishes code just written to instruction fetch
// on Linux, as libgcc's __clear_cache does: CTR_EL0, which Linux lets user
// space read, gives the smallest data and instruction cache lines and
// whether the cleaning (IDC) or the invalidation (DIC) is needed at all.
// Each data line is cleaned to the point of unification, a barrier, each
// instruction line invalidated, a barrier, and the pipeline flushed. Unlike
// Apple's, other implementations need not have coherent data caches or
// 64-byte lines: a Neoverse's lines may differ, and a core may have both
// bits clear.
TEXT ·flushInstructionCache(SB), NOSPLIT|NOFRAME, $0-16
	MOVD start+0(FP), R0
	MOVD size+8(FP), R1
	CBZ R1, done
	ADD R0, R1, R1
	MRS CTR_EL0, R2
	TBNZ $28, R2, cleaned
	// The data lines: 4 << DminLine bytes.
	UBFX $16, R2, $4, R3
	MOVD $4, R4
	LSL R3, R4, R3
	SUB $1, R3, R5
	BIC R5, R0, R6
data:
	WORD $0xd50b7b26 // dc cvau, x6
	ADD R3, R6, R6
	CMP R1, R6
	BLO data
cleaned:
	DSB $11
	TBNZ $29, R2, invalidated
	// The instruction lines: 4 << IminLine bytes.
	AND $15, R2, R3
	MOVD $4, R4
	LSL R3, R4, R3
	SUB $1, R3, R5
	BIC R5, R0, R6
instruction:
	WORD $0xd50b7526 // ic ivau, x6
	ADD R3, R6, R6
	CMP R1, R6
	BLO instruction
	DSB $11
invalidated:
	ISB $15
done:
	RET

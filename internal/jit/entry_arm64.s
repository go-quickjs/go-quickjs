//go:build quickjs_jit && !android && !ios && darwin

#include "funcdata.h"
#include "textflag.h"

// enterSSA tail-jumps to a function compiled by the new pipeline, with the
// context block in R0. The code returns through the link register directly
// to enterSSA's Go caller.
TEXT ·enterSSA(SB), NOSPLIT|NOFRAME, $0-16
	MOVD code+0(FP), R16
	MOVD ctx+8(FP), R0
	JMP (R16)

// callGo is where native code calls Go, as amd64's is: native code jumps
// here, its context in R0, SP and the link register as the Go function that
// entered it left them -- native code touches neither -- so that, to the Go
// runtime, this is a function that one called, its return address saved
// where a frame's is.
TEXT ·callGo(SB), NOSPLIT|NOFRAME, $0-0
	SUB $32, RSP
	NO_LOCAL_POINTERS
	MOVD R30, 0(RSP)
	MOVD R0, 24(RSP)
	MOVD R0, 8(RSP)
	BL ·goCall(SB)
	MOVD 16(RSP), R16
	MOVD 24(RSP), R0
	MOVD 0(RSP), R30
	ADD $32, RSP
	JMP (R16)

// callGoAddr is callGo's address (abi.Encoding's CallGo).
TEXT ·callGoAddr(SB), NOSPLIT, $0-8
	MOVD $·callGo(SB), R0
	MOVD R0, ret+0(FP)
	RET

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

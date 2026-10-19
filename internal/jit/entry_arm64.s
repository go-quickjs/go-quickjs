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

// storeRef is amd64's: the word's address in R15, the pointer in R17,
// where to go on in R16, its context in R0, which it gives back, with the
// link register; nothing else.
TEXT ·storeRef(SB), NOSPLIT|NOFRAME, $0-0
	SUB $48, RSP
	NO_LOCAL_POINTERS
	MOVD R30, 0(RSP)
	MOVD R15, 8(RSP)
	MOVD R17, 16(RSP)
	MOVD R0, 24(RSP)
	MOVD R16, 32(RSP)
	BL ·storeRefGo(SB)
	MOVD 32(RSP), R16
	MOVD 24(RSP), R0
	MOVD 0(RSP), R30
	ADD $48, RSP
	JMP (R16)

// storeRefAddr is storeRef's address.
TEXT ·storeRefAddr(SB), NOSPLIT, $0-8
	MOVD $·storeRef(SB), R0
	MOVD R0, ret+0(FP)
	RET

// storeRefClobber is amd64's: junk in every register native code may keep
// but R0 and the link register, then storeRef.
TEXT ·storeRefClobber(SB), NOSPLIT|NOFRAME, $0-0
	MOVD $0x7ff5a5a5a5a5a5a5, R1
	MOVD R1, R2
	MOVD R1, R3
	MOVD R1, R4
	MOVD R1, R5
	MOVD R1, R6
	MOVD R1, R7
	MOVD R1, R8
	MOVD R1, R9
	MOVD R1, R10
	MOVD R1, R11
	MOVD R1, R12
	MOVD R1, R13
	MOVD R1, R14
	MOVD R1, R19
	MOVD R1, R20
	MOVD R1, R21
	MOVD R1, R22
	MOVD R1, R23
	MOVD R1, R24
	MOVD R1, R25
	FMOVD R1, F0
	FMOVD R1, F1
	FMOVD R1, F2
	FMOVD R1, F3
	FMOVD R1, F4
	FMOVD R1, F5
	FMOVD R1, F6
	FMOVD R1, F7
	FMOVD R1, F8
	FMOVD R1, F9
	FMOVD R1, F10
	FMOVD R1, F11
	FMOVD R1, F12
	FMOVD R1, F13
	FMOVD R1, F14
	FMOVD R1, F15
	FMOVD R1, F16
	FMOVD R1, F17
	FMOVD R1, F18
	FMOVD R1, F19
	FMOVD R1, F20
	FMOVD R1, F21
	FMOVD R1, F22
	FMOVD R1, F23
	FMOVD R1, F24
	FMOVD R1, F25
	FMOVD R1, F26
	FMOVD R1, F27
	FMOVD R1, F28
	FMOVD R1, F29
	FMOVD R1, F30
	FMOVD R1, F31
	JMP ·storeRef(SB)

// storeRefClobberAddr is storeRefClobber's address.
TEXT ·storeRefClobberAddr(SB), NOSPLIT, $0-8
	MOVD $·storeRefClobber(SB), R0
	MOVD R0, ret+0(FP)
	RET

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

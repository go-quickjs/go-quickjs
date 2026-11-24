//go:build quickjs_jit && !android && !ios && (darwin || linux)

#include "funcdata.h"
#include "textflag.h"

// wasmJITAlloc is amd64's: native code jumps here with its context in R0
// and where to go on in R16, SP and the link register as the Go function
// that entered it left them, so that, to the Go runtime, this is a function
// that one called, its return address saved where a frame's is; the
// frame's top word, below the SP it is entered with, is left alone, where
// that function keeps its caller's frame pointer. It goes on at R16 with
// jitAllocStub's result in R17, R0 and the link register as they were.
TEXT ·wasmJITAlloc(SB), NOSPLIT|NOFRAME, $0-0
	SUB $48, RSP
	NO_LOCAL_POINTERS
	MOVD R30, 0(RSP)
	MOVD R0, 8(RSP)
	MOVD R0, 24(RSP)
	MOVD R16, 32(RSP)
	BL ·jitAllocStub(SB)
	MOVD 16(RSP), R17
	MOVD 32(RSP), R16
	MOVD 24(RSP), R0
	MOVD 0(RSP), R30
	ADD $48, RSP
	JMP (R16)

// wasmJITAllocClobber is wasmJITAlloc with junk left in every register
// native code may keep but R0, R16, R17 and the link register, for tests.
TEXT ·wasmJITAllocClobber(SB), NOSPLIT|NOFRAME, $0-0
	SUB $48, RSP
	NO_LOCAL_POINTERS
	MOVD R30, 0(RSP)
	MOVD R0, 8(RSP)
	MOVD R0, 24(RSP)
	MOVD R16, 32(RSP)
	BL ·jitAllocStub(SB)
	MOVD 16(RSP), R17
	MOVD 32(RSP), R16
	MOVD 24(RSP), R0
	MOVD 0(RSP), R30
	ADD $48, RSP
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
	MOVD R1, R15
	MOVD R1, R19
	MOVD R1, R20
	MOVD R1, R21
	MOVD R1, R22
	MOVD R1, R23
	MOVD R1, R24
	MOVD R1, R25
	MOVD R1, R26
	MOVD R1, R27
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
	JMP (R16)

TEXT ·wasmJITAllocAddr(SB), NOSPLIT, $0-8
	MOVD $·wasmJITAlloc(SB), R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·wasmJITAllocClobberAddr(SB), NOSPLIT, $0-8
	MOVD $·wasmJITAllocClobber(SB), R0
	MOVD R0, ret+0(FP)
	RET

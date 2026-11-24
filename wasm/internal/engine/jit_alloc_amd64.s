//go:build quickjs_jit && !android && !ios && (linux || windows)

#include "funcdata.h"
#include "textflag.h"

// wasmJITAlloc is the allocation stub (jit_alloc.go): native code jumps
// here with its context in DI and where to go on in R11, the stack as the
// Go function that entered the code left it -- native code never moves SP
// -- so that, to the Go runtime, this is a function that one called. It
// calls jitAllocStub and goes on at R11 with its result in AX, DI as it
// was.
TEXT ·wasmJITAlloc(SB), NOSPLIT|NOFRAME, $0-0
	ADJSP $32
	NO_LOCAL_POINTERS
	MOVQ DI, 0(SP)
	MOVQ DI, 16(SP)
	MOVQ R11, 24(SP)
	CALL ·jitAllocStub(SB)
	MOVQ 8(SP), AX
	MOVQ 16(SP), DI
	MOVQ 24(SP), R11
	ADJSP $-32
	JMP R11

// wasmJITAllocClobber is wasmJITAlloc with junk left in every register
// native code may keep but DI, R11 and AX, for tests.
TEXT ·wasmJITAllocClobber(SB), NOSPLIT|NOFRAME, $0-0
	ADJSP $32
	NO_LOCAL_POINTERS
	MOVQ DI, 0(SP)
	MOVQ DI, 16(SP)
	MOVQ R11, 24(SP)
	CALL ·jitAllocStub(SB)
	MOVQ 8(SP), AX
	MOVQ 16(SP), DI
	MOVQ 24(SP), R11
	ADJSP $-32
	MOVQ $0x7ff5a5a5a5a5a5a5, BX
	MOVQ BX, CX
	MOVQ BX, DX
	MOVQ BX, SI
	MOVQ BX, R8
	MOVQ BX, R9
	MOVQ BX, R10
	MOVQ BX, R12
	MOVQ BX, R13
	MOVQ BX, R15
	MOVQ BX, X0
	MOVQ BX, X1
	MOVQ BX, X2
	MOVQ BX, X3
	MOVQ BX, X4
	MOVQ BX, X5
	MOVQ BX, X6
	MOVQ BX, X7
	MOVQ BX, X8
	MOVQ BX, X9
	MOVQ BX, X10
	MOVQ BX, X11
	MOVQ BX, X12
	MOVQ BX, X13
	MOVQ BX, X14
	JMP R11

TEXT ·wasmJITAllocAddr(SB), NOSPLIT, $0-8
	LEAQ ·wasmJITAlloc(SB), AX
	MOVQ AX, ret+0(FP)
	RET

TEXT ·wasmJITAllocClobberAddr(SB), NOSPLIT, $0-8
	LEAQ ·wasmJITAllocClobber(SB), AX
	MOVQ AX, ret+0(FP)
	RET

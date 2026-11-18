#include "go_asm.h"
#include "funcdata.h"
#include "textflag.h"

// The native handlers' entry and their call of Go (native.go).

// wasmNativeEnter enters the native code at code with the context in DI.
// The code returns from it once the frame has returned.
TEXT ·wasmNativeEnter(SB), NOSPLIT|NOFRAME, $0-16
	MOVQ code+0(FP), AX
	MOVQ ctx+8(FP), DI
	JMP AX

// wasmNativeCallH calls the Go function whose func value is in DX for
// native code, which jumps here: its arguments in AX, BX, CX and SI, the
// context in DI, the stack as wasmNativeEnter's caller left it, so that to
// the Go runtime this is a function that one called. The function's
// arguments' spill room is the frame's first 32 bytes. It goes on at the
// context's after, the result in AX and the context in DI.
TEXT ·wasmNativeCallH(SB), NOSPLIT|NOFRAME, $0-0
	ADJSP $40
	NO_LOCAL_POINTERS
	MOVQ DI, 32(SP)
	MOVQ SI, DI
	MOVQ TLS, R14
	MOVQ 0(R14)(TLS*1), R14
	XORPS X15, X15
	CALL 0(DX)
	MOVQ 32(SP), DI
	ADJSP $-40
	JMP nativeCtx_after(DI)

TEXT ·wasmNativeCallHAddr(SB), NOSPLIT, $0-8
	LEAQ ·wasmNativeCallH(SB), AX
	MOVQ AX, ret+0(FP)
	RET

TEXT ·wasmNativeHandlersAddr(SB), NOSPLIT, $0-8
	LEAQ ·wasmNativeHandlers(SB), AX
	MOVQ AX, ret+0(FP)
	RET

TEXT ·wasmNativeTableAddr(SB), NOSPLIT, $0-8
	LEAQ ·wasmNativeTable(SB), AX
	MOVQ AX, ret+0(FP)
	RET

TEXT ·wasmNativeTablePtr(SB), NOSPLIT, $0-8
	LEAQ ·wasmNativeTable(SB), AX
	MOVQ AX, ret+0(FP)
	RET

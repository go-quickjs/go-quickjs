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

// wasmNativeStoreRef stores the pointer in R9 at the address in R8 with
// Go's write barrier, for native code, which jumps here while the collector
// marks with its return address in AX: every general register but AX, R8
// and R9 is as it was. As wasmNativeCallH's, its frame is one the function
// that entered the code called.
TEXT ·wasmNativeStoreRef(SB), NOSPLIT|NOFRAME, $0-0
	ADJSP $112
	NO_LOCAL_POINTERS
	MOVQ R8, 0(SP)
	MOVQ R9, 8(SP)
	MOVQ AX, 16(SP)
	MOVQ BX, 24(SP)
	MOVQ CX, 32(SP)
	MOVQ DX, 40(SP)
	MOVQ SI, 48(SP)
	MOVQ DI, 56(SP)
	MOVQ R10, 64(SP)
	MOVQ R11, 72(SP)
	MOVQ R12, 80(SP)
	MOVQ R13, 88(SP)
	CALL ·nativeStoreRefGo(SB)
	MOVQ 16(SP), AX
	MOVQ 24(SP), BX
	MOVQ 32(SP), CX
	MOVQ 40(SP), DX
	MOVQ 48(SP), SI
	MOVQ 56(SP), DI
	MOVQ 64(SP), R10
	MOVQ 72(SP), R11
	MOVQ 80(SP), R12
	MOVQ 88(SP), R13
	ADJSP $-112
	JMP AX

TEXT ·wasmNativeStoreRefAddr(SB), NOSPLIT, $0-8
	LEAQ ·wasmNativeStoreRef(SB), AX
	MOVQ AX, ret+0(FP)
	RET

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

TEXT ·wasmNativeExtTableAddr(SB), NOSPLIT, $0-8
	LEAQ ·wasmNativeExtTable(SB), AX
	MOVQ AX, ret+0(FP)
	RET

TEXT ·wasmNativeExtTablePtr(SB), NOSPLIT, $0-8
	LEAQ ·wasmNativeExtTable(SB), AX
	MOVQ AX, ret+0(FP)
	RET

TEXT ·wasmNativeSIMDTablePtr(SB), NOSPLIT, $0-8
	LEAQ ·wasmNativeSIMDTable(SB), AX
	MOVQ AX, ret+0(FP)
	RET

TEXT ·wasmNativeTablePtr(SB), NOSPLIT, $0-8
	LEAQ ·wasmNativeTable(SB), AX
	MOVQ AX, ret+0(FP)
	RET

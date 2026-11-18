#include "go_asm.h"
#include "funcdata.h"
#include "textflag.h"

// The native handlers' entry, and their store of a pointer with the write
// barrier (zzzyn_native_amd64.go): the JIT's enterRaw and storeRef, the
// package's own, so that the handlers need no JIT.

TEXT ·nativeEnter(SB), NOSPLIT|NOFRAME, $0-16
	MOVQ code+0(FP), AX
	MOVQ ctx+8(FP), DI
	JMP AX

TEXT ·nativeStoreRef(SB), NOSPLIT|NOFRAME, $0-0
	ADJSP $40
	NO_LOCAL_POINTERS
	MOVQ AX, 0(SP)
	MOVQ CX, 8(SP)
	MOVQ DI, 16(SP)
	MOVQ R11, 24(SP)
	CALL ·nativeStoreRefGo(SB)
	MOVQ 16(SP), DI
	MOVQ 24(SP), AX
	ADJSP $-40
	JMP AX

TEXT ·nativeStoreRefAddr(SB), NOSPLIT, $0-8
	LEAQ ·nativeStoreRef(SB), AX
	MOVQ AX, ret+0(FP)
	RET

TEXT ·nativeHandlersAddr(SB), NOSPLIT, $0-8
	LEAQ ·regNativeHandlers(SB), AX
	MOVQ AX, ret+0(FP)
	RET

TEXT ·nativeTableAddr(SB), NOSPLIT, $0-8
	LEAQ ·regNativeTable(SB), AX
	MOVQ AX, ret+0(FP)
	RET

// nativeCallH calls the Go handler whose func value is in DX for native
// code, which jumps here: its arguments in AX, BX, CX and SI, the context
// in DI, the stack as nativeEnter's caller left it, so that to the Go
// runtime this is a function that one called, as the JIT's callGo is. The
// handler's arguments' spill room is the frame's first 32 bytes. It goes
// on at the context's after, the result in AX and the context in DI.
TEXT ·nativeCallH(SB), NOSPLIT|NOFRAME, $0-0
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

TEXT ·nativeCallHAddr(SB), NOSPLIT, $0-8
	LEAQ ·nativeCallH(SB), AX
	MOVQ AX, ret+0(FP)
	RET

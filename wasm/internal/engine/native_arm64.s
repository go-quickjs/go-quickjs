#include "go_asm.h"
#include "funcdata.h"
#include "textflag.h"

// The native handlers' entry, their call of Go and their store of a pointer
// with the write barrier on arm64 (native.go). The code never touches R18,
// R28 (g), R29 (the frame pointer) or R30, which holds wasmNativeEnter's
// return to nativeRun from the entry on: wasmNativeCallH and
// wasmNativeStoreRef are, to the Go runtime, functions nativeRun called,
// their return address saved where a frame's is. Neither writes the word
// below the stack pointer it is entered with, which holds nativeRun's
// caller's frame pointer (Go's arm64 frames save it at -8(RSP)).

// wasmNativeEnter enters the native code at code with the context in R25.
// The code returns from it, by R30, once the frame has returned.
TEXT ·wasmNativeEnter(SB), NOSPLIT|NOFRAME, $0-16
	MOVD code+0(FP), R16
	MOVD ctx+8(FP), R25
	JMP (R16)

// wasmNativeCallH calls the Go function whose func value is in R26 for
// native code, which jumps here: its arguments in R0 to R3, by Go's
// internal convention (ABIInternal), the context in R25. The function's
// arguments' spill room is 8(RSP) to 40(RSP), the context is kept at
// 48(RSP), and 56(RSP) is left alone (above). It goes on at the context's
// after, the result in R0 and the context in R25.
TEXT ·wasmNativeCallH(SB), NOSPLIT|NOFRAME, $0-0
	SUB $64, RSP
	NO_LOCAL_POINTERS
	MOVD R30, 0(RSP)
	MOVD R25, 48(RSP)
	MOVD 0(R26), R16
	CALL (R16)
	MOVD 48(RSP), R25
	MOVD 0(RSP), R30
	ADD $64, RSP
	MOVD nativeCtx_after(R25), R16
	JMP (R16)

// wasmNativeStoreRef stores the pointer in R5 at the address in R4 with
// Go's write barrier, for native code, which jumps here while the collector
// marks with its way back in R16: every general register native code uses
// but R6 and R16 is as it was.
TEXT ·wasmNativeStoreRef(SB), NOSPLIT|NOFRAME, $0-0
	SUB $160, RSP
	NO_LOCAL_POINTERS
	MOVD R30, 0(RSP)
	MOVD R4, 8(RSP)
	MOVD R5, 16(RSP)
	MOVD R0, 24(RSP)
	MOVD R1, 32(RSP)
	MOVD R2, 40(RSP)
	MOVD R3, 48(RSP)
	MOVD R4, 56(RSP)
	MOVD R5, 64(RSP)
	MOVD R7, 72(RSP)
	MOVD R16, 80(RSP)
	MOVD R17, 88(RSP)
	MOVD R19, 96(RSP)
	MOVD R20, 104(RSP)
	MOVD R21, 112(RSP)
	MOVD R22, 120(RSP)
	MOVD R23, 128(RSP)
	MOVD R24, 136(RSP)
	MOVD R25, 144(RSP)
	BL ·nativeStoreRefGo(SB)
	MOVD 24(RSP), R0
	MOVD 32(RSP), R1
	MOVD 40(RSP), R2
	MOVD 48(RSP), R3
	MOVD 56(RSP), R4
	MOVD 64(RSP), R5
	MOVD 72(RSP), R7
	MOVD 80(RSP), R16
	MOVD 88(RSP), R17
	MOVD 96(RSP), R19
	MOVD 104(RSP), R20
	MOVD 112(RSP), R21
	MOVD 120(RSP), R22
	MOVD 128(RSP), R23
	MOVD 136(RSP), R24
	MOVD 144(RSP), R25
	MOVD 0(RSP), R30
	ADD $160, RSP
	JMP (R16)

// wasmNativeAlloc calls nativeAllocGo for native code, which jumps here
// with the context in R25, the request in R4 and its way back in R16: it
// leaves the answer in R4, every general register native code uses but
// R4, R6 and R16 as it was.
TEXT ·wasmNativeAlloc(SB), NOSPLIT|NOFRAME, $0-0
	SUB $160, RSP
	NO_LOCAL_POINTERS
	MOVD R30, 0(RSP)
	MOVD R25, 8(RSP)
	MOVD R4, 16(RSP)
	MOVD R0, 32(RSP)
	MOVD R1, 40(RSP)
	MOVD R2, 48(RSP)
	MOVD R3, 56(RSP)
	MOVD R5, 64(RSP)
	MOVD R7, 72(RSP)
	MOVD R16, 80(RSP)
	MOVD R17, 88(RSP)
	MOVD R19, 96(RSP)
	MOVD R20, 104(RSP)
	MOVD R21, 112(RSP)
	MOVD R22, 120(RSP)
	MOVD R23, 128(RSP)
	MOVD R24, 136(RSP)
	MOVD R25, 144(RSP)
	BL ·nativeAllocGo(SB)
	MOVD 24(RSP), R4
	MOVD 32(RSP), R0
	MOVD 40(RSP), R1
	MOVD 48(RSP), R2
	MOVD 56(RSP), R3
	MOVD 64(RSP), R5
	MOVD 72(RSP), R7
	MOVD 80(RSP), R16
	MOVD 88(RSP), R17
	MOVD 96(RSP), R19
	MOVD 104(RSP), R20
	MOVD 112(RSP), R21
	MOVD 120(RSP), R22
	MOVD 128(RSP), R23
	MOVD 136(RSP), R24
	MOVD 144(RSP), R25
	MOVD 0(RSP), R30
	ADD $160, RSP
	JMP (R16)

TEXT ·wasmNativeAllocAddr(SB), NOSPLIT, $0-8
	MOVD $·wasmNativeAlloc(SB), R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·wasmNativeStoreRefAddr(SB), NOSPLIT, $0-8
	MOVD $·wasmNativeStoreRef(SB), R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·wasmNativeCallHAddr(SB), NOSPLIT, $0-8
	MOVD $·wasmNativeCallH(SB), R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·wasmNativeHandlersAddr(SB), NOSPLIT, $0-8
	MOVD $·wasmNativeHandlers(SB), R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·wasmNativeTableAddr(SB), NOSPLIT, $0-8
	MOVD $·wasmNativeTable(SB), R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·wasmNativeExtTableAddr(SB), NOSPLIT, $0-8
	MOVD $·wasmNativeExtTable(SB), R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·wasmNativeExtTablePtr(SB), NOSPLIT, $0-8
	MOVD $·wasmNativeExtTable(SB), R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·wasmNativeSIMDTablePtr(SB), NOSPLIT, $0-8
	MOVD $·wasmNativeSIMDTable(SB), R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·wasmNativeTablePtr(SB), NOSPLIT, $0-8
	MOVD $·wasmNativeTable(SB), R0
	MOVD R0, ret+0(FP)
	RET

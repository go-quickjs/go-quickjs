#include "go_asm.h"
#include "funcdata.h"
#include "textflag.h"

// The native handlers' entry, their store of a pointer with the write
// barrier and their call of a Go handler on arm64 (zzzyn_native_run.go),
// amd64's in arm64's convention. The code never touches R28 (g), R29 (the
// frame pointer) or R30, which holds nativeEnter's return to nativeRun
// from the entry on: nativeStoreRef and nativeCallH are functions
// nativeRun called, to the Go runtime, as the JIT's callGo is, their
// return address saved where a frame's is. Neither writes the word below
// the stack pointer they are entered with, which holds nativeRun's
// caller's frame pointer (Go's arm64 frames save it at -8(RSP)).

TEXT ·nativeEnter(SB), NOSPLIT|NOFRAME, $0-16
	MOVD code+0(FP), R16
	MOVD ctx+8(FP), R25
	JMP (R16)

// nativeStoreRef stores the pointer in R1 at the slot in R0 by
// nativeStoreRefGo, with the write barrier, and goes on at R16, the
// context in R25 kept.
TEXT ·nativeStoreRef(SB), NOSPLIT|NOFRAME, $0-0
	SUB $48, RSP
	NO_LOCAL_POINTERS
	MOVD R30, 0(RSP)
	MOVD R0, 8(RSP)
	MOVD R1, 16(RSP)
	MOVD R25, 24(RSP)
	MOVD R16, 32(RSP)
	BL ·nativeStoreRefGo(SB)
	MOVD 32(RSP), R16
	MOVD 24(RSP), R25
	MOVD 0(RSP), R30
	ADD $48, RSP
	JMP (R16)

TEXT ·nativeStoreRefAddr(SB), NOSPLIT, $0-8
	MOVD $·nativeStoreRef(SB), R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·nativeHandlersAddr(SB), NOSPLIT, $0-8
	MOVD $·regNativeHandlers(SB), R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·nativeTableAddr(SB), NOSPLIT, $0-8
	MOVD $·regNativeTable(SB), R0
	MOVD R0, ret+0(FP)
	RET

// nativeCallH calls the Go handler whose func value is in R26 for native
// code, which jumps here: its arguments in R0 to R3, by Go's internal
// convention (ABIInternal), the context in R25. The handler's arguments'
// spill room is 8(RSP) to 40(RSP), the context is kept at 48(RSP), and
// 56(RSP) is left alone (above). It goes on at the context's after, the
// result in R0 and the context in R25.
TEXT ·nativeCallH(SB), NOSPLIT|NOFRAME, $0-0
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

TEXT ·nativeCallHAddr(SB), NOSPLIT, $0-8
	MOVD $·nativeCallH(SB), R0
	MOVD R0, ret+0(FP)
	RET

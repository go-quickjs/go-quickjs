//go:build quickjs_jit && !android && !ios && (linux || windows)

#include "funcdata.h"
#include "textflag.h"

// enterSSA tail-jumps to a function compiled by the new pipeline, with the
// context block in DI. The code returns directly to enterSSA's Go caller.
TEXT ·enterSSA(SB), NOSPLIT|NOFRAME, $0-16
	MOVQ code+0(FP), AX
	MOVQ ctx+8(FP), DI
	JMP AX

// callGo is where native code calls Go (abi.Context's GoOp). Native code
// jumps here, its context in DI, the stack as the Go function that entered
// it left it -- native code never moves SP -- so that, to the Go runtime,
// this is a function that one called: its frame and the return address
// above it are all a traceback, a stack scan or the stack's growth meets.
// It calls goCall, which may do whatever Go does, and goes on in the code
// where goCall says, its frame given back first.
TEXT ·callGo(SB), NOSPLIT|NOFRAME, $0-0
	ADJSP $24
	NO_LOCAL_POINTERS
	MOVQ DI, 16(SP)
	MOVQ DI, 0(SP)
	CALL ·goCall(SB)
	MOVQ 8(SP), AX
	MOVQ 16(SP), DI
	ADJSP $-24
	JMP AX

// storeRef is where native code stores a pointer word while the collector
// marks (abi.Encoding's StoreRef): the word's address in AX, the pointer in
// CX, where to go on in R11, its context in DI. It is a function that the
// Go function that entered the code called, as callGo is, calling
// storeRefGo, which stores with the write barrier; it gives back DI, and
// nothing else: its caller saved what it keeps.
TEXT ·storeRef(SB), NOSPLIT|NOFRAME, $0-0
	ADJSP $40
	NO_LOCAL_POINTERS
	MOVQ AX, 0(SP)
	MOVQ CX, 8(SP)
	MOVQ DI, 16(SP)
	MOVQ R11, 24(SP)
	CALL ·storeRefGo(SB)
	MOVQ 16(SP), DI
	MOVQ 24(SP), AX
	ADJSP $-40
	JMP AX

// storeRefAddr is storeRef's address.
TEXT ·storeRefAddr(SB), NOSPLIT, $0-8
	LEAQ ·storeRef(SB), AX
	MOVQ AX, ret+0(FP)
	RET

// storeRefClobber is storeRef with junk in every register native code may
// keep but DI first, which a test gives native code (StoreRefClobber) to see
// that it saves what it keeps across a store.
TEXT ·storeRefClobber(SB), NOSPLIT|NOFRAME, $0-0
	MOVQ $0x7ff5a5a5a5a5a5a5, BX
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
	JMP ·storeRef(SB)

// storeRefClobberAddr is storeRefClobber's address.
TEXT ·storeRefClobberAddr(SB), NOSPLIT, $0-8
	LEAQ ·storeRefClobber(SB), AX
	MOVQ AX, ret+0(FP)
	RET

// callGoAddr is callGo's address (abi.Encoding's CallGo).
TEXT ·callGoAddr(SB), NOSPLIT, $0-8
	LEAQ ·callGo(SB), AX
	MOVQ AX, ret+0(FP)
	RET

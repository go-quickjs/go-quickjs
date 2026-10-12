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

// callGoAddr is callGo's address (abi.Encoding's CallGo).
TEXT ·callGoAddr(SB), NOSPLIT, $0-8
	LEAQ ·callGo(SB), AX
	MOVQ AX, ret+0(FP)
	RET

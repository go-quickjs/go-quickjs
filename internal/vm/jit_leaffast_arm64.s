//go:build quickjs_jit && !android && !ios && (darwin || linux)

#include "funcdata.h"
#include "textflag.h"

// jitLeafFast is amd64's: native code's context in R0, the call's
// description in R17, where the call goes on in R15, SP and the link
// register as the Go function that entered the code left them, as jit's
// callGo takes them; the result's number word comes back in R16.
TEXT ·jitLeafFast(SB), NOSPLIT|NOFRAME, $0-0
	SUB $64, RSP
	NO_LOCAL_POINTERS
	MOVD R30, 0(RSP)
	MOVD R0, 48(RSP)
	MOVD R0, 8(RSP)
	MOVD R17, 16(RSP)
	MOVD R15, 24(RSP)
	BL ·jitLeafGo(SB)
	MOVD 32(RSP), R15
	MOVD 40(RSP), R16
	MOVD 48(RSP), R0
	MOVD 0(RSP), R30
	ADD $64, RSP
	JMP (R15)

// jitLeafFastAddr is jitLeafFast's address.
TEXT ·jitLeafFastAddr(SB), NOSPLIT, $0-8
	MOVD $·jitLeafFast(SB), R0
	MOVD R0, ret+0(FP)
	RET

// jitGoFast is amd64's: the context in R0, the description in R17, where
// the call goes on in R15.
TEXT ·jitGoFast(SB), NOSPLIT|NOFRAME, $0-0
	SUB $48, RSP
	NO_LOCAL_POINTERS
	MOVD R30, 0(RSP)
	MOVD R0, 40(RSP)
	MOVD R0, 8(RSP)
	MOVD R17, 16(RSP)
	MOVD R15, 24(RSP)
	BL ·jitGoDirect(SB)
	MOVD 32(RSP), R15
	MOVD 40(RSP), R0
	MOVD 0(RSP), R30
	ADD $48, RSP
	JMP (R15)

// jitGoFastAddr is jitGoFast's address.
TEXT ·jitGoFastAddr(SB), NOSPLIT, $0-8
	MOVD $·jitGoFast(SB), R0
	MOVD R0, ret+0(FP)
	RET

// MATH_STUB is amd64's: the operands in F0 and F1, where the call goes on
// in R15, the context in R0, SP and the link register as the Go function
// that entered the code left them; the result comes back in F0.
#define MATH_STUB(name, fn) \
TEXT name(SB), NOSPLIT|NOFRAME, $0-0; \
	SUB $64, RSP; \
	NO_LOCAL_POINTERS; \
	MOVD R30, 0(RSP); \
	MOVD R0, 48(RSP); \
	MOVD R15, 40(RSP); \
	MOVD R0, 8(RSP); \
	FMOVD F0, 16(RSP); \
	FMOVD F1, 24(RSP); \
	BL fn(SB); \
	FMOVD 32(RSP), F0; \
	MOVD 40(RSP), R15; \
	MOVD 48(RSP), R0; \
	MOVD 0(RSP), R30; \
	ADD $64, RSP; \
	JMP (R15)

MATH_STUB(·jitPowStub, ·jitPowGo)
MATH_STUB(·jitModStub, ·jitModGo)
MATH_STUB(·jitAtan2Stub, ·jitAtan2Go)
MATH_STUB(·jitHypotStub, ·jitHypotGo)

// jitMathStub is amd64's: which function in R17.
TEXT ·jitMathStub(SB), NOSPLIT|NOFRAME, $0-0
	SUB $64, RSP
	NO_LOCAL_POINTERS
	MOVD R30, 0(RSP)
	MOVD R0, 48(RSP)
	MOVD R15, 40(RSP)
	MOVD R0, 8(RSP)
	FMOVD F0, 16(RSP)
	MOVD R17, 24(RSP)
	BL ·jitMathGo(SB)
	FMOVD 32(RSP), F0
	MOVD 40(RSP), R15
	MOVD 48(RSP), R0
	MOVD 0(RSP), R30
	ADD $64, RSP
	JMP (R15)

// STUB_ADDR is a routine's address.
#define STUB_ADDR(name, stub) \
TEXT name(SB), NOSPLIT, $0-8; \
	MOVD $stub(SB), R0; \
	MOVD R0, ret+0(FP); \
	RET

STUB_ADDR(·jitPowStubAddr, ·jitPowStub)
STUB_ADDR(·jitModStubAddr, ·jitModStub)
STUB_ADDR(·jitAtan2StubAddr, ·jitAtan2Stub)
STUB_ADDR(·jitHypotStubAddr, ·jitHypotStub)
STUB_ADDR(·jitMathStubAddr, ·jitMathStub)

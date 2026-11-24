//go:build quickjs_jit && !android && !ios && (linux || windows)

#include "funcdata.h"
#include "textflag.h"

// jitLeafFast is where native code jumps for a leaf's call (abi.Encoding's
// LeafFast), as it jumps to jit's callGo: its context in DI, the call's
// description in CX, where the call goes on in AX. To the Go runtime it
// is, as callGo is, a function the Go function that entered the code
// called. It calls jitLeafGo and goes where it says, the result's number
// word in R11 and DI as it found it.
TEXT ·jitLeafFast(SB), NOSPLIT|NOFRAME, $0-0
	ADJSP $48
	NO_LOCAL_POINTERS
	MOVQ DI, 40(SP)
	MOVQ DI, 0(SP)
	MOVQ CX, 8(SP)
	MOVQ AX, 16(SP)
	CALL ·jitLeafGo(SB)
	MOVQ 24(SP), AX
	MOVQ 32(SP), R11
	MOVQ 40(SP), DI
	ADJSP $-48
	JMP AX

// jitLeafFastAddr is jitLeafFast's address.
TEXT ·jitLeafFastAddr(SB), NOSPLIT, $0-8
	LEAQ ·jitLeafFast(SB), AX
	MOVQ AX, ret+0(FP)
	RET

// jitGoFast is where native code jumps for any other call of Go whose
// site saves its live registers (abi.Encoding's GoFast): as jitLeafFast,
// the call's description in CX and where it goes on in AX. It calls
// jitGoDirect and goes where it says, DI as it found it.
TEXT ·jitGoFast(SB), NOSPLIT|NOFRAME, $0-0
	ADJSP $40
	NO_LOCAL_POINTERS
	MOVQ DI, 32(SP)
	MOVQ DI, 0(SP)
	MOVQ CX, 8(SP)
	MOVQ AX, 16(SP)
	CALL ·jitGoDirect(SB)
	MOVQ 24(SP), AX
	MOVQ 32(SP), DI
	ADJSP $-40
	JMP AX

// jitGoFastAddr is jitGoFast's address.
TEXT ·jitGoFastAddr(SB), NOSPLIT, $0-8
	LEAQ ·jitGoFast(SB), AX
	MOVQ AX, ret+0(FP)
	RET

// MATH_STUB is the routine of a Math function's call of Go
// (abi.Encoding's GoStubs, abi.GoStub): its operands in X0 and X1, where
// the call goes on in AX, its context in DI. To the Go runtime it is, as
// jitLeafFast is, a function the Go function that entered the code called.
// It calls fn and goes back, the result in X0 and DI as it found it.
#define MATH_STUB(name, fn) \
TEXT name(SB), NOSPLIT|NOFRAME, $0-0; \
	ADJSP $48; \
	NO_LOCAL_POINTERS; \
	MOVQ DI, 40(SP); \
	MOVQ AX, 32(SP); \
	MOVQ DI, 0(SP); \
	MOVSD X0, 8(SP); \
	MOVSD X1, 16(SP); \
	CALL fn(SB); \
	MOVSD 24(SP), X0; \
	MOVQ 32(SP), AX; \
	MOVQ 40(SP), DI; \
	ADJSP $-48; \
	JMP AX

MATH_STUB(·jitPowStub, ·jitPowGo)
MATH_STUB(·jitModStub, ·jitModGo)
MATH_STUB(·jitAtan2Stub, ·jitAtan2Go)
MATH_STUB(·jitHypotStub, ·jitHypotGo)

// jitMathStub is MATH_STUB's for Math's function of one (abi.GoStubMath),
// which function in CX.
TEXT ·jitMathStub(SB), NOSPLIT|NOFRAME, $0-0
	ADJSP $48
	NO_LOCAL_POINTERS
	MOVQ DI, 40(SP)
	MOVQ AX, 32(SP)
	MOVQ DI, 0(SP)
	MOVSD X0, 8(SP)
	MOVQ CX, 16(SP)
	CALL ·jitMathGo(SB)
	MOVSD 24(SP), X0
	MOVQ 32(SP), AX
	MOVQ 40(SP), DI
	ADJSP $-48
	JMP AX

// GO_STUB is the routine of another call of Go with one of its own
// (abi.GoStub): as jitGoFast, the call's description in CX and where it
// goes on in AX, its operands in the context's GoArgs. It calls fn, the
// operation's own Go, and goes where fn says, DI as it found it.
#define GO_STUB(name, fn) \
TEXT name(SB), NOSPLIT|NOFRAME, $0-0; \
	ADJSP $40; \
	NO_LOCAL_POINTERS; \
	MOVQ DI, 32(SP); \
	MOVQ DI, 0(SP); \
	MOVQ CX, 8(SP); \
	MOVQ AX, 16(SP); \
	CALL fn(SB); \
	MOVQ 24(SP), AX; \
	MOVQ 32(SP), DI; \
	ADJSP $-40; \
	JMP AX

GO_STUB(·jitAppendStub, ·jitAppendGo)
GO_STUB(·jitAddStub, ·jitAddGo)
GO_STUB(·jitConcatStub, ·jitConcatGo)

// STUB_ADDR is a routine's address.
#define STUB_ADDR(name, stub) \
TEXT name(SB), NOSPLIT, $0-8; \
	LEAQ stub(SB), AX; \
	MOVQ AX, ret+0(FP); \
	RET

STUB_ADDR(·jitPowStubAddr, ·jitPowStub)
STUB_ADDR(·jitModStubAddr, ·jitModStub)
STUB_ADDR(·jitAtan2StubAddr, ·jitAtan2Stub)
STUB_ADDR(·jitHypotStubAddr, ·jitHypotStub)
STUB_ADDR(·jitMathStubAddr, ·jitMathStub)
STUB_ADDR(·jitAppendStubAddr, ·jitAppendStub)
STUB_ADDR(·jitAddStubAddr, ·jitAddStub)
STUB_ADDR(·jitConcatStubAddr, ·jitConcatStub)

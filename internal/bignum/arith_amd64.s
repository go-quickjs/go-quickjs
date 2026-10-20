//go:build !purego

#include "textflag.h"

// The inner loops of bignum's arithmetic, on the carry flag the compiler's
// code moves through a register at each word: four words a turn, the carry
// kept in BX between turns (SBBQ, NEGQ), and the rest one at a time.

// func addVV(z, x, y []uint) (c uint)
TEXT ·addVV(SB), NOSPLIT, $0-80
	MOVQ z_len+8(FP), CX
	MOVQ z_base+0(FP), DI
	MOVQ x_base+24(FP), SI
	MOVQ y_base+48(FP), DX
	XORQ R9, R9
	XORQ BX, BX
	MOVQ CX, R10
	SHRQ $2, R10
	ANDQ $3, CX
	TESTQ R10, R10
	JZ   addone
addfour:
	NEGQ BX
	MOVQ 0(SI)(R9*8), AX
	ADCQ 0(DX)(R9*8), AX
	MOVQ AX, 0(DI)(R9*8)
	MOVQ 8(SI)(R9*8), AX
	ADCQ 8(DX)(R9*8), AX
	MOVQ AX, 8(DI)(R9*8)
	MOVQ 16(SI)(R9*8), AX
	ADCQ 16(DX)(R9*8), AX
	MOVQ AX, 16(DI)(R9*8)
	MOVQ 24(SI)(R9*8), AX
	ADCQ 24(DX)(R9*8), AX
	MOVQ AX, 24(DI)(R9*8)
	SBBQ BX, BX
	ADDQ $4, R9
	DECQ R10
	JNZ  addfour
addone:
	TESTQ CX, CX
	JZ   adddone
addloop:
	NEGQ BX
	MOVQ 0(SI)(R9*8), AX
	ADCQ 0(DX)(R9*8), AX
	MOVQ AX, 0(DI)(R9*8)
	SBBQ BX, BX
	INCQ R9
	DECQ CX
	JNZ  addloop
adddone:
	NEGQ BX
	MOVQ BX, c+72(FP)
	RET

// func subVV(z, x, y []uint) (b uint)
TEXT ·subVV(SB), NOSPLIT, $0-80
	MOVQ z_len+8(FP), CX
	MOVQ z_base+0(FP), DI
	MOVQ x_base+24(FP), SI
	MOVQ y_base+48(FP), DX
	XORQ R9, R9
	XORQ BX, BX
	MOVQ CX, R10
	SHRQ $2, R10
	ANDQ $3, CX
	TESTQ R10, R10
	JZ   subone
subfour:
	NEGQ BX
	MOVQ 0(SI)(R9*8), AX
	SBBQ 0(DX)(R9*8), AX
	MOVQ AX, 0(DI)(R9*8)
	MOVQ 8(SI)(R9*8), AX
	SBBQ 8(DX)(R9*8), AX
	MOVQ AX, 8(DI)(R9*8)
	MOVQ 16(SI)(R9*8), AX
	SBBQ 16(DX)(R9*8), AX
	MOVQ AX, 16(DI)(R9*8)
	MOVQ 24(SI)(R9*8), AX
	SBBQ 24(DX)(R9*8), AX
	MOVQ AX, 24(DI)(R9*8)
	SBBQ BX, BX
	ADDQ $4, R9
	DECQ R10
	JNZ  subfour
subone:
	TESTQ CX, CX
	JZ   subdone
subloop:
	NEGQ BX
	MOVQ 0(SI)(R9*8), AX
	SBBQ 0(DX)(R9*8), AX
	MOVQ AX, 0(DI)(R9*8)
	SBBQ BX, BX
	INCQ R9
	DECQ CX
	JNZ  subloop
subdone:
	NEGQ BX
	MOVQ BX, b+72(FP)
	RET

// func mulAddRow(z, x []uint, d uint) (c uint)
TEXT ·mulAddRow(SB), NOSPLIT, $0-64
	MOVQ z_base+0(FP), DI
	MOVQ x_base+24(FP), SI
	MOVQ x_len+32(FP), CX
	MOVQ d+48(FP), R8
	XORQ BX, BX
	XORQ R9, R9
	MOVQ CX, R10
	SHRQ $2, R10
	ANDQ $3, CX
	TESTQ R10, R10
	JZ   maone
mafour:
	MOVQ 0(SI)(R9*8), AX
	MULQ R8
	ADDQ BX, AX
	ADCQ $0, DX
	ADDQ AX, 0(DI)(R9*8)
	ADCQ $0, DX
	MOVQ DX, BX
	MOVQ 8(SI)(R9*8), AX
	MULQ R8
	ADDQ BX, AX
	ADCQ $0, DX
	ADDQ AX, 8(DI)(R9*8)
	ADCQ $0, DX
	MOVQ DX, BX
	MOVQ 16(SI)(R9*8), AX
	MULQ R8
	ADDQ BX, AX
	ADCQ $0, DX
	ADDQ AX, 16(DI)(R9*8)
	ADCQ $0, DX
	MOVQ DX, BX
	MOVQ 24(SI)(R9*8), AX
	MULQ R8
	ADDQ BX, AX
	ADCQ $0, DX
	ADDQ AX, 24(DI)(R9*8)
	ADCQ $0, DX
	MOVQ DX, BX
	ADDQ $4, R9
	DECQ R10
	JNZ  mafour
maone:
	TESTQ CX, CX
	JZ   madone
maloop:
	MOVQ 0(SI)(R9*8), AX
	MULQ R8
	ADDQ BX, AX
	ADCQ $0, DX
	ADDQ AX, 0(DI)(R9*8)
	ADCQ $0, DX
	MOVQ DX, BX
	INCQ R9
	DECQ CX
	JNZ  maloop
madone:
	MOVQ BX, c+56(FP)
	RET

// func mulRow(z, x []uint, d uint) (c uint)
TEXT ·mulRow(SB), NOSPLIT, $0-64
	MOVQ z_base+0(FP), DI
	MOVQ x_base+24(FP), SI
	MOVQ x_len+32(FP), CX
	MOVQ d+48(FP), R8
	XORQ BX, BX
	XORQ R9, R9
	MOVQ CX, R10
	SHRQ $2, R10
	ANDQ $3, CX
	TESTQ R10, R10
	JZ   mrone
mrfour:
	MOVQ 0(SI)(R9*8), AX
	MULQ R8
	ADDQ BX, AX
	ADCQ $0, DX
	MOVQ AX, 0(DI)(R9*8)
	MOVQ DX, BX
	MOVQ 8(SI)(R9*8), AX
	MULQ R8
	ADDQ BX, AX
	ADCQ $0, DX
	MOVQ AX, 8(DI)(R9*8)
	MOVQ DX, BX
	MOVQ 16(SI)(R9*8), AX
	MULQ R8
	ADDQ BX, AX
	ADCQ $0, DX
	MOVQ AX, 16(DI)(R9*8)
	MOVQ DX, BX
	MOVQ 24(SI)(R9*8), AX
	MULQ R8
	ADDQ BX, AX
	ADCQ $0, DX
	MOVQ AX, 24(DI)(R9*8)
	MOVQ DX, BX
	ADDQ $4, R9
	DECQ R10
	JNZ  mrfour
mrone:
	TESTQ CX, CX
	JZ   mrdone
mrloop:
	MOVQ 0(SI)(R9*8), AX
	MULQ R8
	ADDQ BX, AX
	ADCQ $0, DX
	MOVQ AX, 0(DI)(R9*8)
	MOVQ DX, BX
	INCQ R9
	DECQ CX
	JNZ  mrloop
mrdone:
	MOVQ BX, c+56(FP)
	RET

// func mulSubRow(z, x []uint, d uint) (c uint)
TEXT ·mulSubRow(SB), NOSPLIT, $0-64
	MOVQ z_base+0(FP), DI
	MOVQ x_base+24(FP), SI
	MOVQ x_len+32(FP), CX
	MOVQ d+48(FP), R8
	XORQ BX, BX
	XORQ R9, R9
	MOVQ CX, R10
	SHRQ $2, R10
	ANDQ $3, CX
	TESTQ R10, R10
	JZ   msone
msfour:
	MOVQ 0(SI)(R9*8), AX
	MULQ R8
	ADDQ BX, AX
	ADCQ $0, DX
	SUBQ AX, 0(DI)(R9*8)
	ADCQ $0, DX
	MOVQ DX, BX
	MOVQ 8(SI)(R9*8), AX
	MULQ R8
	ADDQ BX, AX
	ADCQ $0, DX
	SUBQ AX, 8(DI)(R9*8)
	ADCQ $0, DX
	MOVQ DX, BX
	MOVQ 16(SI)(R9*8), AX
	MULQ R8
	ADDQ BX, AX
	ADCQ $0, DX
	SUBQ AX, 16(DI)(R9*8)
	ADCQ $0, DX
	MOVQ DX, BX
	MOVQ 24(SI)(R9*8), AX
	MULQ R8
	ADDQ BX, AX
	ADCQ $0, DX
	SUBQ AX, 24(DI)(R9*8)
	ADCQ $0, DX
	MOVQ DX, BX
	ADDQ $4, R9
	DECQ R10
	JNZ  msfour
msone:
	TESTQ CX, CX
	JZ   msdone
msloop:
	MOVQ 0(SI)(R9*8), AX
	MULQ R8
	ADDQ BX, AX
	ADCQ $0, DX
	SUBQ AX, 0(DI)(R9*8)
	ADCQ $0, DX
	MOVQ DX, BX
	INCQ R9
	DECQ CX
	JNZ  msloop
msdone:
	MOVQ BX, c+56(FP)
	RET

//go:build !purego

#include "textflag.h"

// The inner loops of bignum's arithmetic, as arith_amd64.s's, on the carry
// flag, which CBZ and a SUB that sets no flags leave as it is between
// words.

// func addVV(z, x, y []uint) (c uint)
TEXT ·addVV(SB), NOSPLIT, $0-80
	MOVD z_len+8(FP), R0
	MOVD z_base+0(FP), R1
	MOVD x_base+24(FP), R2
	MOVD y_base+48(FP), R3
	ADDS $0, R0, R4 // the carry clear
addloop:
	CBZ    R0, adddone
	MOVD.P 8(R2), R4
	MOVD.P 8(R3), R5
	ADCS   R5, R4, R4
	MOVD.P R4, 8(R1)
	SUB    $1, R0
	B      addloop
adddone:
	CSET HS, R4
	MOVD R4, c+72(FP)
	RET

// func subVV(z, x, y []uint) (b uint)
TEXT ·subVV(SB), NOSPLIT, $0-80
	MOVD z_len+8(FP), R0
	MOVD z_base+0(FP), R1
	MOVD x_base+24(FP), R2
	MOVD y_base+48(FP), R3
	SUBS $0, R0, R4 // no borrow: the carry set
subloop:
	CBZ    R0, subdone
	MOVD.P 8(R2), R4
	MOVD.P 8(R3), R5
	SBCS   R5, R4, R4
	MOVD.P R4, 8(R1)
	SUB    $1, R0
	B      subloop
subdone:
	CSET LO, R4
	MOVD R4, b+72(FP)
	RET

// func mulAddRow(z, x []uint, d uint) (c uint)
TEXT ·mulAddRow(SB), NOSPLIT, $0-64
	MOVD z_base+0(FP), R1
	MOVD x_base+24(FP), R2
	MOVD x_len+32(FP), R0
	MOVD d+48(FP), R3
	MOVD ZR, R4
maloop:
	CBZ    R0, madone
	MOVD.P 8(R2), R5
	MUL    R3, R5, R6
	UMULH  R3, R5, R7
	ADDS   R4, R6, R6
	ADC    ZR, R7, R7
	MOVD   (R1), R8
	ADDS   R8, R6, R6
	ADC    ZR, R7, R4
	MOVD.P R6, 8(R1)
	SUB    $1, R0
	B      maloop
madone:
	MOVD R4, c+56(FP)
	RET

// func mulRow(z, x []uint, d uint) (c uint)
TEXT ·mulRow(SB), NOSPLIT, $0-64
	MOVD z_base+0(FP), R1
	MOVD x_base+24(FP), R2
	MOVD x_len+32(FP), R0
	MOVD d+48(FP), R3
	MOVD ZR, R4
mrloop:
	CBZ    R0, mrdone
	MOVD.P 8(R2), R5
	MUL    R3, R5, R6
	UMULH  R3, R5, R7
	ADDS   R4, R6, R6
	ADC    ZR, R7, R4
	MOVD.P R6, 8(R1)
	SUB    $1, R0
	B      mrloop
mrdone:
	MOVD R4, c+56(FP)
	RET

// func mulSubRow(z, x []uint, d uint) (c uint)
TEXT ·mulSubRow(SB), NOSPLIT, $0-64
	MOVD z_base+0(FP), R1
	MOVD x_base+24(FP), R2
	MOVD x_len+32(FP), R0
	MOVD d+48(FP), R3
	MOVD ZR, R4
msloop:
	CBZ    R0, msdone
	MOVD.P 8(R2), R5
	MUL    R3, R5, R6
	UMULH  R3, R5, R7
	ADDS   R4, R6, R6
	ADC    ZR, R7, R7
	MOVD   (R1), R8
	SUBS   R6, R8, R8
	CSET   LO, R9
	ADD    R9, R7, R4
	MOVD.P R8, 8(R1)
	SUB    $1, R0
	B      msloop
msdone:
	MOVD R4, c+56(FP)
	RET

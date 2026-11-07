#include "textflag.h"

// func asciiAVX2(p *byte, n int) int
TEXT ·asciiAVX2(SB), NOSPLIT, $0-24
	MOVQ p+0(FP), SI
	MOVQ n+8(FP), CX
	MOVQ SI, DI
	MOVQ $0x22, AX
	MOVQ AX, X1
	VPBROADCASTB X1, Y1 // '"'
	MOVQ $0x5C, AX
	MOVQ AX, X2
	VPBROADCASTB X2, Y2 // '\\'
	MOVQ $0x20, AX
	MOVQ AX, X3
	VPBROADCASTB X3, Y3 // the first character that is no control

asciiLoop:
	CMPQ CX, $32
	JB   asciiDone
	VMOVDQU (SI), Y0
	VPCMPEQB Y0, Y1, Y4
	VPCMPEQB Y0, Y2, Y5
	VPCMPGTB Y0, Y3, Y6 // signed: true of a control and of a byte above ASCII
	VPOR Y4, Y5, Y4
	VPOR Y4, Y6, Y4
	VPMOVMSKB Y4, AX
	TESTL AX, AX
	JNZ  asciiFound
	ADDQ $32, SI
	SUBQ $32, CX
	JMP  asciiLoop

asciiFound:
	BSFL AX, AX
	ADDQ AX, SI

asciiDone:
	SUBQ DI, SI
	MOVQ SI, ret+16(FP)
	VZEROUPPER
	RET

// func wideAVX2(p *byte, n int, other byte) int
TEXT ·wideAVX2(SB), NOSPLIT, $0-32
	MOVQ p+0(FP), SI
	MOVQ n+8(FP), CX
	MOVBQZX other+16(FP), AX
	MOVQ SI, DI
	MOVQ AX, X7
	VPBROADCASTB X7, Y7 // what else to stop at
	MOVQ $0x22, AX
	MOVQ AX, X1
	VPBROADCASTB X1, Y1 // '"'
	MOVQ $0x5C, AX
	MOVQ AX, X2
	VPBROADCASTB X2, Y2 // '\\'
	MOVQ $0x1F, AX
	MOVQ AX, X3
	VPBROADCASTB X3, Y3 // the last control

wideLoop:
	CMPQ CX, $32
	JB   wideDone
	VMOVDQU (SI), Y0
	VPCMPEQB Y0, Y1, Y4
	VPCMPEQB Y0, Y2, Y5
	VPCMPEQB Y0, Y7, Y6
	VPMINUB Y0, Y3, Y8
	VPCMPEQB Y0, Y8, Y8 // unsigned: x is at most the last control
	VPOR Y4, Y5, Y4
	VPOR Y6, Y8, Y6
	VPOR Y4, Y6, Y4
	VPMOVMSKB Y4, AX
	TESTL AX, AX
	JNZ  wideFound
	ADDQ $32, SI
	SUBQ $32, CX
	JMP  wideLoop

wideFound:
	BSFL AX, AX
	ADDQ AX, SI

wideDone:
	SUBQ DI, SI
	MOVQ SI, ret+24(FP)
	VZEROUPPER
	RET

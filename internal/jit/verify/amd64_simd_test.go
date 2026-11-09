//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package verify

import (
	"fmt"
	"testing"

	"golang.org/x/arch/x86/x86asm"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/amd64"
)

// The vector forms of WebAssembly's SIMD instructions (asm/amd64/simd.go),
// each with every pair of registers it takes.

var vecOps = map[amd64.VOp]x86asm.Op{
	amd64.Paddb: x86asm.PADDB, amd64.Paddw: x86asm.PADDW, amd64.Paddd: x86asm.PADDD, amd64.Paddq: x86asm.PADDQ,
	amd64.Psubb: x86asm.PSUBB, amd64.Psubw: x86asm.PSUBW, amd64.Psubd: x86asm.PSUBD, amd64.Psubq: x86asm.PSUBQ,
	amd64.Paddsb: x86asm.PADDSB, amd64.Paddsw: x86asm.PADDSW, amd64.Paddusb: x86asm.PADDUSB, amd64.Paddusw: x86asm.PADDUSW,
	amd64.Psubsb: x86asm.PSUBSB, amd64.Psubsw: x86asm.PSUBSW, amd64.Psubusb: x86asm.PSUBUSB, amd64.Psubusw: x86asm.PSUBUSW,
	amd64.Pmullw: x86asm.PMULLW, amd64.Pmulhw: x86asm.PMULHW, amd64.Pmulhuw: x86asm.PMULHUW, amd64.Pmuludq: x86asm.PMULUDQ,
	amd64.Pmaddwd: x86asm.PMADDWD, amd64.Pand: x86asm.PAND, amd64.Pandn: x86asm.PANDN, amd64.Por: x86asm.POR,
	amd64.Pxor: x86asm.PXOR, amd64.Pcmpeqb: x86asm.PCMPEQB, amd64.Pcmpeqw: x86asm.PCMPEQW, amd64.Pcmpeqd: x86asm.PCMPEQD,
	amd64.Pcmpgtb: x86asm.PCMPGTB, amd64.Pcmpgtw: x86asm.PCMPGTW, amd64.Pcmpgtd: x86asm.PCMPGTD,
	amd64.Pminub: x86asm.PMINUB, amd64.Pmaxub: x86asm.PMAXUB, amd64.Pminsw: x86asm.PMINSW, amd64.Pmaxsw: x86asm.PMAXSW,
	amd64.Pavgb: x86asm.PAVGB, amd64.Pavgw: x86asm.PAVGW, amd64.Packsswb: x86asm.PACKSSWB, amd64.Packuswb: x86asm.PACKUSWB,
	amd64.Packssdw: x86asm.PACKSSDW, amd64.Punpcklbw: x86asm.PUNPCKLBW, amd64.Punpcklwd: x86asm.PUNPCKLWD,
	amd64.Punpckldq: x86asm.PUNPCKLDQ, amd64.Punpcklqdq: x86asm.PUNPCKLQDQ, amd64.Punpckhbw: x86asm.PUNPCKHBW,
	amd64.Punpckhwd: x86asm.PUNPCKHWD, amd64.Punpckhdq: x86asm.PUNPCKHDQ, amd64.Punpckhqdq: x86asm.PUNPCKHQDQ,
	amd64.Psllw: x86asm.PSLLW, amd64.Pslld: x86asm.PSLLD, amd64.Psllq: x86asm.PSLLQ, amd64.Psrlw: x86asm.PSRLW,
	amd64.Psrld: x86asm.PSRLD, amd64.Psrlq: x86asm.PSRLQ, amd64.Psraw: x86asm.PSRAW, amd64.Psrad: x86asm.PSRAD,
	amd64.Addps: x86asm.ADDPS, amd64.Subps: x86asm.SUBPS, amd64.Mulps: x86asm.MULPS, amd64.Divps: x86asm.DIVPS,
	amd64.Sqrtps: x86asm.SQRTPS, amd64.Minps: x86asm.MINPS, amd64.Maxps: x86asm.MAXPS, amd64.Andps: x86asm.ANDPS,
	amd64.Andnps: x86asm.ANDNPS, amd64.Orps: x86asm.ORPS, amd64.Xorps: x86asm.XORPS, amd64.Movhlps: x86asm.MOVHLPS,
	amd64.Movlhps: x86asm.MOVLHPS, amd64.Addpd: x86asm.ADDPD, amd64.Subpd: x86asm.SUBPD, amd64.Mulpd: x86asm.MULPD,
	amd64.Divpd: x86asm.DIVPD, amd64.Sqrtpd: x86asm.SQRTPD, amd64.Minpd: x86asm.MINPD, amd64.Maxpd: x86asm.MAXPD,
	amd64.Movsd: x86asm.MOVSD_XMM, amd64.Cvtdq2ps: x86asm.CVTDQ2PS, amd64.Cvtps2pd: x86asm.CVTPS2PD,
	amd64.Cvtpd2ps: x86asm.CVTPD2PS, amd64.Cvttps2dq: x86asm.CVTTPS2DQ, amd64.Cvttpd2dq: x86asm.CVTTPD2DQ,
	amd64.Cvtdq2pd: x86asm.CVTDQ2PD, amd64.Pshufb: x86asm.PSHUFB, amd64.Pmaddubsw: x86asm.PMADDUBSW,
	amd64.Pmulhrsw: x86asm.PMULHRSW, amd64.Pabsb: x86asm.PABSB, amd64.Pabsw: x86asm.PABSW, amd64.Pabsd: x86asm.PABSD,
	amd64.Ptest: x86asm.PTEST, amd64.Pmovsxbw: x86asm.PMOVSXBW, amd64.Pmovsxwd: x86asm.PMOVSXWD,
	amd64.Pmovsxdq: x86asm.PMOVSXDQ, amd64.Pmovzxbw: x86asm.PMOVZXBW, amd64.Pmovzxwd: x86asm.PMOVZXWD,
	amd64.Pmovzxdq: x86asm.PMOVZXDQ, amd64.Pmuldq: x86asm.PMULDQ, amd64.Pcmpeqq: x86asm.PCMPEQQ,
	amd64.Packusdw: x86asm.PACKUSDW, amd64.Pcmpgtq: x86asm.PCMPGTQ, amd64.Pminsb: x86asm.PMINSB,
	amd64.Pminsd: x86asm.PMINSD, amd64.Pminuw: x86asm.PMINUW, amd64.Pminud: x86asm.PMINUD, amd64.Pmaxsb: x86asm.PMAXSB,
	amd64.Pmaxsd: x86asm.PMAXSD, amd64.Pmaxuw: x86asm.PMAXUW, amd64.Pmaxud: x86asm.PMAXUD, amd64.Pmulld: x86asm.PMULLD,
}

var vecImmOps = map[amd64.VOp]x86asm.Op{
	amd64.Pshufd: x86asm.PSHUFD, amd64.Pshuflw: x86asm.PSHUFLW, amd64.Pshufhw: x86asm.PSHUFHW,
	amd64.Shufps: x86asm.SHUFPS, amd64.Cmpps: x86asm.CMPPS, amd64.Cmppd: x86asm.CMPPD, amd64.Roundps: x86asm.ROUNDPS,
	amd64.Roundpd: x86asm.ROUNDPD, amd64.Pblendw: x86asm.PBLENDW, amd64.Palignr: x86asm.PALIGNR,
	amd64.Insertps: x86asm.INSERTPS,
}

func TestAMD64WasmVector(t *testing.T) {
	for op, xop := range vecOps {
		for _, d := range xregs {
			for _, s := range xregs {
				name := fmt.Sprintf("%v x%d,x%d", xop, d, s)
				expect(t, name, encode(func(a *amd64.Asm) { a.Vec(op, d, s) }), xop, xr(d), xr(s))
			}
		}
	}
	for op, xop := range vecImmOps {
		for _, d := range xregs {
			for _, s := range xregs {
				for _, imm := range []uint8{0, 1, 0x1b, 0x80, 0xff} {
					name := fmt.Sprintf("%v x%d,x%d,%#x", xop, d, s, imm)
					expect(t, name, encode(func(a *amd64.Asm) { a.VecImm(op, d, s, imm) }), xop, xr(d), xr(s), int64(imm))
				}
			}
		}
	}
	shifts := map[amd64.VShift]x86asm.Op{amd64.PsllwImm: x86asm.PSLLW, amd64.PsrlwImm: x86asm.PSRLW, amd64.PsrawImm: x86asm.PSRAW,
		amd64.PslldImm: x86asm.PSLLD, amd64.PsrldImm: x86asm.PSRLD, amd64.PsradImm: x86asm.PSRAD, amd64.PsllqImm: x86asm.PSLLQ,
		amd64.PsrlqImm: x86asm.PSRLQ, amd64.PsrldqImm: x86asm.PSRLDQ, amd64.PslldqImm: x86asm.PSLLDQ}
	for op, xop := range shifts {
		for _, d := range xregs {
			for _, n := range []uint8{0, 1, 7, 8, 15, 31, 63} {
				expect(t, fmt.Sprintf("%v x%d,%d", xop, d, n), encode(func(a *amd64.Asm) { a.VecShift(op, d, n) }), xop, xr(d), int64(n))
			}
		}
	}
	masks := map[amd64.VOp]x86asm.Op{amd64.Pmovmskb: x86asm.PMOVMSKB, amd64.Movmskps: x86asm.MOVMSKPS, amd64.Movmskpd: x86asm.MOVMSKPD}
	for op, xop := range masks {
		for _, r := range regs {
			for _, x := range xregs {
				expect(t, fmt.Sprintf("%v r%d,x%d", xop, r, x), encode(func(a *amd64.Asm) { a.VecMask(op, r, x) }), xop, r32(r), xr(x))
			}
		}
	}
	pinsr := map[uint8]x86asm.Op{1: x86asm.PINSRB, 2: x86asm.PINSRW, 4: x86asm.PINSRD, 8: x86asm.PINSRQ}
	pextr := map[uint8]x86asm.Op{1: x86asm.PEXTRB, 2: x86asm.PEXTRW, 4: x86asm.PEXTRD, 8: x86asm.PEXTRQ}
	for _, size := range []uint8{1, 2, 4, 8} {
		for _, r := range regs {
			for _, x := range xregs {
				for _, lane := range []uint8{0, 1, 16 / size / 2, 16/size - 1} {
					gr := r32(r)
					if size == 8 {
						gr = r64(r)
					}
					name := fmt.Sprintf("%d r%d,x%d,%d", size, r, x, lane)
					expect(t, "Pinsr "+name, encode(func(a *amd64.Asm) { a.Pinsr(size, x, r, lane) }), pinsr[size], xr(x), gr, int64(lane))
					expect(t, "Pextr "+name, encode(func(a *amd64.Asm) { a.Pextr(size, r, x, lane) }), pextr[size], gr, xr(x), int64(lane))
				}
			}
		}
	}
	for _, x := range xregs {
		for _, b := range regs {
			for _, disp := range disps {
				m := mem{r64(b), int64(disp)}
				expectX(t, "LoadVec", encode(func(a *amd64.Asm) { a.LoadVec(x, b, disp) }), x86asm.MOVDQU, 16, xr(x), m)
				expectX(t, "StoreVec", encode(func(a *amd64.Asm) { a.StoreVec(b, disp, x) }), x86asm.MOVDQU, 16, m, xr(x))
			}
			for _, i := range indexes {
				for _, disp := range []int32{0, -8, 128, 0x7fffffff} {
					m := memx{r64(b), r64(i), 0, int64(disp)}
					expectX(t, "LoadVecIdx", encode(func(a *amd64.Asm) { a.LoadVecIdx(x, b, i, disp) }), x86asm.MOVDQU, 16, xr(x), m)
					expectX(t, "StoreVecIdx", encode(func(a *amd64.Asm) { a.StoreVecIdx(b, i, disp, x) }), x86asm.MOVDQU, 16, m, xr(x))
				}
			}
		}
	}
}

// LoadVecLabel reads the 16 bytes at its label, relative to the end of the
// instruction.
func TestAMD64LoadVecLabel(t *testing.T) {
	for _, x := range xregs {
		code := encode(func(a *amd64.Asm) {
			l := a.NewLabel()
			a.LoadVecLabel(x, l)
			a.Bind(l)
			a.Quad(1)
			a.Quad(2)
		})
		inst := decodeOne(t, "LoadVecLabel", code[:len(code)-16])
		m, ok := inst.Args[1].(x86asm.Mem)
		if inst.Op != x86asm.MOVDQU || inst.Args[0] != xr(x) || !ok || m.Base != x86asm.RIP || m.Disp != 0 {
			t.Fatalf("LoadVecLabel x%d: % x is %v", x, code, inst)
		}
	}
}

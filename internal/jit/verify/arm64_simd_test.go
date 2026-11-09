package verify

import (
	"fmt"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/asm/arm64"
)

// The vector forms of WebAssembly's SIMD instructions (asm/arm64/simd.go),
// each at every lane size it takes and with every register in each of its
// fields, disassembled with x/arch's arm64asm.

// arr is the arrangement of lanes of size s in 128 bits, or in 64 if half.
func arr(s arm64.VSize, half bool) string {
	if half {
		return [...]string{"8b", "4h", "2s", "1d"}[s]
	}
	return [...]string{"16b", "8h", "4s", "2d"}[s]
}

// fieldRegs are the register triples each form is checked with: every
// register in each field, the others fixed.
func fieldRegs() (out [][3]arm64.FReg) {
	for r := arm64.FReg(0); r < 32; r++ {
		out = append(out, [3]arm64.FReg{r, 7, 9}, [3]arm64.FReg{7, r, 9}, [3]arm64.FReg{7, 9, r})
	}
	return out
}

type vform struct {
	op    arm64.VOp
	name  string
	sizes []arm64.VSize
}

var (
	vAll   = []arm64.VSize{arm64.V8, arm64.V16, arm64.V32}
	vAll64 = []arm64.VSize{arm64.V8, arm64.V16, arm64.V32, arm64.V64}
	vFloat = []arm64.VSize{arm64.V32, arm64.V64}
	vBytes = []arm64.VSize{arm64.V8}
)

func TestARM64WasmVector3(t *testing.T) {
	forms := []vform{
		{arm64.VAdd, "add", vAll64}, {arm64.VSub, "sub", vAll64}, {arm64.VSqadd, "sqadd", vAll64},
		{arm64.VUqadd, "uqadd", vAll64}, {arm64.VSqsub, "sqsub", vAll64}, {arm64.VUqsub, "uqsub", vAll64},
		{arm64.VCmeq, "cmeq", vAll64}, {arm64.VCmgt, "cmgt", vAll64}, {arm64.VCmhi, "cmhi", vAll64},
		{arm64.VCmge, "cmge", vAll64}, {arm64.VCmhs, "cmhs", vAll64}, {arm64.VSshl, "sshl", vAll64},
		{arm64.VUshl, "ushl", vAll64}, {arm64.VSmax, "smax", vAll}, {arm64.VUmax, "umax", vAll},
		{arm64.VSmin, "smin", vAll}, {arm64.VUmin, "umin", vAll}, {arm64.VMul, "mul", vAll},
		{arm64.VUrhadd, "urhadd", vAll}, {arm64.VSqrdmulh, "sqrdmulh", []arm64.VSize{arm64.V16, arm64.V32}},
		{arm64.VAddp, "addp", vAll64}, {arm64.VUmaxp, "umaxp", vAll},
		{arm64.VAnd, "and", vBytes}, {arm64.VBic, "bic", vBytes}, {arm64.VOrr, "orr", vBytes},
		{arm64.VEor, "eor", vBytes}, {arm64.VBsl, "bsl", vBytes},
		{arm64.VFadd, "fadd", vFloat}, {arm64.VFsub, "fsub", vFloat}, {arm64.VFmul, "fmul", vFloat},
		{arm64.VFdiv, "fdiv", vFloat}, {arm64.VFmax, "fmax", vFloat}, {arm64.VFmin, "fmin", vFloat},
		{arm64.VFcmeq, "fcmeq", vFloat}, {arm64.VFcmge, "fcmge", vFloat}, {arm64.VFcmgt, "fcmgt", vFloat},
		{arm64.VFmla, "fmla", vFloat}, {arm64.VFmls, "fmls", vFloat},
		{arm64.VZip1, "zip1", vAll64}, {arm64.VZip2, "zip2", vAll64}, {arm64.VUzp1, "uzp1", vAll64},
		{arm64.VUzp2, "uzp2", vAll64},
	}
	for _, f := range forms {
		for _, s := range f.sizes {
			for _, r := range fieldRegs() {
				ar := arr(s, false)
				want := fmt.Sprintf("%s v%d.%s, v%d.%s, v%d.%s", f.name, r[0], ar, r[1], ar, r[2], ar)
				if f.op == arm64.VOrr && r[1] == r[2] {
					want = fmt.Sprintf("mov v%d.16b, v%d.16b", r[0], r[1])
				}
				expectA64(t, want, func(a *arm64.Asm) { a.V3(f.op, s, r[0], r[1], r[2]) })
			}
		}
	}
	// The long products of each half.
	for _, f := range []vform{{arm64.VSmull, "smull", vAll}, {arm64.VUmull, "umull", vAll}} {
		for _, s := range f.sizes {
			for _, r := range fieldRegs() {
				w := arr(s+1, false)
				expectA64(t, fmt.Sprintf("%s v%d.%s, v%d.%s, v%d.%s", f.name, r[0], w, r[1], arr(s, true), r[2], arr(s, true)),
					func(a *arm64.Asm) { a.V3(f.op, s, r[0], r[1], r[2]) })
				expectA64(t, fmt.Sprintf("%s2 v%d.%s, v%d.%s, v%d.%s", f.name, r[0], w, r[1], arr(s, false), r[2], arr(s, false)),
					func(a *arm64.Asm) { a.V3(f.op|arm64.VQ, s, r[0], r[1], r[2]) })
			}
		}
	}
	for _, r := range fieldRegs() {
		expectA64(t, fmt.Sprintf("mov v%d.16b, v%d.16b", r[0], r[1]), func(a *arm64.Asm) { a.VMov(r[0], r[1]) })
	}
}

func TestARM64WasmVector2(t *testing.T) {
	forms := []vform{
		{arm64.VCnt, "cnt", vBytes}, {arm64.VNot, "mvn", vBytes},
		{arm64.VAbs, "abs", vAll64}, {arm64.VNeg, "neg", vAll64},
		{arm64.VFabs, "fabs", vFloat}, {arm64.VFneg, "fneg", vFloat}, {arm64.VFsqrt, "fsqrt", vFloat},
		{arm64.VFrintn, "frintn", vFloat}, {arm64.VFrintm, "frintm", vFloat}, {arm64.VFrintp, "frintp", vFloat},
		{arm64.VFrintz, "frintz", vFloat}, {arm64.VFcvtzs, "fcvtzs", vFloat}, {arm64.VFcvtzu, "fcvtzu", vFloat},
		{arm64.VScvtf, "scvtf", vFloat}, {arm64.VUcvtf, "ucvtf", vFloat},
	}
	for _, f := range forms {
		for _, s := range f.sizes {
			for _, r := range fieldRegs() {
				ar := arr(s, false)
				expectA64(t, fmt.Sprintf("%s v%d.%s, v%d.%s", f.name, r[0], ar, r[1], ar),
					func(a *arm64.Asm) { a.V2(f.op, s, r[0], r[1]) })
			}
		}
	}
	for _, f := range []vform{{arm64.VCmeq0, "cmeq", vAll64}, {arm64.VCmlt0, "cmlt", vAll64}} {
		for _, s := range f.sizes {
			for _, r := range fieldRegs() {
				ar := arr(s, false)
				expectA64(t, fmt.Sprintf("%s v%d.%s, v%d.%s, #0", f.name, r[0], ar, r[1], ar),
					func(a *arm64.Asm) { a.V2(f.op, s, r[0], r[1]) })
			}
		}
	}
	// The pairwise widenings, the narrowings and the sums across the lanes.
	scalar := [...]string{"b", "h", "s", "d"}
	for _, r := range fieldRegs() {
		for _, s := range vAll {
			for _, f := range []vform{{arm64.VSaddlp, "saddlp", nil}, {arm64.VUaddlp, "uaddlp", nil}} {
				expectA64(t, fmt.Sprintf("%s v%d.%s, v%d.%s", f.name, r[0], arr(s+1, false), r[1], arr(s, false)),
					func(a *arm64.Asm) { a.V2(f.op, s, r[0], r[1]) })
			}
			for _, f := range []vform{{arm64.VXtn, "xtn", nil}, {arm64.VSqxtn, "sqxtn", nil}, {arm64.VUqxtn, "uqxtn", nil}, {arm64.VSqxtun, "sqxtun", nil}} {
				expectA64(t, fmt.Sprintf("%s v%d.%s, v%d.%s", f.name, r[0], arr(s, true), r[1], arr(s+1, false)),
					func(a *arm64.Asm) { a.V2(f.op, s, r[0], r[1]) })
				expectA64(t, fmt.Sprintf("%s2 v%d.%s, v%d.%s", f.name, r[0], arr(s, false), r[1], arr(s+1, false)),
					func(a *arm64.Asm) { a.V2(f.op|arm64.VQ, s, r[0], r[1]) })
			}
			for _, f := range []vform{{arm64.VAddv, "addv", nil}, {arm64.VUmaxv, "umaxv", nil}, {arm64.VUminv, "uminv", nil}} {
				expectA64(t, fmt.Sprintf("%s %s%d, v%d.%s", f.name, scalar[s], r[0], r[1], arr(s, false)),
					func(a *arm64.Asm) { a.V2(f.op, s, r[0], r[1]) })
			}
		}
		expectA64(t, fmt.Sprintf("fcvtn v%d.2s, v%d.2d", r[0], r[1]), func(a *arm64.Asm) { a.V2(arm64.VFcvtn, arm64.V64, r[0], r[1]) })
		expectA64(t, fmt.Sprintf("fcvtl v%d.2d, v%d.2s", r[0], r[1]), func(a *arm64.Asm) { a.V2(arm64.VFcvtl, arm64.V64, r[0], r[1]) })
	}
}

func TestARM64WasmVectorShifts(t *testing.T) {
	for s := arm64.V8; s <= arm64.V64; s++ {
		bits := uint8(8) << s
		ar := arr(s, false)
		for _, r := range fieldRegs() {
			for _, n := range []uint8{0, 1, bits / 2, bits - 1} {
				expectA64(t, fmt.Sprintf("shl v%d.%s, v%d.%s, #%d", r[0], ar, r[1], ar, n),
					func(a *arm64.Asm) { a.VShiftImm(arm64.VShl, s, r[0], r[1], n) })
			}
			for _, n := range []uint8{1, bits / 2, bits} {
				expectA64(t, fmt.Sprintf("sshr v%d.%s, v%d.%s, #%d", r[0], ar, r[1], ar, n),
					func(a *arm64.Asm) { a.VShiftImm(arm64.VSshr, s, r[0], r[1], n) })
				expectA64(t, fmt.Sprintf("ushr v%d.%s, v%d.%s, #%d", r[0], ar, r[1], ar, n),
					func(a *arm64.Asm) { a.VShiftImm(arm64.VUshr, s, r[0], r[1], n) })
			}
			if s == arm64.V64 {
				continue
			}
			w := arr(s+1, false)
			expectA64(t, fmt.Sprintf("sxtl v%d.%s, v%d.%s", r[0], w, r[1], arr(s, true)),
				func(a *arm64.Asm) { a.VShiftImm(arm64.VSshll, s, r[0], r[1], 0) })
			expectA64(t, fmt.Sprintf("sxtl2 v%d.%s, v%d.%s", r[0], w, r[1], ar),
				func(a *arm64.Asm) { a.VShiftImm(arm64.VSshll|arm64.VQ, s, r[0], r[1], 0) })
			expectA64(t, fmt.Sprintf("uxtl v%d.%s, v%d.%s", r[0], w, r[1], arr(s, true)),
				func(a *arm64.Asm) { a.VShiftImm(arm64.VUshll, s, r[0], r[1], 0) })
			expectA64(t, fmt.Sprintf("uxtl2 v%d.%s, v%d.%s", r[0], w, r[1], ar),
				func(a *arm64.Asm) { a.VShiftImm(arm64.VUshll|arm64.VQ, s, r[0], r[1], 0) })
		}
	}
}

func TestARM64WasmLanes(t *testing.T) {
	names := [...]string{"b", "h", "s", "d"}
	for s := arm64.V8; s <= arm64.V64; s++ {
		lanes := uint8(16) >> s
		gr := "w"
		if s == arm64.V64 {
			gr = "x"
		}
		for v := arm64.FReg(0); v < 32; v++ {
			for _, g := range []arm64.Reg{0, 5, 16, 27, 30} {
				expectA64(t, fmt.Sprintf("dup v%d.%s, %s%d", v, arr(s, false), gr, g), func(a *arm64.Asm) { a.Dup(s, v, g) })
				for _, i := range []uint8{0, 1, lanes - 1} {
					expectA64(t, fmt.Sprintf("mov v%d.%s[%d], %s%d", v, names[s], i, gr, g), func(a *arm64.Asm) { a.InsG(s, v, i, g) })
					want := fmt.Sprintf("umov w%d, v%d.%s[%d]", g, v, names[s], i)
					switch s {
					case arm64.V64:
						want = fmt.Sprintf("mov x%d, v%d.d[%d]", g, v, i)
					case arm64.V32:
						want = fmt.Sprintf("mov w%d, v%d.s[%d]", g, v, i)
					}
					expectA64(t, want, func(a *arm64.Asm) { a.Umov(s, g, v, i) })
					if s < arm64.V32 {
						expectA64(t, fmt.Sprintf("smov w%d, v%d.%s[%d]", g, v, names[s], i), func(a *arm64.Asm) { a.Smov(s, g, v, i) })
					}
				}
			}
			for _, n := range []arm64.FReg{0, 3, 17, 31} {
				for _, i := range []uint8{0, 1, lanes - 1} {
					expectA64(t, fmt.Sprintf("dup v%d.%s, v%d.%s[%d]", v, arr(s, false), n, names[s], i), func(a *arm64.Asm) { a.DupElem(s, v, n, i) })
					expectA64(t, fmt.Sprintf("mov %s%d, v%d.%s[%d]", names[s], v, n, names[s], i), func(a *arm64.Asm) { a.DupScalar(s, v, n, i) })
					for _, j := range []uint8{0, lanes - 1} {
						expectA64(t, fmt.Sprintf("mov v%d.%s[%d], v%d.%s[%d]", v, names[s], i, n, names[s], j), func(a *arm64.Asm) { a.InsElem(s, v, i, n, j) })
					}
				}
			}
		}
	}
	for _, r := range fieldRegs() {
		for _, i := range []uint8{0, 1, 8, 15} {
			expectA64(t, fmt.Sprintf("ext v%d.16b, v%d.16b, v%d.16b, #%d", r[0], r[1], r[2], i), func(a *arm64.Asm) { a.Ext(r[0], r[1], r[2], i) })
		}
		expectA64(t, fmt.Sprintf("tbl v%d.16b, {v%d.16b}, v%d.16b", r[0], r[1], r[2]), func(a *arm64.Asm) { a.Tbl(r[0], r[1], 1, r[2]) })
		if r[1] < 31 {
			expectA64(t, fmt.Sprintf("tbl v%d.16b, {v%d.16b, v%d.16b}, v%d.16b", r[0], r[1], r[1]+1, r[2]), func(a *arm64.Asm) { a.Tbl(r[0], r[1], 2, r[2]) })
		}
	}
}

func TestARM64WasmVectorMemory(t *testing.T) {
	for v := arm64.FReg(0); v < 32; v++ {
		for _, b := range []arm64.Reg{0, 3, 19, 26, 30} {
			for _, disp := range []int32{0, 16, 8, -8, 255, -256, 4096, 65520} {
				op, form := "ldr", fmt.Sprintf("q%d, [x%d,#%d]", v, b, disp)
				switch {
				case disp == 0:
					form = fmt.Sprintf("q%d, [x%d]", v, b)
				case disp%16 != 0 || disp < 0:
					op = "ldur"
				}
				expectA64(t, op+" "+form, func(a *arm64.Asm) { a.LoadQ(v, b, disp) })
				st := "str"
				if op == "ldur" {
					st = "stur"
				}
				expectA64(t, st+" "+form, func(a *arm64.Asm) { a.StoreQ(b, disp, v) })
			}
			for _, i := range []arm64.Reg{1, 16, 26} {
				expectA64(t, fmt.Sprintf("ldr q%d, [x%d,x%d]", v, b, i), func(a *arm64.Asm) { a.LoadQIdx(v, b, i) })
				expectA64(t, fmt.Sprintf("str q%d, [x%d,x%d]", v, b, i), func(a *arm64.Asm) { a.StoreQIdx(b, i, v) })
			}
		}
		code := a64(t, func(a *arm64.Asm) {
			l := a.NewLabel()
			a.LoadQLabel(v, l)
			a.Ret()
			a.Bind(l)
		})
		if want := fmt.Sprintf("ldr q%d, .+0x8", v); code[0] != want {
			t.Fatalf("LoadQLabel: %s, want %s", code[0], want)
		}
	}
	// A displacement past the immediates' reach goes through the temporary.
	expectA64(t, "mov x27, #0x8; movk x27, #0x1, lsl #16; add x27, x19, x27; ldr q3, [x27]", func(a *arm64.Asm) { a.LoadQ(3, 19, 0x10008) })
}

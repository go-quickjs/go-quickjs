package arm64

// The ASIMD (NEON) forms WebAssembly's SIMD instructions need: the
// operations on whole vector registers, their lanes 8, 16, 32 or 64 bits
// wide, the moves of lanes to and from general registers, and the loads
// and stores of 16 bytes. An FReg names the vector register whose low
// lanes are its S and D views.

// VSize is the width of a vector operation's lanes: for the float
// operations V32 is single precision and V64 double.
type VSize uint8

const (
	V8  VSize = 0
	V16 VSize = 1
	V32 VSize = 2
	V64 VSize = 3
)

// VOp is a vector instruction on 128 bits (Q set), its lane size and its
// registers not yet in it. VQ is the Q bit, which the forms on 64 bits --
// the narrowings and widenings of a register's lower half -- have clear and
// their second forms (SQXTN2, SSHLL2, SMULL2...) set.
type VOp uint32

const VQ VOp = 1 << 30

// The operations on three registers, Vd = Vn op Vm, the lane size at bit 22
// (V3): the integer ones.
const (
	VAdd      VOp = 0x4E208400
	VSub      VOp = 0x6E208400
	VSqadd    VOp = 0x4E200C00
	VUqadd    VOp = 0x6E200C00
	VSqsub    VOp = 0x4E202C00
	VUqsub    VOp = 0x6E202C00
	VCmeq     VOp = 0x6E208C00
	VCmgt     VOp = 0x4E203400 // signed
	VCmhi     VOp = 0x6E203400 // unsigned
	VCmge     VOp = 0x4E203C00
	VCmhs     VOp = 0x6E203C00
	VSshl     VOp = 0x4E204400 // by each lane of Vm, signed: right where negative
	VUshl     VOp = 0x6E204400
	VSmax     VOp = 0x4E206400
	VUmax     VOp = 0x6E206400
	VSmin     VOp = 0x4E206C00
	VUmin     VOp = 0x6E206C00
	VMul      VOp = 0x4E209C00
	VUrhadd   VOp = 0x6E201400
	VSqrdmulh VOp = 0x6E20B400
	VAddp     VOp = 0x4E20BC00
	VUmaxp    VOp = 0x6E20A400
	// The products of the lower (VQ clear) or upper halves' lanes, widened.
	VSmull VOp = 0x0E20C000
	VUmull VOp = 0x2E20C000
)

// The bitwise operations, on bytes (V3 with size V8): Vd = Vn op Vm; BSL
// takes each bit of Vn where Vd's is set, else of Vm.
const (
	VAnd VOp = 0x4E201C00
	VBic VOp = 0x4E601C00 // Vn & ^Vm
	VOrr VOp = 0x4EA01C00
	VEor VOp = 0x6E201C00
	VBsl VOp = 0x6E601C00
)

// The float operations on three registers, V32 or V64.
const (
	VFadd  VOp = 0x4E20D400
	VFsub  VOp = 0x4EA0D400
	VFmul  VOp = 0x6E20DC00
	VFdiv  VOp = 0x6E20FC00
	VFmax  VOp = 0x4E20F400
	VFmin  VOp = 0x4EA0F400
	VFcmeq VOp = 0x4E20E400
	VFcmge VOp = 0x6E20E400
	VFcmgt VOp = 0x6EA0E400
	VFmla  VOp = 0x4E20CC00 // Vd += Vn * Vm, fused
	VFmls  VOp = 0x4EA0CC00 // Vd -= Vn * Vm, fused
)

// The operations on two registers, Vd = op Vn (V2).
const (
	VCnt    VOp = 0x4E205800 // V8 only
	VNot    VOp = 0x6E205800 // V8 only
	VAbs    VOp = 0x4E20B800
	VNeg    VOp = 0x6E20B800
	VCmeq0  VOp = 0x4E209800 // each lane all ones where it is 0
	VCmlt0  VOp = 0x4E20A800 // where it is negative
	VSaddlp VOp = 0x4E202800 // the sum of each pair of lanes, widened
	VUaddlp VOp = 0x6E202800
	VAddv   VOp = 0x4E31B800 // the sum of the lanes, into lane 0, the rest clear
	VUmaxv  VOp = 0x6E30A800
	VUminv  VOp = 0x6E31A800
	// The narrowings of Vn's lanes of twice the size into Vd's lower half
	// (VQ clear, the upper half cleared) or upper half (VQ set): Sqxtn
	// saturating signed to signed, Uqxtn unsigned to unsigned, Sqxtun
	// signed to unsigned, Xtn truncating. The size is the result's.
	VXtn    VOp = 0x0E212800
	VSqxtn  VOp = 0x0E214800
	VUqxtn  VOp = 0x2E214800
	VSqxtun VOp = 0x2E212800
	// The float ones, V32 or V64.
	VFabs   VOp = 0x4EA0F800
	VFneg   VOp = 0x6EA0F800
	VFsqrt  VOp = 0x6EA1F800
	VFrintn VOp = 0x4E218800
	VFrintm VOp = 0x4E219800
	VFrintp VOp = 0x4EA18800
	VFrintz VOp = 0x4EA19800
	VFcvtzs VOp = 0x4EA1B800
	VFcvtzu VOp = 0x6EA1B800
	VScvtf  VOp = 0x4E21D800
	VUcvtf  VOp = 0x6E21D800
	// FCVTN narrows V64 to the two singles of Vd's lower half (VQ clear),
	// the upper half cleared; FCVTL widens the lower half's two singles to
	// doubles; both with V64, the double's size.
	VFcvtn VOp = 0x0E216800
	VFcvtl VOp = 0x0E217800
)

// V3 is Vd = Vn op Vm, lanes of size s.
func (a *Asm) V3(op VOp, s VSize, d, n, m FReg) {
	a.emit(uint32(op) | uint32(fsize(op, s))<<22 | uint32(m)<<16 | uint32(n)<<5 | uint32(d))
}

// V2 is Vd = op Vn, lanes of size s.
func (a *Asm) V2(op VOp, s VSize, d, n FReg) {
	a.emit(uint32(op) | uint32(fsize(op, s))<<22 | uint32(n)<<5 | uint32(d))
}

// fsize is the size field of op for lanes of size s: the float operations
// take only the bit of double precision.
func fsize(op VOp, s VSize) VSize {
	switch op | VQ {
	case VFadd, VFsub, VFmul, VFdiv, VFmax, VFmin, VFcmeq, VFcmge, VFcmgt, VFmla, VFmls,
		VFabs, VFneg, VFsqrt, VFrintn, VFrintm, VFrintp, VFrintz, VFcvtzs, VFcvtzu, VScvtf, VUcvtf,
		VFcvtn | VQ, VFcvtl | VQ:
		return s & 1
	}
	return s
}

// The shifts by an immediate (VShiftImm).
const (
	VShl  VOp = 0x4F005400
	VSshr VOp = 0x4F000400
	VUshr VOp = 0x6F000400
	// The widenings of the lower (VQ clear) or upper half's lanes, shifted
	// left: by 0, SXTL and UXTL. The size is the source's.
	VSshll VOp = 0x0F00A400
	VUshll VOp = 0x2F00A400
)

// VShiftImm shifts every lane of size s of Vn by n bits into Vd: left by 0
// to the lane's width less one, right by 1 to its width.
func (a *Asm) VShiftImm(op VOp, s VSize, d, src FReg, n uint8) {
	esize := uint32(8) << s
	var immhb uint32
	switch op &^ VQ {
	case VSshr &^ VQ, VUshr &^ VQ:
		immhb = 2*esize - uint32(n)
	default:
		immhb = esize + uint32(n)
	}
	a.emit(uint32(op) | immhb<<16 | uint32(src)<<5 | uint32(d))
}

// imm5 is the imm5 field of a lane of size s at index i.
func imm5(s VSize, i uint8) uint32 { return (uint32(i)<<1 | 1) << s }

// Dup is every lane of size s of Vd = the low bits of n (DUP, general).
func (a *Asm) Dup(s VSize, d FReg, n Reg) {
	a.emit(0x4E000C00 | imm5(s, 0)<<16 | uint32(n)<<5 | uint32(d))
}

// DupElem is every lane of size s of Vd = lane i of Vn (DUP, element).
func (a *Asm) DupElem(s VSize, d, n FReg, i uint8) {
	a.emit(0x4E000400 | imm5(s, i)<<16 | uint32(n)<<5 | uint32(d))
}

// DupScalar is Vd's lowest lane of size s = lane i of Vn, the rest of Vd
// cleared (MOV, scalar): a float out of a vector.
func (a *Asm) DupScalar(s VSize, d, n FReg, i uint8) {
	a.emit(0x5E000400 | imm5(s, i)<<16 | uint32(n)<<5 | uint32(d))
}

// InsG is lane i of size s of Vd = the low bits of n, the other lanes kept
// (INS, general).
func (a *Asm) InsG(s VSize, d FReg, i uint8, n Reg) {
	a.emit(0x4E001C00 | imm5(s, i)<<16 | uint32(n)<<5 | uint32(d))
}

// InsElem is lane i of size s of Vd = lane j of Vn, the other lanes kept
// (INS, element).
func (a *Asm) InsElem(s VSize, d FReg, i uint8, n FReg, j uint8) {
	a.emit(0x6E000400 | imm5(s, i)<<16 | uint32(j)<<s<<11 | uint32(n)<<5 | uint32(d))
}

// Umov is d = lane i of size s of Vn, zero-extended (UMOV): a W register
// for lanes of 8 to 32 bits, an X register for 64.
func (a *Asm) Umov(s VSize, d Reg, n FReg, i uint8) {
	q := uint32(0)
	if s == V64 {
		q = 1 << 30
	}
	a.emit(0x0E003C00 | q | imm5(s, i)<<16 | uint32(n)<<5 | uint32(d))
}

// Smov is d = lane i of size s (V8 or V16) of Vn, sign-extended to 32 bits,
// the upper half cleared (SMOV to a W register).
func (a *Asm) Smov(s VSize, d Reg, n FReg, i uint8) {
	a.emit(0x0E002C00 | imm5(s, i)<<16 | uint32(n)<<5 | uint32(d))
}

// Ext is Vd = the 16 bytes from byte i of Vn's bytes followed by Vm's.
func (a *Asm) Ext(d, n, m FReg, i uint8) {
	a.emit(0x6E000000 | uint32(m)<<16 | uint32(i)<<11 | uint32(n)<<5 | uint32(d))
}

// Tbl is Vd's byte k = byte Vm[k] of the table of regs registers from Vn
// (1 or 2: Vn and Vn+1), or 0 where the index is past it.
func (a *Asm) Tbl(d, n FReg, regs int, m FReg) {
	a.emit(0x4E000000 | uint32(regs-1)<<13 | uint32(m)<<16 | uint32(n)<<5 | uint32(d))
}

// The permutations of two registers' lanes (V3).
const (
	VZip1 VOp = 0x4E003800
	VZip2 VOp = 0x4E007800
	VUzp1 VOp = 0x4E001800
	VUzp2 VOp = 0x4E005800
)

// VMov is Vd = Vn, all 128 bits (ORR).
func (a *Asm) VMov(d, n FReg) { a.V3(VOrr, V8, d, n, n) }

// q is the size of a 16-byte load's or store's scaling.
const q size = 4

// LoadQ is Vd = the 16 bytes at [base + disp].
func (a *Asm) LoadQ(d FReg, base Reg, disp int32) {
	a.mem(0x3DC00000, 0x3CC00000, q, Reg(d), base, disp)
}

// StoreQ is [base + disp] = the 16 bytes of Vs.
func (a *Asm) StoreQ(base Reg, disp int32, s FReg) {
	a.mem(0x3D800000, 0x3C800000, q, Reg(s), base, disp)
}

// LoadQIdx and StoreQIdx load and store the 16 bytes at [base + idx].
func (a *Asm) LoadQIdx(d FReg, base, idx Reg) { a.memIdx(0x3DC00000, Reg(d), base, idx) }

func (a *Asm) StoreQIdx(base, idx Reg, s FReg) { a.memIdx(0x3D800000, Reg(s), base, idx) }

// LoadQLabel is Vd = the 16 bytes at the address l is bound at
// (LDR literal): a constant of the code's own, within 1 MB.
func (a *Asm) LoadQLabel(d FReg, l Label) {
	a.fixups = append(a.fixups, fixup{len(a.buf), l, fixCond})
	a.emit(0x9C000000 | uint32(d))
}

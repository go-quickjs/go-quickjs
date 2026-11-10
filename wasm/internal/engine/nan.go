package engine

import "unsafe"

// Which NaN a float operation returns.
//
// The specification lets an operation with a NaN operand return any NaN
// whose payload is one of its NaN operands' or the canonical one, so two
// implementations may differ and both be right; the interpreter and native
// code must not. Of two NaN operands, which one an instruction returns is
// the order of its operands, which Go chooses for a commutative operator
// as it compiles the interpreter; so the interpreter picks the NaN of an
// operation of two or three operands by a rule of its own, and the rule is
// what the host's instructions give with the operation's operands in their
// order, so that native code needs no instruction more than the
// operation's:
//
//   - add, sub, mul, div, min, max and their lanes return, of NaN operands,
//     one quieted (the payload's top bit set): on amd64 and 386
//     (nan_x86.go) the first operand's if it is a NaN, else the second's;
//     elsewhere (nan_other.go) a signaling NaN ahead of a quiet one, and of
//     two of a kind the first operand's.
//   - Of operands that are not NaNs (inf-inf, 0*inf, 0/0), they return the
//     host's default NaN, which no order changes: negative on amd64 and 386,
//     positive on arm64, its payload the quiet bit alone.
//   - A fused multiply-add (relaxed_madd, relaxed_nmadd where they fuse)
//     has a rule of its own: fmaNaN32 and fmaNaN64.
//
// Operations of one operand involve no choice: sqrt, the roundings,
// demote and promote return their NaN operand quieted, demote keeping its
// payload's top bits, sqrt of a negative number the default NaN;
// abs, neg and copysign are operations on the sign bit, which keep a NaN's
// payload and leave a signaling NaN signaling.
//
// Each operation computes its result as Go does and looks at it again only
// when it is a NaN, so that a result that is not costs one comparison. The
// helpers here are inlined where the interpreter's nodes call them, and
// read a float's bits in place rather than by math.Float32bits, which the
// inliner charges more for.

const (
	quietBit32 = 1 << 22
	quietBit64 = 1 << 51
)

// quiet32 is NaN a with its quiet bit set.
func quiet32(a float32) float32 {
	x := *(*uint32)(unsafe.Pointer(&a)) | quietBit32
	return *(*float32)(unsafe.Pointer(&x))
}

func quiet64(a float64) float64 {
	x := *(*uint64)(unsafe.Pointer(&a)) | quietBit64
	return *(*float64)(unsafe.Pointer(&x))
}

func fadd32(a, b float32) float32 {
	r := a + b
	if r != r && (a != a || b != b) {
		r = pick32(a, b)
	}
	return r
}

func fsub32(a, b float32) float32 {
	r := a - b
	if r != r && (a != a || b != b) {
		r = pick32(a, b)
	}
	return r
}

func fmul32(a, b float32) float32 {
	r := a * b
	if r != r && (a != a || b != b) {
		r = pick32(a, b)
	}
	return r
}

func fdiv32(a, b float32) float32 {
	r := a / b
	if r != r && (a != a || b != b) {
		r = pick32(a, b)
	}
	return r
}

func fadd64(a, b float64) float64 {
	r := a + b
	if r != r && (a != a || b != b) {
		r = pick64(a, b)
	}
	return r
}

func fsub64(a, b float64) float64 {
	r := a - b
	if r != r && (a != a || b != b) {
		r = pick64(a, b)
	}
	return r
}

func fmul64(a, b float64) float64 {
	r := a * b
	if r != r && (a != a || b != b) {
		r = pick64(a, b)
	}
	return r
}

func fdiv64(a, b float64) float64 {
	r := a / b
	if r != r && (a != a || b != b) {
		r = pick64(a, b)
	}
	return r
}

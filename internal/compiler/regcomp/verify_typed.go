package regcomp

import (
	"fmt"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The kinds verifier (docs/register-interpreter.md, "The typed table"):
// typed code's every operand of the kind its operation takes, each
// register of one kind for the whole code, so that no join can mix kinds
// and a raw word reaches a Value only by a box.
//
//	K1  each operand's register is of the kind its operation's signature
//	    says (tsig), and below NumRegs;
//	K2  the frame's head -- the function, its this, its parameters -- is
//	    of Values, which nothing writes;
//	K3  a call's window is of Values;
//	K4  nothing raw is at or past a call's window, where its callee's
//	    frame is;
//	K5  the code holds typed operations and the ordinary ones typed code
//	    may (bytecode.TypedPlainOps) alone, and no handler;
//	K6  a raw word reaches a Value only by a box (no move of one into a
//	    Value);
//	K8  every jump lands in the code, which does not run past its end;
//	    every constant, upvalue and call site exists.

// tsig is a typed operation's signature: the kinds of A, B and C (none
// where kNone), whether it writes A, and whether it branches by SC.
type tsig struct {
	a, b, c bytecode.RegKind
	writes  bool
	branch  bool
	// aImm, bImm and cImm say the operand is no register.
	aImm, bImm, cImm bool
}

const kNone = bytecode.RegKind(0xff)

var (
	vI = bytecode.RegInt
	vF = bytecode.RegFloat
	vV = bytecode.RegValue
)

// typedSigs are the typed operations' signatures; TMov's is checked on
// its own.
var typedSigs = func() map[bytecode.ROp]tsig {
	m := map[bytecode.ROp]tsig{}
	set := func(s tsig, ops ...bytecode.ROp) {
		for _, op := range ops {
			m[op] = s
		}
	}
	none := func(s tsig) tsig {
		if s.a == kNone {
			s.aImm = true
		}
		if s.b == kNone {
			s.bImm = true
		}
		if s.c == kNone {
			s.cImm = true
		}
		return s
	}
	set(none(tsig{a: vI, b: kNone, c: kNone, writes: true}), bytecode.TIConst, bytecode.TGGetI)
	set(none(tsig{a: vF, b: kNone, c: kNone, writes: true}), bytecode.TFConst, bytecode.TGGetF)
	set(none(tsig{a: vI, b: kNone, c: kNone}), bytecode.TGSetI)
	set(none(tsig{a: vF, b: kNone, c: kNone}), bytecode.TGSetF)
	set(none(tsig{a: vI, b: vV, c: kNone, writes: true}), bytecode.TConvI)
	set(none(tsig{a: vF, b: vV, c: kNone, writes: true}), bytecode.TConvF)
	set(none(tsig{a: vV, b: vI, c: kNone, writes: true}), bytecode.TIBox, bytecode.TUBox)
	set(none(tsig{a: vV, b: vF, c: kNone, writes: true}), bytecode.TFBox)
	set(tsig{a: vI, b: vI, c: vI, writes: true}, bytecode.TAdd, bytecode.TSub, bytecode.TMul, bytecode.TDivS,
		bytecode.TDivU, bytecode.TRemS, bytecode.TRemU, bytecode.TAnd, bytecode.TOr, bytecode.TXor, bytecode.TShl,
		bytecode.TShr, bytecode.TUShr, bytecode.TMinS, bytecode.TMaxS, bytecode.TLtS, bytecode.TLeS, bytecode.TLtU,
		bytecode.TLeU, bytecode.TEq, bytecode.TNe)
	set(none(tsig{a: vI, b: vI, c: kNone, writes: true}), bytecode.TAddI, bytecode.TAndI, bytecode.TOrI, bytecode.TXorI,
		bytecode.TShlI, bytecode.TShrI, bytecode.TUShrI, bytecode.TNeg, bytecode.TNot, bytecode.TLNot, bytecode.TClz,
		bytecode.TAbs)
	set(none(tsig{a: vI, b: vI, c: kNone, branch: true}), bytecode.TJLtS, bytecode.TJLeS, bytecode.TJLtU,
		bytecode.TJLeU, bytecode.TJEq, bytecode.TJNe)
	set(none(tsig{a: vI, b: kNone, c: kNone, branch: true}), bytecode.TJLtSI, bytecode.TJLeSI, bytecode.TJGtSI,
		bytecode.TJGeSI, bytecode.TJEqI, bytecode.TJNeI, bytecode.TJZ, bytecode.TJNZ)
	set(tsig{a: vF, b: vF, c: vF, writes: true}, bytecode.TFAdd, bytecode.TFSub, bytecode.TFMul, bytecode.TFDiv,
		bytecode.TFMod, bytecode.TFMath2, bytecode.TFMin, bytecode.TFMax)
	set(none(tsig{a: vF, b: vF, c: kNone, writes: true}), bytecode.TFNeg, bytecode.TFAbs, bytecode.TFSqrt,
		bytecode.TFRound, bytecode.TFMath1)
	set(tsig{a: vI, b: vF, c: vF, writes: true}, bytecode.TFLt, bytecode.TFLe, bytecode.TFEq, bytecode.TFNe)
	set(none(tsig{a: vF, b: vF, c: kNone, branch: true}), bytecode.TJFLt, bytecode.TJFNLt, bytecode.TJFLe,
		bytecode.TJFNLe, bytecode.TJFEq, bytecode.TJFNe)
	set(none(tsig{a: vF, b: vI, c: kNone, writes: true}), bytecode.TI2FS, bytecode.TI2FU)
	set(none(tsig{a: vI, b: vF, c: kNone, writes: true}), bytecode.TF2I)
	set(none(tsig{a: vI, b: vI, c: kNone, writes: true}), bytecode.TLd8S, bytecode.TLd8U, bytecode.TLd16S,
		bytecode.TLd16U, bytecode.TLd32)
	set(none(tsig{a: vF, b: vI, c: kNone, writes: true}), bytecode.TLdF32, bytecode.TLdF64)
	set(none(tsig{a: vI, b: vI, c: kNone}), bytecode.TSt8, bytecode.TSt16, bytecode.TSt32)
	set(none(tsig{a: vF, b: vI, c: kNone}), bytecode.TStF32, bytecode.TStF64)
	set(none(tsig{a: vV, b: vI, c: kNone, writes: true}), bytecode.TTabGet)
	return m
}()

// VerifyKinds checks typed code rc of function fn by the rules K1 to K8.
func VerifyKinds(rc *bytecode.RegCode, fn *bytecode.Function) error {
	if !rc.Typed {
		return nil
	}
	fail := func(pc int, format string, args ...any) error {
		return fmt.Errorf("typed code of %s: pc %d (%s): %s", fn.Name, pc, rc.Format(pc), fmt.Sprintf(format, args...))
	}
	n := rc.NumRegs
	if len(rc.Kinds) != n || n > 1<<16 || len(rc.Lines) == 0 || rc.Lines[0].PC != 0 || len(rc.Code) == 0 {
		return fmt.Errorf("typed code of %s: its tables do not fit its code", fn.Name)
	}
	if len(rc.Handlers) > 0 || rc.LocalReg != nil || rc.Caps != nil || rc.UsesArguments || rc.ForwardsArguments || rc.KeepsArgs || rc.ThisRef {
		return fmt.Errorf("typed code of %s: what reads its frame generically", fn.Name)
	}
	head := 2 + rc.Params
	for r := 0; r < head && r < n; r++ {
		if rc.Kinds[r] != bytecode.RegValue {
			return fmt.Errorf("typed code of %s: its head's register %d is raw", fn.Name, r)
		}
	}
	plain := map[bytecode.ROp]bool{}
	for _, op := range bytecode.TypedPlainOps {
		plain[op] = true
	}
	kind := func(r uint16) bytecode.RegKind {
		if int(r) >= n {
			return kNone
		}
		return rc.Kinds[r]
	}
	jumpOK := func(pc int, off int32) bool {
		to := pc + 1 + int(off)
		return to >= 0 && to < len(rc.Code)
	}
	for pc, w := range rc.Code {
		op := w.Op()
		a, b, c := w.A(), w.B(), w.C()
		if op.Typed() {
			if op == bytecode.TMov {
				if k := kind(a); k == kNone || k == bytecode.RegValue || kind(b) != k {
					return fail(pc, "K1: a move of raw words not of one kind")
				}
				if int(a) < head {
					return fail(pc, "K2: a raw word into the frame's head")
				}
				continue
			}
			s, ok := typedSigs[op]
			if !ok {
				return fail(pc, "K5: no signature")
			}
			for i, x := range []struct {
				r    uint16
				k    bytecode.RegKind
				none bool
			}{{a, s.a, s.aImm}, {b, s.b, s.bImm}, {c, s.c, s.cImm}} {
				if !x.none && kind(x.r) != x.k {
					return fail(pc, "K1: operand %c is r%d, of kind %d, not %d", "ABC"[i], x.r, kind(x.r), x.k)
				}
			}
			if s.writes && int(a) < head {
				return fail(pc, "K2: a write of the frame's head")
			}
			if s.branch && !jumpOK(pc, w.SC()) {
				return fail(pc, "K8: a branch out of the code")
			}
			switch op {
			case bytecode.TFConst:
				if int(w.BC()) >= len(rc.Floats) {
					return fail(pc, "K8: no such constant")
				}
			case bytecode.TGGetI, bytecode.TGSetI, bytecode.TGGetF, bytecode.TGSetF:
				if int(w.BC()) >= len(fn.Upvalues) {
					return fail(pc, "K8: no such upvalue")
				}
			case bytecode.TTabGet:
				if int(c) >= len(fn.Upvalues) || int(c) >= len(rc.Asm) || rc.Asm[c].Kind != bytecode.AsmUpTable {
					return fail(pc, "K8: no such table")
				}
			case bytecode.TFMath1:
				if w.X() > bytecode.TMathLog {
					return fail(pc, "K8: no such function")
				}
			case bytecode.TFMath2:
				if w.X() != bytecode.TMathPow && w.X() != bytecode.TMathAtan2 {
					return fail(pc, "K8: no such function")
				}
			}
			continue
		}
		if !plain[op] {
			return fail(pc, "K5: an operation typed code may not hold")
		}
		switch op {
		case bytecode.RMove:
			if kind(a) != bytecode.RegValue || kind(b) != bytecode.RegValue {
				return fail(pc, "K6: a move of what is not a Value")
			}
			if int(a) < head {
				return fail(pc, "K2: a write of the frame's head")
			}
		case bytecode.RLoadInt, bytecode.RLoadUndef:
			if kind(a) != bytecode.RegValue || int(a) < head {
				return fail(pc, "K1: a load into what is not a Value or into the head")
			}
		case bytecode.RGetUpval:
			if kind(a) != bytecode.RegValue || int(a) < head || int(w.BC()) >= len(fn.Upvalues) {
				return fail(pc, "K1: an upvalue into what is not a Value")
			}
		case bytecode.RJmp:
			if !jumpOK(pc, int32(w.BC())) {
				return fail(pc, "K8: a jump out of the code")
			}
		case bytecode.RRet:
			if kind(a) != bytecode.RegValue {
				return fail(pc, "K6: a return of a raw word")
			}
		case bytecode.RCallW:
			if int(c) >= rc.CallSites {
				return fail(pc, "K8: no such call site")
			}
			if kind(b) != bytecode.RegValue || int(b) < head {
				return fail(pc, "K3: a call's value into what is not a Value")
			}
			// K3 and K4: the window and everything past it Values.
			if int(a) < head || int(a)+2+int(w.X()) > n {
				return fail(pc, "K3: a window out of the frame")
			}
			for r := int(a); r < n; r++ {
				if rc.Kinds[r] != bytecode.RegValue {
					return fail(pc, "K4: register r%d past the window is raw", r)
				}
			}
		}
	}
	switch rc.Code[len(rc.Code)-1].Op() {
	case bytecode.RRet, bytecode.RRetUndef, bytecode.RJmp:
	default:
		return fmt.Errorf("typed code of %s: K8: it runs past its end", fn.Name)
	}
	return nil
}

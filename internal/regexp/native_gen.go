//go:build quickjs_jit

package regexp

import (
	"encoding/binary"
	"math"
)

// The code of a pattern's program, made for one kind of subject: an ASCII
// string's bytes, or a string's UTF-16 code units; for amd64 or arm64, by
// one generator writing through the macro assembler (native_masm.go).
//
// The code is the program's instructions one by one, each what run does
// for it, in registers:
//
//   - the position is a pointer into the subject (rCur), the subject's
//     start and end beside it (rSubj, rEnd); a unit is a byte or two;
//   - backtracking has a stack of its own, of words: what a choice point
//     goes on with is the address of the code that resumes it, under what it
//     needs -- a position, a greedy run's bounds, as offsets in bytes -- and
//     a write of a capture, a counter or an empty-check mark pushes the code
//     that puts the old value back under it, as V8's code does, rather than
//     a trail. To backtrack is to pop an address and jump there; the stack's
//     bottom is the address of the attempt's failure;
//   - the steps are counted down (rLeft) from the most this run of the code
//     may take: each instruction is one, at its start, as run counts it, and
//     what a run of characters, a greedy run, a backreference or a lazy
//     run's next character reads is counted where run counts it, so that the
//     count is run's exactly and the step budget runs out at the same
//     instruction.
//
// An instruction that reads a character tests it against the code points
// it matches (native_sets.go): by comparisons where they are, or are not, a
// few runs, by a table of 128 bytes for ASCII, by a table of bits for the
// Basic Multilingual Plane and by a tree of comparisons past it. Over an
// ASCII subject a unit is a character, as it is over UTF-16 without the
// unicode flag; with it, a pair of surrogates is one character, read as one
// only where the instruction matches a surrogate or a code point past the
// BMP -- for one that matches neither, a unit it matches is a whole
// character, and a surrogate fails it as the pair would.
//
// What the code does not do -- a lookaround, an instruction that reads
// leftwards, a backreference under the i flag over UTF-16 -- has the
// program refused for that kind of subject, and matched by the matcher.

// nativeProgram is a pattern's code, made.
type nativeProgram struct {
	bytes   []byte
	maxPush int
	// text is how many of the bytes are instructions; the tables follow.
	text int
}

// nativeGen makes a program's code.
type nativeGen struct {
	m     masm
	prog  *program
	code  []instr
	ncaps int
	// wide marks code over UTF-16 units, and unicode an input that reads a
	// pair of surrogates as one character (the u flag); us is a unit's
	// bytes, and shift its log2.
	wide, unicode bool
	us            int32
	shift         uint8
	// bad marks code that could not be made.
	bad bool
	// at is each instruction's start, its stack check and its step; body
	// is past them.
	at, body []label
	push     []int
	bt       label
	fail     label
	matched  label
	exit     label
	// dataStart is where the tables begin, past every instruction.
	dataStart label
	// cold is the code placed after the instructions': exits, and what
	// choice points and undoings resume at; data the tables after it.
	cold                       []func()
	data                       map[string]label
	dataOrder                  []string
	undoCaps, undoCtrs, undoMk map[int]label
}

// genNative makes the code of prog for arch, whose matcher has ncaps
// capture slots, over UTF-16 units where wide is set and else over ASCII
// bytes, the input reading pairs of surrogates as one character where
// unicode is set; or reports that it is not code native code is made for.
func genNative(arch string, prog *program, ncaps int, wide, unicode bool) (nativeProgram, bool) {
	m := newMasm(arch)
	if m == nil {
		return nativeProgram{}, false
	}
	g := &nativeGen{m: m, prog: prog, code: prog.code, ncaps: ncaps, wide: wide, unicode: unicode && wide, us: 1,
		data: map[string]label{}, undoCaps: map[int]label{}, undoCtrs: map[int]label{}, undoMk: map[int]label{}}
	if wide {
		g.us, g.shift = 2, 1
	}
	maxPush, ok := g.check()
	if !ok {
		return nativeProgram{}, false
	}
	g.emit()
	if g.bad {
		return nativeProgram{}, false
	}
	b, err := g.m.Finish()
	if err != nil || len(b) > maxNativeBytes {
		return nativeProgram{}, false
	}
	return nativeProgram{bytes: append([]byte(nil), b...), maxPush: maxPush, text: g.m.Offset(g.dataStart)}, true
}

// isUnit reports whether op reads one character, as the item of a greedy
// or a lazy run does.
func isUnit(op opcode) bool {
	switch op {
	case opChar, opCharFold, opClass, opAny, opAnyNotNL:
		return true
	}
	return false
}

// check reports whether the code can be made, and the most words one
// instruction pushes; it notes each's.
func (g *nativeGen) check() (int, bool) {
	code := g.code
	if len(code) == 0 || len(code) > 1<<14 || g.ncaps > 1<<20 {
		return 0, false
	}
	g.push = make([]int, len(code))
	fits := func(n int) bool { return n >= 0 && n <= math.MaxInt32/8 }
	target := func(t int) bool { return t >= 0 && t < len(code) }
	maxPush := 1
	for pc, in := range code {
		if in.rev {
			return 0, false
		}
		push := 0
		switch in.op {
		case opChar, opCharFold, opClass, opAny, opAnyNotNL, opMatch,
			opAssertStart, opAssertEnd, opWordBoundary, opNotWordBoundary:
		case opString:
			if in.arg < 0 || in.arg >= len(g.prog.runs) || len(g.prog.runs[in.arg].units) > 1<<16 {
				return 0, false
			}
		case opSplit:
			if !target(in.arg) || !target(in.arg2) || int(in.quick) > len(g.prog.firsts) {
				return 0, false
			}
			push = 2
		case opJmp:
			if !target(in.arg) {
				return 0, false
			}
		case opSave:
			if in.arg < 0 || in.arg >= g.ncaps {
				return 0, false
			}
			push = 2
		case opBackref, opBackrefFold:
			if in.op == opBackrefFold && g.wide {
				// Past ASCII, characters are the same up to case as canonical
				// says, which the code does not ask.
				return 0, false
			}
			if in.arg < 0 || in.arg2 < 0 || in.arg2 > len(g.prog.refSets) || !target(pc+1) {
				return 0, false
			}
			if in.arg2 != 0 {
				for _, k := range g.prog.refSets[in.arg2-1] {
					if k < 0 || 2*k >= g.ncaps {
						return 0, false
					}
				}
			}
		case opCounterInit:
			if in.arg < 0 || in.arg >= g.prog.counters {
				return 0, false
			}
		case opCounterInc:
			if in.arg < 0 || in.arg >= g.prog.counters || !fits(in.arg2) || !target(pc+2) ||
				!target((*matcher)(nil).exitOfCounterLoop(code, pc)) {
				return 0, false
			}
			push = 2
		case opClearCaps:
			if in.arg < 0 || in.arg > in.arg2 || 2*in.arg2+1 >= g.ncaps {
				return 0, false
			}
			push = 4 * (in.arg2 - in.arg + 1)
		case opEmptyCheck:
			if in.arg < 0 || in.arg >= g.prog.emptyChecks {
				return 0, false
			}
			if in.arg2 == 0 {
				push = 2
			}
		case opGreedy, opGreedyRune:
			if !target(pc+2) || !isUnit(code[pc+1].op) || code[pc+1].rev || !fits(in.arg) {
				return 0, false
			}
			push = 3
		case opLazy:
			if !target(pc+2) || !isUnit(code[pc+1].op) || code[pc+1].rev || in.arg < 0 || in.arg > 1 {
				return 0, false
			}
			push = 2
		default:
			// A lookaround, or anything else the code does not do.
			return 0, false
		}
		g.push[pc] = push
		maxPush = max(maxPush, push)
	}
	return maxPush, true
}

// emit makes the code: the entry, each instruction's, then the cold code
// and the tables.
func (g *nativeGen) emit() {
	a := g.m
	g.at = make([]label, len(g.code))
	g.body = make([]label, len(g.code))
	for pc := range g.code {
		g.at[pc], g.body[pc] = a.NewLabel(), a.NewLabel()
	}
	g.bt, g.fail, g.matched, g.exit, g.dataStart = a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel(), a.NewLabel()

	// The entry loads the registers from the context, and goes on where it
	// says, or begins an attempt, pushing its failure.
	a.Load(rSubj, rCtx, offSubject)
	a.Load(rEnd, rCtx, offEnd)
	a.Load(rCaps, rCtx, offCaps)
	a.Load(rTop, rCtx, offStackBase)
	a.Load(rA, rCtx, offTop)
	a.Op(aluAdd, rTop, rA, true)
	a.Load(rCur, rCtx, offCur)
	a.Op(aluAdd, rCur, rSubj, true)
	a.Load(rLeft, rCtx, offLeft)
	a.Load(rA, rCtx, offResume)
	a.Op(aluTest, rA, rA, true)
	start := a.NewLabel()
	a.Jcc(condE, start)
	a.JmpReg(rA)
	a.Bind(start)
	a.LeaLabel(rA, g.fail)
	a.Store(rTop, 0, rA)
	a.OpImm(aluAdd, rTop, 8, true)

	for pc := range g.code {
		g.instr(pc)
	}

	// Backtracking pops where to go on and goes there.
	a.Bind(g.bt)
	a.OpImm(aluSub, rTop, 8, true)
	a.JmpMem(rTop, 0)
	a.Bind(g.fail)
	a.MovImm(rA, nativeFailed)
	a.Jmp(g.exit)
	a.Bind(g.matched)
	a.MovImm(rA, nativeMatched)
	a.Jmp(g.exit)
	// The exit writes back where the code is, its status in rA.
	a.Bind(g.exit)
	a.Store(rCtx, offStatus, rA)
	a.Op(aluSub, rCur, rSubj, true)
	a.Store(rCtx, offCur, rCur)
	a.Store(rCtx, offLeft, rLeft)
	a.Load(rA, rCtx, offStackBase)
	a.Op(aluSub, rTop, rA, true)
	a.Store(rCtx, offTop, rTop)
	a.Ret()

	for i := 0; i < len(g.cold); i++ {
		g.cold[i]()
	}
	a.Bind(g.dataStart)
	for _, d := range g.dataOrder {
		a.Bind(g.data[d])
		b := []byte(d)
		for len(b)%8 != 0 {
			b = append(b, 0)
		}
		a.Data(b)
	}
}

// dataAt is where the code's data b is, placed after the code once however
// many tests use it.
func (g *nativeGen) dataAt(b []byte) label {
	key := string(b)
	if l, ok := g.data[key]; ok {
		return l
	}
	l := g.m.NewLabel()
	g.data[key] = l
	g.dataOrder = append(g.dataOrder, key)
	return l
}

// exitTo returns to Go with status, to go on at resume when entered again.
func (g *nativeGen) exitTo(resume label, status uint64) {
	a := g.m
	a.LeaLabel(rA, resume)
	a.Store(rCtx, offResume, rA)
	a.MovImm(rA, status)
	a.Jmp(g.exit)
}

// later places f's code after the instructions'.
func (g *nativeGen) later(f func()) { g.cold = append(g.cold, f) }

// prologue is an instruction's start: room on the stack for what it
// pushes, and its step.
func (g *nativeGen) prologue(pc int) {
	a := g.m
	a.Bind(g.at[pc])
	if g.push[pc] > 0 {
		grow := a.NewLabel()
		a.CmpMem(rTop, rCtx, offStackLimit, true)
		a.Jcc(condA, grow)
		g.later(func() {
			a.Bind(grow)
			g.exitTo(g.at[pc], nativeGrow)
		})
	}
	limit := a.NewLabel()
	a.OpImm(aluSub, rLeft, 1, true)
	a.Jcc(condS, limit)
	g.later(func() {
		a.Bind(limit)
		g.exitTo(g.body[pc], nativeLimit)
	})
	a.Bind(g.body[pc])
}

// charTest is what the code tests a character an instruction reads
// against.
type charTest struct {
	// ascii is the bytes it matches, over bytes.
	ascii [128]bool
	// spans is the units it matches over UTF-16, or with decode the code
	// points.
	spans []span
	// decode reads a pair of surrogates as one character.
	decode bool
	// all marks a test every character passes.
	all bool
}

// charTestOf is the test of the one-character instruction in, which reads
// a code point where points is set and the input reads them, else a unit.
func (g *nativeGen) charTestOf(in instr, points bool) charTest {
	var t charTest
	if !g.wide {
		m := &matcher{prog: g.prog}
		t.all = true
		for b := range t.ascii {
			t.ascii[b] = m.unitMatches(in, uint16(b))
			t.all = t.all && t.ascii[b]
		}
		return t
	}
	points = points && g.unicode
	top := 0xFFFF
	if points {
		top = 0x10FFFF
	}
	sp, ok := itemSpans(g.prog, in, top)
	if !ok {
		g.bad = true
	}
	t.all = len(complementSpans(sp, 0, top)) == 0
	t.decode = points && (len(clipSpans(sp, 0xD800, 0xDFFF)) > 0 || len(clipSpans(sp, 0x10000, top)) > 0)
	t.spans = sp
	if !t.decode {
		t.spans = clipSpans(sp, 0, 0xFFFF)
	}
	return t
}

// load is dst = the unit at base+disp.
func (g *nativeGen) load(dst, base vreg, disp int32) {
	if g.wide {
		g.m.LoadU16(dst, base, disp)
		return
	}
	g.m.LoadU8(dst, base, disp)
}

// readChar is rB = the character at ptr, below the subject's end, as a code
// point -- a pair of surrogates is one -- and rW its width in bytes.
func (g *nativeGen) readChar(ptr vreg) {
	a := g.m
	done := a.NewLabel()
	a.LoadU16(rB, ptr, 0)
	a.MovImm(rW, 2)
	a.MovRR32(rT, rB)
	a.OpImm(aluSub, rT, 0xD800, false)
	a.OpImm(aluCmp, rT, 0x3FF, false)
	a.Jcc(condA, done)
	a.Lea(rT, ptr, 4)
	a.Op(aluCmp, rT, rEnd, true)
	a.Jcc(condA, done)
	a.LoadU16(rT, ptr, 2)
	a.OpImm(aluSub, rT, 0xDC00, false)
	a.OpImm(aluCmp, rT, 0x3FF, false)
	a.Jcc(condA, done)
	a.OpImm(aluSub, rB, 0xD800, false)
	a.ShiftImm(shiftLeft, rB, 10, false)
	a.Op(aluAdd, rB, rT, false)
	a.OpImm(aluAdd, rB, 0x10000, false)
	a.MovImm(rW, 4)
	a.Bind(done)
}

// matchChar goes to fail unless the character at ptr is one t passes, and
// moves ptr past it. It writes rB, rW, rT and rD.
func (g *nativeGen) matchChar(t *charTest, ptr vreg, fail label) {
	a := g.m
	a.Op(aluCmp, ptr, rEnd, true)
	a.Jcc(condAE, fail)
	switch {
	case !g.wide:
		if !t.all {
			a.LoadU8(rB, ptr, 0)
			g.test128(&t.ascii, rB, fail)
		}
		a.OpImm(aluAdd, ptr, 1, true)
	case !t.decode:
		if !t.all {
			a.LoadU16(rB, ptr, 0)
			g.testSpans(t.spans, 0, 0xFFFF, rB, fail)
		}
		a.OpImm(aluAdd, ptr, 2, true)
	default:
		g.readChar(ptr)
		if !t.all {
			g.testPoint(t.spans, rB, fail)
		}
		a.Op(aluAdd, ptr, rW, true)
	}
}

// testUnit goes to fail unless the unit in r is in sp.
func (g *nativeGen) testUnit(sp []span, r vreg, fail label) {
	if !g.wide {
		set := bools128(sp)
		g.test128(&set, r, fail)
		return
	}
	g.testSpans(sp, 0, 0xFFFF, r, fail)
}

// testPoint goes to fail unless the code point in r is in sp.
func (g *nativeGen) testPoint(sp []span, r vreg, fail label) {
	a := g.m
	supp, ok := a.NewLabel(), a.NewLabel()
	a.OpImm(aluCmp, r, 0x10000, false)
	a.Jcc(condAE, supp)
	g.testSpans(sp, 0, 0xFFFF, r, fail)
	a.Jmp(ok)
	a.Bind(supp)
	g.testSpans(sp, 0x10000, 0x10FFFF, r, fail)
	a.Bind(ok)
}

// test128 goes to fail unless the byte in r is in set: by comparisons
// where it is, or is not, a few runs of bytes, else from a table of the
// set's. It writes rT.
func (g *nativeGen) test128(set *[128]bool, r vreg, fail label) {
	a := g.m
	in := spansOf128(set)
	out := complementSpans(in, 0, 127)
	switch {
	case len(out) == 0:
	case len(in) == 0:
		a.Jmp(fail)
	case len(in) <= 2 && len(in) <= len(out):
		g.chain(in, r, fail, true)
	case len(out) <= 2:
		g.chain(out, r, fail, false)
	default:
		var b [128]byte
		for i, v := range set {
			if v {
				b[i] = 1
			}
		}
		a.LeaLabel(rT, g.dataAt(b[:]))
		a.Op(aluAdd, rT, r, true)
		a.LoadU8(rT, rT, 0)
		a.Op(aluTest, rT, rT, false)
		a.Jcc(condE, fail)
	}
}

// chain compares r with each of a few runs: with inside, it goes to fail
// unless r is in one; without, if it is. It writes rT.
func (g *nativeGen) chain(sp []span, r vreg, fail label, inside bool) {
	a := g.m
	ok := a.NewLabel()
	for i, s := range sp {
		in, out := condBE, condA
		if s.lo == s.hi {
			a.OpImm(aluCmp, r, int32(s.lo), false)
			in, out = condE, condNE
		} else {
			a.MovRR32(rT, r)
			a.OpImm(aluSub, rT, int32(s.lo), false)
			a.OpImm(aluCmp, rT, int32(s.hi-s.lo), false)
		}
		switch {
		case !inside:
			a.Jcc(in, fail)
		case i == len(sp)-1:
			a.Jcc(out, fail)
		default:
			a.Jcc(in, ok)
		}
	}
	a.Bind(ok)
}

// testSpans goes to fail unless r, which is in lo to hi, is in sp: by
// comparisons where it is, or is not, in a few runs; ASCII by a table of
// its own; the rest of the BMP by a table of bits, or a tree of comparisons
// where there are not many runs; past the BMP by a tree. It writes rT and
// rD.
func (g *nativeGen) testSpans(sp []span, lo, hi int, r vreg, fail label) {
	a := g.m
	in := clipSpans(sp, lo, hi)
	out := complementSpans(in, lo, hi)
	switch {
	case len(out) == 0:
	case len(in) == 0:
		a.Jmp(fail)
	case len(in) <= 4 && len(in) <= len(out):
		g.chain(in, r, fail, true)
	case len(out) <= 4:
		g.chain(out, r, fail, false)
	case lo == 0 && hi >= 0x80:
		high, ok := a.NewLabel(), a.NewLabel()
		a.OpImm(aluCmp, r, 0x80, false)
		a.Jcc(condAE, high)
		set := bools128(in)
		g.test128(&set, r, fail)
		a.Jmp(ok)
		a.Bind(high)
		g.testSpans(in, 0x80, hi, r, fail)
		a.Bind(ok)
	case hi <= 0xFFFF && min(len(in), len(out)) > 16:
		g.bmpTable(in, r, fail)
	default:
		ok := a.NewLabel()
		if len(in) <= len(out) {
			g.tree(in, r, ok, fail)
		} else {
			g.tree(out, r, fail, ok)
		}
		a.Bind(ok)
	}
}

// tree goes to hit if r is in sp, else to miss, by halving the runs.
func (g *nativeGen) tree(sp []span, r vreg, hit, miss label) {
	a := g.m
	if len(sp) <= 3 {
		for _, s := range sp {
			if s.lo == s.hi {
				a.OpImm(aluCmp, r, int32(s.lo), false)
				a.Jcc(condE, hit)
				continue
			}
			a.MovRR32(rT, r)
			a.OpImm(aluSub, rT, int32(s.lo), false)
			a.OpImm(aluCmp, rT, int32(s.hi-s.lo), false)
			a.Jcc(condBE, hit)
		}
		a.Jmp(miss)
		return
	}
	mid := len(sp) / 2
	right := a.NewLabel()
	a.OpImm(aluCmp, r, int32(sp[mid].lo), false)
	a.Jcc(condAE, right)
	g.tree(sp[:mid], r, hit, miss)
	a.Bind(right)
	g.tree(sp[mid:], r, hit, miss)
}

// bmpTable goes to fail unless r, below U+10000, is in sp, from a table of
// its bits: blocks of 256, each distinct one kept once, as bmp.go's.
func (g *nativeGen) bmpTable(sp []span, r vreg, fail label) {
	a := g.m
	var bits [1 << 10]uint64
	for _, s := range clipSpans(sp, 0, 0xFFFF) {
		for c := s.lo; c <= s.hi; c++ {
			bits[c>>6] |= 1 << (c & 63)
		}
	}
	var index [256]byte
	var blocks []byte
	seen := map[[4]uint64]byte{}
	for b := 0; b < 256; b++ {
		blk := [4]uint64(bits[4*b : 4*b+4])
		i, ok := seen[blk]
		if !ok {
			i = byte(len(seen))
			seen[blk] = i
			for _, w := range blk {
				blocks = binary.LittleEndian.AppendUint64(blocks, w)
			}
		}
		index[b] = i
	}
	a.MovRR32(rT, r)
	a.ShiftImm(shiftRight, rT, 8, false)
	a.LeaLabel(rD, g.dataAt(index[:]))
	a.Op(aluAdd, rD, rT, true)
	a.LoadU8(rT, rD, 0)
	a.ShiftImm(shiftLeft, rT, 5, false)
	a.LeaLabel(rD, g.dataAt(blocks))
	a.Op(aluAdd, rD, rT, true)
	a.MovRR32(rT, r)
	a.ShiftImm(shiftRight, rT, 6, false)
	a.OpImm(aluAnd, rT, 3, false)
	a.ShiftImm(shiftLeft, rT, 3, false)
	a.Op(aluAdd, rD, rT, true)
	a.Load(rD, rD, 0)
	a.JmpBitClear(rD, r, fail)
}

// pushPos pushes a choice point: the position, under resume's address.
func (g *nativeGen) pushPos(resume label) {
	a := g.m
	a.MovRR(rA, rCur)
	a.Op(aluSub, rA, rSubj, true)
	a.Store(rTop, 0, rA)
	a.LeaLabel(rA, resume)
	a.Store(rTop, 8, rA)
	a.OpImm(aluAdd, rTop, 16, true)
}

// undo is the code that puts back the word at k*8 from the captures, or
// from the address at baseOff in the context -- a counter's, a mark's --
// which a write pushed under it, then goes on backtracking.
func (g *nativeGen) undo(labels map[int]label, k int, baseOff int32) label {
	if l, ok := labels[k]; ok {
		return l
	}
	a := g.m
	l := a.NewLabel()
	labels[k] = l
	g.later(func() {
		a.Bind(l)
		a.OpImm(aluSub, rTop, 8, true)
		a.Load(rA, rTop, 0)
		base := rCaps
		if baseOff >= 0 {
			base = rC
			a.Load(rC, rCtx, baseOff)
		}
		a.Store(base, int32(k*8), rA)
		a.Jmp(g.bt)
	})
	return l
}

// saveOld pushes the old word at k*8 from base, to be put back by undo's
// code; it writes rA.
func (g *nativeGen) saveOld(base vreg, k int, undo label) {
	a := g.m
	a.Load(rA, base, int32(k*8))
	a.Store(rTop, 0, rA)
	a.LeaLabel(rA, undo)
	a.Store(rTop, 8, rA)
	a.OpImm(aluAdd, rTop, 16, true)
}

// curIndex is rA = the position, as an index of units.
func (g *nativeGen) curIndex() {
	g.m.MovRR(rA, rCur)
	g.m.Op(aluSub, rA, rSubj, true)
	if g.shift != 0 {
		g.m.ShiftImm(shiftRight, rA, g.shift, true)
	}
}

// instr makes an instruction's code.
func (g *nativeGen) instr(pc int) {
	a := g.m
	in := g.code[pc]
	g.prologue(pc)
	switch in.op {
	case opChar, opCharFold, opClass, opAny, opAnyNotNL:
		t := g.charTestOf(in, true)
		g.matchChar(&t, rCur, g.bt)

	case opString:
		g.runOf(g.prog.runs[in.arg])

	case opSplit:
		second := g.at[in.arg2]
		if q := in.quick; q != 0 {
			f := &g.prog.firsts[q-1]
			var set [128]bool
			for b := range set {
				set[b] = f.admits(uint16(b))
			}
			sp := spansOf128(&set)
			if f.nonASCII {
				sp = normSpans(append(sp, span{0x80, 0xFFFF}))
			}
			a.Op(aluCmp, rCur, rEnd, true)
			a.Jcc(condAE, second)
			g.load(rB, rCur, 0)
			g.testUnit(sp, rB, second)
		}
		alt := a.NewLabel()
		g.pushPos(alt)
		a.Jmp(g.at[in.arg])
		g.later(func() {
			a.Bind(alt)
			a.OpImm(aluSub, rTop, 8, true)
			a.Load(rCur, rTop, 0)
			a.Op(aluAdd, rCur, rSubj, true)
			a.Jmp(second)
		})

	case opJmp:
		a.Jmp(g.at[in.arg])

	case opSave:
		g.saveOld(rCaps, in.arg, g.undo(g.undoCaps, in.arg, -1))
		g.curIndex()
		a.Store(rCaps, int32(in.arg*8), rA)

	case opMatch:
		a.Jmp(g.matched)

	case opAssertStart, opAssertEnd:
		// A unit is enough: no surrogate ends a line, and a pair is no line
		// terminator either.
		ok := a.NewLabel()
		edge, disp := rSubj, -g.us
		if in.op == opAssertEnd {
			edge, disp = rEnd, 0
		}
		a.Op(aluCmp, rCur, edge, true)
		a.Jcc(condE, ok)
		if in.arg != 0 {
			g.load(rB, rCur, disp)
			g.testUnit(lineTerminators, rB, g.bt)
		} else {
			a.Jmp(g.bt)
		}
		a.Bind(ok)

	case opWordBoundary, opNotWordBoundary:
		// A unit is enough: no surrogate is a word character, and no code
		// point past the BMP is.
		word := wordSpans(in.arg != 0)
		prev, next := a.NewLabel(), a.NewLabel()
		a.Op(aluXor, rA, rA, false)
		a.Op(aluCmp, rCur, rSubj, true)
		a.Jcc(condE, prev)
		g.load(rB, rCur, -g.us)
		g.testUnit(word, rB, prev)
		a.MovImm(rA, 1)
		a.Bind(prev)
		a.Op(aluXor, rC, rC, false)
		a.Op(aluCmp, rCur, rEnd, true)
		a.Jcc(condAE, next)
		g.load(rB, rCur, 0)
		g.testUnit(word, rB, next)
		a.MovImm(rC, 1)
		a.Bind(next)
		a.Op(aluCmp, rA, rC, false)
		if in.op == opWordBoundary {
			a.Jcc(condE, g.bt)
		} else {
			a.Jcc(condNE, g.bt)
		}

	case opBackref, opBackrefFold:
		g.backref(pc, in)

	case opCounterInit:
		a.Load(rC, rCtx, offCounters)
		a.StoreImm(rC, int32(in.arg*8), 0)

	case opCounterInc:
		// The counter, and the bounds: min in arg2, max in r.
		k, lo, hi := in.arg, in.arg2, int(in.r)
		a.Load(rC, rCtx, offCounters)
		a.Load(rA, rC, int32(k*8))
		if hi >= 0 {
			a.OpImm(aluCmp, rA, int32(hi), true)
			a.Jcc(condGE, g.at[(*matcher)(nil).exitOfCounterLoop(g.code, pc)])
		}
		a.Store(rTop, 0, rA)
		a.LeaLabel(rB, g.undo(g.undoCtrs, k, offCounters))
		a.Store(rTop, 8, rB)
		a.OpImm(aluAdd, rTop, 16, true)
		a.Lea(rB, rA, 1)
		a.Store(rC, int32(k*8), rB)
		if lo > 0 {
			// Below the minimum the body is mandatory: the split's choice
			// point is passed over.
			a.OpImm(aluCmp, rA, int32(lo), true)
			a.Jcc(condL, g.at[pc+2])
		}

	case opClearCaps:
		for grp := in.arg; grp <= in.arg2; grp++ {
			skip := a.NewLabel()
			a.Load(rA, rCaps, int32(16*grp))
			a.Load(rB, rCaps, int32(16*grp+8))
			a.Op(aluAnd, rA, rB, true)
			a.Jcc(condS, skip)
			for _, slot := range []int{2 * grp, 2*grp + 1} {
				g.saveOld(rCaps, slot, g.undo(g.undoCaps, slot, -1))
				a.StoreImm(rCaps, int32(slot*8), -1)
			}
			a.Bind(skip)
		}

	case opEmptyCheck:
		k := in.arg
		a.Load(rC, rCtx, offMarks)
		if in.arg2 == 0 {
			// Entering an iteration: where it starts.
			g.saveOld(rC, k, g.undo(g.undoMk, k, offMarks))
			g.curIndex()
			a.Store(rC, int32(k*8), rA)
			break
		}
		// Leaving one that consumed nothing.
		g.curIndex()
		a.CmpMem(rA, rC, int32(k*8), true)
		a.Jcc(condE, g.bt)

	case opGreedy, opGreedyRune:
		g.greedy(pc, in)

	case opLazy:
		g.lazy(pc, in)
	}
}

// runOf is opString's code: the run's units compared with the subject's,
// as many at once as a word holds.
func (g *nativeGen) runOf(lit *literal) {
	a := g.m
	var b []byte
	if g.wide {
		for _, u := range lit.units {
			b = binary.LittleEndian.AppendUint16(b, u)
		}
	} else {
		if !lit.isASCII {
			// No ASCII subject has a unit past ASCII.
			a.Jmp(g.bt)
			return
		}
		b = []byte(lit.ascii)
	}
	a.MovRR(rA, rEnd)
	a.Op(aluSub, rA, rCur, true)
	a.OpImm(aluCmp, rA, int32(len(b)), true)
	a.Jcc(condL, g.bt)
	for off := 0; off < len(b); {
		s := b[off:]
		switch {
		case len(s) >= 8:
			a.Load(rA, rCur, int32(off))
			a.MovImm(rB, binary.LittleEndian.Uint64(s))
			a.Op(aluCmp, rA, rB, true)
			off += 8
		case len(s) >= 4:
			a.LoadU32(rA, rCur, int32(off))
			a.MovImm(rB, uint64(binary.LittleEndian.Uint32(s)))
			a.Op(aluCmp, rA, rB, false)
			off += 4
		case len(s) >= 2:
			a.LoadU16(rA, rCur, int32(off))
			a.OpImm(aluCmp, rA, int32(binary.LittleEndian.Uint16(s)), false)
			off += 2
		default:
			a.LoadU8(rA, rCur, int32(off))
			a.OpImm(aluCmp, rA, int32(s[0]), false)
			off++
		}
		a.Jcc(condNE, g.bt)
	}
	a.OpImm(aluAdd, rCur, int32(len(b)), true)
	// The run's units are the steps its characters would have been.
	if n := len(lit.units); n != 1 {
		a.OpImm(aluSub, rLeft, int32(n-1), true)
	}
}

// backref is a backreference's code: the group's text compared with what is
// at the position, unit by unit, over bytes under the i flag by its
// letters' case too.
func (g *nativeGen) backref(pc int, in instr) {
	a := g.m
	next := g.at[pc+1]
	// The group: the first of the name's that took part, or arg.
	sel := map[int]label{}
	label := func(grp int) label {
		if l, ok := sel[grp]; ok {
			return l
		}
		l := a.NewLabel()
		sel[grp] = l
		return l
	}
	var order []int
	if in.arg2 != 0 {
		for _, k := range g.prog.refSets[in.arg2-1] {
			a.Load(rA, rCaps, int32(16*k))
			a.Op(aluTest, rA, rA, true)
			a.Jcc(condNS, label(k))
			order = append(order, k)
		}
	}
	order = append(order, in.arg)
	a.Jmp(label(in.arg))
	common := a.NewLabel()
	done := map[int]bool{}
	for _, grp := range order {
		if done[grp] {
			continue
		}
		done[grp] = true
		a.Bind(label(grp))
		if 2*grp+1 >= g.ncaps {
			// No such group: it matches nothing.
			a.Jmp(next)
			continue
		}
		a.Load(rA, rCaps, int32(16*grp))
		a.Load(rB, rCaps, int32(16*grp+8))
		a.Jmp(common)
	}
	a.Bind(common)
	// A group that did not take part matches nothing.
	a.Op(aluTest, rA, rA, true)
	a.Jcc(condS, next)
	a.Op(aluTest, rB, rB, true)
	a.Jcc(condS, next)
	a.Op(aluSub, rB, rA, true)
	if g.shift != 0 {
		a.ShiftImm(shiftLeft, rA, g.shift, true)
		a.ShiftImm(shiftLeft, rB, g.shift, true)
	}
	a.MovRR(rC, rEnd)
	a.Op(aluSub, rC, rCur, true)
	a.Op(aluCmp, rB, rC, true)
	a.Jcc(condG, g.bt)
	// The comparison reads n characters, which are n steps.
	a.MovRR(rT, rB)
	if g.shift != 0 {
		a.ShiftImm(shiftRight, rT, g.shift, true)
	}
	a.Op(aluSub, rLeft, rT, true)
	a.MovRR(rT, rSubj)
	a.Op(aluAdd, rT, rA, true)
	a.MovRR(rD, rCur)
	loop, eq, end := a.NewLabel(), a.NewLabel(), a.NewLabel()
	a.Bind(loop)
	a.Op(aluTest, rB, rB, true)
	a.Jcc(condE, end)
	g.load(rA, rT, 0)
	g.load(rC, rD, 0)
	a.Op(aluCmp, rA, rC, false)
	a.Jcc(condE, eq)
	if in.op != opBackrefFold {
		a.Jmp(g.bt)
	} else {
		// Two bytes of ASCII are the same character up to case where they
		// are one letter, in either case: canonical is an ASCII letter's
		// upper case, and anything else of ASCII itself.
		a.OpImm(aluOr, rA, 0x20, false)
		a.OpImm(aluOr, rC, 0x20, false)
		a.Op(aluCmp, rA, rC, false)
		a.Jcc(condNE, g.bt)
		a.OpImm(aluSub, rA, 'a', false)
		a.OpImm(aluCmp, rA, 'z'-'a', false)
		a.Jcc(condA, g.bt)
	}
	a.Bind(eq)
	a.OpImm(aluAdd, rT, g.us, true)
	a.OpImm(aluAdd, rD, g.us, true)
	a.OpImm(aluSub, rB, g.us, true)
	a.Jmp(loop)
	a.Bind(end)
	a.MovRR(rCur, rD)
}

// greedy is a greedy run's code: the longest run of the item, given back a
// character at a time from one choice point, which holds the least it may
// end at and where it ends now.
func (g *nativeGen) greedy(pc int, in instr) {
	a := g.m
	rune := in.op == opGreedyRune
	t := g.charTestOf(g.code[pc+1], rune)
	after := g.at[pc+2]
	if t.all && !t.decode {
		a.MovRR(rC, rEnd)
	} else {
		loop, done := a.NewLabel(), a.NewLabel()
		a.MovRR(rC, rCur)
		a.Bind(loop)
		g.matchChar(&t, rC, done)
		a.Jmp(loop)
		a.Bind(done)
	}
	// Each unit of the run is a step.
	a.MovRR(rA, rC)
	a.Op(aluSub, rA, rCur, true)
	if g.shift != 0 {
		a.ShiftImm(shiftRight, rA, g.shift, true)
	}
	a.Op(aluSub, rLeft, rA, true)
	// rB is the least the run may end at: arg units, or under the unicode
	// flag at least one character where arg is not 0.
	switch {
	case rune && in.arg > 0 && t.decode:
		a.Op(aluCmp, rC, rCur, true)
		a.Jcc(condE, g.bt)
		g.readChar(rCur)
		a.MovRR(rB, rCur)
		a.Op(aluAdd, rB, rW, true)
	case in.arg > 0:
		least := in.arg
		if rune {
			least = 1
		}
		a.OpImm(aluCmp, rA, int32(least), true)
		a.Jcc(condL, g.bt)
		a.Lea(rB, rCur, int32(least)*g.us)
	default:
		a.MovRR(rB, rCur)
	}
	noPush, back := a.NewLabel(), a.NewLabel()
	a.Op(aluCmp, rC, rB, true)
	a.Jcc(condBE, noPush)
	a.Op(aluSub, rB, rSubj, true)
	a.Store(rTop, 0, rB)
	a.MovRR(rA, rC)
	a.Op(aluSub, rA, rSubj, true)
	a.Store(rTop, 8, rA)
	a.LeaLabel(rA, back)
	a.Store(rTop, 16, rA)
	a.OpImm(aluAdd, rTop, 24, true)
	a.Bind(noPush)
	a.MovRR(rCur, rC)
	a.Jmp(after)
	g.later(func() {
		// A character is given back -- under the unicode flag a pair where
		// the run ends with one -- and the choice point stays while the run
		// is longer than its least.
		pop := a.NewLabel()
		a.Bind(back)
		a.Load(rA, rTop, -8)
		if t.decode {
			one := a.NewLabel()
			a.MovImm(rW, 2)
			a.OpImm(aluCmp, rA, 4, true)
			a.Jcc(condL, one)
			a.MovRR(rT, rSubj)
			a.Op(aluAdd, rT, rA, true)
			a.LoadU16(rD, rT, -2)
			a.OpImm(aluSub, rD, 0xDC00, false)
			a.OpImm(aluCmp, rD, 0x3FF, false)
			a.Jcc(condA, one)
			a.LoadU16(rD, rT, -4)
			a.OpImm(aluSub, rD, 0xD800, false)
			a.OpImm(aluCmp, rD, 0x3FF, false)
			a.Jcc(condA, one)
			a.MovImm(rW, 4)
			a.Bind(one)
			a.Op(aluSub, rA, rW, true)
		} else {
			a.OpImm(aluSub, rA, g.us, true)
		}
		a.CmpMem(rA, rTop, -16, true)
		a.Jcc(condLE, pop)
		a.Store(rTop, -8, rA)
		a.OpImm(aluAdd, rTop, 8, true)
		a.MovRR(rCur, rSubj)
		a.Op(aluAdd, rCur, rA, true)
		a.Jmp(after)
		a.Bind(pop)
		a.Load(rA, rTop, -16)
		a.OpImm(aluSub, rTop, 16, true)
		a.MovRR(rCur, rSubj)
		a.Op(aluAdd, rCur, rA, true)
		a.Jmp(after)
	})
}

// lazy is a lazy run's code: what it must take, then the rest of the
// pattern, one more character taken from its choice point each time that
// fails.
func (g *nativeGen) lazy(pc int, in instr) {
	a := g.m
	t := g.charTestOf(g.code[pc+1], in.arg2 != 0)
	after := g.at[pc+2]
	if in.arg > 0 {
		// The character it must take is a step of its own.
		a.OpImm(aluSub, rLeft, 1, true)
		g.matchChar(&t, rCur, g.bt)
	}
	more := a.NewLabel()
	g.pushPos(more)
	a.Jmp(after)
	g.later(func() {
		pop := a.NewLabel()
		a.Bind(more)
		a.OpImm(aluSub, rLeft, 1, true)
		a.Load(rC, rTop, -8)
		a.Op(aluAdd, rC, rSubj, true)
		g.matchChar(&t, rC, pop)
		a.MovRR(rA, rC)
		a.Op(aluSub, rA, rSubj, true)
		a.Store(rTop, -8, rA)
		a.OpImm(aluAdd, rTop, 8, true)
		a.MovRR(rCur, rC)
		a.Jmp(after)
		a.Bind(pop)
		a.OpImm(aluSub, rTop, 8, true)
		a.Jmp(g.bt)
	})
}

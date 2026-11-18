package engine

import (
	"fmt"
	"os"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

// The verifier proves of a function's register code what its handlers,
// the Go table's and the native ones, take on trust (README.md, "The
// verifier"): every slot an instruction names is in the frame -- a v128's
// two, a reference's among the reference slots -- every branch, table
// entry, loop head and catch landing is an instruction's start, the words
// an instruction takes after it are there, every index into the code's
// sites, tables, records and regions and into the module's globals,
// functions, memories, tables, segments and tags is in range, every
// operation and escaped operation has a handler, and the last instruction
// does not fall off the end. Code that fails is a compiler bug.

// regDesc describes an operation's fields for the verifier (rd).
type regDesc struct {
	ok      bool
	a, b, c byte
	// bc, where not 0, says B and C are one 32-bit field of that meaning.
	bc byte
	// words are the kinds of the words after the instruction.
	words string
	// end says control never goes on to the next instruction.
	end bool
}

// rd parses a descriptor: the kinds of A, B and C -- 'n' a numeric slot,
// 'v' a v128's first slot, 'r' a reference slot, 'o' (C) a 16-bit branch
// offset, 'c' (C) a struct field's cell, 'q' (C) a GC record, '-'
// anything -- or, where B is '=', A's kind and BC's meaning:
// 'o' a 32-bit branch offset, 'g', 'G' and 'W' a numeric, reference and
// v128 global, 's' a site, 't' a branch table, 'f' a function, 'm' a
// memory, 'T' a table, 'd' and 'e' a data and an element segment, 'R' a GC
// record, 'r' a reference slot, '-' anything. A '+' adds the kinds of the
// words after the instruction: '-' data, 'n' and 'v' a slot, 'm' a memory
// in the low half, 'M' two memories, 'T' a table in the low half, 'U' two
// tables, 'D' a data segment and a memory, 'E' an element segment and a
// table, 'R' a GC record, 'A' two numeric slots of 32 bits each. A '!'
// last says the operation ends its block.
func rd(s string) regDesc {
	d := regDesc{ok: true}
	if n := len(s); n > 0 && s[n-1] == '!' {
		d.end, s = true, s[:n-1]
	}
	d.a, d.b, d.c = s[0], s[1], s[2]
	if d.b == '=' {
		d.bc, d.b, d.c = d.c, '-', '-'
	}
	if len(s) > 3 && s[3] == '+' {
		d.words = s[4:]
	}
	return d
}

var (
	regDescs     [256]regDesc
	regExtDescs  [256]regDesc
	regSIMDDescs [256]regDesc
)

func init() {
	for op, s := range map[regOp]string{
		rMov: "nn-", rConst32: "n=-", rConst32S: "n=-", rConst64: "n--+-",
		rJmp: "-=o!", rBrNZ: "n=o", rBrZ: "n=o", rBrTable: "n=t!", rRet: "---!", rRet1: "n--!",
		rCall: "-=s", rReturnCall: "-=s!", rSelect: "nnn",
		rGlobalGet32: "n=g", rGlobalGet64: "n=g", rGlobalSet: "n=g",
		rMov2: "vv-", rRefMov: "rr-", rRefNull: "r--", rConst128: "v--+--",
	} {
		regDescs[op] = rd(s)
	}
	for x, s := range map[regOp]string{
		xUnreachable: "---!", xMemorySize: "n--", xMemoryGrow: "nn-",
		xGlobalGetRef: "r=G", xGlobalSetRef: "r=G", xGlobalGet128: "v=W", xGlobalSet128: "v=W",
		xSelectRef: "rrn", xSelect128: "vvn", xRefIsNull: "nr-", xRefFunc: "r=f", xRefEq: "nrr",
		xRefAsNonNull: "r--", xBrNull: "r=o", xBrNonNull: "r=o", xCaughtTo: "-=-",
		xLoadAny: "nn-+m-", xStoreAny: "nn-+m-", xMemorySizeAny: "n=m", xMemoryGrowAny: "nn-+m",
		xMemoryInit: "nnn+D", xMemoryCopy: "nnn+M", xMemoryFill: "nnn+m", xDataDrop: "-=d",
		xV128Load: "vn-+m-", xV128Store: "nv-+m-", xV128LoadLane: "vnv+m-", xV128StoreLane: "nv-+m-",
		xTableGet: "rn-+T", xTableSet: "nr-+T", xTableSize: "n=T", xTableGrow: "nnr+T",
		xTableFill: "nrn+T", xTableCopy: "nnn+U", xTableInit: "nnn+E", xElemDrop: "-=e",
		xCallIndirect: "-=s", xCallRef: "-=s", xReturnCallIndirect: "-=s!", xReturnCallRef: "-=s!",
		xStructNew: "-=R", xStructNewDefault: "-=R", xStructGet: "-=R", xStructSet: "-=R",
		xArrayNew: "-=R", xArrayNewDefault: "-=R", xArrayNewFixed: "-=R", xArrayNewSegment: "-=R",
		xArrayGet: "-=R", xArraySet: "-=R", xArrayLen: "-=R", xArrayFill: "-=R", xArrayCopy: "-=R",
		xArrayInit: "-=R", xRefI31: "-=R", xI31Get: "-=R", xRefTest: "-=R", xRefCast: "-=R",
		xBrOnCast: "r=o+R", xBrOnCastFail: "r=o+R",
		xAtomic: "---+m-AA", xThrow: "-=s!", xThrowRef: "-=r!", xRethrow: "-=-!",
	} {
		regExtDescs[x] = rd(s)
	}
	for s, d := range map[regOp]string{
		sShuffle: "vvv+--", sExtractI8S: "nv-", sExtractI8U: "nv-", sExtractI16S: "nv-", sExtractI16U: "nv-",
		sExtract32: "nv-", sExtract64: "nv-", sReplace8: "vvn+-", sReplace16: "vvn+-", sReplace32: "vvn+-",
		sReplace64: "vvn+-", sSplat8: "vn-", sSplat16: "vn-", sSplat32: "vn-", sSplat64: "vn-",
	} {
		regSIMDDescs[s] = rd(d)
	}
}

// regVerify says every body is verified as it is compiled: in the
// package's tests, and where WASM_REG_VERIFY=1 asks.
var regVerify = os.Getenv("WASM_REG_VERIFY") == "1"

// verify checks rc, the code of function fn of module m with a frame of
// nslots numeric and rslots reference slots.
func (rc *regCode) verify(m *module.Module, nslots, rslots int) error {
	v := regVerifier{rc: rc, m: m, nslots: nslots, rslots: rslots}
	return v.run()
}

type regVerifier struct {
	rc             *regCode
	m              *module.Module
	nslots, rslots int
	// starts says which pcs are an instruction's start; targets are the
	// pcs control may go to, checked once every start is known.
	starts  []bool
	targets []int
	err     error
	// cells is the most cells a struct of the module's types takes, once
	// asked for.
	cells int
}

// maxCells returns the most cells a struct of the module's types takes,
// its header's included: what a struct field's cell is below.
func (v *regVerifier) maxCells() int {
	if v.cells == 0 {
		v.cells = 1
		for ti, t := range v.m.Types {
			if t.Kind == module.TypeStruct {
				v.cells = max(v.cells, rttOf(v.m.Canonical[ti]).ncells)
			}
		}
	}
	return v.cells
}

func (v *regVerifier) fail(pc int, format string, args ...any) {
	if v.err == nil {
		v.err = fmt.Errorf("register code at pc %d: %s", pc, fmt.Sprintf(format, args...))
	}
}

func (v *regVerifier) run() error {
	code := v.rc.code
	if len(code) == 0 {
		return fmt.Errorf("register code: none")
	}
	if len(v.rc.offs) != len(code) {
		return fmt.Errorf("register code: %d offsets for %d words", len(v.rc.offs), len(code))
	}
	v.starts = make([]bool, len(code)+1)
	end := false
	for pc := 0; pc < len(code) && v.err == nil; {
		v.starts[pc] = true
		n, e := v.instr(pc, v.nslots, v.rslots)
		pc, end = pc+n, e
	}
	if v.err != nil {
		return v.err
	}
	if !end {
		return fmt.Errorf("register code: the last instruction goes on past the end")
	}
	for pc := range v.rc.loops {
		v.targets = append(v.targets, int(pc))
	}
	for i := range v.rc.regions {
		for _, cl := range v.rc.regions[i].clauses {
			v.targets = append(v.targets, int(cl.pc))
			for _, mv := range cl.vals {
				v.move(-1, mv.k, mv.to)
			}
			if cl.ref {
				v.slot(-1, 'r', int(cl.exn))
			}
		}
	}
	for _, g := range v.rc.gc {
		for _, s := range g.ops {
			v.anySlot(-1, int(s))
		}
		for _, mv := range g.vals {
			v.move(-1, mv.k, mv.from)
		}
		v.anySlot(-1, int(g.dst))
	}
	for _, t := range v.targets {
		if t < 0 || t >= len(code) || !v.starts[t] {
			v.fail(t, "a branch to %d, not an instruction's start", t)
		}
	}
	return v.err
}

// slot checks a slot of kind k ('n', 'v', 'r') against the frame's.
func (v *regVerifier) slot(pc int, k byte, s int) {
	v.slotIn(pc, k, s, v.nslots, v.rslots)
}

func (v *regVerifier) slotIn(pc int, k byte, s, nslots, rslots int) {
	switch k {
	case 'n':
		if s >= nslots {
			v.fail(pc, "numeric slot %d past the frame's %d", s, nslots)
		}
	case 'v':
		if s+1 >= nslots {
			v.fail(pc, "v128 slot %d past the frame's %d", s, nslots)
		}
	case 'r':
		if s >= rslots {
			v.fail(pc, "reference slot %d past the frame's %d", s, rslots)
		}
	}
}

// anySlot checks a slot a record names, of a kind its use says.
func (v *regVerifier) anySlot(pc, s int) {
	if s >= max(v.nslots, v.rslots) {
		v.fail(pc, "slot %d past the frame's", s)
	}
}

// move checks the frame's end of a move of kind k.
func (v *regVerifier) move(pc int, k uint8, s uint32) {
	v.slot(pc, "nvr"[k], int(s))
}

// desc returns the descriptor of the instruction word w.
func (v *regVerifier) desc(pc int, w uint64) regDesc {
	op := uint8(w)
	x := uint8(w >> 8)
	var d regDesc
	switch op {
	case rExt:
		d = regExtDescs[x]
	case rSIMD:
		d = regSIMDDescs[x]
	default:
		d = regDescs[op]
	}
	if !d.ok {
		v.fail(pc, "operation %d (%d) has no handler", op, x)
	}
	return d
}

// instr checks the instruction at pc against frames of nslots and rslots,
// returning its length in words and whether it ends its block.
func (v *regVerifier) instr(pc, nslots, rslots int) (int, bool) {
	code := v.rc.code
	w := code[pc]
	if uint8(w) == rFar {
		return v.far(pc)
	}
	d := v.desc(pc, w)
	if v.err != nil {
		return 1, false
	}
	n := 1 + len(d.words)
	if pc+n > len(code) {
		v.fail(pc, "%d words past the code's end", pc+n-len(code))
		return 1, false
	}
	for i, k := range [3]byte{d.a, d.b, d.c} {
		f := int(uint16(w >> (16 + 16*i)))
		switch k {
		case 'o':
			v.targets = append(v.targets, pc+int(int16(f)))
		case 'c':
			if f >= v.maxCells() {
				v.fail(pc, "struct cell %d past the module's structs' %d", f, v.maxCells())
			}
		case 'q':
			if f >= len(v.rc.gc) {
				v.fail(pc, "GC record %d past the code's %d", f, len(v.rc.gc))
			}
		default:
			v.slotIn(pc, k, f, nslots, rslots)
		}
	}
	if d.bc != 0 {
		v.bc(pc, d.bc, uint32(w>>32))
	}
	for i, k := range d.words {
		v.word(pc, byte(k), code[pc+1+i], nslots, rslots)
	}
	return n, d.end
}

// bc checks a 32-bit BC of meaning k.
func (v *regVerifier) bc(pc int, k byte, x uint32) {
	m, rc := v.m, v.rc
	in := func(n int, what string) {
		if uint64(x) >= uint64(n) {
			v.fail(pc, "%s %d past the %d there are", what, x, n)
		}
	}
	switch k {
	case 'o':
		v.targets = append(v.targets, pc+int(int32(x)))
	case 'g', 'G', 'W':
		in(len(m.Globals), "global")
		if v.err == nil {
			t := m.Globals[x].Type
			if t.IsRef() != (k == 'G') || (t.Kind == module.KindV128) != (k == 'W') {
				v.fail(pc, "global %d of the wrong kind", x)
			}
		}
	case 's':
		in(len(rc.sites), "site")
		if v.err == nil {
			v.site(pc, &rc.sites[x])
		}
	case 't':
		in(len(rc.tables), "table")
		if v.err == nil {
			if len(rc.tables[x]) == 0 {
				v.fail(pc, "an empty branch table")
			}
			for _, t := range rc.tables[x] {
				v.targets = append(v.targets, int(t))
			}
		}
	case 'f':
		in(len(m.Functions), "function")
	case 'm':
		in(len(m.Memories), "memory")
	case 'T':
		in(len(m.Tables), "table")
	case 'd':
		in(len(m.Datas), "data segment")
	case 'e':
		in(len(m.Elems), "element segment")
	case 'R':
		in(len(rc.gc), "GC record")
	case 'r':
		v.slot(pc, 'r', int(x))
	}
}

// word checks a word after the instruction of kind k.
func (v *regVerifier) word(pc int, k byte, w uint64, nslots, rslots int) {
	lo, hi := uint32(w), uint32(w>>32)
	m := v.m
	in := func(x uint32, n int, what string) {
		if uint64(x) >= uint64(n) {
			v.fail(pc, "%s %d past the %d there are", what, x, n)
		}
	}
	switch k {
	case 'n', 'v':
		v.slotIn(pc, k, int(w), nslots, rslots)
	case 'm':
		in(lo, len(m.Memories), "memory")
	case 'M':
		in(lo, len(m.Memories), "memory")
		in(hi&0xffffff, len(m.Memories), "memory")
	case 'T':
		in(lo, len(m.Tables), "table")
	case 'U':
		in(lo, len(m.Tables), "table")
		in(hi, len(m.Tables), "table")
	case 'D':
		in(lo, len(m.Datas), "data segment")
		in(hi, len(m.Memories), "memory")
	case 'E':
		in(lo, len(m.Elems), "element segment")
		in(hi, len(m.Tables), "table")
	case 'R':
		in(lo, len(v.rc.gc), "GC record")
	case 'A':
		v.slot(pc, 'n', int(lo))
		v.slot(pc, 'n', int(hi))
	}
}

// site checks a call's or a throw's site.
func (v *regVerifier) site(pc int, s *regSite) {
	for _, mv := range s.args {
		v.move(pc, mv.k, mv.from)
	}
	for _, mv := range s.rets {
		v.move(pc, mv.k, mv.to)
	}
	op := uint8(v.rc.code[pc])
	x := uint8(v.rc.code[pc] >> 8)
	switch {
	case op == rCall || op == rReturnCall:
		if uint64(s.fn) >= uint64(len(v.m.Functions)) {
			v.fail(pc, "function %d past the module's", s.fn)
		}
	case op == rExt && x == xThrow:
		if uint64(s.fn) >= uint64(len(v.m.Tags)) {
			v.fail(pc, "tag %d past the module's", s.fn)
		}
	case op == rExt && (x == xCallIndirect || x == xReturnCallIndirect):
		v.slot(pc, 'n', int(s.target))
		if uint64(s.table) >= uint64(len(v.m.Tables)) || uint64(s.typeIndex) >= uint64(len(v.m.Types)) {
			v.fail(pc, "call_indirect of table %d, type %d", s.table, s.typeIndex)
		}
	case op == rExt && (x == xCallRef || x == xReturnCallRef):
		v.slot(pc, 'r', int(s.target))
	}
}

// far checks an instruction behind a far prefix: the prefix's slots
// against the frame, and the instruction against the scratch slots its
// fields name.
func (v *regVerifier) far(pc int) (int, bool) {
	code := v.rc.code
	if pc+3 >= len(code) {
		v.fail(pc, "a far prefix past the code's end")
		return 1, false
	}
	w := code[pc]
	kinds := uint8(w >> 16)
	slots := [4]int{int(uint32(code[pc+1])), int(uint32(code[pc+1] >> 32)), int(uint32(code[pc+2])), int(uint32(w >> 32))}
	for i, s := range slots {
		switch kinds >> (2 * i) & 3 {
		case 1:
			v.slot(pc, 'n', s)
		case 2:
			v.slot(pc, 'v', s)
		case 3:
			v.slot(pc, 'r', s)
		}
	}
	if uint8(code[pc+3]) == rFar {
		v.fail(pc, "a far prefix of a far prefix")
		return 3, false
	}
	v.starts[pc+3] = false
	n, end := v.instr(pc+3, len(rhFarScratch{}.n), len(rhFarScratch{}.r))
	return 3 + n, end
}

// rhFarScratch is the size of a frame's scratch slots (frame.farN, farR).
type rhFarScratch struct {
	n [8]uint64
	r [4]Ref
}

// verifyCompiled verifies function index's register code, panicking on a
// compiler bug.
func verifyCompiled(m *module.Module, index uint32, p *compiled) {
	if err := p.reg.verify(m, p.nslots, p.rslots); err != nil {
		panic(fmt.Sprintf("wasm: function %d: %v", index, err))
	}
}

var _ = opcode.Unreachable

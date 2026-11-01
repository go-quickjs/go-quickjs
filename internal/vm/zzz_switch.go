package vm

import (
	"math"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A switch's cases whose selectors are literals -- strings, numbers,
// booleans, null and undefined, a template without substitutions among
// the strings -- as one lookup of the discriminant in the tree tier. Each
// case is compiled to dup, the selector's push, strict_eq and jump_if_true
// to its body, and each jump ended a block: a match at the nth case had
// been n blocks, each a statement copying the discriminant, an equality's
// node and the node making its boolean, then the jump's test of it, and a
// trip round runTree's loop -- a lexer's keyword table of forty cases is
// forty of each where its last keyword matches, and every miss as many.
//
// A run of such cases, from a dup the block being built has got to, is
// instead looked up as one: the block's end evaluates the discriminant
// into its slot, as the dup would have left it, and looks it up in a
// table made of the selectors, which goes to the first case whose
// selector it strictly equals, in the cases' order, or else to the
// instruction after the run -- the switch's jump to its default clause or
// past it, or the next case's test, one whose selector is not a literal,
// where the chain goes on as it was compiled. Evaluating a literal has no
// effect, so skipping those not compared is not seen; what strict
// equality says is kept: +0 equals -0, NaN nothing, a string a string of
// its code units, however it was made, a number no string. The run's
// later cases are built as they were compiled, the way on of the first
// case's jump, and never run.
//
// Strings are looked up by length, then by the constant itself -- a
// string a constant was stored from is it -- and then by their bytes,
// which a flat string's WTF-8 spells one way: a rope is flattened only if
// a case has its length. Integers in a range of no more than
// switchDense are an array's index, other numbers a map's key; a
// switch (typeof x) of type names a table of the eight types, x's kind
// asked (see zzz_typeof.go) and its name stored where the discriminant
// goes.
//
// opHook gives the run's dup to switchStart, which leaves the lookup for
// the first case's jump_if_true; truthJump, which buildBlock asks for that
// jump's end, is truthJumpHook, which makes it the lookup. Its functions
// are generic, instantiated with struct{} alone, as zzz_typeof.go's are,
// so that every function and closure there was stays at its address.

// switchCase is a case of a run switchStart looks up: its selector, a
// literal, and the instruction its body begins at.
type switchCase struct {
	lit Value
	// str is the selector of a string's case, k its constant, or -1 for
	// the empty string push_empty_string pushes.
	str string
	k   int64
	to  int
}

// switchStr is a string case of a lookup: the selector's WTF-8, its
// constant, -1 for none, and the block its body begins.
type switchStr struct {
	str string
	k   int64
	to  int
}

// switchDense is the most integers' range an array of their blocks is
// made for.
const switchDense = 256

// switchRun is the run of literal cases from the dup at pc of code, and
// the instruction after the run, its last case's jump_if_true's next; no
// case where there is none. A case's dup that a jump goes to -- nothing
// the compiler makes jumps into a switch's tests -- ends the run before
// it.
func switchRun[_ any](b *tbuilder, pc int) ([]switchCase, int) {
	var cases []switchCase
	p := pc
	for {
		c, ok := switchCaseAt[struct{}](b, p)
		if !ok {
			break
		}
		cases = append(cases, c)
		p += 4
	}
	if len(cases) == 0 {
		return nil, pc
	}
	// Where a jump goes into the run, it ends before the case there.
	end := p
	for q, in := range b.fn.Code {
		to := -1
		switch in.Op {
		case bytecode.OpJump, bytecode.OpJumpIfFalse, bytecode.OpJumpIfTrue, bytecode.OpJumpIfCmpFalse,
			bytecode.OpJumpIfFalseKeep, bytecode.OpJumpIfTrueKeep, bytecode.OpJumpIfNullish, bytecode.OpJumpIfNotNullish,
			bytecode.OpIterNextOrJump, bytecode.OpIterResultOrJump, bytecode.OpIterUnpackDelegate,
			bytecode.OpPushCatch, bytecode.OpPushFinally, bytecode.OpAsyncIterClose, bytecode.OpDispose:
			to = int(in.A)
		case bytecode.OpIterResume:
			to = int(in.B)
		}
		if q >= pc && q < p && in.Op == bytecode.OpJumpIfTrue {
			// The run's own, to its bodies.
			continue
		}
		if to > pc && to < end {
			end = pc + (to-pc)/4*4
		}
	}
	n := (end - pc) / 4
	return cases[:n], pc + 4*n
}

// switchCaseAt is the case at p of the function's code, where it is a
// case whose selector is a literal: dup, the literal's push, strict_eq and
// a jump_if_true forward to its body, which is no && or || of its own.
func switchCaseAt[_ any](b *tbuilder, p int) (switchCase, bool) {
	code, consts := b.fn.Code, b.fn.Constants
	if p < 0 || p+3 >= len(code) || code[p].Op != bytecode.OpDup || code[p+2].Op != bytecode.OpStrictEq ||
		code[p+3].Op != bytecode.OpJumpIfTrue || int(code[p+3].A) <= p+3 || int(code[p+3].A) > len(code) {
		return switchCase{}, false
	}
	if _, ok := b.logical[p+3]; ok {
		return switchCase{}, false
	}
	c := switchCase{k: -1, to: int(code[p+3].A)}
	switch in := code[p+1]; in.Op {
	case bytecode.OpPushConst:
		if uint64(in.A) >= uint64(len(consts)) {
			return switchCase{}, false
		}
		switch k := consts[in.A]; k.Kind {
		case bytecode.ConstString:
			c.lit, c.str, c.k = Str(emptyString), k.Str, int64(in.A)
		case bytecode.ConstNumber:
			c.lit = Float(k.Num)
		default:
			return switchCase{}, false
		}
	case bytecode.OpPushEmptyString:
		c.lit = Str(emptyString)
	case bytecode.OpPushInt:
		c.lit = Int32(int32(in.A))
	case bytecode.OpPushTrue:
		c.lit = True
	case bytecode.OpPushFalse:
		c.lit = False
	case bytecode.OpPushNull:
		c.lit = Null
	case bytecode.OpPushUndef:
		c.lit = Undefined
	default:
		return switchCase{}, false
	}
	return c, true
}

// switchLookup is the end looking up the discriminant, which v evaluates,
// in cases, the blocks of their bodies at their to, or going to miss, the
// discriminant stored at slot d-1 first.
func switchLookup[_ any](cases []switchCase, v tval, d, miss int) tnext {
	// The first case of each selector, in the cases' order, is the one
	// strict equality finds.
	var strs []switchStr
	seen := map[string]bool{}
	nums := map[float64]int{}
	var numOrder []float64
	other := map[uint64]int{}
	for _, c := range cases {
		switch l := c.lit; {
		case l.IsString():
			if !seen[c.str] {
				seen[c.str] = true
				strs = append(strs, switchStr{str: c.str, k: c.k, to: c.to})
			}
		case l.IsNumber():
			if f := l.num; f == f {
				if _, ok := nums[f]; !ok {
					// +0 and -0 are one key, as they are one case.
					nums[f] = c.to
					numOrder = append(numOrder, f)
				}
			}
		default:
			b := math.Float64bits(l.num)
			if _, ok := other[b]; !ok {
				other[b] = c.to
			}
		}
	}
	// The strings by length, in code units, each length's in the
	// cases' order: byLen[n] to byLen[n+1] are those of length n.
	var byLen []int32
	var flat []switchStr
	if len(strs) > 0 {
		most := 0
		lens := make([]int, len(strs))
		for i, s := range strs {
			lens[i] = NewString(s.str).length
			most = max(most, lens[i])
		}
		byLen = make([]int32, most+2)
		for n := 0; n <= most; n++ {
			byLen[n] = int32(len(flat))
			for i, s := range strs {
				if lens[i] == n {
					flat = append(flat, s)
				}
			}
		}
		byLen[most+1] = int32(len(flat))
	}
	if len(nums) == 0 && len(other) == 0 {
		// Strings alone, what most switches on strings are.
		return func(c *tctx) int {
			x := v(c)
			c.stack[d-1] = x
			if !x.IsString() {
				return miss
			}
			s := (*String)(x.ref)
			if s.length >= len(byLen)-1 {
				return miss
			}
			for _, e := range flat[byLen[s.length]:byLen[s.length+1]] {
				if e.k >= 0 && c.consts[e.k].ref == unsafe.Pointer(s) {
					return e.to
				}
				if s.left != nil {
					s.Go()
				}
				if s.s == e.str {
					return e.to
				}
			}
			return miss
		}
	}
	// Integers in a short range, as a state machine's or an opcode's
	// switch has, an array's index.
	lo, hi, ints := math.Inf(1), math.Inf(-1), true
	for f := range nums {
		lo, hi = min(lo, f), max(hi, f)
		ints = ints && f == math.Trunc(f) && !math.IsInf(f, 0)
	}
	if len(strs) == 0 && len(other) == 0 && ints && hi-lo < switchDense {
		dense := make([]int, int(hi-lo)+1)
		for i := range dense {
			dense[i] = miss
		}
		for f, to := range nums {
			dense[int(f-lo)] = to
		}
		return func(c *tctx) int {
			x := v(c)
			c.stack[d-1] = x
			if f := x.num; x.IsNumber() && f >= lo && f <= hi {
				// f is an integer where truncating it gives it back;
				// -0's index is 0's.
				if i := int(f - lo); float64(i)+lo == f {
					return dense[i]
				}
			}
			return miss
		}
	}
	// Any mixture: by kind, a few numbers in a row, more by a map.
	var few []float64
	var fewTo []int
	if len(nums) <= 8 {
		for _, f := range numOrder {
			few, fewTo = append(few, f), append(fewTo, nums[f])
		}
		nums = nil
	}
	return func(c *tctx) int {
		x := v(c)
		c.stack[d-1] = x
		switch {
		case x.IsNumber():
			f := x.num
			if nums != nil {
				if to, ok := nums[f]; ok {
					return to
				}
				return miss
			}
			for i, g := range few {
				if f == g {
					return fewTo[i]
				}
			}
			return miss
		case x.IsString():
			s := (*String)(x.ref)
			if s.length >= len(byLen)-1 {
				return miss
			}
			for _, e := range flat[byLen[s.length]:byLen[s.length+1]] {
				if e.k >= 0 && c.consts[e.k].ref == unsafe.Pointer(s) {
					return e.to
				}
				if s.left != nil {
					s.Go()
				}
				if s.s == e.str {
					return e.to
				}
			}
			return miss
		case x.IsBool() || x.IsNullish():
			// One spelling each.
			if to, ok := other[math.Float64bits(x.num)]; ok {
				return to
			}
		}
		return miss
	}
}

// switchTypeof is the end of a switch on typeof of x, every case a
// string, as switchLookup's.
func switchTypeof[_ any](cases []switchCase, x tentry, d, miss int) tnext {
	var to [8]int
	for i := range to {
		to[i] = -1
	}
	for _, c := range cases {
		// A name of no type is a case nothing matches.
		if i := typeofIndex[struct{}](c.str); i >= 0 && to[i] < 0 {
			to[i] = c.to
		}
	}
	for i := range to {
		if to[i] < 0 {
			to[i] = miss
		}
	}
	v := x.rare.typeOf.x.tree()
	return func(c *tctx) int {
		i := typeofKind[struct{}](v(c))
		c.stack[d-1] = Str(typeofOfStr[struct{}](c.r, i))
		return to[i]
	}
}

// switchStart is opHook's for the dup at pc, where it begins a run of a
// switch's cases whose selectors are literals: it takes the discriminant,
// with the run's way out and each body's block as successors of the block
// being built, and leaves for the jump_if_true that ends it, the first
// case's, the discriminant's slot and an entry that names the lookup
// (tentryRare's sw), which truthJumpHook makes the block's end -- going to
// any of those blocks, not only the jump's two. It reports how many
// instructions after the dup it took, and false where there is no such run
// at pc, or pc is a later case of a run, for the dup to be op's: such a
// block, the first case's jump's way on, is never run.
func switchStart[_ any](b *tbuilder, pc int) (int, bool, bool) {
	if len(b.logic) != 0 || len(b.stack) == 0 || pc+3 >= b.end {
		return 0, false, false
	}
	if _, ok := switchCaseAt[struct{}](b, pc-4); ok {
		// A later case of a run, which the first case's lookup took.
		return 0, false, false
	}
	cases, after := switchRun[struct{}](b, pc)
	if len(cases) == 0 {
		return 0, false, false
	}
	x := b.pop()
	if !b.spill() {
		return 0, false, true
	}
	d := len(b.stack) + 1
	// Each body's block, and the run's way out, entered with the
	// discriminant on the stack, as the jumps leave it.
	blocks := map[int]int{}
	for i := -1; i < len(cases); i++ {
		at := after
		if i >= 0 {
			at = cases[i].to
		}
		if _, ok := blocks[at]; ok {
			continue
		}
		s, ok := b.target(at, d)
		if !ok {
			return 0, false, true
		}
		blocks[at] = s
	}
	for i := range cases {
		cases[i].to = blocks[cases[i].to]
	}
	miss := blocks[after]
	strs := true
	for _, c := range cases {
		strs = strs && c.lit.IsString()
	}
	var next tnext
	if strs && x.rare != nil && x.rare.typeOf != nil && x.v != nil && x.slot < 0 {
		// switch (typeof v): v's kind, its name stored for the bodies.
		next = switchTypeof[struct{}](cases, x, d, miss)
	} else {
		next = switchLookup[struct{}](cases, x.tree(), d, miss)
	}
	b.pushSlot(d - 1)
	b.pushEntry(tentry{v: func(*tctx) Value { return Undefined }, slot: -1, rare: &tentryRare{sw: next}})
	return 2, true, true
}

// truthJumpHook is truthJump, a forward jump's end on x: the lookup x names,
// where switchStart made it, or else truthJumpLocal's.
func truthJumpHook[_ any](x tentry, onTrue bool, taken, fall int) tnext {
	if x.rare != nil && x.rare.sw != nil {
		return x.rare.sw
	}
	return truthJumpLocal(x, onTrue, taken, fall)
}

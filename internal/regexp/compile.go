package regexp

// Compiling a pattern into a program for the backtracking matcher.
//
// The instruction set is the classic Thompson one plus what backtracking needs:
// opSplit creates a choice point, opSave records a capture boundary, and the
// counted quantifiers use an explicit counter rather than being unrolled, so
// that a{1,10000} costs ten instructions rather than ten thousand.

type opcode uint8

const (
	// opChar matches one specific code point.
	opChar opcode = iota
	// opCharFold matches a code point up to case folding.
	opCharFold
	// opClass matches against the set at index arg.
	opClass
	// opAny matches any code point; opAnyNotNL excludes line terminators.
	opAny
	opAnyNotNL
	// opSplit tries arg first and arg2 on failure.
	opSplit
	opJmp
	// opSave stores the current position in capture slot arg.
	opSave
	opMatch
	// opBackref matches the text captured by group arg.
	opBackref
	opBackrefFold
	// The zero-width assertions.
	opAssertStart
	opAssertEnd
	opWordBoundary
	opNotWordBoundary
	// opLook runs the sub-program at arg as a lookaround.
	opLook
	// opCounterInit, opCounterInc and opCounterCheck implement a counted
	// quantifier without unrolling it.
	opCounterInit
	opCounterInc
	// opEmptyCheck fails a repetition whose body matched nothing, which is what
	// stops (a*)* from looping forever.
	opEmptyCheck
)

// instr is one instruction of the program.
type instr struct {
	op opcode
	// arg and arg2 carry jump targets, capture slots, class indices or counter
	// bounds, depending on the opcode.
	arg, arg2 int
	r         rune
}

// program is a compiled pattern.
type program struct {
	code    []instr
	classes []*charSet
	// looks holds the sub-programs of lookarounds, referenced by index.
	looks []lookProgram
	// counters is the number of counted quantifiers, which sizes the matcher's
	// counter array.
	counters int
	// emptyChecks is the number of repetitions that need an empty-body guard.
	emptyChecks int
}

// lookProgram is a lookaround's body plus how it is applied.
type lookProgram struct {
	code   []instr
	behind bool
	negate bool
}

// compiler builds a program from a syntax tree.
type compiler struct {
	prog  *program
	flags Flags
}

func compileNode(n node, flags Flags, groupCount int) *program {
	c := &compiler{prog: &program{}, flags: flags}
	// Slot 0 and 1 hold the whole match's bounds, so group k uses slots 2k and
	// 2k+1.
	c.emit(instr{op: opSave, arg: 0})
	c.compile(n)
	c.emit(instr{op: opSave, arg: 1})
	c.emit(instr{op: opMatch})
	return c.prog
}

func (c *compiler) emit(in instr) int {
	c.prog.code = append(c.prog.code, in)
	return len(c.prog.code) - 1
}

func (c *compiler) here() int { return len(c.prog.code) }

func (c *compiler) addClass(s *charSet) int {
	c.prog.classes = append(c.prog.classes, s)
	return len(c.prog.classes) - 1
}

func (c *compiler) compile(n node) {
	switch t := n.(type) {
	case nodeEmpty:
		// Nothing to emit.

	case nodeChar:
		if c.flags&FlagIgnoreCase != 0 {
			c.emit(instr{op: opCharFold, r: foldCase(t.r)})
			return
		}
		c.emit(instr{op: opChar, r: t.r})

	case nodeAny:
		if t.dotAll {
			c.emit(instr{op: opAny})
			return
		}
		c.emit(instr{op: opAnyNotNL})

	case nodeClass:
		set := t.set
		if c.flags&FlagIgnoreCase != 0 && !set.foldCase {
			cp := *set
			cp.foldCase = true
			set = &cp
		}
		c.emit(instr{op: opClass, arg: c.addClass(set)})

	case nodeSeq:
		for _, item := range t.items {
			c.compile(item)
		}

	case nodeAlt:
		c.compileAlt(t)

	case nodeGroup:
		if t.index == 0 {
			c.compile(t.item)
			return
		}
		c.emit(instr{op: opSave, arg: 2 * t.index})
		c.compile(t.item)
		c.emit(instr{op: opSave, arg: 2*t.index + 1})

	case nodeRepeat:
		c.compileRepeat(t)

	case nodeAssert:
		switch t.kind {
		case assertStart:
			c.emit(instr{op: opAssertStart})
		case assertEnd:
			c.emit(instr{op: opAssertEnd})
		case assertWordBoundary:
			c.emit(instr{op: opWordBoundary})
		default:
			c.emit(instr{op: opNotWordBoundary})
		}

	case nodeLook:
		c.compileLook(t)

	case *nodeBackref:
		op := opBackref
		if c.flags&FlagIgnoreCase != 0 {
			op = opBackrefFold
		}
		c.emit(instr{op: op, arg: t.index})
	}
}

// compileAlt lays out alternatives as a chain of choice points.
func (c *compiler) compileAlt(t nodeAlt) {
	var exits []int
	for i, alt := range t.alts {
		if i == len(t.alts)-1 {
			// The last alternative needs no choice point: failing it fails the
			// whole alternation.
			c.compile(alt)
			break
		}
		split := c.emit(instr{op: opSplit})
		c.prog.code[split].arg = c.here()
		c.compile(alt)
		exits = append(exits, c.emit(instr{op: opJmp}))
		c.prog.code[split].arg2 = c.here()
	}
	for _, e := range exits {
		c.prog.code[e].arg = c.here()
	}
}

// compileRepeat emits a quantifier.
//
// The common unbounded forms get a two-instruction loop. A counted form uses a
// runtime counter rather than being unrolled, so that {0,65535} is small.
func (c *compiler) compileRepeat(t nodeRepeat) {
	// A body that can match the empty string needs a guard, or an unbounded
	// repetition of it would never terminate.
	needsGuard := t.max < 0 && canMatchEmpty(t.item)

	switch {
	case t.min == 0 && t.max == 1:
		// ?
		split := c.emit(instr{op: opSplit})
		c.setSplit(split, t.greedy, c.here(), 0)
		c.compile(t.item)
		c.patchSplitAlt(split, t.greedy, c.here())
		return

	case t.min == 0 && t.max < 0:
		// *
		start := c.here()
		split := c.emit(instr{op: opSplit})
		c.setSplit(split, t.greedy, c.here(), 0)
		guard := -1
		if needsGuard {
			guard = c.prog.emptyChecks
			c.prog.emptyChecks++
			c.emit(instr{op: opEmptyCheck, arg: guard, arg2: 0})
		}
		c.compile(t.item)
		if needsGuard {
			c.emit(instr{op: opEmptyCheck, arg: guard, arg2: 1})
		}
		c.emit(instr{op: opJmp, arg: start})
		c.patchSplitAlt(split, t.greedy, c.here())
		return

	case t.min == 1 && t.max < 0:
		// +
		start := c.here()
		guard := -1
		if needsGuard {
			guard = c.prog.emptyChecks
			c.prog.emptyChecks++
			c.emit(instr{op: opEmptyCheck, arg: guard, arg2: 0})
		}
		c.compile(t.item)
		if needsGuard {
			c.emit(instr{op: opEmptyCheck, arg: guard, arg2: 1})
		}
		split := c.emit(instr{op: opSplit})
		c.setSplit(split, t.greedy, start, c.here())
		return
	}

	// The general counted form.
	counter := c.prog.counters
	c.prog.counters++
	c.emit(instr{op: opCounterInit, arg: counter})

	start := c.here()
	// opCounterInc carries the bounds: it consumes an iteration if one is
	// available and branches otherwise.
	inc := c.emit(instr{op: opCounterInc, arg: counter, arg2: t.min})
	c.prog.code[inc].r = rune(t.max)

	split := c.emit(instr{op: opSplit})
	c.setSplit(split, t.greedy, c.here(), 0)
	c.compile(t.item)
	c.emit(instr{op: opJmp, arg: start})
	c.patchSplitAlt(split, t.greedy, c.here())
}

// setSplit points a choice point at its two targets, swapping them for a lazy
// quantifier so that the shorter match is tried first.
func (c *compiler) setSplit(pc int, greedy bool, preferred, other int) {
	if greedy {
		c.prog.code[pc].arg, c.prog.code[pc].arg2 = preferred, other
		return
	}
	c.prog.code[pc].arg, c.prog.code[pc].arg2 = other, preferred
}

// patchSplitAlt fills in whichever branch of a choice point was left open.
func (c *compiler) patchSplitAlt(pc int, greedy bool, target int) {
	if greedy {
		c.prog.code[pc].arg2 = target
		return
	}
	c.prog.code[pc].arg = target
}

// compileLook compiles a lookaround into its own program.
func (c *compiler) compileLook(t nodeLook) {
	sub := &compiler{prog: c.prog, flags: c.flags}
	// The body is compiled into a scratch buffer and then moved, so that its
	// jump targets are relative to its own start.
	saved := c.prog.code
	c.prog.code = nil
	sub.compile(t.item)
	sub.emit(instr{op: opMatch})
	body := c.prog.code
	c.prog.code = saved

	idx := len(c.prog.looks)
	c.prog.looks = append(c.prog.looks, lookProgram{
		code: body, behind: t.behind, negate: t.negate,
	})
	c.emit(instr{op: opLook, arg: idx})
}

// canMatchEmpty reports whether a node can succeed without consuming input.
//
// A repetition whose body can match nothing needs a guard, because the matcher
// would otherwise loop on it forever: (a*)* is the classic case.
func canMatchEmpty(n node) bool {
	switch t := n.(type) {
	case nodeEmpty, nodeAssert, nodeLook:
		return true
	case nodeChar, nodeAny, nodeClass:
		return false
	case *nodeBackref:
		// A backreference to a group that matched nothing consumes nothing.
		return true
	case nodeSeq:
		for _, item := range t.items {
			if !canMatchEmpty(item) {
				return false
			}
		}
		return true
	case nodeAlt:
		for _, alt := range t.alts {
			if canMatchEmpty(alt) {
				return true
			}
		}
		return false
	case nodeGroup:
		return canMatchEmpty(t.item)
	case nodeRepeat:
		return t.min == 0 || canMatchEmpty(t.item)
	}
	return true
}

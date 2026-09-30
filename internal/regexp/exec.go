package regexp

import (
	"errors"
	"unicode/utf16"
	"unicode/utf8"
)

// The backtracking matcher.
//
// Backtracking is explicit rather than recursive: choice points go on a stack
// that the loop pops on failure. That keeps a deeply nested pattern from
// overflowing the goroutine stack, and it makes the step budget easy to apply.
//
// The budget matters. A backtracking engine has an exponential worst case, and
// a pattern like /(a+)+b/ against a long run of "a" will hit it. An engine
// embedded in a host cannot be allowed to hang, so the matcher gives up after a
// bounded number of steps and reports ErrComplexity, which the caller turns
// into a thrown error.
//
// A caller that can be interrupted -- a JavaScript runtime, which has a
// deadline of its own -- passes a check instead, which the matcher asks every
// few thousand steps, and has no budget: a match takes as long as it takes,
// as it does in V8, until the check says to stop.

// ErrComplexity reports that a match exceeded its step budget.
var ErrComplexity = errors.New("regular expression is too complex")

// maxSteps bounds the work one match attempt may do.
//
// The figure is large enough that no reasonable pattern reaches it -- matching
// a megabyte of text with a linear pattern costs a few million steps -- and
// small enough that a pathological one fails in well under a second.
const maxSteps = 100_000_000

// checkInterval is how many steps a matcher with a check takes between asking
// it.
const checkInterval = 4096

// input is the subject string, viewed either as UTF-16 code units or as code
// points depending on the unicode flag.
type input struct {
	units   []uint16
	unicode bool
}

// length returns the number of positions in the input. Positions are always
// code-unit indices, because lastIndex and the reported capture bounds are.
func (in *input) length() int { return len(in.units) }

// at returns the code point starting at position i and its width in code
// units.
//
// Under the unicode flag a surrogate pair is one character two units wide, so
// that a quantifier applies to the whole of it. Otherwise each unit stands
// alone, which is what makes /./ match half of an emoji.
func (in *input) at(i int) (rune, int) {
	if i < 0 || i >= len(in.units) {
		return -1, 0
	}
	c := rune(in.units[i])
	if in.unicode && utf16.IsSurrogate(c) && i+1 < len(in.units) {
		if combined := utf16.DecodeRune(c, rune(in.units[i+1])); combined != utf8.RuneError {
			return combined, 2
		}
	}
	return c, 1
}

// before returns the code point ending at position i, which lookbehind and the
// word-boundary assertion need.
func (in *input) before(i int) (rune, int) {
	if i <= 0 {
		return -1, 0
	}
	c := rune(in.units[i-1])
	if in.unicode && utf16.IsSurrogate(c) && i >= 2 {
		if combined := utf16.DecodeRune(rune(in.units[i-2]), c); combined != utf8.RuneError {
			return combined, 2
		}
	}
	return c, 1
}

// frame is a saved choice point.
// greedyFrame marks, in counterIdx, the choice point of an opGreedy run:
// pos is where the run now ends and counterVal the least it may end at.
const greedyFrame = -2

// frame is a choice point: where to resume, and what to put back first.
//
// Its fields are 32 bits, which every position, count and index fits in --
// the longest string is shorter than that, and a quantifier's bounds are
// kept in a rune -- so that a choice point, which a match pushes one of at
// every turn, is half the size.
type frame struct {
	pc  int32
	pos int32
	// capsLen records how many capture writes had happened, so that undoing a
	// choice point restores the captures as well as the position.
	capsLen int32
	// counter state for the innermost counted quantifier, if any.
	counterIdx int32
	counterVal int32
	// emptyIdx and emptyVal restore an empty-check mark.
	emptyIdx int32
	emptyVal int32
}

// capWrite is one recorded capture assignment, kept so that backtracking can
// roll it back.
type capWrite struct {
	slot int
	prev int
}

// matcher holds the state of one match attempt.
type matcher struct {
	prog  *program
	in    *input
	input input
	caps  []int
	trail []capWrite
	stack []frame

	counters []int
	// emptyMarks holds the position at which each guarded repetition last
	// began an iteration.
	emptyMarks []int

	steps int
	// check, when there is one, is asked every checkInterval steps whether to
	// stop, and the step budget does not apply.
	check func() error
	// busy marks the matcher a pattern lends out, so that a pattern used again
	// while a match is running does not have its state overwritten.
	busy bool
}

// exec runs the program from a starting position, returning the capture slots
// or nil if there is no match.
//
// The matcher is borrowed from the pattern rather than made afresh: matching
// the same pattern over and over is what a program does with one, and its
// backtracking stack and capture trail are what it spends its allocations on.
// A pattern used again while a match is running -- a replacement callback that
// uses the same one -- gets a matcher of its own, as does every match of a
// pattern marked concurrent.
// maxLentFrames is the most choice points and capture writes a lent matcher
// keeps room for: some kilobytes, for each pattern a runtime keeps compiled.
const maxLentFrames = 256

func (re *Regexp) exec(dst []int, units []uint16, start int, check func() error, sticky bool) ([]int, error) {
	// A clone with no matcher of its own borrows its lender's.
	owner, m := re, re.scratch
	if m == nil && re.lender != nil {
		owner, m = re.lender, re.lender.scratch
	}
	if m == nil || m.busy || re.concurrent {
		m = &matcher{
			caps:       make([]int, 2*(re.groupCount+1)),
			counters:   make([]int, re.prog.counters),
			emptyMarks: make([]int, re.prog.emptyChecks),
		}
		switch {
		case re.concurrent:
		case owner.scratch == nil:
			owner.scratch = m
		case re.scratch == nil:
			owner, re.scratch = re, m
		}
	}
	// The input is the matcher's own, rather than made for each match.
	m.input = input{units: units, unicode: re.flags&FlagUnicode != 0}
	in := &m.input
	m.prog, m.in, m.check = re.prog, in, check
	// The budget is what this attempt may spend, so it starts again here: a
	// matcher is lent out over and over, and a pattern that had spent its
	// budget once would have been refused for the rest of the program.
	m.steps = 0
	m.busy = true
	defer func() {
		m.busy = false
		// The input is not held on to: it would keep the subject string alive
		// for as long as the pattern.
		m.input, m.in, m.check = input{}, nil, nil
		// A lender keeps only a small matcher, since it lives as long as
		// the runtime's cache of patterns: one a match has made large is
		// the borrower's from now on, and goes when the borrower does.
		if owner != re && owner.scratch == m && (cap(m.stack) > maxLentFrames || cap(m.trail) > maxLentFrames) {
			owner.scratch, re.scratch = nil, m
		}
	}()

	first := re.prog.first
	// lit is the required literal and litAt where it was last found: a match
	// can begin only where it is within reach, so the positions before that
	// are passed over, and a subject without it has no match.
	lit, litAt := re.prog.lit, -1
	if sticky || in.unicode {
		lit = nil
	}
	m.reset()
	for pos := start; pos <= in.length(); {
		if lit != nil {
			if litAt < pos+lit.minOff {
				if litAt = indexUnits(in.units, lit.units, pos+lit.minOff); litAt < 0 {
					return nil, nil
				}
			}
			pos = max(pos, litAt-lit.maxOff)
		}
		if first != nil && !in.unicode && !sticky {
			// Where no match can begin, none is tried. Without the unicode
			// flag a character is a code unit, so the units no match can
			// begin with are passed over in a loop of their own.
			units := in.units
			for pos < len(units) && !first.admits(units[pos]) {
				pos++
			}
			if pos >= len(units) {
				return nil, nil
			}
			if lit != nil && litAt < pos+lit.minOff {
				// Past where the literal could be reached from: it is
				// looked for again.
				continue
			}
		} else if first != nil {
			// Where no match can begin, none is tried: a position is passed
			// over as the search below would pass it, a character at a
			// time, so that under the unicode flag it never lands inside a
			// surrogate pair.
			if pos >= len(in.units) || !first.admits(in.units[pos]) {
				if pos >= len(in.units) || sticky {
					return nil, nil
				}
				_, w := in.at(pos)
				if w == 0 {
					w = 1
				}
				pos += w
				continue
			}
		}
		ok, err := m.run(re.prog.code, pos)
		if err != nil {
			return nil, err
		}
		if ok {
			return append(dst[:0], m.caps...), nil
		}
		// A failed attempt has put back everything it changed but the
		// captures it wrote before its first choice point, which are taken
		// back here: the next position starts from the state this one did,
		// with no reset to pay for.
		if len(m.trail) != 0 {
			m.undoCaps(0)
		}
		// A sticky pattern is anchored at the start position and does not
		// search forward.
		if sticky {
			return nil, nil
		}
		// Advance by a whole character, so that under the unicode flag the
		// search never starts in the middle of a surrogate pair.
		if !in.unicode {
			pos++
			continue
		}
		_, w := in.at(pos)
		if w == 0 {
			w = 1
		}
		pos += w
	}
	return nil, nil
}

func (m *matcher) reset() {
	for i := range m.caps {
		m.caps[i] = -1
	}
	for i := range m.counters {
		m.counters[i] = 0
	}
	for i := range m.emptyMarks {
		m.emptyMarks[i] = -1
	}
	m.trail = m.trail[:0]
	m.stack = m.stack[:0]
}

// run executes a program from pos, reporting whether it matched.
func (m *matcher) run(code []instr, pos int) (bool, error) {
	pc := 0
	// A forward read of a unit that is a character of its own -- any unit
	// without the unicode flag, and any but a surrogate with it -- is done
	// in place by the instructions that read one, rather than by read: the
	// call is most of what the commonest instructions cost.
	units, unicode := m.in.units, m.in.unicode
	for {
		m.steps++
		if m.check != nil {
			if m.steps%checkInterval == 0 {
				if err := m.check(); err != nil {
					return false, err
				}
			}
		} else if m.steps > maxSteps {
			return false, ErrComplexity
		}

		in := &code[pc]
		switch in.op {
		case opChar:
			if !in.rev && uint(pos) < uint(len(units)) {
				if u := units[pos]; !unicode || !utf16.IsSurrogate(rune(u)) {
					if rune(u) != in.r {
						goto backtrack
					}
					pos++
					pc++
					break
				}
			}
			r, w := m.read(in.rev, pos)
			if r != in.r {
				goto backtrack
			}
			pos = m.advance(in.rev, pos, w)
			pc++

		case opCharFold:
			r, w := m.read(in.rev, pos)
			if r < 0 || canonical(r, m.prog.unicodeFold) != in.r {
				goto backtrack
			}
			pos = m.advance(in.rev, pos, w)
			pc++

		case opClass:
			if !in.rev && uint(pos) < uint(len(units)) {
				if u := units[pos]; u < 128 {
					if m.prog.asciiClasses[in.arg][u>>6]&(1<<(u&63)) == 0 {
						goto backtrack
					}
					pos++
					pc++
					break
				}
			}
			r, w := m.read(in.rev, pos)
			if r < 0 {
				goto backtrack
			}
			if r < 128 {
				if m.prog.asciiClasses[in.arg][r>>6]&(1<<(r&63)) == 0 {
					goto backtrack
				}
			} else if !m.prog.classes[in.arg].contains(r, m.prog.unicodeFold) {
				goto backtrack
			}
			pos = m.advance(in.rev, pos, w)
			pc++

		case opAny:
			r, w := m.read(in.rev, pos)
			if r < 0 {
				goto backtrack
			}
			pos = m.advance(in.rev, pos, w)
			pc++

		case opAnyNotNL:
			r, w := m.read(in.rev, pos)
			if r < 0 || isLineTerminator(r) {
				goto backtrack
			}
			pos = m.advance(in.rev, pos, w)
			pc++

		case opSplit:
			// The second branch becomes a choice point to return to, pushed
			// here as push would push it.
			m.stack = append(m.stack, frame{pc: int32(in.arg2), pos: int32(pos), capsLen: int32(len(m.trail)), counterIdx: -1})
			pc = in.arg

		case opJmp:
			pc = in.arg

		case opSave:
			m.setCap(in.arg, pos)
			pc++

		case opMatch:
			return true, nil

		case opAssertStart:
			if pos == 0 {
				pc++
				break
			}
			// Multiline is carried by the instruction rather than read from
			// the pattern's flags, because an inline modifier can turn it on
			// or off for part of the pattern.
			if in.arg != 0 {
				if r, _ := m.in.before(pos); isLineTerminator(r) {
					pc++
					break
				}
			}
			goto backtrack

		case opAssertEnd:
			if pos == m.in.length() {
				pc++
				break
			}
			if in.arg != 0 {
				if r, _ := m.in.at(pos); isLineTerminator(r) {
					pc++
					break
				}
			}
			goto backtrack

		case opWordBoundary, opNotWordBoundary:
			prev, _ := m.in.before(pos)
			next, _ := m.in.at(pos)
			fold := in.arg != 0
			atBoundary := isWordChar(prev, fold) != isWordChar(next, fold)
			if atBoundary == (in.op == opWordBoundary) {
				pc++
				break
			}
			goto backtrack

		case opBackref, opBackrefFold:
			g := in.arg
			if in.arg2 != 0 {
				for _, k := range m.prog.refSets[in.arg2-1] {
					if m.caps[2*k] >= 0 {
						g = k
						break
					}
				}
			}
			if 2*g+1 >= len(m.caps) {
				// No such group: it matched nothing, as one that did not
				// take part would have.
				pc++
				break
			}
			start, end := m.caps[2*g], m.caps[2*g+1]
			if start < 0 || end < 0 {
				// A group that never participated matches the empty string
				// rather than failing.
				pc++
				break
			}
			n := end - start
			// Leftwards the reference matches the text ending at the cursor,
			// so the comparison starts n units before it.
			at := pos
			if in.rev {
				at = pos - n
				if at < 0 {
					goto backtrack
				}
			} else if pos+n > m.in.length() {
				goto backtrack
			}
			if !m.compareRange(start, at, n, in.op == opBackrefFold) {
				goto backtrack
			}
			pos = at
			if !in.rev {
				pos = pos + n
			}
			pc++

		case opLook:
			ok, err := m.runLook(in.arg, pos)
			if err != nil {
				return false, err
			}
			if !ok {
				goto backtrack
			}
			pc++

		case opCounterInit:
			m.counters[in.arg] = 0
			pc++

		case opCounterInc:
			// The counter is incremented and checked against the bounds the
			// instruction carries: min in arg2, max in r.
			min, max := in.arg2, int(in.r)
			cur := m.counters[in.arg]
			if max >= 0 && cur >= max {
				// The maximum is reached, so the loop must exit. The exit is
				// two instructions on: past the split.
				pc = m.exitOfCounterLoop(code, pc)
				break
			}
			m.counters[in.arg] = cur + 1
			m.push(frame{
				pc: -1, counterIdx: int32(in.arg), counterVal: int32(cur),
				pos: int32(pos), capsLen: int32(len(m.trail)), emptyIdx: -1,
			})
			if cur < min {
				// Below the minimum the body is mandatory, so the choice point
				// the following split would create is skipped.
				pc += 2
				break
			}
			pc++

		case opClearCaps:
			// Every iteration starts with the groups inside it unset: one that
			// matched on an earlier pass is not part of the match unless it
			// matches again.
			for g := in.arg; g <= in.arg2; g++ {
				if m.caps[2*g] >= 0 || m.caps[2*g+1] >= 0 {
					m.setCap(2*g, -1)
					m.setCap(2*g+1, -1)
				}
			}
			pc++

		case opGreedy:
			// The run is taken whole, and each unit of it counts as the step
			// it would have been.
			item := code[pc+1]
			units := m.in.units
			end := pos
			// Each kind of unit is scanned for by a loop of its own, which
			// asks nothing it does not need to of each unit.
			switch item.op {
			case opClass:
				ascii := &m.prog.asciiClasses[item.arg]
				for end < len(units) {
					if u := units[end]; u < 128 {
						if ascii[u>>6]&(1<<(u&63)) == 0 {
							break
						}
					} else if !m.prog.classes[item.arg].contains(rune(u), m.prog.unicodeFold) {
						break
					}
					end++
				}
			case opChar:
				for end < len(units) && rune(units[end]) == item.r {
					end++
				}
			case opAny:
				end = len(units)
			default:
				for end < len(units) && m.unitMatches(item, units[end]) {
					end++
				}
			}
			m.steps += end - pos
			if end-pos < in.arg {
				goto backtrack
			}
			if lo := pos + in.arg; end > lo {
				// One choice point stands for every shorter run, down to
				// the least the loop may take.
				m.push(frame{pc: int32(pc + 2), pos: int32(end), capsLen: int32(len(m.trail)), counterIdx: greedyFrame, counterVal: int32(lo)})
			}
			pos = end
			pc += 2

		case opEmptyCheck:
			if in.arg2 == 0 {
				// Entering an iteration: remember where it started.
				m.push(frame{
					pc: -1, emptyIdx: int32(in.arg), emptyVal: int32(m.emptyMarks[in.arg]),
					pos: int32(pos), capsLen: int32(len(m.trail)), counterIdx: -1,
				})
				m.emptyMarks[in.arg] = pos
				pc++
				break
			}
			// Leaving one: if nothing was consumed, the repetition cannot make
			// progress and must stop rather than loop.
			if m.emptyMarks[in.arg] == pos {
				goto backtrack
			}
			pc++

		default:
			goto backtrack
		}
		continue

	backtrack:
		for {
			if len(m.stack) == 0 {
				return false, nil
			}
			f := m.stack[len(m.stack)-1]
			m.stack = m.stack[:len(m.stack)-1]
			if len(m.trail) > int(f.capsLen) {
				m.undoCaps(int(f.capsLen))
			}
			if f.counterIdx == greedyFrame {
				// A greedy run gives back one unit, and stays a choice
				// point while it has more to give.
				p := f.pos - 1
				if p > f.counterVal {
					f.pos = p
					m.stack = append(m.stack, f)
				}
				pc, pos = int(f.pc), int(p)
				break
			}
			if f.counterIdx >= 0 && f.pc == -1 {
				m.counters[f.counterIdx] = int(f.counterVal)
				continue
			}
			if f.emptyIdx >= 0 && f.pc == -1 {
				m.emptyMarks[f.emptyIdx] = int(f.emptyVal)
				continue
			}
			pc, pos = int(f.pc), int(f.pos)
			break
		}
	}
}

// exitOfCounterLoop finds where a counted loop continues once its maximum is
// reached.
//
// The layout emitted by compileRepeat is: opCounterInc, opSplit, body...,
// opJmp. The split's non-body branch is the exit.
func (m *matcher) exitOfCounterLoop(code []instr, pc int) int {
	split := code[pc+1]
	if split.op != opSplit {
		return pc + 1
	}
	// The body branch is whichever target follows the split; the other is the
	// exit.
	if split.arg == pc+2 {
		return split.arg2
	}
	return split.arg
}

func (m *matcher) push(f frame) {
	if f.counterIdx == 0 && f.pc != -1 {
		f.counterIdx = -1
	}
	m.stack = append(m.stack, f)
}

// unitMatches reports whether one code unit is what a one-unit matcher --
// the instruction opGreedy repeats -- matches.
func (m *matcher) unitMatches(in instr, u uint16) bool {
	r := rune(u)
	switch in.op {
	case opChar:
		return r == in.r
	case opCharFold:
		return canonical(r, m.prog.unicodeFold) == in.r
	case opClass:
		if r < 128 {
			return m.prog.asciiClasses[in.arg][r>>6]&(1<<(r&63)) != 0
		}
		return m.prog.classes[in.arg].contains(r, m.prog.unicodeFold)
	case opAny:
		return true
	default:
		return !isLineTerminator(r)
	}
}

// setCap records a capture write so that backtracking can undo it.
func (m *matcher) setCap(slot, pos int) {
	m.trail = append(m.trail, capWrite{slot: slot, prev: m.caps[slot]})
	m.caps[slot] = pos
}

func (m *matcher) undoCaps(n int) {
	for len(m.trail) > n {
		w := m.trail[len(m.trail)-1]
		m.trail = m.trail[:len(m.trail)-1]
		m.caps[w.slot] = w.prev
	}
}

// read returns the code point the cursor is about to consume, which is the one
// after it going rightwards and the one before it going leftwards.
func (m *matcher) read(rev bool, pos int) (rune, int) {
	if rev {
		return m.in.before(pos)
	}
	return m.in.at(pos)
}

// advance moves the cursor over a code point in the direction being matched.
func (m *matcher) advance(rev bool, pos, w int) int {
	if rev {
		return pos - w
	}
	return pos + w
}

// compareRange tests whether n code units at two positions are equal.
func (m *matcher) compareRange(a, b, n int, fold bool) bool {
	for i := 0; i < n; i++ {
		x, y := rune(m.in.units[a+i]), rune(m.in.units[b+i])
		if x == y {
			continue
		}
		if !fold || canonical(x, m.prog.unicodeFold) != canonical(y, m.prog.unicodeFold) {
			return false
		}
	}
	return true
}

// runLook evaluates a lookaround at a position.
func (m *matcher) runLook(idx, pos int) (bool, error) {
	look := m.prog.looks[idx]

	// A lookaround runs in its own matcher state, but shares the capture array
	// so that a group inside a positive lookahead is visible afterwards -- and
	// the undo log with it, so that backtracking past the lookaround takes
	// those captures back again.
	base := len(m.trail)
	sub := &matcher{
		prog:       m.prog,
		in:         m.in,
		caps:       m.caps,
		trail:      m.trail,
		counters:   make([]int, m.prog.counters),
		emptyMarks: make([]int, m.prog.emptyChecks),
		steps:      m.steps,
		check:      m.check,
	}
	for i := range sub.emptyMarks {
		sub.emptyMarks[i] = -1
	}

	// A lookbehind's body was compiled to match leftwards, so it runs from the
	// lookbehind's position like any other sub-program and walks back from
	// there.
	ok, err := sub.run(look.code, pos)
	m.steps = sub.steps
	if err != nil {
		m.trail = sub.trail
		return false, err
	}

	// A lookaround that did not match leaves no captures behind, and neither
	// does a negative one that did: in both cases nothing of it is part of the
	// match.
	if !ok || look.negate {
		sub.undoCaps(base)
	}
	m.trail = sub.trail
	if look.negate {
		return !ok, nil
	}
	return ok, nil
}

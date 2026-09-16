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

// ErrComplexity reports that a match exceeded its step budget.
var ErrComplexity = errors.New("regular expression is too complex")

// maxSteps bounds the work one match attempt may do.
//
// The figure is large enough that no reasonable pattern reaches it -- matching
// a megabyte of text with a linear pattern costs a few million steps -- and
// small enough that a pathological one fails in well under a second.
const maxSteps = 100_000_000

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
type frame struct {
	pc  int
	pos int
	// capsLen records how many capture writes had happened, so that undoing a
	// choice point restores the captures as well as the position.
	capsLen int
	// counter state for the innermost counted quantifier, if any.
	counterIdx int
	counterVal int
	// emptyIdx and emptyVal restore an empty-check mark.
	emptyIdx int
	emptyVal int
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
	caps  []int
	trail []capWrite
	stack []frame

	counters []int
	// emptyMarks holds the position at which each guarded repetition last
	// began an iteration.
	emptyMarks []int

	steps int
	// multiline affects the ^ and $ assertions and is read from the flags.
	multiline bool

	// useAnchor and anchorEnd require a match to finish at an exact position,
	// which is how lookbehind is evaluated: the body is run forwards from each
	// candidate start and must land on the lookbehind's position.
	useAnchor bool
	anchorEnd int
}

// exec runs the program from a starting position, returning the capture slots
// or nil if there is no match.
func (re *Regexp) exec(in *input, start int) ([]int, error) {
	m := &matcher{
		prog:       re.prog,
		in:         in,
		caps:       make([]int, 2*(re.groupCount+1)),
		counters:   make([]int, re.prog.counters),
		emptyMarks: make([]int, re.prog.emptyChecks),
		multiline:  re.flags&FlagMultiline != 0,
	}

	for pos := start; pos <= in.length(); {
		m.reset()
		ok, err := m.run(re.prog.code, pos)
		if err != nil {
			return nil, err
		}
		if ok {
			out := make([]int, len(m.caps))
			copy(out, m.caps)
			return out, nil
		}
		// A sticky pattern is anchored at the start position and does not
		// search forward.
		if re.flags&FlagSticky != 0 {
			return nil, nil
		}
		// Advance by a whole character, so that under the unicode flag the
		// search never starts in the middle of a surrogate pair.
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
	for {
		m.steps++
		if m.steps > maxSteps {
			return false, ErrComplexity
		}

		in := code[pc]
		switch in.op {
		case opChar:
			r, w := m.in.at(pos)
			if r != in.r {
				goto backtrack
			}
			pos += w
			pc++

		case opCharFold:
			r, w := m.in.at(pos)
			if r < 0 || foldCase(r) != in.r {
				goto backtrack
			}
			pos += w
			pc++

		case opClass:
			r, w := m.in.at(pos)
			if r < 0 || !m.prog.classes[in.arg].contains(r) {
				goto backtrack
			}
			pos += w
			pc++

		case opAny:
			r, w := m.in.at(pos)
			if r < 0 {
				goto backtrack
			}
			pos += w
			pc++

		case opAnyNotNL:
			r, w := m.in.at(pos)
			if r < 0 || isLineTerminator(r) {
				goto backtrack
			}
			pos += w
			pc++

		case opSplit:
			// The second branch becomes a choice point to return to.
			m.push(frame{pc: in.arg2, pos: pos, capsLen: len(m.trail)})
			pc = in.arg

		case opJmp:
			pc = in.arg

		case opSave:
			m.setCap(in.arg, pos)
			pc++

		case opMatch:
			if m.useAnchor && pos != m.anchorEnd {
				// The body matched, but not ending where a lookbehind needs it
				// to, so this is a failure like any other.
				goto backtrack
			}
			return true, nil

		case opAssertStart:
			if pos == 0 {
				pc++
				break
			}
			if m.multiline {
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
			if m.multiline {
				if r, _ := m.in.at(pos); isLineTerminator(r) {
					pc++
					break
				}
			}
			goto backtrack

		case opWordBoundary, opNotWordBoundary:
			prev, _ := m.in.before(pos)
			next, _ := m.in.at(pos)
			atBoundary := isWordChar(prev) != isWordChar(next)
			if atBoundary == (in.op == opWordBoundary) {
				pc++
				break
			}
			goto backtrack

		case opBackref, opBackrefFold:
			start, end := m.caps[2*in.arg], m.caps[2*in.arg+1]
			if start < 0 || end < 0 {
				// A group that never participated matches the empty string
				// rather than failing.
				pc++
				break
			}
			n := end - start
			if pos+n > m.in.length() {
				goto backtrack
			}
			if !m.compareRange(start, pos, n, in.op == opBackrefFold) {
				goto backtrack
			}
			pos += n
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
				pc: -1, counterIdx: in.arg, counterVal: cur,
				pos: pos, capsLen: len(m.trail), emptyIdx: -1,
			})
			if cur < min {
				// Below the minimum the body is mandatory, so the choice point
				// the following split would create is skipped.
				pc += 2
				break
			}
			pc++

		case opEmptyCheck:
			if in.arg2 == 0 {
				// Entering an iteration: remember where it started.
				m.push(frame{
					pc: -1, emptyIdx: in.arg, emptyVal: m.emptyMarks[in.arg],
					pos: pos, capsLen: len(m.trail), counterIdx: -1,
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
			m.undoCaps(f.capsLen)
			if f.counterIdx >= 0 && f.pc == -1 {
				m.counters[f.counterIdx] = f.counterVal
				continue
			}
			if f.emptyIdx >= 0 && f.pc == -1 {
				m.emptyMarks[f.emptyIdx] = f.emptyVal
				continue
			}
			pc, pos = f.pc, f.pos
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

// compareRange tests whether n code units at two positions are equal.
func (m *matcher) compareRange(a, b, n int, fold bool) bool {
	for i := 0; i < n; i++ {
		x, y := rune(m.in.units[a+i]), rune(m.in.units[b+i])
		if x == y {
			continue
		}
		if !fold || foldCase(x) != foldCase(y) {
			return false
		}
	}
	return true
}

// runLook evaluates a lookaround at a position.
func (m *matcher) runLook(idx, pos int) (bool, error) {
	look := m.prog.looks[idx]

	// A lookaround runs in its own matcher state, but shares the capture array
	// so that a group inside a positive lookahead is visible afterwards.
	sub := &matcher{
		prog:       m.prog,
		in:         m.in,
		caps:       m.caps,
		counters:   make([]int, m.prog.counters),
		emptyMarks: make([]int, m.prog.emptyChecks),
		multiline:  m.multiline,
		steps:      m.steps,
	}
	for i := range sub.emptyMarks {
		sub.emptyMarks[i] = -1
	}

	var ok bool
	var err error
	if look.behind {
		// Lookbehind is matched by trying every start position that could end
		// here. The bodies are short in practice, so the quadratic worst case
		// does not bite; a reverse-matching engine would avoid it entirely.
		for start := pos; start >= 0; start-- {
			sub.trail = sub.trail[:0]
			sub.stack = sub.stack[:0]
			matched, e := sub.runAnchored(look.code, start, pos)
			if e != nil {
				err = e
				break
			}
			if matched {
				ok = true
				break
			}
		}
	} else {
		ok, err = sub.run(look.code, pos)
	}
	m.steps = sub.steps
	if err != nil {
		return false, err
	}

	if look.negate {
		// A negative lookaround must not leave captures behind, since it did
		// not match.
		return !ok, nil
	}
	return ok, nil
}

// runAnchored runs a program with the additional requirement that it finish
// exactly at end, which is what lookbehind needs.
func (m *matcher) runAnchored(code []instr, start, end int) (bool, error) {
	m.anchorEnd = end
	m.useAnchor = true
	defer func() { m.useAnchor = false }()
	return m.run(code, start)
}

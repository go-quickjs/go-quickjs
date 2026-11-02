package regexp

// A pattern that is one character class and nothing else -- /["'&<>]/, an
// HTML escaper's test for anything to escape; /\s/, /[^\w-]/ -- matches the
// first unit from the start on that the class takes in, one unit long. The
// matcher found it a position at a time: borrowed, made ready, reset for
// each, its program run from the first instruction, to give the bounds back
// in its own captures -- more than the search itself for a short subject.
// As a literal's search (litOnly), it is answered here without one, with
// the class's own bits for ASCII (program.asciiClasses, folding and
// negation applied), which the matcher's opClass reads too, and the class
// itself for any other unit where the pattern reads code units, not code
// points.

// classOnlyOf reports whether code, n's program, is n's one class and
// nothing else: the whole match's bounds saved around it, read forwards.
func classOnlyOf(n node, code []instr) bool {
	if _, ok := n.(nodeClass); !ok || len(code) != 4 {
		return false
	}
	in := code[1]
	return code[0].op == opSave && in.op == opClass && in.arg == 0 && !in.rev && code[2].op == opSave && code[3].op == opMatch
}

// classIndexASCII is the first position from start on, or at start alone
// where sticky is set, of a byte of b, ASCII, that class's bits take in, or
// -1.
func classIndexASCII(b []byte, class *[2]uint64, start int, sticky bool) int {
	end := len(b)
	if sticky {
		end = min(end, start+1)
	}
	for i := start; i < end; i++ {
		if u := b[i]; class[u>>6]&(1<<(u&63)) != 0 {
			return i
		}
	}
	return -1
}

// classMatchASCII is the match of a pattern that is one class (program's
// classOnly) in b, ASCII, from start, or at start alone where sticky is
// set, into dst, or nil. It is asked before the matcher's entries
// (execASCIILent, execASCII) are called, which keep their code as it was
// for every other pattern: with the scan inlined in them, the patterns
// they match ran slower.
//
//go:noinline
func (re *Regexp) classMatchASCII(dst []int, b []byte, start int, sticky bool) []int {
	if at := classIndexASCII(b, &re.prog.asciiClasses[0], start, sticky); at >= 0 {
		return append(dst[:0], at, at+1)
	}
	return nil
}

// classIndexUnits is classIndexASCII over code units, for a pattern that
// reads code units: one past ASCII is asked of the class, as the matcher's
// opClass asks it.
func (p *program) classIndexUnits(units []uint16, start int, sticky bool) int {
	end := len(units)
	if sticky {
		end = min(end, start+1)
	}
	class := &p.asciiClasses[0]
	for i := start; i < end; i++ {
		if u := units[i]; u < 128 {
			if class[u>>6]&(1<<(u&63)) != 0 {
				return i
			}
		} else if p.classes[0].contains(rune(u), p.unicodeFold) {
			return i
		}
	}
	return -1
}

// classMatch is the match of a pattern that is one class (program's
// classOnly) in units from start, into dst, or nil; ok is false for a
// pattern that reads code points, which the matcher matches.
func (re *Regexp) classMatch(dst []int, units []uint16, start int, sticky bool) (m []int, ok bool) {
	if re.flags&(FlagUnicode|FlagUnicodeSets) != 0 {
		return nil, false
	}
	if at := re.prog.classIndexUnits(units, start, sticky); at >= 0 {
		return append(dst[:0], at, at+1), true
	}
	return nil, true
}

// NeverFails reports whether a match of the pattern, over an ASCII subject
// where ascii is set, is a search the matcher never makes: one that spends
// no step of the budget and asks no check, and so reports neither
// ErrComplexity nor a check's error, whatever the subject. A pattern of
// nothing but characters is one (litOnly), but for a sticky one, which the
// matcher tries at its one position; a pattern of one class is one, but
// where it reads code points over a subject past ASCII.
func (re *Regexp) NeverFails(ascii bool) bool {
	p := re.prog
	if p.litOnly != nil && re.flags&FlagSticky == 0 {
		return true
	}
	return p.classOnly && (ascii || re.flags&(FlagUnicode|FlagUnicodeSets) == 0)
}

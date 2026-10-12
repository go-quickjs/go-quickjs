package regexp

import "unsafe"

// Footprint reports the memory re takes, for a runtime's memory limit: its
// core, which the clones of one pattern share and which prog identifies, so
// that it is counted once however many patterns use it -- the compiled
// program, the source, the group names and the matcher the core lends --
// and what re holds of its own: itself and its matcher, whose stacks a
// match on a long subject leaves large. The Unicode property tables the
// package shares between patterns are not any pattern's, and are not
// counted, nor is the subject a matcher was last given, which is a
// string's. lender is nil: a clone keeps nothing but the core.
func (re *Regexp) Footprint() (prog unsafe.Pointer, progBytes, ownBytes int, lender *Regexp) {
	ownBytes = int(unsafe.Sizeof(*re))
	if m := re.scratch; m != nil {
		ownBytes += m.footprint()
	}
	c := re.core
	progBytes = int(unsafe.Sizeof(*c)) + len(c.source)
	if c.prog != nil {
		progBytes += c.prog.footprint()
	}
	if m := c.lent; m != nil {
		progBytes += m.footprint()
	}
	for name, slots := range c.groupNames {
		progBytes += 48 + len(name) + cap(slots)*int(unsafe.Sizeof(0))
	}
	return unsafe.Pointer(c), progBytes, ownBytes, nil
}

// footprint is what a program takes.
func (p *program) footprint() int {
	const word = int(unsafe.Sizeof(0))
	n := int(unsafe.Sizeof(*p)) + cap(p.code)*int(unsafe.Sizeof(instr{}))
	if p.first != nil {
		n += int(unsafe.Sizeof(*p.first))
	}
	if p.lit != nil {
		n += int(unsafe.Sizeof(*p.lit)) + cap(p.lit.units)*2
	}
	n += cap(p.litOnly) * 2
	n += cap(p.classes) * word
	for _, c := range p.classes {
		n += c.footprint()
	}
	n += cap(p.asciiClasses) * int(unsafe.Sizeof([2]uint64{}))
	n += cap(p.looks) * int(unsafe.Sizeof(lookProgram{}))
	for _, l := range p.looks {
		n += cap(l.code) * int(unsafe.Sizeof(instr{}))
	}
	n += cap(p.refSets) * int(unsafe.Sizeof([]int(nil)))
	for _, s := range p.refSets {
		n += cap(s) * word
	}
	return n
}

// footprint is what a class takes: its ranges, unless they are still a
// shared property table's.
func (s *charSet) footprint() int {
	n := int(unsafe.Sizeof(*s))
	if len(s.ranges) == 0 || unsafe.SliceData(s.ranges) != s.table {
		n += cap(s.ranges) * int(unsafe.Sizeof(charRange{}))
	}
	return n
}

// footprint is what a matcher holds.
func (m *matcher) footprint() int {
	const word = int(unsafe.Sizeof(0))
	return int(unsafe.Sizeof(*m)) +
		cap(m.caps)*word +
		cap(m.trail)*int(unsafe.Sizeof(capWrite{})) +
		cap(m.stack)*int(unsafe.Sizeof(frame{})) +
		cap(m.counters)*word +
		cap(m.emptyMarks)*word
}

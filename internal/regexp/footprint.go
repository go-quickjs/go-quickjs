package regexp

import (
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/allocsize"
)

// Footprint reports the memory re takes, for a runtime's memory limit: its
// core, which the clones of one pattern share and which prog identifies, so
// that it is counted once however many patterns use it -- the compiled
// program, the source, the group names and the matcher the core lends --
// and what re holds of its own: itself and its matcher, whose stacks a
// match on a long subject leaves large. The Unicode property tables the
// package shares between patterns are not any pattern's, and are not
// counted, nor is the subject a matcher was last given, which is a
// string's. lender is nil: a clone keeps nothing but the core. Each
// allocation is counted as the allocator rounds it (allocsize), which on a
// 32-bit platform, whose structs fall below their size classes by more,
// was up to a sixth more than their sizes.
func (re *Regexp) Footprint() (prog unsafe.Pointer, progBytes, ownBytes int, lender *Regexp) {
	ownBytes = allocsize.Of(int(unsafe.Sizeof(*re)))
	if m := re.scratch; m != nil {
		ownBytes += m.footprint()
	}
	c := re.core
	progBytes = allocsize.Of(int(unsafe.Sizeof(*c))) + allocsize.Of(len(c.source))
	if c.prog != nil {
		progBytes += c.prog.footprint()
	}
	if m := c.lent; m != nil {
		progBytes += m.footprint()
	}
	for name, slots := range c.groupNames {
		progBytes += 48 + len(name) + allocsize.Of(cap(slots)*int(unsafe.Sizeof(0)))
	}
	return unsafe.Pointer(c), progBytes, ownBytes, nil
}

// footprint is what a program takes.
func (p *program) footprint() int {
	const word = int(unsafe.Sizeof(0))
	n := allocsize.Of(int(unsafe.Sizeof(*p))) + allocsize.Of(cap(p.code)*int(unsafe.Sizeof(instr{})))
	if p.first != nil {
		n += allocsize.Of(int(unsafe.Sizeof(*p.first)))
	}
	if p.lit != nil {
		n += allocsize.Of(int(unsafe.Sizeof(*p.lit))) + p.lit.dataFootprint()
	}
	if p.litOnly != nil {
		n += p.litOnly.footprint()
	}
	if p.foldLit != nil {
		n += p.foldLit.footprint()
	}
	n += allocsize.Of(cap(p.firsts) * int(unsafe.Sizeof(firstUnits{})))
	n += allocsize.Of(cap(p.runs) * word)
	for _, l := range p.runs {
		n += l.footprint()
	}
	n += allocsize.Of(cap(p.classes) * word)
	for _, c := range p.classes {
		n += c.footprint()
	}
	n += allocsize.Of(cap(p.asciiClasses) * int(unsafe.Sizeof([2]uint64{})))
	n += allocsize.Of(cap(p.looks) * int(unsafe.Sizeof(lookProgram{})))
	for _, l := range p.looks {
		n += allocsize.Of(cap(l.code) * int(unsafe.Sizeof(instr{})))
	}
	n += allocsize.Of(cap(p.refSets) * int(unsafe.Sizeof([]int(nil))))
	for _, s := range p.refSets {
		n += allocsize.Of(cap(s) * word)
	}
	return n
}

// footprint is what a literal takes: itself and its units.
func (l *literal) footprint() int {
	return allocsize.Of(int(unsafe.Sizeof(*l))) + l.dataFootprint()
}

// dataFootprint is what a literal's units take, as code units and bytes.
func (l *literal) dataFootprint() int {
	return allocsize.Of(cap(l.units)*2) + allocsize.Of(len(l.ascii))
}

// footprint is what a class takes: its ranges, unless they are still a
// shared property table's.
func (s *charSet) footprint() int {
	n := allocsize.Of(int(unsafe.Sizeof(*s)))
	if len(s.ranges) == 0 || unsafe.SliceData(s.ranges) != s.table {
		n += allocsize.Of(cap(s.ranges) * int(unsafe.Sizeof(charRange{})))
	}
	return n + s.bmpFootprint()
}

// footprint is what a matcher holds: its stacks, those of its matcher over
// an ASCII subject's bytes, which matches every ASCII string, and the
// backtracking stacks of the native code each ran.
func (m *matcher) footprint() int {
	n := allocsize.Of(int(unsafe.Sizeof(*m))) +
		stacksFootprint(m.caps, m.trail, m.stack, m.counters, m.emptyMarks) +
		m.native.footprint()
	if a := m.bytes; a != nil {
		n += allocsize.Of(int(unsafe.Sizeof(*a))) +
			stacksFootprint(a.caps, a.trail, a.stack, a.counters, a.emptyMarks) +
			a.native.footprint()
	}
	return n
}

// stacksFootprint is what a matcher's captures, trail, stack, counters and
// marks take.
func stacksFootprint(caps []int, trail []capWrite, stack []frame, counters, marks []int) int {
	const word = int(unsafe.Sizeof(0))
	return allocsize.Of(cap(caps)*word) +
		allocsize.Of(cap(trail)*int(unsafe.Sizeof(capWrite{}))) +
		allocsize.Of(cap(stack)*int(unsafe.Sizeof(frame{}))) +
		allocsize.Of(cap(counters)*word) +
		allocsize.Of(cap(marks)*word)
}

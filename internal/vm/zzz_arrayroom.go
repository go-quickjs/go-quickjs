package vm

import "weak"

// An empty array literal learns the length its arrays grow to, so repeated
// results need not allocate every buffer on the way there.
//
// How room is learned. Each time a site runs, it looks at the array it made
// the time before, by then as long as it is going to get if it was a result,
// and the room is the smaller of that length and the one before it. Two
// samples must agree, so one long result among short ones sizes nothing: an
// occasional large array must not size every small one, and what room a hint
// gives is memory a short result would waste. Nothing below arrayRoomLeast
// is worth learning, and an array past arrayRoomLimit counts as no length at
// all, which is ordinary growth: a hint it had outgrown would only change the
// capacities it passes through. A short result drops the room at once and
// pauses sampling for 31 executions, which keeps the common small literal off
// this path; a probe that grows resumes samples in a row.
//
// What it costs to hold. A site's sample is a whole result, so it is strong
// only until the next literal or until a collection has run
// (arraySamplesLetGo), and weak after that: a long render can still teach its
// final length without keeping its result alive. At a turn's end only the
// counts remain, and acrossTurns, which is what native code asks of a site
// before it takes its room (jitLiteralArraySource). Sampling starts on the
// second execution: an array made by code loaded only once must not keep that
// load's object graph until a sweep.
//
// Where it lives. A site's state is on the closureCode: a template's closures
// in one realm of one runtime. Nothing is on the bytecode.Function, which
// runtimes share, and a metered runtime makes its arrays as ever, since room
// is memory it has not been told of.

const (
	// arrayRoomLeast is the shortest length a site learns.
	arrayRoomLeast = 256
	// arrayRoomLimit is the longest.
	arrayRoomLimit = 65536
	// arrayRoomPause is how many executions a short result skips.
	arrayRoomPause = 31
)

// arrayRoom is an empty literal's site.
type arrayRoom struct {
	// sample is the array the site last made, while it is held strongly,
	// and weak what is left of it after a collection: at most one is set.
	sample *Object
	weak   weak.Pointer[Object]
	// room is the capacity the site's next array is made with, 0 for
	// ordinary growth; previous is the last length seen.
	room, previous int
	// listed says the site is in Runtime.arraySamples, once at most.
	listed bool
	// wait is how many executions are left to skip before the next sample.
	wait uint8
	// acrossTurns says room was confirmed at a turn's end: what a result
	// came to, not what a callback's growing array had reached.
	acrossTurns bool
	// pc is the literal's instruction, which names the site to native code:
	// the tree's ordinal does not, and an inlined callee has its own.
	pc int32
}

// arrayObserve notes a's length as s's latest sample.
func arrayObserve[_ any](s *arrayRoom, a *Object) {
	n := len(a.elemsOf())
	if n > arrayRoomLimit {
		n = 0
	}
	if n < arrayRoomLeast {
		s.room, s.previous, s.wait = 0, 0, arrayRoomPause
		s.acrossTurns = false
		return
	}
	s.room = min(n, s.previous)
	s.previous = n
}

// arraySamplesLetGo is clearReturnedFrames' release of what a turn kept: the
// last for...in's cursor, and the sites' samples. In a turn, which is a
// collection having run, a sample goes weak; at a turn's end it is read once
// more, as the result it now is, and let go.
//
//go:noinline
func arraySamplesLetGo[_ any](r *Runtime) {
	forInLetGo[struct{}](r)
	keep := r.arraySamples[:0]
	for _, s := range r.arraySamples {
		if r.frameDepth != 0 {
			if s.sample != nil {
				// A sample may disappear before the next literal. Give
				// unused room back now, even if its final length is lost.
				if n := len(s.sample.elemsOf()); n < s.room {
					s.room = n
					if n < arrayRoomLeast {
						s.room = 0
					}
				}
				s.weak = weak.Make(s.sample)
				s.sample = nil
			}
			if s.weak.Value() != nil {
				keep = append(keep, s)
				continue
			}
		} else {
			a := s.sample
			if a == nil {
				a = s.weak.Value()
			}
			if a != nil {
				arrayObserve[struct{}](s, a)
				s.acrossTurns = s.room >= arrayRoomLeast
			}
		}
		s.sample, s.weak, s.listed = nil, weak.Pointer[Object]{}, false
	}
	clear(r.arraySamples[len(keep):])
	r.arraySamples = keep
}

// arrayEmptyOp builds new_array of no elements at pc, or reports that it
// did not, for op's node.
func arrayEmptyOp[_ any](b *tbuilder, pc int) bool {
	if !b.spill() {
		return false
	}
	n := &arrayEmptyNode[struct{}]{site: b.arraySites, pc: int32(pc)}
	b.arraySites++
	b.push(n.make)
	return true
}

// arrayEmptyNode is an empty literal in the tree tier: its site's index in
// closureCode.arrays, the build's count of them, and its instruction.
type arrayEmptyNode[_ any] struct {
	site int
	pc   int32
}

// arraySiteNew makes cl's site, on the first execution of its literal, the
// one that takes no sample.
func arraySiteNew[_ any](cl *closureCode, site int, pc int32) *arrayRoom {
	for len(cl.arrays) <= site {
		cl.arrays = append(cl.arrays, nil)
	}
	s := &arrayRoom{wait: 1, pc: pc}
	cl.arrays[site] = s
	return s
}

// make is the node: the array, with the site's room, and the sample taken
// and the last one looked at where the site is sampling.
func (n *arrayEmptyNode[_]) make(c *tctx) Value {
	if c.r.meter != nil {
		return Obj(c.r.newArrayFrom(nil))
	}
	var s *arrayRoom
	if n.site < len(c.arrays) {
		s = c.arrays[n.site]
	}
	if s == nil {
		s = arraySiteNew[struct{}](c.closureCode, n.site, n.pc)
	}
	track := s.wait == 0
	if track {
		previous := s.sample
		if previous == nil {
			previous = s.weak.Value()
			s.weak = weak.Pointer[Object]{}
		}
		if previous != nil {
			arrayObserve[struct{}](s, previous)
			if s.wait != 0 {
				s.sample = nil
				track = false
			}
		}
	} else {
		s.wait--
	}
	var a *Object
	if s.room == 0 {
		// newArrayObject of no room, for what it would ask of n.
		ao := new(arrayObject)
		ao.Object = Object{shape: rootFor(ClassArray, c.r.proto.array), class: ClassArray,
			flags: objExtensible | objArrayLengthWritable | objHasExt}
		ao.side.elems = ao.inline[:0]
		a = &ao.Object
	} else {
		a = newArrayObject(c.r.proto.array, s.room)
		a.extOf().elems = a.elemsOf()[:0]
	}
	if track {
		if !s.listed {
			c.r.arraySamples = append(c.r.arraySamples, s)
			s.listed = true
		}
		s.sample = a
	}
	return Obj(a)
}

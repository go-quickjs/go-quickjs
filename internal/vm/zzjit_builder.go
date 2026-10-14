//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "unsafe"

// A loop's string builder (internal/jit/ssa's stringBuilders): s += x, at
// each iteration, made by appends to one string (ssa.GoAppend) where
// nothing but those appends sees it in the loop, rather than a string an
// iteration. The string is the builder's while it is building
// (String.building): its tail, a flat string over a buffer with room,
// grows in place, and its length and ends with it. Nothing else holds it
// then -- the loop's code uses it only for its appends, and freezes it
// where it uses it after (ssa.OpFreeze) -- but native code itself, which
// may leave for Go at any exit with it in a slot, where Go freezes every
// builder (jitFreezeBuilders). An append to a string that is not building
// makes a builder of it and what is appended, the string its left side,
// copied nowhere.

// jitBuilder is a building string, s, and its tail: s itself, or s's right
// side, the flat string over buf that appends grow.
type jitBuilder struct {
	s, tail *String
	buf     []byte
}

// jitAppend is ssa.GoAppend's answer for s += x: s grown in place, if it is
// a builder, or a builder of the two; or false for what Go makes after an
// exit -- a string past the length limit, or a lone high surrogate at the
// join meeting a low one, which a join makes one code point of.
func (r *Runtime) jitAppend(s, x *String) (*String, bool) {
	if x.length == 0 {
		return s, true
	}
	if s.length+x.length > maxStringLength-64 {
		return nil, false
	}
	j := r.jit
	if s.building {
		for i := len(j.builders) - 1; i >= 0; i-- {
			if b := &j.builders[i]; b.s == s {
				if b.tail.endsHigh && x.startsLow {
					return nil, false
				}
				b.buf = append(b.buf, x.Go()...)
				t := b.tail
				t.s = unsafe.String(unsafe.SliceData(b.buf), len(b.buf))
				t.length += x.length
				t.ascii = t.ascii && x.ascii
				t.endsHigh = x.endsHigh
				if s != t {
					s.length += x.length
					s.ascii = s.ascii && x.ascii
					s.endsHigh = x.endsHigh
				}
				j.appended++
				return s, true
			}
		}
		// Not one of this runtime's builders: as if frozen.
		s.building = false
	}
	if s.endsHigh && x.startsLow {
		return nil, false
	}
	xs := x.Go()
	buf := make([]byte, len(xs), max(32, 2*len(xs)))
	copy(buf, xs)
	t := &String{s: unsafe.String(unsafe.SliceData(buf), len(buf)), length: x.length, ascii: x.ascii, endsHigh: x.endsHigh, startsLow: x.startsLow}
	b := t
	if s.length != 0 {
		b = &String{left: s, aux: unsafe.Pointer(t), length: s.length + x.length, ascii: s.ascii && x.ascii,
			endsHigh: x.endsHigh, startsLow: s.startsLow}
	}
	b.building = true
	if len(j.builders) >= 64 {
		// The ones native code has frozen since Go last froze them all.
		kept := j.builders[:0]
		for _, x := range j.builders {
			if x.s.building {
				kept = append(kept, x)
			}
		}
		clear(j.builders[len(kept):])
		j.builders = kept
	}
	j.builders = append(j.builders, jitBuilder{s: b, tail: t, buf: buf})
	return b, true
}

// jitFreezeBuilders freezes every building string: native code has left
// for Go, which may hold any of them now, in a frame.
func (r *Runtime) jitFreezeBuilders() {
	j := r.jit
	for i := range j.builders {
		j.builders[i].s.building = false
	}
	clear(j.builders)
	j.builders = j.builders[:0]
}

package vm

import "unsafe"

// A binding's appends to a string it alone holds: the compiler's ownedAppend
// and localAppend say which additions those are, and what makes them safe.
//
// The string in the binding is an ordinary one at every moment, a rope that
// anything may read, so a tier that knows nothing of this is right: only the
// additions the compiler marked, in the tree tier and native code, come here,
// and the interpreter's are plain additions.
//
// What is claimed. String.building says that nothing but the binding holds
// the string, and strAppend sets it only on a rope it has just made and is
// about to hand the binding, so there is one holder at the moment it is set.
// From then on the only ways to reach the string are a read of the binding,
// which the compiler follows by a str_freeze (strFreeze) that clears building
// before anything else can use what was read, and the old value an addition
// has on the stack, which is its left operand and nobody else's. So a string
// with building set has one holder, and that holder is about to replace it:
// that string, and the strTail its right side is, may be grown in place.
// Building is never set again on a string that has lost it, so a tail is
// claimed once: a read ends it for good, and the append after the read makes
// a new rope over what was read, which the reader's copy cannot see past.
//
// A rope's left side may keep its building, as nothing clears it there: it is
// reachable only through the rope, which never hands it out as a value, and
// no append takes anything but what is in the binding.
//
// An append makes the rope any addition would, and marks it: x += y and
// return x cost what they did. The strAppendRopes-th append in a row finds its
// left side marked, so no one else can have it, and gives it a right side with
// room, a strTail, which that append and the ones after it grow where it is,
// in bytes, with no node and nothing for the collector to walk. A read clears
// the mark, and the append after it starts again with a rope over what was
// read: appends with a read every few cost a rope each, as they did. A tail
// costs what about three ropes cost, so a run has paid that much in ropes
// before it buys one: bought at the second append, x += a; x += b; read x
// took half as long again, for a tail made and thrown away each time.
//
// A string past the longest, a meter's count, a surrogate's halves meeting
// across the join and an empty operand are concat's, as ever: all of them
// may give back something not new, which is never marked. A meter counts
// every rope's bytes as they are made, which a tail would hide from it.

// strAppendRopes is how many appends in a row make a rope before the next
// gives the string a tail.
const strAppendRopes = 3

// strTailLongest is the most bytes an append copies into a tail. Past it a
// node is cheap for what it holds, and a rope already made need not be
// written out: chunks of a megabyte stay ropes, where a tail took all of them
// again, with room for four times the last.
const strTailLongest = 1024

// strTail is the right side of an append's string: a flat string over buf,
// which is grown in place. Bytes below len(buf) are never written again, so a
// flat string cut from s stays right however far buf goes on.
type strTail struct {
	String
	buf []byte
}

// strAppend is a + b at an addition the compiler marked, a and b strings, a
// what the binding held. It gives back what the binding is to hold, a itself
// where it grew.
func strAppend[_ any](r *Runtime, a, b *String) (*String, error) {
	if a.length == 0 || b.length == 0 || a.endsHigh && b.startsLow || r.meter != nil ||
		a.length+b.length > maxStringLength {
		strFreeze[struct{}](Str(a))
		return r.concat(a, b)
	}
	// A rope, unless a has a tail or has earned one and b is short enough
	// for it. Not building: a was read since it was made, or never was the
	// binding's alone. Flat: there is no right side to grow, as in what
	// native code's builder makes (jitAppend), which building alone does not
	// tell from a rope.
	long := b.left != nil || len(b.s) > strTailLongest
	if long || !a.building || a.left == nil || !a.tailed && a.ropes < strAppendRopes-1 {
		// A short string is copied, and has nothing to grow.
		if a.byteLenShallow()+b.byteLenShallow() < ropeThreshold {
			return a.Concat(b), nil
		}
		// Concat's rope, made here for what the checks above have said of
		// the operands: a read after each append spends its time on this
		// path, and a call of Concat asks it all again. A long b starts the
		// count again, so that what a tail starts with is never long.
		s := &String{length: a.length + b.length, ascii: a.ascii && b.ascii,
			endsHigh: b.endsHigh, startsLow: a.startsLow, building: true}
		if a.building && !long {
			s.ropes = a.ropes + 1
		}
		s.left, s.aux = a, unsafe.Pointer(b)
		return s, nil
	}
	if !a.tailed {
		strTailed(a)
	}
	t := (*strTail)(a.aux)
	end := len(t.buf) + len(b.s)
	if end <= cap(t.buf) {
		start := len(t.buf)
		t.buf = t.buf[:end]
		copy(t.buf[start:], b.s)
		// Its pointer stays the same: only the two lengths change. Reading
		// the string's own pointer lets Go omit the redundant write barrier.
		t.s = unsafe.String(unsafe.StringData(t.s), end)
	} else {
		strTailGrow[struct{}](t, b.s)
	}
	t.length += b.length
	// Only a surrogate's join reads a right side's flags, and its pair is no
	// ASCII: ascii here is for what reads a piece by itself.
	t.ascii = t.ascii && b.ascii
	t.endsHigh = b.endsHigh
	a.length += b.length
	a.ascii = a.ascii && b.ascii
	a.endsHigh = b.endsHigh
	return a, nil
}

// strTailGrow appends text to a tail that needs room. Large tails double
// rather than grow by a quarter; short tails keep Go's growth, where the
// spare room would often go unused. A doubling past an int on a 32-bit
// platform comes out negative, and is what the text needs instead.
//
//go:noinline
func strTailGrow[_ any](t *strTail, text string) {
	if cap(t.buf) >= 16384 {
		b := make([]byte, len(t.buf), max(2*cap(t.buf), len(t.buf)+len(text)))
		copy(b, t.buf)
		t.buf = b
	}
	t.buf = append(t.buf, text...)
	t.s = unsafe.String(unsafe.SliceData(t.buf), len(t.buf))
}

// strFreezeNode is str_freeze in the tree tier: strFreeze in place, as this
// follows a read of a binding, which a call would cost more than it does.
func strFreezeNode[_ any](v tval) tval {
	return func(c *tctx) Value {
		x := v(c)
		if x.IsString() {
			if s := x.String(); s.building {
				s.building = false
			}
		}
		return x
	}
}

// strAppendNode is an add marked bytecode.AddOwned in the tree tier, x its
// local's value.
func strAppendNode[_ any](x, y tval, pc int) tval {
	return func(c *tctx) Value {
		l := x(c)
		r := y(c)
		c.at(pc)
		v, err := strAppendOp[struct{}](c.r, l, r)
		if err != nil {
			c.throw(err)
		}
		return v
	}
}

// strAppendOp is an add marked bytecode.AddOwned: a + b, a a local's value. A
// number is made a string here, so that it costs a run of appends nothing; an
// object's conversion, which runs code, is add's, with what add does for it.
//
//go:noinline
func strAppendOp[_ any](r *Runtime, a, b Value) (Value, error) {
	if a.IsString() {
		s := b
		if b.IsNumber() {
			t, err := r.toString(b)
			if err != nil {
				return Undefined, err
			}
			s = Str(t)
		}
		if s.IsString() {
			t, err := strAppend[struct{}](r, a.String(), s.String())
			if err != nil {
				return Undefined, err
			}
			return Str(t), nil
		}
	}
	if a.IsNumber() && b.IsNumber() {
		return Float(a.num + b.num), nil
	}
	return r.add(a, b)
}

// strTailed gives a, a rope, a right side with room, holding what it had,
// which is short, as a's count of appends says (strAppend). Whoever else
// holds what the right side was keeps it: it is copied, not grown.
//
//go:noinline
func strTailed(a *String) {
	right := a.rightSide()
	s := right.Go()
	t := &strTail{buf: make([]byte, len(s), max(64, 4*len(s)))}
	copy(t.buf, s)
	t.s = unsafe.String(unsafe.SliceData(t.buf), len(t.buf))
	t.length, t.ascii, t.endsHigh, t.startsLow = right.length, right.ascii, right.endsHigh, right.startsLow
	a.aux, a.tailed = unsafe.Pointer(&t.String), true
}

// strFreeze is str_freeze: v, if it is a string an append owns, is not any
// more. It writes only such a string: another may be one every runtime
// reads, as the empty string is, and a store would be a data race and bounce
// its cache line between them.
//
//go:noinline
func strFreeze[_ any](v Value) {
	if v.IsString() {
		if s := v.String(); s.building {
			s.building = false
		}
	}
}

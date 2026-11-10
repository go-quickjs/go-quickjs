package vm

import (
	"hash/maphash"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jsnum"
)

// concatMemo holds short strings that joins made, by their text, so that a
// join making the same text again returns the string it made before rather
// than a new one. A program that builds one string from the same parts many
// times -- 'String for key ' + tag + ' in leaf node' for every leaf of a
// tree, a markup's class names, a key made of a prefix and a small number --
// then allocates one string, where it allocated one each time, and keeps one
// alive where it kept many.
//
// It is safe because a string is immutable and its identity is not
// observable: a join returns a string equal to the one it would have made,
// and nothing can tell the two apart. Only flat strings no append can grow
// are kept: joinMemoShort's, each in an allocation of its own.
//
// The memo is a direct-mapped table: a join's text, already put together on
// the stack, is hashed to one slot, and a string there with the same text is
// the answer; otherwise the new string takes the slot. What it keeps alive
// is at most its slots' strings of at most 72 bytes each, about 32 KB.
//
// A program whose joins never repeat would pay a hash and a store on every
// one for nothing, so the memo judges itself: of every concatMemoJudged
// lookups, fewer than one in eight finding their string turns it off for
// concatMemoOff joins, its strings dropped.
type concatMemo struct {
	slots [concatMemoSlots]*String
	// seen and hits count the lookups since the last judgement and those
	// that found their string.
	seen, hits uint32
	// off is how many joins are still to be made without the memo.
	off uint32
}

const (
	concatMemoSlots  = 256
	concatMemoJudged = 4096
	concatMemoOff    = 65536
)

// joinMemoized is joinValues for joinParts, by the memo where it is on:
// joinValues itself, and joinShort, are as they were without it, so that a
// runtime whose memo is off, or under a memory limit, whose joins are
// charged as made, runs the code it ran before the memo.
func (r *Runtime) joinMemoized(parts []Value) *String {
	m := r.concatMemo
	if r.meter != nil {
		return joinValues(parts)
	}
	if m == nil {
		m = new(concatMemo)
		r.concatMemo = m
	} else if m.off != 0 {
		m.off--
		return joinValues(parts)
	}
	// What joinValues measures: a join of more than 72 bytes, or one that
	// pairs a surrogate's halves, is joinValues's.
	size, prevHigh := 0, false
	for _, p := range parts {
		if !p.IsString() {
			size += 24
			prevHigh = false
			continue
		}
		s := p.String()
		if prevHigh && s.startsLow {
			return joinValues(parts)
		}
		size += s.byteLenShallow()
		prevHigh = s.endsHigh
	}
	if size > 72 || len(parts) == 0 {
		return joinValues(parts)
	}
	return joinMemoShort(parts, m)
}

// joinMemoShort is joinShort by the memo: the text put together on the
// stack is looked up, and a string with the same text and flags is the
// answer; otherwise the new string is kept.
func joinMemoShort(parts []Value, m *concatMemo) *String {
	var buf [72]byte
	b := buf[:0]
	units, ascii := 0, true
	for _, p := range parts {
		if !p.IsString() {
			n := len(b)
			b = jsnum.AppendFloat(b, p.Number())
			units += len(b) - n
			continue
		}
		s := p.String()
		b = append(b, s.Go()...)
		units += s.length
		ascii = ascii && s.ascii
	}
	if units == 0 {
		return emptyString
	}
	startsLow, endsHigh := false, false
	if first := parts[0]; first.IsString() {
		startsLow = first.String().startsLow
	}
	if last := parts[len(parts)-1]; last.IsString() {
		endsHigh = last.String().endsHigh
	}
	s, at := m.find(b)
	if s != nil && s.length == units && s.ascii == ascii && s.startsLow == startsLow && s.endsHigh == endsHigh {
		return s
	}
	var out *String
	if len(b) <= 72 {
		var room []byte
		out, room = newStringBytes(len(b))
		copy(room, b)
		out.s = unsafe.String(unsafe.SliceData(room), len(b))
	} else {
		// The measure is an upper bound, as joinShort's is.
		out = &String{s: string(b)}
	}
	out.length, out.ascii = units, ascii
	out.startsLow, out.endsHigh = startsLow, endsHigh
	m.keep(at, out)
	return out
}

// find returns the string with the text b if the memo holds it, and the slot
// b's text belongs in either way.
func (m *concatMemo) find(b []byte) (*String, uint32) {
	// maphash.String, which the Map's keys already use: Bytes would be one
	// more function linked ahead of the packages vm's layout lock does not
	// pin, and moved the regular expression matcher (regexp_replace_dollar
	// 5% slower at every placement).
	at := uint32(maphash.String(mapSeed, unsafe.String(unsafe.SliceData(b), len(b)))) & (concatMemoSlots - 1)
	m.seen++
	if s := m.slots[at]; s != nil && s.s == string(b) {
		m.hits++
		return s, at
	}
	if m.seen >= concatMemoJudged {
		if m.hits < m.seen/8 {
			m.off = concatMemoOff
			clear(m.slots[:])
		}
		m.seen, m.hits = 0, 0
	}
	return nil, at
}

// keep puts s, a join's new string, in the slot find gave, unless the
// lookup turned the memo off.
func (m *concatMemo) keep(at uint32, s *String) {
	if m.off == 0 {
		m.slots[at] = s
	}
}

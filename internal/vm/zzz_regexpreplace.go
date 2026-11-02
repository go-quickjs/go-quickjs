package vm

import (
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/regexp"
)

// A replace by a template without a $, of an ASCII string, made in one
// pass. String.prototype.replace given a RegExp whose replace is the
// built-in's (replaceDirect) went the whole way of RegExp.prototype's
// Symbol.replace for it: the subject and the template converted, the
// flags read again, the matches collected, and the result written through
// a builder that asks, before each piece, whether it has grown too long
// for a string. Where the subject and the template are ASCII and the
// template has no $, the result is the subject's pieces between the
// matches, each match replaced by the template: its length is known once
// the matches are, and it is made in one allocation and written with a
// copy for each piece (replacedFlat), as replacedShort writes one of up to
// 72 bytes -- V8 replaces so, in StringReplaceGlobalRegExpWithString. A
// pattern of nothing but characters is looked for with strings.Index from
// one match to the next (execMatchAllLit), where each had been a match of
// the pattern's, three calls deep. Each replace of a long string by Octane's
// RegExp, /d1/g's in a script of 4 KB with 38 matches, went from 3.8 to
// 2.1 us, and a short one's, the replace micro-benchmark's, from 420 to
// 315 ns.

// replaceFlat is regExpReplace's replace of s by rx, whose flags are fl,
// with the template tv, called directly (replaceDirect), where s and tv are
// ASCII and tv has no $, without a memory limit: lastIndex, the matches,
// the legacy statics and the errors regExpReplace makes, and its result.
func replaceFlat[_ any](r *Runtime, rx Value, s, tv *String, fl regexp.Flags) (Value, error) {
	o := rx.Object()
	d := o.dataOf().(*regexpData)
	re := d.re
	room := r.takeSpans()
	spans := room
	var err error
	if fl&regexp.FlagGlobal != 0 {
		if err = r.setLastIndex(o, Int(0)); err == nil {
			if lit, ok := re.LiteralASCII(); ok {
				spans, err = execMatchAllLit[struct{}](r, o, d, s, lit, spans)
			} else {
				spans, err = r.execMatchAll(rx, s, fl&(regexp.FlagUnicode|regexp.FlagUnicodeSets) != 0, spans)
			}
		}
	} else if err = r.tick(); err == nil {
		var caps []int
		if caps, err = r.execMatch(rx, s); err == nil {
			spans = append(spans, caps...)
		}
	}
	v := Undefined
	if err == nil {
		width := 2 * (re.GroupCount() + 1)
		v, err = replacedFlat[struct{}](r, s, tv, spans, width, len(spans)/width)
	}
	r.giveSpans(spans)
	return v, err
}

// replacedFlat is the result of replacing count matches of an ASCII
// subject s, spans of width each, by an ASCII template without a $, tv, as
// regExpReplace makes it: the subject itself where nothing matched, and
// otherwise the pieces between the matches and the template for each. The
// replace's builder asked, before it wrote each match's, whether what it had
// written was longer than a string may be, and threw where it was.
func replacedFlat[_ any](r *Runtime, s, tv *String, spans []int, width, count int) (Value, error) {
	if count == 0 {
		return Str(s), nil
	}
	n := s.length + count*tv.length
	for k := 0; k < count; k++ {
		n -= spans[k*width+1] - spans[k*width]
	}
	if n > maxStringLength {
		written, next := 0, 0
		for k := 0; k < count; k++ {
			if written > maxStringLength {
				return Undefined, r.throwStringLength()
			}
			written += spans[k*width] - next + tv.length
			next = spans[k*width+1]
		}
	}
	if n == 0 {
		return Str(emptyString), nil
	}
	src, t := s.Go(), tv.Go()
	if n <= 80 {
		// The String and its bytes in one allocation (newStringBytes).
		out, b := newFlatBytes(n)
		w, next := 0, 0
		for k := 0; k < count; k++ {
			w += copy(b[w:], src[next:spans[k*width]])
			w += copy(b[w:], t)
			next = spans[k*width+1]
		}
		copy(b[w:], src[next:])
		out.setBytes(b)
		out.length, out.ascii = n, true
		return Str(out), nil
	}
	// A builder's room, which, written in full, is not cleared first.
	var b strings.Builder
	b.Grow(n)
	next := 0
	for k := 0; k < count; k++ {
		b.WriteString(src[next:spans[k*width]])
		b.WriteString(t)
		next = spans[k*width+1]
	}
	b.WriteString(src[next:])
	return Str(&String{s: b.String(), length: n, ascii: true}), nil
}

// execMatchAllLit is execMatchAll's matches of an ASCII subject s by o, d's
// RegExp, global, a pattern of nothing but the characters lit, all ASCII
// (regexp's LiteralASCII), each looked for with strings.Index from where
// the last ended: the frame of exec, the tick before each search after a
// match, lastIndex and the legacy statics as execMatchAll makes them. A
// literal's search spends no step of the matcher's budget and asks no
// check.
func execMatchAllLit[_ any](r *Runtime, o *Object, d *regexpData, s *String, lit string, spans []int) ([]int, error) {
	src := s.Go()
	i := len(r.argStack)
	r.argStack = append(r.argStack, Str(s))
	err := r.pushNativeFrame(r.regexpExecFn, Obj(o), r.argStack[i:i+1:i+1], Undefined)
	if err == nil {
		first := len(spans)
		pos := 0
		for {
			if len(spans) > first {
				// Each search after the first is a turn of the caller's
				// loop, which counts it.
				if err = r.tick(); err != nil {
					break
				}
			}
			at := -1
			if pos <= len(src)-len(lit) {
				if src[pos:pos+len(lit)] == lit {
					at = pos
				} else if k := strings.Index(src[pos+1:], lit); k >= 0 {
					at = pos + 1 + k
				}
			}
			if at < 0 {
				// The search that found nothing left lastIndex at 0.
				pos = 0
				break
			}
			pos = at + len(lit)
			spans = append(spans, at, pos)
		}
		if serr := r.setLastIndex(o, Int(pos)); serr != nil {
			err = serr
		} else if len(spans) > first {
			r.recordLegacyMatch(d.legacy(), s, spans[len(spans)-2:])
		}
		r.frameDepth--
	}
	r.argStack[i] = Undefined
	r.argStack = r.argStack[:i]
	return spans, err
}

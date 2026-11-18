package vm

import (
	"errors"
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
// global pattern's matches are collected with the matcher borrowed once
// for them all (execMatchAllASCII), and one of nothing but characters is
// looked for with strings.Index from one match to the next, where each had
// been a match of the pattern's, three calls deep. Each replace of a long string by Octane's
// RegExp, /d1/g's in a script of 4 KB with 38 matches, went from 3.8 to
// 2.1 us, and a short one's, the replace micro-benchmark's, from 420 to
// 315 ns.

// replaceFlat is regExpReplace's replace of s by rx, whose flags are fl,
// with the template tv, called directly (replaceDirect), where s and tv are
// ASCII and tv has no $, without a memory limit: lastIndex, the matches,
// the legacy statics and the errors regExpReplace makes, and its result.
func replaceFlat[_ any](r *Runtime, rx Value, s, tv *String, fl regexp.Flags) (Value, error) {
	o := rx.Object()
	re := o.dataOf().(*regexpData).re
	spans := r.takeSpans()
	var err error
	if fl&regexp.FlagGlobal != 0 {
		if err = r.setLastIndex(o, Int(0)); err == nil {
			spans, err = execMatchAllASCII[struct{}](r, rx, s, spans)
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
// otherwise the pieces between the matches and the template for each. One
// longer than a string may be is a RangeError, as V8's is; n is known
// before anything is written, and the template runs no code, so when the
// error is thrown cannot be seen.
func replacedFlat[_ any](r *Runtime, s, tv *String, spans []int, width, count int) (Value, error) {
	if count == 0 {
		return Str(s), nil
	}
	// In int64: count times tv's length is past an int of 32 bits where
	// either is near a string's longest.
	n64 := int64(s.length) + int64(count)*int64(tv.length)
	for k := 0; k < count; k++ {
		n64 -= int64(spans[k*width+1] - spans[k*width])
	}
	if n64 > maxStringLength {
		return Undefined, r.throwStringLength()
	}
	n := int(n64)
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

// execMatchAllASCII is execMatchAll for an ASCII subject s: its matches by
// rx, global, collected by regexp's MatchAllASCIIInto, which borrows the
// matcher once for them all and searches for a pattern of nothing but
// characters or of one class where it is, rather than three calls deep for
// each -- in exec's frame, ticking before each match after the first, the
// step budget's error a SyntaxError, and lastIndex and the legacy statics
// written as execMatchAll writes them.
func execMatchAllASCII[_ any](r *Runtime, rx Value, s *String, spans []int) ([]int, error) {
	o := rx.Object()
	d := o.dataOf().(*regexpData)
	re := d.re
	i := len(r.argStack)
	r.argStack = append(r.argStack, Str(s))
	err := r.pushNativeFrame(r.regexpExecFn, rx, r.argStack[i:i+1:i+1], Undefined)
	if err == nil {
		var check func() error
		if r.abort != nil || r.ctx != nil && r.ctx.Done() != nil {
			check = r.checkInterruptNow
		}
		first := len(spans)
		var next int
		spans, next, err = regexp.MatchAllASCIIInto[struct{}](re, spans, s.Go(), check, r.tick)
		if errors.Is(err, regexp.ErrComplexity) {
			err = r.throwError(errSyntax, "%s", err.Error())
		}
		if next < 0 {
			// The match that found nothing left lastIndex at 0.
			next = 0
		}
		if serr := r.setLastIndex(o, Int(next)); serr != nil {
			err = serr
		} else if len(spans) > first {
			width := 2 * (re.GroupCount() + 1)
			r.recordLegacyMatch(d.legacy(), s, spans[len(spans)-width:])
		}
		r.frameDepth--
	}
	r.argStack[i] = Undefined
	r.argStack = r.argStack[:i]
	return spans, err
}

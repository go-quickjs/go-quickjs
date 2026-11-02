//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"strings"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/regexp"
)

// Native code's call of String.prototype.replace given a RegExp that is no
// literal's and a string, s.replace(re, ""), is a leaf's call (ssa.GoLeaf),
// made without a frame by the VM's routine for leaves, as exec's and
// test's are (zzjit_regexpleaf.go): learned at the call of Go that makes
// it, once a site (jitReplaceLeafSeen), and compiled next a leaf's call of
// Go of its own -- or from the first compile, where the RegExp is a
// global's or a captured binding's and the template a string constant
// (jitReplaceLeafGuess): Octane's runBlocks, each run a few times, had
// made most of their replaces before the compile that learned them. It had been a call of Go each time -- jitGoCall, its
// frame and its tick, jitRegExpLitPool's question of whether a literal's
// RegExp was given, the frame of exec that the matches were collected in
// -- about 8% of Octane's RegExp with the JIT, more than half the work of a
// replace of a short string.
//
// The leaf (jitReplaceLeaf) is the built-in's direct replace
// (replaceDirect), of a string by a RegExp whose flags, exec and
// Symbol.replace are the built-ins, its lastIndex writable -- which runs
// nothing of the script's -- by a string, a template with or without a $:
// a function, which is called, never is. Its check passes where that
// replace would throw nothing: the string it makes is shorter than a
// string's limit -- each match's replacement at most the template, each $
// in it the subject, as regexpReplaceDead bounds it -- and lastIndex,
// which a match that is not global reads, is a number, which ToLength
// converts calling nothing. It makes the matches, lastIndex and the legacy
// statics as the replace does, its matches collected without exec's frame
// and without making an error: where a match fails -- runs out of its
// step budget, or meets the check's interrupt -- or a tick between matches
// finds the runtime stopped, the leaf errs with nothing made
// (errRegExpLeafAgain), having written at most lastIndex 0, which a global
// replace writes first, and the call is made again with a frame, which
// throws. A match past the step budget is then made twice, once each way.
// Where nothing reads the result but, at most, whether it is truthy
// (abi.GoCallState's Unused), the leaf makes no string: one as truthy as
// it would have been stands for it (deadReplaced).
//
// A RegExp literal's replace stays a call of Go, which gives the literal's
// pool its RegExp back (jitRegExpLitPool).
//
// The functions are generic, each instantiated with struct{} alone, and
// the leaf's check and function are methods of a generic type.

// jitReplaceLeaf is String.prototype.replace's leaf for native code: dead
// says nothing reads the call's result but, at most, whether it is truthy.
type jitReplaceLeaf[_ any] struct {
	dead bool
}

// jitReplaceLeafs are the realm-independent leaves: a call's result read,
// and one whose result nothing reads.
var jitReplaceLeafs = [2]jitReplaceLeaf[struct{}]{{}, {dead: true}}

// check is the leaf's check: this a string, the arguments a RegExp the
// built-in replaces directly (replaceDirect) -- its lastIndex a number
// where the RegExp is not global -- and a string whose replacements cannot
// make a string too long to be one.
func (l *jitReplaceLeaf[_]) check(r *Runtime, this Value, args []Value) bool {
	if !this.IsString() || len(args) < 2 || !args[0].IsObject() || !args[1].IsString() {
		return false
	}
	fl, ok := r.replaceDirect(args[0])
	if !ok || fl&regexp.FlagGlobal == 0 && !args[0].Object().vals[0].IsNumber() {
		return false
	}
	n, tv := int64(this.String().length), args[1].String()
	per := int64(tv.length)
	if per > 0 {
		per += int64(strings.Count(tv.Go(), "$")) * n
	}
	return per <= maxStringLength && n+(n+1)*per <= maxStringLength
}

// native is the leaf's function, its check passed: the replace of this by
// args[0] with the template args[1], as regExpReplace makes it, without a
// frame, erring with nothing made where a match fails (errRegExpLeafAgain).
func (l *jitReplaceLeaf[_]) native(r *Runtime, this Value, args []Value) (Value, error) {
	s, rx, tv := this.String(), args[0], args[1].String()
	o := rx.Object()
	d := o.dataOf().(*regexpData)
	re := d.re
	var check func() error
	if (r.abort != nil || r.ctx != nil && r.ctx.Done() != nil) && !re.NeverFails(s.ascii) {
		check = r.checkInterruptNow
	}
	spans := r.takeSpans()
	var err error
	if re.Flags()&regexp.FlagGlobal != 0 {
		spans, err = jitReplaceMatches[struct{}](r, o, d, s, spans, check)
	} else {
		spans, err = jitReplaceMatch[struct{}](r, o, d, s, spans, check)
	}
	if err != nil {
		r.giveSpans(spans)
		return Undefined, errRegExpLeafAgain
	}
	width := 2 * (re.GroupCount() + 1)
	var v Value
	switch {
	case l.dead:
		v = deadReplaced[struct{}](r, rx, s, tv, spans, width)
	case s.ascii && tv.ascii && strings.IndexByte(tv.Go(), '$') < 0:
		v, err = replacedFlat[struct{}](r, s, tv, spans, width, len(spans)/width)
	default:
		v, err = jitReplacedTemplate[struct{}](r, re, s, tv, spans, width)
	}
	r.giveSpans(spans)
	return v, err
}

// jitReplaceMatches appends to spans the matches of s by o, global, d its
// data, as execMatchAll collects them for a replace -- lastIndex 0 first,
// each match from where the last ended, past an empty one by a unit, or a
// code point under u or v, a tick before each after the first -- but
// without exec's frame and without making an error: a match's or a tick's
// is given back as it is. lastIndex stays 0, where execMatchAll leaves it,
// and the legacy statics are the last match's.
func jitReplaceMatches[_ any](r *Runtime, o *Object, d *regexpData, s *String, spans []int, check func() error) ([]int, error) {
	re := d.re
	if err := r.setLastIndex(o, Int(0)); err != nil {
		return spans, err
	}
	first := len(spans)
	if s.ascii {
		var err error
		if spans, _, err = regexp.MatchAllASCIIInto[struct{}](re, spans, s.Go(), check, r.tick); err != nil {
			return spans, err
		}
	} else {
		j := subjectOf(s)
		unicode := re.Flags()&(regexp.FlagUnicode|regexp.FlagUnicodeSets) != 0
		for pos := 0; pos <= j.len(); {
			if len(spans) > first {
				if err := r.tick(); err != nil {
					return spans, err
				}
			}
			start := pos
			if unicode && j.inPair(start) {
				start--
			}
			caps, err := j.match(re, r.capsBuf, start, check)
			if caps != nil {
				r.capsBuf = caps
			}
			if err != nil {
				return spans, err
			}
			if caps == nil {
				break
			}
			spans = append(spans, caps...)
			pos = caps[1]
			if caps[0] == caps[1] {
				pos = j.advance(pos, unicode)
			}
		}
	}
	if len(spans) > first {
		width := 2 * (re.GroupCount() + 1)
		r.recordLegacyMatch(d.legacy(), s, spans[len(spans)-width:])
	}
	return spans, nil
}

// jitReplaceMatch appends to spans the match of s by o, not global, d its
// data, as regexpMatch makes it -- from lastIndex, a number, where the
// RegExp is sticky, which it writes back, and otherwise from the start --
// but without making an error: a match's is given back as it is, nothing
// written.
func jitReplaceMatch[_ any](r *Runtime, o *Object, d *regexpData, s *String, spans []int, check func() error) ([]int, error) {
	re := d.re
	li, err := r.toLength(o.vals[0])
	if err != nil {
		return spans, err
	}
	sticky := re.Flags()&regexp.FlagSticky != 0
	start := 0
	if sticky {
		start = int(min(li, maxStringLength+1))
	}
	j := subjectOf(s)
	var caps []int
	if start <= j.len() {
		if re.Flags()&(regexp.FlagUnicode|regexp.FlagUnicodeSets) != 0 && j.inPair(start) {
			start--
		}
		if caps, err = j.match(re, r.legacyRegExp.spare(), start, check); err != nil {
			return spans, err
		}
	}
	if caps == nil {
		if sticky {
			err = r.setLastIndex(o, Int(0))
		}
		return spans, err
	}
	if sticky {
		if err := r.setLastIndex(o, Int(caps[1])); err != nil {
			return spans, err
		}
	}
	r.takeLegacyMatch(d.legacy(), s, caps)
	return append(spans, caps...), nil
}

// jitReplacedTemplate is the result of replacing the matches of s by re,
// spans of width each, by the template tv, as regExpReplaceIn writes it for
// the built-in exec's matches and a template that is no function -- where
// replacedFlat does not: a subject or a template not all ASCII, or a
// template with a $, expanded from its pieces (templatePieces) -- which
// the leaf's check has bounded shorter than a string's limit.
func jitReplacedTemplate[_ any](r *Runtime, re *regexp.Regexp, s, tv *String, spans []int, width int) (Value, error) {
	count := len(spans) / width
	if count == 0 {
		return Str(s), nil
	}
	j := subjectOf(s)
	size := j.len()
	template := tv.Go()
	gl := r.groupsLayoutOf(re)
	var pieces []templatePiece
	var pieceRoom [16]templatePiece
	if strings.IndexByte(template, '$') >= 0 {
		pieces = r.templatePieces(pieceRoom[:0], template, width/2-1, gl)
	}
	sb := r.newParts()
	if err := sb.Grow(len(s.Go())); err != nil {
		return Undefined, err
	}
	next := 0
	for k := 0; k < count; k++ {
		caps := spans[k*width : (k+1)*width]
		if pieces != nil {
			sb.WriteString(j.slice(next, caps[0]))
			writeTemplate(&sb, &j, caps, pieces, template, gl)
			next = caps[1]
			continue
		}
		if position := caps[0]; position >= next {
			if s.ascii {
				if position > next {
					sb.write(j.slice(next, position), false, false)
				}
				sb.WriteStr(tv)
			} else {
				sb.WriteString(j.slice(next, position))
				sb.WriteString(template)
			}
			next = min(caps[1], size)
		}
	}
	if next < size {
		sb.WriteString(j.slice(next, size))
	}
	if s.ascii && tv.ascii {
		t := sb.String()
		return Str(&String{s: t, length: len(t), ascii: true}), nil
	}
	return r.builtString(sb.String())
}

// jitReplaceLeafSeen notes, for e's next compile (jitFeedback.Leaf), the
// call at pc of String.prototype.replace, of argc arguments, whose
// operands are below the VM stack's top, where it is a leaf's: its check
// passes on them.
func jitReplaceLeafSeen[_ any](r *Runtime, e *jitEntry, pc, argc int) {
	sp := r.stackTop
	if r.meter != nil || argc < 2 || sp-argc-2 < 0 || sp > len(r.stack) {
		return
	}
	callee, this := r.stack[sp-argc-1], r.stack[sp-argc-2]
	if !callee.IsObject() || callee.Object() != r.strReplaceFn || callee.Object().fn() == nil {
		return
	}
	if !jitReplaceLeafs[0].check(r, this, r.stack[sp-argc:sp]) {
		return
	}
	for _, list := range [2][]jitInline{e.nativeCalls, e.inlines} {
		for k := range list {
			if int(list[k].pc) == pc && list[k].leaf {
				return
			}
		}
	}
	e.nativeCalls = append(e.nativeCalls, jitInline{pc: int32(pc), obj: r.strReplaceFn, leaf: true})
	e.inlinePending = true
}

// jitReplaceLeafForm is jitLeafFormOf's form of the leaf's call st
// describes, of String.prototype.replace, fd its function's data, its
// operands from first, argc of them: replace's leaf -- the one that makes
// no string where nothing reads the result (abi.GoCallState's Unused) --
// kept in st for the calls after; nil for any other function, which has no
// leaf.
func jitReplaceLeafForm[_ any](r *Runtime, st *abi.GoCallState, fd *funcData, first, argc int, method bool) *jitLeafForm {
	o := (*Object)(*(*unsafe.Pointer)(unsafe.Pointer(&st.Leaf)))
	if fd == nil || o != r.strReplaceFn || !method || argc < 2 {
		return nil
	}
	l := &jitReplaceLeafs[0]
	if st.Unused {
		l = &jitReplaceLeafs[1]
	}
	form := &jitLeafForm{check: l.check, native: l.native, first: first, argc: argc, keep: st.Keep, method: true}
	st.LeafForm = unsafe.Pointer(form)
	return form
}

// jitReplaceLeafGuess is jitRegExpLeafGuess's for the call at pc of cl's
// code of the replace that the get_prop_this at q reads: String.prototype's
// own, where the call's arguments are a read of a global or a captured
// binding (jitChainValue) whose RegExp the leaf's check passes on now, and
// a string constant -- s.replace(re, ""), a module's RegExp in a loop --
// and nil otherwise.
func jitReplaceLeafGuess[_ any](r *Runtime, cl *closure, p *ir.Program, pc, q int) *Object {
	fn := cl.fn
	if fn.Code[pc].A != 2 || q+3 != pc || p.Maps[q+1].Depth != p.Maps[q].Depth+1 || p.Maps[q+2].Depth != p.Maps[q].Depth+2 {
		return nil
	}
	tv := emptyString
	switch t := fn.Code[q+2]; t.Op {
	case bytecode.OpPushEmptyString:
	case bytecode.OpPushConst:
		if int(t.A) >= len(fn.Constants) || fn.Constants[t.A].Kind != bytecode.ConstString {
			return nil
		}
		tv = NewString(fn.Constants[t.A].Str)
	default:
		return nil
	}
	switch fn.Code[q+1].Op {
	case bytecode.OpGetGlobal, bytecode.OpGetUpvalue, bytecode.OpGetUpvalueCheck:
	default:
		return nil
	}
	rx, ok := r.jitChainValue(cl, p, q+1, jitChainReads)
	f := r.strReplaceFn
	if !ok || f == nil || f.fn() == nil || f.fn().native == nil || !rx.IsObject() ||
		!jitReplaceLeafs[0].check(r, Str(emptyString), []Value{rx, Str(tv)}) {
		return nil
	}
	return f
}

package vm

import (
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/regexp"
)

// A call of RegExp.prototype.exec whose result nothing reads -- a
// statement of its own, re.exec(s);, or a test of whether it matched,
// if (re.exec(s)), while (re.exec(s)), !re.exec(s), re.exec(s) && x --
// makes the match, the legacy statics (RegExp.$1, lastMatch...) and
// lastIndex as any other, but no array and no captures' strings, as V8's
// optimized code drops a call's result it finds unused: an object stands
// for the array, the same each time (regexpLeaf's dummy), which is
// truthy as the array is, and of the same kind for whatever native code
// has guessed of the call's result. The V8 suite's RegExp makes 382 of
// its 466 calls of exec so, an array and its strings each time.
//
// So does a string's replace of a RegExp by a string, s.replace(re, ''),
// whose result nothing reads -- 614 of the suite's 712 replaces: where it
// is the built-in's direct replace (replaceDirect), which runs nothing of
// the script's, it makes the matches, the statics and lastIndex as it
// would, and not the string they make (regexpReplaceDead), the subject
// standing for it -- or, where that string is empty, the empty string, or
// where only the subject is, the template. A replacement that could make a
// string too long to be one, which throws, is replaced as before; so is any
// other replace -- a function's, which is called as it was.
//
// Whether a call's result is read the register compiler says
// (bytecode.RMarkDead): the instruction after the call takes it -- a
// drop, a jump on its truth,
// && 's jump_if_false_keep, which keeps only a falsy value, null, or a
// not. The call is made as it was, through callRest, by the same path:
// the leaf's check, the leaf's call without a frame, the frame and the
// tick where it is refused or errs, errors and all. It only tells the
// function, just before, that the call it is about to make is of a
// result nothing reads, of that RegExp -- exec's receiver, replace's
// first argument (regexpLeaf's dead, replaceDead's) -- which the function
// takes as it begins, before anything it runs could call it again. Where
// a memory meter counts what is made, or a debugger may run the script's
// code at the call's tick, the call is told nothing.
//
// The register interpreter makes a call so marked through regDeadCall,
// wherever the method read is exec's or replace's; any other call is made
// as before. Native code tells the functions through its leaf's call's
// description (ssa.CallSite's Unused) or the bytecode after its call of Go
// (jit_regexpdead.go).
//
// The functions are generic, each instantiated with struct{} alone: Go
// lays a generic function's instantiations out after every function and
// closure of the package (docs/optimizing.md, "The layout lock").

var ()

// deadCallRest is callRest's call of o, fd its function's data, on recv
// with args, where nothing reads the result: the function told so first,
// at dead, of mark -- exec's receiver, replace's RegExp -- and untold
// after; told nothing where mark or dead is nil.
func deadCallRest[_ any](r *Runtime, dead **Object, mark, o *Object, fd *funcData, recv Value, args []Value) (Value, error) {
	if dead == nil || mark == nil || r.meter != nil || r.debug != nil {
		return r.callRest(o, fd, recv, args)
	}
	*dead = mark
	v, err := r.callRest(o, fd, recv, args)
	*dead = nil
	return v, err
}

// replaceDead is String.prototype.replace's function wrapped, slow its
// own, and dead the RegExp the call about to be made -- its next, and only
// that one -- is given where nothing reads its result (see the top of this
// file). call takes it as it begins.
type replaceDead[_ any] struct {
	slow NativeFunc
	dead *Object
}

// regexpReplaceDead is regExpReplace's replace of s by rx, whose flags are
// fl, with the template tv, directly (replaceDirect), where nothing reads
// the result but, at most, whether it is truthy: lastIndex, the matches and
// the legacy statics as it makes them, and for the result one as truthy as
// the string it would have made (deadReplaced). It reports false, having
// done nothing, where the replacement could be too long to be a string --
// each match's at most the template, each $ in it the subject -- which
// regExpReplace throws for.
func regexpReplaceDead[_ any](r *Runtime, rx Value, s, tv *String, fl regexp.Flags) (Value, bool, error) {
	n := int64(s.length)
	if per := int64(tv.length) + int64(strings.Count(tv.Go(), "$"))*n; per > maxStringLength || n+(n+1)*per > maxStringLength {
		return Undefined, false, nil
	}
	if fl&regexp.FlagGlobal == 0 {
		// One match, from lastIndex where the RegExp is sticky.
		if err := r.tick(); err != nil {
			return Undefined, true, err
		}
		caps, err := r.execMatch(rx, s)
		if err != nil || caps == nil {
			return Str(s), true, err
		}
		return deadReplaced[struct{}](r, rx, s, tv, caps, len(caps)), true, nil
	}
	if err := r.setLastIndex(rx.Object(), Int(0)); err != nil {
		return Undefined, true, err
	}
	room := r.takeSpans()
	var err error
	if s.ascii {
		room, err = execMatchAllASCII[struct{}](r, rx, s, room)
	} else {
		room, err = r.execMatchAll(rx, s, fl&(regexp.FlagUnicode|regexp.FlagUnicodeSets) != 0, room)
	}
	v := Str(s)
	if err == nil {
		v = deadReplaced[struct{}](r, rx, s, tv, room, 2*(rx.Object().dataOf().(*regexpData).re.GroupCount()+1))
	}
	r.giveSpans(room)
	return v, true, err
}

// deadReplaced stands for the string a replace of s by rx's matches, spans
// of width each, with the template tv would make, where nothing reads it
// but whether it is truthy: the empty string where that is, and otherwise
// one that is not -- the subject, or, where it is empty, the template. The
// string is empty where the matches are all of the subject and each
// replacement is empty: the template is, or each of its pieces (templatePieces)
// is for that match. The subject had stood for every result, and
// if (s.replace(re, "")) taken a replace of all of a string for a string.
func deadReplaced[_ any](r *Runtime, rx Value, s, tv *String, spans []int, width int) Value {
	left := s.length
	for k := 0; k+1 < len(spans); k += width {
		left -= spans[k+1] - spans[k]
	}
	if left > 0 || len(spans) == 0 {
		return Str(s)
	}
	empty := tv.length == 0
	if !empty && strings.IndexByte(tv.Go(), '$') >= 0 {
		re := rx.Object().dataOf().(*regexpData).re
		gl := r.groupsLayoutOf(re)
		var room [16]templatePiece
		pieces := r.templatePieces(room[:0], tv.Go(), width/2-1, gl)
		empty = true
		for k := 0; empty && k+1 < len(spans); k += width {
			empty = templateEmpty[struct{}](pieces, spans[k:k+width], s.length, gl)
		}
	}
	switch {
	case empty:
		return Str(emptyString)
	case s.length > 0:
		return Str(s)
	}
	return Str(tv)
}

// templateEmpty reports whether the replacement writeTemplate writes for
// the match caps of a subject of n code units, by the template pieces were
// read from, is empty.
func templateEmpty[_ any](pieces []templatePiece, caps []int, n int, gl *groupsLayout) bool {
	for _, p := range pieces {
		switch p.kind {
		case pieceText:
			if p.hi > p.lo {
				return false
			}
		case pieceGroup:
			if lo, hi := caps[2*p.lo], caps[2*p.lo+1]; lo >= 0 && hi > lo {
				return false
			}
		case pieceBefore:
			if caps[0] > 0 {
				return false
			}
		case pieceAfter:
			if caps[1] < n {
				return false
			}
		case pieceNamed:
			for _, g := range gl.groups[p.lo] {
				if lo, hi := caps[2*g], caps[2*g+1]; lo >= 0 && hi >= 0 {
					if hi > lo {
						return false
					}
					break
				}
			}
		}
	}
	return true
}

// replaceDeadOf wraps the realm's String.prototype.replace (replaceDead),
// for regexpLeaves.
func replaceDeadOf[_ any](r *Runtime) {
	if r.strReplaceFn == nil {
		return
	}
	if fd := r.strReplaceFn.fn(); fd != nil && fd.native != nil {
		l := &replaceDead[struct{}]{slow: fd.native}
		fd.native = l.call
		r.strReplaceDead = &l.dead
	}
}

// call is replace's function: where the call's result is one nothing
// reads, of a string by the RegExp dead with a string, which stringReplace
// would replace directly (replaceDirect), the replace without its string
// (regexpReplaceDead); anything else replace's own.
func (l *replaceDead[_]) call(r *Runtime, this Value, args []Value) (Value, error) {
	dead := l.dead
	l.dead = nil
	if dead != nil && len(args) >= 2 && this.IsString() && args[1].IsString() && args[0].IsObject() && args[0].Object() == dead && r.meter == nil {
		if fl, ok := r.replaceDirect(args[0]); ok {
			if v, ok, err := regexpReplaceDead[struct{}](r, args[0], this.String(), args[1].String(), fl); ok {
				return v, err
			}
		}
	}
	// A string's replace by a template without a $, both ASCII, made in
	// one pass (replaceFlat).
	if len(args) >= 2 && this.IsString() && args[1].IsString() && args[0].IsObject() && r.meter == nil {
		if s, tv := this.String(), args[1].String(); s.ascii && tv.ascii && strings.IndexByte(tv.Go(), '$') < 0 {
			if fl, ok := r.replaceDirect(args[0]); ok {
				return replaceFlat[struct{}](r, args[0], s, tv, fl)
			}
		}
	}
	return l.slow(r, this, args)
}

package vm

import (
	"errors"
	"sync"

	"github.com/go-quickjs/go-quickjs/internal/regexp"
)

// RegExp.prototype's exec and test as native leaves (leafChecks): a match
// of a primitive, on a RegExp neither global nor sticky, is called without
// a frame by every tier where nothing it does can run the script's code or
// throw -- as V8 runs a RegExp's exec on its fast path, the prototype as it
// was, without a frame of JavaScript's. An escaper's test of its input for
// anything to escape, /["'&<>]/.exec(s), had been an ordinary built-in's
// call: in the tree tier a frame and a tick; in native code a call of Go
// (jitGoCall, jitGoCallNested from a native callee) -- its frame, the
// frames of the levels it is called from, its operands copied and
// cleared, its code pinned -- which made html_escape_plain slower with
// the JIT than without.
//
// The check (regexpLeaf's) passes where the receiver is a RegExp, its
// lastIndex a number, which ToLength converts without calling anything;
// the pattern neither global nor sticky, so that the match leaves lastIndex
// as it is; the argument, if any, a primitive ToString converts without
// calling anything or throwing -- a string, a number, a boolean, undefined
// or null, not a symbol. A match the matcher makes may fail -- run out of
// its step budget, or meet the check's interrupt -- and its error, made
// without a frame, would have run what making one may run
// (Error.stackTraceLimit's getter): a string's match that fails, nothing
// made yet, errs with nothing made (errRegExpLeafAgain), which every tier
// takes for a leaf's call refused and makes again with a frame, which
// throws -- the budget's failure remembered (regexpLeaf's spent), so that
// the match is not made twice. Any other primitive's match is the
// built-in's own, which throws where it fails, and so a leaf's only where
// its pattern's match is a search that never fails (regexp's NeverFails).
// test calls the RegExp's exec, which must be the built-in, read as
// execIsBuiltin reads it. What the match
// does besides -- the result, the legacy statics -- calls nothing, and is
// the same done twice: a pure body applies a leaf wherever its check
// passes, and an evaluation refused after it is made again with a frame
// (pureInvoke), where a global pattern's match, made twice, had begun the
// second where the first had left lastIndex.
//
// The ops are given without setElemOp, which notes a leaf among those
// native code's calls of Go make without a frame before anything else is
// asked of them (leafFns, jitPlainSet): a RegExp literal's call, whose
// RegExp the call of Go gives back to its literal's pool once it returns
// (jitRegExpLitPool), must go on being made there. Native code learns at
// the call of Go that a call of exec or test on any other RegExp is a
// leaf's (jitRegExpLitSite), which its next compile makes a leaf's call of
// Go of its own (ssa.GoLeaf).
//
// The functions are generic, each instantiated with struct{} alone: Go
// lays a generic function's instantiations out after every function and
// closure of the package (see zzz_direct.go), and the check is a method
// of a generic type, used as a method value, which is laid out after them
// (see zzz_dataview.go). initStringRegExpMethods reaches them through
// regexpLeaves, called where its last call, defMethod's of search, was:
// its code shrinks within the padding that rounds it to 32 bytes, and
// every function and closure of the package stays at its address (go tool
// nm).

// regexpLeafOnce gives exec and test their checks.
var regexpLeafOnce sync.Once

// regexpLeaves is initStringRegExpMethods' last call: String.prototype's
// search defined on p, as it was, and then the realm's exec and test given
// their leaves' ops.
func regexpLeaves[_ any](r *Runtime, p *Object, search NativeFunc) {
	r.defMethod(p, "search", 1, search)
	regexpLeafOnce.Do(regexpLeafChecks[struct{}])
	if fd := r.regexpExecFn.fn(); fd != nil && fd.native != nil {
		l := &regexpLeaf[struct{}]{slow: fd.native}
		fd.native, fd.elemOp = l.match, elemRegExpExec
	}
	if fd := r.regexpTestFn.fn(); fd != nil && fd.native != nil {
		l := &regexpLeaf[struct{}]{test: true, slow: fd.native}
		fd.native, fd.elemOp = l.match, elemRegExpTest
	}
}

// regexpLeafChecks gives the leaves their checks.
func regexpLeafChecks[_ any]() {
	exec, test := &regexpLeaf[struct{}]{}, &regexpLeaf[struct{}]{test: true}
	leafChecks[elemRegExpExec], leafChecks[elemRegExpTest] = exec.check, test.check
}

// regexpLeaf is exec's leaf, or test's where test is set, and the
// built-in's own function, slow. Its methods are what would be closures: a
// generic function's closures are laid out ahead of every generic
// instantiation there was.
type regexpLeaf[_ any] struct {
	slow NativeFunc
	test bool
	// spent is the match a leaf's call last found past the step budget,
	// which the call made again with a frame finds without matching again:
	// a pattern, its subject and where the match began fix whether it
	// spends the budget.
	spent struct {
		re *regexp.Regexp
		s  *String
	}
}

// errRegExpLeafAgain is a leaf's call's error where its match failed: it
// made nothing -- no error, which making may run the script's code
// (Error.stackTraceLimit's getter) -- and is made again with a frame, as
// every tier makes a leaf's call whose function errs, which throws.
var errRegExpLeafAgain = errors.New("regexp: a leaf's match made again with a frame")

// match is exec's or test's function (funcData.native): the built-in's
// own, slow, for anything but a string matched by a RegExp whose lastIndex
// is a number -- and, for test, whose exec is the built-in -- which it
// matches in one pass, as the built-in does: regexpMatch's steps, without
// what the built-in asks first of a value that may be anything -- its
// receiver's data, its argument's conversion, lastIndex's -- and without
// the calls between. lastIndex, a number, converts calling nothing, and
// matters only to a global or a sticky pattern (stateful). A match is
// asked the check regexpMatch would ask (a deadline's, an abort's) and its
// errors are regexpMatch's, the step budget's SyntaxError among them, and
// so is a write of lastIndex that cannot be: a leaf's call, its RegExp
// neither global nor sticky, writes none, and where its match fails errs
// with nothing made (errRegExpLeafAgain), its call made again with a frame
// (framed). Every tier calls it, the leaf's check passed or not: the
// interpreter, the tree tier and native code's leaf routine.
func (l *regexpLeaf[_]) match(r *Runtime, this Value, args []Value) (Value, error) {
	if len(args) == 0 || !args[0].IsString() || !this.IsObject() {
		return l.slow(r, this, args)
	}
	o := this.Object()
	d, ok := o.dataOf().(*regexpData)
	if !ok || o.class != ClassRegExp || len(o.vals) == 0 || !o.vals[0].IsNumber() || l.test && !r.execIsBuiltin(this) {
		return l.slow(r, this, args)
	}
	re, s := d.re, args[0].String()
	if re.Flags()&(regexp.FlagGlobal|regexp.FlagSticky) != 0 {
		return l.stateful(r, o, d, s)
	}
	if l.spent.re == re && l.spent.s == s && l.framed(r) {
		// The leaf's call that found it past the budget, made again.
		l.spent.re, l.spent.s = nil, nil
		return l.failed(r, regexp.ErrComplexity)
	}
	var check func() error
	if (r.abort != nil || r.ctx != nil && r.ctx.Done() != nil) && !re.NeverFails(s.ascii) {
		check = r.checkInterruptNow
	}
	j := subjectOf(s)
	caps, err := j.match(re, r.legacyRegExp.spare(), 0, check)
	if err != nil {
		if !l.framed(r) {
			if errors.Is(err, regexp.ErrComplexity) {
				l.spent.re, l.spent.s = re, s
			}
			return Undefined, errRegExpLeafAgain
		}
		return l.failed(r, err)
	}
	if caps == nil {
		if l.test {
			return False, nil
		}
		return Null, nil
	}
	r.takeLegacyMatch(d.legacy(), s, caps)
	if l.test {
		return True, nil
	}
	return Obj(r.buildMatchResult(re, caps, s)), nil
}

// stateful is match's of s by o, d's RegExp, global or sticky: from where
// lastIndex, a number, says, and written back, as regexpMatch's
// stateful branch matches.
func (l *regexpLeaf[_]) stateful(r *Runtime, o *Object, d *regexpData, s *String) (Value, error) {
	re := d.re
	li, err := r.toLength(o.vals[0])
	if err != nil {
		return Undefined, err
	}
	start := int(min(li, maxStringLength+1))
	j := subjectOf(s)
	var caps []int
	if start <= j.len() {
		if re.Flags()&(regexp.FlagUnicode|regexp.FlagUnicodeSets) != 0 && j.inPair(start) {
			start--
		}
		var check func() error
		if (r.abort != nil || r.ctx != nil && r.ctx.Done() != nil) && !re.NeverFails(s.ascii) {
			check = r.checkInterruptNow
		}
		if caps, err = j.match(re, r.legacyRegExp.spare(), start, check); err != nil {
			return l.failed(r, err)
		}
	}
	if caps == nil {
		if err := r.setLastIndex(o, Int(0)); err != nil {
			return Undefined, err
		}
		if l.test {
			return False, nil
		}
		return Null, nil
	}
	if err := r.setLastIndex(o, Int(caps[1])); err != nil {
		return Undefined, err
	}
	r.takeLegacyMatch(d.legacy(), s, caps)
	if l.test {
		return True, nil
	}
	return Obj(r.buildMatchResult(re, caps, s)), nil
}

// framed reports whether exec's or test's call has a frame of its own, the
// innermost: a leaf's call has none, and its innermost frame is its
// caller's.
func (l *regexpLeaf[_]) framed(r *Runtime) bool {
	f := r.topFrame()
	return f != nil && f.cl == nil && (f.callee == r.regexpExecFn || f.callee == r.regexpTestFn)
}

// failed is match's error where the matcher's match failed, as
// regexpMatch makes it: the step budget's SyntaxError, or the check's.
func (l *regexpLeaf[_]) failed(r *Runtime, err error) (Value, error) {
	if errors.Is(err, regexp.ErrComplexity) {
		return Undefined, r.throwError(errSyntax, "%s", err.Error())
	}
	return Undefined, err
}

// check is the leaf's check (leafChecks). A global or a sticky pattern is
// refused first: every exec and test of them asks it, which a leaf's call
// never makes. A string's match by any other may fail -- spend the step
// budget, or meet the check's interrupt -- and then errs with nothing made
// (errRegExpLeafAgain), to be made again with a frame; a match of any other
// primitive is the built-in's (slow), which throws where it fails, and so
// is a leaf's only where it cannot (regexp's NeverFails).
func (l *regexpLeaf[_]) check(r *Runtime, this Value, args []Value) bool {
	if !this.IsObject() {
		return false
	}
	o := this.Object()
	if o.class != ClassRegExp {
		return false
	}
	d, ok := o.dataOf().(*regexpData)
	if !ok {
		return false
	}
	re := d.re
	if re.Flags()&(regexp.FlagGlobal|regexp.FlagSticky) != 0 {
		return false
	}
	// The subject: a primitive's string, all ASCII but for a string's.
	ascii, str := true, false
	if len(args) > 0 {
		switch a := args[0]; {
		case a.IsString():
			ascii, str = a.String().ascii, true
		case a.IsNumber(), a.IsBool(), a.IsUndefined(), a.IsNull():
		default:
			return false
		}
	}
	// lastIndex, which ToLength converts, a number.
	if !str && !re.NeverFails(ascii) || len(o.vals) == 0 || o.shape == nil || len(o.shape.keys) == 0 || o.shape.keys[0].key != atomLastIndex || !o.vals[0].IsNumber() {
		return false
	}
	// test's call of exec is the built-in's (execIsBuiltin).
	return !l.test || o.proto == r.proto.regexp && len(o.vals) == 1 && propsIntact(o.proto, r.regexpExecProps)
}

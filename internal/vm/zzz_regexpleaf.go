package vm

import (
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
// or null, not a symbol; and the pattern one whose match is a search that
// never fails (regexp's NeverFails), for the subject a primitive's string
// is: a match the matcher makes may run out of its step budget, and its
// error, made without a frame, would have run what making one may run
// (Error.stackTraceLimit's getter). test calls the RegExp's exec, which
// must be the built-in, read as execIsBuiltin reads it. What the match
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
	if fd := r.regexpExecFn.fn(); fd != nil {
		fd.elemOp = elemRegExpExec
	}
	if fd := r.regexpTestFn.fn(); fd != nil {
		fd.elemOp = elemRegExpTest
	}
}

// regexpLeafChecks gives the leaves their checks.
func regexpLeafChecks[_ any]() {
	exec, test := &regexpLeaf[struct{}]{}, &regexpLeaf[struct{}]{test: true}
	leafChecks[elemRegExpExec], leafChecks[elemRegExpTest] = exec.check, test.check
}

// regexpLeaf is exec's leaf, or test's where test is set. Its method is
// what would be a closure: a generic function's closures are laid out
// ahead of every generic instantiation there was.
type regexpLeaf[_ any] struct {
	test bool
}

// check is the leaf's check (leafChecks). A global or a sticky pattern,
// and one the matcher matches, are refused first: every exec and test of
// them asks it, which a leaf's call never makes.
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
	ascii := true
	if len(args) > 0 {
		switch a := args[0]; {
		case a.IsString():
			ascii = a.String().ascii
		case a.IsNumber(), a.IsBool(), a.IsUndefined(), a.IsNull():
		default:
			return false
		}
	}
	// lastIndex, which ToLength converts, a number.
	if !re.NeverFails(ascii) || len(o.vals) == 0 || o.shape == nil || len(o.shape.keys) == 0 || o.shape.keys[0].key != atomLastIndex || !o.vals[0].IsNumber() {
		return false
	}
	// test's call of exec is the built-in's (execIsBuiltin).
	return !l.test || o.proto == r.proto.regexp && len(o.vals) == 1 && propsIntact(o.proto, r.regexpExecProps)
}

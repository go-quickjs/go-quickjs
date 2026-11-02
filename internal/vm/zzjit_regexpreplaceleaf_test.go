//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// regexpReplaceLeafSrc replaces strings by RegExps no literal's, which a
// global object's properties hold: global, plain, sticky, under u with
// empty matches over surrogate pairs, of named groups, of none; by
// templates without a $, with $1, $2, $&, $` and $', with $<k>, not all
// ASCII; with the result read, and with nothing reading it but whether it
// is truthy; a lastIndex made an object whose valueOf counts its calls,
// which the leaf's check refuses; a template whose replacements could make
// a string too long, refused for a long subject; a match past the step
// budget; global and sticky RegExps given a lastIndex first.
const regexpReplaceLeafSrc = `var R = { g: /(a)(b)?/g, n: /(a)(b)?/, y: /a/y, u: /\u{1F600}|(?:)/gu, e: /x*/g, nm: /(?<k>[0-9]+)/g, none: /zzz/g, w: /é|a/g, cost: /(a+)+b/g }
function rep(ss, re, t, n) { var len = 0, last = []; for (var i = 0; i < n; i++) { var s = ss[i % ss.length]; var v = s.replace(re, t); len += v.length; if (i >= n - ss.length) last.push(v + '|' + re.lastIndex + '|' + RegExp.lastMatch + '|' + RegExp['$1'] + '|' + RegExp.leftContext.length) } return len + ' ' + last.join(' ') }
function repDead(ss, re, t, n) { var k = 0; for (var i = 0; i < n; i++) { var s = ss[i % ss.length]; s.replace(re, t); k += re.lastIndex; if (s.replace(re, t)) k += 1000 } return k + '|' + RegExp.lastMatch + '|' + re.lastIndex }
function lix(re, n, odd) { var c = 0, v = 0; for (var i = 0; i < n; i++) { if (odd && i === n - 2) re.lastIndex = { valueOf() { v++; return 0 } }; c += 'xa'.replace(re, '').length } if (odd) re.lastIndex = 0; return c + ":" + v }
function gli(re, n) { var k = 0; for (var i = 0; i < n; i++) { re.lastIndex = i; k += 'xab'.replace(re, '').length * 100 + re.lastIndex } return k }
var big = 'x'.repeat(40000) + 'b'
function mixed(n, last) { var len = 0; for (var i = 0; i < n; i++) len += (last && i === n - 1 ? big : 'ab').replace(R.bb, "$\x60$\x60").length; return len + '|' + RegExp.leftContext.length }
R.bb = /b/g
function cost(n, last) { var out = []; for (var i = 0; i < n; i++) { try { out.push((last && i === n - 1 ? 'a'.repeat(30) + 'c' : 'aab').replace(R.cost, '-')) } catch (e) { out.push(e.name + ':' + e.message + ':' + R.cost.lastIndex) } } return out.slice(-2).join() }
`

// regexpReplaceLeafCases are calls of regexpReplaceLeafSrc's functions,
// each from a fresh runtime, with Node's answers (v26.10.0, a context of
// its own for each), and the fewest leaves' calls of replace a run of the
// call makes natively.
var regexpReplaceLeafCases = []struct {
	call, want string
	leaves     uint64
}{
	{"rep(['xabab', 'zab', 'q', ''], R.g, '', 50)", "38 q|0|ab|a|1 |0|ab|a|1 x|0|ab|a|3 z|0|ab|a|1", 50},
	{"rep(['xabab', 'zab', 'q'], R.n, '-', 50)", "118 q|0|ab|a|1 x-ab|0|ab|a|1 z-|0|ab|a|1", 50},
	{"rep(['xabab', 'zab'], R.g, \"[$1|$2|$&|$`|$']\", 50)", "975 x[a|b|ab|x|ab][a|b|ab|xab|]|0|ab|a|3 z[a|b|ab|z|]|0|ab|a|1", 50},
	{"(R.y.lastIndex = 0, rep(['aaa', 'baa', 'a'], R.y, 'Y', 50))", "118 a|0|a||1 Yaa|1|a||0 bYa|2|a||1", 50},
	{"rep(['é\U0001f600x\U0001f600', '\U0001f600'], R.u, '.', 50)", "225 .é..x..|0|||6 ..|0|||2", 50},
	{"rep(['abc', ''], R.e, '-', 50)", "200 -a-b-c-|0|||3 -|0|||0", 50},
	{"rep(['a12b345', 'x'], R.nm, '<$<k>>', 50)", "300 a<12>b<345>|0|345|345|4 x|0|345|345|4", 50},
	{"rep(['café au lait'], R.w, 'E', 50)", "600 cEfE Eu lEit|0|a||9", 50},
	{"rep(['xabab'], R.none, '', 50)", "250 xabab|0|||0", 50},
	{"rep(['xabab'], R.g, 'é', 50)", "150 xéé|0|ab|a|3", 50},
	{"repDead(['xabab', 'ab', ''], R.g, '', 50)", "17000|ab|0", 100},
	{"(R.y.lastIndex = 0, repDead(['aab', 'b'], R.y, '', 50))", "50025|a|0", 100},
	{"lix(/a/, 50, false) + ',' + lix(R.n, 50, true)", "50:0,50:2", 90},
	{"mixed(50, false) + ',' + mixed(50, true)", "150|1,120147|40000", 90},
	{"gli(R.g, 50) + ',' + gli(R.y, 50)", "5000,14902", 100},
}

// String.prototype.replace given a RegExp no literal's and a string is a
// leaf's call of native code (zzjit_regexpreplaceleaf.go), made through
// the VM's routine for leaves: each answer the interpreter's and Node's,
// with the collector idle and while it marks -- the matches, the
// replacements, lastIndex and the legacy statics -- and where the leaf's
// check refuses -- a lastIndex whose valueOf runs, a result that could be
// too long -- the call is made with a frame, as is its answer.
func TestJITRegExpReplaceLeaf(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		runtime.GC()
		gc := debug.SetGCPercent(-1)
		if marking {
			jitMarkingForTest(t)
		}
		for _, c := range regexpReplaceLeafCases {
			r := jitAgreesForTest(t, regexpReplaceLeafSrc, c.call, c.call, c.call, c.call, c.call, c.call)
			var name string
			for _, n := range []string{"repDead", "rep", "lix", "mixed", "gli"} {
				if strings.Contains(c.call, n+"(") {
					name = n
					break
				}
			}
			e := jitNativeForTest(t, r, name)
			leaves := 0
			for _, st := range e.ssa.GoCallStates() {
				if ssa.GoOp(st.Op&0xff) == ssa.GoLeaf && st.Leaf == uintptr(unsafe.Pointer(r.strReplaceFn)) {
					leaves++
				}
			}
			if want := map[string]int{"rep": 1, "repDead": 2, "lix": 1, "mixed": 1, "gli": 1}[name]; leaves != want {
				t.Errorf("marking %v: %s's code has %d leaves' calls of replace, want %d", marking, name, leaves, want)
			}
			fast := r.jit.leafFast
			v, err := r.Run(compileForTest(t, "''+("+c.call+")"))
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String().Go(); got != c.want {
				t.Errorf("marking %v: %s = %s, want Node's %s", marking, c.call, got, c.want)
			}
			if got := r.jit.leafFast - fast; got < c.leaves {
				t.Errorf("marking %v: %s made %d leaves' calls through the routine, want at least %d", marking, c.call, got, c.leaves)
			}
		}
		debug.SetGCPercent(gc)
	}
}

// A replace whose match spends the step budget errs, as a leaf's call,
// with nothing made, and is made again with a frame, which throws the
// interpreter's SyntaxError, lastIndex where that call left it; the
// matches before, and after, are leaves' calls.
func TestJITRegExpReplaceLeafComplexity(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitAgreesForTest(t, regexpReplaceLeafSrc, "cost(50, false)", "cost(50, false)", "cost(50, false)", "cost(50, true)", "cost(50, false)", "cost(50, true)")
	fast := r.jit.leafFast
	v, err := r.Run(compileForTest(t, "''+cost(50, false) + ' ' + cost(50, true)"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String().Go(), "-,- -,SyntaxError:regular expression is too complex:0"; got != want {
		t.Errorf("= %s, want %s", got, want)
	}
	if got := r.jit.leafFast - fast; got < 50 {
		t.Errorf("%d leaves' calls through the routine, want at least 50", got)
	}
}

// A replace whose RegExp a global or a captured binding holds, given a
// string constant, is compiled a leaf's call from the first compile
// (jitReplaceLeafGuess), never a call of Go to learn it at; its answers
// the interpreter's and Node's.
func TestJITRegExpReplaceLeafGuess(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	setup := `var G = /(a)(x)?/g
function gr(ss, n) { var k = 0; for (var i = 0; i < n; i++) k += ss[i % 2].replace(G, '').length; return k + '|' + RegExp.lastMatch }
var up = (function () { var U = /b+/; return function (ss, n) { var k = 0; for (var i = 0; i < n; i++) k += ss[i % 2].replace(U, '-$&-').length; return k + '|' + RegExp.lastMatch } })()`
	for _, c := range []struct{ call, want, name string }{
		{"gr(['xaay', 'bab'], 200)", "400|a", "gr"},
		{"up(['abbc', 'b'], 200)", "900|b", "up"},
	} {
		r := jitAgreesForTest(t, setup, c.call, c.call, c.call)
		v, err := r.Run(compileForTest(t, "''+"+c.call))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != c.want {
			t.Errorf("%s = %s, want Node's %s", c.call, got, c.want)
		}
		e := jitNativeForTest(t, r, c.name)
		leaves := 0
		for _, st := range e.ssa.GoCallStates() {
			if ssa.GoOp(st.Op&0xff) == ssa.GoLeaf && st.Leaf == uintptr(unsafe.Pointer(r.strReplaceFn)) {
				leaves++
			}
		}
		// A call of Go of replace would have been asked whether it was
		// given a literal's RegExp (jitRegExpLitPool).
		if leaves != 1 || len(e.regexpLitCalls) != 0 {
			t.Errorf("%s's code has %d leaves' calls of replace, and made %d calls of Go of it; want 1 and none", c.name, leaves, len(e.regexpLitCalls))
		}
	}
}

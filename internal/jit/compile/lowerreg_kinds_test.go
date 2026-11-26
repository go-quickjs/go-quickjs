package compile

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
)

// regKindsSrc has functions each kind of lowering takes or refuses at a
// check of its own: plain code, host operations alone (refused for an
// entry, taken for a callee, and for a hot function where it calls), an
// async function and a generator (refused for a callee before any
// instruction), a class's computed accessor (a definition a callee
// leaves to Go), a forwarding call, a regexp literal's method call (the
// lowering run again with it Go's whole), and a loop.
const regKindsSrc = `
function plain(a, b) { return a * b + 1 }
function hostOnly(o) { return o.x + o.y }
function hostCalls(o, f) { return f(o.x) }
async function asyncFn(x) { let s = 0; for (let i = 0; i < x; i++) s += await i; return s }
function* gen(n) { for (let i = 0; i < n; i++) yield i }
function definer(k, n) { let s = 0; for (let i = 0; i < n; i++) s += i; return [s, class { get [k]() { return 1 } }] }
function forwards() { return target.apply(this, arguments) }
function target(a) { return a }
function lit(s) { return /a+b/.exec(s) }
function lits(s) { const r = /x/g; let n = 0; while (r.exec(s)) n++; return n }
function loop(n) { let s = 0; for (let i = 0; i < n; i++) s += i; return s }
function sloppyArgs(a) { arguments[0] = 2; return a }
function mod(a, b) { return a % b }
`

// LowerRegKindsWith lowers once what LowerRegWith, LowerRegHotWith and
// LowerRegInlineWith lower apart: for each function, each kind's program
// is the one its own lowering makes, and its refusal the same.
func TestLowerRegKindsMatchApart(t *testing.T) {
	fns := regCompiled(t, regKindsSrc)
	for _, keep := range []bool{false, true} {
		h := Hints{KeepParams: keep}
		for name, fn := range fns {
			rc := fn.RegCode()
			p, entry, hot, callee := LowerRegKindsWith(rc, fn, h)
			for _, k := range []struct {
				kind  string
				err   error
				apart func(*bytecode.RegCode, *bytecode.Function, Hints) (*ir.Program, error)
			}{{"entry", entry, LowerRegWith}, {"hot", hot, LowerRegHotWith}, {"callee", callee, LowerRegInlineWith}} {
				q, err := k.apart(rc, fn, h)
				if fmt.Sprint(err) != fmt.Sprint(k.err) {
					t.Errorf("%s, %s: refused %v at once, %v apart", name, k.kind, k.err, err)
					continue
				}
				if err == nil && !reflect.DeepEqual(p, q) {
					t.Errorf("%s, %s: the program lowered at once is not the one lowered apart", name, k.kind)
				}
			}
		}
	}
	// What the source is for: each kind takes some and refuses some.
	for name, want := range map[string][3]bool{
		"plain": {true, true, true}, "hostOnly": {false, false, true}, "hostCalls": {false, true, true},
		"asyncFn": {true, true, false}, "gen": {true, true, false}, "definer": {true, true, false},
	} {
		fn := fns[name]
		_, entry, hot, callee := LowerRegKindsWith(fn.RegCode(), fn, Hints{})
		if got := [3]bool{entry == nil, hot == nil, callee == nil}; got != want {
			t.Errorf("%s: entry, hot, callee taken %v (%v, %v, %v), want %v", name, got, entry, hot, callee, want)
		}
	}
}

package ssa

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
)

// A callee with a loop is inlined only where its loop calls what it is
// given -- a parameter, read just before the call, alone or with another
// local (get_local2) -- which a closure its caller makes may be: then the
// inlining fixes what the loop calls at every turn. A loop that calls
// nothing so, Crypto's am3 among them, is refused, its call made once for
// all its turns; and one that calls a parameter reassigned in the loop is
// taken here, the SSA saying what the call calls (calledClosures).
func TestInlineCallsGiven(t *testing.T) {
	for _, c := range []struct {
		src   string
		ok    bool
		given int
	}{
		{`function forEach(fn) { for (let i = this.a; i < this.b; i += 1) { fn(this.s[i]) } }`, true, 1},
		{`function hasSome(fn) { for (let i = 0; i < this.n; i += 1) { if (fn(this.s[i])) { return true } } return false }`, true, 1},
		{`function each(a, f) { var i, s = 0; for (i = 0; i < a.length; i++) s = (s + f(a[i])) | 0; return s }`, true, 1},
		{`function apply(f, x) { return f(x) + 1 }`, true, 1},
		{`function am3(i, x, w, j, c, n) { var a = this.array, b = w.array, xl = x & 0x3fff, xh = x >> 14;
			while (--n >= 0) { var l = a[i] & 0x3fff, h = a[i++] >> 14, m = xh * l + h * xl;
				l = xl * l + ((m & 0x3fff) << 14) + b[j] + c; c = (l >> 28) + (m >> 14) + xh * h; b[j++] = l & 0xfffffff } return c }`, false, 0},
		{`function method(o) { for (let i = 0; i < 3; i++) { o.f(i) } }`, false, 0},
		{`function swap(fn, g) { for (let i = 0; i < 3; i++) { fn(i); fn = g } }`, true, 1},
	} {
		fn := compiledFunctionForTest(t, c.src)
		p, err := jitcompile.LowerSSAInline(fn)
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
		_, given, ok := InlineCallsGiven(p, func(pc int) (int, bool) {
			if in := fn.Code[pc]; in.Op == bytecode.OpCall {
				return int(in.A), true
			}
			return 0, false
		})
		if ok != c.ok || len(given) != c.given {
			t.Errorf("%s: given %v, %v; want %d, %v", c.src, given, ok, c.given, c.ok)
		}
		if _, ok := InlineCalls(p); ok && looping(p) {
			// Inlining without a closure to fix what the loop calls.
			t.Errorf("%s: InlineCalls took a loop", c.src)
		}
	}
}

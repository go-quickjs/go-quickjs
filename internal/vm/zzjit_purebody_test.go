//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// PureBody holds only for a pure body pureCall evaluates, whose sites the
// tree tier cannot mark: a short one, or one that calls. One that
// pureByTree gives its tree, as long as pureTreeCode instructions and
// calling nothing, is marked as any function is and is none.
func TestJITPureBodyOnlyWithoutTree(t *testing.T) {
	for _, tc := range []struct {
		src    string
		byTree bool
	}{
		{`function f(x, y) { return x < y }`, false},
		{`function f(x) { return -x }`, false},
		{`function f(s, x) { return (s * 31n + x) & 0xffffffffffffn }`, true},
		{`function f(a, b, c) { return a * b + b * c + c * a }`, true},
	} {
		fn := jitFunctionForTest(t, tc.src)
		if fn.Leaf != bytecode.LeafPure {
			t.Fatalf("%s: not a pure body (Leaf %v)", tc.src, fn.Leaf)
		}
		if got := pureByTree(fn); got != tc.byTree {
			t.Fatalf("%s: pureByTree %v, want %v (%d instructions)", tc.src, got, tc.byTree, len(fn.Code))
		}
		if got := (&jitFeedback{fn: fn}).PureBody(); got != !tc.byTree {
			t.Errorf("%s: PureBody %v, want %v", tc.src, got, !tc.byTree)
		}
	}
	if (&jitFeedback{}).PureBody() {
		t.Error("PureBody with no function")
	}
	if (&jitFeedback{fn: jitFunctionForTest(t, `function f(n) { let s = 0; for (let i = 0; i < n; i++) s += i; return s }`)}).PureBody() {
		t.Error("PureBody of a function with a loop")
	}
}

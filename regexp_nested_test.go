package quickjs_test

import (
	"context"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestRegExpNestedCountedQuantifiersEnd pins that a counted quantifier
// whose body can match nothing, inside another counted quantifier, ends
// with Node v26's answer: the inner loop's count was set to 0 at each of
// the outer loop's iterations without the count it had being kept, so that
// backtracking into an earlier iteration ran its loop again from the same
// choice point, for ever (KI-81). Each pattern is matched five times, so
// that with the JIT its native code matches too; over an ASCII subject and
// over one past ASCII, under a deadline and without one, where a host that
// cannot stop a script has the step budget, which must not call these
// patterns too complex (KI-17).
func TestRegExpNestedCountedQuantifiersEnd(t *testing.T) {
	const script = `JSON.stringify([
  [/((a?){2}){0,2}x/, "a"], [/((a?){2}){0,3}a/, "aa"], [/(([a-z]?){2,}){0,3}[a-z]/, "abc"],
  [/((a?){2}){0,1}x/, "a"], [/((a?){2}){2}/, "aaa"], [/((a){2,}){0,3}[a-z]/, "a"],
  [/((a?)*){0,2}b/, "aab"], [/((a??){2}){0,3}a/, "aa"], [/(?=((a?){2}){0,2}x)/, "aax"],
  [/((a?){2}\2){0,3}b/, "aab"], [/((\u{1F600}?){2}){0,3}\u{1F600}/u, "\u{1F600}\u{1F600}"],
  [/(([a-z]?){2,}){0,3}[a-z]/v, "abc"], [/((A?){2}){0,3}a/i, "éaa"],
  [/((é?){2}){0,2}x/, "é"], [/((a?){2}){0,2}x/g, "aaxaxa"],
].map(([re, s]) => {
  let r
  for (let i = 0; i < 5; i++) { re.lastIndex = 0; r = re.exec(s) }
  return r
}))`
	const want = `[null,["aa","a",""],["abc","ab","b"],null,["aaa","a",""],["a",null,null],` +
		`["aab","aa","a"],["aa","a","a"],["","aa","a"],["aab","a",""],["😀😀","😀",""],` +
		`["abc","ab","b"],["aa","a",""],null,["aax","aa","a"]]`
	for name, opts := range map[string][]quickjs.Option{"interpreter": nil, "jit": {quickjs.WithJIT()}} {
		t.Run(name, func(t *testing.T) {
			rt := quickjs.New(opts...)
			defer rt.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			v, err := rt.EvalContext(ctx, script)
			if err != nil || v.String() != want {
				t.Errorf("under a deadline: %v, %v; want %s", v, err, want)
			}
			v, err = rt.Eval(script)
			if err != nil || v.String() != want {
				t.Errorf("under the step budget: %v, %v; want %s", v, err, want)
			}
		})
	}
}

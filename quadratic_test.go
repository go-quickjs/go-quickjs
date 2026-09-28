package quickjs_test

import (
	"context"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestLinearBuilds pins that building a string a surrogate half at a time,
// and binding a function to itself over and over, take time in proportion to
// how much is built: joining the halves of a pair flattened the string, and
// each bind copied the "bound bound ..." name in full (KI-27).
func TestLinearBuilds(t *testing.T) {
	for name, tc := range map[string]struct{ src, want string }{
		"surrogate halves": {`
			let s = "";
			for (let i = 0; i < 200000; i++) { s += "\uD83D"; s += "\uDE00"; }
			[s.length, s === "\u{1F600}".repeat(200000), s.codePointAt(399998).toString(16)].join()`,
			"400000,true,1f600"},
		"bound names": {`
			let f = function g() {};
			for (let i = 0; i < 100000; i++) f = f.bind();
			[f.name.length, f.name.startsWith("bound bound "), f.name.endsWith("bound g")].join()`,
			"600001,true,true"},
	} {
		t.Run(name, func(t *testing.T) {
			rt := quickjs.New()
			defer rt.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			v, err := rt.EvalContext(ctx, tc.src)
			if err != nil || v.String() != tc.want {
				t.Errorf("= %v, %v; want %s", v, err, tc.want)
			}
		})
	}
}

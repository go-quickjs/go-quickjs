package quickjs_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs"
)

func TestCompileManyLocals(t *testing.T) {
	var padding strings.Builder
	for i := 0; i < 128; i++ {
		fmt.Fprintf(&padding, "var padding%d = %d;", i, i)
	}
	var block strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&block, "let block%d = %d;", i, i)
	}
	cases := []struct {
		name, body, want string
	}{
		{"shadow", `let x = 1; let f; { let x = 2; f = () => x; { let x = 3; if (x !== 3) throw 0; } if (x !== 2) throw 0; } return [x, f(), padding0, padding127].join(',')`, "1,2,0,127"},
		{"sibling scopes", `let out = []; { let x = 4; out.push(() => x); } { let x = 5; out.push(() => x); } return out.map(f => f()).join(',')`, "4,5"},
		{"duplicate parameters", `return [x, arguments[0], arguments[1]].join(',')`, "8,7,8"},
		{"parameter scope", `let arguments = 9; return [arguments, get()].join(',')`, "9,7"},
		{"var hoisting", `let f = () => later; var later = 6; var later; return f()`, "6"},
		{"catch", `let x = 1; try { throw 2; } catch (x) { let f = () => x; if (f() !== 2) throw 0; } return x`, "1"},
		{"annex B", `var x = 1; { function x() { return 2; } } return x()`, "2"},
		{"direct eval", `let x = 1; { let x = 2; if (eval('x') !== 2) throw 0; } return eval('x')`, "1"},
		{"dead zone", `let x = 1; { try { return x; } catch (e) { return e.name; } let x = 2; }`, "ReferenceError"},
		{"with", `let x = 1; let get; with ({x: 2}) { get = () => x; } return [x, get()].join(',')`, "1,2"},
		{"index in block", `let x = 1; let f; { let x = 2; ` + block.String() + ` f = () => x; } return [x, f()].join(',')`, "1,2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params, args := "", ""
			switch tc.name {
			case "duplicate parameters":
				params, args = "x, x", "7, 8"
			case "parameter scope":
				params, args = "p = 7, get = () => arguments[0]", "7"
			}
			prefix := padding.String()
			if tc.name == "index in block" {
				prefix = ""
			}
			src := "(function(" + params + "){" + prefix + tc.body + "})(" + args + ")"
			for _, debug := range []bool{false, true} {
				var opts []quickjs.Option
				if debug {
					opts = append(opts, quickjs.WithDebugger())
				}
				rt := quickjs.New(opts...)
				defer rt.Close()
				v, err := rt.Eval(src)
				if err != nil || v.String() != tc.want {
					t.Fatalf("debug=%v: got %v, %v; want %s", debug, v, err, tc.want)
				}
			}
		})
	}
}

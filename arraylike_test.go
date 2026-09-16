package quickjs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// Nearly every Array.prototype method is generic: it works on anything with a
// length and numeric keys. That is what lets
// Array.prototype.map.call(arguments, f) work, and it is most of what test262
// exercises.
func TestArrayMethodsAreGeneric(t *testing.T) {
	const obj = `var o = {length: 3, 0: "a", 1: "b", 2: "c"}; `
	const nums = `var o = {length: 3, 0: 1, 1: 2, 2: 3}; `

	cases := []struct{ src, want string }{
		{obj + `Array.prototype.map.call(o, x => x).join(",")`, "a,b,c"},
		{obj + `Array.prototype.filter.call(o, x => x > "a").join(",")`, "b,c"},
		{obj + `Array.prototype.join.call(o, "-")`, "a-b-c"},
		{obj + `Array.prototype.slice.call(o, 1).join(",")`, "b,c"},
		{obj + `String(Array.prototype.indexOf.call(o, "b"))`, "1"},
		{obj + `String(Array.prototype.lastIndexOf.call(o, "c"))`, "2"},
		{obj + `String(Array.prototype.includes.call(o, "c"))`, "true"},
		{obj + `Array.prototype.concat.call([], o).length + "," +
		        Array.prototype.concat.call([1], 2).join(",")`, "1,1,2"},
		{nums + `String(Array.prototype.reduce.call(o, (a, b) => a + b))`, "6"},
		{nums + `String(Array.prototype.reduceRight.call(o, (a, b) => a + b))`, "6"},
		{nums + `String(Array.prototype.every.call(o, x => x > 0))`, "true"},
		{nums + `String(Array.prototype.some.call(o, x => x > 2))`, "true"},
		{nums + `String(Array.prototype.find.call(o, x => x > 1))`, "2"},
		{nums + `String(Array.prototype.findIndex.call(o, x => x > 1))`, "1"},
		{nums + `String(Array.prototype.findLast.call(o, x => x < 3))`, "2"},
		{nums + `var out = []; Array.prototype.forEach.call(o, x => out.push(x)); out.join(",")`,
			"1,2,3"},
		// The arguments object is the canonical array-like.
		{`function f() { return Array.prototype.map.call(arguments, x => x * 2).join(","); }
		  f(1, 2, 3)`, "2,4,6"},
		// A string is array-like too.
		{`Array.prototype.map.call("abc", c => c.toUpperCase()).join("")`, "ABC"},

		// The callback receives (value, index, object).
		{`var seen; [7].map(function (v, i, arr) { seen = [v, i, arr.length].join(":"); });
		  seen`, "7:0:1"},
		{`[1].map(function () { return this.x; }, {x: 9}).join(",")`, "9"},
	}

	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}
}

// TestArrayHoles pins that an elision is an absent property rather than one
// holding undefined, which decides whether the iteration methods visit it and
// whether the prototype shows through.
func TestArrayHoles(t *testing.T) {
	cases := []struct{ src, want string }{
		{`String(1 in [1, , 3])`, "false"},
		{`String(0 in [1, , 3])`, "true"},
		{`JSON.stringify(Object.keys([1, , 3]))`, `["0","2"]`},
		{`String([1, , 3].length)`, "3"},
		{`var n = 0; [1, , 3].forEach(() => n++); String(n)`, "2"},
		{`var n = 0; [1, , 3].map(() => n++); String(n)`, "2"},
		{`String([1, , 3].filter(() => true).length)`, "2"},
		// find visits holes, unlike forEach, because it is looking for a
		// position.
		{`var n = 0; [1, , 3].find(() => { n++; return false; }); String(n)`, "3"},
		// A hole falls through to the prototype.
		{`Array.prototype[1] = "proto"; [1, , 3].join("-")`, "1-proto-3"},
		{`Array.prototype[1] = "proto"; String([1, , 3].includes("proto"))`, "true"},
		// new Array(n) is all holes.
		{`var a = new Array(3); String(0 in a) + "," + a.length`, "false,3"},
		{`var a = []; a[2] = 1; JSON.stringify(Object.keys(a))`, `["2"]`},
		{`var a = [1, 2]; delete a[0]; String(0 in a)`, "false"},
		// A hole stays a hole through map and slice.
		{`String(1 in [1, , 3].map(x => x))`, "false"},
		{`String(0 in [1, , 3].slice(1))`, "false"},
	}

	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}
}

// TestArrayIndexAccessor pins that an index redefined as an accessor is
// actually called. Dense storage cannot express one, so defining it has to
// vacate the slot.
func TestArrayIndexAccessor(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var a = [0, 1]; Object.defineProperty(a, 0, {get() { return 5; }});
		  String(a[0])`, "5"},
		{`var log = []; var a = [0, 1];
		  Object.defineProperty(a, 0, {get() { log.push("g"); return 5; }});
		  a.map(x => x).join(",") + "|" + log.length`, "5,1|1"},
		{`var a = [0]; Object.defineProperty(a, 0, {value: 7, writable: false});
		  a[0] = 9; String(a[0])`, "7"},
	}

	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}
}

// TestHugeArrayLikeIsInterruptible pins that walking an array-like which claims
// an enormous length can still be stopped. Such a walk runs no bytecode, so
// without an explicit check it would be a hang the context bound could not
// reach -- the exact failure the bound exists to prevent.
func TestHugeArrayLikeIsInterruptible(t *testing.T) {
	for _, src := range []string{
		`Array.prototype.includes.call({length: 9007199254740991}, 1)`,
		`Array.prototype.indexOf.call({length: 9007199254740991}, 1)`,
		`Array.prototype.join.call({length: 9007199254740991})`,
	} {
		rt := quickjs.New()
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)

		done := make(chan error, 1)
		go func() {
			_, err := rt.EvalContext(ctx, src)
			done <- err
		}()

		select {
		case err := <-done:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("%s: got %v, want a deadline error", src, err)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("%s: not interrupted", src)
		}
		cancel()
		rt.Close()
	}
}

package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

func TestTypedArrayMethods(t *testing.T) {
	cases := []struct{ src, want string }{
		// %TypedArray% is the prototype of all nine concrete constructors, and
		// is where the shared methods and statics live.
		{`Object.getPrototypeOf(Int8Array).name`, "TypedArray"},
		{`String(Object.getPrototypeOf(Int8Array) === Object.getPrototypeOf(Float64Array))`, "true"},
		{`String(Object.getPrototypeOf(Int8Array.prototype) ===
		         Object.getPrototypeOf(Uint8Array.prototype))`, "true"},

		{`new Uint8Array([1, 2, 3]).map(x => x * 2).join(",")`, "2,4,6"},
		// A view's map yields a view of the same kind, not a plain array.
		{`String(new Uint8Array([1, 2, 3]).map(x => x) instanceof Uint8Array)`, "true"},
		{`new Uint8Array([1, 2, 3]).filter(x => x > 1).join(",")`, "2,3"},
		{`String(new Uint8Array([1, 2, 3]).reduce((a, b) => a + b))`, "6"},
		{`String(new Uint8Array([1, 2, 3]).reduceRight((a, b) => a + b))`, "6"},
		{`new Uint8Array([1, 2, 3]).reverse().join(",")`, "3,2,1"},
		{`new Uint8Array([1, 2, 3]).toReversed().join(",")`, "3,2,1"},
		{`new Uint8Array([1, 2, 3]).with(1, 9).join(",")`, "1,9,3"},
		{`String(new Uint8Array([1, 2, 3]).at(-1))`, "3"},
		{`new Uint8Array([1, 2, 3, 4]).copyWithin(0, 2).join(",")`, "3,4,3,4"},
		{`String(new Uint8Array([1, 2, 3]).lastIndexOf(3))`, "2"},
		{`String(new Uint8Array([1, 2, 3]).findLast(x => x < 3))`, "2"},

		// A typed array sorts numerically by default, where an array sorts by
		// string.
		{`new Uint8Array([10, 9]).sort().join(",")`, "9,10"},
		{`[10, 9].sort().join(",")`, "10,9"},
		{`new Uint8Array([10, 9]).toSorted().join(",")`, "9,10"},

		{`Uint8Array.from([1, 2]).join(",")`, "1,2"},
		{`Uint8Array.from([1, 2], x => x * 3).join(",")`, "3,6"},
		{`Uint8Array.of(1, 2).join(",")`, "1,2"},
		{`String(Uint8Array.from([1]) instanceof Uint8Array)`, "true"},

		{`Array.from(new Uint8Array([1, 2]).entries()).length + ""`, "2"},
		{`Array.from(new Uint8Array([1, 2]).keys()).join(",")`, "0,1"},
		{`Array.from(new Uint8Array([1, 2]).values()).join(",")`, "1,2"},

		// The elements are own enumerable properties even though they live in a
		// buffer rather than in the property table.
		{`Object.keys(new Uint8Array([1, 2])).join(",")`, "0,1"},
		{`Object.getOwnPropertyNames(new Uint8Array([1, 2])).join(",")`, "0,1"},
		{`JSON.stringify(Object.getOwnPropertyDescriptor(new Uint8Array([7]), "0"))`,
			`{"value":7,"writable":true,"enumerable":true,"configurable":true}`},
		{`JSON.stringify(new Uint8Array([1, 2]))`, `{"0":1,"1":2}`},

		// The tag names the concrete type.
		{`Object.prototype.toString.call(new Uint8Array(1))`, "[object Uint8Array]"},
		{`Object.prototype.toString.call(new Float64Array(1))`, "[object Float64Array]"},
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

func TestBigIntBuiltins(t *testing.T) {
	cases := []struct{ src, want string }{
		{`typeof BigInt`, "function"},
		{`String(BigInt(1))`, "1"},
		{`String(BigInt("123"))`, "123"},
		{`String(BigInt(true)) + "," + String(BigInt(false))`, "1,0"},
		{`(10n).toString(2)`, "1010"},
		{`(255n).toString(16)`, "ff"},
		{`String((1n).valueOf())`, "1"},
		{`Object.prototype.toString.call(Object(1n))`, "[object BigInt]"},

		// asIntN and asUintN are how a program works with fixed-width
		// integers: the arithmetic is arbitrary-precision, and wrapping back is
		// an explicit step.
		{`String(BigInt.asIntN(8, 255n))`, "-1"},
		{`String(BigInt.asUintN(8, 255n))`, "255"},
		{`String(BigInt.asUintN(8, -1n))`, "255"},
		{`String(BigInt.asIntN(64, 2n ** 63n))`, "-9223372036854775808"},
		{`String(BigInt.asIntN(0, 5n))`, "0"},
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

	bad := []string{
		// Only an integer converts: rounding silently would be worse than
		// refusing.
		`BigInt(1.5)`,
		`BigInt(NaN)`,
		`BigInt(Infinity)`,
		`BigInt("nope")`,
		`BigInt(Symbol())`,
		// It is deliberately not a constructor.
		`new BigInt(1)`,
	}
	for _, src := range bad {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: no error, want one", src)
		}
		rt.Close()
	}
}

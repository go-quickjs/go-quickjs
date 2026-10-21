package vm

import "testing"

// TestNativeLeafChecksExact pins what every native leaf's check promises
// (leafChecks): where it passes, the built-in's own function, called
// without a frame, answers -- it throws nothing, which it would otherwise
// make its error for without a frame, the ordinary call then making it
// again. Each leaf is asked of every receiver and up to two arguments of a
// set of values of every kind -- numbers about every edge and every count's
// bounds, strings of every kind, booleans, undefined, null, a BigInt,
// symbols, objects and a String object -- with no memory meter, as the
// leaves are called.
func TestNativeLeafChecksExact(t *testing.T) {
	r := New(Config{})
	defer func() { r.Close(); r.ReleaseClosed() }()
	v, err := r.Run(compileForTest(t, `[0, 1, -1, 2, 3.7, -0, NaN, Infinity, -Infinity, 1e9, 2 ** 31, 0x10FFFF, 0x110000,
		1.9, 36.5, 37, 100.9, 101, -0.5, 1e300, "", "a", "abc", "1", "-2", "1e9", "NFC", "NFKD", "NFX", "  x  ", "\uD83D\uDE00", "\uD800", "xy".repeat(300),
		true, false, undefined, null, 5n, Symbol("s"), Symbol.for("r"), {}, [1], new String("w"), { valueOf() { return 2 } }]`))
	if err != nil {
		t.Fatal(err)
	}
	vals := v.Object().elemsOf()
	// The second arguments: a position, a length, a filler, of each kind.
	var seconds []Value
	for _, i := range []int{0, 1, 3, 6, 10, 21, 22, 25, 35, 38, 39, 41} {
		seconds = append(seconds, vals[i])
	}
	// Each native leaf's function, as the calls take it (funcData.native).
	fns := map[uint8]NativeFunc{}
	for _, o := range r.leafFns {
		if fd := o.fn(); fd.elemOp >= elemNativeFirst {
			fns[fd.elemOp] = fd.native
		}
	}
	checked := 0
	for op := uint8(elemNativeFirst); op < elemLeafEnd; op++ {
		check, fn := leafChecks[op], fns[op]
		if check == nil || fn == nil {
			t.Fatalf("native leaf %d has no check or no function", op)
		}
		// No arguments; one, of each value; two, the second of a few.
		lists := [][]Value{nil}
		for _, a := range vals {
			lists = append(lists, []Value{a})
			for _, b := range seconds {
				lists = append(lists, []Value{a, b})
			}
		}
		for _, this := range vals {
			for _, args := range lists {
				if !check(r, this, args) {
					continue
				}
				checked++
				if _, err := fn(r, this, args); err != nil {
					t.Fatalf("native leaf %d passed its check of %v and %v, and threw: %v", op, r.describe(this), args, err)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no check passed")
	}
}

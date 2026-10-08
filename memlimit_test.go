package quickjs_test

import (
	"errors"
	"fmt"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestMemoryLimit pins that WithMemoryLimit stops a script that holds more
// than it allows, however it gets there, and that the script cannot catch it
// and carry on (KI-02).
func TestMemoryLimit(t *testing.T) {
	const limit = 16 << 20
	for name, src := range map[string]string{
		"many strings":    `let a = []; for (let i = 0; i < 200; i++) a.push("x".repeat(1 << 20) + i); a.length`,
		"many objects":    `let a = []; for (let i = 0; i < 2e6; i++) a.push({i}); a.length`,
		"a buffer":        `new ArrayBuffer(200 * 1024 * 1024).byteLength`,
		"a typed array":   `new Float64Array(30 * 1024 * 1024).length`,
		"a wide string":   `"一".repeat(2 ** 27).length`,
		"a doubled rope":  `let s = "ab".repeat(100); for (let i = 0; i < 30; i++) s += s; s.indexOf("zz")`,
		"concat":          `let s = "ab".repeat(100); for (let i = 0; i < 30; i++) s = s.concat(s); s.indexOf("zz")`,
		"padding":         `"x".padStart(2 ** 26).length`,
		"a shared buffer": `new SharedArrayBuffer(200 * 1024 * 1024).byteLength`,
		"caught":          `try { new ArrayBuffer(200 * 1024 * 1024) } catch (e) { "caught" }`,
		"arrays":          `let a = []; for (let i = 0; i < 1e5; i++) a.push(new Array(100).fill(i)); a.length`,
		// A BigInt result is reserved before it is computed, which for
		// these would take minutes past the limit (a fuzzer's program).
		"a squared BigInt": `let x = 5n; for (let i = 0; i < 40; i++) x = -x * x; x > 0n`,
		"a BigInt power":   `3n ** 400000000n > 0n`,
		"a shifted BigInt": `(1n << 800000000n) > 0n`,
	} {
		rt := quickjs.New(quickjs.WithMemoryLimit(limit))
		v, err := rt.Eval(src)
		if !errors.Is(err, quickjs.ErrMemoryLimit) {
			t.Errorf("%s: = %v, %v, want ErrMemoryLimit", name, v, err)
		}
		rt.Close()
	}
}

// TestMemoryLimitDoubling pins that the limit stops a value grown by
// doubling it in a short loop, by each way the engine can write out as much
// as an operation's inputs hold at once. Twenty-two turns are far too few
// for the interrupt check to measure the heap, so each of these is stopped
// by what the operation charges as it makes its result; the loop is short
// enough that one which is not stops at a few hundred megabytes, not at the
// machine's memory.
func TestMemoryLimitDoubling(t *testing.T) {
	const limit = 16 << 20
	loop := func(init, step, out string) string {
		return fmt.Sprintf(`function f() { %s; for (var i = 0; i < 22; i++) { %s } return %s } f().length`, init, step, out)
	}
	str := `var s = "ab".repeat(10)`
	arr := `var a = [1, 2, 3, 4]`
	for name, src := range map[string]string{
		"s += s":              loop(str, `s += s`, "s"),
		"o.s += o.s":          loop(`var o = {s: "ab".repeat(10)}`, `o.s += o.s`, "o.s"),
		"a[0] += a[0]":        loop(`var a = ["ab".repeat(10)]`, `a[0] += a[0]`, "a[0]"),
		"a template":          loop(str, "s = `${s}${s}`", "s"),
		"join":                loop(str, `s = [s, s].join("")`, "s"),
		"JSON.stringify":      loop(str, `s = JSON.stringify([s, s])`, "s"),
		"replaceAll":          loop(str, `s = s.replaceAll("a", "aa")`, "s"),
		"padEnd":              loop(str, `s = s.padEnd(s.length * 2, s)`, "s"),
		"an array's concat":   loop(arr, `a = a.concat(a)`, "a"),
		"an array spread":     loop(arr, `a = [...a, ...a]`, "a"),
		"a spread call":       loop(arr, `a.push(...a)`, "a"),
		"flat":                loop(arr, `a = [a, a].flat()`, "a"),
		"Array.from a length": `Array.from({length: 2e7}).length`,
		"fill":                `new Array(2e7).fill(0).length`,
		"a string's spread":   `[..."x".repeat(2e7)].length`,
	} {
		rt := quickjs.New(quickjs.WithMemoryLimit(limit))
		v, err := rt.Eval(src)
		if !errors.Is(err, quickjs.ErrMemoryLimit) {
			t.Errorf("%s: = %v, %v, want ErrMemoryLimit", name, v, err)
		}
		rt.Close()
	}
}

// TestMemoryLimitResultsCountedOnce pins that what a built-in returns is not
// held against the limit twice when the script already holds it -- the array
// sort returns, a string returned unchanged -- nor what it lets go of: a
// script using about half its limit, over and over, runs to its end.
func TestMemoryLimitResultsCountedOnce(t *testing.T) {
	// About 6 MB held, and at most about 3 MB more made at once.
	const src = `
		let a = new Array(2e5).fill(1), s = "x".repeat(3e6), n = 0;
		for (let i = 0; i < 40; i++) {
			n += a.sort().length + s.trim().length + s.toString().length;
			n += a.slice().length + [s, "y"].join("").length + [...a].length;
			n += JSON.stringify(a).length;
		}
		n`
	free := quickjs.New()
	want, err := free.Eval(src)
	free.Close()
	if err != nil {
		t.Fatal(err)
	}
	rt := quickjs.New(quickjs.WithMemoryLimit(16 << 20))
	defer rt.Close()
	if v, err := rt.Eval(src); err != nil || v.Float() != want.Float() {
		t.Errorf("= %v, %v, want %v", v, err, want)
	}
}

// TestMemoryLimitCountsWhatIsHeld pins that the limit is on what a script
// holds, not on what it has allocated: memory it lets go of is not counted,
// and a script that allocates far more than its limit over its life, a
// little at a time, runs to its end.
func TestMemoryLimitCountsWhatIsHeld(t *testing.T) {
	rt := quickjs.New(quickjs.WithMemoryLimit(16 << 20))
	defer rt.Close()
	v, err := rt.Eval(`
		let total = 0;
		for (let round = 0; round < 20; round++) {
			let a = [];
			for (let i = 0; i < 1000; i++) a.push("y".repeat(10000) + i);
			total += a.length;
		}
		let s = 0;
		for (let i = 0; i < 1e6; i++) { let o = {i, a: [1, 2, 3]}; s += o.a.length }
		total + s`)
	if err != nil || v.Int() != 20000+3e6 {
		t.Fatalf("= %v, %v", v, err)
	}
	// The same runtime is still usable, and the limit still holds.
	if _, err := rt.Eval(`new ArrayBuffer(64 << 20)`); !errors.Is(err, quickjs.ErrMemoryLimit) {
		t.Errorf("a buffer over the limit: %v", err)
	}
	if v, err := rt.Eval(`new ArrayBuffer(1 << 20).byteLength`); err != nil || v.Int() != 1<<20 {
		t.Errorf("a buffer within it: %v, %v", v, err)
	}
}

// TestNoMemoryLimit pins that a runtime without a limit is not measured.
func TestNoMemoryLimit(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if v, err := rt.Eval(`new ArrayBuffer(64 << 20).byteLength`); err != nil || v.Int() != 64<<20 {
		t.Errorf("= %v, %v", v, err)
	}
}

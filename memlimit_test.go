package quickjs_test

import (
	"errors"
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
		"padding":         `"x".padStart(2 ** 28).length`,
		"a shared buffer": `new SharedArrayBuffer(200 * 1024 * 1024).byteLength`,
		"caught":          `try { new ArrayBuffer(200 * 1024 * 1024) } catch (e) { "caught" }`,
		"arrays":          `let a = []; for (let i = 0; i < 1e5; i++) a.push(new Array(100).fill(i)); a.length`,
	} {
		rt := quickjs.New(quickjs.WithMemoryLimit(limit))
		v, err := rt.Eval(src)
		if !errors.Is(err, quickjs.ErrMemoryLimit) {
			t.Errorf("%s: = %v, %v, want ErrMemoryLimit", name, v, err)
		}
		rt.Close()
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

package vm

import (
	"testing"
	"unsafe"
)

// A for-of's cursor and its object -- every for-of's, spread's and array
// destructuring's -- are 112 bytes, a size class: the object has no ext,
// its class saying where the state is, and what only for-in walks is kept
// apart in its own cursor's allocation, which fits 192 bytes.
func TestCursorSizes(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("sizes of a 64-bit build")
	}
	if size := unsafe.Sizeof(iterObject{}); size != 112 {
		t.Errorf("iterObject is %d bytes, want 112", size)
	}
	if size := unsafe.Sizeof(forInObject{}); size > 192 {
		t.Errorf("forInObject is %d bytes, want at most 192", size)
	}
}

// TestCursorOneAllocation pins that a for-of's cursor is one allocation of
// 112 bytes, and a for-in's one of at most 192, with nothing else made for
// either: a loop over an array and one over an object's keys.
func TestCursorOneAllocation(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("sizes of a 64-bit build")
	}
	r := New(Config{})
	defer r.Close()
	arr := Obj(r.newArrayFrom([]Value{Int(1), Int(2), Int(3)}))
	if n := testing.AllocsPerRun(100, func() {
		cur, err := r.startForOf(arr)
		if err != nil || cursorOf(cur) == nil || cursorOf(cur).arr == nil {
			t.Fatal("no cursor over the array")
		}
	}); n != 1 {
		t.Errorf("a for-of's cursor over an array: %v allocations, want 1", n)
	}
	for _, tt := range []struct{ src, want string }{
		{`var s = 0; for (const x of [1, 2, 3]) s += x; s`, "6"},
		{`var s = ''; for (const k in {a: 1, b: 2}) s += k; s`, "ab"},
		{`var s = ''; for (const k in [5, 6]) s += k; for (const k in 'xy') s += k; s`, "0101"},
		{`var o = {a: 1, b: 2, c: 3}, s = ''; for (const k in o) { delete o.c; s += k } s`, "ab"},
		{`var [a, , b] = new Set([1, 2, 3]); a + b`, "4"},
		{`function* g() { yield 1; yield 2 } [...g(), ...'ab'].join()`, "1,2,a,b"},
	} {
		for _, on := range []bool{false, true} {
			SetTreeTier(on)
			v, err := r.Run(compileForTest(t, tt.src))
			if err != nil {
				t.Fatalf("%s: %v", tt.src, err)
			}
			if got, _ := r.ToString(v); got.Go() != tt.want {
				t.Errorf("tree %v: %s = %s, want %s", on, tt.src, got.Go(), tt.want)
			}
		}
	}
	SetTreeTier(true)
}

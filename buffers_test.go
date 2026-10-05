package quickjs_test

import (
	"errors"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestNewArrayBuffer pins what each mode makes of a Go slice: a copy the
// script and the host change apart; shared memory, which each sees the other
// write; and read-only shared memory, which the script cannot write, detach
// or transfer. Shared memory can be detached by the script, which leaves the
// slice alone, but not transferred to another agent.
func TestNewArrayBuffer(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	check, err := rt.Eval(`(buf) => {
		"use strict";
		const out = [buf.byteLength];
		const view = new Uint8Array(buf);
		try { view[0] = 9; out.push("wrote " + view[0]) } catch (e) { out.push(e.name) }
		return out.join(" ");
	}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		mode      quickjs.BufferMode
		want      string
		goSees    byte
		thenWrite string
	}{
		{quickjs.CopyMemory, "3 wrote 9", 1, "1"},
		{quickjs.ShareMemory, "3 wrote 9", 9, "7"},
		{quickjs.ShareMemoryReadOnly, "3 TypeError", 1, "7"},
	} {
		data := []byte{1, 2, 3}
		buf := rt.NewArrayBuffer(data, c.mode)
		got, err := check.Call(buf)
		if err != nil {
			t.Fatal(err)
		}
		if got.String() != c.want || data[0] != c.goSees {
			t.Errorf("mode %d: script %q, want %q; Go sees %d, want %d", c.mode, got, c.want, data[0], c.goSees)
		}
		// Transferred to another agent -- a worker, by postMessage -- a copy
		// moves; shared memory is refused, as it would be the host's memory
		// on another goroutine.
		_, err = rt.Serialize(buf, &quickjs.CloneOptions{Transfer: []quickjs.Value{buf}})
		if refused := err != nil; refused != (c.mode != quickjs.CopyMemory) {
			t.Errorf("mode %d: transfer to another agent: %v", c.mode, err)
		}
		// What the host writes afterwards, the script sees only of shared
		// memory -- the copy was transferred away above, and is detached.
		data[0] = 7
		if c.mode != quickjs.CopyMemory {
			v, err := rt.Eval(`(b) => String(new Uint8Array(b)[0])`)
			if err != nil {
				t.Fatal(err)
			}
			seen, err := v.Call(buf)
			if err != nil {
				t.Fatal(err)
			}
			if seen.String() != c.thenWrite {
				t.Errorf("mode %d: after the host wrote, the script reads %v", c.mode, seen)
			}
		}
	}

	// The script detaches shared memory by transferring it within the
	// runtime: the transfer is a copy, the slice stays as it was.
	data := []byte{4, 5}
	buf := rt.NewArrayBuffer(data, quickjs.ShareMemory)
	moved, err := rt.Eval(`(b) => { const t = b.transfer(); new Uint8Array(t)[0] = 6; return [b.detached, new Uint8Array(t)[0]].join(" ") }`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := moved.Call(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "true 6" || data[0] != 4 {
		t.Errorf("transfer: %v, slice %v", got, data)
	}
}

type celsius float64

// TestNewTypedArray pins the typed array each element type makes, its
// contents, and that shared elements are the slice's -- written by the
// script, read by the host -- where copied ones are not.
func TestNewTypedArray(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	describe, err := rt.Eval(`(a) => {
		const before = a.constructor.name + " " + a.length + " [" + Array.from(a).join(",") + "]";
		a[1] = typeof a[1] === "bigint" ? 40n : 2.5;
		return before;
	}`)
	if err != nil {
		t.Fatal(err)
	}
	run := func(v quickjs.Value, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		got, err := describe.Call(v)
		if err != nil {
			t.Fatal(err)
		}
		return got.String()
	}

	f64 := []float64{1.5, -2, 3}
	if got := run(quickjs.NewTypedArray(rt, f64, quickjs.ShareMemory)); got != "Float64Array 3 [1.5,-2,3]" || f64[1] != 2.5 {
		t.Errorf("float64 shared: %s, slice %v", got, f64)
	}
	f64 = []float64{1.5, -2, 3}
	if got := run(quickjs.NewTypedArray(rt, f64, quickjs.CopyMemory)); got != "Float64Array 3 [1.5,-2,3]" || f64[1] != -2 {
		t.Errorf("float64 copied: %s, slice %v", got, f64)
	}
	i64 := []int64{-1, 1 << 40}
	if got := run(quickjs.NewTypedArray(rt, i64, quickjs.ShareMemory)); got != "BigInt64Array 2 [-1,1099511627776]" || i64[1] != 40 {
		t.Errorf("int64 shared: %s, slice %v", got, i64)
	}
	for _, c := range []struct {
		got  string
		want string
	}{
		{run(quickjs.NewTypedArray(rt, []int8{-1, 2}, quickjs.CopyMemory)), "Int8Array 2 [-1,2]"},
		{run(quickjs.NewTypedArray(rt, []byte{255, 0}, quickjs.CopyMemory)), "Uint8Array 2 [255,0]"},
		{run(quickjs.NewTypedArray(rt, []int16{-300, 2}, quickjs.CopyMemory)), "Int16Array 2 [-300,2]"},
		{run(quickjs.NewTypedArray(rt, []uint16{65535, 2}, quickjs.CopyMemory)), "Uint16Array 2 [65535,2]"},
		{run(quickjs.NewTypedArray(rt, []int32{-70000, 2}, quickjs.CopyMemory)), "Int32Array 2 [-70000,2]"},
		{run(quickjs.NewTypedArray(rt, []uint32{4000000000, 2}, quickjs.CopyMemory)), "Uint32Array 2 [4000000000,2]"},
		{run(quickjs.NewTypedArray(rt, []uint64{1 << 63, 2}, quickjs.CopyMemory)), "BigUint64Array 2 [9223372036854775808,2]"},
		{run(quickjs.NewTypedArray(rt, []float32{0.5, 2}, quickjs.CopyMemory)), "Float32Array 2 [0.5,2]"},
		{run(quickjs.NewTypedArray(rt, []celsius{21.5, 2}, quickjs.CopyMemory)), "Float64Array 2 [21.5,2]"},
		{run(quickjs.NewTypedArray(rt, []float64(nil), quickjs.ShareMemory)), "Float64Array 0 []"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}

	// Read-only shared elements are not written.
	ro := []int32{1, 2}
	v, err := quickjs.NewTypedArray(rt, ro, quickjs.ShareMemoryReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	write, err := rt.Eval(`(a) => { "use strict"; try { a[0] = 5; return "wrote" } catch (e) { return e.name } }`)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := write.Call(v); got.String() != "TypeError" || ro[0] != 1 {
		t.Errorf("read-only: %v, slice %v", got, ro)
	}

	rt.Close()
	if _, err := quickjs.NewTypedArray(rt, []byte{1}, quickjs.CopyMemory); !errors.Is(err, quickjs.ErrClosed) {
		t.Errorf("closed: %v", err)
	}
}

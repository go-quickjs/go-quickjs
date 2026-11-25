//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"runtime"
	"testing"
	"weak"
)

// rehomeKeepSetup's dropped makes an array by a call before its loop,
// reads it, and drops it: entered natively at its start, the call is made
// from native code, which keeps its result in a keep cell of its context.
// carried does so too, and makes in the loop's middle a result the loop
// carries to its end, whose cell may be the one the dropped result had.
// numbers passes its call's result on to another and drops it, its loop
// one of numbers alone, whose polls have no register to write.
const rehomeKeepSetup = `
	function dropped(n) {
		let a = big(), s = a.length, alive = 'none'
		a = null
		for (let i = 0; i < n; i++) {
			s += i
			if (i === n - 1) alive = check()
		}
		return alive + ':' + s
	}
	function carried(n) {
		let a = big(), s = a.length, r = 'none'
		a = null
		for (let i = 0; i < n; i++) {
			s += i
			if (i === n >> 1) r = tag(i)
		}
		return r + ':' + s
	}
	function numbers(n) {
		consume(big())
		let s = 0
		for (let i = 0; i < n; i++) {
			s += i
			if (i === n - 1) note()
		}
		return noted() + ':' + s
	}`

// rehomeKeepRuntime is a runtime that has run rehomeKeepSetup, its JIT at
// the runtime's own thresholds, polling every 64 back edges and bringing
// the frame up to date at each such poll unless noRehome; check runs the
// collector and says whether big's last array is still alive, note does
// the same and keeps its answer for noted, whose call it leaves to come
// after the loop.
func rehomeKeepRuntime(t *testing.T, jit, noRehome bool) *Runtime {
	t.Helper()
	var made []weak.Pointer[Object]
	noted := "none"
	r := newTestRuntime(t, Config{JIT: jit})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	if jit {
		jitDefaultsForTest(r)
		r.jitState().stressBudget = 64
		r.jitTest.noRehome = noRehome
		r.jitTest.rehomeEvery = 1
	}
	r.global.setOwnRaw(r.atoms.intern("big"), Obj(r.newNativeFunc("big", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		a := rt.newArrayFrom(make([]Value, 1<<16))
		made = append(made, weak.Make(a))
		return Obj(a), nil
	})), propDefault)
	r.global.setOwnRaw(r.atoms.intern("tag"), Obj(r.newNativeFunc("tag", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return Str(NewString(fmt.Sprintf("tag%v", args[0].Number()))), nil
	})), propDefault)
	verdict := func() string {
		runtime.GC()
		runtime.GC()
		if made[len(made)-1].Value() != nil {
			return "kept"
		}
		return "collected"
	}
	r.global.setOwnRaw(r.atoms.intern("check"), Obj(r.newNativeFunc("check", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return Str(NewString(verdict())), nil
	})), propDefault)
	r.global.setOwnRaw(r.atoms.intern("note"), Obj(r.newNativeFunc("note", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		noted = verdict()
		return Undefined, nil
	})), propDefault)
	r.global.setOwnRaw(r.atoms.intern("noted"), Obj(r.newNativeFunc("noted", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return Str(NewString(noted)), nil
	})), propDefault)
	r.global.setOwnRaw(r.atoms.intern("consume"), Obj(r.newNativeFunc("consume", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return Undefined, nil
	})), propDefault)
	if _, err := r.Run(compileForTest(t, rehomeKeepSetup)); err != nil {
		t.Fatal(err)
	}
	return r
}

// A keep cell no value live at a loop's poll reads is emptied there, at
// the polls that bring the frame up to date (abi.Rehome's DeadKeeps): the
// result of a call native code made before the loop, dropped once read,
// had stayed in its keep cell -- the call's cell, which native code
// writes and nothing clears -- for as long as the loop ran natively. Here
// big's array, its result, is collected by the loop's end at every call
// that enters the code at its start, with the JIT as without it; with the
// polls writing nothing it stays alive. So too where the loop has only
// numbers, whose polls have no register to write: big's array, passed to
// consume and dropped, had stayed in big's call's cell to the loop's end.
// Each answer is the interpreter's, the collector idle and marking.
func TestJITRehomeEmptiesDeadKeepCells(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		name := "plain"
		if marking {
			name = "marking"
		}
		t.Run(name, func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			testRehomeEmptiesDeadKeepCells(t)
		})
	}
}

func testRehomeEmptiesDeadKeepCells(t *testing.T) {
	want := rehomeKeepRuntime(t, false, false)
	r := rehomeKeepRuntime(t, true, false)
	off := rehomeKeepRuntime(t, true, true)
	for _, fn := range []string{"dropped", "numbers"} {
		src := `''+` + fn + `(200000)`
		wv, err := want.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got := wv.String().Go(); got[:9] != "collected" {
			t.Fatalf("%s, interpreter: %s", fn, got)
		}
		keptOff := false
		for i := range 4 {
			gv, err := r.Run(compileForTest(t, src))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := gv.String().Go(), wv.String().Go(); i > 0 && got != want {
				t.Errorf("%s, call %d: got %s, interpreter %s", fn, i, got, want)
			}
			ov, err := off.Run(compileForTest(t, src))
			if err != nil {
				t.Fatal(err)
			}
			if got := ov.String().Go(); i > 0 && got[:4] == "kept" {
				keptOff = true
			}
		}
		if e := jitRegLifeForTest(r, fn); e == nil || e.starts == 0 {
			t.Fatalf("%s's code was never entered at its start: %+v", fn, e)
		}
		if !keptOff {
			t.Errorf("%s: with the polls writing nothing, big's array was collected all the same: the test sees no keep cell", fn)
		}
	}
	// A result the loop carries past the polls after it is made, in a cell
	// the dropped result may have had, is never emptied: read after the
	// loop, it is the interpreter's.
	const csrc = `''+carried(200000)`
	cw, err := want.Run(compileForTest(t, csrc))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		cv, err := r.Run(compileForTest(t, csrc))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := cv.String().Go(), cw.String().Go(); got != want {
			t.Errorf("carried, call %d: got %s, interpreter %s", i, got, want)
		}
	}
}

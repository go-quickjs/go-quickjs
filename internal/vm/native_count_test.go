//go:build amd64 || arm64

package vm

import (
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// nativeCountSrc's loop exits to Go at each turn's delete, which no native
// handler makes, and runs the rest natively.
const nativeCountSrc = `
var o = { x: 1 };
function f(n) { var s = 0; for (var i = 0; i < n; i++) { delete o.x; o.y = i; s += o.y } return s }
`

// A runtime's native code counts into the counters of the test that made
// it (nativeCounting, Runtime's nativeCounts), and only there: a runtime
// running native code on another goroutine meanwhile, made before, adds
// nothing to another test's count. Every runtime had counted into one
// array of the process's, and one exit of 1,728 a test counted had once
// been another's.
func TestRegNativeCountsPerRuntime(t *testing.T) {
	if verifyShapes {
		t.Skip("the shape checks are the Go handlers' alone")
	}
	// Native whatever QJS_NONATIVE says, as nativeRunSrc runs it.
	was := regNativeOn
	regNativeOn = true
	defer func() { regNativeOn = was }()
	prog := compileForTest(t, nativeCountSrc+"String(f(1000))")
	run := func(r *Runtime) {
		t.Helper()
		if v, err := r.Run(prog); err != nil || v.String().Go() != "499500" {
			t.Fatalf("got %v, %v; want 499500", v, err)
		}
	}
	// Alone: what the runtime counts.
	nativeCounting(t)
	alone := New(Config{})
	defer alone.Close()
	run(alone)
	want := nativeCounter.Load().exits
	if want[bytecode.RDelete] < 1000 {
		t.Fatalf("delete exited %d times, want 1000 or more", want[bytecode.RDelete])
	}
	// Another runtime, counting into counters of its own, runs the loop on
	// another goroutine until told to stop.
	nativeCounting(t)
	other := nativeCounter.Load()
	busy := New(Config{})
	defer busy.Close()
	if _, err := busy.Run(compileForTest(t, nativeCountSrc)); err != nil {
		t.Fatal(err)
	}
	fn, _ := busy.GetProp(Obj(busy.Global()), busy.Intern("f"))
	var stop atomic.Bool
	done := make(chan error, 1)
	go func() {
		for !stop.Load() {
			if _, err := busy.Call(fn, Undefined, []Value{Float(100)}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for atomic.LoadInt64(&other.exits[bytecode.RDelete]) == 0 {
		runtime.Gosched()
	}
	// The counting runtime, made now, while the other runs.
	nativeCounting(t)
	mine := nativeCounter.Load()
	r := New(Config{})
	defer r.Close()
	from := atomic.LoadInt64(&other.exits[bytecode.RDelete])
	run(r)
	during := atomic.LoadInt64(&other.exits[bytecode.RDelete]) - from
	stop.Store(true)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if during == 0 {
		t.Log("the other runtime ran no delete during the count")
	}
	for op := range want {
		if got := atomic.LoadInt64(&mine.exits[op]); got != want[op] {
			t.Errorf("%v: %d exits counted beside another runtime's native code, %d alone", bytecode.ROp(op), got, want[op])
		}
	}
}

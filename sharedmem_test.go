package quickjs_test

import (
	"sync"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/hostjobs"
	"github.com/go-quickjs/go-quickjs/internal/sharedmem"
)

// sharedPair makes two runtimes that share the SharedArrayBuffer the first
// one makes from src, each holding it in a global named sab.
func sharedPair(t *testing.T, src string) (*quickjs.Runtime, *quickjs.Runtime) {
	t.Helper()
	a, b := quickjs.New(), quickjs.New()
	t.Cleanup(func() { a.Close(); b.Close() })
	sab, err := a.Eval(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Set("sab", sab); err != nil {
		t.Fatal(err)
	}
	mem, ok, err := sharedmem.Share(sab)
	if err != nil || !ok {
		t.Fatalf("share: %v %v", ok, err)
	}
	attached, err := sharedmem.Attach(b, mem)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Set("sab", attached); err != nil {
		t.Fatal(err)
	}
	return a, b
}

// TestSharedMemoryWaitNotify pins that an agent waiting in Atomics.wait is
// woken by another agent's Atomics.notify, on another goroutine.
func TestSharedMemoryWaitNotify(t *testing.T) {
	a, b := sharedPair(t, "new SharedArrayBuffer(8)")
	result := make(chan string, 1)
	go func() {
		v, err := b.Eval("Atomics.store(new Int32Array(sab), 1, 1); Atomics.wait(new Int32Array(sab), 0, 0, 10000)")
		if err != nil {
			result <- err.Error()
			return
		}
		result <- v.String()
	}()
	// Once the other agent has said it is about to wait, notify until it is
	// woken: it may not be waiting yet the first time.
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, err := a.Eval("Atomics.load(new Int32Array(sab), 1) === 1 ? Atomics.notify(new Int32Array(sab), 0) : 0")
		if err != nil {
			t.Fatal(err)
		}
		if v.Int() == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := <-result; got != "ok" {
		t.Errorf("Atomics.wait = %q, want ok", got)
	}
	// A wait whose value is already different returns at once, and one that
	// nobody wakes times out.
	v, err := b.Eval("[Atomics.wait(new Int32Array(sab), 1, 0), Atomics.wait(new Int32Array(sab), 0, 0, 10)].join()")
	if err != nil || v.String() != "not-equal,timed-out" {
		t.Errorf("= %v, %v", v, err)
	}
}

// TestSharedMemoryAtomicity pins that Atomics operations from two agents at
// once lose nothing, for every width -- a byte or a half-word is updated
// within the word it is part of.
func TestSharedMemoryAtomicity(t *testing.T) {
	a, b := sharedPair(t, "new SharedArrayBuffer(32)")
	src := `
		const i8 = new Int8Array(sab, 0, 4), i16 = new Int16Array(sab, 4, 2);
		const i32 = new Int32Array(sab, 8, 2), i64 = new BigInt64Array(sab, 16, 2);
		for (let k = 0; k < 20000; k++) {
			Atomics.add(i8, 1, 1); Atomics.add(i16, 1, 1); Atomics.add(i32, 1, 1); Atomics.add(i64, 1, 1n);
			Atomics.xor(i8, 2, 1);
		}`
	var wg sync.WaitGroup
	for _, rt := range []*quickjs.Runtime{a, b} {
		wg.Add(1)
		go func(rt *quickjs.Runtime) {
			defer wg.Done()
			if _, err := rt.Eval(src); err != nil {
				t.Error(err)
			}
		}(rt)
	}
	wg.Wait()
	v, err := a.Eval(`[Atomics.load(new Int8Array(sab), 1), Atomics.load(new Int16Array(sab, 4), 1),
		Atomics.load(new Int32Array(sab, 8), 1), Atomics.load(new BigInt64Array(sab, 16), 1),
		Atomics.load(new Int8Array(sab), 2), Atomics.load(new Int8Array(sab), 0), Atomics.load(new Int8Array(sab), 3)].join()`)
	if err != nil {
		t.Fatal(err)
	}
	// 40000 wraps in an Int8 and an Int16; the neighbours of each element are
	// untouched, and xor-ing 1 an even number of times leaves 0.
	want := "64,-25536,40000,40000,0,0,0"
	if v.String() != want {
		t.Errorf("= %s, want %s", v, want)
	}
}

// TestSharedMemoryGrowth pins that a growable buffer shared between agents
// grows in place, and that the growth one agent makes the other sees.
func TestSharedMemoryGrowth(t *testing.T) {
	a, b := sharedPair(t, "new SharedArrayBuffer(4, { maxByteLength: 64 })")
	if _, err := a.Eval("Atomics.store(new Int32Array(sab), 0, 7); sab.grow(32); Atomics.store(new Int32Array(sab), 7, 9)"); err != nil {
		t.Fatal(err)
	}
	v, err := b.Eval("[sab.byteLength, sab.growable, sab.maxByteLength, Atomics.load(new Int32Array(sab), 0), Atomics.load(new Int32Array(sab), 7), new Int32Array(sab).length].join()")
	if err != nil || v.String() != "32,true,64,7,9,8" {
		t.Errorf("= %v, %v", v, err)
	}
	// One whose maximum could never be reserved cannot be shared.
	rt := quickjs.New()
	defer rt.Close()
	big, err := rt.Eval("new SharedArrayBuffer(0, { maxByteLength: 2 ** 31 })")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := sharedmem.Share(big); err == nil {
		t.Error("a buffer with a 2 GiB maximum was shared")
	}
}

// runHostJobs waits for work another goroutine finished for rt, and runs it.
func runHostJobs(t *testing.T, rt *quickjs.Runtime) {
	t.Helper()
	select {
	case <-hostjobs.Ready(rt):
	case <-time.After(10 * time.Second):
		t.Fatal("nothing arrived")
	}
	if err := rt.RunJobs(); err != nil {
		t.Fatal(err)
	}
}

// TestWaitAsync pins Atomics.waitAsync: an answer at once where there is
// nothing to wait for, and otherwise a promise that the time running out, or
// another agent's notify, settles on the waiting runtime's goroutine.
func TestWaitAsync(t *testing.T) {
	a, b := sharedPair(t, "new SharedArrayBuffer(8)")
	v, err := a.Eval(`const i = new Int32Array(sab);
		const r = [Atomics.waitAsync(i, 0, 1), Atomics.waitAsync(i, 0, 0, 0)];
		JSON.stringify(r) + "," + Object.keys(r[0]).join()`)
	if err != nil || v.String() != `[{"async":false,"value":"not-equal"},{"async":false,"value":"timed-out"}],async,value` {
		t.Fatalf("= %v, %v", v, err)
	}

	// Out of time.
	if _, err := a.Eval(`var outcome; const w = Atomics.waitAsync(i, 0, 0, 20);
		w.value.then(v => { outcome = v }); if (!w.async) throw 0;`); err != nil {
		t.Fatal(err)
	}
	runHostJobs(t, a)
	if v, _ := a.Eval("outcome"); v.String() != "timed-out" {
		t.Errorf("outcome = %v", v)
	}

	// Woken by another agent, which counts the waiter as it counts one that
	// blocks.
	if _, err := a.Eval(`outcome = undefined; Atomics.waitAsync(i, 0, 0).value.then(v => { outcome = v })`); err != nil {
		t.Fatal(err)
	}
	if v, err := b.Eval("Atomics.notify(new Int32Array(sab), 0)"); err != nil || v.Int() != 1 {
		t.Fatalf("notify = %v, %v", v, err)
	}
	runHostJobs(t, a)
	if v, _ := a.Eval("outcome"); v.String() != "ok" {
		t.Errorf("outcome = %v", v)
	}
}

// TestWaitPastSharedLength pins that an agent may wait on memory a growable
// SharedArrayBuffer gained after it was shared: wait and waitAsync read the
// memory at the length it had when shared, and panicked past it, which
// closed the runtime (KI-11).
func TestWaitPastSharedLength(t *testing.T) {
	a, _ := sharedPair(t, "new SharedArrayBuffer(4, { maxByteLength: 64 })")
	v, err := a.Eval(`sab.grow(16);
		const i = new Int32Array(sab);
		[Atomics.wait(i, 2, 0, 1), Atomics.wait(i, 3, 5, 1), Atomics.waitAsync(i, 2, 0, 0).value].join()`)
	if err != nil || v.String() != "timed-out,not-equal,timed-out" {
		t.Errorf("= %v, %v", v, err)
	}
}

package quickjs_test

import (
	"context"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

func TestSharedArrayBuffer(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		{`var b = new SharedArrayBuffer(4); [b.byteLength, b.growable, b.maxByteLength,
		   Object.prototype.toString.call(b), b.slice(1).byteLength, b.slice(1) instanceof SharedArrayBuffer].join()`,
			"4,false,4,[object SharedArrayBuffer],3,true"},
		// A growable one only grows.
		{`var b = new SharedArrayBuffer(2, {maxByteLength: 8}); var ta = new Uint8Array(b); b.grow(6);
		  var r = [b.growable, b.byteLength, ta.length];
		  try { b.grow(4) } catch (e) { r.push(e.constructor.name) }
		  r.join()`, "true,6,6,RangeError"},
		// The two kinds of buffer refuse each other's methods.
		{`var errs = [];
		  for (var f of [() => ArrayBuffer.prototype.slice.call(new SharedArrayBuffer(1)),
		                 () => Object.getOwnPropertyDescriptor(SharedArrayBuffer.prototype, "byteLength").get.call(new ArrayBuffer(1)),
		                 () => new SharedArrayBuffer(1).transfer()]) {
		    try { f() } catch (e) { errs.push(e.constructor.name) }
		  }
		  errs.join()`, "TypeError,TypeError,TypeError"},
		// A view of a fixed length over a growable buffer can be frozen, since
		// the buffer never shrinks away from it.
		{`var b = new SharedArrayBuffer(2, {maxByteLength: 8});
		  var r = []; for (var ta of [new Uint8Array(b, 0, 0), new Uint8Array(b)]) {
		    try { Object.freeze(ta); r.push("ok") } catch (e) { r.push(e.constructor.name) }
		  }
		  r.join()`, "ok,TypeError"},
	})
}

func TestAtomics(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		{`var ta = new Int32Array(new SharedArrayBuffer(8));
		  [Atomics.store(ta, 0, 5), Atomics.add(ta, 0, 2), Atomics.sub(ta, 0, 1), Atomics.and(ta, 0, 3),
		   Atomics.or(ta, 0, 8), Atomics.xor(ta, 0, 1), Atomics.exchange(ta, 0, 42), Atomics.load(ta, 0)].join()`,
			"5,5,7,6,2,10,11,42"},
		{`var ta = new Uint8Array(4); [Atomics.compareExchange(ta, 0, 256, 7), ta[0],
		   Atomics.compareExchange(ta, 0, 1, 9), ta[0]].join()`, "0,7,7,7"},
		// store answers with the value as converted rather than as stored.
		{`var ta = new Uint8Array(1); [Atomics.store(ta, 0, 300.7), ta[0], Object.is(Atomics.store(ta, 0, -0), 0)].join()`,
			"300,44,true"},
		{`var ta = new BigInt64Array(1); [Atomics.add(ta, 0, 5n), Atomics.load(ta, 0)].join()`, "0,5"},
		{`var errs = [];
		  for (var f of [() => Atomics.add(new Float64Array(1), 0, 1), () => Atomics.add(new Uint8ClampedArray(1), 0, 1),
		                 () => Atomics.load(new Int32Array(1), 1), () => Atomics.wait(new Int32Array(1), 0, 0, 0),
		                 () => Atomics.notify(new Uint8Array(1), 0), () => Atomics.pause(1.5)]) {
		    try { f() } catch (e) { errs.push(e.constructor.name) }
		  }
		  errs.join()`, "TypeError,TypeError,RangeError,TypeError,TypeError,TypeError"},
		{`var ta = new Int32Array(new SharedArrayBuffer(4));
		  [Atomics.wait(ta, 0, 1), Atomics.wait(ta, 0, 0, 0), Atomics.notify(ta, 0), Atomics.isLockFree(4),
		   Atomics.isLockFree(3), Atomics.pause(), Object.prototype.toString.call(Atomics)].join()`,
			"not-equal,timed-out,0,true,false,,[object Atomics]"},
	})
}

// TestAtomicsWaitHonorsContext pins that a wait nothing can notify still ends
// when the host's context does.
func TestAtomicsWaitHonorsContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	rt := quickjs.New()
	defer rt.Close()
	start := time.Now()
	_, err := rt.EvalContext(ctx, `Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0)`)
	if err == nil {
		t.Fatal("a wait with no timeout and no one to notify it returned")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the wait outlived its context by %v", time.Since(start))
	}
}

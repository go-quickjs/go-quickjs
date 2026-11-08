package vm

import (
	"testing"
	"unsafe"
)

// A typed array's view keeps a copy of where its elements are and how many
// there are (typedArrayData's data and length), which native code reads,
// holding while its epoch is the runtime's typedEpoch: everything that
// moves a buffer's bytes or changes their length or detaches them advances
// that, so that no copy of an epoch still current is stale. Each step
// below changes a buffer -- shrinks and grows a resizable one within its
// capacity and past it, transfers one as it is, shorter and longer, to a
// fixed length, detaches one by the host's call and by a transfer in a
// clone, grows a shared one, and shares one, which moves its memory --
// and every view whose copy is of the epoch after the step must have the
// copy its buffer gives; every view's copy is then made anew for the next
// step to test.
func TestTypedViewCache(t *testing.T) {
	r := New(Config{})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	run := func(src string) Value {
		t.Helper()
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		return v
	}
	run(`
	var views = []
	function track(...vs) { for (const v of vs) views.push(v); return vs[0] }
	var rab = new ArrayBuffer(16, { maxByteLength: 64 })
	track(new Int32Array(rab), new Uint8Array(rab, 4), new Int16Array(rab, 2, 4), new Float64Array(rab, 8, 1), new Uint8Array(rab, 16))
	var plain = new Uint8Array(32); track(plain, plain.subarray(4, 9), new Float32Array(plain.buffer, 8, 3))
	var fixed = new ArrayBuffer(24); track(new Uint16Array(fixed), new BigInt64Array(fixed, 8))
	var other = new ArrayBuffer(12); track(new Int8Array(other))
	var imm = new Uint8Array([1, 2, 3]).buffer.transferToImmutable(); track(new Uint8Array(imm), new Int8Array(imm, 1))
	var sab = new SharedArrayBuffer(8, { maxByteLength: 64 }); track(new Int32Array(sab), new Uint8Array(sab, 0, 4), new Uint8Array(sab, 6))
	var sab2 = new SharedArrayBuffer(16); track(new Int16Array(sab2))
	var sab3 = new SharedArrayBuffer(8, { maxByteLength: 64 }); track(new Int32Array(sab3), new Uint8Array(sab3, 2, 4))
	`)
	views := run("views").Object()
	check := func(step string) {
		t.Helper()
		// The elements read before the loop, which the loop needs
		// (go run ./internal/cmd/hoistcheck).
		els := views.elemsOf()
		for i := range els {
			v := els[i].Object().dataOf().(*typedArrayData)
			if v.epoch == r.typedEpoch {
				b := v.storage()
				count := v.countIn(b)
				if v.length != count && !(v.noLength && v.length < count) {
					t.Errorf("%s: view %d has a copy of %d elements, its buffer %d", step, i, v.length, count)
				}
				var want unsafe.Pointer
				if v.length > 0 {
					want = unsafe.Pointer(&b.bytes[v.byteOffset])
				}
				if v.data != want {
					t.Errorf("%s: view %d's copy has its elements at %p, its buffer at %p", step, i, v.data, want)
				}
				if v.readonly != b.immutable {
					t.Errorf("%s: view %d's copy is readonly %v, its buffer immutable %v", step, i, v.readonly, b.immutable)
				}
			}
			r.cacheView(v)
		}
	}
	check("made")
	steps := []string{
		"rab.resize(6)",
		"rab.resize(12)",
		"rab.resize(48)",
		"rab.resize(16)",
		"rab.resize(64)",
		"plain[0] = 1; new Uint8Array(rab)[1] = 2",
		"rab = rab.transfer(64); track(new Uint8Array(rab))",
		"rab = rab.transfer(); track(new Uint8Array(rab), new Int32Array(rab, 4))",
		"fixed = fixed.transfer(40); track(new Uint16Array(fixed), new Uint32Array(fixed, 4, 2))",
		"fixed = fixed.transfer(8); track(new Uint8Array(fixed))",
		"fixed = fixed.transferToFixedLength(16); track(new Uint8Array(fixed))",
		"sab.grow(40)",
		"sab.grow(64)",
		"track(new Uint8Array(sab), new Int8Array(sab, 60))",
	}
	for _, s := range steps {
		run(s)
		check(s)
	}
	// The host's detach, a clone's transfer, a share.
	r.DetachArrayBuffer(run("other"))
	check("DetachArrayBuffer")
	if _, err := r.Serialize(run("plain"), []Value{run("plain.buffer")}, nil); err != nil {
		t.Fatal(err)
	}
	check("Serialize with a transfer")
	if _, _, err := r.SharedMemoryOf(run("sab")); err != nil {
		t.Fatal(err)
	}
	check("SharedMemoryOf")
	// Sharing a growable buffer reserves its maximum, which moves it.
	if _, _, err := r.SharedMemoryOf(run("sab3")); err != nil {
		t.Fatal(err)
	}
	check("SharedMemoryOf of a buffer below its maximum")
	run("sab.grow(64)")
	check("sab.grow")
	if got := run("views.map(v => v.length).join()").String().Go(); got == "" {
		t.Fatal("no views")
	}
}

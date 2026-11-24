package vm

import "testing"

// TestTypedArrayFromEndlessIterable pins that an iterable a typed array is
// made from, which is not asked how long it is, is refused by the count of
// what it has given -- at 2 GiB of values, as large as a buffer may be, on
// a 64-bit platform and 256 MiB on a 32-bit one -- in a runtime without a memory limit too, rather than taking the process
// down (KI-98). Under a limit the limit refuses it sooner
// (TestTypedArrayIterableSources, in the root package).
func TestTypedArrayFromEndlessIterable(t *testing.T) {
	if testing.Short() {
		t.Skip("the refusal only arrives after 2 GiB of values")
	}
	// 2 GiB of values, and the 1.6 GiB they were grown from; on a 32-bit
	// platform 256 MiB of them, and the slices they were grown through.
	heavyAlone(t)
	const tooLarge = "RangeError: the typed array length is too large"
	for _, src := range []string{
		`new Uint8Array(endless)`,
		`Uint8Array.from(endless)`,
	} {
		r := newTestRuntime(t, Config{})
		v, err := r.Run(compileForTest(t, `
			var step = {value: 0, done: false}, closed = 0
			var endless = {[Symbol.iterator]: () => ({next: () => step, return() { closed++; return {} }})}
			try { `+src+`; "made" } catch (e) { e.name + ": " + e.message + ", closed " + closed }`))
		r.Close()
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if got, want := v.String().Go(), tooLarge+", closed 1"; got != want {
			t.Errorf("%s = %s, want %s", src, got, want)
		}
	}
}

//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"math"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// A native caller's level that Go makes the frame of, its callee's exit
// unwinding it (jitUnwindNative), makes the objects its code did not make
// from its own slots alone (jitMaterializeBelow): past its operands is its
// native callee's frame, the exit's words in it naming the callee's
// objects by the callee's homes. Read as the caller's, they had been made
// of the caller's homes -- objects whose shape was another object's field
// -- and written over the callee's slots, which the callee read at its
// next property operation: a fault under the JIT's stress, in RayTrace's
// Color.addScalar, called natively from rayTrace. Here a word inside the
// frame is made of the frame's home; one past its limit is left as it is.
func TestJITMaterializeStopsAtCalleeFrame(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := jitRuntimeForTest(t, Config{JIT: true})
	if _, err := r.Run(compileForTest(t, `function h(a, b) { return a }`)); err != nil {
		t.Fatal(err)
	}
	v, err := r.Run(compileForTest(t, `({x: 1})`))
	if err != nil || !v.IsObject() {
		t.Fatal(v, err)
	}
	model := v.Object()
	r.ensureShape(model)
	cl := jitGlobalClosureForTest(r, "h")
	n := cl.fn.LocalCount
	base := r.stackTop + n
	if base+64 > len(r.stack) {
		t.Skip("VM stack too short")
	}
	f := &frame{cl: cl, base: base, locals: r.stack[base-n : base : base]}
	e := &jitEntry{ssaHomes: 40}
	word := func(w uint64) Value { return Value{num: math.Float64frombits(w)} }
	ptr := func(p unsafe.Pointer) Value { return word(uint64(uintptr(p))) }
	// The frame's home, at slot n+20, of an object of no property: its
	// shape and its prototype.
	home := n + 20
	r.stack[base+20], r.stack[base+21] = ptr(unsafe.Pointer(model.shape)), ptr(unsafe.Pointer(model.proto))
	w := abi.VirtualWord(home, 0)
	r.stack[base+1], r.stack[base+30] = word(w), word(w)
	r.jitMaterializeBelow(f, e, base+25)
	if in := r.stack[base+1]; !in.IsObject() || in.Object().shape != model.shape || in.Object().proto != model.proto {
		t.Errorf("the frame's word was not made of its home: %#v", in)
	}
	if out := r.stack[base+30]; out.ref != nil || math.Float64bits(out.num) != w {
		t.Errorf("the callee's word, past the frame, was made of the caller's home: %#v", out)
	}
	clear(r.stack[base-n : base+40])
}

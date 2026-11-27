//go:build quickjs_wasm

package wasmjs

import (
	"testing"
	"unsafe"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/hostaccess"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// newHas returns a walk's has: whether a pointer was counted, marking it.
func newHas() func(unsafe.Pointer) bool {
	seen := map[unsafe.Pointer]bool{}
	return func(p unsafe.Pointer) bool {
		if seen[p] {
			return true
		}
		seen[p] = true
		return false
	}
}

// TestModuleFootprintCountsCode pins that the register code a module's
// functions are compiled to is counted in the footprint of what reaches
// it, once however many objects stand for the compiled module: a Module,
// its clone in the same runtime and two instances of it.
func TestModuleFootprintCountsCode(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if err := Install(rt); err != nil {
		t.Fatal(err)
	}
	// A function passing its argument through another 300 times.
	body := []byte{0}
	for range 300 {
		body = append(body, 0x20, 0, 0x10, 0, 0x21, 0)
	}
	body = append(body, 0x20, 0, 0x0b)
	code := []byte{2, 4, 0, 0x20, 0, 0x0b, byte(len(body) | 0x80), byte(len(body) >> 7)}
	code = append(code, body...)
	b := []byte{0, 'a', 's', 'm', 1, 0, 0, 0, 1, 6, 1, 0x60, 1, 0x7f, 1, 0x7f, 3, 3, 2, 0, 0,
		7, 7, 1, 3, 'r', 'u', 'n', 0, 1, 10, byte(len(code) | 0x80), byte(len(code) >> 7)}
	b = append(b, code...)
	if err := rt.Global().Set("bytes", rt.NewArrayBuffer(b, quickjs.CopyMemory)); err != nil {
		t.Fatal(err)
	}
	v, err := rt.Eval(`const m = new WebAssembly.Module(bytes);
		globalThis.objs = [m, new WebAssembly.Instance(m), new WebAssembly.Instance(m)]`)
	if err != nil {
		t.Fatal(err)
	}
	mod, err := v.Index(0)
	if err != nil {
		t.Fatal(err)
	}
	clone, err := rt.Serialize(mod, nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err := rt.Deserialize(clone, nil)
	if err != nil {
		t.Fatal(err)
	}
	data := func(arr quickjs.Value, i int) vm.HostSized {
		e, err := arr.Index(i)
		if err != nil {
			t.Fatal(err)
		}
		return vm.HostData(hostaccess.Unwrap(e)).(vm.HostSized)
	}
	md := data(v, 0).(*moduleData)
	cloned := vm.HostData(hostaccess.Unwrap(again)).(vm.HostSized)
	if cloned != vm.HostSized(md) {
		t.Fatal("the clone is of another compiled module")
	}
	objs := []vm.HostSized{md, cloned, data(v, 1), data(v, 2)}
	total := func(has func(unsafe.Pointer) bool) int64 {
		var n int64
		for _, o := range objs {
			n += o.MemoryFootprint(has)
		}
		return n
	}
	before := md.code.MemoryFootprint(newHas())
	if res, err := rt.Eval(`objs[1].exports.run(3)`); err != nil || res.String() != "3" {
		t.Fatalf("run(3) = %v, %v", res, err)
	}
	codeBytes := md.code.MemoryFootprint(newHas())
	if codeBytes-before < int64(len(b)) {
		t.Fatalf("running a module of %d bytes compiled %d bytes of code", len(b), codeBytes-before)
	}
	without := newHas()
	without(unsafe.Pointer(md.code))
	if got := total(newHas()) - total(without); got != codeBytes {
		t.Fatalf("a module, its clone and two instances count %d bytes of code, want %d once", got, codeBytes)
	}
}

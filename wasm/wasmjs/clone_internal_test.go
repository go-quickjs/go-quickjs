//go:build quickjs_wasm

package wasmjs

import (
	"errors"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/hostaccess"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// TestCloneSharesCompiledModule pins that a Module cloned into another
// runtime is the same compiled module, not compiled again, and that a shared
// Memory cloned there is the same memory; and that a runtime without the
// namespace refuses both.
func TestCloneSharesCompiledModule(t *testing.T) {
	from, to, bare := quickjs.New(), quickjs.New(), quickjs.New()
	for _, rt := range []*quickjs.Runtime{from, to, bare} {
		defer rt.Close()
	}
	for _, rt := range []*quickjs.Runtime{from, to} {
		if err := Install(rt); err != nil {
			t.Fatal(err)
		}
	}
	v, err := from.Eval(`[new WebAssembly.Module(new Uint8Array([0,97,115,109,1,0,0,0])),
		new WebAssembly.Memory({initial: 1, maximum: 2, shared: true})]`)
	if err != nil {
		t.Fatal(err)
	}
	s, err := from.Serialize(v, nil)
	if err != nil {
		t.Fatal(err)
	}
	again := s.Copy()
	got, err := to.Deserialize(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	data := func(arr quickjs.Value, i int) any {
		e, err := arr.Index(i)
		if err != nil {
			t.Fatal(err)
		}
		return vm.HostData(hostaccess.Unwrap(e))
	}
	if a, b := data(v, 0).(*moduleData), data(got, 0).(*moduleData); a == nil || a != b || a.code != b.code {
		t.Errorf("the clone's module is %p, the original's %p", b, a)
	}
	if a, b := data(v, 1).(*memoryData), data(got, 1).(*memoryData); a == nil || b == nil || a == b || a.mem != b.mem {
		t.Errorf("the clone's memory is %+v, the original's %+v", b, a)
	}
	_, err = bare.Deserialize(again, nil)
	var dce *quickjs.DataCloneError
	if !errors.As(err, &dce) || dce.Message != "Unable to deserialize cloned data." {
		t.Errorf("deserializing without the namespace: %v", err)
	}
}

// TestCloneBeforeGlobalRead pins that a Module cloned into a runtime whose
// script has not read the global WebAssembly is made by its namespace, which
// the global then is.
func TestCloneBeforeGlobalRead(t *testing.T) {
	from, to := quickjs.New(), quickjs.New()
	defer from.Close()
	defer to.Close()
	for _, rt := range []*quickjs.Runtime{from, to} {
		if err := Install(rt); err != nil {
			t.Fatal(err)
		}
	}
	v, err := from.Eval(`new WebAssembly.Module(new Uint8Array([0,97,115,109,1,0,0,0]))`)
	if err != nil {
		t.Fatal(err)
	}
	s, err := from.Serialize(v, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := to.Deserialize(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := to.Set("m", got); err != nil {
		t.Fatal(err)
	}
	res, err := to.Eval(`m instanceof WebAssembly.Module && new WebAssembly.Instance(m) instanceof WebAssembly.Instance`)
	if err != nil || !res.Bool() {
		t.Errorf("got %v, %v", res, err)
	}
}

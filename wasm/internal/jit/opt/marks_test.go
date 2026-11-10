//go:build amd64 || arm64

package opt

import (
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

// compileWat decodes a module in the text format for the tests.
func compileWat(t *testing.T, src string) *module.Module {
	t.Helper()
	bin, err := wat.Compile([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	lim := decoder.CoreLimits()
	m, derr := decoder.Decode(bin, lim)
	if derr != nil {
		t.Fatal(derr)
	}
	if derr := decoder.ValidateFunctions(m, lim); derr != nil {
		t.Fatal(derr)
	}
	return m
}

// testRequest is a request for function fn of m with a roomy frame.
func testRequest(m *module.Module, fn uint32) *wjit.Request {
	return &wjit.Request{Module: m, Func: fn, FrameSlots: 256, FrameSlotsOf: func(uint32) int { return 256 },
		RefSlotsOf: func(uint32) int { return 0 }}
}

// TestMarks checks the marks and the listing a request asks for: in order
// of their offsets, within the code, naming the blocks, values, edges and
// polls of a loop, and the listing each value's location.
func TestMarks(t *testing.T) {
	m := compileWat(t, `(module
  (memory 1)
  (func (export "run") (param $n i32) (result i32)
    (local $i i32) (local $acc i32)
    (loop $l
      (local.set $acc (i32.add (local.get $acc) (i32.load (local.get $i))))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 4))) (local.get $n))))
    (local.get $acc)))`)
	req := testRequest(m, 0)
	code, err := Compile(req)
	if err != nil {
		t.Fatal(err)
	}
	if code.Marks != nil || code.Listing != "" {
		t.Fatal("marks made unasked")
	}
	req.Marks = true
	code, err = Compile(req)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	last := -1
	for _, mk := range code.Marks {
		if mk.Off < last || mk.Off > len(code.Bytes) {
			t.Fatalf("mark %q at %d after %d, code of %d bytes", mk.Tag, mk.Off, last, len(code.Bytes))
		}
		last = mk.Off
		f := strings.Fields(mk.Tag)
		switch {
		case f[0] == "poll" || f[0] == "edge" || f[0] == "entry" || f[0] == "cold":
			seen[f[0]] = true
		case len(f) >= 2 && f[0][0] == 'v':
			seen[f[1]] = true
		}
	}
	for _, want := range []string{"entry", "poll", "edge", "cold", "load", "add"} {
		if !seen[want] {
			t.Errorf("no mark of %q in %v", want, code.Marks)
		}
	}
	if !strings.Contains(code.Listing, "load i32") || !strings.Contains(code.Listing, "  ; ") {
		t.Errorf("listing without the values' locations:\n%s", code.Listing)
	}
}

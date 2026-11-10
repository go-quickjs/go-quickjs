//go:build amd64 || arm64

package opt

import (
	"bytes"
	"runtime"
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

// TestPhiConstantsStoredOnEdges checks that a loop's phis in registers
// whose values live across a call are not stored into their slots where
// their blocks begin when an argument is a constant -- a resumption
// switch's zero, as a Go program's functions have -- but the constant on
// its edge: the values the loop carries are stored where they are made.
func TestPhiConstantsStoredOnEdges(t *testing.T) {
	m := compileWat(t, `(module
  (global $g (mut i32) (i32.const 0))
  (func $cold (param i32) (result i32)
    (global.set $g (i32.add (global.get $g) (local.get 0)))
    (i32.const 0))
  (func (export "run") (param $at i32) (param $n i32) (result i64)
    (local $i i32) (local $a i64) (local $b i64) (local $c i64)
    (block $exit
      (block $resume1
        (block $resume0
          (br_table $resume0 $resume1 $exit (local.get $at)))
        (local.set $a (i64.const 5)))
      (loop $l
        (local.set $a (i64.add (local.get $a) (i64.extend_i32_u (local.get $i))))
        (local.set $b (i64.xor (local.get $b) (i64.rotl (local.get $a) (i64.const 7))))
        (local.set $c (i64.add (local.get $c) (i64.const 3)))
        (if (i32.eqz (i32.and (local.get $i) (i32.const 15)))
          (then (drop (call $cold (local.get $i)))))
        (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 1))) (local.get $n)))))
    (i64.add (local.get $a) (i64.add (local.get $b) (local.get $c)))))`)
	req := testRequest(m, 1)
	req.Marks = true
	code, err := Compile(req)
	if err != nil {
		t.Fatal(err)
	}
	// The phis in a register with a slot.
	kept := map[string]bool{}
	for _, line := range strings.Split(code.Listing, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[1] != "=" || f[2] != "phi" || !strings.Contains(line, "(slot") {
			continue
		}
		kept[f[0]] = true
	}
	if len(kept) == 0 {
		t.Fatalf("no phi in a register with a slot:\n%s", code.Listing)
	}
	for _, mk := range code.Marks {
		if f := strings.Fields(mk.Tag); len(f) == 2 && f[1] == "spill" && kept[f[0]] {
			t.Errorf("phi %s stored where its block begins:\n%s", f[0], code.Listing)
		}
	}
}

// TestMarksAtTheirCode checks that a mark is where the code it names
// begins, past jumps the assembler shortened: a call's at its poll's
// count, the first of its instructions.
func TestMarksAtTheirCode(t *testing.T) {
	m := compileWat(t, `(module
  (func $f (param i32) (result i32) (i32.add (local.get 0) (i32.const 1)))
  (func (export "run") (param $n i32) (result i32)
    (local $i i32)
    (loop $l
      (if (i32.and (local.get $i) (i32.const 1))
        (then (local.set $n (call $f (local.get $n))))
        (else (local.set $n (i32.mul (local.get $n) (i32.const 3)))))
      (br_if $l (i32.lt_u (local.tee $i (i32.add (local.get $i) (i32.const 1))) (i32.const 100))))
    (call $f (local.get $n))))`)
	req := testRequest(m, 1)
	req.Marks = true
	code, err := Compile(req)
	if err != nil {
		t.Fatal(err)
	}
	// The poll's count: SUB R12, 1 on amd64, SUBS X22, X22, #1 on arm64.
	count := []byte{0x49, 0x83, 0xEC, 0x01}
	if runtime.GOARCH == "arm64" {
		count = []byte{0xD6, 0x06, 0x00, 0xF1}
	}
	calls := 0
	for _, mk := range code.Marks {
		if !strings.HasSuffix(mk.Tag, " call tuple") {
			continue
		}
		calls++
		if !bytes.HasPrefix(code.Bytes[mk.Off:], count) {
			t.Errorf("mark %q at %#x, at % x", mk.Tag, mk.Off, code.Bytes[mk.Off:min(mk.Off+8, len(code.Bytes))])
		}
	}
	if calls != 2 {
		t.Fatalf("%d calls marked, want 2: %v", calls, code.Marks)
	}
}

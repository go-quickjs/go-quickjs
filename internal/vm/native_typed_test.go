//go:build amd64 || arm64

package vm

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
)

// The typed table's call path (reg_typed.go), by its prototype's typed
// twins (bytecode.TypedTwin, QJS_TYPEDPROTO): typed code that is its
// ordinary code run through the typed table, entered by call sites alone.

// typedProtoSrc calls between typed and ordinary code every way: typed to
// typed (tfib, teven and todd), ordinary to typed (cross_add), typed to
// ordinary (tplain to a leaf, tframe to a body with a frame), throws
// through typed frames, f.arguments and f.caller read through an ordinary
// callee of typed code, the stack's limit reached in typed code, and sites
// whose callee turns from typed to ordinary and back.
const typedProtoSrc = `
var global_res;
function tfib(k) { return k < 2 ? k : tfib(k - 1) + tfib(k - 2); }
function teven(k) { return k === 0 ? 1 : todd(k - 1) + 1; }
function todd(k) { return k === 0 ? 0 : teven(k - 1) + 1; }
function cross_add(a, b) { a = a | 0; b = b | 0; return (a + b) | 0; }
function plain_add(a, b) { return a - b; }
function plain_inc(k) { return k + 1; }
function plain_frame(k) { var o = { v: k }; return o.v + 10; }
function tplain(k) { return k < 2 ? k : plain_inc(k - 1) + 1; }
function tframe(k) { return k < 2 ? k : plain_frame(k - 1) + 1; }
function thrower(k) { throw new RangeError("deep " + k); }
function tthrow(k) { return k < 1 ? thrower(k) : tthrow(k - 1) + 1; }
function ffi_args() { return targs.arguments.length * 100 + targs.arguments[0]; }
function targs(k) { return k < 2 ? ffi_args() : 0; }
function ffi_caller() { return ffi_caller.caller === tcaller ? 7 : 9; }
function tcaller(k) { return k < 2 ? ffi_caller() : 0; }
function plain_even(k) { return 1000 + k; }
(function () {
	var out = [], s = 0, i, fs = [cross_add, plain_add];
	for (i = 0; i < 30; i++) s = (s + tfib(12)) | 0;
	out.push(s, teven(100), todd(99), tplain(9), tframe(9));
	for (i = 0, s = 0; i < 5000; i++) s = cross_add(s, i);
	out.push(s);
	for (i = 0, s = 0; i < 5000; i++) s = fs[i & 1](s, i);
	out.push(s);
	try { tthrow(6); } catch (e) { out.push(e.name, e.message, e.stack); }
	try { out.push(targs(1), targs(1, 2, 3)); } catch (e) { out.push(e.name, e.message); }
	try { out.push(tcaller(0)); } catch (e) { out.push(e.name, e.message); }
	try { teven(1e7); } catch (e) { out.push(e instanceof RangeError, e.message); }
	var saved = teven;
	for (i = 0; i < 6; i++) {
		teven = i & 1 ? plain_even : saved;
		out.push(todd(5));
	}
	teven = saved;
	out.push(teven(10));
	return out.join("|");
})()
`

// typedProtoNames are the functions typedProtoSrc gives typed twins.
var typedProtoNames = []string{"tfib", "teven", "todd", "cross_add", "tplain", "tframe", "tthrow", "targs", "tcaller"}

// typedRunSrc runs src as nativeRunSrc does, the functions named given
// typed twins, and checks each was given one.
func typedRunSrc(t *testing.T, src string, native bool, names ...string) string {
	return typedRunSrcIn(t, Config{}, src, native, names...)
}

// typedRunSrcIn is typedRunSrc in a runtime of cfg.
func typedRunSrcIn(t *testing.T, cfg Config, src string, native bool, names ...string) string {
	t.Helper()
	was := compiler.SetTypedProto(names...)
	fn := compileForTest(t, src)
	compiler.SetTypedProto(was...)
	r := newTestRuntime(t, cfg.withNative(native))
	defer r.Close()
	v, err := r.Run(fn)
	if err != nil {
		t.Fatalf("native %v: %v", native, err)
	}
	if r.frameDepth != 0 {
		t.Fatalf("native %v: frame depth %d left", native, r.frameDepth)
	}
	for _, name := range names {
		g, err := r.GetProp(Obj(r.Global()), r.Intern(name))
		if err != nil || !g.IsObject() || g.Object().fn() == nil {
			t.Fatalf("no function %s", name)
		}
		if cl := g.Object().fn().closure; cl.regTyped == nil {
			t.Errorf("native %v: %s was given no typed twin", native, name)
		}
	}
	return v.String().Go()
}

// TestRegTypedProtoCalls runs typedProtoSrc with typed twins and without,
// by the Go table's loop and natively, the collector idle and as while it
// marks: the same answers, stack traces among them.
//
// In a runtime of Node's quirks, f.arguments and f.caller read the frames
// of typed code; in a standard one they throw.
func TestRegTypedProtoCalls(t *testing.T) {
	for _, cfg := range []Config{{}, {NodeQuirks: true}} {
		want := typedRunSrcIn(t, cfg, typedProtoSrc, false)
		for _, native := range []bool{false, true} {
			for _, marking := range []bool{false, true} {
				nativeMarking(t, marking)
				if got := typedRunSrcIn(t, cfg, typedProtoSrc, native, typedProtoNames...); got != want {
					t.Errorf("quirks %v, native %v, marking %v:\n got %s\nwant %s", cfg.NodeQuirks, native, marking, got, want)
				}
			}
		}
		t.Logf("quirks %v: %s", cfg.NodeQuirks, want)
	}
	nativeMarking(t, false)
}

// typedSwapTables has op run by Go in typed code alone, counted: the typed
// Go table's handler wrapped, and natively a copy of the typed
// dispatcher's table whose entry for op is the exit stub's, whose exit
// runs it. It returns the count so far.
func typedSwapTables(t *testing.T, op bytecode.ROp) func() int64 {
	var n atomic.Int64
	h := regTypedTable[op]
	regTypedTable[op] = func(regs *[1 << 16]Value, w bytecode.RInstr, pc int, st *regState) int {
		n.Add(1)
		return h(regs, w, pc, st)
	}
	tab := new([variantSlotsForTest]uintptr)
	*tab = *(*[variantSlotsForTest]uintptr)(nativePtr(&nativeTyped.addr))
	tab[op] = nativeExitAddr
	old := nativeTyped
	nativeTyped = newNativeTable(uintptr(unsafe.Pointer(tab)))
	nativeCounting(t)
	t.Cleanup(func() { regTypedTable[op] = h; nativeTyped = old; _ = tab })
	// Natively, the exit runs the typed table's Go handler too
	// (nativeTypedExit), which counts it.
	return func() int64 { return n.Load() }
}

// variantSlotsForTest is how many entries a variant's table with no
// overrides has: the operations' and the two exits'.
const variantSlotsForTest = 258

// TestRegTypedProtoRunsTyped checks that a twin's code runs through the
// typed table, by the Go table's loop and natively: the operation the
// tables are swapped for runs in Go only in typed code.
func TestRegTypedProtoRunsTyped(t *testing.T) {
	const src = `var global_res;
function cross_add(a, b) { a = a | 0; b = b | 0; return (a + b) | 0; }
(function () { var s = 0; for (var i = 0; i < 1000; i++) s = cross_add(s, i); return String(s) })()`
	count := typedSwapTables(t, bytecode.RAddI32)
	for _, native := range []bool{false, true} {
		before := count()
		if got := typedRunSrc(t, src, native); got != "499500" {
			t.Fatalf("native %v: %s", native, got)
		}
		if d := count() - before; d != 0 {
			t.Errorf("native %v: ordinary code ran add_i32 %d times by the typed table", native, d)
		}
		before = count()
		if got := typedRunSrc(t, src, native, "cross_add"); got != "499500" {
			t.Fatalf("native %v: typed: %s", native, got)
		}
		if d := count() - before; d != 1000 {
			t.Errorf("native %v: typed code ran add_i32 %d times by the typed table, want 1000", native, d)
		}
	}
}

// TestRegTypedProtoInterrupt stops typed code by the runtime's context,
// by the Go table and natively, and then runs it to its end.
func TestRegTypedProtoInterrupt(t *testing.T) {
	const forever = `function tfib(k) { return k < 2 ? k : tfib(k - 1) + tfib(k - 2); } tfib(60)`
	const finite = `String(tfib(20))`
	for _, native := range []bool{false, true} {
		was := compiler.SetTypedProto("tfib")
		fn, fin := compileForTest(t, forever), compileForTest(t, finite)
		compiler.SetTypedProto(was...)
		r := newTestRuntime(t, nativeConfig(native))
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		r.SetContext(ctx)
		unwatch := watchStopForTest(r, ctx)
		_, err := r.Run(fn)
		unwatch()
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("native %v: stopped with %v", native, err)
		}
		r.SetContext(context.Background())
		if v, err := r.Run(fin); err != nil || v.String().Go() != "6765" {
			t.Errorf("native %v: after the stop: %v, %v", native, v, err)
		}
		if g, _ := r.GetProp(Obj(r.Global()), r.Intern("tfib")); g.Object().fn().closure.regTyped == nil {
			t.Errorf("native %v: no typed twin", native)
		}
		r.Close()
	}
}

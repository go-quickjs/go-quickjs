//go:build amd64 || arm64

package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

const nativeLeafMissSource = `
function leafValue(f, x) { var v = f(x); return v }
function leafMake(x) { return function () { return x } }
function leafMakeOther(x) { return function () { return x } }
function leafMisses(n) {
	var sum = 0;
	for (var i = 0; i < n; i++) {
		var f = i & 1 ? leafMake(i) : leafMakeOther(i);
		sum += leafValue(f, 0);
	}
	return sum;
}`

func BenchmarkRegNativeLeafMiss(b *testing.B) {
	r := newTestRuntime(b, Config{})
	b.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	if _, err := r.Run(compileForTest(b, nativeLeafMissSource)); err != nil {
		b.Fatal(err)
	}
	fn := compileForTest(b, `leafMisses(3000)`)
	if _, err := r.Run(fn); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if v, err := r.Run(fn); err != nil || v.Number() != 4498500 {
			b.Fatalf("got %v, %v", v, err)
		}
	}
}

// A miss must cache these existing native leaves and retry the instruction,
// rather than run their bodies through Go's framed call entry.
func TestRegNativeLeafMissRetries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, source string
		leaf         uint8
	}{
		{"capture", `function make(x) { return function () { return x } }; var leaf = make({a: 7})`, regLeafCode},
		{"arrow", `function make(x) { return () => x }; var leaf = make({a: 7})`, regLeafCode},
		{"mutable", `var change; function make(x) { change = function (v) { x = v }; return function () { return x } }; var leaf = make({a: 7})`, regLeafCode},
		{"argument", `function leaf(x) { return x }`, regLeafArg},
		{"constant", `function leaf() { return 7 }`, regLeafConst},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestRuntime(t, Config{})
			defer r.Close()
			if _, err := r.Run(compileForTest(t, nativeLeafMissSource+tc.source)); err != nil {
				t.Fatal(err)
			}
			caller := r.global.getOwn(r.atoms.intern("leafValue")).value().Object().fn().closure
			callee := r.global.getOwn(r.atoms.intern("leaf")).value().Object()
			c, rc := r.regOf(caller), r.regOf(callee.fn().closure)
			if rc.leaf != tc.leaf || tc.leaf == regLeafCode && rc.leafUp == 0 {
				t.Fatalf("leaf kind %d, captured slot %d", rc.leaf, rc.leafUp)
			}
			r.regCallsMake(caller, c)
			pc := -1
			for i, w := range c.rc.Code {
				if w.Op() == bytecode.RCallW {
					pc = i
					break
				}
			}
			if pc < 0 {
				t.Fatal("caller has no call instruction")
			}
			w := c.rc.Code[pc]
			base := int(w.A())
			r.stack[base], r.stack[base+1], r.stack[base+2] = Obj(callee), Undefined, Float(9)
			f := frame{cl: caller}
			st := regState{r: r, f: &f, c: c, cl: caller}
			regs := regWindow(r.stack, 0)
			r.frameHigh = 0
			if next := nativeCallMiss(regs, w, pc, &st); next != pc {
				t.Fatalf("miss continued at %d, want retry at %d (frame high %d)", next, pc, r.frameHigh)
			}
			e := &caller.regCalls[w.C()]
			if e.callee != callee || e.rc != rc || e.kind&regCallLeaf == 0 || r.frameHigh != 0 {
				t.Fatalf("miss did not cache a frameless native retry: %+v, frame high %d", e, r.frameHigh)
			}
		})
	}
}

func TestRegNativeLeafMissCaptures(t *testing.T) {
	nativeCounting(t)
	const src = nativeLeafMissSource + `
var change;
function mutable(x) { change = function (v) { x = v }; return function () { return x } }
var ok = true, f = mutable({a: 3});
for (var i = 0; i < 50; i++) {
	if (leafValue(leafMake(i), 0) !== i || leafValue(f, 0).a !== (i === 0 ? 3 : i - 1)) ok = false;
	change({a: i});
}

leafMisses(1000) + "|" + ok + "|" + leafValue(f, 0).a`
	want := nativeRunSrc(t, src, false)
	if want != "499500|true|49" {
		t.Fatalf("Go table got %s", want)
	}
	for _, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		if got := nativeRunSrc(t, src, true); got != want {
			t.Fatalf("marking %v: got %s, want %s", marking, got, want)
		}
	}
}

func TestRegNativeLeafMissFallback(t *testing.T) {
	t.Parallel()
	const src = nativeLeafMissSource + `
var out = [];
function identity(x) { return x }
function direct() { "use strict"; return this }
function ownArguments() { return arguments.length }
function rest(...xs) { return xs[0] + 2 }
var bound = identity.bind(null, 11);
var proxy = new Proxy(identity, { apply(f, t, args) { return args[0] + 3 } });
for (var i = 0; i < 20; i++) {
	leafValue(leafMake(i), 0);
	out.push(leafValue(bound, 1), leafValue(proxy, 4), leafValue(ownArguments, 5), leafValue(rest, 6));
}
out.push(leafValue(direct, 0) === undefined);
out.slice(-5).join("|")`
	for _, native := range []bool{false, true} {
		if got := nativeRunSrc(t, src, native); got != "11|7|1|8|true" {
			t.Fatalf("native %v: got %s", native, got)
		}
	}
}

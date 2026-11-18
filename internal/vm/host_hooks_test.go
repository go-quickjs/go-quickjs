package vm_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/hostaccess"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// hostRuntime makes a runtime and returns it with its VM runtime.
func hostRuntime(t *testing.T, opts ...quickjs.Option) (*quickjs.Runtime, *vm.Runtime) {
	t.Helper()
	rt := quickjs.New(opts...)
	t.Cleanup(func() { rt.Close() })
	return rt, hostaccess.VM(rt)
}

// hostEval evaluates src and returns its result as a string.
func hostEval(t *testing.T, rt *quickjs.Runtime, src string) string {
	t.Helper()
	v, err := rt.Eval(src)
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	return v.String()
}

// setGlobal makes v the global name.
func setGlobal(r *vm.Runtime, name string, v vm.Value) {
	vm.HostDefineOwn(r.Global(), r.Intern(name), v, true, false, true)
}

func TestHostNodeQuirks(t *testing.T) {
	_, plain := hostRuntime(t)
	_, quirks := hostRuntime(t, quickjs.WithNodeQuirks())
	if vm.HostNodeQuirks(plain) || !vm.HostNodeQuirks(quirks) {
		t.Fatalf("HostNodeQuirks: plain %v, quirks %v", vm.HostNodeQuirks(plain), vm.HostNodeQuirks(quirks))
	}
}

func TestHostErrorClass(t *testing.T) {
	rt, r := hostRuntime(t)
	ctor, proto := vm.HostErrorClass(r, "OddError")
	setGlobal(r, "OddError", vm.Obj(ctor))
	got := hostEval(t, rt, `
		const e = new OddError("hi", {cause: 7});
		const f = OddError("called");
		class Sub extends OddError {}
		const s = new Sub("sub");
		[Object.getPrototypeOf(OddError) === Error, OddError.length, OddError.name,
		 Object.getPrototypeOf(OddError.prototype) === Error.prototype,
		 OddError.prototype.name, JSON.stringify(OddError.prototype.message),
		 Object.prototype.toString.call(e), String(e), e.cause, e instanceof OddError,
		 f.message, f instanceof OddError, s instanceof Sub, String(s),
		 Object.getOwnPropertyDescriptor(OddError, "prototype").writable,
		 typeof e.stack].join()`)
	want := `true,1,OddError,true,OddError,"",[object Error],OddError: hi,7,true,called,true,true,OddError: sub,false,string`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	e := vm.HostNewError(r, proto, "made in Go")
	setGlobal(r, "made", vm.Obj(e))
	if got := hostEval(t, rt, `[made instanceof OddError, made.message, String(made)].join()`); got != "true,made in Go,OddError: made in Go" {
		t.Fatalf("HostNewError: %s", got)
	}
}

func TestHostSlot(t *testing.T) {
	rt, r := hostRuntime(t)
	o := r.NewPlainObject()
	type data struct{ n int }
	vm.HostSetSlot(r, o, &data{n: 3})
	setGlobal(r, "o", vm.Obj(o))
	if d, ok := vm.HostSlot(r, vm.Obj(o)).(*data); !ok || d.n != 3 {
		t.Fatalf("HostSlot = %v", vm.HostSlot(r, vm.Obj(o)))
	}
	if vm.HostSlot(r, vm.Obj(r.NewPlainObject())) != nil || vm.HostSlot(r, vm.Float(1)) != nil {
		t.Fatal("HostSlot of an object without a slot is not nil")
	}
	// The slot is invisible to script, and survives freezing.
	got := hostEval(t, rt, `Object.freeze(o); [Reflect.ownKeys(o).length, Object.getOwnPropertySymbols(o).length, JSON.stringify(o)].join()`)
	if got != "0,0,{}" {
		t.Fatalf("the slot shows: %s", got)
	}
	if _, ok := vm.HostSlot(r, vm.Obj(o)).(*data); !ok {
		t.Fatal("freezing lost the slot")
	}
}

func TestHostDefineOwnAndAccessor(t *testing.T) {
	rt, r := hostRuntime(t)
	o := r.NewPlainObject()
	vm.HostDefineOwn(o, r.Intern("ro"), vm.Float(1), false, true, false)
	vm.HostDefineOwn(o, vm.HostToStringTag(r), vm.Str(vm.NewString("Tagged")), false, false, true)
	g := r.NewFunc("get x", 0, func(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
		return vm.Float(42), nil
	})
	vm.HostDefineAccessor(r, o, r.Intern("x"), g, nil, true, true)
	setGlobal(r, "o", vm.Obj(o))
	got := hostEval(t, rt, `
		const d = Object.getOwnPropertyDescriptor(o, "ro");
		const a = Object.getOwnPropertyDescriptor(o, "x");
		[d.writable, d.enumerable, d.configurable, o.x, a.enumerable, a.configurable, a.set,
		 Object.prototype.toString.call(o), Object.keys(o).join("+")].join()`)
	if got != "false,true,false,42,true,true,,[object Tagged],ro+x" {
		t.Fatalf("got %s", got)
	}
}

func TestHostPreventExtensionsAndPrototype(t *testing.T) {
	rt, r := hostRuntime(t)
	o := vm.HostObject(nil, nil)
	vm.HostPreventExtensions(o)
	p := r.NewPlainObject()
	q := r.NewPlainObject()
	vm.HostSetPrototype(q, p)
	setGlobal(r, "o", vm.Obj(o))
	setGlobal(r, "p", vm.Obj(p))
	setGlobal(r, "q", vm.Obj(q))
	got := hostEval(t, rt, `[Object.isExtensible(o), Object.getPrototypeOf(o), Object.getPrototypeOf(q) === p].join()`)
	if got != "false,,true" {
		t.Fatalf("got %s", got)
	}
}

func TestHostHasProperty(t *testing.T) {
	rt, r := hostRuntime(t)
	v, err := rt.Eval(`new Proxy({}, {has(t, k) { if (k === "boom") throw new RangeError("no"); return k === "yes" }})`)
	if err != nil {
		t.Fatal(err)
	}
	p := hostaccess.Unwrap(v).Object()
	if ok, err := vm.HostHasProperty(r, p, r.Intern("yes")); !ok || err != nil {
		t.Fatalf("yes: %v %v", ok, err)
	}
	if ok, err := vm.HostHasProperty(r, p, r.Intern("no")); ok || err != nil {
		t.Fatalf("no: %v %v", ok, err)
	}
	if _, err := vm.HostHasProperty(r, p, r.Intern("boom")); err == nil || !strings.Contains(err.Error(), "RangeError") {
		t.Fatalf("boom: %v", err)
	}
}

func TestHostBufferSource(t *testing.T) {
	rt, r := hostRuntime(t)
	for _, c := range []struct {
		src  string
		want string
		ok   bool
	}{
		{`new Uint8Array([1, 2, 3]).buffer`, "\x01\x02\x03", true},
		{`new Uint8Array([1, 2, 3, 4]).subarray(1, 3)`, "\x02\x03", true},
		{`new Uint16Array(new SharedArrayBuffer(2))`, "\x00\x00", true},
		{`(() => { const b = new ArrayBuffer(2); b.transfer(); return b })()`, "", true},
		{`new SharedArrayBuffer(2)`, "", false},
		{`new DataView(new ArrayBuffer(2))`, "", false},
		{`[1, 2]`, "", false},
	} {
		v, err := rt.Eval(c.src)
		if err != nil {
			t.Fatal(err)
		}
		b, ok := vm.HostBufferSource(hostaccess.Unwrap(v))
		if ok != c.ok || string(b) != c.want {
			t.Errorf("%s: %q %v, want %q %v", c.src, b, ok, c.want, c.ok)
		}
	}
	_ = r
}

func TestHostCaptureStack(t *testing.T) {
	rt, r := hostRuntime(t)
	setGlobal(r, "capture", r.NewFunction("capture", 0, func(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
		o := rt.NewPlainObject()
		vm.HostCaptureStack(rt, o)
		return vm.Obj(o), nil
	}))
	got := hostEval(t, rt, `function where() { return capture() } typeof where().stack + " " + where().stack.includes("where")`)
	if got != "string true" {
		t.Fatalf("got %s", got)
	}
}

func TestHostCheckInterrupt(t *testing.T) {
	_, r := hostRuntime(t)
	if err := vm.HostCheckInterrupt(r); err != nil {
		t.Fatalf("an idle runtime is interrupted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.SetContext(ctx)
	cancel()
	if err := vm.HostCheckInterrupt(r); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled runtime: %v", err)
	}
}

func TestHostStringsAndBigInts(t *testing.T) {
	rt, r := hostRuntime(t)
	s := vm.HostStringFromUnits([]uint16{'a', 0xD83D, 0xDE00, 0xD800})
	if s.Len() != 4 || s.CharCodeAt(3) != 0xD800 || s.CodePointAt(1) != 0x1F600 {
		t.Fatalf("HostStringFromUnits: %d units", s.Len())
	}
	for _, c := range []struct {
		v    vm.Value
		n    uint64
		fits bool
	}{
		{vm.HostBigInt64(0), 0, true},
		{vm.HostBigInt64(-1), 1<<64 - 1, false},
		{vm.HostBigInt64(1 << 62), 1 << 62, true},
		{vm.True, 1, true},
	} {
		n, fits, err := vm.HostToBigUint64(r, c.v)
		if err != nil || n != c.n || fits != c.fits {
			t.Errorf("HostToBigUint64(%v) = %d %v %v, want %d %v", c.v, n, fits, err, c.n, c.fits)
		}
	}
	for _, c := range []struct {
		src  string
		n    uint64
		fits bool
	}{
		{"18446744073709551615n", 1<<64 - 1, true},
		{"18446744073709551616n", 0, false},
		{"-(2n ** 64n) + 5n", 5, false},
	} {
		v, err := rt.Eval(c.src)
		if err != nil {
			t.Fatal(err)
		}
		n, fits, err := vm.HostToBigUint64(r, hostaccess.Unwrap(v))
		if err != nil || n != c.n || fits != c.fits {
			t.Errorf("HostToBigUint64(%s) = %d %v %v, want %d %v", c.src, n, fits, err, c.n, c.fits)
		}
	}
	if _, _, err := vm.HostToBigUint64(r, vm.Float(1)); err == nil {
		t.Error("HostToBigUint64 converted a Number")
	}
}

func TestHostConstructorAndObject(t *testing.T) {
	rt, r := hostRuntime(t)
	type widget struct{ size int }
	proto := r.NewPlainObject()
	ctor := vm.HostConstructor(r, "Widget", 1, proto, func(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
		if !rt.Constructing() {
			return vm.Undefined, rt.ThrowTypeError("Widget must be invoked with 'new'")
		}
		p, err := vm.HostProtoFromNewTarget(rt, proto)
		if err != nil {
			return vm.Undefined, err
		}
		return vm.Obj(vm.HostObject(p, &widget{size: 5})), nil
	})
	setGlobal(r, "Widget", vm.Obj(ctor))
	got := hostEval(t, rt, `
		class Big extends Widget {}
		const w = new Widget(), b = new Big();
		let called;
		try { Widget() } catch (e) { called = e.constructor.name }
		[Widget.name, Widget.length, Object.getPrototypeOf(w) === Widget.prototype,
		 Object.getPrototypeOf(b) === Big.prototype, b instanceof Widget, Object.isExtensible(w),
		 Widget.prototype.constructor === Widget, called].join()`)
	if got != "Widget,1,true,true,true,true,true,TypeError" {
		t.Fatalf("got %s", got)
	}
	v, _ := rt.Eval(`w`)
	if d, ok := vm.HostData(hostaccess.Unwrap(v)).(*widget); !ok || d.size != 5 {
		t.Fatalf("HostData = %v", vm.HostData(hostaccess.Unwrap(v)))
	}
	if vm.HostData(vm.Float(1)) != nil {
		t.Fatal("HostData of a number is not nil")
	}
}

func TestHostBigInt64(t *testing.T) {
	rt, r := hostRuntime(t)
	setGlobal(r, "big", vm.HostBigInt64(-5))
	if got := hostEval(t, rt, `typeof big + " " + big`); got != "bigint -5" {
		t.Fatalf("got %s", got)
	}
	v, _ := rt.Eval(`2n ** 64n + 3n`)
	if n, err := vm.HostToBigInt64(r, hostaccess.Unwrap(v)); err != nil || n != 3 {
		t.Fatalf("HostToBigInt64 = %d %v", n, err)
	}
	if _, err := vm.HostToBigInt64(r, vm.Float(1)); err == nil {
		t.Fatal("HostToBigInt64 converted a Number")
	}
}

func TestHostArrayBuffers(t *testing.T) {
	rt, r := hostRuntime(t)
	mem := make([]byte, 8, 16)
	mem[0] = 7
	resized := -1
	var buf vm.Value
	buf = vm.HostArrayBuffer(r, mem, nil, true, 16, func(n int) error {
		resized = n
		vm.HostSetArrayBufferBytes(r, buf, mem[:n])
		return nil
	})
	fixed := vm.HostArrayBuffer(r, mem[:4], nil, false, 0, nil)
	setGlobal(r, "buf", buf)
	setGlobal(r, "fixed", fixed)
	got := hostEval(t, rt, `
		const out = [buf.byteLength, buf.resizable, buf.maxByteLength, new Uint8Array(buf)[0]];
		try { buf.transfer() } catch (e) { out.push(e.message) }
		buf.resize(12);
		out.push(buf.byteLength, fixed.resizable, fixed.byteLength);
		globalThis.view = new Uint8Array(fixed);
		out.join()`)
	if got != "8,true,16,7,ArrayBuffer is not detachable and could not be cloned.,12,false,4" || resized != 12 {
		t.Fatalf("got %s (resized %d)", got, resized)
	}
	vm.HostDetachArrayBuffer(r, fixed)
	if got := hostEval(t, rt, `[fixed.detached, fixed.byteLength, view.length].join()`); got != "true,0,0" {
		t.Fatalf("after detaching: %s", got)
	}
	shared := make([]byte, 16)
	sab := vm.HostSharedArrayBuffer(r, shared, 8, nil, -1, nil)
	setGlobal(r, "sab", sab)
	if got := hostEval(t, rt, `new Int32Array(sab)[0] = 9; [sab.byteLength, sab.growable, Object.prototype.toString.call(sab)].join()`); got != "8,false,[object SharedArrayBuffer]" || shared[0] != 9 {
		t.Fatalf("shared: %s (%d)", got, shared[0])
	}
}

func TestHostDefineLazyGlobal(t *testing.T) {
	rt, r := hostRuntime(t)
	built := 0
	vm.HostDefineLazyGlobal(r, "Lazy", func(r *vm.Runtime) *vm.Object {
		built++
		o := r.NewPlainObject()
		vm.HostDefineOwn(o, r.Intern("n"), vm.Float(1), true, true, true)
		return o
	})
	if built != 0 {
		t.Fatal("the global was built before it was read")
	}
	if got := hostEval(t, rt, `[Lazy.n, Lazy === Lazy, Object.getOwnPropertyDescriptor(globalThis, "Lazy").writable].join()`); got != "1,true,true" {
		t.Fatalf("got %s", got)
	}
	if built != 1 {
		t.Fatalf("built %d times", built)
	}
}

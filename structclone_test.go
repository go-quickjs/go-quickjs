package quickjs_test

import (
	"errors"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestStructuredCloneAcrossRuntimes pins that a value serialized in one
// runtime is deserialized whole in another, on another goroutine: its shared
// references and cycles kept, a transferred buffer's bytes moved, and a
// SharedArrayBuffer's memory shared.
func TestStructuredCloneAcrossRuntimes(t *testing.T) {
	a, b := quickjs.New(), quickjs.New()
	defer a.Close()
	defer b.Close()
	v, err := a.Eval(`
		var sab = new SharedArrayBuffer(4), buf = new Uint8Array([1, 2, 3, 4]).buffer;
		const key = {k: 1};
		const value = {
			map: new Map([[key, key]]), set: new Set([key]), key,
			text: "aé𝒳\ud800z", big: -(2n ** 70n), when: new Date(7),
			error: new TypeError("t", {cause: [1]}), view: new Uint16Array(buf, 2),
			shared: new Int32Array(sab), re: /x+/giu, list: [1, , 3],
		};
		value.self = value;
		value`)
	if err != nil {
		t.Fatal(err)
	}
	transfer, _ := a.Get("buf")
	data, err := a.Serialize(v, &quickjs.CloneOptions{Transfer: []quickjs.Value{transfer}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Eval("[buf.detached, buf.byteLength].join()"); got.String() != "true,0" {
		t.Errorf("the transferred buffer = %s, want it detached", got)
	}

	done := make(chan error, 1)
	go func() {
		out, err := b.Deserialize(data, nil)
		if err == nil {
			err = b.Set("value", out)
		}
		done <- err
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := b.Eval(`
		const [[mk, mv]] = value.map, [sk] = value.set;
		Atomics.store(value.shared, 0, 42);
		[mk === mv, mk === sk, mk === value.key, value.self === value,
		 value.text === "aé𝒳\ud800z", value.big === -(2n ** 70n), value.when.getTime(),
		 value.error instanceof TypeError, value.error.message, value.error.cause[0],
		 value.view.length, value.view.byteOffset, new Uint8Array(value.view.buffer).join(""),
		 value.re.flags, value.re.source, 1 in value.list, value.list.length].join()`)
	if err != nil {
		t.Fatal(err)
	}
	want := "true,true,true,true,true,true,7,true,t,1,1,2,1234,giu,x+,false,3"
	if got.String() != want {
		t.Errorf("deserialized = %s, want %s", got, want)
	}
	// The memory the two runtimes share is one.
	if got, _ := a.Eval("Atomics.load(new Int32Array(sab), 0)"); got.Int() != 42 {
		t.Errorf("shared memory = %s, want 42", got)
	}
}

// TestStructuredCloneErrors pins what a failed serialization is: a
// DataCloneError with V8's message for what cannot be cloned, and the
// exception itself for one a getter throws.
func TestStructuredCloneErrors(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	for src, want := range map[string]string{
		"(function f() {})":                    "function f() {} could not be cloned.",
		"({s: Symbol('d')})":                   "Symbol(d) could not be cloned.",
		"new WeakSet":                          "#<WeakSet> could not be cloned.",
		"[Promise.resolve()]":                  "#<Promise> could not be cloned.",
		"new Proxy([], {})":                    "[object Array] could not be cloned.",
		"new Intl.Collator":                    "#<Collator> could not be cloned.",
		"(function () { return arguments })()": "#<Object> could not be cloned.",
		"(function* () {})()":                  "[object Generator] could not be cloned.",
		"new WeakRef({})":                      "#<WeakRef> could not be cloned.",
		"(() => { const b = new ArrayBuffer(1); b.transfer(); return b })()": "An ArrayBuffer is detached and could not be cloned.",
	} {
		v, err := rt.Eval(src)
		if err != nil {
			t.Fatal(err)
		}
		_, err = rt.Serialize(v, nil)
		var dce *quickjs.DataCloneError
		if !errors.As(err, &dce) || dce.Message != want {
			t.Errorf("%s: %v, want DataCloneError %q", src, err, want)
		}
	}

	// Math and JSON are ordinary objects, and clone as ones.
	v, err := rt.Eval(`[Math, JSON]`)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := rt.Serialize(v, nil); err != nil {
		t.Errorf("[Math, JSON]: %v", err)
	} else if out, err := rt.Deserialize(data, nil); err != nil || out.Len() != 2 {
		t.Errorf("[Math, JSON] = %v, %v", out, err)
	}

	v, err = rt.Eval(`({get x() { throw new RangeError("from a getter") }})`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = rt.Serialize(v, nil)
	var jsErr *quickjs.Error
	if !errors.As(err, &jsErr) {
		t.Fatalf("err = %v, want the getter's exception", err)
	}
	if name, _ := jsErr.Value().Get("name"); name.String() != "RangeError" {
		t.Errorf("thrown = %s, want the RangeError", jsErr.Value())
	}

	// A buffer listed to transfer stays where it is when the clone fails.
	if _, err := rt.Eval(`var kept = new ArrayBuffer(2)`); err != nil {
		t.Fatal(err)
	}
	kept, _ := rt.Get("kept")
	fn, _ := rt.Eval("[() => 1]")
	if _, err := rt.Serialize(fn, &quickjs.CloneOptions{Transfer: []quickjs.Value{kept}}); err == nil {
		t.Fatal("a function was cloned")
	}
	if got, _ := rt.Eval("kept.byteLength"); got.Int() != 2 {
		t.Errorf("kept.byteLength = %s after a failed clone, want 2", got)
	}
}

// TestStructuredCloneDepth pins that a value nested too deeply to walk is a
// RangeError rather than the end of the goroutine's stack.
func TestStructuredCloneDepth(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	v, err := rt.Eval(`let deep = []; for (let i = 0; i < 20000; i++) deep = [deep]; deep`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = rt.Serialize(v, nil)
	var jsErr *quickjs.Error
	if !errors.As(err, &jsErr) {
		t.Fatalf("err = %v, want a RangeError", err)
	}
	if name, _ := jsErr.Value().Get("name"); name.String() != "RangeError" {
		t.Errorf("thrown = %s, want a RangeError", jsErr.Value())
	}
}

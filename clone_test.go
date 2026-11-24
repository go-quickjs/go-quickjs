package quickjs_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// A Serialized is deserialized once, since deserializing moves its buffers'
// bytes, and a Copy is another, deserialized on its own.
func TestSerializedOnceAndCopy(t *testing.T) {
	t.Parallel()
	rt := quickjs.New()
	defer rt.Close()
	v, err := rt.Eval(`({bytes: new Uint8Array([1, 2, 3])})`)
	if err != nil {
		t.Fatal(err)
	}
	data, err := rt.Serialize(v, nil)
	if err != nil {
		t.Fatal(err)
	}
	other := data.Copy()
	first, err := rt.Deserialize(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Deserialize(data, nil); !errors.Is(err, quickjs.ErrDeserialized) {
		t.Errorf("deserialized twice: %v, want ErrDeserialized", err)
	}
	second, err := rt.Deserialize(other, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Set("first", first); err != nil {
		t.Fatal(err)
	}
	if err := rt.Set("second", second); err != nil {
		t.Fatal(err)
	}
	got, err := rt.Eval(`first.bytes[0] = 9; [first.bytes.join(""), second.bytes.join(""), first.bytes.buffer !== second.bytes.buffer].join()`)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "923,123,true" {
		t.Errorf("the copies = %s, want two buffers of their own", got)
	}
}

// A host's object clones as its brand says: opaque as an empty object,
// unsupported not at all, transfer-only only when transferred, and a host
// object through the codec, which revives it on the other side.
func TestCloneBrands(t *testing.T) {
	t.Parallel()
	rt := quickjs.New()
	defer rt.Close()
	branded := func(b quickjs.CloneBrand) quickjs.Value {
		o := rt.NewObject()
		if err := o.Set("secret", 1); err != nil {
			t.Fatal(err)
		}
		if !o.SetCloneBrand(b) {
			t.Fatalf("brand %d refused", b)
		}
		return o
	}

	data, err := rt.Serialize(branded(quickjs.CloneOpaque), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := rt.Deserialize(data, nil); err != nil || len(out.Keys()) != 0 {
		t.Errorf("opaque = %v, %v; want an empty object", out.Keys(), err)
	}

	var dce *quickjs.DataCloneError
	if _, err := rt.Serialize(branded(quickjs.CloneUnsupported), nil); !errors.As(err, &dce) {
		t.Errorf("unsupported = %v, want a DataCloneError", err)
	}
	if _, err := rt.Serialize(branded(quickjs.CloneTransferOnly), nil); !errors.As(err, &dce) {
		t.Errorf("transfer-only, not transferred = %v, want a DataCloneError", err)
	}

	type token struct{ id int }
	codec := &quickjs.CloneCodec{
		Serialize: func(v quickjs.Value) (any, bool, error) {
			id, _ := v.Get("secret")
			return token{id.Int()}, true, nil
		},
		Revive: func(tok any) (quickjs.Value, error) {
			o := rt.NewObject()
			return o, o.Set("revived", tok.(token).id)
		},
	}
	data, err = rt.Serialize(branded(quickjs.CloneHost), &quickjs.CloneOptions{Codec: codec})
	if err != nil {
		t.Fatal(err)
	}
	out, err := rt.Deserialize(data, codec)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := out.Get("revived"); v.Int() != 1 {
		t.Errorf("revived = %v, want 1", v)
	}

	arr, _ := rt.NewArray(1)
	if arr.SetCloneBrand(quickjs.CloneOpaque) {
		t.Error("an array was branded")
	}
}

// A value is serialized only in its own runtime, and a closed runtime
// serializes nothing.
func TestSerializeRefuses(t *testing.T) {
	t.Parallel()
	a, b := quickjs.New(), quickjs.New()
	defer a.Close()
	v, _ := b.Eval(`({x: 1})`)
	if _, err := a.Serialize(v, nil); err == nil {
		t.Error("a value of another runtime was serialized")
	}
	b.Close()
	if _, err := b.Serialize(quickjs.Value{}, nil); !errors.Is(err, quickjs.ErrClosed) {
		t.Errorf("closed: %v, want ErrClosed", err)
	}
}

// A Value of one runtime is refused by every other, wherever the host hands
// it over: read with the other's shapes and atoms, an object of one runtime
// is another object in the other, and a function of one runs its closure
// with the other's state (KI-82). Refused, nothing of it is read.
func TestForeignValueRefused(t *testing.T) {
	t.Parallel()
	a, b := quickjs.New(), quickjs.New()
	defer a.Close()
	defer b.Close()
	eval := func(rt *quickjs.Runtime, src string) quickjs.Value {
		t.Helper()
		v, err := rt.Eval(src)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	objA := eval(a, `({a: 41})`)
	fnA := eval(a, `(function () { return 1 })`)
	strA := eval(a, `"a string of a"`)
	bufA := eval(a, `new ArrayBuffer(8)`)
	promiseA := a.NewPromise()
	objB := eval(b, `({})`)
	fnB := eval(b, `(function () { return 2 })`)
	promiseB := b.NewPromise()
	realmB, err := b.NewRealm()
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		do   func() error
	}{
		{"Runtime.Set an object", func() error { return b.Set("x", objA) }},
		{"Runtime.Set a function", func() error { return b.Set("f", fnA) }},
		{"Runtime.Set a string", func() error { return b.Set("s", strA) }},
		{"Realm.Set", func() error { return realmB.Set("x", objA) }},
		{"Encode", func() error { _, err := b.Encode(objA); return err }},
		{"Encode in a map", func() error { _, err := b.Encode(map[string]any{"k": objA}); return err }},
		{"Encode in a slice", func() error { _, err := b.Encode([]quickjs.Value{objA}); return err }},
		{"Encode in a struct", func() error {
			_, err := b.Encode(struct{ V quickjs.Value }{objA})
			return err
		}},
		{"Encode a promise", func() error { _, err := b.Encode(promiseA); return err }},
		{"NewArray", func() error { _, err := b.NewArray(objA); return err }},
		{"Value.Set", func() error { return objB.Set("k", objA) }},
		{"Call", func() error { _, err := fnB.Call(objA); return err }},
		{"CallWithThis", func() error { _, err := fnB.CallWithThis(objA); return err }},
		{"CallContext", func() error { _, err := fnB.CallContext(context.Background(), fnA); return err }},
		{"New", func() error { _, err := fnB.New(objA); return err }},
		{"Equal", func() error { _, err := objB.Equal(objA); return err }},
		{"Equal with a primitive", func() error { _, err := eval(b, `41`).Equal(strA); return err }},
		{"SetModule", func() error { return b.SetModule("m1", map[string]any{"x": objA}) }},
		{"SetModuleValues", func() error {
			return b.SetModuleValues("m2", map[string]quickjs.Value{"own": objB, "x": objA})
		}},
		{"Throw", func() error {
			if err := b.Throw(objA); !errors.Is(err, quickjs.ErrForeignValue) {
				return fmt.Errorf("Throw gave %v", err)
			}
			return quickjs.ErrForeignValue
		}},
		{"DetachArrayBuffer", func() error { return b.DetachArrayBuffer(bufA) }},
		{"Serialize", func() error { _, err := b.Serialize(objA, nil); return err }},
		{"Serialize's transfer", func() error {
			_, err := b.Serialize(objB, &quickjs.CloneOptions{Transfer: []quickjs.Value{bufA}})
			return err
		}},
		{"Promise.Resolve", func() error { return promiseB.Resolve(objA) }},
		{"Promise.Reject", func() error { return promiseB.Reject(objA) }},
	}
	for _, c := range cases {
		if err := c.do(); !errors.Is(err, quickjs.ErrForeignValue) {
			t.Errorf("%s: %v, want ErrForeignValue", c.name, err)
		}
	}

	// What a Go function returns is converted as Encode converts it, and a
	// foreign value it returns ends the script with the error, as a result
	// that cannot be converted does.
	if err := b.Set("leak", func() quickjs.Value { return objA }); err != nil {
		t.Fatal(err)
	}
	if v, err := b.Eval(`try { leak(); "taken" } catch (e) { "caught" }`); !errors.Is(err, quickjs.ErrForeignValue) {
		t.Errorf("a Go function returning a foreign value: %v, %v", v, err)
	}

	// Nothing refused reached b, and a's values are as they were.
	if got := eval(b, `[typeof x, typeof f, typeof s, typeof leak].join()`).String(); got != "undefined,undefined,undefined,function" {
		t.Errorf("b after the refusals: %s", got)
	}
	if got := eval(a, `41`).Int(); got != 41 {
		t.Errorf("a after the refusals: %d", got)
	}
	if v, err := objA.Get("a"); err != nil || v.Int() != 41 {
		t.Errorf("objA.a after the refusals: %v, %v", v, err)
	}
	if bytes, ok := bufA.Bytes(); !ok || len(bytes) != 8 {
		t.Errorf("a's buffer was detached by b: %d bytes, %v", len(bytes), ok)
	}

	// A runtime's own values, and the zero Value, which is no runtime's, are
	// taken as before.
	if err := b.Set("own", objB); err != nil {
		t.Errorf("own value: %v", err)
	}
	if err := b.Set("zero", quickjs.Value{}); err != nil {
		t.Errorf("the zero Value: %v", err)
	}
	if eq, err := (quickjs.Value{}).Equal(objA); err != nil || eq {
		t.Errorf("the zero Value == an object: %v, %v", eq, err)
	}
	if _, err := fnB.CallWithThis(objB, objB, quickjs.Value{}); err != nil {
		t.Errorf("own arguments: %v", err)
	}
}

// Refusing a foreign value reads nothing of the runtime it belongs to, so
// a host that hands one over by mistake, with each runtime on its own
// goroutine, races with nothing (-race). Taken, a function of one runtime
// called from both ran one closure with two runtimes' state.
func TestForeignValueRefusedConcurrently(t *testing.T) {
	t.Parallel()
	a, b := quickjs.New(), quickjs.New()
	defer a.Close()
	defer b.Close()
	fnA, err := a.Eval(`(function (n) { let s = 0; for (let i = 0; i < n; i++) s += i; return s })`)
	if err != nil {
		t.Fatal(err)
	}
	objA, err := a.Eval(`({a: 41})`)
	if err != nil {
		t.Fatal(err)
	}
	fnB, err := b.Eval(`(function (x) { return typeof x })`)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if _, err := fnA.Call(100); err != nil {
				t.Errorf("a's own call: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if err := b.Set("f", fnA); !errors.Is(err, quickjs.ErrForeignValue) {
				// Taken, the function runs here with b's state while a
				// runs it too, which -race reports.
				b.Eval(`f(100)`)
				t.Errorf("Set: %v", err)
				return
			}
			if _, err := fnB.Call(fnA); !errors.Is(err, quickjs.ErrForeignValue) {
				t.Errorf("Call: %v", err)
				return
			}
			if _, err := b.Encode(map[string]any{"o": objA}); !errors.Is(err, quickjs.ErrForeignValue) {
				t.Errorf("Encode: %v", err)
				return
			}
			if _, err := fnB.Call(i); err != nil {
				t.Errorf("b's own call: %v", err)
				return
			}
		}
	}()
	wg.Wait()
}

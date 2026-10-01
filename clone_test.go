package quickjs_test

import (
	"errors"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// A Serialized is deserialized once, since deserializing moves its buffers'
// bytes, and a Copy is another, deserialized on its own.
func TestSerializedOnceAndCopy(t *testing.T) {
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

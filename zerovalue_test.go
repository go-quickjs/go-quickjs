package quickjs_test

import (
	"errors"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestZeroValueIsZero pins that the zero Value is the number zero everywhere:
// what script receives by each way a host hands one over, and what the
// Value's own methods say of it. String, Float, Decode and Equal used to call
// it invalid, closed or NaN while script was given 0.
func TestZeroValueIsZero(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	var zero quickjs.Value

	type fields struct {
		A quickjs.Value
		B *quickjs.Value
	}
	for name, v := range map[string]any{
		"direct": zero,
		"fields": fields{},
		"slice":  []quickjs.Value{zero},
		"result": func() quickjs.Value { return zero },
		"thrown": func() error { return rt.Throw(zero) },
	} {
		if err := rt.Set(name, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := rt.SetModuleValues("m", map[string]quickjs.Value{"z": zero}); err != nil {
		t.Fatal(err)
	}
	mod, err := rt.EvalModule("main.js", `
		import {z} from "m";
		let caught = "none";
		try { thrown() } catch (e) { caught = e }
		export default [direct, fields.A, fields.B, slice[0], result(), z, caught].map(String).join(" ");
	`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := mod.Get("default")
	if err != nil {
		t.Fatal(err)
	}
	if want := "0 0 null 0 0 0 0"; got.String() != want {
		t.Errorf("script: got %q, want %q", got.String(), want)
	}

	// What the host asks of it.
	if k := zero.Kind(); k != quickjs.KindNumber {
		t.Errorf("Kind: %v", k)
	}
	if s, f, n, b := zero.String(), zero.Float(), zero.Int(), zero.Bool(); s != "0" || f != 0 || n != 0 || b {
		t.Errorf("String %q, Float %v, Int %v, Bool %v", s, f, n, b)
	}
	var a any
	var s string
	var i int
	var p *float64
	var v quickjs.Value
	for _, dst := range []any{&a, &s, &i, &p, &v} {
		if err := zero.Decode(dst); err != nil {
			t.Errorf("Decode into %T: %v", dst, err)
		}
	}
	if a != 0.0 || s != "0" || i != 0 || p == nil || *p != 0 || !v.StrictEqual(zero) {
		t.Errorf("Decode: any %v, string %q, int %v, *float64 %v, Value %v", a, s, i, p, v)
	}
	var m map[string]any
	if err := zero.Decode(&m); err == nil {
		t.Error("Decode into a map: a number should not decode into one")
	}

	// The comparisons, both ways round and with values of a runtime.
	num0, _ := rt.Eval(`0`)
	negZero, _ := rt.Eval(`-0`)
	str0, _ := rt.Eval(`"0"`)
	undef, _ := rt.Eval(`undefined`)
	if !zero.StrictEqual(num0) || !num0.StrictEqual(zero) || !zero.StrictEqual(negZero) || zero.StrictEqual(undef) || zero.StrictEqual(str0) {
		t.Error("StrictEqual: the zero Value should be === 0 and -0 only")
	}
	for _, c := range []struct {
		x, y quickjs.Value
		want bool
	}{
		{zero, zero, true}, {zero, num0, true}, {num0, zero, true},
		{zero, str0, true}, {str0, zero, true}, {zero, undef, false},
	} {
		if eq, err := c.x.Equal(c.y); err != nil || eq != c.want {
			t.Errorf("%v == %v: got %v %v, want %v", c.x, c.y, eq, err, c.want)
		}
	}

	// It belongs to no runtime, so what needs one says so.
	if _, err := zero.Get("x"); !errors.Is(err, quickjs.ErrClosed) {
		t.Errorf("Get: %v", err)
	}
	if _, err := zero.Call(); !errors.Is(err, quickjs.ErrClosed) {
		t.Errorf("Call: %v", err)
	}
}

package wasm_test

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/api"
)

// TestReferencesCrossHierarchiesAtTheBoundary pins how a reference the host
// passes is taken where its type is of the other hierarchy: a struct or an
// i31 wasm made, given where an externref is expected, is externalized, and
// a host value given where an anyref is expected is internalized -- what
// extern.convert_any and any.convert_extern do, and what the JavaScript
// API does, where any value is an externref. Inside wasm the externalized
// struct converts back to the same struct. Node 26 gives the same answers
// for the same module and values.
func TestReferencesCrossHierarchiesAtTheBoundary(t *testing.T) {
	ctx, r := newRuntime(t)
	mod := instantiate(t, ctx, r, `(module
		(type $s (struct (field i32)))
		(func (export "mkStruct") (result anyref) (struct.new $s (i32.const 7)))
		(func (export "mkI31") (result anyref) (ref.i31 (i32.const 5)))
		(func (export "mkExtStruct") (result externref) (extern.convert_any (struct.new $s (i32.const 9))))
		(func (export "takeExtern") (param externref) (result i32)
			(ref.test (ref $s) (any.convert_extern (local.get 0))))
		(func (export "takeAny") (param anyref) (result i32)
			(ref.test (ref $s) (local.get 0))))`)
	made := func(name string) any {
		res, err := mod.ExportedFunction(name).CallValues(ctx)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return res[0].Ref
	}
	for _, tc := range []struct {
		name  string
		v     any
		taker string
		want  uint64
	}{
		{"a struct as externref", made("mkStruct"), "takeExtern", 1},
		{"a struct as anyref", made("mkStruct"), "takeAny", 1},
		{"an i31 as externref", made("mkI31"), "takeExtern", 0},
		{"an externalized struct as externref", made("mkExtStruct"), "takeExtern", 1},
		{"an externalized struct as anyref", made("mkExtStruct"), "takeAny", 1},
		{"a host value as anyref", struct{}{}, "takeAny", 0},
	} {
		res, err := mod.ExportedFunction(tc.taker).CallValues(ctx, api.Value{Ref: tc.v})
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if res[0].Lo != tc.want {
			t.Errorf("%s: %s returned %d, want %d", tc.name, tc.taker, res[0].Lo, tc.want)
		}
	}
}

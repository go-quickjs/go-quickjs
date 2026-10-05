//go:build go1.27

package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestNewTypedArrayMethod pins the method form Go 1.27 allows: the same typed
// array as the function's, over the same memory.
func TestNewTypedArrayMethod(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	s := []float32{0.5, 1}
	a, err := rt.NewTypedArray(s, quickjs.ShareMemory)
	if err != nil {
		t.Fatal(err)
	}
	set, err := rt.Eval(`(a) => { a[0] = 4; return a.constructor.name + " " + a.length }`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := set.Call(a)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "Float32Array 2" || s[0] != 4 {
		t.Errorf("got %v, slice %v", got, s)
	}
}

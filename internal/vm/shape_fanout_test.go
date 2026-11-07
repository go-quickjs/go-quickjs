package vm

import (
	"fmt"
	"testing"
)

// A root keeps a transition for each first key a program's objects start with,
// past what a shape further along keeps: objects that begin alike and go on
// each their own way are a table of keys, whose shapes are kept for nothing.
func TestShapeFanout(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	parsed := func(text string) *Object {
		t.Helper()
		v, _, err := r.parseJSON(text, false)
		if err != nil {
			t.Fatal(err)
		}
		return v.Object()
	}
	// The root has transitions of its own from the start, and room for these.
	for i := range 1000 {
		if o := parsed(fmt.Sprintf(`{"first%d":1}`, i)); o.shape.unique {
			t.Fatalf("object %d has a layout of its own", i)
		}
	}
	// One first transition and maxShapeFanout more.
	shared := 0
	for i := range 2 * maxShapeFanout {
		if o := parsed(fmt.Sprintf(`{"then":1,"second%d":2}`, i)); !o.shape.unique {
			shared++
		}
	}
	if shared != maxShapeFanout+1 {
		t.Errorf("%d layouts shared past the first key, want %d", shared, maxShapeFanout+1)
	}
}

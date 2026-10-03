package vm

import "testing"

// A cache gives a layout of its own only to a prototype that has properties
// and no shape. A receiver like that -- a dictionary with a null prototype --
// keeps having none, or every key added to it would make a new layout.
func TestProtoShapeOnlyForPrototypes(t *testing.T) {
	r := New(Config{})
	key := r.atoms.intern("k")
	dict := newObject(nil, ClassObject)
	dict.setOwnRaw(key, Int(1), propDefault)
	c := propCache{shape: noShape}
	for i := 0; i < 3; i++ {
		r.cachedProp(&c, dict, key)
	}
	if dict.shape != nil {
		t.Errorf("a dictionary read through a cache was given a shape")
	}

	proto := newObject(nil, ClassObject)
	proto.setOwnRaw(key, Int(2), propDefault)
	child := newObject(proto, ClassObject)
	c = propCache{shape: noShape}
	r.cachedProp(&c, child, key)
	if v, ok := r.cachedProp(&c, child, key); !ok || v.Number() != 2 {
		t.Errorf("read through the prototype = %v %v, want a cache hit of 2", v, ok)
	}
}

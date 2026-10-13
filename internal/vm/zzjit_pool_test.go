//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"testing"
	"unsafe"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
)

// Pool objects have the interpreter's allocation layout, including its
// inline-property capacities and growth into separate property storage.
func TestJITObjectPoolLayouts(t *testing.T) {
	proto := newObject(nil, ClassObject)
	root := &shape{}
	for n := 0; n <= 16; n++ {
		pool := &abi.ObjectPool{Size: abi.PoolSize}
		jitFillObjectPool(pool, proto, root, n)
		want := newLiteralObject(proto, ClassObject, n)
		if pool.Count != pool.Size || pool.Proto != unsafe.Pointer(proto) {
			t.Fatalf("size %d: count %d, prototype %p", n, pool.Count, pool.Proto)
		}
		for i := range int(pool.Count) {
			o := (*Object)(pool.Objects[i])
			if o.proto != proto || o.shape != root || o.class != want.class || o.flags != want.flags || len(o.vals) != 0 || cap(o.vals) != cap(want.vals) {
				t.Fatalf("size %d, object %d: wrong initial layout", n, i)
			}
			o.shape = nil
			for j := 0; j <= cap(want.vals); j++ {
				o.appendProp(Property{key: Atom(100 + j), value: Int(i + j)})
			}
			if o.flags&objInlineProps != 0 {
				t.Fatalf("size %d, object %d: growth did not leave inline storage", n, i)
			}
		}
		for i := range int(pool.Count) {
			if got := (*Object)(pool.Objects[i]).vals[0]; got != Int(i) {
				t.Fatalf("size %d, object %d: property storage shared", n, i)
			}
		}
	}
}

// Keeping one pool object must not retain any other object from its refill,
// or the graph reachable from that other object, as a slab allocation would.
func TestJITObjectPoolIndependentLifetimes(t *testing.T) {
	for n := 0; n <= 9; n++ {
		pool := &abi.ObjectPool{Size: abi.PoolSize}
		jitFillObjectPool(pool, nil, nil, n)
		kept := (*Object)(pool.Objects[0])
		other := weak.Make((*Object)(pool.Objects[1]))
		clear(pool.Objects[:])
		runtime.GC()
		if other.Value() != nil {
			t.Fatalf("size %d: keeping one object retains another", n)
		}
		runtime.KeepAlive(kept)
	}
}

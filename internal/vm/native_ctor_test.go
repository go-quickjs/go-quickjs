//go:build amd64 || arm64

package vm

import "testing"

func TestNativeCtorPrototypeSlot(t *testing.T) {
	nativeCounting(t)
	r := New(Config{})
	t.Cleanup(func() { r.Close() })
	v, err := r.Run(compileForTest(t, `
function Pair(a, b) { this.a = a; this.b = b }
Pair.extra = 1;
function make() { return new Pair(1, 2) }
for (var i = 0; i < 100; i++) make();
Pair`))
	if err != nil {
		t.Fatal(err)
	}
	// Keep a cache across these turns: the runtime releases its call-site
	// caches at each turn's end.
	e := &regCallCache{callee: v.Object(), kind: regCallConstruct}
	if obj := r.regCtorThis(e, e.callee.fn()); obj == nil {
		t.Fatal("settled constructor was not allocated")
	}
	if e.this.ref == nil || (*shape)(e.this.ref) != e.callee.shape {
		t.Fatal("constructor's prototype slot was not cached")
	}
	if got := e.callee.shape.keys[int(e.this.num)].key; got != atomPrototype {
		t.Fatalf("cached key %d, want prototype", got)
	}
	oldShape := e.callee.shape
	for _, marking := range []bool{false, true} {
		nativeMarking(t, marking)
		v, err = r.Run(compileForTest(t, `
var p = { tag: 7 }; Pair.prototype = p;
var x; for (var i = 0; i < 20; i++) x = make();
[Object.getPrototypeOf(x) === p, x.a, x.b].join()`))
		if err != nil || v.String().Go() != "true,1,2" {
			t.Fatalf("marking %v: %v, %v", marking, v, err)
		}
		if e.callee.shape != oldShape {
			t.Fatal("assigning prototype changed the constructor's shape")
		}
		obj := r.regCtorThis(e, e.callee.fn())
		if obj == nil || obj.proto() != e.callee.vals()[int(e.this.num)].Object() {
			t.Fatal("cached slot did not read the new prototype")
		}
	}
	v, err = r.Run(compileForTest(t, `
delete Pair.extra; Pair.another = 2;
Pair.prototype = 3;
var x = make();
var bare = Object.getPrototypeOf(x) === Object.prototype;
Pair.prototype = { set a(v) { this.seen = v * 3 } };
var y = make();
[bare, y.seen, y.b, Object.hasOwn(y, "a")].join()`))
	if err != nil || v.String().Go() != "true,3,2,false" {
		t.Fatalf("changed shape and prototype: %v, %v", v, err)
	}
	r.regCtorThis(e, e.callee.fn())
	if (*shape)(e.this.ref) != e.callee.shape || e.callee.shape == oldShape {
		t.Fatal("changed constructor shape did not refresh the slot")
	}
}

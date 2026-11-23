//go:build amd64 || arm64

package vm

import (
	"fmt"
	"sync/atomic"
	"testing"
)

const nativeForwardClasses = `
function create() { return function () { this.initialize.apply(this, arguments) } }
function init(a, b) { this.a = a; this.b = b }
var cs = [];
for (var k = 0; k < 24; k++) {
 var C = create();
 %s;
 cs.push(C);
}
function run(n) {
 var s = 0;
 for (var i = 0; i < n; i++) for (var j = 0; j < cs.length; j++) {
  var p = new cs[j](i, 1); s += p.a + p.b;
 }
 return s;
}
run(100);`

func TestNativeForwardClasses(t *testing.T) {
	nativeCounting(t)
	for _, tc := range nativeForwardPrototypes {
		for _, marking := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/marking=%v", tc.name, marking), func(t *testing.T) {
				nativeMarking(t, marking)
				r := newTestRuntime(t, Config{}.withNative(true))
				defer r.Close()
				if _, err := r.Run(compileForTest(t, fmt.Sprintf(nativeForwardClasses, tc.src))); err != nil {
					t.Fatal(err)
				}
				cl := globalObject(r, "cs").elemsOf()[0].Object().fn().closure
				if cl.fwd == nil || cl.ic[int(cl.reg.fwdSites&0xffff)].fills < maxCacheFills {
					t.Fatal("the classes did not warm the forwarder's table")
				}
				fn := compileForTest(t, "run(100)")
				before := atomic.LoadInt64(&nativeCountsOf(t).calls)
				v, err := r.Run(fn)
				if err != nil || v.Number() != 121200 {
					t.Fatalf("got %v, %v", v, err)
				}
				if frames := atomic.LoadInt64(&nativeCountsOf(t).calls) - before; frames != 0 {
					t.Fatalf("warmed constructions entered %d frames", frames)
				}
			})
		}
	}
}

var nativeForwardPrototypes = []struct{ name, src string }{
	{"default", "C.prototype.initialize = init"},
	{"literal", "C.prototype = { initialize: init }"},
}

func BenchmarkNativeForwardClasses(b *testing.B) {
	for _, tc := range nativeForwardPrototypes {
		b.Run(tc.name, func(b *testing.B) {
			r := newTestRuntime(b, Config{}.withNative(true))
			b.Cleanup(func() { r.Close(); r.ReleaseClosed() })
			if _, err := r.Run(compileForTest(b, fmt.Sprintf(nativeForwardClasses, tc.src))); err != nil {
				b.Fatal(err)
			}
			fn := compileForTest(b, "run(1000)")
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				v, err := r.Run(fn)
				if err != nil || v.Number() != 12012000 {
					b.Fatalf("got %v, %v", v, err)
				}
			}
		})
	}
}

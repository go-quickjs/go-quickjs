package vm

import (
	"runtime"
	"testing"
	"weak"
)

// TestCaptureCellsKeepNoFunction pins what a binding a closure shares keeps
// alive: the binding, not the function object of the closure that first
// captured it. Each maker returns a reader and a writer over the same
// locals; the reader, with a large array on a property of its own, is
// dropped and the writer kept, and the reader must be collected.
func TestCaptureCellsKeepNoFunction(t *testing.T) {
	for _, tt := range []struct{ name, src string }{
		{"three cells", `function outer(x, y) { return function mk() { var a = 1, b = 2, c = 3; var get = function () { return x + y + a + b + c }; var set = function (v) { a = v; b++; c++ }; return [get, set] } }`},
		{"four cells", `function outer(x0, x1, x2, x3, x4, x5, x6, x7, x8, x9, x10) { return function mk() { var a = 1, b = 2, c = 3, d = 4; var get = function () { return x0 + x1 + x2 + x3 + x4 + x5 + x6 + x7 + x8 + x9 + x10 + a + b + c + d }; var set = function (v) { a = v; b++; c++; d++ }; return [get, set] } }`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestRuntime(t, Config{})
			defer r.Close()
			v, err := r.Run(compileForTest(t, tt.src+`
				var mk = outer(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11), kept = []
				for (var i = 0; i < 64; i++) { var p = mk(); p[0].big = new Array(1000).fill(i); kept.push(p) }
				var gets = kept.map(function (p) { return p[0] })
				kept = kept.map(function (p) { return p[1] }); p = undefined
				gets`))
			if err != nil {
				t.Fatal(err)
			}
			var gone []weak.Pointer[Object]
			for _, g := range v.Object().elemsOf() {
				gone = append(gone, weak.Make(g.Object()))
			}
			v = Undefined
			if _, err := r.Run(compileForTest(t, `gets = undefined; for (var i = 0; i < kept.length; i++) kept[i](i); kept.length`)); err != nil {
				t.Fatal(err)
			}
			runtime.GC()
			runtime.GC()
			live := 0
			for _, w := range gone {
				if w.Value() != nil {
					live++
				}
			}
			// One may be left in a register of the last call's frame.
			if live > 1 {
				t.Errorf("%d of %d dropped readers kept alive by the cells their writers share", live, len(gone))
			}
			runtime.KeepAlive(r)
		})
	}
}

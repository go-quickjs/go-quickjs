package vm

import "testing"

// The int32 element reads and writes of a global's elements -- Mandreel's
// heap, typed arrays in globals, heap32[(r0+1)] -- read the global in
// place, where its site's slot holds it (ztree_a_int32elem.go), and its
// get_global's node otherwise: the global given another array, made an
// accessor, shadowed by a script's lexical binding, an ordinary array.
// Each answer is the interpreter's.
func TestInt32GlobalElements(t *testing.T) {
	elem, get := i32Elem, i32GetNode
	defer func() { i32Elem, i32GetNode = elem, get }()
	globals := 0
	i32Elem = func(obj, key tentry, s *i32Site) ti32 {
		if obj.global != nil {
			globals++
		}
		return elem(obj, key, s)
	}
	i32GetNode = func(obj, key tentry, pc int) tval {
		if obj.global != nil {
			globals++
		}
		return get(obj, key, pc)
	}
	scripts := []string{`
		heap32 = new Int32Array(64); heapU8 = new Uint8Array(heap32.buffer); var out = [];
		function fill(n) { for (var i = 0; i < n; i++) { var r0 = (i * 4) | 0; heap32[(r0 >> 2) + 1] = (i * 2654435761) | 0 } }
		function sum(n) { var s = 0; for (var i = 0; i < n; i++) { var r0 = i | 0; s = (s + (heap32[(r0 + 1)] >> 3)) | 0; s = (s ^ heapU8[(r0 << 2)]) | 0 } return s }
		function keyed(n) { var s = 0; for (var k = 1; k < n; k++) { s = (s + heap32[k]) | 0 } return s }
		function read(i) { return heap32[(i | 0) + 1] }
		function round() { fill(32); return [sum(30), keyed(30), read(3), read(70)].join(',') }
		for (var j = 0; j < 4; j++) out.push(round());
		heap32 = new Int32Array(64); heap32[4] = 7;
		out.push(round(), read(3));
		Object.defineProperty(globalThis, 'heap32', { get() { return [9, 8, 7, 6, 5] }, configurable: true });
		out.push(sum(3), keyed(4), read(1));
		Object.defineProperty(globalThis, 'heap32', { value: new Int32Array(64), writable: true, configurable: true });
		out.push(round());
		heap32 = [1, 2, 3, 4, 5, 6];
		out.push(sum(4), keyed(5), read(2));
		heap32 = new Int32Array(64);
		out.join(' | ')`,
		`let heap32 = new Int32Array(64); heap32[2] = 5; out.push(round(), sum(2)); out.join(' | ')`,
	}
	var want []string
	defer SetTreeTier(true)
	for _, tree := range []bool{false, true} {
		SetTreeTier(tree)
		globals = 0
		r := New(Config{})
		var got []string
		for _, src := range scripts {
			v, err := r.Run(compileForTest(t, src))
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, v.String().Go())
		}
		r.Close()
		if !tree {
			want = got
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("script %d: the trees gave\n%s\nthe interpreter\n%s", i, got[i], want[i])
			}
		}
		if globals < 3 {
			t.Errorf("tree tier: %d element reads of a global built in place", globals)
		}
	}
}

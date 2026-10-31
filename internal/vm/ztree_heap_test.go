package vm

import "testing"

// A global's element read into a local or as a value, and a local, a
// constant or such a read written to it (heapAt), at a local key, a local
// and a constant, or either under a bitwise operator, and a read a bitwise
// operator takes, as Mandreel reads and writes its heap: typed arrays of each
// kind, a key that is no number -- a string the addition joins or the
// operator converts, an object whose valueOf runs, one whose valueOf
// changes the local stored, which is read after it -- a sum that is no
// int32, a key that is negative, fractional, NaN, past the end or no
// index, a store of a value that is no number or no int32, a key's local
// the store writes, set_local_get, get_local2, and the global made a plain
// array, an accessor, deleted, and shadowed by a let. Each answer is the
// interpreter's, and Node's.
func TestTreeGlobalHeap(t *testing.T) {
	src := `
		heap32 = new Int32Array(16); heapFloat = new Float32Array(16); heapU16 = new Uint16Array(16); heapD = new Float64Array(16);
		for (var i = 0; i < 16; i++) { heap32[i] = i * 3 - 7; heapFloat[i] = i / 4; heapU16[i] = 65535 - i; heapD[i] = i / 3 }
		function rd(fp) {
			var r0, r1, f0, s = '';
			r0 = heap32[(fp)];
			r1 = heap32[(fp+1)];
			f0 = heapFloat[(fp+2)];
			s += r0 + ',' + r1 + ',' + f0 + ',';
			r0 = heapU16[(fp+4)>>1];
			r1 = heapD[(fp+3)];
			s += r0 + ',' + r1 + ',';
			r1 = heap32[(r1)];
			s += r1 + ',';
			fp = heap32[(fp+1)];
			return s + fp;
		}
		function wr(fp, v) {
			heap32[(fp)] = v;
			heapFloat[(fp+1)] = v;
			heapD[(fp+2)] = v;
			return heap32[(fp)] + ',' + heapFloat[(fp+1)] + ',' + heapD[(fp+2)];
		}
		function sh(fp, v) {
			var r0, r1, s = '';
			r0 = heap32[fp>>2];
			r1 = heapU16[(fp+6)>>1];
			s += r0 + ',' + r1 + ',';
			r0 = heap32[(fp+4)>>>2];
			r1 = heap32[fp>>>2];
			s += r0 + ',' + r1 + ',';
			heap32[(fp+8)>>2] = v;
			heapU16[fp>>1] = v;
			return s + heap32[(fp+8)>>2] + ',' + heapU16[fp>>1];
		}
		function mix(fp, f1) {
			var f0, r0, s = '';
			f0 = f1 * heapFloat[(fp+2)];
			r0 = heap32[(fp)] + heapU16[(fp+2)>>1];
			s += f0 + ',' + r0 + ',' + (heap32[(fp+1)] | 0) + ',' + (heapD[fp>>0] < 1) + ',';
			heap32[(fp+1)] = 3;
			heapD[fp>>0] = heap32[(fp+1)];
			heapFloat[(fp)] = 2.5;
			heapD[(fp+3)>>0] = heap32[(fp+2)];
			heap32[(fp)] = heapFloat[(fp+1)>>>0];
			heap32[(fp+2)] = heap32[(fp+2)] + 1;
			s += String(heap32[(fp+1)]) + ',' + heapD[fp] + ',' + heapFloat[(fp)] + ',' + heapD[(fp+3)] + ',' + heap32[(fp)] + ',' + heap32[(fp+2)];
			return s + ',' + Math.max(heap32[(fp)], heapU16[(fp)]);
		}
		function order(fp) {
			var f0 = 1, key = { valueOf: function () { f0 = 99; return fp } };
			heapD[(key+1)] = f0;
			heapD[(key)] = f0;
			return heapD[fp + 1] + ',' + heapD[fp];
		}
		var out = [];
		for (var i = 0; i < 3; i++) out.push(order(i));
		for (var i = 0; i < 4; i++) out.push(rd(i * 2));
		out.push(rd(-1), rd(1.5), rd(40), rd('1'), rd({ valueOf: function () { return 3 } }), rd(4294967294));
		for (var i = 0; i < 4; i++) out.push(wr(i, i * 1.75 - 3));
		for (var i = 0; i < 4; i++) out.push(sh(i * 4, i * 7 - 9));
		for (var i = 0; i < 4; i++) out.push(mix(i * 2, i + 0.5));
		out.push(mix(-1, 2), mix('2', 3), mix(30, 1), mix({ valueOf: function () { return 1 } }, 4));
		out.push(sh(2147483647, 1), sh(-8, 2), sh('8', 3), sh({ valueOf: function () { return 12 } }, 4), sh(1.5, 5), sh(NaN, 6), sh(4, '7'), sh(4, 1e10));
		out.push(wr(1, { valueOf: function () { return 9.5 } }), wr(2, 'x'), wr(40, 5), wr(-1, 5), wr(0.5, 5), wr('2', 7));
		out.push(Array.prototype.join.call(heap32), Object.keys(heap32).length, heap32[-1], heap32['0.5']);
		heap32 = [10, 20, 30, , 50]; heapFloat = { 2: 'two', 3: 'three', 4: 'four' };
		out.push(rd(0), rd(2), wr(3, 'w'), heap32.length, String(heap32[4]));
		Object.defineProperty(globalThis, 'heapU16', { get: function () { out.push('get'); return [1, 2, 3, 4, 5, 6, 7, 8] }, configurable: true });
		out.push(rd(1));
		heapD = null;
		try { rd(0) } catch (e) { out.push(e.constructor.name) }
		try { wr(0, 1) } catch (e) { out.push(e.constructor.name) }
		heapD = new Float64Array(8);
		delete globalThis.heapU16;
		try { rd(0) } catch (e) { out.push(e.constructor.name) }
		out.join(' | ')`
	var want string
	defer SetTreeTier(true)
	for _, tree := range []bool{false, true} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		got := v.String().Go()
		if !tree {
			want = got
			continue
		}
		if got != want {
			t.Errorf("the trees gave\n%s\nthe interpreter\n%s", got, want)
		}
	}
	t.Log(want)
}

// A let at the top level shadows the global the heap nodes read in place:
// a script run after the first declares heap32, and the functions the
// first made then read the let's array.
func TestTreeGlobalHeapShadowed(t *testing.T) {
	first := `
		heap32 = new Int32Array([1, 2, 3, 4]);
		function rd(fp) { var r0; r0 = heap32[(fp+1)]; return r0 }
		function wr(fp, v) { heap32[(fp)] = v }
		var s = 0;
		for (var i = 0; i < 50; i++) s += rd(i & 1);
		String(s)`
	second := `
		let heap32 = [7, 8, 9];
		wr(0, 70);
		[rd(0), rd(1), heap32[0], globalThis.heap32[0]].join(',')`
	defer SetTreeTier(true)
	for _, tree := range []bool{false, true} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, first))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != "125" {
			t.Errorf("tree tier %v: the first script gave %s", tree, got)
		}
		v, err = r.Run(compileForTest(t, second))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != "8,9,70,1" {
			t.Errorf("tree tier %v: got %s, want 8,9,70,1", tree, got)
		}
	}
}

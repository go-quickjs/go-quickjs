package vm

import "testing"

// The index keys of a sparse array filled from its end down, and of an
// object given them so, and of an object and an array given them in no
// order, come out of Object.keys in order, and setting the
// array's length below them deletes them from the highest down, stopping at
// none but a key that may not go: Node's answers (v26.10.0). The keys had
// been sorted in the square of their number (zzz_sortindexed.go), seconds
// for these.
func TestSortIndexedKeys(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		function mk(n, step) { var a = []; for (var i = n - 1; i >= 0; i -= step) a[i] = i; return a }
		function mkObj(n) { var o = {}; for (var i = n - 1; i >= 0; i -= 2) o[i] = i; o.x = 1; return o }
		var a = mk(150000, 3), o = mkObj(100000);
		var k = Object.keys(a), ko = Object.keys(o);
		var sorted = true; for (var i = 1; i < k.length; i++) if (+k[i - 1] >= +k[i]) sorted = false;
		var b = mk(150000, 3); Object.defineProperty(b, 2, { value: 0, configurable: false, writable: true, enumerable: true }); b.length = 10;
		var c = mk(150000, 3); c.length = 100;
		var d = mk(150000, 3); Object.defineProperty(d, 149998, { value: 0, configurable: false }); d.length = 0;
		var p = {}; for (var i = 0; i < 10007; i++) p[(i * 7919) % 10007 + 1] = i; var kp = Object.keys(p); var ps = true; for (var i = 1; i < kp.length; i++) if (+kp[i - 1] >= +kp[i]) ps = false;
		var q = []; for (var i = 0; i < 5000; i++) q[((i * 7919) % 5003) * 3 + 4000] = i; var kq = Object.keys(q); var qs = true; for (var i = 1; i < kq.length; i++) if (+kq[i - 1] >= +kq[i]) qs = false;
		k.length + ':' + k[0] + ':' + k[k.length - 1] + ':' + sorted + '|' + ko.length + ':' + ko[0] + ':' + ko[ko.length - 2] + ':' + ko[ko.length - 1] +
			'|' + b.length + ':' + Object.keys(b).join() + '|' + c.length + ':' + Object.keys(c).join() + '|' + d.length + ':' + Object.keys(d).length +
			'|' + kp.length + ':' + ps + ':' + kp[0] + ':' + kp[kp.length - 1] + '|' + kq.length + ':' + qs + ':' + kq[0] + ':' + kq[kq.length - 1] + ':' + q.length`))
	if err != nil {
		t.Fatal(err)
	}
	const want = "50000:2:149999:true|50001:1:99999:x|10:2,5,8|100:2,5,8,11,14,17,20,23,26,29,32,35,38,41,44,47,50,53,56,59,62,65,68,71,74,77,80,83,86,89,92,95,98|149999:49999|10007:true:1:10007|5000:true:4000:19006:19007"
	if got := v.String().Go(); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

package vm

import "testing"

// TestPushPopWithoutFrame pins Array.prototype.push and pop called from a
// script: on a dense array of its own they are applied without a native
// frame, as Math's functions are, and anything else -- a hole the
// prototype answers for, a frozen array, a length that is not writable, an
// array-like, a setter on the prototype chain -- is the built-in's own
// work, with its errors. The output is Node's.
func TestPushPopWithoutFrame(t *testing.T) {
	const src = `var out = [];
function run() {
	var a = [];
	for (var i = 0; i < 5; i++) out.push(a.push(i, i * 10));
	var s = 0;
	while (a.length) s += a.pop();
	out.push(s, a.pop(), a.length);
	var h = [1, , 3];
	h.pop();
	Array.prototype[1] = "proto";
	out.push(h.pop(), h.length);
	delete Array.prototype[1];
	var f = Object.freeze([1, 2]);
	try { f.push(3) } catch (e) { out.push(e instanceof TypeError, f.length) }
	try { f.pop() } catch (e) { out.push(e instanceof TypeError, f.length) }
	class Sub extends Array {}
	var sub = new Sub();
	sub.push(7);
	out.push(sub.pop(), sub instanceof Sub);
	var like = {length: 1, 0: "x"};
	out.push(Array.prototype.push.call(like, "y"), like.length, Array.prototype.pop.call(like), like.length);
	var nw = [1, 2, 3];
	Object.defineProperty(nw, "length", {writable: false});
	try { nw.pop() } catch (e) { out.push(e instanceof TypeError, nw.length, nw[2]) }
	var setter = [];
	Object.defineProperty(Object.prototype, "0", {set: function (v) { out.push("set " + v) }, configurable: true});
	setter.push("s");
	out.push(setter.length);
	delete Object.prototype[0];
}
for (var k = 0; k < 3; k++) run();
JSON.stringify(out);
`
	const want = `[2,4,6,8,10,110,null,0,"proto",1,true,2,true,2,7,true,2,2,"y",1,true,3,null,"set s",1,2,4,6,8,10,110,null,0,"proto",1,true,2,true,2,7,true,2,2,"y",1,true,3,null,"set s",1,2,4,6,8,10,110,null,0,"proto",1,true,2,true,2,7,true,2,2,"y",1,true,3,null,"set s",1]`
	defer SetTreeTier(true)
	for _, tree := range []bool{false, true} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree %v:\n got %s\nwant %s", tree, got, want)
		}
		r.Close()
	}
}

// TestTruncateClearsSlot pins that shortening an array releases what it
// held past its new length, one element at a time as a whole run at once,
// so that nothing it held stays reachable through its storage.
func TestTruncateClearsSlot(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `var a = [{}, {}, {}, {}, {}, {}]; a.length = 5; a.length = 4; a.length = 1; a`))
	if err != nil {
		t.Fatal(err)
	}
	o := v.Object()
	if len(o.elemsOf()) != 1 || !o.elemsOf()[0].IsObject() {
		t.Fatalf("length %d", len(o.elemsOf()))
	}
	for i, e := range o.elemsOf()[:6][1:] {
		if e.ref != nil || e.num != 0 {
			t.Errorf("slot %d still holds %v", i+1, e)
		}
	}
}

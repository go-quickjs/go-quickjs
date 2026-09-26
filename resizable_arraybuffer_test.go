package quickjs_test

import "testing"

func TestResizableArrayBuffer(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		{`var b = new ArrayBuffer(2, {maxByteLength: 8}); var r = [b.resizable, b.maxByteLength, b.byteLength];
		  b.resize(6); r.push(b.byteLength); b.resize(1); r.push(b.byteLength); r.join()`, "true,8,2,6,1"},
		{`var b = new ArrayBuffer(2); [b.resizable, b.maxByteLength].join()`, "false,2"},
		// What a resize gives back reads as zero, even bytes that were there
		// before a shrink took them away.
		{`var b = new ArrayBuffer(2, {maxByteLength: 4}); new Uint8Array(b).fill(9);
		  b.resize(1); b.resize(4); [...new Uint8Array(b)].join()`, "9,0,0,0"},
		{`var errs = [];
		  for (var f of [() => new ArrayBuffer(4, {maxByteLength: 2}),
		                 () => new ArrayBuffer(0, {maxByteLength: 2 ** 50}),
		                 () => new ArrayBuffer(0, {maxByteLength: 2}).resize(3),
		                 () => new ArrayBuffer(2).resize(1)]) {
		    try { f() } catch (e) { errs.push(e.constructor.name) }
		  }
		  errs.join()`, "RangeError,RangeError,RangeError,TypeError"},
		// transfer keeps a buffer resizable; transferToFixedLength does not.
		{`var b = new ArrayBuffer(2, {maxByteLength: 8});
		  [b.transfer().resizable, new ArrayBuffer(2, {maxByteLength: 8}).transferToFixedLength().resizable].join()`,
			"true,false"},
		// A view with no length of its own tracks the buffer; one with a
		// fixed length goes out of bounds when the buffer no longer holds it,
		// and comes back when it does.
		{`var b = new ArrayBuffer(4, {maxByteLength: 8});
		  var track = new Uint8Array(b), fixed = new Uint8Array(b, 1, 2), off = new Uint8Array(b, 2);
		  var r = []; var see = () => r.push([track.length, fixed.length, fixed.byteOffset, off.length].join("/"));
		  see(); b.resize(6); see(); b.resize(2); see(); b.resize(1); see(); b.resize(4); see(); r.join(" ")`,
			"4/2/1/2 6/2/1/4 2/0/0/0 1/0/0/0 4/2/1/2"},
		{`var b = new ArrayBuffer(4, {maxByteLength: 8}); var fixed = new Uint8Array(b, 0, 4); b.resize(2);
		  var errs = [];
		  for (var f of [() => fixed.fill(1), () => fixed.at(0), () => [...fixed]]) {
		    try { f() } catch (e) { errs.push(e.constructor.name) }
		  }
		  errs.join() + "|" + fixed[0] + "|" + Object.keys(fixed).length`, "TypeError,TypeError,TypeError|undefined|0"},
		// subarray of a tracking view with no end tracks too.
		{`var b = new ArrayBuffer(4, {maxByteLength: 8}); var s = new Uint8Array(b).subarray(1);
		  b.resize(8); s.length`, "7"},
		// A DataView tracks the same way, and one out of bounds throws.
		{`var b = new ArrayBuffer(4, {maxByteLength: 8}); var v = new DataView(b), f = new DataView(b, 0, 4);
		  b.resize(8); var r = [v.byteLength, f.byteLength]; b.resize(2);
		  try { f.getUint8(0) } catch (e) { r.push(e.constructor.name) }
		  try { f.byteLength } catch (e) { r.push(e.constructor.name) }
		  r.push(v.byteLength); r.join()`, "8,4,TypeError,TypeError,2"},
		// An assignment's value is converted before its index is checked, so a
		// valueOf that grows the buffer lands the write.
		{`var b = new ArrayBuffer(0, {maxByteLength: 1}); var ta = new Int8Array(b);
		  ta[0] = {valueOf() { b.resize(1); return 100 }}; ta[0]`, "100"},
		// A method settles the length before it runs user code.
		{`var b = new ArrayBuffer(1, {maxByteLength: 4}); var ta = new Int8Array(b);
		  ta.fill({valueOf() { b.resize(4); return 5 }}); [...ta].join()`, "5,0,0,0"},
		{`var b = new ArrayBuffer(4, {maxByteLength: 8}); var ta = new Uint8Array(b); ta.set([0, 1, 2, 3]);
		  ta.copyWithin({valueOf() { b.resize(3); return 2 }}, 1); [...ta].join()`, "0,1,1"},
		// Nothing over a resizable buffer can promise not to change length.
		{`var b = new ArrayBuffer(4, {maxByteLength: 8}); var errs = [];
		  for (var ta of [new Uint8Array(b), new Uint8Array(b, 0, 0)]) {
		    try { Object.freeze(ta) } catch (e) { errs.push(e.constructor.name) }
		    errs.push(Reflect.preventExtensions(ta));
		  }
		  errs.join()`, "TypeError,false,TypeError,false"},
		{`ArrayBuffer.prototype.resize.length`, "1"},
	})
}

// TestArrayAtIsGeneric pins that Array.prototype.at reads through the length
// property and ordinary gets, rather than an array's dense storage.
func TestArrayAtIsGeneric(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		{`[Object.freeze([1, 2]).at(0), Array.prototype.at.call({length: 1, 0: 5}, 0), [, 7].at(1),
		   Array.prototype.at.call("ab", -1), Array.prototype.at.call(new Uint8Array([1, 2, 3]), -1),
		   [1].at(1), [].at(0)].join()`, "1,5,7,b,3,,"},
	})
}

package quickjs_test

import "testing"

func TestImmutableArrayBuffer(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		// transferToImmutable detaches the source and copies to the new length.
		{`var a = new Uint8Array([1, 2, 3]).buffer; var b = a.transferToImmutable(4);
		  [a.detached, b.immutable, b.resizable, b.byteLength, [...new Uint8Array(b)]].join("|")`,
			"true|true|false|4|1,2,3,0"},
		{`var a = new Uint8Array([1, 2, 3, 4]).buffer; var b = a.sliceToImmutable(1, -1);
		  [a.detached, a.immutable, b.immutable, [...new Uint8Array(b)]].join("|")`,
			"false|false|true|2,3"},
		// An immutable buffer can be neither transferred nor sliced into.
		{`var b = new ArrayBuffer(2).transferToImmutable(); var errs = [];
		  for (var f of [() => b.transfer(), () => b.transferToFixedLength(), () => b.transferToImmutable()]) {
		    try { f() } catch (e) { errs.push(e.constructor.name) }
		  }
		  errs.join() + "|" + b.slice(0, 1).immutable`, "TypeError,TypeError,TypeError|false"},
		{`class B extends ArrayBuffer { static get [Symbol.species]() {
		    return function (n) { return new ArrayBuffer(n).transferToImmutable() } } }
		  try { new B(4).slice(0, 2) } catch (e) { e.constructor.name }`, "TypeError"},
		// Every write through a view is refused, before an argument is read.
		{`var ta = new Uint8Array(new Uint8Array([3, 1, 2]).buffer.transferToImmutable());
		  var read = 0, v = {valueOf() { read++; return 9 }}; var errs = [];
		  for (var f of [() => ta.fill(v), () => ta.set([v]), () => ta.copyWithin(v, 0), () => ta.reverse(),
		                 () => ta.sort(), () => ta.setFromHex("ff"), () => new DataView(ta.buffer).setUint8(v, v)]) {
		    try { f() } catch (e) { errs.push(e.constructor.name) }
		  }
		  errs.join() + "|" + read + "|" + [...ta]`,
			"TypeError,TypeError,TypeError,TypeError,TypeError,TypeError,TypeError|0|3,1,2"},
		// Reading and copying out are fine.
		{`var ta = new Uint8Array(new Uint8Array([3, 1, 2]).buffer.transferToImmutable());
		  [ta.toSorted(), ta.map(x => x * 2), ta.slice(1), ta.subarray(1).buffer === ta.buffer].join("|")`,
			"1,2,3|6,2,4|1,2|true"},
		// An element is a non-writable, non-configurable property. Assigning
		// to it fails without converting the value, even out of range.
		{`"use strict"; var ta = new Uint8Array(new ArrayBuffer(2).transferToImmutable());
		  var d = Object.getOwnPropertyDescriptor(ta, 0); var read = 0; var errs = [];
		  for (var k of [0, 5]) {
		    try { ta[k] = {valueOf() { read++; return 1 }} } catch (e) { errs.push(e.constructor.name) }
		  }
		  [d.writable, d.configurable, d.enumerable, errs, read, Reflect.set(ta, 0, 0)].join("|")`,
			"false|false|true|TypeError,TypeError|0|false"},
		{`var ta = new Uint8Array(new Uint8Array([7]).buffer.transferToImmutable());
		  [Reflect.defineProperty(ta, 0, {value: 7}), Reflect.defineProperty(ta, 0, {value: 8}),
		   Reflect.defineProperty(ta, 0, {writable: true}), Object.isFrozen(Object.freeze(ta))].join()`,
			"true,false,false,true"},
		// A species or from/of result is written into, so it may not be immutable.
		{`var imm = new Uint8Array(new ArrayBuffer(4).transferToImmutable()); var errs = [];
		  var ta = new Uint8Array(2); ta.constructor = {[Symbol.species]: function () { return imm }};
		  for (var f of [() => ta.map(x => x), () => ta.filter(x => true), () => ta.slice(),
		                 () => Uint8Array.from.call(function () { return imm }, [1]),
		                 () => Uint8Array.of.call(function () { return imm }, 1)]) {
		    try { f() } catch (e) { errs.push(e.constructor.name) }
		  }
		  errs.join()`, "TypeError,TypeError,TypeError,TypeError,TypeError"},
		{`try { new ArrayBuffer(1).sliceToImmutable(0, 2 ** 53) ; "ok" } catch (e) { e.constructor.name }`, "ok"},
		{`[ArrayBuffer.prototype.transferToImmutable.length, ArrayBuffer.prototype.sliceToImmutable.length,
		   Object.getOwnPropertyDescriptor(ArrayBuffer.prototype, "immutable").get.name].join()`, "0,2,get immutable"},
	})
}

// TestTypedArrayFromArrayLikeOrder pins that an array-like source's elements
// are read only after the result is constructed, one at a time.
func TestTypedArrayFromArrayLikeOrder(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		{`var log = []; var src = { get length() { log.push("length"); return 2 },
		    get 0() { log.push("0"); return 1 }, get 1() { log.push("1"); return 2 } };
		  var r = Uint8Array.from.call(function (n) { log.push("new " + n); return new Uint8Array(n) }, src, x => x * 10);
		  log.join() + "|" + [...r]`, "length,new 2,0,1|10,20"},
		{`[...Uint8Array.from(new Set([1, 2])), ...Uint8Array.from("12")].join()`, "1,2,1,2"},
	})
}

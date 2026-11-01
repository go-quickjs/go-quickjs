package vm

import "testing"

// dataViewTestSrc reads and writes DataViews through each accessor: indices
// that are integers, fractions, -0, NaN, undefined, a string and an object
// (which convert), at the end of the view and past it, negative, infinite
// and past 2**53; endianness flags of each kind; values that are numbers
// past each type's range, NaN, -0, a string, a boolean, null, undefined, an
// object (counted), a BigInt and a symbol, and BigInts past 64 bits for the
// BigInt accessors; views over a resizable buffer, tracking its length and
// of a fixed length, as it shrinks and grows; a detached buffer, one
// detached by a conversion, a receiver that is no DataView, a
// SharedArrayBuffer and a view at an offset. mix's loop calls them
// directly, each turn at other indices. dataViewTestWant is Node's answer
// (v26.10.0).
const dataViewTestSrc = `var calls = 0
function g(f) { try { return String(f()) } catch (e) { return e.constructor.name } }
function table() {
  var buf = new ArrayBuffer(16), v = new DataView(buf), o = []
  v.setUint8(0, 255); v.setInt8(1, -2); v.setUint16(2, 0x1234); v.setUint16(4, 0x1234, true)
  v.setInt32(6, -5, 1); v.setFloat32(10, 1.5); v.setUint8(14, 3.7); v.setUint8(15, 1e20 + 7)
  o.push(Array.from(new Uint8Array(buf)).join('.'))
  var idx = [0, 1.9, -0, -0.5, NaN, undefined, '3', 14, 15, 16, -1, Infinity, 2 ** 53, { valueOf() { calls++; return 2 } }]
  for (var i of idx) {
    o.push(g(() => v.getUint8(i)) + ',' + g(() => v.getInt16(i)) + ',' + g(() => v.getUint16(i, true)) + ',' + g(() => v.getInt32(i, 1)) +
      ',' + g(() => v.getUint32(i)) + ',' + g(() => v.getFloat32(i, 0)) + ',' + g(() => v.getFloat64(i, {})) + ',' + g(() => v.getFloat16(i, 'x')) +
      ',' + g(() => v.getBigInt64(i, true)) + ',' + g(() => v.getBigUint64(i)))
  }
  o.push(v.getUint8(0) + ',' + v.getUint8())
  var vals = [7, NaN, -0, 1e20, 3.7, -1.5, 65537.9, '7', true, null, undefined, { valueOf() { calls++; return 9 } }, 2n, Symbol()]
  for (var x of vals) {
    var w = new DataView(new ArrayBuffer(8))
    o.push(g(() => (w.setUint8(1, x), w.getUint8(1))) + ',' + g(() => (w.setInt16(0, x, true), w.getInt16(0, true))) + ',' +
      g(() => (w.setUint32(4, x), w.getUint32(4))) + ',' + g(() => (w.setFloat32(0, x, 1), w.getFloat32(0, 1))) + ',' +
      g(() => (w.setFloat64(0, x), w.getFloat64(0))) + ',' + g(() => (w.setFloat16(6, x), w.getFloat16(6))) + ',' + g(() => w.setInt8(8, x)))
  }
  var bigs = [-1n, 5n, 2n ** 70n + 3n, -(2n ** 63n), 5, '7', true]
  for (var b of bigs) {
    var w = new DataView(new ArrayBuffer(8))
    o.push(g(() => (w.setBigInt64(0, b), w.getBigUint64(0))) + ',' + g(() => (w.setBigUint64(0, b, true), w.getBigInt64(0, true))) + ',' + g(() => w.setBigInt64(1, b)))
  }
  var rab = new ArrayBuffer(8, { maxByteLength: 16 }), tv = new DataView(rab, 4), fv = new DataView(rab, 0, 4)
  tv.setUint8(3, 9)
  o.push(tv.getUint8(3) + ',' + g(() => tv.getUint8(4)) + ',' + fv.getUint8(3))
  rab.resize(6)
  o.push(g(() => tv.getUint8(1)) + ',' + g(() => tv.getUint8(2)) + ',' + g(() => tv.setUint8(2, 1)) + ',' + g(() => fv.getUint8(0)))
  rab.resize(3)
  o.push(g(() => tv.getUint8(0)) + ',' + g(() => tv.setUint8(0, 1)) + ',' + g(() => fv.getUint8(0)))
  rab.resize(12)
  o.push(g(() => tv.getUint8(7)) + ',' + g(() => tv.getUint8(8)) + ',' + g(() => fv.getUint8(3)))
  var d = new ArrayBuffer(8), dv = new DataView(d)
  dv.setUint8(0, 1)
  d.transfer()
  o.push(g(() => dv.getUint8(0)) + ',' + g(() => dv.setUint8(0, 1)) + ',' + g(() => dv.getUint8(100)))
  o.push(g(() => DataView.prototype.getUint8.call({}, 0)) + ',' + g(() => DataView.prototype.setUint8.call(new Uint8Array(4), 0, 1)) + ',' +
    g(() => DataView.prototype.getInt32.call(undefined, 0)))
  var sv = new DataView(new SharedArrayBuffer(8))
  sv.setInt32(0, -7); sv.setFloat32(4, 2.5, true)
  o.push(sv.getInt32(0) + ',' + sv.getFloat32(4, true) + ',' + g(() => sv.getInt32(5)))
  var ov = new DataView(buf, 4, 8)
  o.push(ov.getUint8(7) + ',' + g(() => ov.getUint8(8)) + ',' + ov.getUint16(0, true))
  // a conversion that detaches the buffer, then the check
  var cb = new ArrayBuffer(8), cv = new DataView(cb)
  o.push(g(() => cv.setUint8(0, { valueOf() { cb.transfer(); return 1 } })) + ',' + g(() => cv.getUint8({ valueOf() { cb.transfer(); return 0 } })))
  return o.join(' ') + ' calls=' + calls
}
// a loop of direct calls, the index and the value varied by the turn, the
// last turns out of bounds
function mix(v, n) {
  var s = 0, t = ''
  for (var i = 0; i < n; i++) {
    var k = i % 9
    v.setUint8(k, i); v.setInt16(k, -i, true); v.setUint32(k & 3, i * 7, k & 1); v.setFloat64(k & 7, i / 4, true)
    s += v.getUint8(k) + v.getInt16(k + 1) + v.getUint32(k & 3, k & 1) + v.getFloat64(k & 7, true) + v.getInt8(k)
    v.setBigInt64(0, BigInt(i) - 3n, true)
    s += Number(v.getBigInt64(0, true))
  }
  try { v.getUint32(13) } catch (e) { t = e.constructor.name }
  return s + t
}
var mv = new DataView(new ArrayBuffer(16))
table() + ' | ' + mix(mv, 50) + ' | ' + mix(mv, 200)
`

const dataViewTestWant = `255.254.18.52.52.18.251.255.255.255.63.192.0.0.3.0 255,-2,65279,873660159,4294840884,NaN,NaN,NaN,-1387359462293761,18446201139177389055 254,-494,4862,875827966,4262605876,-4.858458537663523e+37,NaN,0.0008535385131835938,-5419372899586,18307752833475936255 255,-2,65279,873660159,4294840884,NaN,NaN,NaN,-1387359462293761,18446201139177389055 255,-2,65279,873660159,4294840884,NaN,NaN,NaN,-1387359462293761,18446201139177389055 255,-2,65279,873660159,4294840884,NaN,NaN,NaN,-1387359462293761,18446201139177389055 255,-2,65279,873660159,4294840884,NaN,NaN,NaN,-1387359462293761,18446201139177389055 52,13364,13364,-82693068,875827963,1.677071139738473e-7,1.9999999816384504,0.2626953125,4611686018344694836,3761652462302265151 3,768,3,RangeError,RangeError,RangeError,RangeError,1.7881393432617188e-7,RangeError,RangeError 0,RangeError,RangeError,RangeError,RangeError,RangeError,RangeError,RangeError,RangeError,RangeError RangeError,RangeError,RangeError,RangeError,RangeError,RangeError,RangeError,RangeError,RangeError,RangeError RangeError,RangeError,RangeError,RangeError,RangeError,RangeError,RangeError,RangeError,RangeError,RangeError RangeError,RangeError,RangeError,RangeError,RangeError,RangeError,RangeError,RangeError,RangeError,RangeError RangeError,RangeError,RangeError,RangeError,RangeError,RangeError,RangeError,RangeError,RangeError,RangeError 18,4660,13330,305411090,305411090,5.6862166791309555e-28,NaN,0.25439453125,-21169425390,1311730647613571071 255,255 7,7,7,7,7,7,RangeError 0,0,0,NaN,NaN,NaN,RangeError 0,0,0,0,0,0,RangeError 0,0,1661992960,100000002004087730000,100000000000000000000,Infinity,RangeError 3,3,3,3.700000047683716,3.7,3.69921875,RangeError 255,-1,4294967295,-1.5,-1.5,-1.5,RangeError 1,1,65537,65537.8984375,65537.9,Infinity,RangeError 7,7,7,7,7,7,RangeError 1,1,1,1,1,1,RangeError 0,0,0,0,0,0,RangeError 0,0,0,NaN,NaN,NaN,RangeError 9,9,9,9,9,9,RangeError TypeError,TypeError,TypeError,TypeError,TypeError,TypeError,TypeError TypeError,TypeError,TypeError,TypeError,TypeError,TypeError,TypeError 18446744073709551615,-1,RangeError 5,5,RangeError 3,3,RangeError 9223372036854775808,-9223372036854775808,RangeError TypeError,TypeError,TypeError 7,7,RangeError 1,1,RangeError 9,RangeError,0 0,RangeError,RangeError,0 TypeError,TypeError,TypeError 0,RangeError,0 TypeError,TypeError,TypeError TypeError,TypeError,TypeError -7,2.5,RangeError 192,RangeError,4660 TypeError,TypeError calls=17 | 4754.25RangeError | 83413RangeError`

// DataView's accessors are native leaves (zzz_dataview.go), applied
// without a frame where nothing they do runs script or throws: each answer
// is Node's, in the tree tier and in the interpreter, and an immutable
// buffer's setters still throw.
func TestDataViewLeaves(t *testing.T) {
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, dataViewTestSrc))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != dataViewTestWant {
			t.Errorf("tree tier %v:\ngot  %s\nwant %s", tree, got, dataViewTestWant)
		}
		v, err = r.Run(compileForTest(t, dataViewImmutableSrc))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got, want := v.String().Go(), "5,TypeError,TypeError,5 5,TypeError,TypeError,5 5,TypeError,TypeError,5"; got != want {
			t.Errorf("tree tier %v: immutable buffer: got %s, want %s", tree, got, want)
		}
	}
	SetTreeTier(true)
	// Every accessor is declared the leaf of its width.
	r := New(Config{})
	defer r.Close()
	for _, spec := range dataViewTypes {
		get, set := dataViewLeafOps[struct{}](spec)
		for name, op := range map[string]uint8{"get" + spec.name: get, "set" + spec.name: set} {
			v, err := r.Run(compileForTest(t, "DataView.prototype."+name))
			if err != nil {
				t.Fatal(err)
			}
			if fd := v.Object().fn(); fd == nil || fd.elemOp != op || leafChecks[op] == nil {
				t.Errorf("%s is not the leaf %d", name, op)
			}
		}
	}
}

// dataViewImmutableSrc writes through a view of an immutable buffer, which
// throws, and reads it, which does not.
const dataViewImmutableSrc = `var b = new ArrayBuffer(8); var v0 = new DataView(b); v0.setUint8(1, 5); var ib = b.transferToImmutable(); var iv = new DataView(ib)
function g(f) { try { return String(f()) } catch (e) { return e.constructor.name } }
var o = []; for (var i = 0; i < 3; i++) o.push(iv.getUint8(1) + ',' + g(() => iv.setUint8(1, 2)) + ',' + g(() => iv.setBigInt64(0, 1n)) + ',' + iv.getUint8(1))
o.join(' ')
`

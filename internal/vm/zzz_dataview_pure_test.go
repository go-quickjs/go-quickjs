package vm

import "testing"

// dataViewPureSrc calls DataView's setters, of every type, big-endian and
// little-endian, in pure bodies -- arrows a call evaluates without a frame
// (pureCall) -- that give up at a getter after the store, that store after
// the getter, that store in their tail where nothing gives up, and that
// are called where their caller gives up after them, or nested. Each store
// adds to what the view held, so that one made twice shows. rx's arrows
// make a RegExp's exec and test, neither global nor sticky, before giving
// up: done twice, they leave RegExp.$1, lastMatch and lastIndex as once.
// dataViewPureWant and "5:c:0" are Node's answers (v26.10.0).
const dataViewPureSrc = `var o = {};
Object.defineProperty(o, 'x', { get() { return 1 } });
function g(f) { return f() }
function g2(f) { return f() + o.x }
var out = []
function kInt8() {
  var dv = new DataView(new ArrayBuffer(16))
  for (var i = 0; i < 5; i++) {
    g(() => (dv.setInt8(0, dv.getInt8(0) + 1), o.x))
    g(() => (dv.setInt8(8, dv.getInt8(8, true) + 1, true), o.x))
    g(() => (o.x, dv.setInt8(0, dv.getInt8(0) + 1)))
    g(() => dv.setInt8(8, dv.getInt8(8, true) + 1, true))
    g2(() => dv.setInt8(0, dv.getInt8(0) + 1))
    g(() => g(() => (dv.setInt8(8, dv.getInt8(8, true) + 1, true), o.x)))
  }
  return 'Int8:' + dv.getInt8(0) + ',' + dv.getInt8(8, true)
}
function kUint8() {
  var dv = new DataView(new ArrayBuffer(16))
  for (var i = 0; i < 5; i++) {
    g(() => (dv.setUint8(0, dv.getUint8(0) + 1), o.x))
    g(() => (dv.setUint8(8, dv.getUint8(8, true) + 1, true), o.x))
    g(() => (o.x, dv.setUint8(0, dv.getUint8(0) + 1)))
    g(() => dv.setUint8(8, dv.getUint8(8, true) + 1, true))
    g2(() => dv.setUint8(0, dv.getUint8(0) + 1))
    g(() => g(() => (dv.setUint8(8, dv.getUint8(8, true) + 1, true), o.x)))
  }
  return 'Uint8:' + dv.getUint8(0) + ',' + dv.getUint8(8, true)
}
function kInt16() {
  var dv = new DataView(new ArrayBuffer(16))
  for (var i = 0; i < 5; i++) {
    g(() => (dv.setInt16(0, dv.getInt16(0) + 1), o.x))
    g(() => (dv.setInt16(8, dv.getInt16(8, true) + 1, true), o.x))
    g(() => (o.x, dv.setInt16(0, dv.getInt16(0) + 1)))
    g(() => dv.setInt16(8, dv.getInt16(8, true) + 1, true))
    g2(() => dv.setInt16(0, dv.getInt16(0) + 1))
    g(() => g(() => (dv.setInt16(8, dv.getInt16(8, true) + 1, true), o.x)))
  }
  return 'Int16:' + dv.getInt16(0) + ',' + dv.getInt16(8, true)
}
function kUint16() {
  var dv = new DataView(new ArrayBuffer(16))
  for (var i = 0; i < 5; i++) {
    g(() => (dv.setUint16(0, dv.getUint16(0) + 1), o.x))
    g(() => (dv.setUint16(8, dv.getUint16(8, true) + 1, true), o.x))
    g(() => (o.x, dv.setUint16(0, dv.getUint16(0) + 1)))
    g(() => dv.setUint16(8, dv.getUint16(8, true) + 1, true))
    g2(() => dv.setUint16(0, dv.getUint16(0) + 1))
    g(() => g(() => (dv.setUint16(8, dv.getUint16(8, true) + 1, true), o.x)))
  }
  return 'Uint16:' + dv.getUint16(0) + ',' + dv.getUint16(8, true)
}
function kInt32() {
  var dv = new DataView(new ArrayBuffer(16))
  for (var i = 0; i < 5; i++) {
    g(() => (dv.setInt32(0, dv.getInt32(0) + 1), o.x))
    g(() => (dv.setInt32(8, dv.getInt32(8, true) + 1, true), o.x))
    g(() => (o.x, dv.setInt32(0, dv.getInt32(0) + 1)))
    g(() => dv.setInt32(8, dv.getInt32(8, true) + 1, true))
    g2(() => dv.setInt32(0, dv.getInt32(0) + 1))
    g(() => g(() => (dv.setInt32(8, dv.getInt32(8, true) + 1, true), o.x)))
  }
  return 'Int32:' + dv.getInt32(0) + ',' + dv.getInt32(8, true)
}
function kUint32() {
  var dv = new DataView(new ArrayBuffer(16))
  for (var i = 0; i < 5; i++) {
    g(() => (dv.setUint32(0, dv.getUint32(0) + 1), o.x))
    g(() => (dv.setUint32(8, dv.getUint32(8, true) + 1, true), o.x))
    g(() => (o.x, dv.setUint32(0, dv.getUint32(0) + 1)))
    g(() => dv.setUint32(8, dv.getUint32(8, true) + 1, true))
    g2(() => dv.setUint32(0, dv.getUint32(0) + 1))
    g(() => g(() => (dv.setUint32(8, dv.getUint32(8, true) + 1, true), o.x)))
  }
  return 'Uint32:' + dv.getUint32(0) + ',' + dv.getUint32(8, true)
}
function kFloat16() {
  var dv = new DataView(new ArrayBuffer(16))
  for (var i = 0; i < 5; i++) {
    g(() => (dv.setFloat16(0, dv.getFloat16(0) + 1.5), o.x))
    g(() => (dv.setFloat16(8, dv.getFloat16(8, true) + 1.5, true), o.x))
    g(() => (o.x, dv.setFloat16(0, dv.getFloat16(0) + 1.5)))
    g(() => dv.setFloat16(8, dv.getFloat16(8, true) + 1.5, true))
    g2(() => dv.setFloat16(0, dv.getFloat16(0) + 1.5))
    g(() => g(() => (dv.setFloat16(8, dv.getFloat16(8, true) + 1.5, true), o.x)))
  }
  return 'Float16:' + dv.getFloat16(0) + ',' + dv.getFloat16(8, true)
}
function kFloat32() {
  var dv = new DataView(new ArrayBuffer(16))
  for (var i = 0; i < 5; i++) {
    g(() => (dv.setFloat32(0, dv.getFloat32(0) + 1.5), o.x))
    g(() => (dv.setFloat32(8, dv.getFloat32(8, true) + 1.5, true), o.x))
    g(() => (o.x, dv.setFloat32(0, dv.getFloat32(0) + 1.5)))
    g(() => dv.setFloat32(8, dv.getFloat32(8, true) + 1.5, true))
    g2(() => dv.setFloat32(0, dv.getFloat32(0) + 1.5))
    g(() => g(() => (dv.setFloat32(8, dv.getFloat32(8, true) + 1.5, true), o.x)))
  }
  return 'Float32:' + dv.getFloat32(0) + ',' + dv.getFloat32(8, true)
}
function kFloat64() {
  var dv = new DataView(new ArrayBuffer(16))
  for (var i = 0; i < 5; i++) {
    g(() => (dv.setFloat64(0, dv.getFloat64(0) + 1.5), o.x))
    g(() => (dv.setFloat64(8, dv.getFloat64(8, true) + 1.5, true), o.x))
    g(() => (o.x, dv.setFloat64(0, dv.getFloat64(0) + 1.5)))
    g(() => dv.setFloat64(8, dv.getFloat64(8, true) + 1.5, true))
    g2(() => dv.setFloat64(0, dv.getFloat64(0) + 1.5))
    g(() => g(() => (dv.setFloat64(8, dv.getFloat64(8, true) + 1.5, true), o.x)))
  }
  return 'Float64:' + dv.getFloat64(0) + ',' + dv.getFloat64(8, true)
}
function kBigInt64() {
  var dv = new DataView(new ArrayBuffer(16))
  for (var i = 0; i < 5; i++) {
    g(() => (dv.setBigInt64(0, dv.getBigInt64(0) + 1n), o.x))
    g(() => (dv.setBigInt64(8, dv.getBigInt64(8, true) + 1n, true), o.x))
    g(() => (o.x, dv.setBigInt64(0, dv.getBigInt64(0) + 1n)))
    g(() => dv.setBigInt64(8, dv.getBigInt64(8, true) + 1n, true))
    g2(() => dv.setBigInt64(0, dv.getBigInt64(0) + 1n))
    g(() => g(() => (dv.setBigInt64(8, dv.getBigInt64(8, true) + 1n, true), o.x)))
  }
  return 'BigInt64:' + dv.getBigInt64(0) + ',' + dv.getBigInt64(8, true)
}
function kBigUint64() {
  var dv = new DataView(new ArrayBuffer(16))
  for (var i = 0; i < 5; i++) {
    g(() => (dv.setBigUint64(0, dv.getBigUint64(0) + 1n), o.x))
    g(() => (dv.setBigUint64(8, dv.getBigUint64(8, true) + 1n, true), o.x))
    g(() => (o.x, dv.setBigUint64(0, dv.getBigUint64(0) + 1n)))
    g(() => dv.setBigUint64(8, dv.getBigUint64(8, true) + 1n, true))
    g2(() => dv.setBigUint64(0, dv.getBigUint64(0) + 1n))
    g(() => g(() => (dv.setBigUint64(8, dv.getBigUint64(8, true) + 1n, true), o.x)))
  }
  return 'BigUint64:' + dv.getBigUint64(0) + ',' + dv.getBigUint64(8, true)
}
function run() { return [kInt8(), kUint8(), kInt16(), kUint16(), kInt32(), kUint32(), kFloat16(), kFloat32(), kFloat64(), kBigInt64(), kBigUint64()].join(' ') }
var re = /b(c)/, sub = 'abcd'
function rx() {
  var n = 0
  for (var i = 0; i < 5; i++) {
    g(() => (re.exec(sub), o.x))
    g(() => (re.test(sub), o.x))
    if (RegExp.$1 === 'c' && RegExp.lastMatch === 'bc' && re.lastIndex === 0) n++
  }
  return n + ':' + RegExp.$1 + ':' + re.lastIndex
}
`

const dataViewPureWant = "Int8:15,15 Uint8:15,15 Int16:15,15 Uint16:15,15 Int32:15,15 Uint32:15,15 Float16:22.5,22.5 Float32:22.5,22.5 Float64:22.5,22.5 BigInt64:15,15 BigUint64:15,15"

// A native leaf that writes -- a DataView's setter -- is applied in a pure
// body only where the body may store (pureInvoke): one that gave up after
// it is evaluated again with a frame, which had written again. Each answer
// is Node's, in the tree tier and in the interpreter.
func TestDataViewSetterInPureBody(t *testing.T) {
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		if _, err := r.Run(compileForTest(t, dataViewPureSrc)); err != nil {
			t.Fatal(err)
		}
		for src, want := range map[string]string{"run()": dataViewPureWant, "rx()": "5:c:0"} {
			v, err := r.Run(compileForTest(t, src))
			if err != nil {
				t.Fatal(err)
			}
			if got := v.String().Go(); got != want {
				t.Errorf("tree tier %v: %s = %s, want Node's %s", tree, src, got, want)
			}
		}
		r.Close()
	}
	SetTreeTier(true)
}

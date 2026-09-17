package quickjs_test

import (
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

func TestDataView(t *testing.T) {
	cases := []struct{ src, want string }{
		// Big-endian is the default, which is what makes the byte order
		// observable through a single-byte read.
		{`var d = new DataView(new ArrayBuffer(8));
		  d.setInt32(0, 0x12345678);
		  [d.getInt32(0), d.getUint8(0), d.getUint8(3)].join(",")`, "305419896,18,120"},
		{`var d = new DataView(new ArrayBuffer(8));
		  d.setInt32(0, 0x12345678, true);
		  [d.getUint8(0), d.getInt32(0, true)].join(",")`, "120,305419896"},

		{`var d = new DataView(new ArrayBuffer(8)); d.setFloat64(0, 1.5); String(d.getFloat64(0))`, "1.5"},
		{`var d = new DataView(new ArrayBuffer(8)); d.setFloat32(0, 1.5, true); String(d.getFloat32(0, true))`, "1.5"},
		{`var d = new DataView(new ArrayBuffer(8)); d.setFloat64(0, NaN); String(d.getFloat64(0))`, "NaN"},

		// Signedness is a reinterpretation of the same bytes.
		{`var d = new DataView(new ArrayBuffer(8)); d.setInt16(0, -2);
		  [d.getInt16(0), d.getUint16(0)].join(",")`, "-2,65534"},
		{`var d = new DataView(new ArrayBuffer(8)); d.setUint8(0, 255); String(d.getInt8(0))`, "-1"},
		{`var d = new DataView(new ArrayBuffer(8)); d.setUint32(0, -1); String(d.getUint32(0))`, "4294967295"},

		// A negative BigInt must wrap in two's complement rather than store its
		// magnitude.
		{`var d = new DataView(new ArrayBuffer(8)); d.setBigInt64(0, -1n); String(d.getBigInt64(0))`, "-1"},
		{`var d = new DataView(new ArrayBuffer(8)); d.setBigInt64(0, -1n); String(d.getBigUint64(0))`, "18446744073709551615"},
		{`var d = new DataView(new ArrayBuffer(8));
		  d.setBigInt64(0, -9223372036854775808n); String(d.getBigInt64(0))`, "-9223372036854775808"},

		{`var d = new DataView(new ArrayBuffer(16), 4, 8);
		  [d.byteOffset, d.byteLength, d.buffer.byteLength].join(",")`, "4,8,16"},
		{`var d = new DataView(new ArrayBuffer(16), 4); String(d.byteLength)`, "12"},

		{`Object.prototype.toString.call(new DataView(new ArrayBuffer(1)))`, "[object DataView]"},
		{`String(ArrayBuffer.isView(new DataView(new ArrayBuffer(1))))`, "true"},
		// A declared length stops at the first optional parameter, so every
		// getter reports one and every setter two however many bytes it moves.
		{`[DataView.prototype.getInt8.length, DataView.prototype.getInt32.length,
		   DataView.prototype.setInt8.length, DataView.prototype.setInt32.length].join(",")`,
			"1,1,2,2"},

		// A DataView and a typed array over the same buffer see each other's
		// writes, which is the point of sharing a buffer.
		{`var b = new ArrayBuffer(4); var d = new DataView(b); var a = new Uint8Array(b);
		  d.setUint32(0, 0x01020304); Array.from(a).join(",")`, "1,2,3,4"},
	}

	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}
}

func TestDataViewErrors(t *testing.T) {
	cases := []struct{ src, want string }{
		{`new DataView(new ArrayBuffer(4)).getInt32(1)`, "RangeError"},
		{`new DataView(new ArrayBuffer(4)).getInt32(-1)`, "RangeError"},
		{`new DataView(new ArrayBuffer(4)).setInt32(4, 0)`, "RangeError"},
		{`new DataView(new ArrayBuffer(4), 8)`, "RangeError"},
		{`new DataView(new ArrayBuffer(4), 0, 8)`, "RangeError"},
		{`new DataView({})`, "TypeError"},
		{`new DataView(new Uint8Array(4))`, "TypeError"},
		// Calling it without new is a TypeError, not a silent construction.
		{`DataView(new ArrayBuffer(4))`, "TypeError"},
		// The BigInt accessors will not convert a number for you.
		{`new DataView(new ArrayBuffer(8)).setBigInt64(0, 1)`, "TypeError"},
		{`DataView.prototype.getInt32.call({}, 0)`, "TypeError"},
	}

	for _, tc := range cases {
		rt := quickjs.New()
		_, err := rt.Eval(tc.src)
		if err == nil {
			t.Errorf("%s: no error, want %s", tc.src, tc.want)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %s", tc.src, err, tc.want)
		}
		rt.Close()
	}
}

// Transferring a buffer hands its storage to a new one and detaches the
// original, which is what makes passing a large buffer around cost nothing:
// there is only ever one owner, so nothing is copied and nothing can be read
// through a stale view.
func TestArrayBufferTransfer(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var b = new ArrayBuffer(8); var c = b.transfer();
		  [b.detached, c.detached, c.byteLength].join(",")`, "true,false,8"},
		// The bytes move rather than being copied and left behind.
		{`var b = new ArrayBuffer(4); new Uint8Array(b)[0] = 7;
		  String(new Uint8Array(b.transfer())[0])`, "7"},
		// A longer target is zero-filled; a shorter one drops the tail.
		{`String(new ArrayBuffer(4).transfer(8).byteLength)`, "8"},
		{`String(new ArrayBuffer(8).transfer(2).byteLength)`, "2"},
		{`var b = new ArrayBuffer(4); new Uint8Array(b)[3] = 9;
		  String(new Uint8Array(b.transfer(2)).length)`, "2"},
		{`String(new ArrayBuffer(1).detached)`, "false"},
		{`typeof ArrayBuffer.prototype.transferToFixedLength`, "function"},
		// A view over a detached buffer reads as undefined rather than throwing.
		{`var b = new ArrayBuffer(4); var v = new Uint8Array(b); b.transfer();
		  String(v[0])`, "undefined"},
	}

	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}

	// A buffer can only be transferred once.
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval(`var b = new ArrayBuffer(4); b.transfer(); b.transfer();`); err == nil {
		t.Error("transferring a detached buffer should throw")
	}
}

// A half stores a number in two bytes: one sign bit, five of exponent and ten
// of mantissa. That is about three decimal digits, which is what half floats
// are for -- image and audio samples, and model weights -- at half the space of
// a Float32Array.
func TestFloat16(t *testing.T) {
	const d = `var d = new DataView(new ArrayBuffer(8)); `
	cases := []struct{ src, want string }{
		{d + `d.setFloat16(0, 1.5); String(d.getFloat16(0))`, "1.5"},
		// Ten mantissa bits, so a tenth is not exactly representable.
		{d + `d.setFloat16(0, 0.1); String(d.getFloat16(0))`, "0.0999755859375"},
		{d + `d.setFloat16(0, Infinity); String(d.getFloat16(0))`, "Infinity"},
		{d + `d.setFloat16(0, -Infinity); String(d.getFloat16(0))`, "-Infinity"},
		{d + `d.setFloat16(0, NaN); String(d.getFloat16(0))`, "NaN"},
		{d + `d.setFloat16(0, -0); String(1 / d.getFloat16(0))`, "-Infinity"},
		// 65504 is the largest half; anything from 65520 up rounds to infinity
		// rather than wrapping.
		{d + `d.setFloat16(0, 65504); String(d.getFloat16(0))`, "65504"},
		{d + `d.setFloat16(0, 65520); String(d.getFloat16(0))`, "Infinity"},
		// The smallest normal and the smallest subnormal.
		{d + `d.setFloat16(0, 6.103515625e-5); String(d.getFloat16(0))`, "0.00006103515625"},
		{d + `d.setFloat16(0, 5.960464477539063e-8); String(d.getFloat16(0))`,
			"5.960464477539063e-8"},
		{d + `d.setFloat16(0, 1e-10); String(d.getFloat16(0))`, "0"},
		// Rounding is to nearest even, like every other float operation.
		{d + `d.setFloat16(0, 2049); String(d.getFloat16(0))`, "2048"},
		{d + `d.setFloat16(0, 2051); String(d.getFloat16(0))`, "2052"},

		{d + `d.setFloat16(0, 1.5, true); [d.getUint8(0), d.getUint8(1)].join(",")`, "0,62"},
		{d + `d.setFloat16(0, 1.5); [d.getUint8(0), d.getUint8(1)].join(",")`, "62,0"},
		{`[DataView.prototype.getFloat16.length, DataView.prototype.setFloat16.length].join(",")`,
			"1,2"},

		{`new Float16Array([1.5, 0.1]).join(",")`, "1.5,0.0999755859375"},
		{`String(Float16Array.BYTES_PER_ELEMENT)`, "2"},
		{`Object.prototype.toString.call(new Float16Array(1))`, "[object Float16Array]"},
		{`var a = new Float16Array(1); a[0] = 1.337; String(a[0])`, "1.3369140625"},

		// f16round is how a script sees what a half would hold without
		// allocating one.
		{`String(Math.f16round(1.337))`, "1.3369140625"},
		{`String(Math.f16round(1.5))`, "1.5"},
		{`String(Math.f16round(NaN))`, "NaN"},
		{`String(Math.f16round.length)`, "1"},
		{`var a = new Float16Array(1); a[0] = 1.337; String(a[0] === Math.f16round(1.337))`,
			"true"},
	}

	for _, tc := range cases {
		rt := quickjs.New()
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		rt.Close()
	}

	for _, src := range []string{
		`new DataView(new ArrayBuffer(2)).getFloat16(1)`,
		`new DataView(new ArrayBuffer(2)).setFloat16(1, 0)`,
		`new DataView(new ArrayBuffer(2)).getFloat16(Infinity)`,
		`DataView.prototype.getFloat16.call({}, 0)`,
	} {
		rt := quickjs.New()
		if _, err := rt.Eval(src); err == nil {
			t.Errorf("%s: accepted, want an error", src)
		}
		rt.Close()
	}
}

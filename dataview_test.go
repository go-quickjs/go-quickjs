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
		// The single-byte accessors take no endianness argument, so their
		// declared length is one less.
		{`[DataView.prototype.getInt8.length, DataView.prototype.getInt32.length,
		   DataView.prototype.setInt32.length].join(",")`, "1,2,3"},

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

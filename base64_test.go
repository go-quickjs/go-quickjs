package quickjs_test

import (
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

func TestUint8ArrayBase64AndHex(t *testing.T) {
	cases := []struct{ src, want string }{
		{`new Uint8Array([72,101,108,108,111]).toBase64()`, "SGVsbG8="},
		{`new Uint8Array([72,101,108,108,111]).toHex()`, "48656c6c6f"},
		{`Array.from(Uint8Array.fromBase64("SGVsbG8=")).join(",")`, "72,101,108,108,111"},
		{`Array.from(Uint8Array.fromHex("48656c6c6f")).join(",")`, "72,101,108,108,111"},
		{`new Uint8Array(0).toBase64() + "|" + new Uint8Array(0).toHex()`, "|"},
		{`String(Uint8Array.fromBase64("").length)`, "0"},

		// base64url swaps + and / for - and _, so the result is safe in a URL.
		{`new Uint8Array([251,255]).toBase64()`, "+/8="},
		{`new Uint8Array([251,255]).toBase64({alphabet: "base64url"})`, "-_8="},
		{`Array.from(Uint8Array.fromBase64("-_8", {alphabet: "base64url"})).join(",")`, "251,255"},

		{`new Uint8Array([1]).toBase64()`, "AQ=="},
		{`new Uint8Array([1]).toBase64({omitPadding: true})`, "AQ"},

		// Encoders wrap long output, so a decoder has to ignore whitespace.
		{`Array.from(Uint8Array.fromBase64("SGVs bG8=")).join(",")`, "72,101,108,108,111"},

		// loose, the default, accepts a final group with its padding missing.
		{`Array.from(Uint8Array.fromBase64("SGVsbG8")).join(",")`, "72,101,108,108,111"},
		{`Array.from(Uint8Array.fromBase64("AQ==", {lastChunkHandling: "strict"})).join(",")`, "1"},
		{`Array.from(Uint8Array.fromBase64("AAE=", {lastChunkHandling: "strict"})).join(",")`, "0,1"},
		// A group that carries bits beyond its whole bytes is accepted loosely
		// and rejected strictly, since the decoder is about to discard them.
		{`Array.from(Uint8Array.fromBase64("AR==")).join(",")`, "1"},

		// The setFrom methods report how much they consumed, which is what
		// makes streaming possible.
		{`var a = new Uint8Array(3);
		  JSON.stringify(a.setFromBase64("SGVsbG8=")) + "|" + Array.from(a).join(",")`,
			`{"read":4,"written":3}|72,101,108`},
		{`JSON.stringify(new Uint8Array(10).setFromBase64("SGVsbG8="))`,
			`{"read":8,"written":5}`},
		{`var a = new Uint8Array(5);
		  JSON.stringify(a.setFromHex("48656c6c6f")) + "|" + Array.from(a).join(",")`,
			`{"read":10,"written":5}|72,101,108,108,111`},
		// stop-before-partial leaves the incomplete group unread, so the caller
		// can prepend it to whatever arrives next.
		{`JSON.stringify(new Uint8Array(10).setFromBase64("SGVsbG8",
		    {lastChunkHandling: "stop-before-partial"}))`, `{"read":4,"written":3}`},
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

func TestUint8ArrayBase64Errors(t *testing.T) {
	cases := []struct{ src, want string }{
		{`Uint8Array.fromBase64("!!!")`, "SyntaxError"},
		// A group of one is never valid: a single 6-bit value cannot carry a
		// byte.
		{`Uint8Array.fromBase64("A")`, "SyntaxError"},
		{`Uint8Array.fromBase64("A===")`, "SyntaxError"},
		{`Uint8Array.fromBase64("AA=A")`, "SyntaxError"},
		{`Uint8Array.fromBase64("SGVsbG8", {lastChunkHandling: "strict"})`, "SyntaxError"},
		{`Uint8Array.fromBase64("AR==", {lastChunkHandling: "strict"})`, "SyntaxError"},
		{`Uint8Array.fromBase64("AAF=", {lastChunkHandling: "strict"})`, "SyntaxError"},
		{`Uint8Array.fromHex("4")`, "SyntaxError"},
		{`Uint8Array.fromHex("zz")`, "SyntaxError"},

		// The input is deliberately not coerced.
		{`Uint8Array.fromBase64(1)`, "TypeError"},
		{`Uint8Array.fromHex(null)`, "TypeError"},
		// Bytes only: a wider view would leave the caller reasoning about byte
		// order.
		{`new Uint16Array(2).toBase64()`, "TypeError"},
		{`Uint8Array.fromBase64("AA", {alphabet: "x"})`, "TypeError"},
		{`Uint8Array.fromBase64("AA", {lastChunkHandling: "x"})`, "TypeError"},
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

// TestUint8ArrayBase64RoundTrip checks that every byte survives both encodings,
// which the table above cannot cover exhaustively.
func TestUint8ArrayBase64RoundTrip(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	v, err := rt.Eval(`
		var all = new Uint8Array(256);
		for (var i = 0; i < 256; i++) all[i] = i;
		var viaBase64 = Uint8Array.fromBase64(all.toBase64());
		var viaURL = Uint8Array.fromBase64(all.toBase64({alphabet: "base64url"}),
		                                   {alphabet: "base64url"});
		var viaHex = Uint8Array.fromHex(all.toHex());
		[viaBase64, viaURL, viaHex].every(
		    out => out.length === 256 && all.every((b, i) => out[i] === b));
	`)
	if err != nil {
		t.Fatal(err)
	}
	if v.String() != "true" {
		t.Errorf("round trip failed: got %s", v.String())
	}
}

package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// A Go string may hold bytes that are not UTF-8. Given to a script, each
// such byte is U+FFFD, as ranging over the Go string decodes it, and the
// string's length, its slices and its code units agree: a slice of one
// used to walk past the end of its bytes and panic.
func TestInvalidUTF8FromGo(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	f, err := rt.Eval(`(s) => [s.length, JSON.stringify(s), JSON.stringify(s.slice(0, s.length)),
		JSON.stringify(s.slice(1)), s.charCodeAt(1), s === s.slice(0, 1) + s.slice(1)].join(" | ")`)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ in, want string }{
		{"x\xffy", `3 | "x�y" | "x�y" | "�y" | 65533 | true`},
		{"ab\xff", `3 | "ab�" | "ab�" | "b�" | 98 | true`},
		{"\xff", `1 | "�" | "�" | "" | NaN | true`},
		{"a\xc3", `2 | "a�" | "a�" | "�" | 65533 | true`},
		{"a\xe2\x82", `3 | "a��" | "a��" | "��" | 65533 | true`},
		// The halves of U+1F600 encoded one by one are the pair.
		{"\xed\xa0\xbd\xed\xb8\x80", `2 | "😀" | "😀" | "\ude00" | 56832 | true`},
		// A lone surrogate stays one.
		{"a\xed\xa0\xbd", `2 | "a\ud83d" | "a\ud83d" | "\ud83d" | 55357 | true`},
	} {
		v, err := f.Call(c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if got := v.String(); got != c.want {
			t.Errorf("%q:\n got  %s\n want %s", c.in, got, c.want)
		}
	}

	// The other ways a Go string reaches a script.
	if err := rt.Set("k\xffz", "v\xff"); err != nil {
		t.Fatal(err)
	}
	rt.Set("fromGo", func() string { return "p\xe2\x82q" })
	rt.Set("throwGo", func() error { return rt.ThrowError("bad \xff byte") })
	// The last element is a string literal whose source holds a raw byte
	// that is not UTF-8.
	v, err := rt.Eval(`[
		Object.keys(globalThis).filter(k => k[0] === "k").map(k => k.length + JSON.stringify(k) + JSON.stringify(globalThis[k])),
		JSON.stringify(fromGo().slice(1)),
		(() => { try { throwGo() } catch (e) { return JSON.stringify(e.message.slice(4)) } })(),
		JSON.stringify('a` + "\xff" + `b'.slice(1)),
	].join(" | ")`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String(), `3"k�z""v�" | "��q" | "� byte" | "�b"`; got != want {
		t.Errorf("\n got  %s\n want %s", got, want)
	}
}

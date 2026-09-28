package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestLengthsPastAnInt32 pins what indices, lengths and offsets past 2^31
// and 2^32 do, which is the same on every platform: on a 32-bit one, where
// an int is 32 bits, they were truncated -- into panics, into writes that went
// to index 0, and into limits and offsets that became 0 (KI-28). Run it with
// GOARCH=386 too.
func TestLengthsPastAnInt32(t *testing.T) {
	stop := `(e) => e === "stop" ? "stopped" : e.name + ": " + e.message`
	for src, want := range map[string]string{
		`[1,2,3][2**31]`:                         "undefined",
		`var a = []; a[2**31+5] = 1; a.length`:   "2147483654",
		`var b = []; b.length = 2**31; b.length`: "2147483648",
		`try { Array.prototype.sort.call({length: 2**31, get 0() { throw "stop" }}) } catch (e) { (` + stop + `)(e) }`: "stopped",
		`try { String.raw({raw: {length: 2**31, get 0() { throw "stop" }}}) } catch (e) { (` + stop + `)(e) }`:         "stopped",
		`var o = {length: 2**32+2}; Array.prototype.fill.call(o, 1, 2**32, 2**32+1); o[2**32]`:                         "1",
		`JSON.stringify("a,b".split(",", 2**32-1))`:                                                                    `["a","b"]`,
		`var r = /a/g; r.lastIndex = 2**32 + 1; r.exec("aaa"); r.lastIndex`:                                            "0",
		`var q = /(?:)/g; q.lastIndex = 2**32 + 1; q[Symbol.replace]("aaa", ""); q.lastIndex`:                          "0",
		`var p = {length: 2**31+2}; Array.prototype.splice.call(p, 2**31, 1); p.length`:                                "2147483649",
		`try { new Uint8Array(new ArrayBuffer(8), 2**32) } catch (e) { e.name }`:                                       "RangeError",
		`try { new Uint8Array(8).set([1], 2**32) } catch (e) { e.name }`:                                               "RangeError",
	} {
		rt := quickjs.New()
		v, err := rt.Eval(src)
		if err != nil || v.String() != want {
			t.Errorf("%s\n = %v, %v; want %s", src, v, err, want)
		}
		rt.Close()
	}
}

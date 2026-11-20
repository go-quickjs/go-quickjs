package vm

import "testing"

// The case conversions of short ASCII strings are remembered by their
// text (asciiCaseMemo): conversions of many texts, to both cases and
// again, more than the memo holds, so that entries are taken over, each
// give the text converted -- checked here against a conversion by code
// units -- and one of a longer text, or of one with nothing to convert,
// as before; a single character's is the runtime's string of it.
func TestCaseMemo(t *testing.T) {
	t.Parallel()
	const src = `
		function byUnits(s, up) {
			var r = ""
			for (var i = 0; i < s.length; i++) {
				var c = s.charCodeAt(i)
				if (up && c >= 97 && c <= 122) c -= 32
				else if (!up && c >= 65 && c <= 90) c += 32
				r += String.fromCharCode(c)
			}
			return r
		}
		var bad = [], n = 0
		for (var round = 0; round < 3; round++) {
			for (var i = 0; i < 300; i++) {
				var s = "aB" + i.toString(36) + (i % 7 ? "-x" : "Q") + (i % 5 ? "" : "zzzzzzzzzzzzzzzz")
				var u = s.toUpperCase(), l = s.toLowerCase()
				if (u !== byUnits(s, true) || l !== byUnits(s, false) || u.toLowerCase() !== l || l.toUpperCase() !== u) bad.push(s)
				n++
			}
		}
		var same = "12-+".toUpperCase() + "12-+".toLowerCase() + "".toUpperCase() + "q".toUpperCase() + "Q".toLowerCase() + "1".toUpperCase() + "Q".toUpperCase()
		n + " " + bad.length + " " + same + " " + "Hello".toUpperCase() + "Hello".toLowerCase()`
	const want = "900 0 12-+12-+Qq1Q HELLOhello"
	r := newTestRuntime(t, Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String().Go(); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

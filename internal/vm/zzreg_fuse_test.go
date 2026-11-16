package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/compiler"
)

// regRunBoth runs src with the register tier off and on, each from a
// compile of its own, and checks both give want and that at least built
// functions ran as register code.
func regRunBoth(t *testing.T, src, want string, built int64) {
	t.Helper()
	for _, reg := range []bool{false, true} {
		compiler.SetRegisterForm(true)
		fn := compileForTest(t, src)
		compiler.SetRegisterForm(false)
		SetRegTier(reg)
		before := RegsBuilt()
		r := New(Config{})
		v, err := r.Run(fn)
		SetRegTier(false)
		if err != nil {
			t.Fatalf("reg=%v: %v", reg, err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("reg=%v: got %s, want %s", reg, got, want)
		}
		if n := RegsBuilt() - before; reg && n < built {
			t.Errorf("only %d functions ran as register code, want %d", n, built)
		}
		r.Close()
	}
}

// TestRegFusedElements pins the register form's fused operations: x[++i]
// and x[--i] of a local, which step the key and read the element as one
// operation, on their fast path and on each way off it -- a key that is no
// number, a hole, an element past the end, an element a getter answers, an
// object that is no array, a key whose step throws -- and the
// compare-and-branches with an upvalue, with numbers and without, and an
// element's store whose value is made in the assignment's own register.
func TestRegFusedElements(t *testing.T) {
	const src = `
		function pre(a) { var i = -1, s = ""; while (i < 4) s += a[++i] + "|"; return s + "@" + i }
		function down(a) { var i = a.length, s = 0; while (i > 0) s += a[--i]; return s + "@" + i }
		function strKey(a) { var i = "0"; return [a[++i], typeof i].join() }
		function objKey(a) { var n = 0, i = { valueOf() { n++; return 1 } }; return [a[++i], i, n].join() }
		function same() { var k = 0; k = [10, 20, 30][++k]; return String(k) }
		function throws(a) { var i = { valueOf() { throw new TypeError("no") } }; try { return a[++i] } catch (e) { return e.message + typeof i } }
		function nullBase() { var i = 0, o = null; try { return o[++i] } catch (e) { return (e instanceof TypeError) + ":" + i } }
		function ups() { var lim = 3, slim = "3"; return upLoop() + "," + upStr()
		function upLoop() { var s = 0; for (var j = 0; j < lim; j++) s += j; for (var k = 0; k <= lim; k++) s += 10; return s }
		function upStr() { var s = 0; for (var j = 0; j < slim; j++) s++; if (j <= slim) s += 100; if (!(j < slim)) s += 1000; return s } }
		function upNaN() { var s = 0, n = NaN; return (function () { for (var j = 0; j < n; j++) s++; if (!(j <= n)) s += 7; return s })() }
		function chain(x) { var last = 0, cur = 0; for (var i = 0; i < 3; i++) last = x[cur] = last + x[++cur]; return [last, x.join()].join("/") }
		function chainSelf(x) { x = x[0] = 5; return String(x) }
		var holes = [1, , 3]; Object.defineProperty(Array.prototype, 4, { get() { return "g" }, configurable: true });
		var out = [pre([1, 2, 3]), pre(holes), down([1, 2, 3]), strKey(["a", "b"]), objKey(["a", "b"]), same(),
			throws([1]), nullBase(), pre({ 0: "o", 1: "p" }), pre("str"), ups(), upNaN(), chain([1, 2, 3, 4]), chainSelf([1])].join(" ");
		delete Array.prototype[4];
		out`
	const want = "1|2|3|undefined|g|@4 1|undefined|3|undefined|g|@4 6@0 b,number ,2,1 20 noobject true:1 o|p|undefined|undefined|undefined|@4 s|t|r|undefined|undefined|@4 43,1103 7 9/2,5,9,4 5"
	regRunBoth(t, src, want, 10)
}

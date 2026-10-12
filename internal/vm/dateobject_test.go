package vm

import "testing"

// TestDateObjects pins Date objects made with their date in the same
// allocation, on both tiers: each form of the constructor, a copy that is
// then changed apart from its original, a subclass, a prototype from
// Reflect.construct, coercion, an invalid date, and the function called
// rather than constructed. The answers are Node's, in any time zone.
func TestDateObjects(t *testing.T) {
	defer SetTreeTier(true)
	const src = `function f() {
  var out = [];
  var a = new Date(0), b = new Date(a), c = new Date("1970-01-02T00:00:00Z"), d = new Date(Date.UTC(2020, 1, 29, 12)), e = new Date(NaN);
  out.push(a.getTime(), b.getTime(), c.getTime(), d.toISOString(), String(e.getTime()), e.toString());
  b.setUTCFullYear(2001); out.push(a.getTime(), b.toISOString(), Object.prototype.toString.call(b), b instanceof Date);
  var g = new Date(2020, 0, 1, 10, 20, 30, 400); out.push(g.getFullYear(), g.getMonth(), g.getHours(), g.getMilliseconds());
  class D extends Date { tag() { return "D" + this.getUTCDate(); } }
  var h = new D(86400000 * 3); out.push(h.tag(), h instanceof D, Object.getPrototypeOf(h) === D.prototype);
  var p = new Date({ valueOf: function () { return 5; } }); out.push(p.getTime(), new Date(8.64e15 + 1).getTime(), new Date(true).getTime());
  var q = Reflect.construct(Date, [7], Object); out.push(Object.getPrototypeOf(q) === Object.prototype, Date.prototype.getTime.call(q));
  out.push(typeof Date(), Object.keys(a).length, JSON.stringify({ a: a }));
  return out.join("|");
}
f()`
	const want = `0|0|86400000|2020-02-29T12:00:00.000Z|NaN|Invalid Date|0|2001-01-01T00:00:00.000Z|[object Date]|true|2020|0|10|400|D4|true|true|5|NaN|1|true|7|string|0|{"a":"1970-01-01T00:00:00.000Z"}`
	for _, on := range []bool{false, true} {
		SetTreeTier(on)
		fn := compileForTest(t, src)
		r := New(Config{})
		v, err := r.Run(fn)
		if err != nil {
			r.Close()
			t.Fatalf("tree %v: %v", on, err)
		}
		got, err := r.ToString(v)
		if err != nil || got.Go() != want {
			t.Errorf("tree %v: got %v, %v; want %s", on, got, err, want)
		}
		r.Close()
	}
}

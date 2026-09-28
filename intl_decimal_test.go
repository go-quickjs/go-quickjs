package quickjs_test

import (
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestIntlNumericStringExtremes pins how Intl.NumberFormat takes a numeric
// string with an extreme exponent. One far below one used to be written out
// zero by zero -- 1e-100000000000 ran the process out of memory -- and one
// whose exponent overflowed lost its sign (KI-10); one whose exponent is past
// an int64 was not a number at all (KI-28). The standard writes each as
// the number it is; ICU refuses one whose first digit is more than 999,999,999
// places below the point, and WithNodeQuirks does as V8 does.
func TestIntlNumericStringExtremes(t *testing.T) {
	src := `const out = [];
		for (const v of ["1e-100000000000", "-1e9223372036854775807", "5e-999999999", "1e-1000000000", "\u00851",
				"1e99999999999999999999", "-1e-99999999999999999999"]) {
			try { out.push(new Intl.NumberFormat("en").format(v)) } catch (e) { out.push(e.constructor.name + ": " + e.message) }
		}
		out.join(" | ")`
	for _, c := range []struct {
		quirks bool
		want   string
	}{
		{false, "0 | -∞ | 0 | 0 | NaN | ∞ | -0"},
		{true, "TypeError: Internal error. Icu error. | -∞ | 0 | TypeError: Internal error. Icu error. | NaN | ∞ | TypeError: Internal error. Icu error."},
	} {
		var opts []quickjs.Option
		if c.quirks {
			opts = append(opts, quickjs.WithNodeQuirks())
		}
		rt := quickjs.New(opts...)
		start := time.Now()
		if got := evalString(t, rt, src); got != c.want {
			t.Errorf("quirks %v:\n got %s\nwant %s", c.quirks, got, c.want)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("quirks %v took %v", c.quirks, d)
		}
		rt.Close()
	}
}

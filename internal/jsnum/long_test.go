package jsnum

import (
	"math"
	"math/big"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
)

// TestLongDecimalsRoundAsStrconv pins that a decimal literal of more than
// maxDecimalDigits characters is read as strconv reads it whole, digit for
// digit: at, just above and just below each midpoint between two doubles --
// where the rounding turns, and which takes up to 767 significant digits to
// write -- with its digits moved across the point, padded with zeros, signed,
// and pushed past the doubles' range.
func TestLongDecimalsRoundAsStrconv(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	zeros := strings.Repeat("0", 1200)
	var cases []string
	for n := 0; n < 300; n++ {
		d := math.Float64frombits(r.Uint64() &^ (1 << 63))
		if math.IsInf(d, 0) || math.IsNaN(d) || d == 0 {
			continue
		}
		mid := new(big.Rat).SetFloat64(d)
		mid.Add(mid, new(big.Rat).SetFloat64(math.Nextafter(d, math.Inf(1))))
		mid.Quo(mid, big.NewRat(2, 1))
		// The midpoint's exact digits: a binary fraction ends in as many
		// decimal places as it has bits after the point.
		m := strings.TrimRight(mid.FloatString(1100), "0")
		below := []byte(m)
		for i := len(below) - 1; ; i-- {
			if below[i] != '.' && below[i] != '0' {
				below[i]--
				break
			}
		}
		cases = append(cases, m, m+zeros, m+zeros+"1", string(below)+strings.Repeat("9", 1200),
			"-"+m+zeros, "+"+m+"1", "000"+m+zeros, m+zeros+"e-20", m+"e+15", m+zeros+"E400", m+zeros+"e-400")
	}
	for _, digits := range []string{strings.Repeat("1", 5000), "9" + zeros, "1" + zeros + "1", "." + zeros + "1",
		zeros + "." + zeros + "5", "0" + zeros + "." + zeros, "-0." + zeros, strings.Repeat("9", 3000) + "e-3000",
		"1" + zeros + "e99999999999999999999", "1" + zeros + "e-99999999999999999999", "5" + zeros + "."} {
		cases = append(cases, digits)
	}
	// The point, an exponent's sign and a stray byte at each place in the
	// eight bytes digitsEnd reads at once.
	for k := 1; k <= 9; k++ {
		run := strings.Repeat("3", k)
		cases = append(cases, run+"."+zeros+"7", run+"e-"+run, "1"+zeros+run+"x"+run, run+"."+run+"e+5"+zeros)
	}
	for _, s := range cases {
		want, wantErr := strconv.ParseFloat(s, 64)
		got, err := ParseFloat(s)
		if math.Float64bits(got) != math.Float64bits(want) || (err == nil) != (wantErr == nil) {
			t.Errorf("ParseFloat(%.40s... (%d characters)) = %v, %v; strconv gives %v, %v", s, len(s), got, err, want, wantErr)
		}
		if strings.ContainsAny(s, "eE") || s[0] == '+' || s[0] == '-' {
			continue
		}
		if ne, ok := wantErr.(*strconv.NumError); ok && ne.Err == strconv.ErrSyntax {
			// Not a literal: NaN to ToNumber.
			want = math.NaN()
		}
		if got := ToNumber(s); math.Float64bits(got) != math.Float64bits(want) {
			t.Errorf("ToNumber(%.40s... (%d characters)) = %v, want %v", s, len(s), got, want)
		}
	}
}

// TestLongDecimalsOtherwise pins what a long string that is not a decimal
// literal is, as it was: NaN to ToNumber, and to parseFloat and parseInt
// the value of its numeric prefix.
func TestLongDecimalsOtherwise(t *testing.T) {
	long := strings.Repeat("7", 2000)
	for _, s := range []string{long + "x", long + "_1", long + "..5", long + "e", long + "e+", "x" + long, long + "1.5.5", "1_" + long} {
		if v := ToNumber(s); !math.IsNaN(v) {
			t.Errorf("ToNumber(%.20s...) = %v, want NaN", s, v)
		}
	}
	want, _ := strconv.ParseFloat(long, 64)
	if v := ParseFloatPrefix(long + "x"); v != want {
		t.Errorf("parseFloat = %v, want %v", v, want)
	}
	if v := ParseIntPrefix(long+".5", 10); v != want {
		t.Errorf("parseInt = %v, want %v", v, want)
	}
	if v := ParseIntPrefix("-"+long, 0); v != -want {
		t.Errorf("parseInt of a negative = %v, want %v", v, -want)
	}
}

// FuzzLongDecimals compares ParseFloat with strconv on long literals made
// from the fuzzer's bytes: a run of digits, a point, more digits and an
// exponent, each repeated to past maxDecimalDigits.
//
//	go test ./internal/jsnum -run '^$' -fuzz '^FuzzLongDecimals$' -fuzztime=10m
func FuzzLongDecimals(f *testing.F) {
	f.Add("1", "", uint16(900), "", false)
	f.Add("5", "0", uint16(800), "-20", true)
	f.Add("2.2250738585072011", "0", uint16(1000), "-308", false)
	f.Fuzz(func(t *testing.T, head, fill string, n uint16, exp string, neg bool) {
		var b strings.Builder
		if neg {
			b.WriteByte('-')
		}
		b.WriteString(head)
		b.WriteString(strings.Repeat(fill, int(n%4000)))
		if exp != "" {
			b.WriteString("e" + exp)
		}
		s := b.String()
		if len(s) > 20000 {
			return
		}
		want, wantErr := strconv.ParseFloat(s, 64)
		if wantErr != nil && strings.ContainsAny(s, "_xXpPiInN") {
			// What strconv reads that no decimal literal is: shortDecimal
			// passes it on whole, as it was.
			return
		}
		got, err := ParseFloat(s)
		if math.Float64bits(got) != math.Float64bits(want) || (err == nil) != (wantErr == nil) {
			t.Fatalf("ParseFloat(%q) = %v, %v; strconv gives %v, %v", s, got, err, want, wantErr)
		}
	})
}

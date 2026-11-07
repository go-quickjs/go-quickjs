package jsnum

import (
	"fmt"
	"math"
	"math/big"
	"math/rand/v2"
	"strconv"
	"testing"
)

// checkShort compares ParseShort on s with strconv, which is exact, wherever
// it answers: it may always decline.
func checkShort(t *testing.T, s string) bool {
	t.Helper()
	f, end, ok := ParseShort(s, 0)
	if !ok {
		return false
	}
	if end != len(s) {
		t.Fatalf("ParseShort(%q) ended at %d", s, end)
	}
	want, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("ParseShort(%q) = %v, strconv: %v", s, f, err)
	}
	if math.Float64bits(f) != math.Float64bits(want) {
		t.Fatalf("ParseShort(%q) = %v (%#x), want %v (%#x)", s, f, math.Float64bits(f), want, math.Float64bits(want))
	}
	return true
}

func TestParseShortAgainstStrconv(t *testing.T) {
	for _, s := range []string{
		"0", "-0", "0.0", "-0.0", "0e0", "-0e5", "0.000", "1", "-1", "12", "1.5", "-1.5",
		"0.1", "0.2", "0.3", "1e23", "1e22", "1e-22", "8.41e21", "2.0e-308",
		"9007199254740991", "9007199254740992", "9007199254740993", "9007199254740994",
		"18446744073709551615", "1844674407370955161", "9999999999999999999",
		"1.7976931348623157e308", "4.9e-324", "2.2250738585072014e-308",
		"123456789012345678", "0.000001", "1E5", "1e+5", "1e-5", "1.0E+2",
		"3.141592653589793", "2.718281828459045", "0.30000000000000004",
	} {
		checkShort(t, s)
	}
	r := rand.New(rand.NewPCG(1, 2))
	answered, n := 0, 2000000
	if testing.Short() {
		n = 100000
	}
	for range n {
		m := r.Uint64() >> uint(r.IntN(64))
		e := r.IntN(160) - 80
		var s string
		switch r.IntN(4) {
		case 0:
			s = fmt.Sprintf("%de%d", m, e)
		case 1:
			d := strconv.FormatUint(m, 10)
			at := r.IntN(len(d)) + 1
			s = d[:at] + "." + d[at:]
			if at == len(d) {
				s += "0"
			}
			if len(d) > 1 && d[0] == '0' {
				continue
			}
		case 2:
			s = strconv.FormatFloat(r.Float64()*math.Pow(10, float64(e)), 'g', -1, 64)
		case 3:
			s = strconv.FormatFloat(math.Float64frombits(r.Uint64()), 'e', r.IntN(18), 64)
		}
		if r.IntN(2) == 0 {
			s = "-" + s
		}
		if checkShort(t, s) {
			answered++
		}
	}
	if answered < n/3 {
		t.Errorf("answered %d of %d: it should answer most numbers", answered, n)
	}
}

// Decimals on either side of the midpoint of two neighbouring doubles, which
// is where a rounding that is only nearly right goes wrong.
func TestParseShortAtMidpoints(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	n := 300000
	if testing.Short() {
		n = 20000
	}
	for range n {
		exp := r.IntN(300) - 150
		f := math.Ldexp(float64(1<<52|r.Uint64()&(1<<52-1)), exp)
		next := math.Nextafter(f, math.Inf(1))
		mid := new(big.Float).SetPrec(2000).SetFloat64(f)
		mid.Add(mid, new(big.Float).SetPrec(2000).SetFloat64(next))
		mid.Quo(mid, big.NewFloat(2))
		for _, digits := range []int{17, 18, 19} {
			s := mid.Text('e', digits-1)
			checkShort(t, s)
		}
	}
}

// The table of powers is written out, and each entry can only be read: here
// each is made again with exact arithmetic, as the top 128 bits of the power
// of ten, rounded down, which is what the algorithm's bounds assume.
func TestShortPowersAreExact(t *testing.T) {
	two64 := new(big.Int).Lsh(big.NewInt(1), 64)
	for exp := minShortPower; exp <= maxShortPower; exp++ {
		abs := exp
		if abs < 0 {
			abs = -abs
		}
		p := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(abs)), nil)
		var top *big.Int
		if exp >= 0 {
			top = new(big.Int).Set(p)
			if n := p.BitLen() - 128; n > 0 {
				top.Rsh(top, uint(n))
			} else {
				top.Lsh(top, uint(-n))
			}
		} else {
			// 2^(128+bits) / 10^-exp has 128 or 129 bits.
			top = new(big.Int).Lsh(big.NewInt(1), uint(128+p.BitLen()))
			top.Quo(top, p)
			if n := top.BitLen() - 128; n > 0 {
				top.Rsh(top, uint(n))
			}
		}
		hi, lo := new(big.Int).DivMod(top, two64, new(big.Int))
		got := shortPowers[exp-minShortPower]
		if got[1] != hi.Uint64() || got[0] != lo.Uint64() {
			t.Errorf("1e%d: {%#x, %#x}, want {%#x, %#x}", exp, got[0], got[1], lo.Uint64(), hi.Uint64())
		}
	}
}

// What the fast ways must decline or get right: integers a double cannot
// hold, on either side of a midpoint, and every power of ten either side of
// where the exact path stops, with the digits at the edges of what it takes.
func TestParseShortEdges(t *testing.T) {
	for m := uint64(1<<53 - 4); m < 1<<53+16; m++ {
		checkShort(t, strconv.FormatUint(m, 10))
		checkShort(t, strconv.FormatUint(m, 10)+".0")
		checkShort(t, strconv.FormatUint(m, 10)+"e1")
		checkShort(t, strconv.FormatUint(m, 10)+"e-1")
	}
	for e := minShortPower - 2; e <= maxShortPower+2; e++ {
		for _, m := range []string{"1", "9", "12.5", "1.7976931348623157", "2.2250738585072014", "4.9406564584124654", "9007199254740993", "9999999999999999999", "1844674407370955161"} {
			checkShort(t, fmt.Sprintf("%se%d", m, e))
			checkShort(t, fmt.Sprintf("-%se%d", m, e))
		}
	}
	// Past what a double holds, and below: strconv is what says, and an
	// answer here would have to be its answer. They are declined, in fact.
	for _, s := range []string{
		"1.7976931348623157e308", "1.7976931348623158e308", "1.7976931348623159e308",
		"2.4703282292062327e-324", "2.4703282292062328e-324", "2.2250738585072011e-308",
		"2.2250738585072012e-308", "1e-323", "5e-324", "1e308", "1e309",
	} {
		if f, _, ok := ParseShort(s, 0); ok {
			want, err := strconv.ParseFloat(s, 64)
			if err != nil || math.Float64bits(want) != math.Float64bits(f) {
				t.Errorf("ParseShort(%q) = %v, strconv %v, %v", s, f, want, err)
			}
		}
	}
}

func TestParseShortDeclines(t *testing.T) {
	for _, s := range []string{
		"", "-", "01", "-01", "1.", ".5", "1e", "1e+", "1.e5", "--1",
		"123456789012345678901", "1.2345678901234567890", "1e400", "1e-400", "1e99999",
	} {
		if f, end, ok := ParseShort(s, 0); ok {
			t.Errorf("ParseShort(%q) = %v, %d", s, f, end)
		}
	}
	// A number is read up to what cannot be in it.
	if f, end, ok := ParseShort("[12.5,3]", 1); !ok || f != 12.5 || end != 5 {
		t.Errorf("got %v, %d, %v", f, end, ok)
	}
}

// Whole numbers are written in place, two digits at a time, from the end:
// each length of number, either side of each power of ten, with what is
// already in the buffer left alone, whether or not there is room after it.
func TestAppendFloatWholeNumbers(t *testing.T) {
	check := func(v int64) {
		t.Helper()
		want := strconv.FormatInt(v, 10)
		if got := string(AppendFloat(nil, float64(v))); got != want {
			t.Fatalf("AppendFloat(%d) = %q", v, got)
		}
		room := append(make([]byte, 0, 64), "ab"...)
		if got := string(AppendFloat(room, float64(v))); got != "ab"+want {
			t.Fatalf("AppendFloat(%d) after text = %q", v, got)
		}
		tight := []byte("ab")[:2:2]
		if got := string(AppendFloat(tight, float64(v))); got != "ab"+want {
			t.Fatalf("AppendFloat(%d) with no room = %q", v, got)
		}
	}
	const top = 1<<53 - 1
	for _, v := range []int64{0, 1, 9, 10, 99, 100, top, top - 1, 1 << 31, 1<<31 - 1, 1 << 32} {
		check(v)
		check(-v)
	}
	for p := int64(10); p <= 1e15; p *= 10 {
		for d := int64(-2); d <= 2; d++ {
			check(p + d)
			check(-(p + d))
		}
	}
	r := rand.New(rand.NewPCG(7, 8))
	for range 1000000 {
		check(int64(r.Uint64() >> (11 + uint(r.IntN(53)))))
	}
}

// A number of few places is written from its digits: what strconv writes is
// what a shorter way must come to.
func TestAppendFloatPlaces(t *testing.T) {
	check := func(v float64) {
		t.Helper()
		got := string(AppendFloat(nil, v))
		want := strconv.FormatFloat(v, 'f', -1, 64)
		if a := math.Abs(v); a < 1e-6 || a >= 1e21 {
			want = FormatFloat(v)
		}
		if got != want {
			t.Fatalf("AppendFloat(%v) = %q, want %q", v, got, want)
		}
	}
	for _, v := range []float64{
		0.5, -0.5, 0.25, 1.5, 20.5, 0.1, 0.2, 0.3, 0.1 + 0.2, 1.1, 2.2, 3.3, 0.000001, 0.000002,
		999999999.5, 999999999.999999, 1000000000.5, 123456.789, 0.05, 0.005, 0.0005, 100.01,
		1.000001, 1.0000001, 4.35, 2.675, 1.005, 1.255, 1e-7, 8.5e-6,
	} {
		check(v)
		check(-v)
	}
	// Either side of where the shortcut's range ends, a whole number away
	// from its top and an ulp at a time at its bottom.
	for _, edge := range []float64{1e9, 1e-6, 1, 1e15 / 1e6} {
		for _, dir := range []float64{0, math.Inf(1)} {
			v := edge
			for range 2000 {
				check(v)
				check(-v)
				v = math.Nextafter(v, dir)
			}
		}
	}
	r := rand.New(rand.NewPCG(5, 6))
	n := 3000000
	if testing.Short() {
		n = 100000
	}
	for range n {
		// Decimals of a few places, and their neighbours, which are not.
		v := float64(r.IntN(2000000000)) / math.Pow10(r.IntN(9))
		check(v)
		check(math.Nextafter(v, 0))
		check(math.Nextafter(v, math.Inf(1)))
		check(r.Float64() * 1000)
	}
}

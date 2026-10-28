package bignum

import (
	"errors"
	"math/big"
	"math/rand/v2"
	"strings"
	"testing"
)

var errStop = errors.New("stop")

// checkedOps are the checked entry points, each of operands of about n
// words, against math/big, for a check: the result's text, or why it
// stopped.
func checkedOps(r *rand.Rand, n int) map[string]func(check func() error) (string, error) {
	x, y := randNat(r, n), randNat(r, n)
	short := randNat(r, 7)
	wide := randNat(r, 2*n)
	// Products by FFT take little work: of operands four times longer.
	xl, yl := randNat(r, 4*n), randNat(r, 4*n)
	s := toBig(x).Text(10)
	text := func(z []uint, err error) (string, error) {
		if err != nil {
			return "", err
		}
		return toBig(z).Text(16), nil
	}
	return map[string]func(func() error) (string, error){
		"mul": func(c func() error) (string, error) {
			return text(MulChecked(make([]uint, 8*n), xl, yl, c))
		},
		"mul by a short one": func(c func() error) (string, error) {
			return text(MulChecked(make([]uint, n+7), x, short, c))
		},
		"sqr": func(c func() error) (string, error) {
			return text(SqrChecked(make([]uint, 8*n), xl, c))
		},
		"quo": func(c func() error) (string, error) {
			q, _, err := DivModChecked(make([]uint, QuoRoom(2*n, n)), nil, wide, y, c)
			return text(q, err)
		},
		"rem": func(c func() error) (string, error) {
			_, rem, err := DivModChecked(nil, make([]uint, n), wide, y, c)
			return text(rem, err)
		},
		"quo by a short one": func(c func() error) (string, error) {
			q, rem, err := DivModChecked(make([]uint, QuoRoom(2*n, 7)), make([]uint, 7), wide, short, c)
			t, _ := text(rem, err)
			u, err := text(q, err)
			return t + "," + u, err
		},
		"append": func(c func() error) (string, error) {
			b, err := AppendChecked([]byte("pre"), true, x, 10, c)
			return string(b), err
		},
		"parse": func(c func() error) (string, error) {
			z, ok, err := ParseChecked(s, 10, c)
			if !ok && err == nil {
				return "", errors.New("not parsed")
			}
			return text(z, err)
		},
	}
}

// TestChecked pins the checked entry points: with a check that never stops
// them their results are the unchecked ones, the check asked at intervals
// of the work, and one that stops them stops them, its error returned.
func TestChecked(t *testing.T) {
	r := rand.New(rand.NewPCG(11, 12))
	n := 20000
	if testing.Short() {
		n = 12000
	}
	x, y := randNat(r, n), randNat(r, n)
	short := randNat(r, 7)
	wide := randNat(r, 2*n)
	xl, yl := randNat(r, 4*n), randNat(r, 4*n)
	bx, by, bw, bs := toBig(x), toBig(y), toBig(wide), toBig(short)
	bxl, byl := toBig(xl), toBig(yl)
	q, rem := new(big.Int).QuoRem(bw, by, new(big.Int))
	qs, rs := new(big.Int).QuoRem(bw, bs, new(big.Int))
	want := map[string]string{
		"mul":                new(big.Int).Mul(bxl, byl).Text(16),
		"mul by a short one": new(big.Int).Mul(bx, bs).Text(16),
		"sqr":                new(big.Int).Mul(bxl, bxl).Text(16),
		"quo":                q.Text(16),
		"rem":                rem.Text(16),
		"quo by a short one": rs.Text(16) + "," + qs.Text(16),
		"append":             "pre-" + bx.Text(10),
		"parse":              bx.Text(16),
	}
	for name, op := range checkedOps(rand.New(rand.NewPCG(11, 12)), n) {
		asked := 0
		got, err := op(func() error { asked++; return nil })
		if err != nil || got != want[name] {
			t.Errorf("%s: %.40s..., %v", name, got, err)
		}
		if got, err := op(nil); err != nil || got != want[name] {
			t.Errorf("%s unchecked: %.40s..., %v", name, got, err)
		}
		if asked == 0 && !strings.Contains(name, "short") {
			t.Errorf("%s: never asked", name)
		}
		// Stopped at the first check, and never asked again. (A product or
		// quotient by a short one is too little work to be asked.)
		stops := 0
		if _, err := op(func() error { stops++; return errStop }); asked > 0 && (err != errStop || stops != 1) {
			t.Errorf("%s: stopped with %v, asked %d times", name, err, stops)
		}
		t.Logf("%s: asked %d times", name, asked)
	}
}

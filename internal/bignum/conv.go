package bignum

import "math/bits"

const digitChars = "0123456789abcdefghijklmnopqrstuvwxyz"

// digitPairs is each of 00 to 99, two digits.
const digitPairs = "00010203040506070809101112131415161718192021222324252627282930313233343536373839" +
	"40414243444546474849505152535455565758596061626364656667686970717273747576777879" +
	"8081828384858687888990919293949596979899"

// chunk is the largest power of base a word holds, and how many digits it
// is: what a magnitude is written and read in a word at a time.
func chunk(base int) (uint, int) {
	b := uint(base)
	p, n := b, 1
	for {
		hi, lo := bits.Mul(p, b)
		if hi != 0 {
			return p, n
		}
		p, n = lo, n+1
	}
}

// Append appends x in base, 2 to 36, its digits in lower case, to dst: "0"
// for zero, a minus for neg. A magnitude of many words is written by
// halves, divided by a power of the base about the square root of it, each
// half the same way, so that its time is a product's, not quadratic.
func Append(dst []byte, neg bool, x []uint, base int) []byte {
	p := newProc(nil)
	dst = appendNat(dst, neg, x, base, p)
	p.done()
	return dst
}

// appendNat is Append, p's account kept.
func appendNat(dst []byte, neg bool, x []uint, base int, p *proc) []byte {
	if len(x) == 0 {
		return append(dst, '0')
	}
	if neg {
		dst = append(dst, '-')
	}
	// Digits, at most, then written backward into the room.
	n := BitLen(x)/bits.Len(uint(base)-1) + 1
	if base&(base-1) != 0 {
		n = int(float64(BitLen(x))/log2(base)) + 2
	}
	start := len(dst)
	dst = append(dst, make([]byte, n)...)
	buf := dst[start:]
	w := len(buf)
	cw, cn := chunk(base)
	if base&(base-1) == 0 {
		// A power of two's digits are the bits, a few at a time.
		w = writePow2(buf, w, x, uint(bits.TrailingZeros(uint(base))), p)
	} else if len(x) > convSplit {
		pw := powers(cw, len(x), p)
		if p.stopped() {
			return dst
		}
		w = writeSplit(buf, w, x, base, cw, cn, pw, p)
		if p.stopped() {
			return dst
		}
	} else {
		w = writeSmall(buf, w, x, base, cw, cn)
	}
	// The digits were written from the end of the room: move them down.
	copy(buf, buf[w:])
	return dst[:start+len(buf)-w]
}

// log2 is a base's logarithm in base 2, for the digits of a magnitude.
func log2(base int) float64 {
	// No math import for one function: a short table.
	return [...]float64{2: 1, 3: 1.584962500721156, 4: 2, 5: 2.321928094887362, 6: 2.584962500721156, 7: 2.807354922057604,
		8: 3, 9: 3.169925001442312, 10: 3.321928094887362, 11: 3.4594316186372973, 12: 3.584962500721156, 13: 3.700439718141092,
		14: 3.807354922057604, 15: 3.9068905956085187, 16: 4, 17: 4.087462841250339, 18: 4.169925001442312, 19: 4.247927513443585,
		20: 4.321928094887363, 21: 4.392317422778761, 22: 4.459431618637297, 23: 4.523561956057013, 24: 4.584962500721156,
		25: 4.643856189774724, 26: 4.700439718141092, 27: 4.754887502163468, 28: 4.807354922057604, 29: 4.857980995127572,
		30: 4.906890595608519, 31: 4.954196310386876, 32: 5, 33: 5.044394119358453, 34: 5.087462841250339, 35: 5.129283016944966,
		36: 5.169925001442312}[base] - 1e-9
}

// convSplit is the words from which a magnitude is written by halves.
const convSplit = 40

// writeSmall writes x's digits backward into buf, ending before w, a word
// of digits at a time, its leading zeros dropped. It returns where they
// begin.
func writeSmall(buf []byte, w int, x []uint, base int, cw uint, cn int) int {
	q := make([]uint, len(x))
	copy(q, x)
	q = Norm(q)
	for len(q) > 0 {
		var r uint
		q, r = DivWord(q, q, cw)
		if base == 10 {
			// Two digits at a time, by a constant, which is a
			// multiplication.
			for i := 0; i < cn; i += 2 {
				if r == 0 && len(q) == 0 {
					break
				}
				d := r % 100
				r /= 100
				w--
				buf[w] = digitPairs[2*d+1]
				if i+1 == cn || r == 0 && len(q) == 0 && d < 10 {
					break
				}
				w--
				buf[w] = digitPairs[2*d]
			}
			continue
		}
		for i := 0; i < cn; i++ {
			if r == 0 && len(q) == 0 {
				break
			}
			w--
			buf[w] = digitChars[r%uint(base)]
			r /= uint(base)
		}
	}
	return w
}

// writePow2 writes x's digits in base 2^s backward into buf, ending
// before w, each s of its bits, its leading zeros dropped, and returns
// where they begin: in time linear in x, as V8's ToStringFormatter writes
// a power of two's.
func writePow2(buf []byte, w int, x []uint, s uint, p *proc) int {
	mask := uint(1)<<s - 1
	n := BitLen(x)
	for pos := 0; pos < n; pos += int(s) {
		i, o := pos/W, uint(pos%W)
		d := x[i] >> o
		if o+s > W && i+1 < len(x) {
			d |= x[i+1] << (W - o)
		}
		w--
		buf[w] = digitChars[d&mask]
		if pos&(1<<20-1) < int(s) && p.spend(1<<20/W) {
			return w
		}
	}
	return w
}

// convPower is a divisor writeSplit halves by, cw^(2^k), and whether it
// divides by Barrett's method, and its reciprocal, made the first time it
// does: a power divides as many of the halves of its size as there are,
// each by the same reciprocal, as V8's tostring.cc keeps its levels'.
type convPower struct {
	d       []uint
	inv     []uint
	barrett bool
}

// powers are cw^(2^k), k from 0, while one is under the words of a
// magnitude of n: the divisors writeSplit halves by. A power of d words
// divides about n/(2d) halves, by Barrett's method where that pays for
// as many divisions by its reciprocal (barrettPays).
func powers(cw uint, n int, pr *proc) []convPower {
	p := []convPower{{d: []uint{cw}}}
	for {
		last := p[len(p)-1].d
		if 2*len(last) > n {
			break
		}
		sq := make([]uint, 2*len(last))
		if len(last) >= karatsubaThreshold {
			sq = Norm(sqr(sq, last, pr))
			if pr.stopped() {
				return p
			}
		} else {
			sq = Sqr(sq, last)
		}
		p = append(p, convPower{d: sq})
	}
	for i := range p {
		d := len(p[i].d)
		uses := max(1, n/(2*d))
		p[i].barrett = d >= 2 && barrettPays(d, uses*d-1)
	}
	return p
}

// divide writes x/c's power to q and the remainder to r, by Barrett's
// method and the power's reciprocal where it divides so, for a quotient
// long enough, as divLarge would.
func (c *convPower) divide(q, r, x []uint, pr *proc) ([]uint, []uint) {
	n := len(c.d)
	if !c.barrett || 2*(len(x)-n+1) < n {
		return divMod(q, r, x, c.d, pr)
	}
	if c.inv == nil {
		b := make([]uint, n)
		shlInto(b, c.d, uint(bits.LeadingZeros(c.d[n-1])))
		inv := make([]uint, n+1)
		invert(inv, b, pr)
		if pr.stopped() {
			return nil, nil
		}
		c.inv = inv
	}
	return divBarrett(q, r, x, c.d, c.inv, pr)
}

// writeSplit writes x by halves, backward, ending before w, its leading
// zeros dropped: the quotient and the remainder by the largest power p[k]
// not above it, the remainder -- the lower half -- padded with zeros to
// its 2^k chunks of digits.
//
// A write pr stops leaves the digits undefined.
func writeSplit(buf []byte, w int, x []uint, base int, cw uint, cn int, p []convPower, pr *proc) int {
	if len(x) <= convSplit {
		pr.spend(len(x) * len(x))
		return writeSmall(buf, w, x, base, cw, cn)
	}
	k := len(p) - 1
	for k > 0 && Cmp(p[k].d, x) > 0 {
		k--
	}
	c := &p[k]
	defer pr.release(pr.mark())
	q, r := c.divide(pr.alloc(QuoRoom(len(x), len(c.d))), pr.alloc(len(c.d)), x, pr)
	if pr.stopped() {
		return w
	}
	end := w
	w = writeSplit(buf, w, r, base, cw, cn, p[:k], pr)
	if pr.stopped() {
		return w
	}
	for end-w < (1<<k)*cn {
		w--
		buf[w] = '0'
	}
	return writeSplit(buf, w, q, base, cw, cn, p, pr)
}

// Parse is the magnitude of s's digits in base, 2 to 36, of either case:
// false for an empty s or one with another character.
func Parse(s string, base int) ([]uint, bool) {
	p := newProc(nil)
	z, ok := parse(s, base, p)
	p.done()
	return z, ok
}

// parseSplit is the chunks of digits, words, from which Parse reads a
// magnitude by halves, not a chunk at a time: measured, as V8's
// kFromStringLargeThreshold (300) is. In cycles on a Zen 2, decimal
// strings by halves took 1.15 the time at 100 chunks, the same at 150,
// 0.81 at 200, 0.58 at 600, 0.28 at 3000. A variable, which the tests
// lower.
var parseSplit = 150

// parse is Parse, p's account kept: a power of two's digits as bits; a
// long string's chunks of a word's digits each, combined by pairs, by
// halves, which is products of the size of what they combine, not a
// chunk at a time, which is quadratic, as V8's fromstring.cc reads them.
func parse(s string, base int, p *proc) ([]uint, bool) {
	if s == "" {
		return nil, false
	}
	if base&(base-1) == 0 {
		return parsePow2(s, uint(bits.TrailingZeros(uint(base))), p)
	}
	cw, cn := chunk(base)
	if n := (len(s) + cn - 1) / cn; n >= parseSplit {
		return parseHalves(s, base, cw, cn, n, p)
	}
	z := make([]uint, 0, len(s)/cn+2)
	for i := 0; i < len(s); {
		// A chunk of digits, the first one short if it must be.
		k := min(cn, len(s)-i)
		if i == 0 {
			if r := len(s) % cn; r != 0 {
				k = r
			}
		}
		var v, m uint = 0, 1
		for _, c := range []byte(s[i : i+k]) {
			d := digitValue(c)
			if d >= base {
				return nil, false
			}
			v = v*uint(base) + uint(d)
			m *= uint(base)
		}
		if k == cn {
			m = cw
		}
		z = z[:len(z)+1]
		z = MulAddWord(z, z[:len(z)-1], m, v)
		if p.spend(len(z)) {
			return nil, false
		}
		i += k
	}
	return Norm(z), true
}

// parsePow2 is parse of s's digits in base 2^b: its bits, from its last
// digit on.
func parsePow2(s string, b uint, p *proc) ([]uint, bool) {
	z := make([]uint, (len(s)*int(b)+W-1)/W)
	base := 1 << b
	pos := 0
	for i := len(s) - 1; i >= 0; i-- {
		d := digitValue(s[i])
		if d >= base {
			return nil, false
		}
		w, o := pos/W, uint(pos%W)
		z[w] |= uint(d) << o
		if o+b > W {
			z[w+1] |= uint(d) >> (W - o)
		}
		pos += int(b)
		if i&(1<<16-1) == 0 && p.spend(1<<16) {
			return nil, false
		}
	}
	return Norm(z), true
}

// parseHalves is parse of s's n chunks of cn digits, the first short: the
// chunks' values, least significant first, each a word; then pairs of
// them, each the higher times cw plus the lower, in two words; then pairs
// of those, the higher times cw^2, in four; and so on, each level's
// elements, of 2^l words, a number below cw^(2^l), and the last of an
// odd count carried up as it is.
func parseHalves(s string, base int, cw uint, cn, n int, p *proc) ([]uint, bool) {
	defer p.release(p.mark())
	a, b := p.alloc(2*n), p.alloc(2*n)
	for j := 0; j < n; j++ {
		hi := len(s) - j*cn
		var v uint
		for _, c := range []byte(s[max(hi-cn, 0):hi]) {
			d := digitValue(c)
			if d >= base {
				return nil, false
			}
			v = v*uint(base) + uint(d)
		}
		a[j] = v
		if j&(1<<12-1) == 1<<12-1 && p.spend(1<<12*cn) {
			return nil, false
		}
	}
	m := []uint{cw}
	size := 1
	for cnt := n; cnt > 1; cnt = (cnt + 1) / 2 {
		if size > 1 {
			sq := p.alloc(2 * len(m))
			m = sqrAny(sq, m, p)
			if p.stopped() {
				return nil, false
			}
		}
		for j := 0; j < cnt/2; j++ {
			lo := Norm(a[2*j*size : (2*j+1)*size])
			hi := Norm(a[(2*j+1)*size : (2*j+2)*size])
			z := b[2*j*size : 2*(j+1)*size]
			clear(z)
			if len(hi) > 0 {
				if len(hi) >= len(m) {
					mul(z[:len(hi)+len(m)], hi, m, p)
				} else {
					mul(z[:len(hi)+len(m)], m, hi, p)
				}
				if p.stopped() {
					return nil, false
				}
			}
			addAt(z, lo, 0)
		}
		if cnt&1 != 0 {
			last := (cnt - 1) * size
			copy(b[last:], a[last:last+size])
			clear(b[last+size : last+2*size])
		}
		a, b = b, a
		size *= 2
	}
	z := Norm(a[:size])
	return append([]uint(nil), z...), true
}

// digitValue is a digit's value, or 99 for a character that is no digit.
func digitValue(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'z':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'Z':
		return int(c-'A') + 10
	}
	return 99
}

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
	if len(x) > convSplit {
		w = writeSplit(buf, w, x, base, cw, cn, powers(cw, len(x)))
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

// powers are cw^(2^k), k from 0, while one is under the words of a
// magnitude of n: the divisors writeSplit halves by.
func powers(cw uint, n int) [][]uint {
	p := [][]uint{{cw}}
	for {
		last := p[len(p)-1]
		if 2*len(last) > n {
			return p
		}
		p = append(p, Sqr(make([]uint, 2*len(last)), last))
	}
}

// writeSplit writes x by halves, backward, ending before w, its leading
// zeros dropped: the quotient and the remainder by the largest power p[k]
// not above it, the remainder -- the lower half -- padded with zeros to
// its 2^k chunks of digits.
func writeSplit(buf []byte, w int, x []uint, base int, cw uint, cn int, p [][]uint) int {
	if len(x) <= convSplit {
		return writeSmall(buf, w, x, base, cw, cn)
	}
	k := len(p) - 1
	for k > 0 && Cmp(p[k], x) > 0 {
		k--
	}
	d := p[k]
	q, r := DivMod(make([]uint, QuoRoom(len(x), len(d))), make([]uint, len(d)), x, d)
	end := w
	w = writeSplit(buf, w, r, base, cw, cn, p[:k])
	for end-w < (1<<k)*cn {
		w--
		buf[w] = '0'
	}
	return writeSplit(buf, w, q, base, cw, cn, p)
}

// Parse is the magnitude of s's digits in base, 2 to 36, of either case:
// false for an empty s or one with another character.
func Parse(s string, base int) ([]uint, bool) {
	if s == "" {
		return nil, false
	}
	cw, cn := chunk(base)
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
		i += k
	}
	return Norm(z), true
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

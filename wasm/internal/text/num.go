package text

import (
	"math"
	"math/bits"
	"strconv"
)

// parseUnsigned parses a nat literal (decimal or 0x-prefixed hexadecimal,
// with underscores) that must fit in the given number of bits.
func parseUnsigned(s string, width int) (uint64, bool) {
	s = stripUnderscores(s)
	var v uint64
	var err error
	if len(s) > 2 && s[0] == '0' && s[1] == 'x' {
		v, err = strconv.ParseUint(s[2:], 16, 64)
	} else {
		v, err = strconv.ParseUint(s, 10, 64)
	}
	if err != nil {
		return 0, false
	}
	if width < 64 && v>>uint(width) != 0 {
		return 0, false
	}
	return v, true
}

// ParseNat32 parses an unsigned 32-bit literal: a Nat token's text.
func ParseNat32(s string) (uint32, bool) {
	if !isNat(s) {
		return 0, false
	}
	v, ok := parseUnsigned(s, 32)
	return uint32(v), ok
}

// ParseNat64 parses an unsigned 64-bit literal.
func ParseNat64(s string) (uint64, bool) {
	if !isNat(s) {
		return 0, false
	}
	return parseUnsigned(s, 64)
}

// ParseInt parses an integer literal of the given width (8, 16, 32 or 64
// bits), signed or not: an unsigned value up to 2^width-1, or a negative one
// down to -2^(width-1). It returns the value's two's complement bits.
func ParseInt(s string, width int) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	neg := false
	body := s
	switch s[0] {
	case '+':
		body = s[1:]
	case '-':
		neg = true
		body = s[1:]
	}
	if !isNat(body) {
		return 0, false
	}
	v, ok := parseUnsigned(body, width)
	if !ok {
		return 0, false
	}
	mask := uint64(math.MaxUint64)
	if width < 64 {
		mask = 1<<uint(width) - 1
	}
	if neg {
		if v > 1<<uint(width-1) {
			return 0, false
		}
		v = -v & mask
	}
	return v, true
}

// ParseF32 parses a float literal (any number token) to its bits.
func ParseF32(s string) (uint32, bool) {
	b, ok := parseFloat(s, 32)
	return uint32(b), ok
}

// ParseF64 parses a float literal (any number token) to its bits.
func ParseF64(s string) (uint64, bool) {
	return parseFloat(s, 64)
}

func parseFloat(s string, width int) (uint64, bool) {
	if numKind(s) == EOF {
		return 0, false
	}
	neg := false
	body := s
	switch s[0] {
	case '+':
		body = s[1:]
	case '-':
		neg = true
		body = s[1:]
	}
	mantBits := uint(52)
	expMask := uint64(0x7ff0000000000000)
	if width == 32 {
		mantBits = 23
		expMask = 0x7f800000
	}
	signBit := uint64(1) << uint(width-1)
	var b uint64
	switch {
	case body == "inf":
		b = expMask
	case body == "nan":
		b = expMask | 1<<(mantBits-1)
	case len(body) > 4 && body[:4] == "nan:":
		p, ok := parseUnsigned(body[4:], 64)
		if !ok || p == 0 || p>>mantBits != 0 {
			return 0, false
		}
		b = expMask | p
	default:
		t := stripUnderscores(body)
		if len(t) > 2 && t[0] == '0' && t[1] == 'x' {
			hasExp := false
			for i := 2; i < len(t); i++ {
				if t[i] == 'p' || t[i] == 'P' {
					hasExp = true
				}
			}
			if !hasExp {
				t += "p0"
			}
		}
		f, err := strconv.ParseFloat(t, width)
		if err != nil {
			return 0, false
		}
		if width == 32 {
			if math.IsInf(float64(float32(f)), 0) {
				return 0, false
			}
			b = uint64(math.Float32bits(float32(f)))
		} else {
			if math.IsInf(f, 0) {
				return 0, false
			}
			b = math.Float64bits(f)
		}
	}
	if neg {
		b |= signBit
	}
	return b, true
}

// The LEB128 encoders.

func appendU64(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func appendU32(b []byte, v uint32) []byte { return appendU64(b, uint64(v)) }

func appendS64(b []byte, v int64) []byte {
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v == 0 && c&0x40 == 0 || v == -1 && c&0x40 != 0 {
			return append(b, c)
		}
		b = append(b, c|0x80)
	}
}

func appendName(b []byte, s string) []byte {
	b = appendU32(b, uint32(len(s)))
	return append(b, s...)
}

func log2(v uint64) (uint32, bool) {
	if v == 0 || v&(v-1) != 0 {
		return 0, false
	}
	return uint32(bits.TrailingZeros64(v)), true
}
